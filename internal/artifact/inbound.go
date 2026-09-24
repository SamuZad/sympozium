package artifact

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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
		fmt.Fprintf(os.Stderr, "artifact: invalid INBOUND_ATTACHMENTS: %v\n", err)
		return nil
	}
	client := NewClientFromEnv()
	if client == nil {
		fmt.Fprintln(os.Stderr, "artifact: inbound attachments present but artifact-server is not configured")
		return nil
	}
	dir := filepath.Join(workspaceDir, "attachments")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "artifact: mkdir %s: %v\n", dir, err)
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
			fmt.Fprintf(os.Stderr, "artifact: fetch inbound attachment %s: %v\n", ref.ArtifactID, err)
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
			fmt.Fprintf(os.Stderr, "artifact: write %s: %v\n", path, err)
			continue
		}
		saved = append(saved, path)
	}
	return saved
}
