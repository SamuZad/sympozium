# syntax=docker/dockerfile:1.7
# Sympozium Claude Code harness image.
#
# Stage 1: build the Go shim that translates Sympozium env vars / mounts
# into Claude Code's native config (CLAUDE_CONFIG_DIR, CLAUDE.md, MCP
# config, ANTHROPIC_* env), runs `claude -p`, and writes
# /ipc/output/result.json on completion.
#
# Stage 2: a slim Debian-based Node runtime with the official
# `@anthropic-ai/claude-code` CLI installed globally. Claude Code 2.x
# requires Node >= 22; we use a glibc base (not Alpine) because the package
# ships prebuilt native helpers (ripgrep) that do not always have musl builds.

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
RUN apk add --no-cache git ca-certificates
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=mod -ldflags="-s -w" -o /harness-claude-code ./cmd/harness-claude-code \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=mod -ldflags="-s -w" -o /sympozium-tool ./cmd/sympozium-tool

FROM node:22-bookworm-slim
ARG CLAUDE_CODE_VERSION=2.1.280

# Install Claude Code + the tools it leans on. git is required for most of
# Claude Code's repo-aware behaviour; jq/curl are cheap conveniences for the
# agent's shell. dumb-init keeps PID 1 well-behaved so SIGTERM from the
# kubelet reaches `claude` and it can flush its transcript.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates dumb-init git jq curl \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g --omit=dev "@anthropic-ai/claude-code@${CLAUDE_CODE_VERSION}" \
    && npm cache clean --force

# Sympozium-managed pods only talk to the operator's own collector: turn off
# Claude Code's auto-updater, crash reporting and product telemetry. This
# does NOT affect the OTel export the shim enables (CLAUDE_CODE_ENABLE_TELEMETRY).
ENV DISABLE_AUTOUPDATER=1 \
    CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1

COPY --from=builder /harness-claude-code /harness-claude-code
# `sympozium-tool` exposes the Sympozium IPC tools (send-message,
# schedule, exec-into-sidecar, memory-*) as a shell CLI so Claude Code's
# Bash tool can invoke them. CLAUDE.md (written by the shim) describes the
# subcommands; Claude Code loads it as memory.
COPY --from=builder /sympozium-tool /usr/local/bin/sympozium-tool

# Non-root user. UID 65532 matches distroless/nonroot so pods using a
# shared PodSecurityPolicy/SCC keep working. Claude Code refuses
# --dangerously-skip-permissions as root, so non-root is also functional.
RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin nonroot
USER 65532:65532
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/harness-claude-code"]
