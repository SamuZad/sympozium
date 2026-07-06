# Sympozium artifact-server.
#
# A small, standalone, filesystem-backed blob store for agent-produced files.
# Agents upload files here over authenticated HTTP and reference them by an
# unguessable id; channel pods download by id to deliver them. Bytes never
# travel over the event bus.
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
RUN apk add --no-cache git ca-certificates
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -mod=mod -ldflags="-s -w" -o /artifact-server ./cmd/artifact-server

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /artifact-server /artifact-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/artifact-server"]
