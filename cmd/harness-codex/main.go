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
//
// Everything that is not codex-specific (skills/channel context assembly,
// sympozium-tool docs, MCP bridge discovery, attachment scanning, IPC result,
// OTel) lives in internal/harness and is shared with the other CLI shims.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sympozium-ai/sympozium/internal/artifact"
	"github.com/sympozium-ai/sympozium/internal/harness"
	"github.com/sympozium-ai/sympozium/internal/ipc"
)

const harnessName = "codex"

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
	harness.AppendResourceAttribute("harness", harnessName)

	o := harness.InitObservability(ctx, harnessName)
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = o.Shutdown(shutdownCtx)
	}()

	instance := os.Getenv("INSTANCE_NAME")
	model := os.Getenv("MODEL_NAME")
	namespace := os.Getenv("AGENT_NAMESPACE")
	ctx, runSpan := o.StartRunSpan(ctx,
		attribute.String("harness", harnessName),
		attribute.String("instance", instance),
		attribute.String("model", model),
		attribute.String("k8s.namespace.name", namespace),
		attribute.String("sympozium.agent_run.id", os.Getenv("AGENT_RUN_ID")),
	)
	defer runSpan.End()
	harness.WriteTraceContext(ctx, harnessName)

	fail := func(err error) error {
		harness.MarkSpanError(runSpan, err)
		o.RecordRun(ctx, "error", instance, model, namespace, 0)
		return err
	}

	codexHome := harness.EnvOr("CODEX_HOME", "/workspace/.codex")
	if err := os.MkdirAll(codexHome, 0o755); err != nil {
		return fail(fmt.Errorf("mkdir codex home %s: %w", codexHome, err))
	}
	_ = os.Setenv("CODEX_HOME", codexHome)
	// Defense-in-depth: any codex sub-process or dependency that ignores
	// CODEX_HOME and falls back to ~/.codex would otherwise write to the
	// container's ephemeral layer. Symlink ~/.codex onto the PVC so those
	// writes still survive across runs on a session-scoped workspace.
	if err := linkHomeCodex(codexHome); err != nil {
		harness.Logf(harnessName, "warning: failed to link ~/.codex: %v", err)
	}

	// Built-in codex providers (notably `openai`) set requires_openai_auth
	// internally and ignore the env-var route. They only read credentials
	// from $CODEX_HOME/auth.json — the same file `codex login --api-key`
	// writes. Materialise it from OPENAI_API_KEY so headless pods authenticate.
	if err := writeAuthJSON(codexHome); err != nil {
		harness.Logf(harnessName, "warning: failed to write auth.json: %v", err)
	}

	workspace := harness.EnvOr("WORKSPACE_DIR", "/workspace")
	if err := writeAgentsMD(codexHome, artifact.MaterializeInbound(ctx, workspace)); err != nil {
		return fail(fmt.Errorf("write AGENTS.md: %w", err))
	}
	if err := writeConfigTOML(codexHome); err != nil {
		return fail(fmt.Errorf("write config.toml: %w", err))
	}
	if bridgeURL, ok := harness.ConfiguredMCPBridgeURL(harnessName); ok {
		if err := harness.WaitForMCPBridge(ctx, bridgeURL, 20*time.Second, "harness-codex"); err != nil {
			return fail(fmt.Errorf("wait for MCP bridge %s: %w", bridgeURL, err))
		}
	}

	started := time.Now()
	response, codexRun, runErr := runCodex(ctx, o)
	duration := time.Since(started).Milliseconds()

	res := ipc.AgentResult{}
	res.Metrics.DurationMs = duration
	res.Metrics.InputTokens = int(codexRun.Usage.PromptTotal())
	res.Metrics.OutputTokens = int(codexRun.Usage.Output)
	res.Metrics.ToolCalls = codexRun.ToolCalls()
	status := "success"
	if runErr != nil {
		status = "error"
		res.Status = "error"
		res.Error = runErr.Error()
		res.Response = response // partial output if any
		harness.MarkSpanError(runSpan, runErr)
	} else {
		res.Status = "success"
		res.Response = response
		res.Attachments = harness.BuildResponseAttachments(ctx, harnessName, response, workspace)
	}
	harness.Logf(harnessName, "run finished: status=%s turns=%d tools=%d in=%d out=%d",
		status, codexRun.Turns, res.Metrics.ToolCalls, res.Metrics.InputTokens, res.Metrics.OutputTokens)
	o.RecordRun(ctx, status, instance, model, namespace, duration)
	// Canonical token / tool metrics from codex's own event stream, so they
	// exist regardless of codex's native metrics exporter.
	o.RecordTokenUsage(ctx, model, codexRun.Usage)
	for _, ti := range codexRun.SortedToolInvocations() {
		o.RecordToolInvocation(ctx, ti.Key.Name, ti.Key.Status, int64(ti.Count))
	}

	if err := harness.WriteResult(res); err != nil {
		harness.Logf(harnessName, "failed to write result.json: %v", err)
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

// writeAgentsMD renders CODEX_HOME/AGENTS.md — the file codex auto-reads as
// system context — from the mounted skills, the persona system prompt, the
// channel frame, pre-downloaded attachments, and the sympozium-tool docs.
func writeAgentsMD(codexHome string, inboundAttachmentPaths []string) error {
	var b strings.Builder
	b.WriteString(harness.SkillsMarkdown(harness.EnvOr("SKILLS_DIR", "/skills")))
	b.WriteString(harness.SystemPromptSection())
	b.WriteString(harness.ChannelContextSection())
	b.WriteString(harness.InboundAttachmentsSection(inboundAttachmentPaths))
	b.WriteString(harness.SympoziumToolsSection())
	if b.Len() == 0 {
		return nil
	}
	return os.WriteFile(filepath.Join(codexHome, "AGENTS.md"), []byte(b.String()), 0o644)
}

func writeConfigTOML(codexHome string) error {
	var sb strings.Builder

	provider := strings.ToLower(strings.TrimSpace(harness.EnvOr("MODEL_PROVIDER", "openai")))
	model := harness.EnvOr("MODEL_NAME", "gpt-5")
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
	// the box (codex.turn.token_usage histogram by token_type,
	// codex.tool.call, per-API timing, etc.) under its own names; the
	// canonical gen_ai.client.token.usage / sympozium.tool.invocations
	// series are emitted by this shim from the --json event stream. Note
	// codex's metrics_exporter defaults to "statsig" (OpenAI's sink) — it
	// must be pointed at our collector explicitly or set to "none".
	writeCodexOTelBlock(&sb)

	rendered := sb.String()
	// Mirror the final TOML to stderr so we can see exactly what codex parses
	// without execing into the pod. The TOML never contains the API key.
	fmt.Fprintln(os.Stderr, "harness-codex: rendered config.toml:\n"+rendered)
	return os.WriteFile(filepath.Join(codexHome, "config.toml"), []byte(rendered), 0o644)
}

func writeMCPBridgeBlock(sb *strings.Builder) {
	bridgeURL, ok := harness.ConfiguredMCPBridgeURL(harnessName)
	if !ok {
		return
	}

	// Codex talks MCP to one local bridge endpoint. The mcp-bridge sidecar owns
	// remote MCP URLs, headers, auth secrets, tool filtering, and dispatch.
	fmt.Fprintf(sb, "[mcp_servers.sympozium_bridge]\n")
	fmt.Fprintf(sb, "url = %q\n\n", bridgeURL)
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
	endpoint := harness.FirstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_OTLP_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if !enabled || endpoint == "" {
		sb.WriteString("[otel]\nexporter = \"none\"\nmetrics_exporter = \"none\"\ntrace_exporter = \"none\"\n")
		return
	}

	protocol := strings.ToLower(harness.FirstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_OTLP_PROTOCOL"),
		os.Getenv("OTEL_EXPORTER_OTLP_PROTOCOL"),
	))
	useGRPC := protocol == "grpc" || (protocol == "" && !strings.HasPrefix(endpoint, "http"))

	environment := harness.FirstNonEmpty(os.Getenv("AGENT_NAMESPACE"), "default")

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

// runCodex executes `codex exec --json`, tees the JSONL event stream to the pod
// log while folding it into a codexRun (token usage, tool invocations, last
// agent message, failure), and returns the final answer.
//
// The answer is read from --output-last-message first; when codex leaves that
// empty — typically because the model ended its turn on a tool call with no
// closing message — the last agent_message item from the stream is used, so a
// successful run never reports an empty result while the stream held one.
func runCodex(ctx context.Context, o *harness.Observability) (string, *codexRun, error) {
	run := &codexRun{}
	task := os.Getenv("TASK")
	if task == "" {
		return "", run, fmt.Errorf("TASK env var is empty")
	}
	workspace := harness.EnvOr("WORKSPACE_DIR", "/workspace")
	lastMessagePath := filepath.Join(os.TempDir(), "codex-last.txt")
	_ = os.Remove(lastMessagePath)

	ctx, span := o.StartExecSpan(ctx,
		attribute.String("codex.workspace", workspace),
		attribute.String("codex.model", os.Getenv("MODEL_NAME")),
	)
	defer span.End()

	args := []string{
		"exec",
		"--json",
		"--skip-git-repo-check",
		"--cd", workspace,
		"--output-last-message", lastMessagePath,
		task,
	}
	cmd := exec.CommandContext(ctx, "codex", args...)
	cmd.Stderr = os.Stderr
	cmd.Stdin = nil
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return "", run, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		harness.MarkSpanError(span, err)
		return "", run, fmt.Errorf("start codex: %w", err)
	}
	run.consume(stdout, os.Stdout)
	runErr := cmd.Wait()
	if runErr != nil {
		harness.MarkSpanError(span, runErr)
		if run.FailureMessage != "" {
			runErr = fmt.Errorf("codex exec failed: %s: %w", run.FailureMessage, runErr)
		} else {
			runErr = fmt.Errorf("codex exec failed: %w", runErr)
		}
	} else if run.FailureMessage != "" {
		// codex exited 0 but the stream reported a fatal error.
		runErr = fmt.Errorf("codex reported: %s", run.FailureMessage)
		harness.MarkSpanError(span, runErr)
	}

	response := ""
	if data, err := os.ReadFile(lastMessagePath); err == nil {
		response = strings.TrimSpace(string(data))
	}
	if response == "" {
		response = strings.TrimSpace(run.LastAgentMessage)
	}
	return response, run, runErr
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
