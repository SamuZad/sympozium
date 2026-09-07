# Configuration

## Environment Variables

| Variable | Component | Description |
|----------|-----------|-------------|
| `EVENT_BUS_URL` | All | NATS server URL |
| `DATABASE_URL` | API Server | PostgreSQL connection string |
| `INSTANCE_NAME` | Channels | Owning Agent name |
| `MEMORY_SERVER_URL` | Agent Runner / API Server / Controller | Base URL of the central memory-server (e.g. `http://sympozium-memory-server.sympozium-system.svc:8080`). When unset, memory tools are disabled. |
| `MEMORY_ADMIN_SAS` | Memory Server | Comma-separated list of `namespace/serviceaccount` allowed to use admin-only endpoints. The Helm chart auto-includes `sympozium-system/sympozium-controller-manager` and `sympozium-system/sympozium-apiserver`. |
| `MEMORY_MEMBERSHIP_CACHE_TTL` | Memory Server | TTL for caller membership cache entries (default `10m`). |
| `MEMORY_MEMBERSHIP_CACHE_SIZE` | Memory Server | Max entries in the membership cache (default `4096`). |
| `MEMORY_POSTGRES_URL` | Memory Server | PostgreSQL+pgvector connection string for memory rows. |
| `MEMORY_EMBEDDING_URL` / `MEMORY_EMBEDDING_MODEL` / `MEMORY_EMBEDDING_API_KEY` | Memory Server | OpenAI-compatible embedding endpoint, model name, and key. |
| `MAX_TOOL_ITERATIONS` | Agent Runner | Maximum tool-call iterations (default: 50). Can also be set per-run via `spec.env` in AgentRun CR. |
| `CHANNEL_ATTACHMENT_MAX_BYTES` | Agent Runner / Codex + Claude Code harnesses | Maximum local attachment size embedded into outbound channel messages (default: 768000 bytes). |
| `CHANNEL_ATTACHMENT_TOTAL_MAX_BYTES` | Codex + Claude Code harnesses | Cumulative base64 budget for files auto-attached to the final answer, kept under the event-bus max message size (default: 900000 base64 bytes). Files beyond the budget are skipped, leaving the text reply intact. |
| `ARTIFACT_SERVER_URL` | Agent Runner / Codex + Claude Code harnesses / Slack channel / Controller | Base URL of the central artifact-server (e.g. `http://sympozium-artifact-server.sympozium-system.svc:8080`). When set, agent-produced files are uploaded by reference (id) and downloaded by channel pods for delivery, so bytes never travel over NATS. When unset, the harness falls back to inline base64 attachments. |
| `CODEX_HOME` | Codex harness | Where codex stores `config.toml`, `auth.json`, `AGENTS.md` and session state (default `/workspace/.codex`, so it persists on a per-session workspace PVC). |
| `CLAUDE_CONFIG_DIR` | Claude Code harness | Where Claude Code stores settings, `CLAUDE.md`, and session transcripts (default `/workspace/.claude`, so it persists on a per-session workspace PVC). |
| `CLAUDE_CODE_CONTINUE` | Claude Code harness | `true`/`false` forces or forbids resuming the previous Claude Code conversation (`--continue`). Default: resume for conversational session keys (channels, web, MCP) when a transcript exists; never for `sched:*`. |
| `CLAUDE_CODE_MAX_BUDGET_USD` | Claude Code harness | Per-run API spend cap in dollars, passed to `claude` as `--max-budget-usd`. |
| `HARNESS_CLAUDE_EXTRA_ARGS` | Claude Code harness | Space-separated extra flags appended to the `claude` invocation (e.g. `--fallback-model claude-sonnet-5`). |
| `MCP_BRIDGE_URL` | Codex + Claude Code harnesses | Loopback MCP endpoint served by the mcp-bridge sidecar (default `http://127.0.0.1:8765/mcp`); only used when the sidecar's manifest (`MCP_MANIFEST_PATH`) lists tools. |
| `ARTIFACT_LISTEN` | Artifact Server | Listen address (default `:8080`). |
| `ARTIFACT_DATA_DIR` | Artifact Server | Directory (PVC mount) where blobs + metadata are stored (default `/data`). |
| `ARTIFACT_MAX_BYTES` | Artifact Server | Maximum accepted upload size in bytes (default `26214400` = 25 MiB). Larger uploads are rejected with HTTP 413. |
| `ARTIFACT_TTL` | Artifact Server | How long artifacts live before the background pruner removes them (default `24h`). Uploads may request a shorter TTL via the `X-Artifact-TTL` header. |
| `ARTIFACT_READER_SERVICE_ACCOUNTS` | Artifact Server | Comma-separated `namespace/serviceaccount` list always allowed to read any artifact, in addition to the owner + sibling-channel convention. |
| `ARTIFACT_ADMIN_SAS` | Artifact Server | Comma-separated `namespace/serviceaccount` list allowed to read/delete any artifact. The Helm chart auto-includes `sympozium-system/sympozium-controller-manager`. |
| `ARTIFACT_TOKEN_CACHE_TTL` / `ARTIFACT_TOKEN_CACHE_SIZE` | Artifact Server | TokenReview cache TTL (default `60s`) and max entries (default `4096`). |
| `ARTIFACT_AGENT_SA_SUFFIX` / `ARTIFACT_CHANNEL_SA_SUFFIX` | Artifact Server | ServiceAccount name suffixes used to pair a producing agent (`<name>-agent`) with its consuming channel pod (`<name>-channel`) for convention-based read authorization (defaults `-agent` / `-channel`). |
| `TELEGRAM_BOT_TOKEN` | Telegram | Bot API token |
| `SLACK_BOT_TOKEN` | Slack | Bot OAuth token |
| `SLACK_APP_TOKEN` | Slack | App-level token for Socket Mode |
| `DISCORD_BOT_TOKEN` | Discord | Bot token |
| `WHATSAPP_ACCESS_TOKEN` | WhatsApp | Cloud API access token |

## LLM Providers

Sympozium supports any GenAI provider with an OpenAI-compatible API:

| Provider | Base URL | API Key Variable |
|----------|----------|-----------------|
| OpenAI | (default) | `OPENAI_API_KEY` |
| Anthropic | (default) | `ANTHROPIC_API_KEY` |
| Azure OpenAI | your endpoint | `AZURE_OPENAI_API_KEY` |
| Ollama | `http://ollama:11434/v1` | none |
| LM Studio | `http://localhost:1234/v1` | none |
| llama-server | `http://localhost:8080/v1` | none |
| Unsloth | `http://localhost:8080/v1` | none |
| Any OpenAI-compatible | custom URL | custom |

See the [Ollama guide](../guides/ollama.md), [LM Studio guide](../guides/lm-studio.md), [llama-server guide](../guides/llama-server.md), or [Unsloth guide](../guides/unsloth.md) for detailed local LLM setup.
