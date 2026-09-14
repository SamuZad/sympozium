package cellninstall

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The probe is a one-token chat request with the fleet's key on the backend's
// own protocol; a provider that lists models but refuses completions, a bad
// key and a wrong model name are all reported before the cluster is touched.
func TestPreflightBackendReportsDeadProvidersAndBadKeys(t *testing.T) {
	ctx := context.Background()
	var seen []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		seen = append(seen, r.URL.Path+" "+r.Header.Get("Authorization")+" "+r.Header.Get("x-api-key"))
		switch {
		case r.URL.Path == "/v1/messages" && r.Header.Get("x-api-key") == "sk-ant-valid-credential-000001" && r.Header.Get("anthropic-version") != "":
			w.WriteHeader(200)
		case r.URL.Path == "/busy/chat/completions":
			w.WriteHeader(503)
			_, _ = w.Write([]byte(`{"error":{"message":"Service is too busy."}}`))
		case r.Header.Get("Authorization") != "Bearer sk-valid-credential-0000000001":
			w.WriteHeader(401)
		case body["model"] != "qwen.gguf" || body["max_tokens"] != float64(1):
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":"model not found"}`))
		default:
			w.WriteHeader(200)
		}
	}))
	defer server.Close()
	chat := FleetBackend{Name: "native", Model: FleetModel{Provider: "llama-server", Protocol: "openai-chat", Endpoint: server.URL + "/v1/chat/completions", Name: "qwen.gguf"}}
	if err := PreflightBackend(ctx, server.Client(), chat, "sk-valid-credential-0000000001"); err != nil {
		t.Fatalf("healthy backend refused: %v", err)
	}
	if err := PreflightBackend(ctx, server.Client(), chat, "sk-wrong"); err == nil || !strings.Contains(err.Error(), "refused the key") {
		t.Fatalf("bad key not reported: %v", err)
	}
	wrongModel := chat
	wrongModel.Model.Name = "other.gguf"
	if err := PreflightBackend(ctx, server.Client(), wrongModel, "sk-valid-credential-0000000001"); err == nil || !strings.Contains(err.Error(), "HTTP 404") {
		t.Fatalf("wrong model not reported: %v", err)
	}
	busy := FleetBackend{Name: "deepseek", Model: FleetModel{Provider: "deepseek", Protocol: "openai-chat", Endpoint: server.URL + "/busy/chat/completions", Name: "deepseek-chat"}}
	if err := PreflightBackend(ctx, server.Client(), busy, "sk-valid-credential-0000000001"); err == nil || !strings.Contains(err.Error(), "not serving completions (HTTP 503)") || !strings.Contains(err.Error(), "too busy") {
		t.Fatalf("outage not reported: %v", err)
	}
	messages := FleetBackend{Name: "claude", Model: FleetModel{Provider: "anthropic", Protocol: "anthropic-messages", Endpoint: server.URL + "/v1/messages", Name: "claude-test"}}
	if err := PreflightBackend(ctx, server.Client(), messages, "sk-ant-valid-credential-000001"); err != nil {
		t.Fatalf("anthropic protocol probe failed: %v", err)
	}
	down := FleetBackend{Name: "gone", Model: FleetModel{Provider: "llama-server", Protocol: "openai-chat", Endpoint: "http://127.0.0.1:1/v1/chat/completions", Name: "x"}}
	if err := PreflightBackend(ctx, server.Client(), down, "sk-valid-credential-0000000001"); err == nil || !strings.Contains(err.Error(), "unreachable") || !strings.Contains(err.Error(), "skip-preflight") {
		t.Fatalf("unreachable endpoint not reported with the escape hatch: %v", err)
	}
	for _, s := range seen {
		if strings.Contains(s, "sk-valid-credential") && !strings.HasPrefix(s, "/v1/chat/completions") && !strings.HasPrefix(s, "/busy") {
			t.Fatalf("key sent to an unexpected path: %s", s)
		}
	}
}

func TestPreflightCredentialPrefersFileThenSecretThenPlaceholder(t *testing.T) {
	ctx := context.Background()
	store := fleetStore()
	llama := FleetBackend{Name: "local", Model: FleetModel{Provider: ModelProviderLlamaServer}}
	if got, err := PreflightCredential(ctx, store, llama); err != nil || got != fleetModelPlaceholderCredential {
		t.Fatalf("keyless backend: %q %v", got, err)
	}
	keyed := FleetBackend{Name: "openai", Model: FleetModel{Provider: ModelProviderOpenAI}}
	if _, err := PreflightCredential(ctx, store, keyed); err == nil || !strings.Contains(err.Error(), "needs a key") {
		t.Fatalf("keyed backend without any key accepted: %v", err)
	}
	if err := PublishFleetBackendCredential(ctx, store, FleetBackend{Name: "openai", Model: keyed.Model, CredentialFile: writeKey(t, "sk-openai-existing-credential-01")}); err != nil {
		t.Fatal(err)
	}
	if got, err := PreflightCredential(ctx, store, keyed); err != nil || got != "sk-openai-existing-credential-01" {
		t.Fatalf("published key not used: %q %v", got, err)
	}
	keyed.CredentialFile = writeKey(t, "sk-openai-from-file-credential-01")
	if got, err := PreflightCredential(ctx, store, keyed); err != nil || got != "sk-openai-from-file-credential-01" {
		t.Fatalf("file key not preferred: %q %v", got, err)
	}
}

func writeKey(t *testing.T, key string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(key+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
