package celln

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The gateway's "backend removed" refusal is definitive owner loss; every other
// 503 stays an uncertain outcome that preserves the incarnation.
func TestOwnerRemovedIsDistinctFromUncertainty(t *testing.T) {
	id := "blake3:" + strings.Repeat("a", 64)
	for name, tc := range map[string]struct {
		body string
		want error
	}{
		"removed":   {`{"error":"original parent backend removed","retryAuthorized":false}`, ErrOwnerRemoved},
		"other-503": {`{"error":"parent ownership unavailable","retryAuthorized":false}`, ErrReconcile},
		"garbage":   {`removed`, ErrReconcile},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusServiceUnavailable)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("owner-removed-test-credential-long"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := New(Config{BaseURL: server.URL, TokenFile: token})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.ParentStatus(context.Background(), id); !errors.Is(err, tc.want) {
				t.Fatalf("status: got %v want %v", err, tc.want)
			}
			if err := client.StopParent(context.Background(), id); !errors.Is(err, tc.want) {
				t.Fatalf("stop: got %v want %v", err, tc.want)
			}
		})
	}
}

// Only the owner's own create refusal is definitive; the gateway's "already
// claimed" conflict and a refusal on any other route stay uncertain.
func TestCreateRefusedIsDistinctFromClaimConflict(t *testing.T) {
	id := "blake3:" + strings.Repeat("b", 64)
	profile := "blake3:" + strings.Repeat("c", 64)
	for name, tc := range map[string]struct {
		body string
		want error
	}{
		"owner-refused": {`{"error":"parent creation refused; reconcile incarnation","incarnation":"` + id + `","retryAuthorized":false}`, ErrCreateRefused},
		"gateway-claim": {`{"error":"parent identity already claimed; reconcile original owner","retryAuthorized":false}`, ErrReconcile},
		"garbage":       {`refused`, ErrReconcile},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			token := filepath.Join(t.TempDir(), "token")
			if err := os.WriteFile(token, []byte("create-refused-test-credential-long"), 0600); err != nil {
				t.Fatal(err)
			}
			client, err := New(Config{BaseURL: server.URL, TokenFile: token})
			if err != nil {
				t.Fatal(err)
			}
			if err := client.CreateParent(context.Background(), profile, id); !errors.Is(err, tc.want) {
				t.Fatalf("create: got %v want %v", err, tc.want)
			}
			if tc.want == ErrCreateRefused {
				if err := client.StopParent(context.Background(), id); errors.Is(err, ErrCreateRefused) {
					t.Fatal("a refusal body on another route was treated as a create refusal")
				}
			}
		})
	}
}
