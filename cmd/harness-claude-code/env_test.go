package main

import (
	"reflect"
	"testing"
)

func lookupFrom(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestProviderEnv(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		baseURL  string
		env      map[string]string
		want     map[string]string
	}{
		{
			name:     "anthropic with key needs nothing",
			provider: "anthropic",
			env:      map[string]string{"ANTHROPIC_API_KEY": "sk-ant"},
			want:     map[string]string{},
		},
		{
			name:     "empty provider defaults to anthropic",
			provider: "",
			env:      map[string]string{"ANTHROPIC_API_KEY": "sk-ant"},
			want:     map[string]string{},
		},
		{
			name:     "anthropic base url override (proxy)",
			provider: "anthropic",
			baseURL:  "https://llm-gw.internal/anthropic/",
			env:      map[string]string{"ANTHROPIC_API_KEY": "sk-ant"},
			want:     map[string]string{"ANTHROPIC_BASE_URL": "https://llm-gw.internal/anthropic"},
		},
		{
			name:     "bedrock flips the bedrock switch and bridges no keys",
			provider: "bedrock",
			env:      map[string]string{"OPENAI_API_KEY": "should-not-leak"},
			want:     map[string]string{"CLAUDE_CODE_USE_BEDROCK": "1"},
		},
		{
			name:     "vertex flips the vertex switch",
			provider: "vertex",
			want:     map[string]string{"CLAUDE_CODE_USE_VERTEX": "1"},
		},
		{
			name:     "openai-compatible gateway bridges provider key as bearer token",
			provider: "openai",
			baseURL:  "http://litellm.svc:4000",
			env:      map[string]string{"OPENAI_API_KEY": "sk-gw"},
			want: map[string]string{
				"ANTHROPIC_BASE_URL":   "http://litellm.svc:4000",
				"ANTHROPIC_AUTH_TOKEN": "sk-gw",
			},
		},
		{
			name:     "openrouter key bridged",
			provider: "openrouter",
			baseURL:  "https://openrouter.ai/api",
			env:      map[string]string{"OPENROUTER_API_KEY": "or-key"},
			want: map[string]string{
				"ANTHROPIC_BASE_URL":   "https://openrouter.ai/api",
				"ANTHROPIC_AUTH_TOKEN": "or-key",
			},
		},
		{
			name:     "existing anthropic auth token is left alone",
			provider: "openai",
			env:      map[string]string{"OPENAI_API_KEY": "sk-gw", "ANTHROPIC_AUTH_TOKEN": "already"},
			want:     map[string]string{},
		},
		{
			name:     "generic API_KEY secret becomes the anthropic key",
			provider: "anthropic",
			env:      map[string]string{"API_KEY": "generic"},
			want:     map[string]string{"ANTHROPIC_API_KEY": "generic"},
		},
		{
			name:     "no credentials yields nothing to set",
			provider: "anthropic",
			want:     map[string]string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := providerEnv(tt.provider, tt.baseURL, lookupFrom(tt.env))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("providerEnv(%q, %q) = %v, want %v", tt.provider, tt.baseURL, got, tt.want)
			}
		})
	}
}

func TestThinkingBudget(t *testing.T) {
	tests := []struct {
		mode string
		want string
		ok   bool
	}{
		{"", "", false},
		{"bogus", "", false},
		{"off", "0", true},
		{"OFF", "0", true},
		{"minimal", "1024", true},
		{"low", "4096", true},
		{"medium", "16384", true},
		{"high", "32768", true},
		{"xhigh", "65536", true},
	}
	for _, tt := range tests {
		got, ok := thinkingBudget(tt.mode)
		if got != tt.want || ok != tt.ok {
			t.Errorf("thinkingBudget(%q) = (%q, %v), want (%q, %v)", tt.mode, got, ok, tt.want, tt.ok)
		}
	}
}

func TestModelTuningEnv(t *testing.T) {
	got := modelTuningEnv(lookupFrom(map[string]string{
		"THINKING_MODE":          "medium",
		"MAX_TOKENS":             "8192",
		"MODEL_PROVIDER_HEADERS": `{"X-Team":"sre","Anthropic-Beta":"foo"}`,
	}))
	want := map[string]string{
		"MAX_THINKING_TOKENS":           "16384",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS": "8192",
		"ANTHROPIC_CUSTOM_HEADERS":      "Anthropic-Beta: foo\nX-Team: sre",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("modelTuningEnv = %v, want %v", got, want)
	}

	// Garbage is ignored rather than propagated.
	got = modelTuningEnv(lookupFrom(map[string]string{
		"MAX_TOKENS":             "lots",
		"MODEL_PROVIDER_HEADERS": "{not json",
	}))
	if len(got) != 0 {
		t.Fatalf("expected nothing for invalid inputs, got %v", got)
	}
}

func TestTelemetryEnv(t *testing.T) {
	if got := telemetryEnv(lookupFrom(nil)); got != nil {
		t.Fatalf("expected nil when observability is off, got %v", got)
	}
	if got := telemetryEnv(lookupFrom(map[string]string{"SYMPOZIUM_OTEL_ENABLED": "true"})); got != nil {
		t.Fatalf("expected nil without an endpoint, got %v", got)
	}

	// Bare host:port → gRPC with a URL scheme added.
	got := telemetryEnv(lookupFrom(map[string]string{
		"SYMPOZIUM_OTEL_ENABLED":       "true",
		"SYMPOZIUM_OTEL_OTLP_ENDPOINT": "sympozium-otel-collector.sympozium-system.svc:4317",
	}))
	if got["CLAUDE_CODE_ENABLE_TELEMETRY"] != "1" || got["OTEL_METRICS_EXPORTER"] != "otlp" || got["OTEL_LOGS_EXPORTER"] != "otlp" {
		t.Fatalf("telemetry not enabled: %v", got)
	}
	if got["OTEL_EXPORTER_OTLP_PROTOCOL"] != "grpc" || got["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://sympozium-otel-collector.sympozium-system.svc:4317" {
		t.Fatalf("unexpected grpc mapping: %v", got)
	}
	if got["OTEL_METRICS_INCLUDE_SESSION_ID"] != "false" || got["OTEL_METRICS_INCLUDE_ACCOUNT_UUID"] != "false" {
		t.Fatalf("per-session/account metric attributes must be disabled to bound cardinality: %v", got)
	}

	// http endpoint → http/protobuf.
	got = telemetryEnv(lookupFrom(map[string]string{
		"SYMPOZIUM_OTEL_ENABLED":      "true",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318",
	}))
	if got["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" || got["OTEL_EXPORTER_OTLP_ENDPOINT"] != "http://collector:4318" {
		t.Fatalf("unexpected http mapping: %v", got)
	}

	// Explicit protocol "http" normalised to the OTel spec value.
	got = telemetryEnv(lookupFrom(map[string]string{
		"SYMPOZIUM_OTEL_ENABLED":       "true",
		"SYMPOZIUM_OTEL_OTLP_ENDPOINT": "http://collector:4318",
		"SYMPOZIUM_OTEL_OTLP_PROTOCOL": "http",
	}))
	if got["OTEL_EXPORTER_OTLP_PROTOCOL"] != "http/protobuf" {
		t.Fatalf("expected http/protobuf, got %v", got)
	}
}
