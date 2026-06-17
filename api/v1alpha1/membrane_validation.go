package v1alpha1

import (
	"fmt"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ValidateMembrane returns the list of validation errors found in a
// MembraneSpec. It is intentionally side-effect free and dependency-free
// so it can be called from any binary that has the typed Spec — the
// controller (to surface status conditions) and the memory-server (to
// treat invalid rules as "matches nothing" defensively).
//
// Returns nil if the spec is valid (or nil).
func ValidateMembrane(spec *MembraneSpec) []error {
	if spec == nil {
		return nil
	}
	var errs []error
	for i, rule := range spec.Export {
		if err := validateEnsembleTargetSelector(rule.ToEnsembles); err != nil {
			errs = append(errs, fmt.Errorf("membrane.export[%d] (%q): %w", i, rule.Name, err))
		}
		for j, v := range rule.Visibilities {
			if v != "public" && v != "trusted" {
				errs = append(errs, fmt.Errorf("membrane.export[%d].visibilities[%d]: %q must be 'public' or 'trusted' (private is never exportable)", i, j, v))
			}
		}
	}
	for i, rule := range spec.Import {
		if err := validateEnsembleTargetSelector(rule.FromEnsembles); err != nil {
			errs = append(errs, fmt.Errorf("membrane.import[%d] (%q): %w", i, rule.Name, err))
		}
	}
	return errs
}

// validateEnsembleTargetSelector rejects selector pairs that would resolve
// to "everything in the cluster". At least one of NamespaceSelector or
// EnsembleSelector must be non-nil AND non-empty.
func validateEnsembleTargetSelector(s EnsembleTargetSelector) error {
	if isEmptySelector(s.NamespaceSelector) && isEmptySelector(s.EnsembleSelector) {
		return fmt.Errorf("namespaceSelector and ensembleSelector cannot both be empty/unset (would match every ensemble in the cluster)")
	}
	return nil
}

// isEmptySelector returns true for nil, an empty struct, or a selector
// with no matchLabels and no matchExpressions. Note: this is the
// label-selector semantics where "empty == match everything" — which is
// exactly what we want to reject for cross-namespace export/import.
func isEmptySelector(s *metav1.LabelSelector) bool {
	if s == nil {
		return true
	}
	return len(s.MatchLabels) == 0 && len(s.MatchExpressions) == 0
}
