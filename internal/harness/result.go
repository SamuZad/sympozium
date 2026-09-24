package harness

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// Structured result marker printed to stdout. The AgentRun controller tails the
// agent container logs and parses the JSON between these two markers to
// populate AgentRun.status.result and status.tokenUsage. They must match
// resultMarkerStart / resultMarkerEnd in internal/controller and the marker
// printed by cmd/agent-runner.
const (
	ResultMarkerStart = "__SYMPOZIUM_RESULT__"
	ResultMarkerEnd   = "__SYMPOZIUM_END__"
)

// WriteResult publishes the run result over both channels the platform reads:
//
//  1. Atomically writes it to RESULT_PATH (default /ipc/output/result.json),
//     the file the IPC bridge watches to publish the AgentRun completion event
//     that channels (Slack, ...) deliver.
//  2. Prints a single-line __SYMPOZIUM_RESULT__ marker to stdout, which is
//     what the controller parses out of the pod logs to fill
//     AgentRun.status.result. Without it a harness run completes with an empty
//     result even though the reply reached the channel.
//
// The marker is printed even when the file write fails: the log path does not
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
	if marker, err := json.Marshal(forLogs); err == nil {
		// Leading newline keeps the marker on its own line even if the wrapped
		// CLI left stdout mid-line; the controller uses LastIndex so anything
		// printed before it is ignored.
		fmt.Fprintf(stdout, "\n%s%s%s\n", ResultMarkerStart, string(marker), ResultMarkerEnd)
	} else if fileErr == nil {
		fileErr = err
	}

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
