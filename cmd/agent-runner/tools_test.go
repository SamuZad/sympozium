package main

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNormalizeSidecarTarget(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"plain lowercase", "github-gitops", "github-gitops"},
		{"mixed case", "Github-Gitops", "github-gitops"},
		{"upper case", "GITHUB-GITOPS", "github-gitops"},
		{"surrounding whitespace", "  github-gitops\n", "github-gitops"},
		{"tab and newline", "\tgithub-gitops\n", "github-gitops"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := normalizeSidecarTarget(c.in)
			if got != c.want {
				t.Fatalf("normalizeSidecarTarget(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// TestExecRequestJSONIncludesTarget locks in the IPC protocol contract: when
// Target is set, the JSON payload written to /ipc/tools/exec-request-*.json
// MUST contain a top-level "target" field with the literal string value. The
// skill-sidecar tool-executor scripts depend on this field name.
func TestExecRequestJSONIncludesTarget(t *testing.T) {
	req := execRequest{
		ID:      "req-1",
		Command: "gh issue list",
		WorkDir: "/workspace",
		Timeout: 30,
		Target:  "github-gitops",
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var generic map[string]any
	if err := json.Unmarshal(data, &generic); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got, ok := generic["target"].(string)
	if !ok {
		t.Fatalf("target field missing or not a string in JSON: %s", string(data))
	}
	if got != "github-gitops" {
		t.Fatalf("target = %q, want %q", got, "github-gitops")
	}
}

// TestExecRequestJSONOmitsEmptyTarget verifies the legacy compatibility path:
// when Target is empty, the JSON payload MUST NOT contain a "target" key. Old
// (unmigrated) sidecar images do not understand the field; emitting an empty
// string would still cause `jq -r '.target // ""'` to behave correctly, but
// the omitempty tag preserves byte-level compatibility with the pre-fix
// protocol so existing parsers / fixtures see no diff.
func TestExecRequestJSONOmitsEmptyTarget(t *testing.T) {
	req := execRequest{
		ID:      "req-2",
		Command: "kubectl get pods",
		WorkDir: "/workspace",
		Timeout: 30,
	}
	data, err := json.Marshal(req)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(data), `"target"`) {
		t.Fatalf("expected no target key in JSON when empty, got: %s", string(data))
	}
}

// TestExecuteCommandToolDefAdvertisesTarget asserts the tool schema exposed to
// the LLM continues to advertise an optional `target` parameter and that
// `command` remains the only required field. This guards against accidental
// schema regressions that would either drop target routing or break callers
// that omit target.
func TestExecuteCommandToolDefAdvertisesTarget(t *testing.T) {
	var def *ToolDef
	for i := range defaultTools() {
		td := defaultTools()[i]
		if td.Name == ToolExecuteCommand {
			def = &td
			break
		}
	}
	if def == nil {
		t.Fatalf("execute_command tool not found in defaultTools()")
	}
	props, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type in execute_command schema")
	}
	if _, ok := props["target"]; !ok {
		t.Fatalf("execute_command schema is missing the optional 'target' property: %v", props)
	}
	required, _ := def.Parameters["required"].([]string)
	for _, r := range required {
		if r == "target" {
			t.Fatalf("'target' must be optional, but appears in required: %v", required)
		}
	}
}

func TestSendChannelMessageToolDefAdvertisesAttachments(t *testing.T) {
	var def *ToolDef
	tools := defaultTools()
	for i := range tools {
		if tools[i].Name == ToolSendChannelMessage {
			def = &tools[i]
			break
		}
	}
	if def == nil {
		t.Fatalf("send_channel_message tool not found in defaultTools()")
	}
	props, ok := def.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("properties missing or wrong type in send_channel_message schema")
	}
	if _, ok := props["attachments"]; !ok {
		t.Fatalf("send_channel_message schema is missing optional attachments property: %v", props)
	}
	required, _ := def.Parameters["required"].([]string)
	for _, r := range required {
		if r == "attachments" {
			t.Fatalf("attachments must be optional, but appears in required: %v", required)
		}
	}
}

func TestParseChannelAttachments(t *testing.T) {
	attachments, errText := parseChannelAttachments([]any{
		map[string]any{"url": "https://example.com/chart.png", "filename": "chart.png", "mimeType": "image/png"},
	})
	if errText != "" {
		t.Fatalf("parseChannelAttachments returned error: %s", errText)
	}
	if len(attachments) != 1 {
		t.Fatalf("len(attachments) = %d, want 1", len(attachments))
	}
	if attachments[0].Type != "image" || attachments[0].URL != "https://example.com/chart.png" || attachments[0].Filename != "chart.png" || attachments[0].MimeType != "image/png" {
		t.Fatalf("attachment = %+v", attachments[0])
	}

	_, errText = parseChannelAttachments([]any{map[string]any{"url": "http://example.com/insecure.png"}})
	if !strings.Contains(errText, "https://") {
		t.Fatalf("expected https validation error, got %q", errText)
	}
}

func TestParseChannelAttachmentsReadsLocalPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "sympozium-attachment-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	path := filepath.Join(tmpDir, "chart.png")
	if err := os.WriteFile(path, pngBytes, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	attachments, errText := parseChannelAttachments([]any{map[string]any{"path": path}})
	if errText != "" {
		t.Fatalf("parseChannelAttachments returned error: %s", errText)
	}
	if len(attachments) != 1 {
		t.Fatalf("len(attachments) = %d, want 1", len(attachments))
	}
	attachment := attachments[0]
	if attachment.Type != "image" || attachment.URL != "" || attachment.Filename != "chart.png" || attachment.MimeType != "image/png" {
		t.Fatalf("attachment metadata = %+v", attachment)
	}
	decoded, err := base64.StdEncoding.DecodeString(attachment.ContentBase64)
	if err != nil {
		t.Fatalf("decode ContentBase64: %v", err)
	}
	if string(decoded) != string(pngBytes) {
		t.Fatalf("decoded attachment content = %v, want %v", decoded, pngBytes)
	}
}

func TestParseChannelAttachmentsHonorsConfiguredMaxBytes(t *testing.T) {
	t.Setenv(channelAttachmentMaxBytesEnv, "8")
	tmpDir, err := os.MkdirTemp("/tmp", "sympozium-attachment-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	path := filepath.Join(tmpDir, "chart.png")
	if err := os.WriteFile(path, []byte("more than eight bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, errText := parseChannelAttachments([]any{map[string]any{"path": path}})
	if !strings.Contains(errText, "max is 8 bytes") {
		t.Fatalf("expected configured max-bytes error, got %q", errText)
	}
}
