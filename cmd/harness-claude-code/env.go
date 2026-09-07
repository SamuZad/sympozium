package main

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"

	"github.com/sympozium-ai/sympozium/internal/harness"
)

// providerEnv maps Sympozium's MODEL_PROVIDER / MODEL_BASE_URL onto the
// environment Claude Code reads. lookup resolves other env vars (os.Getenv in
// production; a map in tests).
//
// Claude Code speaks the Anthropic Messages API only, through three routes:
// the Anthropic API (ANTHROPIC_API_KEY / ANTHROPIC_AUTH_TOKEN, optional
// ANTHROPIC_BASE_URL), AWS Bedrock (CLAUDE_CODE_USE_BEDROCK=1 + AWS creds)
// and Google Vertex (CLAUDE_CODE_USE_VERTEX=1 + GCP creds). Any other
// MODEL_PROVIDER is treated as an Anthropic-compatible gateway (LiteLLM,
// an enterprise proxy, …): MODEL_BASE_URL becomes ANTHROPIC_BASE_URL and
// that provider's key is surfaced as the bearer token when no Anthropic
// credential is present.
func providerEnv(provider, baseURL string, lookup func(string) string) map[string]string {
	out := map[string]string{}
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = "anthropic"
	}

	switch p {
	case "bedrock", "aws-bedrock", "aws":
		out["CLAUDE_CODE_USE_BEDROCK"] = "1"
	case "vertex", "google-vertex", "gcp-vertex", "vertexai":
		out["CLAUDE_CODE_USE_VERTEX"] = "1"
	}

	if u := strings.TrimSpace(baseURL); u != "" {
		out["ANTHROPIC_BASE_URL"] = strings.TrimRight(u, "/")
	}

	// Credential bridging. Bedrock/Vertex authenticate with cloud
	// credentials, not an Anthropic key, so nothing to bridge there.
	if p == "bedrock" || p == "aws-bedrock" || p == "aws" ||
		p == "vertex" || p == "google-vertex" || p == "gcp-vertex" || p == "vertexai" {
		return out
	}
	if lookup("ANTHROPIC_API_KEY") != "" || lookup("ANTHROPIC_AUTH_TOKEN") != "" {
		return out
	}
	if p != "anthropic" {
		if key := lookup(providerEnvKey(p)); key != "" {
			// A non-Anthropic provider key against an Anthropic-compatible
			// endpoint: gateways expect it as a bearer token.
			out["ANTHROPIC_AUTH_TOKEN"] = key
			return out
		}
	}
	if key := lookup("API_KEY"); key != "" {
		// Generic secret key accepted by the controller's auth allowlist.
		out["ANTHROPIC_API_KEY"] = key
	}
	return out
}

// providerEnvKey maps a Sympozium provider id to the env var that holds its
// API key. Mirrors allowedAuthSecretKeys in internal/controller.
func providerEnvKey(provider string) string {
	switch strings.ToLower(provider) {
	case "anthropic":
		return "ANTHROPIC_API_KEY"
	case "azure", "azure-openai":
		return "AZURE_OPENAI_API_KEY"
	case "google", "gemini":
		return "GOOGLE_API_KEY"
	case "mistral":
		return "MISTRAL_API_KEY"
	case "groq":
		return "GROQ_API_KEY"
	case "deepseek":
		return "DEEPSEEK_API_KEY"
	case "openrouter":
		return "OPENROUTER_API_KEY"
	default:
		return "OPENAI_API_KEY"
	}
}

// modelTuningEnv maps THINKING_MODE, MAX_TOKENS and MODEL_PROVIDER_HEADERS
// onto Claude Code's tuning env vars.
func modelTuningEnv(lookup func(string) string) map[string]string {
	out := map[string]string{}
	if budget, ok := thinkingBudget(lookup("THINKING_MODE")); ok {
		out["MAX_THINKING_TOKENS"] = budget
	}
	if v := strings.TrimSpace(lookup("MAX_TOKENS")); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			out["CLAUDE_CODE_MAX_OUTPUT_TOKENS"] = strconv.Itoa(n)
		}
	}
	if h, ok := customHeaders(lookup("MODEL_PROVIDER_HEADERS")); ok {
		out["ANTHROPIC_CUSTOM_HEADERS"] = h
	}
	return out
}

// thinkingBudget maps the Sympozium THINKING_MODE enum onto Claude Code's
// MAX_THINKING_TOKENS. "off" disables extended thinking (budget 0); an empty
// or unknown mode leaves Claude Code's default behaviour untouched. Budgets
// must fit inside the model's max output tokens — the high tiers assume a
// current-generation Claude model.
func thinkingBudget(mode string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "off":
		return "0", true
	case "minimal":
		return "1024", true
	case "low":
		return "4096", true
	case "medium":
		return "16384", true
	case "high":
		return "32768", true
	case "xhigh":
		return "65536", true
	default:
		return "", false
	}
}

// customHeaders converts the controller's JSON-encoded MODEL_PROVIDER_HEADERS
// ({"X-Team":"sre"}) into Claude Code's ANTHROPIC_CUSTOM_HEADERS format:
// one "Name: Value" pair per line, sorted for determinism.
func customHeaders(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", false
	}
	var headers map[string]string
	if err := json.Unmarshal([]byte(raw), &headers); err != nil || len(headers) == 0 {
		return "", false
	}
	names := make([]string, 0, len(headers))
	for n := range headers {
		if strings.TrimSpace(n) != "" {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	lines := make([]string, 0, len(names))
	for _, n := range names {
		lines = append(lines, n+": "+headers[n])
	}
	if len(lines) == 0 {
		return "", false
	}
	return strings.Join(lines, "\n"), true
}

// telemetryEnv enables Claude Code's native OpenTelemetry export against the
// same OTLP collector the controller configures for the agent-runner, so
// dashboards stay unified across harnesses. Claude Code emits
// claude_code.token.usage / cost.usage / session.count metrics and
// tool/api log events; the collector's transform processor normalises the
// metric names. Returns nil when Sympozium observability is off.
func telemetryEnv(lookup func(string) string) map[string]string {
	if !strings.EqualFold(lookup("SYMPOZIUM_OTEL_ENABLED"), "true") {
		return nil
	}
	endpoint := harness.FirstNonEmpty(
		lookup("SYMPOZIUM_OTEL_OTLP_ENDPOINT"),
		lookup("OTEL_EXPORTER_OTLP_ENDPOINT"),
	)
	if endpoint == "" {
		return nil
	}
	protocol := strings.ToLower(harness.FirstNonEmpty(
		lookup("SYMPOZIUM_OTEL_OTLP_PROTOCOL"),
		lookup("OTEL_EXPORTER_OTLP_PROTOCOL"),
	))
	switch protocol {
	case "grpc":
	case "http", "http/protobuf", "http/json":
		if protocol == "http" {
			protocol = "http/protobuf"
		}
	default:
		if strings.HasPrefix(endpoint, "http") {
			protocol = "http/protobuf"
		} else {
			protocol = "grpc"
		}
	}
	// The JS OTLP gRPC exporter wants a URL, not a bare host:port.
	if protocol == "grpc" && !strings.Contains(endpoint, "://") {
		endpoint = "http://" + endpoint
	}
	return map[string]string{
		"CLAUDE_CODE_ENABLE_TELEMETRY": "1",
		"OTEL_METRICS_EXPORTER":        "otlp",
		"OTEL_LOGS_EXPORTER":           "otlp",
		"OTEL_EXPORTER_OTLP_PROTOCOL":  protocol,
		"OTEL_EXPORTER_OTLP_ENDPOINT":  endpoint,
		// Agent pods are short-lived; flush well inside a typical run so
		// the last data points are not lost at exit.
		"OTEL_METRIC_EXPORT_INTERVAL": "10000",
		"OTEL_LOGS_EXPORT_INTERVAL":   "5000",
	}
}
