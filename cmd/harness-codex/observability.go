package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sympozium-ai/sympozium/pkg/telemetry"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// harnessObservability mirrors the agent-runner observability layer so codex
// pods emit the same Sympozium-conventional metrics + spans. Tokens and
// tool-call counters are stubs for now; populating them would require
// parsing `codex exec --json` events (deferred).
type harnessObservability struct {
	enabled  bool
	tracer   trace.Tracer
	shutdown func(context.Context) error

	agentRuns     metric.Int64Counter
	agentRunDurMs metric.Float64Histogram
}

var harnessObs = &harnessObservability{
	tracer:   otel.Tracer("sympozium/harness-codex"),
	shutdown: func(context.Context) error { return nil },
}

// initObservability bootstraps OTel via pkg/telemetry using the same
// SYMPOZIUM_OTEL_* env vars the controller injects for agent-runner.
// Returns a no-op observer when SYMPOZIUM_OTEL_ENABLED is unset/false or
// when the configured OTLP endpoint is unreachable.
func initObservability(ctx context.Context) *harnessObservability {
	if !strings.EqualFold(os.Getenv("SYMPOZIUM_OTEL_ENABLED"), "true") {
		return harnessObs
	}
	endpoint := firstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_OTLP_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if endpoint == "" {
		log.Println("harness-codex: SYMPOZIUM_OTEL_ENABLED=true but no OTLP endpoint set; skipping OTel bootstrap")
		return harnessObs
	}
	if !checkOTLPEndpoint(endpoint) {
		log.Printf("harness-codex: OTLP endpoint %s unreachable; falling back to noop", endpoint)
		return harnessObs
	}
	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)

	serviceName := firstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_SERVICE_NAME"),
		os.Getenv("OTEL_SERVICE_NAME"),
		"sympozium-harness-codex",
	)

	tel, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:     serviceName,
		BatchTimeout:    1 * time.Second,
		ShutdownTimeout: 3 * time.Second,
	})
	if err != nil {
		log.Printf("harness-codex: failed to initialize OTel: %v", err)
		return harnessObs
	}

	o := &harnessObservability{
		enabled:  true,
		tracer:   tel.Tracer(),
		shutdown: tel.Shutdown,
	}

	meter := otel.Meter("sympozium/harness-codex")
	if c, err := meter.Int64Counter(
		"sympozium.agent.runs",
		metric.WithUnit("{run}"),
		metric.WithDescription("Agent runs completed"),
	); err == nil {
		o.agentRuns = c
	} else {
		log.Printf("harness-codex: failed creating metric sympozium.agent.runs: %v", err)
	}
	if h, err := meter.Float64Histogram("sympozium.agent.run.duration"); err == nil {
		o.agentRunDurMs = h
	} else {
		log.Printf("harness-codex: failed creating metric sympozium.agent.run.duration: %v", err)
	}

	harnessObs = o
	return o
}

func (o *harnessObservability) startRunSpan(ctx context.Context, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if o == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return o.tracer.Start(ctx, "sympozium.agent.run", trace.WithAttributes(attrs...))
}

func (o *harnessObservability) startCodexSpan(ctx context.Context, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if o == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return o.tracer.Start(ctx, "sympozium.harness.codex.exec", trace.WithAttributes(attrs...))
}

func (o *harnessObservability) recordRun(
	ctx context.Context,
	status, instance, model, namespace string,
	durationMs int64,
) {
	if o == nil || !o.enabled {
		return
	}
	attrs := metric.WithAttributes(
		attribute.String("instance", instance),
		attribute.String("status", status),
		attribute.String("namespace", namespace),
		attribute.String("model", model),
		attribute.String("harness", "codex"),
	)
	if o.agentRuns != nil {
		o.agentRuns.Add(ctx, 1, attrs)
	}
	if o.agentRunDurMs != nil {
		o.agentRunDurMs.Record(ctx, float64(durationMs), attrs)
	}
}

func markSpanError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// writeTraceContext drops a trace-context.json next to the workspace marker
// so downstream tooling (and re-entrant runs on a session-scoped PVC) can
// correlate logs without re-parsing OTel state.
func writeTraceContext(ctx context.Context) {
	sc := trace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return
	}
	payload := map[string]string{
		"trace_id":      sc.TraceID().String(),
		"span_id":       sc.SpanID().String(),
		"traceparent":   formatTraceparent(sc),
		"agent_run_id":  os.Getenv("AGENT_RUN_ID"),
		"instance_name": os.Getenv("INSTANCE_NAME"),
		"namespace":     os.Getenv("AGENT_NAMESPACE"),
		"model":         os.Getenv("MODEL_NAME"),
		"harness":       "codex",
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	path := "/workspace/.sympozium/trace-context.json"
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	_ = os.WriteFile(path, data, 0o644)
}

func formatTraceparent(sc trace.SpanContext) string {
	if !sc.IsValid() {
		return ""
	}
	flags := "00"
	if sc.IsSampled() {
		flags = "01"
	}
	return "00-" + sc.TraceID().String() + "-" + sc.SpanID().String() + "-" + flags
}

func checkOTLPEndpoint(endpoint string) bool {
	addr := strings.TrimPrefix(strings.TrimPrefix(endpoint, "https://"), "http://")
	if addr == "" {
		return false
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}
