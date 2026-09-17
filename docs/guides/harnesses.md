# Harnesses: Running Codex and Claude Code as the Agent Loop

By default every AgentRun executes the built-in **agent-runner**: Sympozium's own
LLM loop with native function-calling tools. A *harness* swaps that loop for a
third-party coding agent while keeping everything around it — skills, channels,
schedules, memory, MCP servers, sidecars, observability — exactly the same.

| `harness` value | Agent container image | What runs inside |
|-----------------|-----------------------|------------------|
| `""` / `agent-runner` | `agent-runner` | Sympozium's LLM loop (default) |
| `codex` | `harness-codex` | OpenAI's `codex exec` behind a Go shim |
| `claude-code` | `harness-claude-code` | Anthropic's `claude -p` behind a Go shim |

The controller maps the enum to a controller-managed image (same registry and
tag as the rest of the release), so you never pin harness digests by hand.
Unknown values fall back to the agent-runner.

## Selecting a harness

Set it on the Agent (inherited by every run, schedule fire and channel message),
on an Ensemble (default for every stamped persona, overridable per persona), or
directly on an ad-hoc AgentRun:

```yaml
apiVersion: sympozium.ai/v1alpha1
kind: Agent
metadata:
  name: repo-fixer
spec:
  harness: claude-code
  workspace:
    perSessionPVC: true          # keep Claude Code's session state across runs
  agents:
    default:
      model: claude-fable-5-1
      systemPrompt: |
        You are the on-call engineer for the payments repo.
  authRefs:
    - secret: anthropic-key      # holds ANTHROPIC_API_KEY
  skills:
    - github-gitops
```

```yaml
apiVersion: sympozium.ai/v1alpha1
kind: Ensemble
spec:
  harness: codex                 # ensemble-wide default
  personas:
    - name: reviewer
      harness: claude-code       # this persona opts out of codex
    - name: fixer
      harness: agent-runner      # this one uses the built-in loop
```

Pair a harness with `workspace.perSessionPVC: true` whenever you want the
CLI's own state (Claude Code transcripts and memory, codex `~/.codex`) to
survive between AgentRuns of the same session — a Slack thread, a web
endpoint session, a schedule. Without it `/workspace` is an emptyDir and each
run starts from a clean slate.

## What every harness shares

Both shims are thin Go binaries (`cmd/harness-codex`, `cmd/harness-claude-code`)
built on `internal/harness`. On start they:

1. **Assemble context** from the mounted skills (`/skills/**/*.md`), the
   persona `systemPrompt`, the channel frame (which channel/chat/thread the
   task came from and the exact `sympozium-tool send-message` command to
   reply), and any inbound attachments already downloaded to
   `/workspace/attachments/`.
2. **Expose Sympozium tools as a shell CLI.** The CLI agent's general-purpose
   tool is a shell, so the image ships `sympozium-tool` and the context
   documents its subcommands: `send-message`, `schedule` (create/update/
   suspend/resume/delete plus `status`/`list`, each returning the state the
   controller applied), `exec --target <skillpack> -- <cmd>` (runs in a
   SkillPack sidecar with its RBAC), `memory-search|store|list`,
   `get-attachment`. They write the same `/ipc/**` files the agent-runner
   does, so the IPC bridge treats them identically.
3. **Wire MCP through the bridge.** When the `mcp-bridge` sidecar has
   discovered tools it publishes a manifest; the shim then points the CLI at
   the single loopback endpoint `http://127.0.0.1:8765/mcp` and waits for it to
   answer `initialize`. Remote URLs, headers and secrets never reach the CLI.
4. **Run the CLI**, capture its final answer, and write
   `/ipc/output/result.json` with status, response, token counts and
   duration — the same contract as the agent-runner.
5. **Auto-attach produced files.** Any path under `/workspace` or `/tmp` the
   final answer references (Markdown link or bare absolute path) is attached
   to the relayed reply — uploaded to the artifact-server when
   `ARTIFACT_SERVER_URL` is set, inline base64 otherwise. See
   [Writing Tools](writing-tools.md#large-files-the-artifact-server-reference-by-id).
6. **Emit telemetry** tagged `harness=<name>`. The shim parses the CLI's own
   event stream (`codex exec --json`, `claude -p --output-format stream-json`)
   and emits the same canonical series the agent-runner does, with the same
   instrument types and attributes: `gen_ai.client.token.usage` (histogram;
   `gen_ai.token.type` = `input` | `output` | `cache_read` | `cache_write`,
   disjoint buckets that sum to the billed total), `sympozium.tool.invocations`
   (counter; `tool_name`, `status`), `sympozium.agent.runs`,
   `sympozium.agent.run.duration`, and a `sympozium.harness.<name>.exec` span.
   One PromQL query therefore covers every harness — for example
   `sum by (harness, gen_ai_token_type) (rate(gen_ai_client_token_usage_sum[5m]))`.
   The shim also enables the CLI's *native* OTel export against the same
   collector; those metrics (`codex.turn.token_usage`, `claude_code.token.usage`,
   `claude_code.cost.usage`, …) keep their own names and semantics as optional
   detail and are not renamed, because they differ in instrument type and
   bucket vocabulary. Codex's `metrics_exporter` defaults to OpenAI's Statsig
   sink; the shim points it at the collector or sets it to `none`. Claude Code's
   per-session and per-account metric attributes are disabled by the shim to
   keep Prometheus cardinality bounded.

The agent container keeps the standard hardening — read-only root filesystem,
all capabilities dropped, non-root UID 65532, NetworkPolicy. That pod boundary
is the security model; both shims therefore disable the CLI's *own* sandbox /
permission prompts (`sandbox_mode = "danger-full-access"` for codex,
`--dangerously-skip-permissions` for Claude Code), which would otherwise only
block legitimate writes to `/ipc` and calls to the memory-server.

## Claude Code harness

### Configuration mapping

| Sympozium input | Claude Code equivalent | Notes |
|-----------------|------------------------|-------|
| `model.model` (`MODEL_NAME`) | `--model` | Any Claude model id or alias Claude Code accepts. |
| `model.provider` = `anthropic` / empty | `ANTHROPIC_API_KEY` from the auth secret | Default. |
| `model.provider` = `bedrock` | `CLAUDE_CODE_USE_BEDROCK=1` | Supply AWS credentials/region via `spec.env` or IRSA. |
| `model.provider` = `vertex` | `CLAUDE_CODE_USE_VERTEX=1` | Supply GCP credentials/project via `spec.env` or workload identity. |
| `model.provider` = anything else + `model.baseURL` | `ANTHROPIC_BASE_URL` + `ANTHROPIC_AUTH_TOKEN` | For Anthropic-compatible gateways (LiteLLM, enterprise proxies). The provider's key (e.g. `OPENAI_API_KEY`) becomes the bearer token. Claude Code speaks the Anthropic Messages API only. |
| `model.baseURL` (`MODEL_BASE_URL`) | `ANTHROPIC_BASE_URL` | Trailing slash stripped. |
| `model.thinking` (`THINKING_MODE`) | `MAX_THINKING_TOKENS` | `off`→0, `minimal`→1024, `low`→4096, `medium`→16384, `high`→32768, `xhigh`→65536. Empty leaves Claude Code's default. |
| `model.maxTokens` (`MAX_TOKENS`) | `CLAUDE_CODE_MAX_OUTPUT_TOKENS` | |
| `model.providerHeaders` | `ANTHROPIC_CUSTOM_HEADERS` | One `Name: Value` per line. |
| `model.temperature` | — | Not supported by Claude Code; ignored with a log line. |
| `timeout` (`RUN_TIMEOUT`) | shim-side deadline | `claude` gets SIGTERM, then SIGKILL after 15s; the partial answer is still recorded. |
| observability enabled | `CLAUDE_CODE_ENABLE_TELEMETRY=1`, `OTEL_*_EXPORTER=otlp` | Points at the controller-configured OTLP endpoint. |

Anything the shim derives is set **only if unset**, so a value you put in
`spec.env` (for example `ANTHROPIC_BASE_URL` or `MAX_THINKING_TOKENS`) always wins.

### Where the context goes

Claude Code has two natural places for context, and the shim uses both:

- `CLAUDE_CONFIG_DIR/CLAUDE.md` (default `/workspace/.claude/CLAUDE.md`) —
  Claude Code's memory file, regenerated every run. It carries the stable,
  bulky material: a short "you are running headless in a Sympozium pod" note,
  the mounted skills, and the `sympozium-tool` reference.
- `--append-system-prompt` — the per-run material: the persona
  `systemPrompt`, the channel frame, and the inbound attachment list. (If this
  ever exceeds ~100 KiB it is routed into `CLAUDE.md` instead so the process can
  still exec.)

The task itself is fed on **stdin**, never argv, so long tasks are safe.

### State and session continuity

`CLAUDE_CONFIG_DIR` defaults to `/workspace/.claude` and `HOME` is pointed at
`/workspace` when the image's home directory is read-only, so *all* Claude Code
state — settings, `.claude.json`, memory, `projects/**/*.jsonl` transcripts —
lands on the workspace volume. With `workspace.perSessionPVC: true` that state
persists across runs, and the shim resumes the previous conversation with
`--continue`:

| Session key | Default | Why |
|-------------|---------|-----|
| `chan:*` (channel threads), web, MCP, ad-hoc | continue when a transcript exists | Each AgentRun is one turn of an ongoing dialogue; a Slack thread reads as one conversation. |
| `sched:*` (schedules) | start fresh | Fires are independent jobs; continuing would accumulate context and cost run over run. |

Override with `CLAUDE_CODE_CONTINUE=true|false` in `spec.env`. If Claude Code
cannot resume (e.g. a prior run died mid-write) the shim retries once with a
fresh session instead of failing the run.

### Execution and result

The shim runs

```
claude -p --output-format stream-json --verbose --dangerously-skip-permissions \
  [--model M] [--append-system-prompt …] [--mcp-config … --strict-mcp-config] \
  [--continue] [--max-budget-usd N] [extra args] < task
```

Every stream-json event is echoed to the pod log, so `kubectl logs -c agent`
shows tool calls and the final result as JSONL, plus human-readable
`harness-claude-code: tool_use <Tool>` and a one-line session summary
(`turns=… tools=… in=… out=… cost_usd=…`). Token usage
(input + cache-creation + cache-read, output) and the tool-call count populate
`status.metrics`. An `error_*` result subtype (e.g. `error_max_budget_usd`) or a
non-zero exit marks the run `Failed` while keeping any partial answer.

`--strict-mcp-config` makes Claude Code ignore `.mcp.json` files a checked-out
repository might contain; only the Sympozium bridge is offered. Claude Code's
auto-updater, crash reporting and product telemetry are disabled in the image
(`DISABLE_AUTOUPDATER`, `CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC`); the OTel
export the shim enables is unaffected.

### Extra knobs (via `spec.env`)

| Variable | Effect |
|----------|--------|
| `CLAUDE_CONFIG_DIR` | Move Claude Code's state directory (default `/workspace/.claude`). |
| `CLAUDE_CODE_CONTINUE` | `true`/`false` — force or forbid `--continue`. |
| `CLAUDE_CODE_MAX_BUDGET_USD` | Per-run API spend cap, passed as `--max-budget-usd` (e.g. `2.50`). |
| `HARNESS_CLAUDE_EXTRA_ARGS` | Space-separated extra `claude` flags, e.g. `--fallback-model claude-sonnet-5`. |
| `ANTHROPIC_*`, `CLAUDE_CODE_*`, `MAX_THINKING_TOKENS` | Any native Claude Code variable; overrides the shim's derived value. |

## Codex harness

The codex shim renders `$CODEX_HOME/config.toml` (model, provider table with
`base_url`/`env_key` for OpenAI-compatible endpoints, `sandbox_mode`,
`approval_policy`, `model_reasoning_effort` from `THINKING_MODE`, the
`[mcp_servers.sympozium_bridge]` block, analytics off, `[otel]` export),
materialises `auth.json` from `OPENAI_API_KEY`, writes `AGENTS.md` with the
assembled context, and runs `codex exec --skip-git-repo-check --cd /workspace
--output-last-message …`. `CODEX_HOME` defaults to `/workspace/.codex`.

## Building and loading the images

```bash
make docker-build-harness-claude-code TAG=v0.1.0
make docker-build-harness-codex TAG=v0.1.0
kind load docker-image ghcr.io/sympozium-ai/sympozium/harness-claude-code:v0.1.0 --name kind
```

The CLI versions are pinned with build args (`CLAUDE_CODE_VERSION`,
`CODEX_VERSION`) in `images/harness-*/Dockerfile`; bump them deliberately and
rebuild. Both images are part of `make docker-build` / `make kind-load`.

An end-to-end smoke test lives at
`test/integration/test-claude-code-harness.sh` (needs a Kind cluster with the
image loaded and `ANTHROPIC_API_KEY`).
