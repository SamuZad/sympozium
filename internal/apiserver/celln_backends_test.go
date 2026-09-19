package apiserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/go-logr/logr"
	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellninstall"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// A backend's model parameters are accepted on POST, validated with the
// fleet's rules, probed, stored for the nodes and shown on GET.
func TestCellnFleetBackendsCarryModelParameters(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = sympoziumv1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = appsv1.AddToScheme(scheme)
	installed := `[{"name":"native","provider":"deepseek","protocol":"openai-chat","endpoint":"https://api.deepseek.com/chat/completions","model":"deepseek-chat","allowInsecure":false,"credentialFile":"/etc/celln-native/credentials/native"},` +
		`{"name":"tuned","provider":"openai","protocol":"openai-chat","endpoint":"https://api.openai.com/v1/chat/completions","model":"gpt-4o","allowInsecure":false,"credentialFile":"/etc/celln-native/credentials/tuned","parameters":{"temperature":0.2}}]`
	configure := &appsv1.DaemonSet{ObjectMeta: metav1.ObjectMeta{Name: cellninstall.FleetConfigureDaemonSet, Namespace: "celln-system"}, Spec: appsv1.DaemonSetSpec{Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "configure", Env: []corev1.EnvVar{
		{Name: "FLEET_SCOPE", Value: "starter"}, {Name: "FLEET_PRINCIPAL", Value: "sympozium:celln"}, {Name: "FLEET_PACKAGE_HASH", Value: "blake3:" + strings.Repeat("b", 64)}, {Name: "FLEET_BACKENDS", Value: installed},
	}}}}}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configure).Build()
	srv := NewServer(cl, nil, nil, logr.Discard())
	srv.completeCellnBackend = func(string, cellninstall.FleetFacts, bool) {}

	var probe map[string]any
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probe = nil
		_ = json.NewDecoder(r.Body).Decode(&probe)
		if _, refused := probe["unknown_knob"]; refused {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":"unknown_knob is not supported"}`))
		}
	}))
	defer model.Close()
	post := func(body string) *httptest.ResponseRecorder {
		res := httptest.NewRecorder()
		srv.Handler(nil).ServeHTTP(res, httptest.NewRequest(http.MethodPost, "/api/v1/celln-platform/backends", strings.NewReader(body)))
		return res
	}
	request := func(name, parameters string) string {
		return `{"name":"` + name + `","provider":"llama-server","model":"qwen.gguf","endpoint":"` + model.URL + `/v1/chat/completions","allowInsecure":true,"parameters":` + parameters + `}`
	}

	// Invalid parameters: 400 with the precise rule, nothing probed or stored.
	for parameters, refusal := range map[string]string{
		`{"max_tokens":4096}`:                      `model parameters: "max_tokens" is reserved (Celln sets it on every request)`,
		`{"Temperature":1}`:                        `model parameters: key "Temperature" must match ^[a-z][a-z0-9_]{0,63}$`,
		`{"a":{"b":{"c":{"d":1}}}}`:                `model parameters: a.b.c nests deeper than 3 levels`,
		`{"stop":[1,2,3,4,5,6,7,8,9]}`:             `model parameters: stop holds 9 items, at most 8`,
		`{"a":null}`:                               `model parameters: a is null`,
		`{"a":"` + strings.Repeat("x", 257) + `"}`: `model parameters: a must be a string of at most 256 bytes without NUL`,
	} {
		probe = nil
		res := post(request("bad", parameters))
		if res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), refusal) || probe != nil {
			t.Fatalf("%s: status %d body %q probe %v", parameters, res.Code, res.Body.String(), probe)
		}
	}
	if res := post(`{"name":"bad","provider":"llama-server","parameters":[1]}`); res.Code != http.StatusBadRequest {
		t.Fatalf("non-object parameters: %d", res.Code)
	}
	// A parameter the provider refuses is reported by the probe.
	if res := post(request("bad", `{"unknown_knob":true}`)); res.Code != http.StatusBadRequest || !strings.Contains(res.Body.String(), "backend probe failed") || !strings.Contains(res.Body.String(), `{"unknown_knob":true}`) {
		t.Fatalf("refused parameter: %d %s", res.Code, res.Body.String())
	}
	if extra, _, err := cellninstall.ReadExtraBackends(t.Context(), cl); err != nil || len(extra) != 0 {
		t.Fatalf("a refused backend was stored: %v %v", extra, err)
	}

	thinkingOff := map[string]any{"chat_template_kwargs": map[string]any{"enable_thinking": false}}
	res := post(request("local", `{"chat_template_kwargs":{"enable_thinking":false}}`))
	if res.Code != http.StatusAccepted {
		t.Fatalf("add: %d %s", res.Code, res.Body.String())
	}
	var added CellnFleetBackend
	if err := json.Unmarshal(res.Body.Bytes(), &added); err != nil || !reflect.DeepEqual(added.Parameters, thinkingOff) || added.Source != "added" {
		t.Fatalf("add response: %+v %v", added, err)
	}
	if !reflect.DeepEqual(probe["chat_template_kwargs"], map[string]any{"enable_thinking": false}) || probe["max_tokens"] != float64(1) {
		t.Fatalf("probe body: %v", probe)
	}
	// What the node script reads carries them; a backend without carries no key.
	if res := post(`{"name":"plain","provider":"llama-server","model":"qwen.gguf","endpoint":"` + model.URL + `/v1/chat/completions","allowInsecure":true}`); res.Code != http.StatusAccepted {
		t.Fatalf("add plain: %d %s", res.Code, res.Body.String())
	}
	var cm corev1.ConfigMap
	if err := cl.Get(t.Context(), types.NamespacedName{Namespace: "celln-system", Name: cellninstall.FleetExtraBackendsConfigMap}, &cm); err != nil {
		t.Fatal(err)
	}
	if stored := cm.Data["backends.json"]; strings.Count(stored, `"parameters"`) != 1 || !strings.Contains(stored, `"parameters":{"chat_template_kwargs":{"enable_thinking":false}}`) {
		t.Fatalf("stored list: %s", stored)
	}
	// The name exists now: parameters cannot be changed by adding it again.
	if res := post(request("local", `{"temperature":1}`)); res.Code != http.StatusConflict {
		t.Fatalf("re-add: %d %s", res.Code, res.Body.String())
	}

	list := httptest.NewRecorder()
	srv.Handler(nil).ServeHTTP(list, httptest.NewRequest(http.MethodGet, "/api/v1/celln-platform/backends", nil))
	var backends []CellnFleetBackend
	if err := json.Unmarshal(list.Body.Bytes(), &backends); err != nil || len(backends) != 4 {
		t.Fatalf("list: %v %s", err, list.Body.String())
	}
	got := map[string]map[string]any{}
	for _, b := range backends {
		got[b.Name] = b.Parameters
	}
	if got["native"] != nil || got["plain"] != nil || !reflect.DeepEqual(got["tuned"], map[string]any{"temperature": 0.2}) || !reflect.DeepEqual(got["local"], thinkingOff) {
		t.Fatalf("listed parameters: %v", got)
	}
	if strings.Count(list.Body.String(), `"parameters"`) != 2 {
		t.Fatalf("backends without parameters list the key: %s", list.Body.String())
	}
}
