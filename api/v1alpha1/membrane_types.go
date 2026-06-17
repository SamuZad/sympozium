package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// MembraneSpec configures the synthetic membrane layer for shared memory,
// enabling selective permeability, provenance tracking, token budgets,
// and circuit breakers across agent configurations in an ensemble.
type MembraneSpec struct {
	// Permeability defines per-agent-config visibility and selectivity rules.
	// If empty, all entries default to the ensemble-level DefaultVisibility.
	// +optional
	Permeability []PermeabilityRule `json:"permeability,omitempty"`

	// DefaultVisibility is the default visibility tier for new entries
	// when not overridden by a per-agent-config PermeabilityRule.
	// +kubebuilder:validation:Enum=public;trusted;private
	// +kubebuilder:default="public"
	// +optional
	DefaultVisibility string `json:"defaultVisibility,omitempty"`

	// TrustGroups defines named groups of agent configs that share "trusted"
	// visibility. Agents in the same trust group can see each other's
	// "trusted" entries. If empty, trust is derived from ensemble Relationships
	// (delegation and supervision edges imply mutual trust).
	// +optional
	TrustGroups []TrustGroup `json:"trustGroups,omitempty"`

	// Export declares which memories owned by Agents in THIS ensemble may
	// be read by Agents in OTHER ensembles (typically in other namespaces).
	//
	// Export is one half of a two-sided opt-in: a reader only sees an
	// exported memory if its own ensemble's Import rules also accept the
	// producing ensemble. Without a matching Export, cross-ensemble reads
	// silently return zero rows (no 403).
	//
	// Empty list = no cross-ensemble export. This is the safe default; a
	// missing Membrane keeps memory strictly within the owning ensemble.
	// +optional
	Export []MembraneExportRule `json:"export,omitempty"`

	// Import declares which OTHER ensembles' exported memories Agents in
	// THIS ensemble are willing to read.
	//
	// Import is the other half of the opt-in: even if a peer Exports to
	// us, we must Import from them. Unmatched memories are filtered out
	// of search results, not surfaced as permission errors.
	//
	// Empty list = no cross-ensemble import. Search results contain only
	// memories produced inside this ensemble.
	// +optional
	Import []MembraneImportRule `json:"import,omitempty"`

	// TokenBudget configures ensemble-level token spending limits.
	// +optional
	TokenBudget *TokenBudgetSpec `json:"tokenBudget,omitempty"`

	// CircuitBreaker configures failure thresholds for delegation chains.
	// +optional
	CircuitBreaker *CircuitBreakerSpec `json:"circuitBreaker,omitempty"`

	// TimeDecay configures how old entries lose salience in search results.
	// +optional
	TimeDecay *TimeDecaySpec `json:"timeDecay,omitempty"`
}

// MembraneExportRule names a class of memories produced inside this
// ensemble and the set of peer ensembles allowed to read them.
//
// All fields combine with AND semantics. A memory is exportable iff it
// matches every populated filter AND the consumer matches the target
// selectors.
type MembraneExportRule struct {
	// Name is a human-readable identifier (e.g. "prod-incident-postmortems").
	// Surfaces in status conditions and audit logs.
	// +optional
	Name string `json:"name,omitempty"`

	// Visibilities is the set of source-visibility tiers eligible for
	// export. Defaults to ["public"]. "private" entries are NEVER
	// exportable regardless of this field.
	// +optional
	// +kubebuilder:validation:items:Enum=public;trusted
	Visibilities []string `json:"visibilities,omitempty"`

	// Tags filters by memory tags. A memory matches if it carries AT LEAST
	// ONE of these tags. Empty = no tag filter (all memories match).
	// +optional
	Tags []string `json:"tags,omitempty"`

	// ToEnsembles selects which peer Ensembles are allowed to consume
	// memories matched by this rule.
	//
	// Both selectors must match (AND). At least one selector MUST be set
	// — an unrestricted export is rejected at admission to prevent
	// "share with everything" footguns.
	ToEnsembles EnsembleTargetSelector `json:"toEnsembles"`
}

// MembraneImportRule declares which exported memories from peer
// ensembles this ensemble's Agents are willing to read.
//
// Fields combine with AND semantics (same as Export). Unmatched memories
// are filtered out of search results silently.
type MembraneImportRule struct {
	// Name is a human-readable identifier.
	// +optional
	Name string `json:"name,omitempty"`

	// Tags filters which exported memories to ingest. Empty = no tag
	// filter (accept everything the peer exports).
	// +optional
	Tags []string `json:"tags,omitempty"`

	// FromEnsembles selects which peer Ensembles to read from.
	// Same admission rule as Export.ToEnsembles: at least one selector
	// must be set.
	FromEnsembles EnsembleTargetSelector `json:"fromEnsembles"`
}

// EnsembleTargetSelector picks a set of Ensemble objects across the
// cluster by (namespace, ensemble) labels. Both selectors are evaluated
// in label-selector form (matchLabels + matchExpressions); a missing
// selector means "match nothing" — the admission webhook rejects any
// rule whose selectors are both empty.
type EnsembleTargetSelector struct {
	// NamespaceSelector picks namespaces by label. Use the empty selector
	// `{}` to mean "all namespaces" — but only when paired with a
	// non-empty EnsembleSelector, since the combination is what bounds
	// the blast radius.
	// +optional
	NamespaceSelector *metav1.LabelSelector `json:"namespaceSelector,omitempty"`

	// EnsembleSelector picks Ensembles by label within the selected
	// namespaces. Use the empty selector `{}` to mean "all ensembles in
	// the selected namespaces" — but only when paired with a non-empty
	// NamespaceSelector.
	// +optional
	EnsembleSelector *metav1.LabelSelector `json:"ensembleSelector,omitempty"`
}

// PermeabilityRule defines what an agent config exposes to and accepts from
// the shared memory membrane.
type PermeabilityRule struct {
	// AgentConfig is the agent config name this rule applies to.
	AgentConfig string `json:"agentConfig"`

	// DefaultVisibility for entries created by this agent config.
	// Overrides the ensemble-level default.
	// +kubebuilder:validation:Enum=public;trusted;private
	// +optional
	DefaultVisibility string `json:"defaultVisibility,omitempty"`

	// ExposeTags lists tags this agent config publishes through the membrane.
	// Empty means expose all tags. Entries with tags not in this list
	// are treated as "private" regardless of their visibility setting.
	// +optional
	ExposeTags []string `json:"exposeTags,omitempty"`

	// AcceptTags lists tags this agent config is interested in receiving.
	// Empty means accept all visible entries. When set, search results
	// are filtered to only include entries with at least one matching tag.
	// +optional
	AcceptTags []string `json:"acceptTags,omitempty"`
}

// TrustGroup defines a named group of agent configs that share "trusted"
// visibility with each other.
type TrustGroup struct {
	// Name is a human-readable identifier for this trust group.
	Name string `json:"name"`

	// AgentConfigs lists the agent config names in this group.
	AgentConfigs []string `json:"agentConfigs"`
}

// TokenBudgetSpec configures ensemble-level token spending limits.
type TokenBudgetSpec struct {
	// MaxTokens is the maximum total tokens (input+output) across all agent
	// runs in a single ensemble execution wave. 0 means unlimited.
	// +optional
	MaxTokens int64 `json:"maxTokens,omitempty"`

	// MaxTokensPerRun limits tokens for any single AgentRun. 0 means unlimited.
	// +optional
	MaxTokensPerRun int64 `json:"maxTokensPerRun,omitempty"`

	// Action determines what happens when the budget is exceeded.
	// "halt" (default) prevents new runs from starting.
	// "warn" allows runs but sets a warning condition on the ensemble.
	// +kubebuilder:validation:Enum=halt;warn
	// +kubebuilder:default="halt"
	// +optional
	Action string `json:"action,omitempty"`
}

// CircuitBreakerSpec configures failure detection for delegation chains.
type CircuitBreakerSpec struct {
	// ConsecutiveFailures is how many consecutive delegate failures
	// trigger the circuit breaker.
	// +kubebuilder:default=3
	// +optional
	ConsecutiveFailures int `json:"consecutiveFailures,omitempty"`

	// CooldownDuration is how long the breaker stays open before
	// allowing retries. Format: "5m", "1h". Empty means manual reset only.
	// +optional
	CooldownDuration string `json:"cooldownDuration,omitempty"`
}

// TimeDecaySpec configures salience decay for memory entries.
type TimeDecaySpec struct {
	// TTL is the default time-to-live for entries. Entries older than this
	// are excluded from search results but not deleted.
	// Format: "24h", "168h" (7 days).
	// +optional
	TTL string `json:"ttl,omitempty"`

	// DecayFunction controls how relevance decreases with age.
	// +kubebuilder:validation:Enum=linear;exponential
	// +kubebuilder:default="linear"
	// +optional
	DecayFunction string `json:"decayFunction,omitempty"`
}
