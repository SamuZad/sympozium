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

// parseResultEvent mirrors the controller's final JSONL event extraction.
func parseResultEvent(t *testing.T, logs string) (status, response, errMsg string, durationMs int64) {
	t.Helper()
	var parsed struct {
		Event string `json:"event"`
		Data  struct {
			Result struct {
				Status   string `json:"status"`
				Response string `json:"response"`
				Error    string `json:"error"`
				Metrics  struct {
					DurationMs int64 `json:"durationMs"`
				} `json:"metrics"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(logs)), &parsed); err != nil {
		t.Fatalf("result event is not valid JSON: %v\n%s", err, logs)
	}
	if parsed.Event != "run.completed" && parsed.Event != "run.failed" {
		t.Fatalf("event = %q, want final run event", parsed.Event)
	}
	return parsed.Data.Result.Status, parsed.Data.Result.Response, parsed.Data.Result.Error, parsed.Data.Result.Metrics.DurationMs
}

func TestWriteResultWritesFileAndEmitsJSONL(t *testing.T) {
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
	status, response, errMsg, durationMs := parseResultEvent(t, stdout.String())
	if status != "success" || response != res.Response || errMsg != "" || durationMs != 1234 {
		t.Fatalf("marker mismatch: status=%q response=%q err=%q durationMs=%d", status, response, errMsg, durationMs)
	}

	if strings.Count(stdout.String(), "\n") != 1 {
		t.Fatalf("result event must be one JSONL line, got:\n%s", stdout.String())
	}
}

func TestWriteResultEmitsJSONLOnError(t *testing.T) {
	out := filepath.Join(t.TempDir(), "output", "result.json")
	var stdout bytes.Buffer

	res := ipc.AgentResult{Status: "error", Error: "codex exec failed: exit status 1", Response: "partial"}

	if err := writeResult(res, out, &stdout); err != nil {
		t.Fatalf("writeResult: %v", err)
	}
	status, response, errMsg, _ := parseResultEvent(t, stdout.String())
	if status != "error" || errMsg != res.Error || response != "partial" {
		t.Fatalf("marker mismatch: status=%q response=%q err=%q", status, response, errMsg)
	}
}

func TestWriteResultEventOmitsAttachments(t *testing.T) {
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

	// Log event carries the reply but not the attachment payload.
	logs := stdout.String()
	if strings.Contains(logs, "attachments") || strings.Contains(logs, "QUJDQUJD") {
		t.Fatalf("result event must not include attachments:\n%s", logs)
	}
	status, response, _, _ := parseResultEvent(t, logs)
	if status != "success" || response != res.Response {
		t.Fatalf("marker mismatch: status=%q response=%q", status, response)
	}
}

func TestWriteResultEmitsEventWhenFileWriteFails(t *testing.T) {
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
	status, response, _, _ := parseResultEvent(t, stdout.String())
	if status != "success" || response != res.Response {
		t.Fatalf("event should be emitted despite file error, got status=%q response=%q", status, response)
	}
}

func TestProcessOutputWriterAlwaysEmitsJSONL(t *testing.T) {
	var out bytes.Buffer
	w := NewProcessOutputWriter(&out, "codex", "stdout")
	if _, err := w.Write([]byte(`{"type":"item.completed"}` + "\nplain text")); err != nil {
		t.Fatal(err)
	}
	w.Flush()

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2: %s", len(lines), out.String())
	}
	for _, line := range lines {
		var event LogEvent
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			t.Fatalf("not valid JSONL: %v", err)
		}
		if event.Message != "Native process output" || event.Event != "process.output" || event.Harness != "codex" {
			t.Fatalf("unexpected envelope: %+v", event)
		}
	}
	var first map[string]any
	_ = json.Unmarshal([]byte(lines[0]), &first)
	if first["data"].(map[string]any)["native_event"] == nil {
		t.Fatalf("native JSON was not preserved: %s", lines[0])
	}
	var second map[string]any
	_ = json.Unmarshal([]byte(lines[1]), &second)
	if second["data"].(map[string]any)["raw_message"] != "plain text" {
		t.Fatalf("plain text was not preserved: %s", lines[1])
	}
}
