package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// SympoziumToolPath is where harness images install the `sympozium-tool`
// shell CLI. Its presence is how a shim knows it can advertise the tool.
const SympoziumToolPath = "/usr/local/bin/sympozium-tool"

// sympoziumToolPath is overridable in tests.
var sympoziumToolPath = SympoziumToolPath

// memorySkillPack is the name of the built-in SkillPack the ensemble
// controller attaches to every Agent; it mounts at <skillsDir>/memory/.
const memorySkillPack = "memory"

// MemoryConfigured reports whether this run has a memory-server to talk to.
// The AgentRun controller injects MEMORY_SERVER_URL only when the parent
// Agent has memory enabled, and `sympozium-tool memory-*` refuses to run
// without it, so this is the single switch the harness keys its memory
// guidance on. Advertising memory to a run that cannot use it only makes the
// model burn turns on calls that fail with "memory is disabled for this run".
func MemoryConfigured() bool {
	return strings.TrimSpace(os.Getenv("MEMORY_SERVER_URL")) != ""
}

// isMemorySkillFile reports whether path belongs to the built-in memory
// SkillPack in either mounted layout: <skillsDir>/memory/*.md or
// <skillsDir>/memory.md.
func isMemorySkillFile(skillsDir, path string) bool {
	rel, err := filepath.Rel(skillsDir, path)
	if err != nil {
		return false
	}
	first := strings.SplitN(filepath.ToSlash(rel), "/", 2)[0]
	return first == memorySkillPack || first == memorySkillPack+".md"
}

// SkillsMarkdown concatenates every mounted skill file into one Markdown
// document. Agent-runner parity: skills live at <skillsDir>/<pack>/<skill>.md
// AND <skillsDir>/<skill>.md (top-level); both layouts are inlined, each file
// once, in glob order. Returns "" when no skills are mounted.
//
// The built-in "memory" skill is left out when the run has no memory-server
// (see MemoryConfigured): the ensemble controller attaches it to every Agent
// regardless of memory being enabled, and its text tells the model to search
// and store memory on every task.
func SkillsMarkdown(skillsDir string) string {
	var b strings.Builder
	patterns := []string{
		filepath.Join(skillsDir, "*", "*.md"),
		filepath.Join(skillsDir, "*.md"),
	}
	skipMemory := !MemoryConfigured()
	seen := map[string]bool{}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
			if skipMemory && isMemorySkillFile(skillsDir, m) {
				continue
			}
			data, err := os.ReadFile(m)
			if err != nil {
				continue
			}
			b.WriteString(string(data))
			if !strings.HasSuffix(string(data), "\n") {
				b.WriteByte('\n')
			}
			b.WriteString("\n")
		}
	}
	return b.String()
}

// SystemPromptSection renders the persona system prompt (SYSTEM_PROMPT env)
// as a Markdown section, or "" when none is set.
func SystemPromptSection() string {
	sp := strings.TrimSpace(os.Getenv("SYSTEM_PROMPT"))
	if sp == "" {
		return ""
	}
	return "# Agent system prompt\n\n" + sp + "\n"
}

// ChannelContextSection gives a CLI agent the conversational frame the
// agent-runner injects into its system prompt. A one-shot CLI exec is
// otherwise stateless, with the task string as its only context, so without
// this the agent "misses the fact" a message belongs to a Slack/Telegram/
// Discord thread and treats every turn in isolation — which also lets
// cross-thread memories (the memory-server is scoped by agent/ensemble, not
// by thread) bleed in as if they were relevant. Keep it terse: a couple of
// sentences is the anchor. Returns "" for runs not triggered by a channel
// (schedule, web, TUI).
func ChannelContextSection() string {
	channel := strings.TrimSpace(os.Getenv("SOURCE_CHANNEL"))
	if channel == "" {
		return ""
	}
	chatID := strings.TrimSpace(os.Getenv("SOURCE_CHAT_ID"))
	threadID := strings.TrimSpace(os.Getenv("SOURCE_THREAD_ID"))

	reply := fmt.Sprintf("sympozium-tool send-message --channel %s --chat-id %s", channel, chatID)
	if threadID != "" {
		reply += fmt.Sprintf(" --thread-id %s", threadID)
	}

	var b strings.Builder
	b.WriteString("\n# Channel context\n\n")
	fmt.Fprintf(&b, "This task was received through the **%s** channel (chat ID `%s`). "+
		"Reply through this channel by running `%s` to deliver results, ask follow-up "+
		"questions, or send notifications to the user.", channel, chatID, reply)
	if threadID != "" {
		fmt.Fprintf(&b, " The originating message is in thread `%s` — keep replies in the "+
			"same thread (the command above already targets it) and treat this as a "+
			"continuation, not an isolated request.", threadID)
	}
	b.WriteString("\n")
	return b.String()
}

// InboundAttachmentsSection lists files from the triggering channel message
// that were already materialised into the workspace, so the agent knows where
// to find them without any tool call. Returns "" when there are none.
func InboundAttachmentsSection(paths []string) string {
	if len(paths) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Inbound attachments\n\n" +
		"The triggering message included file attachments. They have already been downloaded for you:\n\n")
	for _, p := range paths {
		fmt.Fprintf(&b, "- %s\n", p)
	}
	return b.String()
}

// SympoziumToolsSection documents the `sympozium-tool` CLI shipped in harness
// images. CLI agents' general-purpose tool is a shell, so Sympozium-specific
// capabilities are made discoverable the way those agents expect: as shell
// commands described in their context file. The wrappers write to the same
// /ipc/{tools,messages,schedules}/ paths the agent-runner does, so the IPC
// bridge handles them identically regardless of which harness produced them.
// Returns "" when the tool binary is not installed in this image.
func SympoziumToolsSection() string {
	if _, err := os.Stat(sympoziumToolPath); err != nil {
		return ""
	}
	var b strings.Builder
	b.WriteString("\n# Sympozium tools (shell)\n\n" +
		"Sympozium-specific capabilities are exposed via the `sympozium-tool` CLI on PATH. " +
		"All subcommands accept `--help`.\n\n")
	// Memory guidance only for runs that actually have a memory-server; see
	// MemoryConfigured for why advertising it otherwise is harmful.
	if MemoryConfigured() {
		b.WriteString("## memory (persistent, shared with other harnesses)\n\n" +
			"Search before investigating; store concise findings after.\n\n" +
			"```\n" +
			"sympozium-tool memory-search --query \"...\" [--top-k 5] [--scope agent|ensemble]\n" +
			"sympozium-tool memory-store  --content \"...\" [--tags a,b] [--scope agent|ensemble] [--visibility public|trusted]\n" +
			"sympozium-tool memory-list   [--scope agent|ensemble] [--limit 20]\n" +
			"```\n\n" +
			"`--scope agent` (default) is private to you; `--scope ensemble` is shared with personas in the same ensemble.\n\n")
	}
	b.WriteString("## exec (run in a SkillPack sidecar)\n\n" +
		"Your agent container is intentionally low-privilege. Use `exec` whenever a SkillPack sidecar holds the needed tooling/RBAC (e.g. kubectl, gh).\n\n" +
		"```\n" +
		"sympozium-tool exec --target <skillpack> [--workdir DIR] [--timeout SECS] -- <cmd> [args...]\n" +
		"# e.g. sympozium-tool exec --target k8s-ops -- kubectl get pods -A\n" +
		"```\n\n" +
		"Stdout/stderr stream back; this CLI exits with the command's exit code.\n\n" +
		"## send-message (reply via a channel)\n\n" +
		"Notify the user through Telegram/Slack/Discord/WhatsApp. Omit `--chat-id` for self-chat; pass `--thread-id` to stay in an existing Slack/Discord thread.\n\n" +
		"```\n" +
		"sympozium-tool send-message --channel <telegram|slack|discord|whatsapp> --text \"...\" [--chat-id ID] [--thread-id ID] [--attachment-path /tmp/chart.png]\n" +
		"```\n\n" +
		"Slack image attachments can use `--attachment-path` for a local generated PNG or `--attachment-url` for a public HTTPS image. Local files are capped by CHANNEL_ATTACHMENT_MAX_BYTES (default 768000).\n\n" +
		"## schedule (recurring agent runs)\n\n" +
		"Create/update/suspend/resume/delete a `SympoziumSchedule`, or inspect your schedules. Each fire triggers a fresh agent run with the given task. " +
		"Every command prints the state the controller actually applied (final CR name, cron, suspend flag, run counts, last run outcome) and exits non-zero on errors such as \"not found\" — check it rather than assuming success.\n\n" +
		"```\n" +
		"sympozium-tool schedule --action list                      # every schedule targeting you\n" +
		"sympozium-tool schedule --name <name> --action status       # one schedule + its last run's outcome/failure reason\n" +
		"sympozium-tool schedule --name <name> --action create  --schedule \"0 9 * * 1-5\" --task \"...\" [--model MODEL] [--provider PROVIDER] [--base-url URL]\n" +
		"sympozium-tool schedule --name <name> --action update  [--schedule \"...\"] [--task \"...\"] [--model MODEL] [--provider PROVIDER] [--base-url URL]\n" +
		"sympozium-tool schedule --name <name> --action suspend|resume|delete\n" +
		"```\n\n" +
		"Add `--json` for the raw reply.\n\n" +
		"## get-attachment (download a channel attachment)\n\n" +
		"Inbound channel attachments are normally pre-downloaded to /workspace/attachments/ (listed in the \"Inbound attachments\" section when present). To re-download one by artifact ID:\n\n" +
		"```\n" +
		"sympozium-tool get-attachment --id <artifactID> [--output /workspace/file.bin]\n" +
		"```\n")
	return b.String()
}
