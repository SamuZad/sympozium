package harness

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// WriteResult publishes the run result over both channels the platform reads:
//
//  1. Atomically writes it to RESULT_PATH (default /ipc/output/result.json),
//     the file the IPC bridge watches to publish the AgentRun completion event
//     that channels (Slack, ...) deliver.
//  2. Emits a single run.completed/run.failed JSONL event to stdout, which is
//     what the controller parses out of the pod logs to fill AgentRun status.
//
// The log event is emitted even when the file write fails: the log path does not
// depend on the IPC volume, so the controller can still surface the result.
func WriteResult(res ipc.AgentResult) error {
	return writeResult(res, EnvOr("RESULT_PATH", "/ipc/output/result.json"), os.Stdout)
}

func writeResult(res ipc.AgentResult, out string, stdout io.Writer) error {
	fileErr := writeResultFile(res, out)

	// The controller only reads status/response/error/metrics from the marker
	// and attachments may carry inline base64 file bytes, so leave them out of
	// the log line (matching the marker cmd/agent-runner prints). The IPC file
	// above keeps them for channel delivery.
	forLogs := res
	forLogs.Attachments = nil
	statusEvent := "run.completed"
	level := "info"
	message := "Agent run completed"
	if forLogs.Status == "error" {
		statusEvent, level, message = "run.failed", "error", "Agent run failed"
	}
	EmitLog(stdout, EnvOr("HARNESS_NAME", ""), level, statusEvent, message, "stdout", map[string]any{"result": forLogs})

	return fileErr
}

func writeResultFile(res ipc.AgentResult, out string) error {
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	tmp := out + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, out)
}
