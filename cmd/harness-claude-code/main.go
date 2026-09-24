// harness-claude-code is the Sympozium shim for Anthropic's Claude Code CLI.
//
// It runs as the agent container entrypoint when an Agent / AgentRun selects
// `harness: claude-code`. The shim:
//
//  1. Points CLAUDE_CONFIG_DIR (and, when $HOME is read-only, HOME) at the
//     /workspace volume so Claude Code's settings, memory file and session
//     transcripts land on the session PVC and survive across AgentRuns.
//  2. Renders CLAUDE.md (mounted skills + sympozium-tool docs) into that
//     config dir — Claude Code auto-loads it as memory — and passes the
//     persona system prompt plus channel context via --append-system-prompt.
//  3. Maps Sympozium MODEL_* / THINKING_MODE / MAX_TOKENS env vars onto the
//     ANTHROPIC_* / CLAUDE_CODE_* env vars and --model flag Claude Code
//     understands, and wires the local MCP bridge via --mcp-config.
//  4. Runs `claude -p` in stream-json mode, tees every event to the pod log,
//     extracts the final answer + token usage + tool-call count, and writes
//     /ipc/output/result.json so the controller can surface the result.
//
// Session continuity: on a per-session workspace PVC the shim resumes the
// previous Claude Code conversation (`--continue`) for conversational
// session keys (channels, web, MCP) so a Slack thread reads as one dialogue,
// and starts fresh for schedules. CLAUDE_CODE_CONTINUE=true|false overrides.
//
// Everything that is not Claude-Code-specific (skills/channel context
// assembly, sympozium-tool docs, MCP bridge discovery, attachment scanning,
// IPC result, OTel) lives in internal/harness and is shared with the other
// CLI shims.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"

	"github.com/sympozium-ai/sympozium/internal/artifact"
	"github.com/sympozium-ai/sympozium/internal/harness"
	"github.com/sympozium-ai/sympozium/internal/ipc"
)

const harnessName = "claude-code"

// maxSystemPromptArgBytes bounds what we pass through a single argv entry.
// Linux caps one argument at 128 KiB (MAX_ARG_STRLEN); anything larger is
// routed into CLAUDE.md instead so the run never fails to exec.
const maxSystemPromptArgBytes = 100 * 1024

// mcpConfigFile is the Claude Code MCP config the shim renders inside
// CLAUDE_CONFIG_DIR when the mcp-bridge sidecar advertises tools.
const mcpConfigFile = "sympozium-mcp.json"

func main() {
	if err := run(context.Background()); err != nil {
		fmt.Fprintln(os.Stderr, "harness-claude-code:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) error {
	// Tag Claude Code's native OTel emissions with `harness=claude-code` so
	// dashboards can split metrics by harness. The JS OTel SDK honors
	// OTEL_RESOURCE_ATTRIBUTES per spec; the controller has already populated
	// it with sympozium.instance.name, sympozium.agent_run.id, k8s.namespace.name.
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

	task := os.Getenv("TASK")
	if strings.TrimSpace(task) == "" {
		return fail(fmt.Errorf("TASK env var is empty"))
	}

	workspace := harness.EnvOr("WORKSPACE_DIR", "/workspace")
	configDir, err := prepareConfigDir(workspace)
	if err != nil {
		return fail(err)
	}
	if prevHome := os.Getenv("HOME"); ensureWritableHome(workspace) != prevHome {
		harness.Logf(harnessName, "HOME %q is not writable; using %s", prevHome, workspace)
	}

	inbound := artifact.MaterializeInbound(ctx, workspace)
	systemPrompt := buildSystemPrompt(inbound)
	extraMemory := ""
	if len(systemPrompt) > maxSystemPromptArgBytes {
		harness.Logf(harnessName, "system prompt is %d bytes; routing it through CLAUDE.md instead of argv", len(systemPrompt))
		extraMemory, systemPrompt = systemPrompt, ""
	}
	if err := writeClaudeMD(configDir, extraMemory); err != nil {
		return fail(fmt.Errorf("write CLAUDE.md: %w", err))
	}

	mcpConfigPath, mcpEnabled, err := writeMCPConfig(configDir)
	if err != nil {
		return fail(fmt.Errorf("write MCP config: %w", err))
	}
	if mcpEnabled {
		bridgeURL, _ := harness.ConfiguredMCPBridgeURL(harnessName)
		if err := harness.WaitForMCPBridge(ctx, bridgeURL, 20*time.Second, "harness-claude-code"); err != nil {
			return fail(fmt.Errorf("wait for MCP bridge %s: %w", bridgeURL, err))
		}
	}

	// Translate Sympozium's provider/model/thinking env into Claude Code's
	// own knobs. Operator-supplied values (spec.env) always win.
	applyEnvIfUnset(providerEnv(os.Getenv("MODEL_PROVIDER"), os.Getenv("MODEL_BASE_URL"), os.Getenv))
	applyEnvIfUnset(modelTuningEnv(os.Getenv))
	applyEnvIfUnset(telemetryEnv(os.Getenv))
	if os.Getenv("TEMPERATURE") != "" {
		harness.Logf(harnessName, "TEMPERATURE is not supported by Claude Code; ignoring")
	}

	sessionKey := os.Getenv("SESSION_KEY")
	opts := claudeOptions{
		Task:          task,
		Workspace:     workspace,
		Model:         model,
		SystemPrompt:  systemPrompt,
		MCPConfigPath: mcpConfigPath,
		Continue:      shouldContinueSession(sessionKey, os.Getenv("CLAUDE_CODE_CONTINUE"), hasPriorSession(configDir)),
		MaxBudgetUSD:  maxBudgetUSD(os.Getenv("CLAUDE_CODE_MAX_BUDGET_USD")),
		ExtraArgs:     strings.Fields(os.Getenv("HARNESS_CLAUDE_EXTRA_ARGS")),
	}
	if opts.Continue {
		harness.Logf(harnessName, "continuing previous Claude Code session for %q", sessionKey)
	}

	execCtx := ctx
	if d := runTimeout(os.Getenv("RUN_TIMEOUT")); d > 0 {
		var cancel context.CancelFunc
		execCtx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}

	started := time.Now()
	result, stderrTail, runErr := runClaude(execCtx, o, opts)
	if runErr != nil && opts.Continue && result.Final == nil && strings.Contains(stderrTail, "No conversation found") {
		// The PVC carried session files Claude Code could not resume (e.g. a
		// prior run crashed mid-write). Fall back to a fresh session rather
		// than failing the run.
		harness.Logf(harnessName, "no resumable conversation; retrying with a fresh session")
		opts.Continue = false
		result, _, runErr = runClaude(execCtx, o, opts)
	}
	duration := time.Since(started).Milliseconds()
	if errors.Is(execCtx.Err(), context.DeadlineExceeded) {
		runErr = fmt.Errorf("claude exceeded RUN_TIMEOUT %s", os.Getenv("RUN_TIMEOUT"))
	}
	if runErr == nil {
		runErr = result.Err()
	}

	res := ipc.AgentResult{}
	res.Metrics.DurationMs = duration
	res.Metrics.InputTokens = result.InputTokens()
	res.Metrics.OutputTokens = result.OutputTokens()
	res.Metrics.ToolCalls = result.ToolCalls
	response := result.Response()
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
	if result.Final != nil {
		harness.Logf(harnessName, "session %s finished: subtype=%s turns=%d tools=%d in=%d out=%d cost_usd=%.4f",
			result.Final.SessionID, result.Final.Subtype, result.Final.NumTurns, result.ToolCalls,
			res.Metrics.InputTokens, res.Metrics.OutputTokens, result.Final.TotalCostUSD)
	}
	o.RecordRun(ctx, status, instance, model, namespace, duration)
	// Canonical token / tool metrics from Claude Code's own event stream, so
	// they exist regardless of Claude Code's native telemetry export.
	o.RecordTokenUsage(ctx, model, result.Usage())
	for _, ti := range result.SortedToolInvocations() {
		o.RecordToolInvocation(ctx, ti.Key.Name, ti.Key.Status, int64(ti.Count))
	}

	if err := harness.WriteResult(res); err != nil {
		harness.Logf(harnessName, "failed to write result.json: %v", err)
	}
	return runErr
}

// prepareConfigDir resolves CLAUDE_CONFIG_DIR (default <workspace>/.claude),
// creates it, and exports it so every `claude` invocation — and any helper it
// spawns — stores settings, memory and transcripts on the workspace volume.
// With a per-session PVC that state survives across AgentRuns; on an
// emptyDir it is simply discarded with the pod.
func prepareConfigDir(workspace string) (string, error) {
	configDir := harness.EnvOr("CLAUDE_CONFIG_DIR", filepath.Join(workspace, ".claude"))
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		return "", fmt.Errorf("mkdir claude config dir %s: %w", configDir, err)
	}
	_ = os.Setenv("CLAUDE_CONFIG_DIR", configDir)
	return configDir, nil
}

// ensureWritableHome makes sure $HOME points at a writable directory. The
// agent container runs with readOnlyRootFilesystem, so the image's /home/<user>
// is not writable; Claude Code (and git, npm, …) expect to be able to drop
// dotfiles under $HOME. When the current HOME is unusable we point it at the
// workspace, which also makes `~/.claude` coincide with the default
// CLAUDE_CONFIG_DIR. Returns the effective HOME.
func ensureWritableHome(fallback string) string {
	if home := os.Getenv("HOME"); home != "" && dirWritable(home) {
		return home
	}
	_ = os.Setenv("HOME", fallback)
	return fallback
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".harness-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// writeClaudeMD renders <configDir>/CLAUDE.md, the memory file Claude Code
// loads into every conversation. It carries the stable, bulky context —
// environment notes, mounted skills, sympozium-tool docs — while the per-run
// persona prompt and channel frame travel via --append-system-prompt.
// The file is regenerated on every run so skills removed from the Agent do not
// linger on a persisted PVC. extra, when non-empty, is appended verbatim (used
// when the system prompt is too large for argv).
func writeClaudeMD(configDir, extra string) error {
	var b strings.Builder
	b.WriteString(environmentSection())
	b.WriteString(harness.SkillsMarkdown(harness.EnvOr("SKILLS_DIR", "/skills")))
	b.WriteString(harness.SympoziumToolsSection())
	if extra != "" {
		b.WriteString("\n")
		b.WriteString(extra)
	}
	return os.WriteFile(filepath.Join(configDir, "CLAUDE.md"), []byte(b.String()), 0o644)
}

// environmentSection tells Claude Code what kind of place it woke up in. It
// runs headless: nobody will answer a clarifying question, and the final
// message is what gets relayed back to the user.
func environmentSection() string {
	return "# Sympozium environment\n\n" +
		"You are running headless inside a Sympozium agent pod on Kubernetes. There is no interactive user: " +
		"never stop to ask for confirmation or clarification — make reasonable assumptions, state them, and finish the task. " +
		"Your final message is relayed to the user (and to the channel that triggered this run, if any), so end with a " +
		"clear, self-contained answer. Any file under /workspace or /tmp that your final message references by path " +
		"is attached to that reply automatically.\n\n" +
		"The working directory (/workspace) is your scratch space. " +
		"Your container is intentionally low-privilege; tooling and RBAC live in SkillPack sidecars reached via `sympozium-tool exec`.\n\n"
}

// buildSystemPrompt assembles the per-run, per-conversation context that is
// appended to Claude Code's system prompt: the persona prompt, the channel
// frame, and pre-downloaded inbound attachments. Returns "" when none apply.
func buildSystemPrompt(inboundAttachmentPaths []string) string {
	var b strings.Builder
	b.WriteString(harness.SystemPromptSection())
	b.WriteString(harness.ChannelContextSection())
	b.WriteString(harness.InboundAttachmentsSection(inboundAttachmentPaths))
	return strings.TrimSpace(b.String())
}

// writeMCPConfig renders a Claude Code MCP config pointing at the single
// local mcp-bridge endpoint when the sidecar has discovered tools, and removes
// any stale file otherwise. Returns the config path and whether MCP is on.
func writeMCPConfig(configDir string) (string, bool, error) {
	path := filepath.Join(configDir, mcpConfigFile)
	bridgeURL, ok := harness.ConfiguredMCPBridgeURL(harnessName)
	if !ok {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return "", false, err
		}
		return "", false, nil
	}
	// Claude Code speaks MCP to one local bridge endpoint. The mcp-bridge
	// sidecar owns remote MCP URLs, headers, auth secrets, tool filtering,
	// and dispatch — the harness never sees any of that.
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"sympozium_bridge": map[string]any{
				"type": "http",
				"url":  bridgeURL,
			},
		},
	}
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", false, err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", false, err
	}
	return path, true, nil
}

// hasPriorSession reports whether Claude Code left a conversation transcript
// in the config dir from an earlier run on this workspace. Transcripts live at
// <configDir>/projects/<cwd-slug>/<session-id>.jsonl.
func hasPriorSession(configDir string) bool {
	matches, _ := filepath.Glob(filepath.Join(configDir, "projects", "*", "*.jsonl"))
	return len(matches) > 0
}

// shouldContinueSession decides whether to pass --continue.
//
// Explicit CLAUDE_CODE_CONTINUE wins (true still requires a transcript to
// resume). Otherwise continuity is on for conversational session keys —
// channel threads (chan:), web endpoints, MCP sessions — where each AgentRun
// is one turn of an ongoing dialogue, and off for schedules (sched:), whose
// fires are independent jobs that would otherwise accumulate context and cost
// run over run.
func shouldContinueSession(sessionKey, override string, hasPrior bool) bool {
	switch strings.ToLower(strings.TrimSpace(override)) {
	case "true", "1", "yes", "on":
		return hasPrior
	case "false", "0", "no", "off":
		return false
	}
	if !hasPrior {
		return false
	}
	return !strings.HasPrefix(strings.TrimSpace(sessionKey), "sched:")
}

// runTimeout parses RUN_TIMEOUT (a Go duration such as "30m") into a
// deadline for the claude process; 0 means no shim-side timeout.
func runTimeout(raw string) time.Duration {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// maxBudgetUSD validates CLAUDE_CODE_MAX_BUDGET_USD (a positive dollar
// amount such as "2.50") for --max-budget-usd; anything else disables the cap
// rather than passing garbage to claude.
func maxBudgetUSD(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	v, err := strconv.ParseFloat(raw, 64)
	if err != nil || v <= 0 {
		harness.Logf(harnessName, "ignoring invalid CLAUDE_CODE_MAX_BUDGET_USD %q", raw)
		return ""
	}
	return raw
}

// applyEnvIfUnset exports each key that the operator has not already set.
func applyEnvIfUnset(vars map[string]string) {
	for k, v := range vars {
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}
