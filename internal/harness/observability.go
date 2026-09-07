package harness

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

// Observability mirrors the agent-runner observability layer so harness pods
// emit the same Sympozium-conventional metrics + spans, tagged with the
// harness name. Token and tool-call counters are populated by each shim from
// its CLI's own output where available; the wrapped CLI's native OTel
// emissions (if any) are normalised by the collector's transform processor.
type Observability struct {
	name     string
	enabled  bool
	tracer   trace.Tracer
	shutdown func(context.Context) error

	agentRuns     metric.Int64Counter
	agentRunDurMs metric.Float64Histogram
}

// InitObservability bootstraps OTel via pkg/telemetry using the same
// SYMPOZIUM_OTEL_* env vars the controller injects for agent-runner.
// Returns a no-op observer when SYMPOZIUM_OTEL_ENABLED is unset/false or
// when the configured OTLP endpoint is unreachable — a harness must never
// fail a run because telemetry is down.
func InitObservability(ctx context.Context, name string) *Observability {
	noop := &Observability{
		name:     name,
		tracer:   otel.Tracer("sympozium/harness-" + name),
		shutdown: func(context.Context) error { return nil },
	}
	if !strings.EqualFold(os.Getenv("SYMPOZIUM_OTEL_ENABLED"), "true") {
		return noop
	}
	endpoint := FirstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_OTLP_ENDPOINT"),
		os.Getenv("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if endpoint == "" {
		log.Printf("harness-%s: SYMPOZIUM_OTEL_ENABLED=true but no OTLP endpoint set; skipping OTel bootstrap", name)
		return noop
	}
	if !CheckOTLPEndpoint(endpoint) {
		log.Printf("harness-%s: OTLP endpoint %s unreachable; falling back to noop", name, endpoint)
		return noop
	}
	_ = os.Setenv("OTEL_EXPORTER_OTLP_ENDPOINT", endpoint)

	serviceName := FirstNonEmpty(
		os.Getenv("SYMPOZIUM_OTEL_SERVICE_NAME"),
		os.Getenv("OTEL_SERVICE_NAME"),
		"sympozium-harness-"+name,
	)

	tel, err := telemetry.Init(ctx, telemetry.Config{
		ServiceName:     serviceName,
		BatchTimeout:    1 * time.Second,
		ShutdownTimeout: 3 * time.Second,
	})
	if err != nil {
		log.Printf("harness-%s: failed to initialize OTel: %v", name, err)
		return noop
	}

	o := &Observability{
		name:     name,
		enabled:  true,
		tracer:   tel.Tracer(),
		shutdown: tel.Shutdown,
	}

	meter := otel.Meter("sympozium/harness-" + name)
	if c, err := meter.Int64Counter(
		"sympozium.agent.runs",
		metric.WithUnit("{run}"),
		metric.WithDescription("Agent runs completed"),
	); err == nil {
		o.agentRuns = c
	} else {
		log.Printf("harness-%s: failed creating metric sympozium.agent.runs: %v", name, err)
	}
	if h, err := meter.Float64Histogram("sympozium.agent.run.duration"); err == nil {
		o.agentRunDurMs = h
	} else {
		log.Printf("harness-%s: failed creating metric sympozium.agent.run.duration: %v", name, err)
	}
	return o
}

// Shutdown flushes and tears down the OTel providers (no-op when disabled).
func (o *Observability) Shutdown(ctx context.Context) error {
	if o == nil || o.shutdown == nil {
		return nil
	}
	return o.shutdown(ctx)
}

// StartRunSpan opens the top-level "sympozium.agent.run" span.
func (o *Observability) StartRunSpan(ctx context.Context, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if o == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return o.tracer.Start(ctx, "sympozium.agent.run", trace.WithAttributes(attrs...))
}

// StartExecSpan opens the child span covering the wrapped CLI's execution,
// named "sympozium.harness.<name>.exec".
func (o *Observability) StartExecSpan(ctx context.Context, attrs ...attribute.KeyValue) (context.Context, trace.Span) {
	if o == nil {
		return ctx, trace.SpanFromContext(ctx)
	}
	return o.tracer.Start(ctx, "sympozium.harness."+o.name+".exec", trace.WithAttributes(attrs...))
}

// RecordRun increments the run counter and records the duration histogram
// with the standard attribute set (instance, status, namespace, model,
// harness).
func (o *Observability) RecordRun(
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
		attribute.String("harness", o.name),
	)
	if o.agentRuns != nil {
		o.agentRuns.Add(ctx, 1, attrs)
	}
	if o.agentRunDurMs != nil {
		o.agentRunDurMs.Record(ctx, float64(durationMs), attrs)
	}
}

// MarkSpanError records err on span and sets the span status to Error.
func MarkSpanError(span trace.Span, err error) {
	if span == nil || err == nil {
		return
	}
	span.RecordError(err)
	span.SetStatus(codes.Error, err.Error())
}

// WriteTraceContext drops a trace-context.json next to the workspace marker
// so downstream tooling (and re-entrant runs on a session-scoped PVC) can
// correlate logs without re-parsing OTel state.
func WriteTraceContext(ctx context.Context, name string) {
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
		"harness":       name,
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return
	}
	path := filepath.Join(EnvOr("WORKSPACE_DIR", "/workspace"), ".sympozium", "trace-context.json")
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

// CheckOTLPEndpoint returns true when a TCP connection to the OTLP endpoint
// succeeds within 2s. Used to degrade to no-op telemetry instead of blocking
// a run behind an unreachable collector.
func CheckOTLPEndpoint(endpoint string) bool {
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
