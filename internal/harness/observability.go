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

	agentRuns       metric.Int64Counter
	agentRunDurMs   metric.Float64Histogram
	tokenUsage      metric.Int64Histogram
	toolInvocations metric.Int64Counter
}

// TokenUsage is a run's token consumption split into disjoint buckets, so
// summing all four gives the total billed tokens. Each CLI reports these
// differently — Claude Code (Anthropic API) already reports input,
// cache_read and cache_creation as disjoint figures; Codex (OpenAI Responses
// API) reports cached and cache-write tokens as breakdowns *of* input_tokens —
// so shims normalise into this shape before recording.
type TokenUsage struct {
	// Input is uncached prompt tokens.
	Input int64
	// Output is completion tokens (reasoning included where the API folds
	// it into output).
	Output int64
	// CacheRead is prompt tokens served from the provider's prompt cache.
	CacheRead int64
	// CacheWrite is prompt tokens written into the provider's prompt cache.
	CacheWrite int64
}

// Total returns the sum of every bucket.
func (u TokenUsage) Total() int64 { return u.Input + u.Output + u.CacheRead + u.CacheWrite }

// PromptTotal returns everything the model read: uncached input plus cache
// traffic. This is what the IPC result's inputTokens metric represents.
func (u TokenUsage) PromptTotal() int64 { return u.Input + u.CacheRead + u.CacheWrite }

// tokenSeries maps a TokenUsage onto (gen_ai.token.type, count) pairs,
// skipping empty buckets. "input" and "output" follow the OTel GenAI
// semconv; the cache buckets are Sympozium extensions.
func tokenSeries(u TokenUsage) []struct {
	Type  string
	Count int64
} {
	all := []struct {
		Type  string
		Count int64
	}{
		{"input", u.Input},
		{"output", u.Output},
		{"cache_read", u.CacheRead},
		{"cache_write", u.CacheWrite},
	}
	out := all[:0]
	for _, s := range all {
		if s.Count > 0 {
			out = append(out, s)
		}
	}
	return out
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
	// Same instrument, name and attribute set as the agent-runner, so one
	// query covers every harness. The wrapped CLIs' native token metrics
	// (codex.turn.token_usage, claude_code.token.usage) keep their own names
	// and instrument types; this is the canonical one.
	if h, err := meter.Int64Histogram(
		"gen_ai.client.token.usage",
		metric.WithUnit("{token}"),
		metric.WithDescription("Number of input and output tokens used"),
	); err == nil {
		o.tokenUsage = h
	} else {
		log.Printf("harness-%s: failed creating metric gen_ai.client.token.usage: %v", name, err)
	}
	if c, err := meter.Int64Counter(
		"sympozium.tool.invocations",
		metric.WithUnit("{invocation}"),
		metric.WithDescription("Tool invocations by the agent"),
	); err == nil {
		o.toolInvocations = c
	} else {
		log.Printf("harness-%s: failed creating metric sympozium.tool.invocations: %v", name, err)
	}
	return o
}

// RecordTokenUsage records one histogram sample per non-empty token bucket,
// attributed like the agent-runner's samples (model, gen_ai.token.type) plus
// harness. Shims call it once per run with the totals they parsed from the
// CLI's own output, so the metric exists even when the CLI's native
// telemetry is off.
func (o *Observability) RecordTokenUsage(ctx context.Context, model string, u TokenUsage) {
	if o == nil || !o.enabled || o.tokenUsage == nil {
		return
	}
	for _, s := range tokenSeries(u) {
		o.tokenUsage.Record(ctx, s.Count, metric.WithAttributes(
			attribute.String("model", model),
			attribute.String("gen_ai.token.type", s.Type),
			attribute.String("harness", o.name),
		))
	}
}

// RecordToolInvocation counts one tool call with the agent-runner's
// attribute set (tool_name, status) plus harness.
func (o *Observability) RecordToolInvocation(ctx context.Context, toolName, status string, count int64) {
	if o == nil || !o.enabled || o.toolInvocations == nil || count <= 0 {
		return
	}
	o.toolInvocations.Add(ctx, count, metric.WithAttributes(
		attribute.String("tool_name", toolName),
		attribute.String("status", status),
		attribute.String("harness", o.name),
	))
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
