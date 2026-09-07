package harness

import (
	"context"
	"encoding/base64"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/sympozium-ai/sympozium/internal/artifact"
	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// BuildResponseAttachments scans the agent's final answer for files it
// produced (e.g. a generated chart or CSV) and returns them as attachments so
// the auto-relayed reply can deliver the files, not just a dead local path.
//
// Detection is deterministic and does not depend on the model choosing to call
// a tool: any local path the final answer references — as a Markdown link or a
// bare absolute path — that exists under an allowed root and fits the size
// budget is attached.
//
// Delivery has two modes. When ARTIFACT_SERVER_URL is set, bytes are uploaded
// to the artifact-server over HTTP and the attachment carries only a small
// ArtifactID reference — nothing large rides the event bus, so a generous
// per-file cap applies and there is no cumulative budget. When it is unset, the
// bytes are embedded inline as base64 and a conservative cumulative budget is
// enforced to stay under the event-bus max message size; anything beyond the
// budget is skipped, leaving the text reply intact.
//
// name is the harness name used to prefix diagnostics.
func BuildResponseAttachments(ctx context.Context, name, response, workspace string) []ipc.Attachment {
	if strings.TrimSpace(response) == "" {
		return nil
	}
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "" || workspace == "." {
		workspace = "/workspace"
	}

	const maxAttachments = 10

	uploader := artifact.NewClientFromEnv()
	perFileMax := resultAttachmentMaxBytes()
	if uploader != nil {
		perFileMax = artifactAttachmentMaxBytes()
	}
	totalBase64Budget := resultAttachmentTotalBase64Budget()

	var (
		out        []ipc.Attachment
		seen       = map[string]bool{}
		usedBase64 int64
	)

	for _, raw := range extractCandidateAttachmentPaths(response) {
		if len(out) >= maxAttachments {
			break
		}
		abs, ok := resolveResultAttachmentCandidate(raw, workspace)
		if !ok || seen[abs] {
			continue
		}
		seen[abs] = true

		info, err := os.Stat(abs)
		if err != nil || info.IsDir() || !info.Mode().IsRegular() {
			continue
		}
		if info.Size() <= 0 || info.Size() > perFileMax {
			continue
		}
		data, err := os.ReadFile(abs)
		if err != nil {
			continue
		}
		mimeType := detectAttachmentMimeType(abs, data)
		attType := "file"
		if strings.HasPrefix(mimeType, "image/") {
			attType = "image"
		}
		filename := filepath.Base(abs)

		if uploader != nil {
			id, size, err := uploader.Upload(ctx, filename, mimeType, data)
			if err != nil {
				// Never fail the reply over a failed upload; drop the file and
				// keep the text.
				Logf(name, "artifact upload failed for %s: %v", filename, err)
				continue
			}
			out = append(out, ipc.Attachment{
				Type:       attType,
				ArtifactID: id,
				Filename:   filename,
				MimeType:   mimeType,
				Size:       size,
			})
			continue
		}

		// Inline fallback (no artifact-server configured).
		b64 := base64.StdEncoding.EncodeToString(data)
		if usedBase64+int64(len(b64)) > totalBase64Budget {
			// Skip rather than risk exceeding the event bus max message size.
			continue
		}
		usedBase64 += int64(len(b64))
		out = append(out, ipc.Attachment{
			Type:          attType,
			ContentBase64: b64,
			Filename:      filename,
			MimeType:      mimeType,
			Size:          info.Size(),
		})
	}
	return out
}

// artifactAttachmentMaxBytes is the per-file cap when uploading to the
// artifact-server. Because bytes travel over HTTP rather than NATS, it defaults
// far higher than the inline base64 cap. Configurable via
// ARTIFACT_ATTACHMENT_MAX_BYTES.
func artifactAttachmentMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("ARTIFACT_ATTACHMENT_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return int64(25 * 1024 * 1024)
}

// markdownLinkTargetRE captures the target of a Markdown link: the text inside
// the parentheses of `](target)`, stopping at whitespace or the closing paren
// (so an optional `"title"` is excluded).
var markdownLinkTargetRE = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)`)

// bareAbsPathRE captures whitespace/bracket-delimited absolute paths that carry
// a file extension (e.g. `/workspace/chart.png`).
var bareAbsPathRE = regexp.MustCompile(`(/[^\s()\[\]<>"'` + "`" + `]+\.[A-Za-z0-9]{1,10})`)

// extractCandidateAttachmentPaths pulls likely local file references out of the
// final answer: Markdown link targets first (in order), then bare absolute
// paths. Scheme URLs (http://, s3://, …) are ignored — those are real
// hyperlinks, not local artifacts.
func extractCandidateAttachmentPaths(response string) []string {
	var candidates []string
	add := func(s string) {
		s = strings.Trim(strings.TrimSpace(s), "`'\"<>")
		if s == "" || strings.Contains(s, "://") {
			return
		}
		candidates = append(candidates, s)
	}
	for _, m := range markdownLinkTargetRE.FindAllStringSubmatch(response, -1) {
		add(m[1])
	}
	for _, m := range bareAbsPathRE.FindAllString(response, -1) {
		add(m)
	}
	return candidates
}

// resolveResultAttachmentCandidate cleans a candidate reference, resolves it to
// an absolute path (relative references resolve against the workspace), and
// verifies it lives under an allowed root. It returns the absolute path and
// whether it is acceptable.
func resolveResultAttachmentCandidate(raw, workspace string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var abs string
	if filepath.IsAbs(raw) {
		abs = filepath.Clean(raw)
	} else {
		abs = filepath.Clean(filepath.Join(workspace, raw))
	}
	if !isAllowedResultAttachmentPath(abs, workspace) {
		return "", false
	}
	return abs, true
}

// isAllowedResultAttachmentPath restricts attachable files to the workspace and
// /tmp so a referenced path can never exfiltrate arbitrary container files.
func isAllowedResultAttachmentPath(abs, workspace string) bool {
	roots := []string{workspace, "/workspace", "/tmp"}
	for _, root := range roots {
		root = filepath.Clean(root)
		if abs == root {
			return true
		}
		if strings.HasPrefix(abs, root+string(os.PathSeparator)) {
			return true
		}
	}
	return false
}

// detectAttachmentMimeType resolves a MIME type from the file extension, falling
// back to content sniffing.
func detectAttachmentMimeType(path string, data []byte) string {
	if ext := filepath.Ext(path); ext != "" {
		if mt := mime.TypeByExtension(ext); mt != "" {
			if i := strings.IndexByte(mt, ';'); i >= 0 {
				mt = mt[:i]
			}
			return strings.TrimSpace(mt)
		}
	}
	sniff := data
	if len(sniff) > 512 {
		sniff = sniff[:512]
	}
	return http.DetectContentType(sniff)
}

// resultAttachmentMaxBytes is the per-file cap, shared with the send-message
// tooling via CHANNEL_ATTACHMENT_MAX_BYTES (default 768000).
func resultAttachmentMaxBytes() int64 {
	if v := strings.TrimSpace(os.Getenv("CHANNEL_ATTACHMENT_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return int64(750 * 1024)
}

// resultAttachmentTotalBase64Budget caps the cumulative base64 size of all
// embedded attachments on a single completion event, keeping it comfortably
// under the NATS/JetStream max message size (default ~1 MB). Configurable via
// CHANNEL_ATTACHMENT_TOTAL_MAX_BYTES (measured in base64 bytes).
func resultAttachmentTotalBase64Budget() int64 {
	if v := strings.TrimSpace(os.Getenv("CHANNEL_ATTACHMENT_TOTAL_MAX_BYTES")); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return int64(900 * 1000)
}
