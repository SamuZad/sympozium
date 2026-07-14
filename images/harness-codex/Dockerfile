# syntax=docker/dockerfile:1.7
# Sympozium codex harness image.
#
# Stage 1: build the Go shim that translates Sympozium env vars / mounts
# into codex's native config.toml + AGENTS.md, runs `codex exec`, and
# writes /ipc/output/result.json on completion.
#
# Stage 2: a slim Debian-based Node runtime with the official `@openai/codex`
# CLI installed globally. We use a glibc base (not Alpine) because the
# codex npm package downloads a prebuilt Rust binary that does not always
# ship a musl build.

FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
RUN apk add --no-cache git ca-certificates
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=mod -ldflags="-s -w" -o /harness-codex ./cmd/harness-codex \
 && CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -mod=mod -ldflags="-s -w" -o /sympozium-tool ./cmd/sympozium-tool

FROM node:20-bookworm-slim
ARG CODEX_VERSION=0.144.3

# Install codex CLI + minimal CA bundle. dumb-init isn't strictly required
# since codex handles its own signals, but keeps PID 1 well-behaved.
RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates dumb-init jq \
    && rm -rf /var/lib/apt/lists/* \
    && npm install -g --omit=dev "@openai/codex@${CODEX_VERSION}" \
    && npm cache clean --force

COPY --from=builder /harness-codex /harness-codex
# `sympozium-tool` exposes the Sympozium IPC tools (send_channel_message,
# schedule_task, execute_command-into-sidecar) as a shell CLI so codex's
# native local_shell can invoke them. AGENTS.md (written by the shim)
# describes the subcommands; codex picks them up as system context.
COPY --from=builder /sympozium-tool /usr/local/bin/sympozium-tool

# Non-root user. UID 65532 matches distroless/nonroot so pods using a
# shared PodSecurityPolicy/SCC keep working.
RUN groupadd --gid 65532 nonroot \
    && useradd --uid 65532 --gid 65532 --create-home --shell /usr/sbin/nologin nonroot
USER 65532:65532
WORKDIR /workspace

ENTRYPOINT ["/usr/bin/dumb-init", "--", "/harness-codex"]
