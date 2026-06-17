# Sympozium central memory server.
#
# Modes:
#   memory-server migrate   — apply the SQL schema and exit (Helm Job)
#   memory-server serve     — run the HTTP API (Deployment, scalable)
FROM --platform=$BUILDPLATFORM golang:1.25-alpine AS builder
ARG TARGETOS=linux
ARG TARGETARCH
RUN apk add --no-cache git ca-certificates
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -mod=mod -ldflags="-s -w" -o /memory-server ./cmd/memory-server

FROM gcr.io/distroless/static:nonroot
COPY --from=builder /memory-server /memory-server
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/memory-server"]
CMD ["serve"]
