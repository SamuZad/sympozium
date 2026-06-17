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
