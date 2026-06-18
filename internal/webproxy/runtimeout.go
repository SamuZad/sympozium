package webproxy

import (
	"time"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

// resolveRunTimeout returns the wall-clock cap for a single AgentRun
// spawned for inst, honouring spec.agents.default.runTimeout when set.
func resolveRunTimeout(inst *sympoziumv1alpha1.Agent) time.Duration {
	return inst.Spec.Agents.Default.EffectiveRunTimeout()
}
