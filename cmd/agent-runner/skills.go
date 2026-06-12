package main

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const defaultSkillsDir = "/skills"

// skillIndexEntry is the lightweight summary of a single skill file that
// the agent uses to populate the system prompt and the `skills` tool catalog.
// Bodies stay on disk until the agent invokes the `skills` tool.
type skillIndexEntry struct {
	Name        string // "<pack>/<skill>" or "<skill>" for top-level
	Path        string
	Description string
}

// loadSkillIndex scans /skills, which is laid out as one subdirectory per
// SkillPack mount (/skills/<pack>/<skill>.md). Top-level .md files are also
// indexed as a defensive fallback for legacy layouts and tests.
func loadSkillIndex(skillsDir string) []skillIndexEntry {
	entries, err := os.ReadDir(skillsDir)
	if err != nil {
		slog.Info("skills.dir.not_found", "dir", skillsDir, "error", err)
		return nil
	}

	var out []skillIndexEntry
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			// Kubernetes projected volumes create ..data, ..timestamp, etc.
			continue
		}
		full := filepath.Join(skillsDir, name)
		if entry.IsDir() {
			out = append(out, collectPackEntries(full, name)...)
			continue
		}
		if filepath.Ext(name) != ".md" {
			continue
		}
		out = append(out, makeSkillEntry(full, strings.TrimSuffix(name, ".md")))
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	if len(out) > 0 {
		slog.Info("skills.indexed", "count", len(out), "dir", skillsDir)
	}
	return out
}

func collectPackEntries(packDir, packName string) []skillIndexEntry {
	entries, err := os.ReadDir(packDir)
	if err != nil {
		slog.Warn("skills.pack.read_failed", "dir", packDir, "error", err)
		return nil
	}
	var out []skillIndexEntry
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || filepath.Ext(name) != ".md" {
			continue
		}
		skill := strings.TrimSuffix(name, ".md")
		out = append(out, makeSkillEntry(filepath.Join(packDir, name), packName+"/"+skill))
	}
	return out
}

func makeSkillEntry(path, name string) skillIndexEntry {
	data, err := os.ReadFile(path)
	if err != nil {
		slog.Warn("skills.file.read_failed", "path", path, "error", err)
		return skillIndexEntry{Name: name, Path: path}
	}
	fm, body := splitFrontmatter(string(data))
	desc := fm["description"]
	if desc == "" {
		desc = firstSummaryLine(body)
	}
	return skillIndexEntry{Name: name, Path: path, Description: desc}
}

// splitFrontmatter peels off a leading `---\n...\n---\n` block of `key: value`
// lines. The SkillPack reconciler is the only producer in production, so the
// format is intentionally narrow: no comments, no nested structures, optional
// matching single/double quotes around values.
func splitFrontmatter(s string) (map[string]string, string) {
	if !strings.HasPrefix(s, "---\n") {
		return nil, s
	}
	end := strings.Index(s[4:], "\n---\n")
	if end < 0 {
		return nil, s
	}
	block := s[4 : 4+end]
	body := s[4+end+len("\n---\n"):]

	fm := map[string]string{}
	for _, line := range strings.Split(block, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		fm[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"'`)
	}
	return fm, body
}

// firstSummaryLine is a fallback for hand-authored ConfigMaps that don't
// supply frontmatter: take the legacy `> blockquote` if present, otherwise
// the first non-heading, non-blank line.
func firstSummaryLine(body string) string {
	for _, raw := range strings.Split(body, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "> ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "> "))
		}
		return line
	}
	return ""
}

// buildSystemPrompt assembles the full system prompt. The skill catalog
// itself is delivered through the `skills` tool description, not inlined
// here — this section just points the model at the tool.
func buildSystemPrompt(base string, skills []skillIndexEntry, toolsEnabled bool) string {
	var sb strings.Builder
	sb.WriteString(base)

	if len(skills) > 0 {
		fmt.Fprintf(&sb,
			"\n\n## Your Skills\n\nYou have %d skill(s) available. Each skill is a self-contained "+
				"procedure (commands to run, files to read, validation steps) authored by the operator. "+
				"The full catalog and one-line summaries are exposed through the `skills` tool. "+
				"When a task matches a skill's domain, call `skills` with the skill name to load the "+
				"full instructions BEFORE attempting the work — do not improvise from the summary.",
			len(skills),
		)
	}

	if toolsEnabled {
		sb.WriteString("\n\n## Tool Usage\n\n")
		sb.WriteString("You have access to tools that let you execute commands, inspect files, fetch web content, and send messages through channels. ")
		sb.WriteString("When the task requires interacting with Kubernetes or running shell commands, ")
		sb.WriteString("use the `execute_command` tool to run them. The commands run inside a sidecar container ")
		sb.WriteString("that has kubectl and other CLI tools available.\n\n")
		sb.WriteString("**Important: You are running inside a Kubernetes pod with full cluster admin access. ")
		sb.WriteString("kubectl is pre-configured via a mounted ServiceAccount token and works out of the box. ")
		sb.WriteString("You have RBAC permissions to read all resources cluster-wide and manage workloads in any namespace. ")
		sb.WriteString("Do NOT check kubeconfig, contexts, or try to configure cluster access — just run kubectl commands directly. ")
		sb.WriteString("Commands like `kubectl get pods -A` and `kubectl get nodes` work. ")
		sb.WriteString("`kubectl config current-context` will always error in-cluster; this is normal and expected.**\n\n")
		sb.WriteString("Always use tools to gather real information rather than guessing. ")
		sb.WriteString("For example, if asked about pod status, run `kubectl get pods` rather than speculating.\n\n")
		sb.WriteString("After executing commands, summarise the results clearly for the user.\n\n")
		sb.WriteString("### Fetching Web Content\n\n")
		sb.WriteString("You have a `fetch_url` tool that lets you download and read web pages, API responses, ")
		sb.WriteString("and online documentation. HTML pages are automatically converted to readable plain text. ")
		sb.WriteString("Use this to research information, read docs, check endpoints, or download data from the internet.\n\n")
		sb.WriteString("### Writing Files\n\n")
		sb.WriteString("You have a `write_file` tool that lets you create or overwrite files under /workspace or /tmp. ")
		sb.WriteString("Use this to save reports, create scripts, write configuration files, or produce any output artifacts.\n\n")
		sb.WriteString("### Sending Messages Through Channels\n\n")
		sb.WriteString("You have a `send_channel_message` tool that lets you send messages through connected channels ")
		sb.WriteString("(WhatsApp, Telegram, Discord, Slack). Use it whenever the user asks you to notify someone, ")
		sb.WriteString("send a summary, or deliver any message. You can send to specific chat IDs, phone numbers, ")
		sb.WriteString("or leave the chatId empty to send to the device owner.\n")
		sb.WriteString("For WhatsApp, use the phone number in international format without + (e.g. '447450248165' for +44 7450 248165).\n")
		sb.WriteString("For Telegram, use the numeric chat ID.\n")
		sb.WriteString("For Discord/Slack, use the channel ID.")
	}

	return sb.String()
}
