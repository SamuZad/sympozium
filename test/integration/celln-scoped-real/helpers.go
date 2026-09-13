package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	"github.com/sympozium-ai/sympozium/internal/modelbudget"
	"github.com/sympozium-ai/sympozium/internal/modelgateway"
	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func privateRegular(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 1<<20 || info.Mode().Perm()&0077 != 0 {
		return errors.New("must be a nonempty bounded regular file inaccessible to group/other")
	}
	return nil
}

func readBounded(path string, limit int64) ([]byte, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > limit {
		return nil, errors.New("file is empty, non-regular, or exceeds its bound")
	}
	return os.ReadFile(path)
}

func liveClient(o options) (client.Client, kubernetes.Interface, *runtime.Scheme, error) {
	raw, err := clientcmd.LoadFromFile(o.kubeconfig)
	if err != nil {
		return nil, nil, nil, err
	}
	if _, ok := raw.Contexts[o.contextName]; !ok {
		return nil, nil, nil, fmt.Errorf("context %q is absent from the explicit kubeconfig", o.contextName)
	}
	rest, err := clientcmd.NewNonInteractiveClientConfig(*raw, o.contextName, &clientcmd.ConfigOverrides{CurrentContext: o.contextName}, nil).ClientConfig()
	if err != nil {
		return nil, nil, nil, err
	}
	rest.Timeout = 10 * time.Second
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{api.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme, appsv1.AddToScheme, networkingv1.AddToScheme, rbacv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, nil, nil, err
		}
	}
	c, err := client.New(rest, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, nil, err
	}
	clientset, err := kubernetes.NewForConfig(rest)
	return c, clientset, scheme, err
}

func resourceNames(namespace string) (string, string) {
	return "celln-live-" + namespace, "celln-live-policy-" + namespace
}

func requireAbsent(ctx context.Context, c client.Client, o options, pkg nativePackage) error {
	for _, name := range []string{o.namespace, o.enduringNamespace, o.deniedNamespace, o.preparationNamespace} {
		var ns corev1.Namespace
		err := c.Get(ctx, types.NamespacedName{Name: name}, &ns)
		if o.reviewOwnership == "" {
			if err == nil {
				return fmt.Errorf("refusing existing namespace %q without --existing-review-ownership", name)
			}
			if !apierrors.IsNotFound(err) {
				return fmt.Errorf("check namespace %q: %w", name, err)
			}
			continue
		}
		if err != nil {
			return fmt.Errorf("owned review namespace %q unavailable: %w", name, err)
		}
		if ns.Labels["sympozium.ai/celln-review"] != o.reviewOwnership {
			return fmt.Errorf("namespace %q lacks exact review ownership", name)
		}
		tenant := ns.Labels["sympozium.ai/celln-review-tenant"] == "enabled"
		if tenant != (name == o.namespace || name == o.enduringNamespace) {
			return fmt.Errorf("namespace %q has unexpected tenant enablement", name)
		}
		if err := requireReviewNamespaceEmpty(ctx, c, name); err != nil {
			return err
		}
	}
	_, policyName := resourceNames(o.namespace)
	for name, object := range map[string]client.Object{pkg.OneShot.Name: &api.CellnRuntimeProfile{}, pkg.Enduring.Name: &api.CellnRuntimeProfile{}, pkg.Uppercase.Name: &api.ClusterCellnTool{}, policyName: &api.CellnExecutionPolicy{}} {
		if err := c.Get(ctx, types.NamespacedName{Name: name}, object); err == nil {
			return fmt.Errorf("refusing existing cluster authority object %q", name)
		} else if !apierrors.IsNotFound(err) {
			return fmt.Errorf("check cluster authority %q: %w", name, err)
		}
	}
	return nil
}

func requireReviewNamespaceEmpty(ctx context.Context, c client.Client, name string) error {
	lists := []client.ObjectList{&corev1.PodList{}, &corev1.SecretList{}, &corev1.ServiceList{}, &corev1.PersistentVolumeClaimList{}, &batchv1.JobList{}, &batchv1.CronJobList{}, &appsv1.DeploymentList{}, &appsv1.StatefulSetList{}, &appsv1.DaemonSetList{}, &appsv1.ReplicaSetList{}, &networkingv1.NetworkPolicyList{}, &rbacv1.RoleList{}, &rbacv1.RoleBindingList{}, &api.AgentList{}, &api.AgentRunList{}, &api.AgentRunTurnList{}, &api.AgentRuntimeList{}, &api.ModelConnectionList{}}
	for _, list := range lists {
		if err := c.List(ctx, list, client.InNamespace(name)); err != nil {
			return fmt.Errorf("verify empty review namespace %q: %w", name, err)
		}
		raw, _ := json.Marshal(list)
		var envelope struct {
			Items []json.RawMessage `json:"items"`
		}
		_ = json.Unmarshal(raw, &envelope)
		if len(envelope.Items) != 0 {
			return fmt.Errorf("review namespace %q contains unexpected %T resources", name, list)
		}
	}
	var configMaps corev1.ConfigMapList
	if err := c.List(ctx, &configMaps, client.InNamespace(name)); err != nil {
		return err
	}
	for _, item := range configMaps.Items {
		if item.Name != "kube-root-ca.crt" {
			return fmt.Errorf("review namespace %q contains unexpected ConfigMap %q", name, item.Name)
		}
	}
	var serviceAccounts corev1.ServiceAccountList
	if err := c.List(ctx, &serviceAccounts, client.InNamespace(name)); err != nil {
		return err
	}
	for _, item := range serviceAccounts.Items {
		if item.Name != "default" {
			return fmt.Errorf("review namespace %q contains unexpected ServiceAccount %q", name, item.Name)
		}
	}
	return nil
}

func openDisposablePostgres(ctx context.Context, databaseURL, repo string) (*pgxpool.Pool, *modelbudget.Store, *modelgateway.PostgresAuthorityStore, error) {
	pool, err := pgxpool.New(ctx, databaseURL)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("parse disposable PostgreSQL URL: %w", err)
	}
	fail := func(err error) (*pgxpool.Pool, *modelbudget.Store, *modelgateway.PostgresAuthorityStore, error) {
		pool.Close()
		return nil, nil, nil, err
	}
	var existing *string
	if err := pool.QueryRow(ctx, `SELECT to_regclass('public.celln_model_budgets')::text`).Scan(&existing); err != nil {
		return fail(fmt.Errorf("probe disposable PostgreSQL: %w", err))
	}
	if existing != nil {
		return fail(errors.New("refusing PostgreSQL database with an existing Celln budget schema; supply an initially empty disposable database"))
	}
	for _, name := range []string{"002_celln_model_budget.sql", "003_celln_model_gateway.sql"} {
		raw, err := readBounded(filepath.Join(repo, "migrations", name), 1<<20)
		if err != nil {
			return fail(fmt.Errorf("read migration %s: %w", name, err))
		}
		if _, err := pool.Exec(ctx, string(raw)); err != nil {
			return fail(fmt.Errorf("apply migration %s: %w", name, err))
		}
	}
	budgets, err := modelbudget.New(pool)
	if err != nil {
		return fail(err)
	}
	authorities, err := modelgateway.NewPostgresAuthorityStore(pool)
	if err != nil {
		return fail(err)
	}
	return pool, budgets, authorities, nil
}

func randomBearer() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func writeIssuerMaterial(dir string) (ed25519.PublicKey, ed25519.PrivateKey, string, string, error) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, nil, "", "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return nil, nil, "", "", err
	}
	keyFile := filepath.Join(dir, "issuer-key.pem")
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0600); err != nil {
		return nil, nil, "", "", err
	}
	jwks := map[string]any{"keys": []any{map[string]any{"kty": "OKP", "crv": "Ed25519", "use": "sig", "alg": "EdDSA", "kid": issuerKeyID, "x": base64.RawURLEncoding.EncodeToString(pub)}}}
	raw, _ := json.Marshal(jwks)
	jwksFile := filepath.Join(dir, "issuer-jwks.json")
	if err := os.WriteFile(jwksFile, raw, 0644); err != nil {
		return nil, nil, "", "", err
	}
	return pub, private, keyFile, jwksFile, nil
}

func newTLSServer(handler http.Handler) *httptest.Server {
	server := httptest.NewUnstartedServer(handler)
	server.Config.ErrorLog = log.New(io.Discard, "", 0)
	server.StartTLS()
	return server
}

func serverTrust(server *httptest.Server) (*x509.CertPool, []byte, error) {
	certificate := server.Certificate()
	if certificate == nil {
		return nil, nil, errors.New("TLS server supplied no certificate")
	}
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	return roots, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw}), nil
}

func unusedLoopbackAddress() (string, error) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return address, nil
}

type managedProcess struct {
	cmd  *exec.Cmd
	done chan error
}

type brokerDiagnostics struct {
	line    []byte
	discard bool
}

func (d *brokerDiagnostics) Write(data []byte) (int, error) {
	for _, b := range data {
		if b == '\n' {
			if !d.discard {
				switch string(d.line) {
				case "CELLN_BROKER_REFUSAL ENDPOINT", "CELLN_BROKER_REFUSAL MODEL", "CELLN_BROKER_REFUSAL OUTPUT_BUDGET", "CELLN_BROKER_REFUSAL GATEWAY", "CELLN_BROKER_REFUSAL CANCELLED", "CELLN_BROKER_REFUSAL PARAMETERS", "CELLN_BROKER_REFUSAL REQUEST", "CELLN_BROKER_REFUSAL REQUEST_BUDGET", "CELLN_BROKER_REFUSAL POLICY":
					fmt.Fprintln(os.Stderr, string(d.line))
				}
			}
			d.line = nil
			d.discard = false
		} else if len(d.line) < 128 {
			d.line = append(d.line, b)
		} else {
			d.discard = true
		}
	}
	return len(data), nil
}

func startCelln(ctx context.Context, o options, address, jwks, gateway, gatewayCA, parentTemplate string) (*managedProcess, error) {
	args := []string{"--root", o.cellnRoot, "dispatcher", "--mote-store", filepath.Join(o.cellnRoot, "motes"), "--tool-store", filepath.Join(o.cellnRoot, "tools"), "--max-cells", "6", "--memory-bytes", "1073741824", "--egress-slots", "4", "--listen", address, "--token-file", o.cellnTokenFile, "--scoped-operator-token-file", o.scopedOperatorTokenFile, "--scoped-jwks-file", jwks, "--scoped-issuer", issuerName, "--scoped-gateway-origin", gateway, "--scoped-gateway-ca", gatewayCA, "--scoped-parent-request-file", parentTemplate}
	cmd := exec.CommandContext(ctx, o.cellnBinary, args...)
	cmd.Stdout, cmd.Stderr = io.Discard, &brokerDiagnostics{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Env = []string{"PATH=/usr/bin:/bin", "RUST_BACKTRACE=0"}
	if o.cellnRuntimeDir != "" {
		cmd.Env = append(cmd.Env, "CELLN_RUNTIME_DIR="+o.cellnRuntimeDir)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start real Celln receiver: %w", err)
	}
	process := &managedProcess{cmd: cmd, done: make(chan error, 1)}
	go func() { process.done <- cmd.Wait() }()
	return process, nil
}

func writeParentTemplate(path string, pkg nativePackage) error {
	request := map[string]any{
		"apiVersion": "celln.dev/v1alpha1", "id": "$parent",
		"workload":     map[string]any{"id": "$parent", "caller": "$principal"},
		"mote":         pkg.Parent.Artifact.Mote,
		"tools":        []any{map[string]any{"alias": pkg.Parent.Artifact.EntryPoint, "hash": pkg.Parent.Artifact.Executable.Hash, "closure": pkg.Parent.Artifact.Closure}},
		"invocation":   map[string]any{"alias": pkg.Parent.Artifact.EntryPoint, "args": []string{}},
		"capabilities": map[string]any{"workspace": pkg.Parent.Limits.Workspace, "egress": pkg.Parent.Limits.Egress, "timeoutMs": 120000, "memoryBytes": pkg.Parent.Limits.MemoryBytes, "outputBytes": 65536},
		"execution":    map[string]any{"lane": pkg.Parent.Artifact.Lane, "requireHardwareIsolation": true},
	}
	// The receiver requires retained parent/child substrate overhead beyond two
	// parent-sized guests. This is capacity, not a new executable artifact.
	reserved := (uint64(pkg.Parent.Limits.MemoryBytes)+uint64(pkg.Enduring.Spec.Limits.MemoryBytes))*2 + (64 << 20)
	raw, err := json.Marshal(map[string]any{"apiVersion": "celln.scoped-parent-template/v1", "request": request, "reservedMemoryBytes": reserved})
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func stopProcess(process *managedProcess) {
	if process == nil || process.cmd == nil || process.cmd.Process == nil {
		return
	}
	select {
	case <-process.done:
		return
	default:
	}
	_ = syscall.Kill(-process.cmd.Process.Pid, syscall.SIGTERM)
	select {
	case <-process.done:
	case <-time.After(3 * time.Second):
		_ = syscall.Kill(-process.cmd.Process.Pid, syscall.SIGKILL)
		<-process.done
	}
}

func waitForListener(ctx context.Context, address string, process *managedProcess) error {
	deadline := time.NewTicker(100 * time.Millisecond)
	defer deadline.Stop()
	for {
		connection, err := net.DialTimeout("tcp", address, 200*time.Millisecond)
		if err == nil {
			_ = connection.Close()
			return nil
		}
		select {
		case err := <-process.done:
			return fmt.Errorf("Celln receiver exited before listening: %v", err)
		case <-ctx.Done():
			return fmt.Errorf("Celln receiver did not listen: %w", ctx.Err())
		case <-deadline.C:
		}
	}
}

func writeControllerConfig(path string, o options, keyFile, receiver, receiverCA, gateway, gatewayCA, gatewayToken string) error {
	value := cellnscoped.Config{
		ClusterID: o.contextName, PreparationNamespace: o.preparationNamespace,
		Receiver: cellnscoped.EndpointConfig{URL: receiver, CAFile: receiverCA, TokenFile: o.scopedOperatorTokenFile},
		Gateway:  &cellnscoped.EndpointConfig{URL: gateway, CAFile: gatewayCA, TokenFile: gatewayToken},
		Issuer:   cellnscoped.IssuerConfig{Name: issuerName, KeyID: issuerKeyID, PrivateKeyFile: keyFile},
	}
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0600)
}

func (p *providerRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	attempt := p.attempts.Add(1)
	fail := func(message string, status int) {
		p.mu.Lock()
		if p.failure == "" {
			p.failure = message
		}
		p.mu.Unlock()
		http.Error(w, "refused", status)
	}
	if r.Method != http.MethodPost || r.URL.Path != "/v1/chat/completions" || r.URL.RawQuery != "" {
		fail("unexpected method or path", http.StatusBadRequest)
		return
	}
	wantSecret := p.enduringSecret
	if attempt <= 2 {
		wantSecret = p.oneShotSecret
	}
	if r.Header.Get("Authorization") != "Bearer "+wantSecret || r.Header.Get("X-Celln-Execution-Permit") != "" || r.Header.Get("X-Celln-Model-Permit") != "" {
		fail("credential or scoped-header mismatch", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 128<<10))
	if err != nil {
		fail("provider request exceeded bound", http.StatusRequestEntityTooLarge)
		return
	}
	var request struct {
		Model     string                           `json:"model"`
		Messages  []struct{ Role, Content string } `json:"messages"`
		MaxTokens int64                            `json:"max_tokens"`
		Stream    bool                             `json:"stream,omitempty"`
		Tools     []json.RawMessage                `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil || request.Model != modelName || request.Stream || request.MaxTokens != requestReservation || len(request.Tools) != 1 || !bytes.Contains(request.Tools[0], []byte(`"name":"uppercase"`)) {
		fail("model request contract mismatch", http.StatusBadRequest)
		return
	}
	if len(request.Messages) == 0 {
		fail("empty model conversation", http.StatusBadRequest)
		return
	}
	last := request.Messages[len(request.Messages)-1]
	var message map[string]any
	completionTokens := int64(1)
	if last.Role == "tool" {
		var result struct {
			Text string `json:"text"`
		}
		if strictJSON([]byte(last.Content), &result) != nil || result.Text == "" || result.Text != strings.ToUpper(result.Text) {
			fail("real uppercase tool result missing from follow-up", http.StatusBadRequest)
			return
		}
		message = map[string]any{"role": "assistant", "content": result.Text}
	} else if last.Role == "user" {
		var text string
		for _, candidate := range []string{"celln", "violet"} {
			if strings.Contains(strings.ToLower(last.Content), candidate) {
				text = candidate
			}
		}
		if text == "" {
			fail("bounded uppercase task missing", http.StatusBadRequest)
			return
		}
		completionTokens = 2
		message = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "uppercase-call", "type": "function", "function": map[string]any{"name": "uppercase", "arguments": fmt.Sprintf(`{"text":%q}`, text)}}}}
	} else {
		fail("unexpected final conversation role", http.StatusBadRequest)
		return
	}
	p.valid.Add(1)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"id": "bounded-scoped-provider", "object": "chat.completion",
		"choices": []any{map[string]any{"index": 0, "message": message, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": completionTokens, "total_tokens": 1 + completionTokens},
	})
}

func (p *providerRecorder) failureMessage() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.failure == "" {
		return "none"
	}
	return p.failure
}
