package harness

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// parseMarker mirrors parseAgentResultFromLogs in internal/controller: take the
// last marker start, the first marker end after it, and unmarshal the JSON in
// between.
func parseMarker(t *testing.T, logs string) (status, response, errMsg string, durationMs int64) {
	t.Helper()
	start := strings.LastIndex(logs, ResultMarkerStart)
	if start < 0 {
		t.Fatalf("stdout has no %s marker:\n%s", ResultMarkerStart, logs)
	}
	payload := logs[start+len(ResultMarkerStart):]
	end := strings.Index(payload, ResultMarkerEnd)
	if end < 0 {
		t.Fatalf("stdout has no %s marker after start:\n%s", ResultMarkerEnd, logs)
	}
	var parsed struct {
		Status   string `json:"status"`
		Response string `json:"response"`
		Error    string `json:"error"`
		Metrics  struct {
			DurationMs int64 `json:"durationMs"`
		} `json:"metrics"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(payload[:end])), &parsed); err != nil {
		t.Fatalf("marker payload is not valid JSON: %v\n%s", err, payload[:end])
	}
	return parsed.Status, parsed.Response, parsed.Error, parsed.Metrics.DurationMs
}

func TestWriteResultWritesFileAndPrintsMarker(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output", "result.json")
	var stdout bytes.Buffer

	res := ipc.AgentResult{Status: "success", Response: "The SDK lives in repo X.\nSee line 2."}
	res.Metrics.DurationMs = 1234

	if err := writeResult(res, out, &stdout); err != nil {
		t.Fatalf("writeResult: %v", err)
	}

	// File path: what the IPC bridge publishes to channels.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("result.json not written: %v", err)
	}
	var fromFile ipc.AgentResult
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatalf("result.json is not valid JSON: %v", err)
	}
	if fromFile.Response != res.Response || fromFile.Status != "success" {
		t.Fatalf("result.json content mismatch: %+v", fromFile)
	}
	if _, err := os.Stat(out + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temp file left behind: %v", err)
	}

	// Log path: what the controller parses into status.result.
	status, response, errMsg, durationMs := parseMarker(t, stdout.String())
	if status != "success" || response != res.Response || errMsg != "" || durationMs != 1234 {
		t.Fatalf("marker mismatch: status=%q response=%q err=%q durationMs=%d", status, response, errMsg, durationMs)
	}

	// The marker must be a single line so it survives the controller's
	// TailLines window intact, and it must start on its own line.
	line := stdout.String()
	if !strings.HasPrefix(line, "\n") {
		t.Fatalf("marker should start on a fresh line, got %q", line[:1])
	}
	body := strings.TrimSpace(line)
	if strings.Contains(body, "\n") {
		t.Fatalf("marker must be single-line, got:\n%s", body)
	}
}

func TestWriteResultPrintsMarkerOnError(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output", "result.json")
	var stdout bytes.Buffer

	res := ipc.AgentResult{Status: "error", Error: "codex exec failed: exit status 1", Response: "partial"}

	if err := writeResult(res, out, &stdout); err != nil {
		t.Fatalf("writeResult: %v", err)
	}
	status, response, errMsg, _ := parseMarker(t, stdout.String())
	if status != "error" || errMsg != res.Error || response != "partial" {
		t.Fatalf("marker mismatch: status=%q response=%q err=%q", status, response, errMsg)
	}
}

func TestWriteResultMarkerOmitsAttachments(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output", "result.json")
	var stdout bytes.Buffer

	res := ipc.AgentResult{Status: "success", Response: "see attached"}
	res.Attachments = []ipc.Attachment{{
		Type:          "file",
		Filename:      "query.sql",
		MimeType:      "application/sql",
		ContentBase64: strings.Repeat("QUJD", 1024), // inline bytes, must not reach pod logs
	}}

	if err := writeResult(res, out, &stdout); err != nil {
		t.Fatalf("writeResult: %v", err)
	}

	// File keeps the attachments for the IPC bridge / channel delivery.
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var fromFile ipc.AgentResult
	if err := json.Unmarshal(data, &fromFile); err != nil {
		t.Fatal(err)
	}
	if len(fromFile.Attachments) != 1 || fromFile.Attachments[0].ContentBase64 == "" {
		t.Fatalf("result.json lost attachments: %+v", fromFile.Attachments)
	}

	// Marker carries the reply but not the attachment payload.
	logs := stdout.String()
	if strings.Contains(logs, "attachments") || strings.Contains(logs, "QUJDQUJD") {
		t.Fatalf("marker must not include attachments:\n%s", logs)
	}
	status, response, _, _ := parseMarker(t, logs)
	if status != "success" || response != res.Response {
		t.Fatalf("marker mismatch: status=%q response=%q", status, response)
	}
}

func TestWriteResultPrintsMarkerWhenFileWriteFails(t *testing.T) {
	// A regular file where the output *directory* should be makes MkdirAll fail,
	// simulating a missing/unwritable IPC volume.
	blocker := filepath.Join(t.TempDir(), "output")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(blocker, "result.json")
	var stdout bytes.Buffer

	res := ipc.AgentResult{Status: "success", Response: "still delivered via logs"}
	err := writeResult(res, out, &stdout)
	if err == nil {
		t.Fatal("expected file write error")
	}
	status, response, _, _ := parseMarker(t, stdout.String())
	if status != "success" || response != res.Response {
		t.Fatalf("marker should be printed despite file error, got status=%q response=%q", status, response)
	}
}
