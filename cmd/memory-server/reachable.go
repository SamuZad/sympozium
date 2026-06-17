package main

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

// ensembleRef identifies an Ensemble cluster-wide.
type ensembleRef struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
}

// reachablePeer is one peer ensemble the caller may read from, together
// with the access clauses derived from the matching (import, export) rule
// pairs. A row in the peer ensemble is visible to the caller iff it
// satisfies AT LEAST ONE clause (clauses combine with OR).
type reachablePeer struct {
	Ensemble ensembleRef    `json:"ensemble"`
	Clauses  []accessClause `json:"clauses,omitempty"`
}

// accessClause is the per-rule-pair filter applied to candidate peer rows.
// All non-empty fields combine with AND. Empty Tag slices mean "no tag
// filter" for that side.
type accessClause struct {
	// AllowVisibilities is the set of source-visibility tiers the peer's
	// Export rule permits. Always non-empty; "private" is stripped.
	AllowVisibilities []string `json:"allowVisibilities"`
	// ImportTagsAny: row.tags must overlap (any-of) this set if non-empty.
	ImportTagsAny []string `json:"importTagsAny,omitempty"`
	// ExportTagsAny: row.tags must overlap (any-of) this set if non-empty.
	ExportTagsAny []string `json:"exportTagsAny,omitempty"`
}

// resolveReachablePeers computes the set of peer Ensembles whose entries the
// caller is permitted to read via the cross-ensemble membrane Export/Import
// opt-in, together with the per-rule access clauses that gate which rows
// of those peers may actually flow.
//
// The check is two-sided: a peer (ns, name) is reachable iff
//
//  1. The caller's Ensemble has at least one Import rule whose FromEnsembles
//     selectors match the peer's (Namespace, Ensemble) labels, AND
//  2. The peer Ensemble has at least one Export rule whose ToEnsembles
//     selectors match the caller's (Namespace, Ensemble) labels.
//
// For every (Import, Export) pair that satisfies both directions, an
// accessClause is recorded carrying the rule pair's allowed visibilities
// and tag filters. The store layer turns those clauses into a per-peer
// SQL predicate. "private" entries are NEVER exportable.
//
// The result is deduplicated by (ns, name) and stable across rule order.
// An empty membrane on either side yields no reachable peers.
func resolveReachablePeers(
	ctx context.Context,
	c ctrlclient.Client,
	callerNS string,
	callerEnsemble *sympoziumv1alpha1.Ensemble,
) ([]reachablePeer, error) {
	if callerEnsemble == nil || callerEnsemble.Spec.SharedMemory == nil || callerEnsemble.Spec.SharedMemory.Membrane == nil || len(callerEnsemble.Spec.SharedMemory.Membrane.Import) == 0 {
		return nil, nil
	}

	var callerNamespace corev1.Namespace
	if err := c.Get(ctx, ctrlclient.ObjectKey{Name: callerNS}, &callerNamespace); err != nil {
		if apierrors.IsNotFound(err) {
			// Shouldn't happen in practice but don't fail closed for missing
			// metadata — just treat as "no labels", which makes us match
			// only selectors that target a name-equality.
			callerNamespace = corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: callerNS}}
		} else {
			return nil, fmt.Errorf("get caller namespace %q: %w", callerNS, err)
		}
	}
	callerNSLabels := labels.Set(callerNamespace.Labels)
	callerEnsembleLabels := labels.Set(callerEnsemble.Labels)

	seen := make(map[ensembleRef]int) // index into out
	var out []reachablePeer

	for _, importRule := range callerEnsemble.Spec.SharedMemory.Membrane.Import {
		nsSel, ensSel, err := compileTarget(importRule.FromEnsembles)
		if err != nil {
			return nil, fmt.Errorf("import rule %q: %w", importRule.Name, err)
		}
		if nsSel == nil || ensSel == nil {
			// Admission-rejected shape; skip defensively.
			continue
		}

		var nsList corev1.NamespaceList
		if err := c.List(ctx, &nsList, &ctrlclient.ListOptions{LabelSelector: nsSel}); err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}
		for _, ns := range nsList.Items {
			var ensList sympoziumv1alpha1.EnsembleList
			if err := c.List(ctx, &ensList,
				ctrlclient.InNamespace(ns.Name),
				&ctrlclient.ListOptions{LabelSelector: ensSel},
			); err != nil {
				return nil, fmt.Errorf("list ensembles in %q: %w", ns.Name, err)
			}
			for i := range ensList.Items {
				peer := &ensList.Items[i]
				if peer.Namespace == callerNS && peer.Name == callerEnsemble.Name {
					continue
				}
				matchingExports := peerExportRulesMatching(peer, callerNSLabels, callerEnsembleLabels)
				if len(matchingExports) == 0 {
					continue
				}

				ref := ensembleRef{Namespace: peer.Namespace, Name: peer.Name}
				idx, ok := seen[ref]
				if !ok {
					out = append(out, reachablePeer{Ensemble: ref})
					idx = len(out) - 1
					seen[ref] = idx
				}
				for _, exp := range matchingExports {
					out[idx].Clauses = append(out[idx].Clauses, accessClause{
						AllowVisibilities: exportableVisibilities(exp.Visibilities),
						ImportTagsAny:     copyStrings(importRule.Tags),
						ExportTagsAny:     copyStrings(exp.Tags),
					})
				}
			}
		}
	}
	return out, nil
}

// peerExportRulesMatching returns the subset of peer.Membrane.Export whose
// ToEnsembles selectors admit the caller (by namespace + ensemble labels).
func peerExportRulesMatching(
	peer *sympoziumv1alpha1.Ensemble,
	callerNSLabels, callerEnsembleLabels labels.Set,
) []sympoziumv1alpha1.MembraneExportRule {
	if peer.Spec.SharedMemory == nil || peer.Spec.SharedMemory.Membrane == nil {
		return nil
	}
	var matched []sympoziumv1alpha1.MembraneExportRule
	for _, exportRule := range peer.Spec.SharedMemory.Membrane.Export {
		nsSel, ensSel, err := compileTarget(exportRule.ToEnsembles)
		if err != nil || nsSel == nil || ensSel == nil {
			continue
		}
		if !nsSel.Matches(callerNSLabels) {
			continue
		}
		if !ensSel.Matches(callerEnsembleLabels) {
			continue
		}
		matched = append(matched, exportRule)
	}
	return matched
}

// exportableVisibilities applies the spec default (["public"]) and strips
// "private" defensively — the CRD enum already forbids it, but the schema
// is enforced at admission, not at read time.
func exportableVisibilities(v []string) []string {
	if len(v) == 0 {
		return []string{"public"}
	}
	out := make([]string, 0, len(v))
	for _, s := range v {
		if s == "private" {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return []string{"public"}
	}
	return out
}

func copyStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := make([]string, len(in))
	copy(out, in)
	return out
}

// compileTarget converts an EnsembleTargetSelector into a pair of
// labels.Selector instances (namespace + ensemble). Returns (nil, nil, nil)
// if either selector is missing — admission rejects this shape, but we
// guard at runtime too.
func compileTarget(t sympoziumv1alpha1.EnsembleTargetSelector) (labels.Selector, labels.Selector, error) {
	if t.NamespaceSelector == nil || t.EnsembleSelector == nil {
		return nil, nil, nil
	}
	nsSel, err := metav1.LabelSelectorAsSelector(t.NamespaceSelector)
	if err != nil {
		return nil, nil, fmt.Errorf("namespaceSelector: %w", err)
	}
	ensSel, err := metav1.LabelSelectorAsSelector(t.EnsembleSelector)
	if err != nil {
		return nil, nil, fmt.Errorf("ensembleSelector: %w", err)
	}
	return nsSel, ensSel, nil
}
