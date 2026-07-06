package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// stubAuth maps bearer tokens to identities for handler tests, standing in for
// the live TokenReview path.
func stubAuth(tokens map[string]identity) authenticateFunc {
	return func(_ context.Context, token string) (identity, error) {
		if id, ok := tokens[token]; ok {
			return id, nil
		}
		return identity{}, errors.New("unauthorized")
	}
}

func newTestServer(t *testing.T, tokens map[string]identity) *server {
	t.Helper()
	st, err := newStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cfg := &Config{
		MaxBytes:        1024,
		TTL:             time.Hour,
		AgentSASuffix:   "-agent",
		ChannelSASuffix: "-channel",
	}
	return newServer(cfg, st, stubAuth(tokens))
}

func upload(t *testing.T, h http.Handler, token, filename, contentType string, body []byte) *http.Response {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/artifacts", bytes.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("X-Artifact-Filename", filename)
	req.Header.Set("Content-Type", contentType)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Result()
}

func TestUploadDownloadRoundTrip(t *testing.T) {
	agent := identity{Namespace: "team-x", ServiceAccountName: "analyst-agent"}
	channel := identity{Namespace: "team-x", ServiceAccountName: "analyst-channel"}
	h := newTestServer(t, map[string]identity{"agent-tok": agent, "chan-tok": channel}).handler()

	body := []byte("PNGBYTES")
	resp := upload(t, h, "agent-tok", "chart.png", "image/png", body)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload status = %d, want 201", resp.StatusCode)
	}
	var ur uploadResponse
	if err := json.NewDecoder(resp.Body).Decode(&ur); err != nil {
		t.Fatal(err)
	}
	if ur.ID == "" || ur.Size != int64(len(body)) {
		t.Fatalf("bad upload response: %+v", ur)
	}

	// Sibling channel pod downloads by id.
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+ur.ID, nil)
	req.Header.Set("Authorization", "Bearer chan-tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("download status = %d, want 200", rr.Code)
	}
	got, _ := io.ReadAll(rr.Body)
	if !bytes.Equal(got, body) {
		t.Fatalf("downloaded %q, want %q", got, body)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "image/png" {
		t.Fatalf("content-type = %q, want image/png", ct)
	}
}

func TestDownloadUnauthenticatedIs401(t *testing.T) {
	h := newTestServer(t, map[string]identity{}).handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+"00000000000000000000000000000000", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rr.Code)
	}
}

func TestDownloadUnknownIDIs404(t *testing.T) {
	reader := identity{Namespace: "team-x", ServiceAccountName: "analyst-agent"}
	h := newTestServer(t, map[string]identity{"tok": reader}).handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+"deadbeefdeadbeefdeadbeefdeadbeef", nil)
	req.Header.Set("Authorization", "Bearer tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rr.Code)
	}
}

func TestDownloadForbiddenForStranger(t *testing.T) {
	agent := identity{Namespace: "team-x", ServiceAccountName: "analyst-agent"}
	stranger := identity{Namespace: "team-x", ServiceAccountName: "intruder-agent"}
	h := newTestServer(t, map[string]identity{"agent-tok": agent, "bad-tok": stranger}).handler()

	resp := upload(t, h, "agent-tok", "secret.csv", "text/csv", []byte("secret"))
	var ur uploadResponse
	_ = json.NewDecoder(resp.Body).Decode(&ur)

	req := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+ur.ID, nil)
	req.Header.Set("Authorization", "Bearer bad-tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rr.Code)
	}
}

func TestUploadTooLargeIs413(t *testing.T) {
	agent := identity{Namespace: "team-x", ServiceAccountName: "analyst-agent"}
	h := newTestServer(t, map[string]identity{"agent-tok": agent}).handler()
	resp := upload(t, h, "agent-tok", "big.bin", "application/octet-stream", bytes.Repeat([]byte("A"), 2048))
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413", resp.StatusCode)
	}
}

func TestDeleteBySiblingChannel(t *testing.T) {
	agent := identity{Namespace: "team-x", ServiceAccountName: "analyst-agent"}
	channel := identity{Namespace: "team-x", ServiceAccountName: "analyst-channel"}
	h := newTestServer(t, map[string]identity{"agent-tok": agent, "chan-tok": channel}).handler()

	resp := upload(t, h, "agent-tok", "chart.png", "image/png", []byte("data"))
	var ur uploadResponse
	_ = json.NewDecoder(resp.Body).Decode(&ur)

	req := httptest.NewRequest(http.MethodDelete, "/v1/artifacts/"+ur.ID, nil)
	req.Header.Set("Authorization", "Bearer chan-tok")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", rr.Code)
	}

	// Second download should now 404.
	req2 := httptest.NewRequest(http.MethodGet, "/v1/artifacts/"+ur.ID, nil)
	req2.Header.Set("Authorization", "Bearer chan-tok")
	rr2 := httptest.NewRecorder()
	h.ServeHTTP(rr2, req2)
	if rr2.Code != http.StatusNotFound {
		t.Fatalf("post-delete download status = %d, want 404", rr2.Code)
	}
}

func TestHealthzUnauthenticated(t *testing.T) {
	h := newTestServer(t, map[string]identity{}).handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("healthz status = %d, want 200", rr.Code)
	}
}
