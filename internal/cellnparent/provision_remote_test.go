package cellnparent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
	"k8s.io/apimachinery/pkg/types"
)

// A synthetic owner proves the gateway/registration boundary only; it is not
// evidence of Celln issuance or hardware execution.
func testRemoteProvisioner(t *testing.T, ctx context.Context, loader cellnauthority.Loader, intent ProvisionIntent, template HostProvisionTemplate) {
	t.Helper()
	const token = "remote-provision-test-credential-long-enough"
	uid := string(intent.Selection.Run.UID)
	incarnation, err := provisionIncarnation(template.Scope, uid)
	if err != nil {
		t.Fatal(err)
	}
	launch := "blake3:" + strings.Repeat("c", 64)
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte(token+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	requests := 0
	owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, _ := io.ReadAll(io.LimitReader(r.Body, 65537))
		var plan HostProvisionPlan
		if r.Method != http.MethodPost || r.URL.Path != "/v1/parents/provision" || r.Header.Get("Authorization") != "Bearer "+token || r.Header.Get("X-Celln-Parent-Incarnation") != incarnation || json.Unmarshal(body, &plan) != nil || plan.APIVersion != "celln.parent-provision-plan/v1" || plan.RunUID != uid || plan.Scope != template.Scope {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"apiVersion": "celln.parent-provisioned/v1", "launchProfile": launch, "incarnation": incarnation})
	}))
	defer owner.Close()
	p := RemoteProvisioner{Journal: t.TempDir(), Approvals: t.TempDir(), Target: owner.URL, TokenFile: tokenFile}
	first, err := p.ProvisionAndApprove(ctx, loader, intent, template)
	if err != nil {
		t.Fatal(err)
	}
	second, err := p.ProvisionAndApprove(ctx, loader, intent, template)
	if err != nil || first != second {
		t.Fatalf("retry changed approval: %v", err)
	}
	if first.Binding.Incarnation != incarnation || first.Binding.LaunchProfile != launch || first.Binding.RunUID != uid || first.Binding.Target != owner.URL || first.TokenFile != tokenFile {
		t.Fatal("unbound result")
	}
	config := RegistrationConfig{APIVersion: "sympozium.ai/celln-parent-registrations-v1", Journal: p.Journal, Approvals: p.Approvals, OperatorSource: loader.OperatorSource, RuntimeSource: loader.RuntimeSource, AgentSource: loader.AgentSource, RemoteProvisioner: &p, HostTemplates: []HostProvisionTemplate{template}}
	raw, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(t.TempDir(), "remote-provisioner.json")
	if err := os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	dispatcher, err := LoadRegistrationDispatcher(configPath, p.Approvals, loader.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.Admit(ctx, types.NamespacedName{Namespace: intent.Selection.Run.Namespace, Name: intent.Selection.Run.Name}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(p.Approvals, approvalFileName(uid))); err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("expected one owner request per issuance, got %d", requests)
	}
	// The durable choice pins owner and plan; no request reaches a substitute.
	changed := p
	changed.Target = "https://another-owner.example"
	if _, err := changed.ProvisionAndApprove(ctx, loader, intent, template); err == nil {
		t.Fatal("retry switched owner")
	}
	template.Native.AdmissionWindowMs++
	if _, err := p.ProvisionAndApprove(ctx, loader, intent, template); err == nil {
		t.Fatal("retry switched plan")
	}
	if requests != 3 {
		t.Fatal("refused retry contacted the owner")
	}
}

func TestRemoteProvisionRefusesUnboundOwnerResults(t *testing.T) {
	incarnation := "blake3:" + strings.Repeat("b", 64)
	valid := map[string]string{"apiVersion": "celln.parent-provisioned/v1", "launchProfile": "blake3:" + strings.Repeat("a", 64), "incarnation": incarnation}
	for name, respond := range map[string]func(http.ResponseWriter){
		"invalid": func(w http.ResponseWriter) { _, _ = io.WriteString(w, "{}") },
		"unknown": func(w http.ResponseWriter) {
			_, _ = io.WriteString(w, `{"apiVersion":"celln.parent-provisioned/v1","launchProfile":"blake3:`+strings.Repeat("a", 64)+`","incarnation":"`+incarnation+`","extra":true}`)
		},
		"stray": func(w http.ResponseWriter) {
			_ = json.NewEncoder(w).Encode(map[string]string{"apiVersion": "celln.parent-provisioned/v1", "launchProfile": valid["launchProfile"], "incarnation": "blake3:" + strings.Repeat("c", 64)})
		},
		"accepted": func(w http.ResponseWriter) { w.WriteHeader(http.StatusAccepted); _ = json.NewEncoder(w).Encode(valid) },
		"refused":  func(w http.ResponseWriter) { w.WriteHeader(http.StatusConflict); _ = json.NewEncoder(w).Encode(valid) },
	} {
		t.Run(name, func(t *testing.T) {
			owner := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { respond(w) }))
			defer owner.Close()
			tokenFile := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(tokenFile, []byte("remote-provision-test-credential-long-enough"), 0600); err != nil {
				t.Fatal(err)
			}
			p := RemoteProvisioner{Target: owner.URL, TokenFile: tokenFile}
			if _, err := p.issue(context.Background(), []byte(`{"apiVersion":"celln.parent-provision-plan/v1"}`), "tenant", incarnation); err == nil {
				t.Fatal("unbound owner result accepted")
			}
		})
	}
	p := RemoteProvisioner{Target: "http://127.0.0.1:1", TokenFile: "/operator/token"}
	if _, err := p.issue(context.Background(), []byte(`{}`), "tenant", incarnation); err == nil {
		t.Fatal("unreachable owner accepted")
	}
}
