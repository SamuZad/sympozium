// Command celln-review-ui exposes only the bounded, operator-templated review
// workflows. It has no provider, issuer, receiver or gateway credentials.
package main

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/go-logr/logr"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/apiserver"
	webui "github.com/sympozium-ai/sympozium/web"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"
)

const hold = "sympozium.ai/review-ui-evidence"
const label = "sympozium.ai/celln-ux-review"

var requestID = regexp.MustCompile(`^[a-f0-9]{32}$`)

type review struct {
	kube      client.Client
	templates map[string]*api.AgentRun
	handler   http.Handler
	tokenFile string
}

func main() {
	addr := flag.String("listen", ":8443", "HTTPS listener")
	cert := flag.String("cert", "/tls/tls.crt", "review TLS certificate")
	key := flag.String("key", "/tls/tls.key", "review TLS key")
	token := flag.String("token-file", "/auth/token", "independent UI bearer file")
	templates := flag.String("templates", "/templates", "operator-owned run templates")
	flag.Parse()
	if err := serve(*addr, *cert, *key, *token, *templates); err != nil {
		fmt.Fprintln(os.Stderr, "review UI refused startup or serving")
		os.Exit(1)
	}
}
func serve(addr, cert, key, token, templates string) error {
	secret, err := os.ReadFile(token)
	if err != nil || len(strings.TrimSpace(string(secret))) < 32 {
		return errors.New("UI authentication required")
	}
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	_ = api.AddToScheme(scheme)
	cfg := ctrl.GetConfigOrDie()
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return err
	}
	kube, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return err
	}
	r := &review{kube: c, templates: map[string]*api.AgentRun{}, tokenFile: token}
	for mode, file := range map[string]string{"direct": "direct.yaml", "model": "model-one-shot.yaml", "enduring": "enduring.yaml"} {
		raw, err := os.ReadFile(templates + "/" + file)
		if err != nil {
			return err
		}
		var run api.AgentRun
		if err = yaml.UnmarshalStrict(raw, &run); err != nil {
			return err
		}
		if !allowedNamespace(run.Namespace) || run.Spec.Backend != "celln" || run.Spec.CellnSelection == nil {
			return errors.New("invalid review template")
		}
		r.templates[mode] = &run
	}
	frontend, err := fs.Sub(webui.Dist, "dist")
	if err != nil {
		return err
	}
	server := apiserver.NewServer(c, nil, kube, logr.Discard())
	reader := apiserver.NewTokenReader(token, logr.Discard())
	r.handler = server.HandlerWithUI(reader, frontend)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	httpServer := &http.Server{Addr: addr, Handler: r, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second}
	done := make(chan error, 1)
	go func() { done <- httpServer.ListenAndServeTLS(cert, key) }()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		shutdown, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdown)
	}
}
func allowedNamespace(ns string) bool {
	return ns == "celln-review-a-495" || ns == "celln-review-b-495"
}
func (r *review) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if req.URL.Path == "/readyz" {
		w.WriteHeader(200)
		return
	}
	if !strings.HasPrefix(req.URL.Path, "/api/") && !strings.HasPrefix(req.URL.Path, "/ws") {
		r.handler.ServeHTTP(w, req)
		return
	}
	raw, err := os.ReadFile(r.tokenFile)
	token := strings.TrimSpace(string(raw))
	if err != nil || len(token) < 32 || subtle.ConstantTimeCompare([]byte(req.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	if req.URL.Path == "/api/v1/review/config" && req.Method == "GET" {
		profiles := []map[string]any{}
		for _, mode := range []string{"direct", "model", "enduring"} {
			base := r.templates[mode]
			profiles = append(profiles, map[string]any{"mode": mode, "provider": base.Spec.Model.Provider, "model": base.Spec.Model.Model, "namespace": base.Namespace, "enduring": base.Spec.Enduring, "timeout": base.Spec.Timeout.Duration.String(), "tools": base.Spec.CellnSelection.ClusterToolRefs})
		}
		output(w, map[string]any{"profiles": profiles})
		return
	}
	if req.URL.Path == "/api/v1/review/runs" {
		switch req.Method {
		case "GET":
			r.list(w, req)
		case "POST":
			r.create(w, req)
		default:
			http.Error(w, "method not allowed", 405)
		}
		return
	}
	ns := req.URL.Query().Get("namespace")
	parts := strings.Split(strings.Trim(req.URL.Path, "/"), "/")
	if !allowedNamespace(ns) || len(parts) < 4 || strings.Join(parts[:3], "/") != "api/v1/runs" {
		http.Error(w, "review route only", 403)
		return
	}
	var run api.AgentRun
	if err := r.kube.Get(req.Context(), client.ObjectKey{Namespace: ns, Name: parts[3]}, &run); err != nil {
		http.Error(w, "review run unavailable", 404)
		return
	}
	if run.Labels[label] != "495" {
		http.Error(w, "not a UI-owned review run", 403)
		return
	}
	if len(parts) == 5 && parts[4] == "archive" && req.Method == "POST" {
		r.archive(w, req, &run)
		return
	}
	allowed := len(parts) == 5 && parts[4] == "turns" && (req.Method == "GET" || req.Method == "POST")
	allowed = allowed || len(parts) == 7 && parts[4] == "turns" && parts[6] == "cancel" && req.Method == "POST"
	allowed = allowed || len(parts) == 4 && (req.Method == "GET" || req.Method == "DELETE")
	if !allowed {
		http.Error(w, "review route only", 403)
		return
	}
	if req.Method == "DELETE" && req.URL.Query().Get("uid") != string(run.UID) {
		http.Error(w, "original run UID required", 409)
		return
	}
	// Existing API handlers enforce run/turn UID binding, idempotency and scoped
	// readiness. No unbounded generic run-creation or catalogue mutation route.
	r.handler.ServeHTTP(w, req)
}
func output(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func decode(w http.ResponseWriter, req *http.Request, value any) bool {
	d := json.NewDecoder(http.MaxBytesReader(w, req.Body, 8192))
	d.DisallowUnknownFields()
	if d.Decode(value) != nil || d.Decode(&struct{}{}) != io.EOF {
		http.Error(w, "invalid bounded request", 400)
		return false
	}
	return true
}
func (r *review) list(w http.ResponseWriter, req *http.Request) {
	items := []api.AgentRun{}
	for _, ns := range []string{"celln-review-a-495", "celln-review-b-495"} {
		var runs api.AgentRunList
		if err := r.kube.List(req.Context(), &runs, client.InNamespace(ns), client.MatchingLabels{label: "495"}); err != nil {
			http.Error(w, "review inventory unavailable", 503)
			return
		}
		items = append(items, runs.Items...)
	}
	output(w, map[string]any{"items": items})
}
func (r *review) create(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Mode      string `json:"mode"`
		Task      string `json:"task"`
		RequestID string `json:"requestId"`
	}
	if !decode(w, req, &in) {
		return
	}
	base := r.templates[in.Mode]
	if base == nil || !requestID.MatchString(in.RequestID) || strings.TrimSpace(in.Task) == "" || len(in.Task) > 2048 {
		http.Error(w, "invalid review mode, task or request identity", 400)
		return
	}
	run := base.DeepCopy()
	run.Name = "ux-" + in.Mode + "-" + in.RequestID
	run.ResourceVersion = ""
	run.UID = ""
	run.OwnerReferences = nil
	run.Finalizers = []string{hold}
	if run.Labels == nil {
		run.Labels = map[string]string{}
	}
	run.Labels[label] = "495"
	task := in.Task
	if in.Mode == "direct" {
		raw, _ := json.Marshal(map[string]string{"text": task})
		task = string(raw)
	}
	run.Spec.Task = api.NewStringTask(task)
	// An immutable UI-local claim survives archived Kubernetes run records.
	// Absence after a claim never authorizes another create, including after a
	// crash between claiming and creating the original run.
	encoded, _ := json.Marshal(run.Spec)
	digest := fmt.Sprintf("%x", sha256.Sum256(encoded))
	immutable := true
	claim := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "claim-" + run.Name, Namespace: "celln-review-ui-495"}, Immutable: &immutable, Data: map[string]string{"requestHash": digest}}
	if err := r.kube.Create(req.Context(), claim); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			http.Error(w, "original request claim unavailable", 503)
			return
		}
		var originalClaim corev1.ConfigMap
		var original api.AgentRun
		if r.kube.Get(req.Context(), client.ObjectKeyFromObject(claim), &originalClaim) != nil || originalClaim.Data["requestHash"] != digest || r.kube.Get(req.Context(), client.ObjectKeyFromObject(run), &original) != nil || original.Labels[label] != "495" {
			http.Error(w, "original request unresolved, changed or archived; no replacement work created", 409)
			return
		}
		output(w, &original)
		return
	}
	if err := r.kube.Create(req.Context(), run); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			http.Error(w, "review creation refused", 503)
			return
		}
		var original api.AgentRun
		if r.kube.Get(req.Context(), client.ObjectKeyFromObject(run), &original) != nil || original.Labels[label] != "495" || !reflect.DeepEqual(original.Spec, run.Spec) {
			http.Error(w, "request identity conflicts with original run", 409)
			return
		}
		run = &original
	}
	output(w, run)
}
func (r *review) archive(w http.ResponseWriter, req *http.Request, run *api.AgentRun) {
	var in struct {
		UID string `json:"uid"`
	}
	if !decode(w, req, &in) {
		return
	}
	if in.UID != string(run.UID) || run.DeletionTimestamp == nil {
		http.Error(w, "stop the exact original run before removing its record", 409)
		return
	}
	confirmed := run.Status.CellnScoped != nil && run.Status.CellnScoped.CleanupConfirmed
	for _, condition := range run.Status.Conditions {
		if run.Status.CellnScoped == nil && run.Status.Phase == api.AgentRunPhaseFailed && condition.Type == "CellnScopedExecution" && (condition.Reason == "AdmissionRefused" || condition.Reason == "CancelledBeforeAdmission") && condition.Status == metav1.ConditionFalse {
			confirmed = true
		}
	}
	if !confirmed {
		http.Error(w, "cleanup remains unconfirmed; retain all recovery evidence", 409)
		return
	}
	finalizers := []string{}
	for _, value := range run.Finalizers {
		if value != hold {
			finalizers = append(finalizers, value)
		}
	}
	patch, _ := json.Marshal([]map[string]any{{"op": "test", "path": "/metadata/uid", "value": run.UID}, {"op": "test", "path": "/metadata/resourceVersion", "value": run.ResourceVersion}, {"op": "replace", "path": "/metadata/finalizers", "value": finalizers}})
	if err := r.kube.Patch(req.Context(), run, client.RawPatch(types.JSONPatchType, patch)); err != nil {
		http.Error(w, "record changed; reload before removal", 409)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
