package controller

import (
	"strings"
	"testing"
)

func TestScopedFailureOnlyExposesKnownPublicCodes(t *testing.T) {
	if !strings.Contains(scopedFailureSummary("Refused", "AUTH_CAPACITY"), "broker capacity") {
		t.Fatal("capacity is not actionable")
	}
	for _, reason := range []string{"secret-token-example", "AUTH_CAPACITY\nsecret", "AUTH_SECRET_EXAMPLE"} {
		if got := scopedFailureSummary("Refused", reason); got != "Celln scoped execution Refused" {
			t.Fatalf("untrusted reason exposed: %q", got)
		}
	}
}
