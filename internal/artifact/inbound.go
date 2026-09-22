package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sympozium-ai/sympozium/internal/jsonlog"
)

// InboundRef mirrors the artifact-referencing fields of channel.Attachment as
// serialized into the INBOUND_ATTACHMENTS env var by the channel router.
type InboundRef struct {
	ArtifactID string `json:"artifactId"`
	Filename   string `json:"filename"`
	MimeType   string `json:"mimeType"`
	Size       int64  `json:"size"`
}

// MaterializeInbound downloads artifact-backed attachments from the
// triggering channel message into <workspaceDir>/attachments/ and returns the
// saved paths. Best-effort: failures are logged and skipped so the run still
// starts with the message text.
func MaterializeInbound(ctx context.Context, workspaceDir string) []string {
	raw := strings.TrimSpace(os.Getenv("INBOUND_ATTACHMENTS"))
	if raw == "" {
		return nil
	}
	var refs []InboundRef
	if err := json.Unmarshal([]byte(raw), &refs); err != nil {
		jsonlog.Emit(os.Stderr, "artifact", os.Getenv("HARNESS_NAME"), "warn", "artifact.inbound.invalid", "Invalid inbound attachment metadata", "stderr", map[string]any{"error": err.Error()})
		return nil
	}
	client := NewClientFromEnv()
	if client == nil {
		jsonlog.Emit(os.Stderr, "artifact", os.Getenv("HARNESS_NAME"), "warn", "artifact.inbound.unavailable", "Inbound attachments cannot be materialized", "stderr", nil)
		return nil
	}
	dir := filepath.Join(workspaceDir, "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		jsonlog.Emit(os.Stderr, "artifact", os.Getenv("HARNESS_NAME"), "warn", "artifact.inbound.mkdir_failed", "Could not create attachment directory", "stderr", map[string]any{"error": err.Error(), "path": dir})
		return nil
	}
	var saved []string
	seen := map[string]bool{}
	for i, ref := range refs {
		if ref.ArtifactID == "" {
			continue
		}
		data, fetchedName, err := client.Fetch(ctx, ref.ArtifactID)
		if err != nil {
			jsonlog.Emit(os.Stderr, "artifact", os.Getenv("HARNESS_NAME"), "warn", "artifact.inbound.fetch_failed", "Could not fetch inbound attachment", "stderr", map[string]any{"error": err.Error(), "artifact_id": ref.ArtifactID})
			continue
		}
		name := filepath.Base(strings.TrimSpace(ref.Filename))
		if name == "" || name == "." || name == string(os.PathSeparator) {
			name = filepath.Base(strings.TrimSpace(fetchedName))
		}
		if name == "" || name == "." || name == string(os.PathSeparator) {
			name = ref.ArtifactID
		}
		if seen[name] {
			name = fmt.Sprintf("%d-%s", i, name)
		}
		seen[name] = true
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, data, 0o644); err != nil {
			jsonlog.Emit(os.Stderr, "artifact", os.Getenv("HARNESS_NAME"), "warn", "artifact.inbound.write_failed", "Could not write inbound attachment", "stderr", map[string]any{"error": err.Error(), "path": path})
			continue
		}
		saved = append(saved, path)
	}
	return saved
}
