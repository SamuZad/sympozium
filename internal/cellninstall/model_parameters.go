package cellninstall

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Model parameters are a bounded JSON object the Celln host merges into every
// provider request of one backend; the guest never sees or changes them. The
// rules below are Celln's own (it validates the plan again on every node):
// refusing here reports a bad object before anything reaches the cluster.

const (
	maxModelParameterKeys   = 16
	maxModelParameterDepth  = 3
	maxModelParameterString = 256
	maxModelParameterArray  = 8
	// MaxModelParametersBytes bounds the serialized object.
	MaxModelParametersBytes = 2048
	// ModelParametersMinCelln is the newest Celln release whose plans refuse
	// model parameters; nodes need a newer one.
	ModelParametersMinCelln = "v0.5.22"
	// ModelParametersCellnHint names the usual reason a backend with
	// parameters never gets configured.
	ModelParametersCellnHint = "this backend sets model parameters, which need a Celln release newer than " + ModelParametersMinCelln + " on the nodes (an older one refuses the plan: look for \"starter-configure\" in the celln-node-configure logs)"
)

var modelParameterKeyPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)

// ReservedModelParameters are the request fields Celln owns; a parameter may
// not set them.
var ReservedModelParameters = []string{"model", "messages", "system", "stream", "stream_options", "max_tokens", "max_completion_tokens", "n", "tools", "tool_choice", "functions", "function_call", "parallel_tool_calls", "user"}

// ValidateModelParameters applies Celln's rules for modelConnection.parameters.
// A nil or empty object is valid and means no parameters.
func ValidateModelParameters(parameters map[string]any) error {
	if len(parameters) == 0 {
		return nil
	}
	if len(parameters) > maxModelParameterKeys {
		return fmt.Errorf("model parameters: at most %d top-level keys, got %d", maxModelParameterKeys, len(parameters))
	}
	for _, key := range sortedKeys(parameters) {
		for _, reserved := range ReservedModelParameters {
			if key == reserved {
				return fmt.Errorf("model parameters: %q is reserved (Celln sets it on every request)", key)
			}
		}
	}
	if err := validateModelParameterObject(parameters, "", 1); err != nil {
		return err
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return fmt.Errorf("model parameters: %w", err)
	}
	if len(raw) > MaxModelParametersBytes {
		return fmt.Errorf("model parameters: serialized size %d bytes exceeds %d", len(raw), MaxModelParametersBytes)
	}
	return nil
}

func validateModelParameterObject(object map[string]any, path string, depth int) error {
	if depth > maxModelParameterDepth {
		return fmt.Errorf("model parameters: %s nests deeper than %d levels", path, maxModelParameterDepth)
	}
	for _, key := range sortedKeys(object) {
		at := key
		if path != "" {
			at = path + "." + key
		}
		if !modelParameterKeyPattern.MatchString(key) {
			return fmt.Errorf("model parameters: key %q must match %s", at, modelParameterKeyPattern)
		}
		switch value := object[key].(type) {
		case map[string]any:
			if err := validateModelParameterObject(value, at, depth+1); err != nil {
				return err
			}
		case []any:
			if len(value) > maxModelParameterArray {
				return fmt.Errorf("model parameters: %s holds %d items, at most %d", at, len(value), maxModelParameterArray)
			}
			for i, item := range value {
				switch item.(type) {
				case map[string]any, []any:
					return fmt.Errorf("model parameters: %s[%d] must be a boolean, number or string", at, i)
				}
				if err := validateModelParameterScalar(item, fmt.Sprintf("%s[%d]", at, i)); err != nil {
					return err
				}
			}
		default:
			if err := validateModelParameterScalar(value, at); err != nil {
				return err
			}
		}
	}
	return nil
}

func validateModelParameterScalar(value any, at string) error {
	switch v := value.(type) {
	case bool:
		return nil
	case string:
		if len(v) > maxModelParameterString || strings.ContainsRune(v, 0) {
			return fmt.Errorf("model parameters: %s must be a string of at most %d bytes without NUL", at, maxModelParameterString)
		}
		return nil
	case json.Number:
		f, err := v.Float64()
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return fmt.Errorf("model parameters: %s must be a finite number", at)
		}
		return nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return fmt.Errorf("model parameters: %s must be a finite number", at)
		}
		return nil
	case float32:
		return validateModelParameterScalar(float64(v), at)
	case int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64:
		return nil
	case nil:
		return fmt.Errorf("model parameters: %s is null; use a boolean, number, string, object or array", at)
	default:
		return fmt.Errorf("model parameters: %s has unsupported type %T", at, value)
	}
}

func sortedKeys(object map[string]any) []string {
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// ParseModelParameters decodes and validates one JSON object. Numbers keep
// their written form. Empty input and {} mean no parameters (nil).
func ParseModelParameters(raw []byte) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, nil
	}
	if len(raw) > 4*MaxModelParametersBytes {
		return nil, fmt.Errorf("model parameters: serialized size exceeds %d bytes", MaxModelParametersBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("model parameters: not valid JSON: %w", err)
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, fmt.Errorf("model parameters: one JSON object expected")
	}
	object, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("model parameters: a JSON object is required")
	}
	if len(object) == 0 {
		return nil, nil
	}
	if err := ValidateModelParameters(object); err != nil {
		return nil, err
	}
	return object, nil
}

// ReadModelParametersFile reads the parameters-file of a backend.
func ReadModelParametersFile(path string) (map[string]any, error) {
	info, err := os.Stat(path)
	if !filepath.IsAbs(path) || err != nil || !info.Mode().IsRegular() || info.Size() > 4*MaxModelParametersBytes {
		return nil, fmt.Errorf("model parameters file %q must be a regular absolute file of at most %d bytes", path, 4*MaxModelParametersBytes)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	parameters, err := ParseModelParameters(raw)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return parameters, nil
}

// ModelParametersJSON is the compact form carried through chart values, the
// node's backend list and messages; empty for no parameters.
func ModelParametersJSON(parameters map[string]any) string {
	if len(parameters) == 0 {
		return ""
	}
	raw, err := json.Marshal(parameters)
	if err != nil {
		return ""
	}
	return string(raw)
}

// SameModelParameters compares two objects by JSON value, whatever Go number
// types their decoders chose.
func SameModelParameters(a, b map[string]any) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == 0 && len(b) == 0
	}
	normal := func(parameters map[string]any) any {
		var out any
		raw, err := json.Marshal(parameters)
		if err != nil || json.Unmarshal(raw, &out) != nil {
			return nil
		}
		return out
	}
	left, right := normal(a), normal(b)
	return left != nil && reflect.DeepEqual(left, right)
}

// PublishedModelParameters reads what the fleet published for every backend:
// model.parameters of its configured.json (nil when it has none).
func PublishedModelParameters(data map[string]string) (map[string]map[string]any, error) {
	backends, err := PublishedBackends(data)
	if err != nil {
		return nil, err
	}
	out := map[string]map[string]any{}
	for _, backend := range backends {
		var configured struct {
			Model struct {
				Parameters map[string]any `json:"parameters"`
			} `json:"model"`
		}
		decoder := json.NewDecoder(strings.NewReader(data[publishedKey(backend, "configured.json", data)]))
		decoder.UseNumber()
		if err := decoder.Decode(&configured); err != nil {
			return nil, fmt.Errorf("published configuration of backend %s is unreadable: %w", backend, err)
		}
		out[backend] = configured.Model.Parameters
	}
	return out, nil
}

// CheckPublishedModelParameters refuses an install whose backend parameters
// differ from what the fleet already published for that backend in the same
// scope and package. A node never reconfigures a configured backend and a
// published backend's files are never rewritten, so the change would be
// silently ignored; the refusal says how to get the parameters instead.
func CheckPublishedModelParameters(ctx context.Context, store client.Reader, o FleetOptions) error {
	backends, err := o.ResolvedBackends()
	if err != nil {
		return err
	}
	var published corev1.ConfigMap
	if err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetConfigurationConfigMap}, &published); apierrors.IsNotFound(err) {
		return nil
	} else if err != nil {
		return err
	}
	// A scope moving to another package or scope configures every backend afresh.
	if published.Annotations[packageAnnotation] != o.PackageHash {
		return nil
	}
	if scope := published.Annotations[FleetScopeAnnotation]; scope != "" && scope != o.Scope {
		return nil
	}
	have, err := PublishedModelParameters(published.Data)
	if err != nil {
		return err
	}
	for _, b := range backends {
		current, ok := have[b.Name]
		if !ok || SameModelParameters(current, b.Model.Parameters) {
			continue
		}
		return ModelParametersChangeRefusal(b, current)
	}
	return nil
}

// ModelParametersChangeRefusal explains why a published backend keeps its
// parameters and the two ways forward.
func ModelParametersChangeRefusal(b FleetBackend, published map[string]any) error {
	show := func(parameters map[string]any) string {
		if raw := ModelParametersJSON(parameters); raw != "" {
			return raw
		}
		return "none"
	}
	spec := fmt.Sprintf("name=%s-2,provider=%s,model=%s,endpoint=%s,protocol=%s", b.Name, b.Model.Provider, b.Model.Name, b.Model.Endpoint, b.Model.Protocol)
	if b.Model.AllowInsecure {
		spec += ",allow-insecure=true"
	}
	if b.Model.NeedsCredential() {
		spec += ",credential-file=/abs/path/key"
	}
	request := map[string]any{"name": b.Name + "-2", "provider": b.Model.Provider, "model": b.Model.Name, "endpoint": b.Model.Endpoint, "protocol": b.Model.Protocol, "parameters": b.Model.Parameters}
	if b.Model.AllowInsecure {
		request["allowInsecure"] = true
	}
	if b.Model.NeedsCredential() {
		request["credential"] = "<key>"
	}
	body, _ := json.Marshal(request)
	return fmt.Errorf("model parameters of a published backend cannot change: backend %s was published with %s and this install asks for %s.\n"+
		"  The nodes configured it once and its published configuration is never rewritten. Either\n"+
		"  - keep %s as published and add a backend under another name, then move agents to it:\n"+
		"      --celln-fleet-backend %s,parameters-file=/abs/path/parameters.json\n"+
		"    or POST /api/v1/celln-platform/backends %s\n"+
		"  - or move the fleet to a new scope with --celln-fleet-scope and --celln-fleet-replace-package, which configures every backend afresh and ends every live parent on the fleet",
		b.Name, show(published), show(b.Model.Parameters), b.Name, spec, body)
}

// strvalsEscape makes any text one literal Helm strvals value: a backslash
// before every character strvals gives meaning to (commas, equals signs,
// braces, brackets, dots, backslashes) keeps it as written.
func strvalsEscape(value string) string {
	var out strings.Builder
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r > 127) {
			out.WriteByte('\\')
		}
		out.WriteRune(r)
	}
	return out.String()
}
