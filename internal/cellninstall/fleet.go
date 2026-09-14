package cellninstall

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/sympozium-ai/sympozium/internal/cellnparent"
	"github.com/sympozium-ai/sympozium/internal/cellnplatform"
	"github.com/zeebo/blake3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Names shared between the chart's fleet mode and the installer.
const (
	FleetParentTokenSecret      = "celln-router-parent"
	FleetParentClientsConfigMap = "celln-fleet-parent-clients"
	FleetConfigurationConfigMap = "celln-fleet-configuration"
	FleetModelCredentialSecret  = "celln-fleet-model-credential"
	FleetParentConfigSecret     = "celln-parent-config"
	// FleetJournalRoot is the controller's claim: journal/ and approvals/ live
	// here instead of on an owner node.
	FleetJournalRoot = "/var/lib/sympozium/celln-parent"

	fleetNamespace           = "celln-system"
	fleetControllerTokenFile = "/etc/sympozium/celln/token"
)

var (
	scopePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	digestImagePattern = regexp.MustCompile(`^[^@:\s,]+(:[0-9]+)?(/[^@\s,]+)?@sha256:[a-f0-9]{64}$`)
	blake3Pattern      = regexp.MustCompile(`^blake3:[a-f0-9]{64}$`)
)

// FleetOptions describes one per-node native fleet: every node labeled
// celln.dev/kvm=true prepares the same reviewed package under
// /var/lib/sympozium-celln/<scope> and serves parents from it.
type FleetOptions struct {
	Scope, Principal, Publisher string
	PackageImage, PackageHash   string
	// Model is the default backend's route when Backends is empty (the single
	// backend named cellnplatform.DefaultBackend). An empty Provider keeps
	// Celln's reviewed default (DeepSeek).
	Model FleetModel
	// ModelCredentialFile is the default backend's provider key (local path).
	ModelCredentialFile string
	// Backends are the scope's model backends; every node configures all of
	// them and a namespace may run parents on any of them side by side.
	Backends []FleetBackend
	// Limits are the scope's parent ceilings; zero fields take DefaultFleetLimits.
	Limits FleetLimits
}

// FleetBackend is one model backend of a scope: a DNS-label name, its route
// and the local file holding its provider key (empty for keyless backends).
type FleetBackend struct {
	Name           string
	Model          FleetModel
	CredentialFile string
}

var backendNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

// CredentialProfileFor names the node-held credential a backend's model
// connections reference; the default backend keeps the scope itself.
func CredentialProfileFor(scope, backend string) string {
	if backend == cellnplatform.DefaultBackend {
		return scope
	}
	return scope + "-" + backend
}

// BackendCredentialSecret is the celln-system Secret holding a backend's key.
func BackendCredentialSecret(backend string) string {
	if backend == cellnplatform.DefaultBackend {
		return FleetModelCredentialSecret
	}
	return FleetModelCredentialSecret + "-" + backend
}

// BackendCredentialPath is where every dispatcher mounts a backend's key.
func BackendCredentialPath(backend string) string {
	if backend == cellnplatform.DefaultBackend {
		return "/etc/celln-native/model-token"
	}
	return "/etc/celln-native/" + backend + "/model-token"
}

// ResolvedBackends returns the scope's backends with routes resolved and
// names validated: Backends when given, else the single default backend
// built from Model and ModelCredentialFile.
func (o FleetOptions) ResolvedBackends() ([]FleetBackend, error) {
	backends := o.Backends
	if len(backends) == 0 {
		backends = []FleetBackend{{Name: cellnplatform.DefaultBackend, Model: o.Model, CredentialFile: o.ModelCredentialFile}}
	}
	if len(backends) > 32 {
		return nil, fmt.Errorf("at most 32 model backends per scope")
	}
	seen := map[string]bool{}
	out := make([]FleetBackend, 0, len(backends))
	for _, b := range backends {
		if !backendNamePattern.MatchString(b.Name) || seen[b.Name] {
			return nil, fmt.Errorf("backend names must be unique DNS labels of at most 32 characters: %q", b.Name)
		}
		seen[b.Name] = true
		model, err := b.Model.Resolve(CredentialProfileFor(o.Scope, b.Name))
		if err != nil {
			return nil, fmt.Errorf("backend %s: %w", b.Name, err)
		}
		if b.CredentialFile != "" && !filepath.IsAbs(b.CredentialFile) {
			return nil, fmt.Errorf("backend %s: credential file must be an absolute path", b.Name)
		}
		out = append(out, FleetBackend{Name: b.Name, Model: model, CredentialFile: b.CredentialFile})
	}
	return out, nil
}

// FleetLimits bound one parent for its whole life. They become the policy
// ceilings every run in the scope is admitted under; Celln validates them
// again when configuring each node.
type FleetLimits struct {
	LeaseSeconds     int64
	MaxTurns         int64
	MaxModelRequests int64
	MaxOutputTokens  int64
}

// DefaultFleetLimits favour long-running agents: a day-long parent with room
// for a working session of turns (the starter profile spends up to 3 requests
// and 1536 output tokens per turn).
var DefaultFleetLimits = FleetLimits{LeaseSeconds: 86400, MaxTurns: 256, MaxModelRequests: 768, MaxOutputTokens: 393216}

// Resolve fills zero fields from the defaults and applies the CRD bounds.
func (l FleetLimits) Resolve() (FleetLimits, error) {
	d := DefaultFleetLimits
	if l.LeaseSeconds == 0 {
		l.LeaseSeconds = d.LeaseSeconds
	}
	if l.MaxTurns == 0 {
		l.MaxTurns = d.MaxTurns
	}
	if l.MaxModelRequests == 0 {
		l.MaxModelRequests = d.MaxModelRequests
	}
	if l.MaxOutputTokens == 0 {
		l.MaxOutputTokens = d.MaxOutputTokens
	}
	if l.LeaseSeconds < 60 || l.LeaseSeconds > 86400 || l.MaxTurns < 1 || l.MaxTurns > 1024 || l.MaxModelRequests < 3 || l.MaxModelRequests > 6144 || l.MaxOutputTokens < 1536 || l.MaxOutputTokens > 3145728 {
		return l, fmt.Errorf("fleet limits out of range: lease 60–86400 s, turns 1–1024, model requests 3–6144, output tokens 1536–3145728")
	}
	return l, nil
}

// FleetModel selects the scope's model backend. Provider presets fill the
// protocol and endpoint; an explicit endpoint or protocol overrides them.
type FleetModel struct {
	Provider      string
	Protocol      string
	Endpoint      string
	Name          string
	AllowInsecure bool
}

// Fleet model provider presets.
const (
	ModelProviderDeepSeek    = "deepseek"
	ModelProviderOpenAI      = "openai"
	ModelProviderAnthropic   = "anthropic"
	ModelProviderLlamaServer = "llama-server"
)

// fleetModelPlaceholderCredential is published for backends that take no key
// (llama-server). Celln requires a bounded printable credential file; local
// servers ignore the bearer.
const fleetModelPlaceholderCredential = "sympozium-local-model-no-credential"

// Resolve applies the provider preset and validates the route exactly as the
// tenant ModelConnection will be validated.
func (m FleetModel) Resolve(scope string) (FleetModel, error) {
	if m.Provider == "" {
		m.Provider = ModelProviderDeepSeek
	}
	switch m.Provider {
	case ModelProviderDeepSeek:
		m.Protocol = firstNonEmpty(m.Protocol, "openai-chat")
		m.Endpoint = firstNonEmpty(m.Endpoint, "https://api.deepseek.com/chat/completions")
		m.Name = firstNonEmpty(m.Name, "deepseek-chat")
	case ModelProviderOpenAI:
		m.Protocol = firstNonEmpty(m.Protocol, "openai-chat")
		m.Endpoint = firstNonEmpty(m.Endpoint, "https://api.openai.com/v1/chat/completions")
	case ModelProviderAnthropic:
		m.Protocol = firstNonEmpty(m.Protocol, "anthropic-messages")
		m.Endpoint = firstNonEmpty(m.Endpoint, "https://api.anthropic.com/v1/messages")
	case ModelProviderLlamaServer:
		m.Protocol = firstNonEmpty(m.Protocol, "openai-chat")
		if m.Endpoint == "" {
			return m, fmt.Errorf("llama-server needs --celln-fleet-model-endpoint, e.g. http://HOST:8080/v1/chat/completions")
		}
	default:
		if m.Endpoint == "" || m.Protocol == "" {
			return m, fmt.Errorf("model provider %q needs an explicit endpoint and protocol (openai-chat or anthropic-messages)", m.Provider)
		}
	}
	if m.Name == "" {
		return m, fmt.Errorf("model provider %q needs --celln-fleet-model (the model name the backend serves)", m.Provider)
	}
	if strings.HasPrefix(strings.ToLower(m.Endpoint), "http://") && !m.AllowInsecure {
		return m, fmt.Errorf("model endpoint %s is plain HTTP; pass --celln-fleet-model-allow-insecure to approve it (private networks only)", m.Endpoint)
	}
	spec := api.ModelConnectionSpec{Provider: m.Provider, Protocol: m.Protocol, Endpoint: m.Endpoint, CredentialProfile: scope, Models: []string{m.Name}, AllowInsecure: m.AllowInsecure}
	if err := spec.Validate(); err != nil {
		return m, fmt.Errorf("model route: %w", err)
	}
	if strings.ContainsAny(m.Endpoint+m.Name+m.Provider, ",= \t") {
		return m, fmt.Errorf("model provider, endpoint and name must not contain commas, equals signs or spaces")
	}
	return m, nil
}

// NeedsCredential reports whether the backend requires a real provider key.
func (m FleetModel) NeedsCredential() bool { return m.Provider != ModelProviderLlamaServer }

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

// StatePath is the per-node authority location the chart derives from Scope.
func (o FleetOptions) StatePath() string { return "/var/lib/sympozium-celln/" + o.Scope }

func (o FleetOptions) validate() error {
	identities := o.Principal + o.Publisher
	if !scopePattern.MatchString(o.Scope) || !digestImagePattern.MatchString(o.PackageImage) || !blake3Pattern.MatchString(o.PackageHash) ||
		o.Principal == "" || o.Publisher == "" || strings.ContainsAny(identities, " \t\r\n,=") {
		return fmt.Errorf("fleet requires a DNS-label scope, a digest-pinned package image, a blake3 package hash, a publisher and a principal")
	}
	return nil
}

// FleetValues renders the node-preparation phase. The controller is wired by
// ConfigureFleet only after the nodes have published the starter configuration.
func FleetValues(o FleetOptions) ([]string, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	backends, err := o.ResolvedBackends()
	if err != nil {
		return nil, err
	}
	limits, err := o.Limits.Resolve()
	if err != nil {
		return nil, err
	}
	values := make([]string, 0, 16+8*len(backends))
	for i, b := range backends {
		prefix := fmt.Sprintf("celln.fleet.backends[%d].", i)
		values = append(values,
			prefix+"name="+b.Name,
			prefix+"provider="+b.Model.Provider,
			prefix+"protocol="+b.Model.Protocol,
			prefix+"endpoint="+b.Model.Endpoint,
			prefix+"model="+b.Model.Name,
			fmt.Sprintf("%sallowInsecure=%t", prefix, b.Model.AllowInsecure),
			prefix+"credentialSecret="+BackendCredentialSecret(b.Name),
			prefix+"credentialPath="+BackendCredentialPath(b.Name),
		)
	}
	return append(values,
		fmt.Sprintf("celln.fleet.limits.leaseSeconds=%d", limits.LeaseSeconds),
		fmt.Sprintf("celln.fleet.limits.maxTurns=%d", limits.MaxTurns),
		fmt.Sprintf("celln.fleet.limits.maxModelRequests=%d", limits.MaxModelRequests),
		fmt.Sprintf("celln.fleet.limits.maxOutputTokens=%d", limits.MaxOutputTokens),
		"celln.dispatcher.enabled=false",
		"celln.router.backends=null",
		"celln.router.parentTokenSecret="+FleetParentTokenSecret,
		"celln.fleet.enabled=true",
		"celln.fleet.scope="+o.Scope,
		"celln.fleet.package.image="+o.PackageImage,
		"celln.fleet.package.hash="+o.PackageHash,
		"celln.fleet.publisher="+o.Publisher,
		"celln.fleet.principal="+o.Principal,
		"celln.fleet.parentClientsConfigMap="+FleetParentClientsConfigMap,
		"celln.fleet.configurationConfigMap="+FleetConfigurationConfigMap,
	), nil
}

// PrepareFleetTrust publishes the shared parent principal: the gateway's
// bearer credential and the hash-only client policy every owner installs. An
// existing pair is accepted only when the policy still matches the credential;
// a live principal is never replaced.
func PrepareFleetTrust(ctx context.Context, store client.Client, principal string) error {
	if principal == "" || strings.ContainsAny(principal, " \t\r\n") {
		return fmt.Errorf("bounded parent principal required")
	}
	var secret corev1.Secret
	err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetParentTokenSecret}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		policy, err := parentClientPolicy(principal, token)
		if err != nil {
			return err
		}
		for _, object := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: FleetParentTokenSecret, Namespace: fleetNamespace, Labels: fleetLabels()}, Data: map[string][]byte{"token": []byte(token)}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: FleetParentClientsConfigMap, Namespace: fleetNamespace, Labels: fleetLabels()}, Data: map[string]string{"trusted-parent-clients.json": policy}},
		} {
			if err := store.Create(ctx, object); err != nil {
				return fmt.Errorf("publish %s: %w; partial fleet trust retained, never replaced", object.GetName(), err)
			}
		}
		return nil
	case err != nil:
		return err
	}
	var policy corev1.ConfigMap
	if err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetParentClientsConfigMap}, &policy); err != nil {
		return fmt.Errorf("fleet credential exists without its client policy; reconcile %s manually", FleetParentClientsConfigMap)
	}
	expected, err := parentClientPolicy(principal, string(secret.Data["token"]))
	if err != nil || policy.Data["trusted-parent-clients.json"] != expected {
		return fmt.Errorf("existing fleet client policy does not match the gateway credential and principal; never rotate by replacement")
	}
	return nil
}

func parentClientPolicy(principal, token string) (string, error) {
	if len(token) < 24 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("invalid parent credential")
	}
	raw, err := json.Marshal(map[string]any{"apiVersion": "celln.parent-clients/v1", "clients": []map[string]string{{"principal": principal, "tokenHash": fmt.Sprintf("blake3:%x", blake3.Sum256([]byte(token)))}}})
	return string(raw), err
}

func fleetLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/part-of": "sympozium"}
}

// PublishFleetModelCredential copies one operator credential file into the
// Secret every dispatcher mounts at the model profile's path. The controller
// and guests never read it. An existing Secret is kept when the file is
// omitted or identical; it is never rotated by replacement.
func PublishFleetModelCredential(ctx context.Context, store client.Client, path string, model FleetModel) error {
	return PublishFleetBackendCredential(ctx, store, FleetBackend{Name: cellnplatform.DefaultBackend, Model: model, CredentialFile: path})
}

// PublishFleetBackendCredential publishes one backend's provider key as its
// celln-system Secret. An existing Secret is kept when no file is given and
// never replaced with different content; keyless backends get a placeholder.
func PublishFleetBackendCredential(ctx context.Context, store client.Client, b FleetBackend) error {
	name, path, model := BackendCredentialSecret(b.Name), b.CredentialFile, b.Model
	var existing corev1.Secret
	err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: name}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && path == "" {
		return nil
	}
	if path == "" && !model.NeedsCredential() {
		// A keyless local backend still needs Celln's bounded credential file.
		return store.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fleetNamespace, Labels: fleetLabels()}, Data: map[string][]byte{"token": []byte(fleetModelPlaceholderCredential)}})
	}
	info, statErr := os.Lstat(path)
	if path == "" || !filepath.IsAbs(path) || statErr != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return fmt.Errorf("bounded regular absolute model credential file required")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		return readErr
	}
	credential := strings.TrimRight(string(raw), "\r\n")
	if len(credential) < 24 || strings.ContainsAny(credential, "\r\n\t ") {
		return fmt.Errorf("model credential file must hold one line of at least 24 printable characters")
	}
	if err == nil {
		if string(existing.Data["token"]) != credential {
			return fmt.Errorf("model credential Secret %s already exists with different content; omit the file to keep it", name)
		}
		return nil
	}
	return store.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: fleetNamespace, Labels: fleetLabels()}, Data: map[string][]byte{"token": []byte(credential)}})
}

var fleetConfigurationFiles = []string{"catalogue.json", "configured.json", "native-template.json"}

// PublishedBackends lists the backends a fleet configuration ConfigMap
// carries: keys are "<backend>.<file>"; unprefixed keys belong to the default
// backend. Every backend must carry every file.
func PublishedBackends(data map[string]string) ([]string, error) {
	files := map[string]map[string]bool{}
	for key, content := range data {
		backend, file := "", ""
		for _, f := range fleetConfigurationFiles {
			switch {
			case key == f:
				backend, file = cellnplatform.DefaultBackend, f
			case strings.HasSuffix(key, "."+f):
				backend, file = strings.TrimSuffix(key, "."+f), f
			}
		}
		if file == "" {
			return nil, fmt.Errorf("published fleet configuration carries an unknown key %q", key)
		}
		if len(content) == 0 || len(content) > 1<<20 {
			return nil, fmt.Errorf("published fleet configuration is incomplete: %s", key)
		}
		if !backendNamePattern.MatchString(backend) {
			return nil, fmt.Errorf("published fleet configuration names an invalid backend %q", backend)
		}
		if files[backend] == nil {
			files[backend] = map[string]bool{}
		}
		files[backend][file] = true
	}
	backends := make([]string, 0, len(files))
	for backend, have := range files {
		for _, f := range fleetConfigurationFiles {
			if !have[f] {
				return nil, fmt.Errorf("published fleet configuration is incomplete: %s.%s", backend, f)
			}
		}
		backends = append(backends, backend)
	}
	sort.Strings(backends)
	return backends, nil
}

func publishedKey(backend, file string, data map[string]string) string {
	if backend == cellnplatform.DefaultBackend {
		if _, ok := data[file]; ok {
			return file
		}
	}
	return backend + "." + file
}

// ReadFleetConfiguration materializes the starter configuration a fleet node
// published into dir/<backend>/ for every backend, for InstallPlatform. It
// reports false until a node has published; existing files must be identical.
func ReadFleetConfiguration(ctx context.Context, store client.Client, dir string) (bool, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return false, fmt.Errorf("clean absolute configuration directory required")
	}
	var published corev1.ConfigMap
	if err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetConfigurationConfigMap}, &published); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	backends, err := PublishedBackends(published.Data)
	if err != nil {
		return false, err
	}
	if len(backends) == 0 {
		return false, fmt.Errorf("published fleet configuration carries no backend")
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return false, err
	}
	for _, backend := range backends {
		if err := os.Mkdir(filepath.Join(dir, backend), 0700); err != nil && !os.IsExist(err) {
			return false, err
		}
		for _, name := range fleetConfigurationFiles {
			content := published.Data[publishedKey(backend, name, published.Data)]
			path := filepath.Join(dir, backend, name)
			if existing, err := os.ReadFile(path); err == nil {
				if string(existing) != content {
					return false, fmt.Errorf("existing %s/%s differs from the published fleet configuration", backend, name)
				}
				continue
			}
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				return false, err
			}
		}
	}
	return true, nil
}

// ConfigurationBackends lists the backend directories a materialized
// configuration holds, sorted.
func ConfigurationBackends(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() && backendNamePattern.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	if len(out) == 0 {
		return nil, fmt.Errorf("configuration directory %s holds no backend", dir)
	}
	return out, nil
}

// ConfigureFleet rebinds a completed Install to gateway issuance: the owner a
// parent lands on issues its permit, and the controller keeps only its own
// journal/approvals on a claim. It publishes the controller Secret and returns
// the values that wire the controller. An existing Secret is never replaced.
func ConfigureFleet(ctx context.Context, store client.Client, o Options) ([]string, error) {
	if o.ControllerNamespace == "" || o.OwnerTarget != ManagedRouterURL {
		return nil, fmt.Errorf("fleet parents are issued through the shared Celln gateway origin")
	}
	var registration cellnparent.RegistrationConfig
	if _, err := read(filepath.Join(o.OutputDir, "registrations.json"), &registration); err != nil {
		return nil, err
	}
	journal, approvals := filepath.Join(FleetJournalRoot, "journal"), filepath.Join(FleetJournalRoot, "approvals")
	if p := registration.Platform; p != nil {
		// Platform admission (InstallPlatform) is already gateway-issued and
		// namespace-free; it only needs the controller's claim paths.
		if registration.LocalProvisioner != nil || registration.RemoteProvisioner != nil || len(registration.HostTemplates) != 0 || len(registration.Registrations) != 0 || p.Target != o.OwnerTarget || p.TokenFile != fleetControllerTokenFile || p.Journal != journal || p.Approvals != approvals || registration.Journal != journal || registration.Approvals != approvals || p.ClusterID == "" {
			return nil, fmt.Errorf("fleet wiring requires a platform registration bound to the shared gateway and controller claim")
		}
	} else {
		if registration.LocalProvisioner == nil || registration.RemoteProvisioner != nil || len(registration.HostTemplates) != 1 || len(registration.Registrations) != 0 {
			return nil, fmt.Errorf("fleet wiring requires a fresh local starter registration")
		}
		registration.Journal = journal
		registration.Approvals = approvals
		registration.RemoteProvisioner = &cellnparent.RemoteProvisioner{Journal: journal, Approvals: approvals, Target: o.OwnerTarget, TokenFile: fleetControllerTokenFile}
		registration.LocalProvisioner = nil
	}
	data, err := json.Marshal(registration)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: FleetParentConfigSecret, Namespace: o.ControllerNamespace, Labels: fleetLabels()}, Data: map[string][]byte{"registrations.json": data}}
	if err := store.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("publish %s: %w; existing wiring is never replaced", secret.Name, err)
	}
	return []string{"celln.fleet.parentConfigSecret=" + FleetParentConfigSecret}, nil
}
