package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSkillsMarkdown_InlinesBothLayoutsOnce(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "k8s-ops"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "k8s-ops", "pods.md"), []byte("# Pods"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "top.md"), []byte("# Top\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Non-markdown files are ignored.
	if err := os.WriteFile(filepath.Join(dir, "k8s-ops", "notes.txt"), []byte("ignored"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := SkillsMarkdown(dir)
	if strings.Count(got, "# Pods") != 1 || strings.Count(got, "# Top") != 1 {
		t.Fatalf("expected each skill exactly once, got:\n%s", got)
	}
	if strings.Contains(got, "ignored") {
		t.Fatalf("non-markdown file leaked into skills:\n%s", got)
	}
	// A file without a trailing newline gets one, and files are separated by a blank line.
	if !strings.Contains(got, "# Pods\n\n") {
		t.Fatalf("expected newline normalisation, got %q", got)
	}
	if SkillsMarkdown(filepath.Join(dir, "missing")) != "" {
		t.Fatal("expected empty output for a missing skills dir")
	}
}

func TestSystemPromptSection(t *testing.T) {
	t.Setenv("SYSTEM_PROMPT", "")
	if SystemPromptSection() != "" {
		t.Fatal("expected empty section without SYSTEM_PROMPT")
	}
	t.Setenv("SYSTEM_PROMPT", "  You are the SRE persona.  ")
	got := SystemPromptSection()
	if got != "# Agent system prompt\n\nYou are the SRE persona.\n" {
		t.Fatalf("unexpected section: %q", got)
	}
}

func TestChannelContextSection(t *testing.T) {
	t.Setenv("SOURCE_CHANNEL", "")
	if ChannelContextSection() != "" {
		t.Fatal("expected no channel context for non-channel runs")
	}

	t.Setenv("SOURCE_CHANNEL", "slack")
	t.Setenv("SOURCE_CHAT_ID", "C123")
	t.Setenv("SOURCE_THREAD_ID", "")
	got := ChannelContextSection()
	if !strings.Contains(got, "**slack** channel (chat ID `C123`)") {
		t.Fatalf("missing channel/chat anchor:\n%s", got)
	}
	if !strings.Contains(got, "sympozium-tool send-message --channel slack --chat-id C123`") {
		t.Fatalf("missing reply command:\n%s", got)
	}
	if strings.Contains(got, "--thread-id") || strings.Contains(got, "thread") {
		t.Fatalf("thread guidance should be absent without SOURCE_THREAD_ID:\n%s", got)
	}

	t.Setenv("SOURCE_THREAD_ID", "1700000000.000100")
	got = ChannelContextSection()
	if !strings.Contains(got, "--thread-id 1700000000.000100") {
		t.Fatalf("reply command should target the thread:\n%s", got)
	}
	if !strings.Contains(got, "continuation, not an isolated request") {
		t.Fatalf("missing continuation guidance:\n%s", got)
	}
}

func TestInboundAttachmentsSection(t *testing.T) {
	if InboundAttachmentsSection(nil) != "" {
		t.Fatal("expected empty section for no attachments")
	}
	got := InboundAttachmentsSection([]string{"/workspace/attachments/a.png", "/workspace/attachments/b.csv"})
	if !strings.Contains(got, "- /workspace/attachments/a.png\n- /workspace/attachments/b.csv\n") {
		t.Fatalf("unexpected listing:\n%s", got)
	}
}

func TestSympoziumToolsSection_OnlyWhenInstalled(t *testing.T) {
	orig := sympoziumToolPath
	t.Cleanup(func() { sympoziumToolPath = orig })

	sympoziumToolPath = filepath.Join(t.TempDir(), "does-not-exist")
	if SympoziumToolsSection() != "" {
		t.Fatal("expected no tools section when the binary is absent")
	}

	tool := filepath.Join(t.TempDir(), "sympozium-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sympoziumToolPath = tool
	t.Setenv("MEMORY_SERVER_URL", "http://sympozium-memory-server.sympozium-system.svc:8080")
	got := SympoziumToolsSection()
	for _, want := range []string{"memory-search", "sympozium-tool exec --target", "send-message", "schedule --name", "get-attachment"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools section missing %q", want)
		}
	}
}

func TestSympoziumToolsSection_MemoryOnlyWhenServerConfigured(t *testing.T) {
	orig := sympoziumToolPath
	t.Cleanup(func() { sympoziumToolPath = orig })
	tool := filepath.Join(t.TempDir(), "sympozium-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	sympoziumToolPath = tool

	// Memory disabled on the Agent: the controller injects no MEMORY_SERVER_URL.
	t.Setenv("MEMORY_SERVER_URL", "")
	got := SympoziumToolsSection()
	for _, absent := range []string{"## memory", "memory-search", "memory-store", "memory-list"} {
		if strings.Contains(got, absent) {
			t.Errorf("memory guidance %q must not be advertised without a memory server:\n%s", absent, got)
		}
	}
	// Everything else is still documented.
	for _, want := range []string{"sympozium-tool exec --target", "send-message", "schedule --name", "get-attachment"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools section missing %q", want)
		}
	}

	t.Setenv("MEMORY_SERVER_URL", "http://sympozium-memory-server.sympozium-system.svc:8080")
	got = SympoziumToolsSection()
	for _, want := range []string{"## memory", "memory-search", "memory-store", "memory-list"} {
		if !strings.Contains(got, want) {
			t.Errorf("tools section missing %q with a memory server configured", want)
		}
	}
}

func TestSkillsMarkdown_SkipsMemorySkillWithoutServer(t *testing.T) {
	dir := t.TempDir()
	for _, f := range []struct{ path, body string }{
		{filepath.Join("memory", "memory.md"), "# Persistent Memory\n"},
		{"memory.md", "# Persistent Memory (top-level)\n"},
		{filepath.Join("k8s-ops", "pods.md"), "# Pods\n"},
	} {
		p := filepath.Join(dir, f.path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Setenv("MEMORY_SERVER_URL", "")
	got := SkillsMarkdown(dir)
	if strings.Contains(got, "Persistent Memory") {
		t.Fatalf("memory skill must be skipped without a memory server:\n%s", got)
	}
	if !strings.Contains(got, "# Pods") {
		t.Fatalf("other skills must still be inlined:\n%s", got)
	}

	t.Setenv("MEMORY_SERVER_URL", "http://sympozium-memory-server.sympozium-system.svc:8080")
	got = SkillsMarkdown(dir)
	if strings.Count(got, "Persistent Memory") != 2 || !strings.Contains(got, "# Pods") {
		t.Fatalf("expected memory skill (both layouts) and other skills with a memory server:\n%s", got)
	}
}

func TestAppendResourceAttribute(t *testing.T) {
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "")
	AppendResourceAttribute("harness", "claude-code")
	if got := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); got != "harness=claude-code" {
		t.Fatalf("got %q", got)
	}
	// Existing attributes are preserved and the key is not duplicated.
	t.Setenv("OTEL_RESOURCE_ATTRIBUTES", "k8s.namespace.name=default,harness=codex")
	AppendResourceAttribute("harness", "claude-code")
	if got := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); got != "k8s.namespace.name=default,harness=codex" {
		t.Fatalf("existing key should win, got %q", got)
	}
	AppendResourceAttribute("sympozium.agent_run.id", "run-1")
	if got := os.Getenv("OTEL_RESOURCE_ATTRIBUTES"); got != "k8s.namespace.name=default,harness=codex,sympozium.agent_run.id=run-1" {
		t.Fatalf("got %q", got)
	}
}
