package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// tinyPNG is a minimal valid PNG (1x1) used to exercise image detection.
var tinyPNG = []byte{
	0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A,
	0x00, 0x00, 0x00, 0x0D, 0x49, 0x48, 0x44, 0x52,
	0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
	0x08, 0x06, 0x00, 0x00, 0x00, 0x1F, 0x15, 0xC4,
	0x89, 0x00, 0x00, 0x00, 0x0A, 0x49, 0x44, 0x41,
	0x54, 0x78, 0x9C, 0x63, 0x00, 0x01, 0x00, 0x00,
	0x05, 0x00, 0x01, 0x0D, 0x0A, 0x2D, 0xB4, 0x00,
	0x00, 0x00, 0x00, 0x49, 0x45, 0x4E, 0x44, 0xAE,
	0x42, 0x60, 0x82,
}

func TestBuildResponseAttachments_MarkdownAndBare(t *testing.T) {
	ws := t.TempDir()
	pngPath := filepath.Join(ws, "chart.png")
	if err := os.WriteFile(pngPath, tinyPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	csvPath := filepath.Join(ws, "data.csv")
	if err := os.WriteFile(csvPath, []byte("a,b\n1,2\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// PNG referenced as a Markdown link (relative), CSV as a bare absolute path.
	resp := "Here is the chart: [chart.png](chart.png)\nData: " + csvPath

	atts := buildResponseAttachments(context.Background(), resp, ws)
	if len(atts) != 2 {
		t.Fatalf("expected 2 attachments, got %d: %+v", len(atts), atts)
	}

	byName := map[string]string{} // filename -> type
	for _, a := range atts {
		if a.ContentBase64 == "" {
			t.Errorf("attachment %q has empty content", a.Filename)
		}
		byName[a.Filename] = a.Type
	}
	if byName["chart.png"] != "image" {
		t.Errorf("chart.png type = %q, want image", byName["chart.png"])
	}
	if byName["data.csv"] != "file" {
		t.Errorf("data.csv type = %q, want file", byName["data.csv"])
	}
}

func TestBuildResponseAttachments_IgnoresURLsAndDisallowedPaths(t *testing.T) {
	ws := t.TempDir()
	resp := "See [remote](https://example.com/x.png) and /etc/shadow and /var/secret.pem"
	if atts := buildResponseAttachments(context.Background(), resp, ws); len(atts) != 0 {
		t.Fatalf("expected no attachments, got %d: %+v", len(atts), atts)
	}
}

func TestBuildResponseAttachments_Dedup(t *testing.T) {
	ws := t.TempDir()
	p := filepath.Join(ws, "chart.png")
	if err := os.WriteFile(p, tinyPNG, 0o644); err != nil {
		t.Fatal(err)
	}
	resp := "[a](chart.png) again [b](chart.png) and " + p
	atts := buildResponseAttachments(context.Background(), resp, ws)
	if len(atts) != 1 {
		t.Fatalf("expected 1 deduped attachment, got %d", len(atts))
	}
}

func TestBuildResponseAttachments_PerFileSizeCap(t *testing.T) {
	ws := t.TempDir()
	big := filepath.Join(ws, "big.bin")
	if err := os.WriteFile(big, make([]byte, 2000), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CHANNEL_ATTACHMENT_MAX_BYTES", "1000")
	if atts := buildResponseAttachments(context.Background(), ws+"/big.bin", ws); len(atts) != 0 {
		t.Fatalf("expected oversized file to be skipped, got %d", len(atts))
	}
}

func TestBuildResponseAttachments_TotalBudget(t *testing.T) {
	ws := t.TempDir()
	for _, n := range []string{"a.png", "b.png"} {
		if err := os.WriteFile(filepath.Join(ws, n), tinyPNG, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Budget large enough for one base64-encoded tiny PNG but not two.
	one := (len(tinyPNG)+2)/3*4 + 4
	t.Setenv("CHANNEL_ATTACHMENT_TOTAL_MAX_BYTES", strconv.Itoa(one))
	resp := "[a](a.png) [b](b.png)"
	atts := buildResponseAttachments(context.Background(), resp, ws)
	if len(atts) != 1 {
		t.Fatalf("expected budget to allow exactly 1 attachment, got %d", len(atts))
	}
}

func TestExtractCandidateAttachmentPaths_Order(t *testing.T) {
	resp := "link [x](/workspace/x.png) then bare /tmp/y.csv end"
	got := extractCandidateAttachmentPaths(resp)
	if len(got) == 0 || got[0] != "/workspace/x.png" {
		t.Fatalf("expected markdown target first, got %v", got)
	}
	joined := strings.Join(got, ",")
	if !strings.Contains(joined, "/tmp/y.csv") {
		t.Errorf("expected bare path captured, got %v", got)
	}
}

func TestBuildResponseAttachments_UploadsToArtifactServer(t *testing.T) {
	ws := t.TempDir()
	pngPath := filepath.Join(ws, "chart.png")
	if err := os.WriteFile(pngPath, tinyPNG, 0o644); err != nil {
		t.Fatal(err)
	}

	// Stub artifact-server: verify auth + filename headers, return an id.
	var gotAuth, gotFilename string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotFilename = r.Header.Get("X-Artifact-Filename")
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"id":"deadbeefdeadbeefdeadbeefdeadbeef","size":` + strconv.Itoa(len(body)) + `}`))
	}))
	defer srv.Close()

	tokFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokFile, []byte("test-sa-token"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ARTIFACT_SERVER_URL", srv.URL)
	t.Setenv("SA_TOKEN_PATH", tokFile)

	atts := buildResponseAttachments(context.Background(), "chart at [c](chart.png)", ws)
	if len(atts) != 1 {
		t.Fatalf("expected 1 attachment, got %d", len(atts))
	}
	a := atts[0]
	if a.ArtifactID != "deadbeefdeadbeefdeadbeefdeadbeef" {
		t.Errorf("ArtifactID = %q, want the stub id", a.ArtifactID)
	}
	if a.ContentBase64 != "" {
		t.Errorf("expected no inline base64 when uploading, got %d bytes", len(a.ContentBase64))
	}
	if a.Type != "image" || a.Filename != "chart.png" {
		t.Errorf("unexpected attachment metadata: %+v", a)
	}
	if gotAuth != "Bearer test-sa-token" {
		t.Errorf("upload auth header = %q, want bearer token", gotAuth)
	}
	if gotFilename != "chart.png" {
		t.Errorf("upload filename header = %q, want chart.png", gotFilename)
	}
}
