package controller

// Only a closed set of public reason codes is copied to tenant-visible status.
// Arbitrary native diagnostics can include topology or guest-controlled text.
func scopedFailureSummary(phase, reason string) string {
	switch reason {
	case "AUTH_CAPACITY":
		return "AUTH_CAPACITY: native cell, memory or broker capacity is currently reserved. Stop another conversation and wait for confirmed cleanup, or ask the operator to increase capacity. This failed run will not restart automatically."
	case "AUTH_WORK_DEADLINE_EXPIRED":
		return "AUTH_WORK_DEADLINE_EXPIRED: the original execution deadline expired. Inspect cleanup before starting a fresh run; the old deadline cannot be extended."
	case "AUTH_BUDGET_EXHAUSTED":
		return "AUTH_BUDGET_EXHAUSTED: the original shared allowance is exhausted. Refreshing does not restore it."
	case "AUTH_POLICY_WITHDRAWN":
		return "AUTH_POLICY_WITHDRAWN: the original execution is no longer approved by operator policy."
	}
	return "Celln scoped execution " + phase
}
