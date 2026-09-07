package harness

import (
	"encoding/json"
	"os"
	"path/filepath"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// WriteResult atomically writes the run result to RESULT_PATH (default
// /ipc/output/result.json), the file the IPC bridge watches to publish the
// AgentRun completion event.
func WriteResult(res ipc.AgentResult) error {
	out := EnvOr("RESULT_PATH", "/ipc/output/result.json")
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
