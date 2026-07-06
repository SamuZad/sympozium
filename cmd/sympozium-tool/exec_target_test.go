package main

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveExecTarget(t *testing.T) {
	inventory := "agent-nxg-helper-github-gitops,agent-nxg-helper-nxg-essential-tools"
	cases := []struct {
		name      string
		input     string
		inventory string
		want      string
		errSubstr string
	}{
		{name: "empty input", input: "", inventory: inventory, want: ""},
		{name: "legacy no inventory passthrough", input: "nxg-essential-tools", inventory: "", want: "nxg-essential-tools"},
		{name: "exact match", input: "agent-nxg-helper-nxg-essential-tools", inventory: inventory, want: "agent-nxg-helper-nxg-essential-tools"},
		{name: "unique suffix", input: "nxg-essential-tools", inventory: inventory, want: "agent-nxg-helper-nxg-essential-tools"},
		{name: "case and space normalized", input: " NXG-ESSENTIAL-TOOLS ", inventory: inventory, want: "agent-nxg-helper-nxg-essential-tools"},
		{
			name:      "ambiguous suffix",
			input:     "tools",
			inventory: "agent-a-tools,agent-b-tools",
			errSubstr: "ambiguous",
		},
		{
			name:      "unknown target",
			input:     "missing-tools",
			inventory: inventory,
			errSubstr: "unknown target",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExecTarget(tc.input, tc.inventory)
			if tc.errSubstr != "" {
				if err == nil {
					t.Fatalf("resolveExecTarget returned nil error, want %q", tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveExecTarget: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveExecTarget(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

func TestBuildSendMessageAttachmentsReadsLocalPath(t *testing.T) {
	tmpDir, err := os.MkdirTemp("/tmp", "sympozium-tool-attachment-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	pngBytes := []byte{0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a, 0x00, 0x00, 0x00, 0x0d}
	path := filepath.Join(tmpDir, "chart.png")
	if err := os.WriteFile(path, pngBytes, 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	attachments, err := buildSendMessageAttachments([]string{path}, nil)
	if err != nil {
		t.Fatalf("buildSendMessageAttachments: %v", err)
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

func TestBuildSendMessageAttachmentsHonorsConfiguredMaxBytes(t *testing.T) {
	t.Setenv(channelAttachmentMaxBytesEnv, "8")
	tmpDir, err := os.MkdirTemp("/tmp", "sympozium-tool-attachment-*")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	defer os.RemoveAll(tmpDir)

	path := filepath.Join(tmpDir, "chart.png")
	if err := os.WriteFile(path, []byte("more than eight bytes"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	_, err = buildSendMessageAttachments([]string{path}, nil)
	if err == nil || !strings.Contains(err.Error(), "max is 8 bytes") {
		t.Fatalf("expected configured max-bytes error, got %v", err)
	}
}
