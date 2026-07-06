// harness-codex is the Sympozium shim for OpenAI's `codex` CLI.
//
// It runs as the agent container entrypoint when an Agent / AgentRun
// selects `harness: codex`. The shim:
//
//  1. Materialises codex configuration from Sympozium env vars and the
//     standard /config/mcp-servers.yaml mount.
//  2. Concatenates mounted skill markdown plus the system prompt into
//     CODEX_HOME/AGENTS.md so codex picks them up as context.
//  3. Invokes `codex exec` with the agent task, captures the final
//     assistant message, and writes /ipc/output/result.json so the
//     controller can surface the result.
//
// The shim is provider-agnostic but ships sensible defaults for OpenAI.
// For custom OpenAI-compatible endpoints (Ollama, LM Studio, vLLM…)
// the controller-provided MODEL_BASE_URL triggers a [model_providers.<id>]
// override; otherwise we let codex's built-in provider table apply.
package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

type mcpToolManifest struct {
	Tools []struct {
		Name string `json:"name"`
	} `json:"tools"`
}

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "harness-codex:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Tag codex's native OTel emissions with `harness=codex` so dashboards
	// can split metrics by harness. The Rust OTel SDK that codex uses
	// honors OTEL_RESOURCE_ATTRIBUTES per spec; the controller has already
	// populated it with sympozium.instance.name, sympozium.agent_run.id,
	// k8s.namespace.name.
	appendResourceAttribute("harness", "codex")

	o := initObservability(ctx)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = o.shutdown(shutdownCtx)
	}()

	instance := os.Getenv("INSTANCE_NAME")
	model := os.Getenv("MODEL_NAME")
	namespace := os.Getenv("AGENT_NAMESPACE")
	ctx, runSpan := o.startRunSpan(ctx,
		attribute.String("harness", "codex"),
		attribute.String("instance", instance),
		attribute.String("model", model),
		attribute.String("k8s.namespace.name", namespace),
		attribute.String("sympozium.agent_run.id", os.Getenv("AGENT_RUN_ID")),
	)
	defer runSpan.End()
	writeTraceContext(ctx)

	codexHome := envOr("CODEX_HOME", "/workspace/.codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		err = fmt.Errorf("mkdir codex home %s: %w", codexHome, err)
		markSpanError(runSpan, err)
		o.recordRun(ctx, "error", instance, model, namespace, 0)
		return err
	}
	_ = os.Setenv("CODEX_HOME", codexHome)
	// Defense-in-depth: any codex sub-process or dependency that ignores
	// CODEX_HOME and falls back to ~/.codex would otherwise write to the
	// container's ephemeral layer. Symlink ~/.codex onto the PVC so those
	// writes still survive across runs on a session-scoped workspace.
	if err := linkHomeCodex(codexHome); err != nil {
		fmt.Fprintf(os.Stderr, "harness-codex: warning: failed to link ~/.codex: %v\n", err)
	}

	// Built-in codex providers (notably `openai`) set requires_openai_auth
	// internally and ignore the env-var route. They only read credentials
	// from $CODEX_HOME/auth.json — the same file `codex login --api-key`
	// writes. Materialise it from OPENAI_API_KEY so headless pods authenticate.
	if err := writeAuthJSON(codexHome); err != nil {
		fmt.Fprintf(os.Stderr, "harness-codex: warning: failed to write auth.json: %v\n", err)
	}

	if err := writeAgentsMD(codexHome); err != nil {
		err = fmt.Errorf("write AGENTS.md: %w", err)
		markSpanError(runSpan, err)
		o.recordRun(ctx, "error", instance, model, namespace, 0)
		return err
	}
	if err := writeConfigTOML(codexHome); err != nil {
		err = fmt.Errorf("write config.toml: %w", err)
		markSpanError(runSpan, err)
		o.recordRun(ctx, "error", instance, model, namespace, 0)
		return err
	}
	if bridgeURL, ok := configuredMCPBridgeURL(); ok {
		if err := waitForMCPBridge(ctx, bridgeURL, 20*time.Second); err != nil {
			err = fmt.Errorf("wait for MCP bridge %s: %w", bridgeURL, err)
			markSpanError(runSpan, err)
			o.recordRun(ctx, "error", instance, model, namespace, 0)
			return err
		}
	}

	started := time.Now()
	response, runErr := runCodex(ctx, o)
	duration := time.Since(started).Milliseconds()

	res := ipc.AgentResult{}
	res.Metrics.DurationMs = duration
	status := "success"
	if runErr != nil {
		status = "error"
		res.Status = "error"
		res.Error = runErr.Error()
		res.Response = response // partial output if any
		markSpanError(runSpan, runErr)
	} else {
		res.Status = "success"
		res.Response = response
		res.Attachments = buildResponseAttachments(ctx, response, envOr("WORKSPACE_DIR", "/workspace"))
	}
	o.recordRun(ctx, status, instance, model, namespace, duration)

	if err := writeResult(res); err != nil {
		fmt.Fprintf(os.Stderr, "harness-codex: failed to write result.json: %v\n", err)
	}
	return runErr
}

// linkHomeCodex points $HOME/.codex at the PVC-backed CODEX_HOME so any
// code path that hardcodes the default location still hits persistent
// storage. If $HOME/.codex already exists and isn't the right symlink,
// we leave it alone and let the caller log a warning.
func linkHomeCodex(codexHome string) error {
	home, err := os.UserHomeDir()
	if err != nil || home == "" || home == codexHome {
		return nil
	}
	target := filepath.Join(home, ".codex")
	if target == codexHome {
		return nil
	}
	if existing, err := os.Readlink(target); err == nil {
		if existing == codexHome {
			return nil
		}
		if err := os.Remove(target); err != nil {
			return fmt.Errorf("remove stale symlink %s: %w", target, err)
		}
	} else if _, err := os.Lstat(target); err == nil {
		// Non-symlink entry already present (e.g. a real dir baked into
		// the image). Leave it alone — operator intent is unclear.
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	return os.Symlink(codexHome, target)
}

func writeAgentsMD(codexHome string) error {
	var b strings.Builder
	skillsDir := envOr("SKILLS_DIR", "/skills")
	// Agent-runner parity: skills live at /skills/<pack>/<skill>.md AND
	// /skills/<skill>.md (top-level). Inline both — codex picks them up
	// because it auto-reads AGENTS.md as system context.
	patterns := []string{
		filepath.Join(skillsDir, "*", "*.md"),
		filepath.Join(skillsDir, "*.md"),
	}
	seen := map[string]bool{}
	for _, pat := range patterns {
		matches, _ := filepath.Glob(pat)
		for _, m := range matches {
			if seen[m] {
				continue
			}
			seen[m] = true
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
	if sp := strings.TrimSpace(os.Getenv("SYSTEM_PROMPT")); sp != "" {
		b.WriteString("# Agent system prompt\n\n")
		b.WriteString(sp)
		b.WriteString("\n")
	}
	writeChannelContextSection(&b)
	writeSympoziumToolsSection(&b)
	if b.Len() == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(codexHome, "AGENTS.md"), []byte(b.String()), 0o644)
}

// writeChannelContextSection gives codex the conversational frame the
// agent-runner injects into its system prompt. `codex exec` is otherwise a
// stateless one-shot whose only context is the task string, so without this
// codex "misses the fact" a message belongs to a Slack/Telegram/Discord thread
// and treats every turn in isolation — which also lets cross-thread memories
// (the memory-server is scoped by agent/ensemble, not by thread) bleed in as if
// they were relevant. Keep it terse: a couple of sentences is the anchor.
func writeChannelContextSection(b *strings.Builder) {
	channel := strings.TrimSpace(os.Getenv("SOURCE_CHANNEL"))
	if channel == "" {
		// Not a channel-triggered run (e.g. schedule, web, TUI). Nothing to anchor.
		return
	}
	chatID := strings.TrimSpace(os.Getenv("SOURCE_CHAT_ID"))
	threadID := strings.TrimSpace(os.Getenv("SOURCE_THREAD_ID"))

	reply := fmt.Sprintf("sympozium-tool send-message --channel %s --chat-id %s", channel, chatID)
	if threadID != "" {
		reply += fmt.Sprintf(" --thread-id %s", threadID)
	}

	b.WriteString("\n# Channel context\n\n")
	fmt.Fprintf(b, "This task was received through the **%s** channel (chat ID `%s`). "+
		"Reply through this channel by running `%s` to deliver results, ask follow-up "+
		"questions, or send notifications to the user.", channel, chatID, reply)
	if threadID != "" {
		fmt.Fprintf(b, " The originating message is in thread `%s` — keep replies in the "+
			"same thread (the command above already targets it) and treat this as a "+
			"continuation, not an isolated request.", threadID)
	}
	b.WriteString("\n")
}

// writeSympoziumToolsSection appends documentation for the `sympozium-tool`
// CLI shipped in the codex harness image. Codex's only general-purpose tool
// is `local_shell`, so we make Sympozium-specific capabilities discoverable
// the way codex expects to discover them: as shell commands described in
// AGENTS.md. The wrappers themselves write to the same /ipc/{tools,messages,
// schedules}/ paths the agent-runner does, so the IPC bridge handles them
// identically regardless of which harness produced them.
func writeSympoziumToolsSection(b *strings.Builder) {
	if _, err := os.Stat("/usr/local/bin/sympozium-tool"); err != nil {
		// Not the codex image, or wrapper missing — skip silently.
		return
	}
	b.WriteString("\n# Sympozium tools (shell)\n\n" +
		"Sympozium-specific capabilities are exposed via the `sympozium-tool` CLI on PATH. " +
		"All subcommands accept `--help`.\n\n" +
		"## memory (persistent, shared with other harnesses)\n\n" +
		"Search before investigating; store concise findings after.\n\n" +
		"```\n" +
		"sympozium-tool memory-search --query \"...\" [--top-k 5] [--scope agent|ensemble]\n" +
		"sympozium-tool memory-store  --content \"...\" [--tags a,b] [--scope agent|ensemble] [--visibility public|trusted]\n" +
		"sympozium-tool memory-list   [--scope agent|ensemble] [--limit 20]\n" +
		"```\n\n" +
		"`--scope agent` (default) is private to you; `--scope ensemble` is shared with personas in the same ensemble.\n\n" +
		"## exec (run in a SkillPack sidecar)\n\n" +
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
		"Create/update/suspend/resume/delete a `SympoziumSchedule`. Each fire triggers a fresh agent run with the given task.\n\n" +
		"```\n" +
		"sympozium-tool schedule --name <name> --action create  --schedule \"0 9 * * 1-5\" --task \"...\"\n" +
		"sympozium-tool schedule --name <name> --action update  [--schedule \"...\"] [--task \"...\"]\n" +
		"sympozium-tool schedule --name <name> --action suspend|resume|delete\n" +
		"```\n")
}

func writeConfigTOML(codexHome string) error {
	var sb strings.Builder

	provider := strings.ToLower(strings.TrimSpace(envOr("MODEL_PROVIDER", "openai")))
	model := envOr("MODEL_NAME", "gpt-5")
	baseURL := strings.TrimSpace(os.Getenv("MODEL_BASE_URL"))

	providerID := tomlIdent(provider)
	fmt.Fprintf(&sb, "model = %q\n", model)
	fmt.Fprintf(&sb, "model_provider = %q\n", providerID)

	// Top-level keys MUST appear before any [section] header — TOML
	// would otherwise scope them into the preceding table.

	// Sandbox + approval. The agent container is already locked down at
	// the K8s pod level (readOnlyRootFilesystem, all caps dropped, non-
	// root UID, NetworkPolicy) — that's the real security boundary. The
	// codex-level sandbox on top of it would only block legitimate writes
	// to /ipc/ (where sympozium-tool drops its IPC files) and HTTP calls
	// to the central memory-server, breaking every Sympozium tool wrapper
	// without adding meaningful isolation.
	sb.WriteString("sandbox_mode = \"danger-full-access\"\n")
	sb.WriteString("approval_policy = \"never\"\n")

	// Map Sympozium THINKING_MODE (off/low/medium/high/minimal) onto
	// codex's model_reasoning_effort. Codex only honors this on Responses-
	// API models (o-series, gpt-5 family); other models ignore it.
	if effort := codexReasoningEffort(os.Getenv("THINKING_MODE")); effort != "" {
		fmt.Fprintf(&sb, "model_reasoning_effort = %q\n", effort)
	}
	sb.WriteString("\n")

	// Only override the provider table when we have something custom to say.
	// Otherwise we let codex's built-in provider defaults apply.
	if baseURL != "" || !isBuiltInProvider(providerID) {
		fmt.Fprintf(&sb, "[model_providers.%s]\n", providerID)
		fmt.Fprintf(&sb, "name = %q\n", titleCase(provider))
		if baseURL != "" {
			fmt.Fprintf(&sb, "base_url = %q\n", baseURL)
		}
		fmt.Fprintf(&sb, "env_key = %q\n", providerEnvKey(provider))
		// Chat-completions is the safe wire format for OpenAI-compatible servers.
		fmt.Fprintf(&sb, "wire_api = \"chat\"\n\n")
	}

	writeMCPBridgeBlock(&sb)

	// Disable codex's anonymous analytics — Sympozium-managed pods should
	// only emit telemetry to the operator's own collector.
	sb.WriteString("[analytics]\nenabled = false\n\n")

	// Native codex OTel export. Codex emits a rich set of metrics out of
	// the box (turn.token_usage by token_type, codex.tool.call, per-API
	// timing, etc.) — see https://developers.openai.com/codex/config-advanced#observability-and-telemetry
	// We point codex at the same OTLP endpoint the controller injects for
	// the agent-runner so dashboards stay unified across harnesses.
	writeCodexOTelBlock(&sb)

	rendered := sb.String()
	// Mirror the final TOML to stderr so we can see exactly what codex parses
	// without execing into the pod. The TOML never contains the API key.
	fmt.Fprintln(os.Stderr, "harness-codex: rendered config.toml:\n"+rendered)
	return os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(rendered), 0o644)
}

func writeMCPBridgeBlock(sb *strings.Builder) {
	bridgeURL, ok := configuredMCPBridgeURL()
	if !ok {
		return
	}

	// Codex talks MCP to one local bridge endpoint. The mcp-bridge sidecar owns
	// remote MCP URLs, headers, auth secrets, tool filtering, and dispatch.
	fmt.Fprintf(sb, "[mcp_servers.sympozium_bridge]\n")
	fmt.Fprintf(sb, "url = %q\n\n", bridgeURL)
}

func configuredMCPBridgeURL() (string, bool) {
	manifestPath := envOr("MCP_MANIFEST_PATH", "/ipc/tools/mcp-tools.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil || len(data) == 0 {
		return "", false
	}
	var manifest mcpToolManifest
	if err := json.Unmarshal(data, &manifest); err != nil || len(manifest.Tools) == 0 {
		if err != nil {
			fmt.Fprintf(os.Stderr, "harness-codex: ignoring malformed MCP manifest %s: %v\n", manifestPath, err)
		}
		return "", false
	}
	return envOr("MCP_BRIDGE_URL", "http://127.0.0.1:8765/mcp"), true
}

func waitForMCPBridge(ctx context.Context, bridgeURL string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	client := &http.Client{Timeout: 2 * time.Second}
	payload := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","capabilities":{},"clientInfo":{"name":"harness-codex","version":"1"}}}`
	var lastErr error

	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, bridgeURL, strings.NewReader(payload))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
		} else {
			lastErr = err
		}

		if time.Now().After(deadline) {
			return lastErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

// writeAuthJSON materialises $CODEX_HOME/auth.json from OPENAI_API_KEY so
// codex's built-in `openai` provider (which has requires_openai_auth=true
// and ignores env_key) can authenticate. Schema matches codex-rs/login/src/
// auth/storage.rs AuthDotJson and what `codex login --api-key` writes.
func writeAuthJSON(codexHome string) error {
	key := strings.TrimSpace(os.Getenv("OPENAI_API_KEY"))
	if key == "" {
		return nil
	}
	path := filepath.Join(codexHome, "auth.json")
	payload := map[string]string{
		"auth_mode":      "apikey",
		"OPENAI_API_KEY": key,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// writeCodexOTelBlock emits `[otel]` configuration for codex when the
// controller-injected SYMPOZIUM_OTEL_* env vars are present. When disabled
// or no endpoint is configured we still write [otel].exporter = "none" so
// codex doesn't fall back to its default statsig metrics exporter.
func writeCodexOTelBlock(sb *strings.Builder) {
	enabled := strings.EqualFold(os.Getenv("SYMPOZIUM_OTEL_ENABLED"), "true")
	endpoint := firstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_OTLP_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if !enabled || endpoint == "" {
		sb.WriteString("[otel]\nexporter = \"none\"\nmetrics_exporter = \"none\"\ntrace_exporter = \"none\"\n")
		return
	}

	protocol := strings.ToLower(firstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_OTLP_PROTOCOL"),
		os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
	))
	useGRPC := protocol == "grpc" || (protocol == "" && !strings.HasPrefix(endpoint, "http"))

	environment := firstNonEmpty(os.Getenv("AGENT_NAMESPACE"), "default")

	fmt.Fprintf(sb, "[otel]\nenvironment = %q\nlog_user_prompt = false\n\n", environment)

	if useGRPC {
		host := strings.TrimPrefix(strings.TrimPrefix(endpoint, "grpc://"), "https://")
		fmt.Fprintf(sb, "[otel.exporter.otlp-grpc]\nendpoint = %q\n\n", host)
		fmt.Fprintf(sb, "[otel.metrics_exporter.otlp-grpc]\nendpoint = %q\n\n", host)
		fmt.Fprintf(sb, "[otel.trace_exporter.otlp-grpc]\nendpoint = %q\n\n", host)
		return
	}
	base := strings.TrimRight(endpoint, "/")
	fmt.Fprintf(sb, "[otel.exporter.otlp-http]\nendpoint = %q\nprotocol = \"binary\"\n\n", base+"/v1/logs")
	fmt.Fprintf(sb, "[otel.metrics_exporter.otlp-http]\nendpoint = %q\nprotocol = \"binary\"\n\n", base+"/v1/metrics")
	fmt.Fprintf(sb, "[otel.trace_exporter.otlp-http]\nendpoint = %q\nprotocol = \"binary\"\n\n", base+"/v1/traces")
}

func runCodex(ctx context.Context, o *harnessObservability) (string, error) {
	task := os.Getenv("TASK")
	if task == "" {
		return "", fmt.Errorf("TASK env var is empty")
	}
	workspace := envOr("WORKSPACE_DIR", "/workspace")
	lastMessagePath := filepath.Join(os.TempDir(), "codex-last.txt")
	_ = os.Remove(lastMessagePath)

	ctx, span := o.startCodexSpan(ctx,
		attribute.String("codex.workspace", workspace),
		attribute.String("codex.model", os.Getenv("MODEL_NAME")),
	)
	defer span.End()

	args := []string{
		"exec",
		"--skip-git-repo-check",
		"--cd", workspace,
		"--output-last-message", lastMessagePath,
		task,
	}
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	runErr := cmd.Run()
	if runErr != nil {
		markSpanError(span, runErr)
	}

	// Prefer the dedicated last-message file; fall back to empty string.
	if data, err := os.ReadFile(lastMessagePath); err == nil {
		return strings.TrimSpace(string(data)), runErr
	}
	if runErr != nil {
		return "", fmt.Errorf("codex exec failed: %w", runErr)
	}
	return "", nil
}

// buildResponseAttachments scans the agent's final answer for files it produced
// (e.g. a generated chart or CSV) and returns them as attachments so the
// auto-relayed reply can deliver the files, not just a dead local path.
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
func buildResponseAttachments(ctx context.Context, response, workspace string) []ipc.Attachment {
	if strings.TrimSpace(response) == "" {
		return nil
	}
	workspace = filepath.Clean(strings.TrimSpace(workspace))
	if workspace == "" || workspace == "." {
		workspace = "/workspace"
	}

	const maxAttachments = 10

	uploader := newArtifactUploader()
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
			id, size, err := uploader.upload(ctx, filename, mimeType, data)
			if err != nil {
				// Never fail the reply over a failed upload; drop the file and
				// keep the text.
				fmt.Fprintf(os.Stderr, "harness-codex: artifact upload failed for %s: %v\n", filename, err)
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

// artifactUploader posts agent-produced files to the artifact-server so their
// bytes never ride the event bus. It is nil when ARTIFACT_SERVER_URL is unset,
// in which case the caller falls back to inline base64 embedding.
type artifactUploader struct {
	baseURL string
	token   string
	client  *http.Client
}

// newArtifactUploader returns a configured uploader, or nil if the
// artifact-server is not configured or the pod has no ServiceAccount token.
func newArtifactUploader() *artifactUploader {
	base := strings.TrimRight(strings.TrimSpace(os.Getenv("ARTIFACT_SERVER_URL")), "/")
	if base == "" {
		return nil
	}
	token, err := readServiceAccountToken()
	if err != nil || token == "" {
		fmt.Fprintf(os.Stderr, "harness-codex: artifact upload disabled (no SA token): %v\n", err)
		return nil
	}
	return &artifactUploader{
		baseURL: base,
		token:   token,
		client:  &http.Client{Timeout: 60 * time.Second},
	}
}

// upload posts a single file and returns the artifact id and stored size.
func (u *artifactUploader) upload(ctx context.Context, filename, mimeType string, data []byte) (string, int64, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.baseURL+"/v1/artifacts", bytes.NewReader(data))
	if err != nil {
		return "", 0, err
	}
	req.Header.Set("Authorization", "Bearer "+u.token)
	req.Header.Set("Content-Type", mimeType)
	req.Header.Set("X-Artifact-Filename", filename)

	resp, err := u.client.Do(req)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode != http.StatusCreated {
		return "", 0, fmt.Errorf("artifact-server HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var ur struct {
		ID   string `json:"id"`
		Size int64  `json:"size"`
	}
	if err := json.Unmarshal(body, &ur); err != nil {
		return "", 0, err
	}
	if ur.ID == "" {
		return "", 0, fmt.Errorf("artifact-server returned empty id")
	}
	return ur.ID, ur.Size, nil
}

// readServiceAccountToken reads the projected pod SA token used to authenticate
// to the artifact-server. SA_TOKEN_PATH overrides the location (tests).
func readServiceAccountToken() (string, error) {
	path := envOr("SA_TOKEN_PATH", "/var/run/secrets/kubernetes.io/serviceaccount/token")
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(b)), nil
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

func writeResult(res ipc.AgentResult) error {
	out := envOr("RESULT_PATH", "/ipc/output/result.json")
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

// providerEnvKey maps a Sympozium provider id to the env var that holds the
// API key. Mirrors allowedAuthSecretKeys in internal/controller.
func providerEnvKey(provider string) string {
	switch strings.ToLower(provider) {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "azure", "azure-openai":
		return "AZURE_OPENAI_API_KEY"
	case "google", "gemini":
		return "GOOGLE_API_KEY"
	case "mistral":
		return "MISTRAL_API_KEY"
	case "groq":
		return "GROQ_API_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}

// isBuiltInProvider returns true for provider ids codex ships built-in.
// For these we avoid overriding [model_providers.<id>] unless a base_url
// is explicitly configured.
func isBuiltInProvider(id string) bool {
	switch id {
	case "openai", "ollama", "gemini":
		return true
	}
	return false
}

// codexReasoningEffort maps the Sympozium THINKING_MODE enum to the values
// codex accepts for model_reasoning_effort. "off" disables reasoning and
// returns "" so the key is omitted entirely.
func codexReasoningEffort(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "off":
		return ""
	case "minimal", "low", "medium", "high", "xhigh":
		return strings.ToLower(strings.TrimSpace(mode))
	default:
		return ""
	}
}

func tomlIdent(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	out := b.String()
	if out == "" {
		out = "default"
	}
	return out
}

func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// appendResourceAttribute appends a key=value entry to OTEL_RESOURCE_ATTRIBUTES
// (the standard OTel SDK env var) unless a value for that key is already
// present. Codex's Rust OTel SDK reads this env var natively, so attributes
// added here attach to every metric / log / span codex emits.
func appendResourceAttribute(key, value string) {
	if key == "" || value == "" {
		return
	}
	prefix := key + "="
	current := os.Getenv("OTEL_RESOURCE_ATTRIBUTES")
	for _, pair := range strings.Split(current, ",") {
		if strings.HasPrefix(strings.TrimSpace(pair), prefix) {
			return
		}
	}
	entry := prefix + value
	if current == "" {
		_ = os.Setenv("OTEL_RESOURCE_ATTRIBUTES", entry)
		return
	}
	_ = os.Setenv("OTEL_RESOURCE_ATTRIBUTES", current+","+entry)
}
