package harness

import (
	"context"
	"testing"
)

func TestTokenUsageTotals(t *testing.T) {
	u := TokenUsage{Input: 100, Output: 40, CacheRead: 300, CacheWrite: 20}
	if u.Total() != 460 {
		t.Errorf("Total = %d, want 460", u.Total())
	}
	if u.PromptTotal() != 420 {
		t.Errorf("PromptTotal = %d, want 420", u.PromptTotal())
	}
}

func TestTokenSeries_SkipsEmptyBuckets(t *testing.T) {
	series := tokenSeries(TokenUsage{Input: 10, Output: 5})
	if len(series) != 2 {
		t.Fatalf("expected 2 series, got %d: %+v", len(series), series)
	}
	if series[0].Type != "input" || series[0].Count != 10 || series[1].Type != "output" || series[1].Count != 5 {
		t.Fatalf("unexpected series: %+v", series)
	}

	series = tokenSeries(TokenUsage{CacheRead: 7, CacheWrite: 3})
	if len(series) != 2 || series[0].Type != "cache_read" || series[1].Type != "cache_write" {
		t.Fatalf("unexpected cache series: %+v", series)
	}
	if len(tokenSeries(TokenUsage{})) != 0 {
		t.Fatal("empty usage must produce no series")
	}
}

func TestRecordersAreNoopsWhenDisabled(t *testing.T) {
	// InitObservability with SYMPOZIUM_OTEL_ENABLED unset returns a disabled
	// observer; every recorder must be safe to call (and on a nil receiver).
	t.Setenv("SYMPOZIUM_OTEL_ENABLED", "")
	o := InitObservability(context.Background(), "test")
	o.RecordTokenUsage(context.Background(), "m", TokenUsage{Input: 1})
	o.RecordToolInvocation(context.Background(), "Bash", "success", 1)
	o.RecordRun(context.Background(), "success", "i", "m", "ns", 1)

	var nilObs *Observability
	nilObs.RecordTokenUsage(context.Background(), "m", TokenUsage{Input: 1})
	nilObs.RecordToolInvocation(context.Background(), "Bash", "success", 1)
	if err := nilObs.Shutdown(context.Background()); err != nil {
		t.Fatalf("nil Shutdown: %v", err)
	}
}
