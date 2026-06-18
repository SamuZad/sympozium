package v1alpha1

import "time"

// DefaultRunTimeout is the fallback wall-clock cap applied to a single
// AgentRun when neither the Agent nor the AgentRun specify one.
const DefaultRunTimeout = 10 * time.Minute

// EffectiveRunTimeout returns the configured RunTimeout parsed as a
// time.Duration, or DefaultRunTimeout when unset / unparseable.
func (c AgentConfig) EffectiveRunTimeout() time.Duration {
	if c.RunTimeout == "" {
		return DefaultRunTimeout
	}
	d, err := time.ParseDuration(c.RunTimeout)
	if err != nil || d <= 0 {
		return DefaultRunTimeout
	}
	return d
}
