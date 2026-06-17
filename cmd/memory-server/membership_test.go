package main

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

// helper for building a baseline caller setup.
type membraneCase struct {
	callerNS        string
	callerEnsemble  string
	callerNSLabels  map[string]string
	callerEnsLabels map[string]string
	importRules     []sympoziumv1alpha1.MembraneImportRule
	peers           []*sympoziumv1alpha1.Ensemble
	extraNamespaces []*corev1.Namespace
}

func buildCase(t *testing.T, c membraneCase) ([]reachablePeer, error) {
	t.Helper()
	scheme, err := newScheme()
	if err != nil {
		t.Fatalf("scheme: %v", err)
	}
	callerNS := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: c.callerNS, Labels: c.callerNSLabels},
	}
	caller := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{
			Name:      c.callerEnsemble,
			Namespace: c.callerNS,
			Labels:    c.callerEnsLabels,
		},
		Spec: sympoziumv1alpha1.EnsembleSpec{
			SharedMemory: &sympoziumv1alpha1.SharedMemorySpec{
				Membrane: &sympoziumv1alpha1.MembraneSpec{Import: c.importRules},
			},
		},
	}

	builder := fake.NewClientBuilder().WithScheme(scheme).WithObjects(callerNS, caller)
	for _, ns := range c.extraNamespaces {
		builder = builder.WithObjects(ns)
	}
	for _, p := range c.peers {
		builder = builder.WithObjects(p)
	}
	client := builder.Build()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return resolveReachablePeers(ctx, client, c.callerNS, caller)
}

func TestResolveReachablePeers_NoMembrane(t *testing.T) {
	scheme, _ := newScheme()
	callerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a"}}
	caller := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "ops", Namespace: "team-a"},
	}
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(callerNS, caller).Build()

	got, err := resolveReachablePeers(context.Background(), client, "team-a", caller)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no reachable peers, got %v", got)
	}
}

func TestResolveReachablePeers_ImportButPeerDoesNotExport(t *testing.T) {
	peer := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "team-b", Labels: map[string]string{"tier": "prod"}},
		Spec:       sympoziumv1alpha1.EnsembleSpec{}, // no membrane, no export
	}
	peerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"env": "prod"}}}

	got, err := buildCase(t, membraneCase{
		callerNS:        "team-a",
		callerEnsemble:  "ops",
		callerNSLabels:  map[string]string{"env": "prod"},
		callerEnsLabels: map[string]string{"tier": "prod"},
		importRules: []sympoziumv1alpha1.MembraneImportRule{{
			Name: "ingest-from-sec",
			FromEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
				EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		}},
		peers:           []*sympoziumv1alpha1.Ensemble{peer},
		extraNamespaces: []*corev1.Namespace{peerNS},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no reachable peers (peer does not export), got %v", got)
	}
}

func TestResolveReachablePeers_TwoSidedMatch(t *testing.T) {
	peer := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "team-b", Labels: map[string]string{"tier": "prod"}},
		Spec: sympoziumv1alpha1.EnsembleSpec{
			SharedMemory: &sympoziumv1alpha1.SharedMemorySpec{
				Membrane: &sympoziumv1alpha1.MembraneSpec{
					Export: []sympoziumv1alpha1.MembraneExportRule{{
						Name: "share-with-ops",
						ToEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
							EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
						},
					}},
				},
			},
		},
	}
	peerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"env": "prod"}}}

	got, err := buildCase(t, membraneCase{
		callerNS:        "team-a",
		callerEnsemble:  "ops",
		callerNSLabels:  map[string]string{"env": "prod"},
		callerEnsLabels: map[string]string{"tier": "prod"},
		importRules: []sympoziumv1alpha1.MembraneImportRule{{
			Name: "ingest-from-sec",
			FromEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
				EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		}},
		peers:           []*sympoziumv1alpha1.Ensemble{peer},
		extraNamespaces: []*corev1.Namespace{peerNS},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Ensemble != (ensembleRef{Namespace: "team-b", Name: "sec"}) {
		t.Fatalf("expected single reachable peer team-b/sec, got %v", got)
	}
	if len(got[0].Clauses) != 1 {
		t.Fatalf("expected single clause for default visibility, got %d: %#v", len(got[0].Clauses), got[0].Clauses)
	}
	cl := got[0].Clauses[0]
	if len(cl.AllowVisibilities) != 1 || cl.AllowVisibilities[0] != "public" {
		t.Fatalf("expected default AllowVisibilities=[public], got %v", cl.AllowVisibilities)
	}
	if len(cl.ImportTagsAny) != 0 || len(cl.ExportTagsAny) != 0 {
		t.Fatalf("expected no tag filters, got import=%v export=%v", cl.ImportTagsAny, cl.ExportTagsAny)
	}
}

func TestResolveReachablePeers_ExcludesSelfAndDedupes(t *testing.T) {
	// peer "sec" matches via two different import rules; should appear once.
	// caller's own ensemble must never be reported.
	peer := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "team-b", Labels: map[string]string{"tier": "prod", "kind": "platform"}},
		Spec: sympoziumv1alpha1.EnsembleSpec{
			SharedMemory: &sympoziumv1alpha1.SharedMemorySpec{
				Membrane: &sympoziumv1alpha1.MembraneSpec{
					Export: []sympoziumv1alpha1.MembraneExportRule{{
						Name: "open",
						ToEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
							EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
						},
					}},
				},
			},
		},
	}
	peerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"env": "prod"}}}

	got, err := buildCase(t, membraneCase{
		callerNS:        "team-a",
		callerEnsemble:  "ops",
		callerNSLabels:  map[string]string{"env": "prod"},
		callerEnsLabels: map[string]string{"tier": "prod"},
		importRules: []sympoziumv1alpha1.MembraneImportRule{
			{
				Name: "rule-1",
				FromEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
					EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
				},
			},
			{
				Name: "rule-2",
				FromEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
					NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
					EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"kind": "platform"}},
				},
			},
		},
		peers:           []*sympoziumv1alpha1.Ensemble{peer},
		extraNamespaces: []*corev1.Namespace{peerNS},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Ensemble != (ensembleRef{Namespace: "team-b", Name: "sec"}) {
		t.Fatalf("expected exactly one deduped reachable peer, got %v", got)
	}
	if len(got[0].Clauses) != 2 {
		t.Fatalf("expected two clauses (one per matching import rule), got %d: %#v", len(got[0].Clauses), got[0].Clauses)
	}
}

func TestResolveReachablePeers_NamespaceSelectorRejects(t *testing.T) {
	// peer exports but its namespace lacks the required label.
	peer := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "team-b", Labels: map[string]string{"tier": "prod"}},
		Spec: sympoziumv1alpha1.EnsembleSpec{
			SharedMemory: &sympoziumv1alpha1.SharedMemorySpec{
				Membrane: &sympoziumv1alpha1.MembraneSpec{
					Export: []sympoziumv1alpha1.MembraneExportRule{{
						ToEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
							EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
						},
					}},
				},
			},
		},
	}
	// peer's namespace lacks env=prod, so the caller's import rule won't list it.
	peerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"env": "dev"}}}

	got, err := buildCase(t, membraneCase{
		callerNS:        "team-a",
		callerEnsemble:  "ops",
		callerNSLabels:  map[string]string{"env": "prod"},
		callerEnsLabels: map[string]string{"tier": "prod"},
		importRules: []sympoziumv1alpha1.MembraneImportRule{{
			FromEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
				EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		}},
		peers:           []*sympoziumv1alpha1.Ensemble{peer},
		extraNamespaces: []*corev1.Namespace{peerNS},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected no reachable peers (peer ns label mismatch), got %v", got)
	}
}

func TestResolveReachablePeers_TagAndVisibilityFilters(t *testing.T) {
	// Caller imports rows tagged "findings"; peer exports rows tagged
	// "data" at visibilities {"public","trusted"}. The resulting clause
	// must surface both tag any-of filters and the explicit visibility set.
	peer := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "team-b", Labels: map[string]string{"tier": "prod"}},
		Spec: sympoziumv1alpha1.EnsembleSpec{
			SharedMemory: &sympoziumv1alpha1.SharedMemorySpec{
				Membrane: &sympoziumv1alpha1.MembraneSpec{
					Export: []sympoziumv1alpha1.MembraneExportRule{{
						Name:         "share-data",
						Visibilities: []string{"public", "trusted"},
						Tags:         []string{"data"},
						ToEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
							EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
						},
					}},
				},
			},
		},
	}
	peerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"env": "prod"}}}

	got, err := buildCase(t, membraneCase{
		callerNS:        "team-a",
		callerEnsemble:  "ops",
		callerNSLabels:  map[string]string{"env": "prod"},
		callerEnsLabels: map[string]string{"tier": "prod"},
		importRules: []sympoziumv1alpha1.MembraneImportRule{{
			Name: "ingest-findings",
			Tags: []string{"findings"},
			FromEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
				EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		}},
		peers:           []*sympoziumv1alpha1.Ensemble{peer},
		extraNamespaces: []*corev1.Namespace{peerNS},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || got[0].Ensemble != (ensembleRef{Namespace: "team-b", Name: "sec"}) {
		t.Fatalf("expected single reachable peer team-b/sec, got %v", got)
	}
	if len(got[0].Clauses) != 1 {
		t.Fatalf("expected single clause, got %d: %#v", len(got[0].Clauses), got[0].Clauses)
	}
	cl := got[0].Clauses[0]
	if len(cl.AllowVisibilities) != 2 || cl.AllowVisibilities[0] != "public" || cl.AllowVisibilities[1] != "trusted" {
		t.Fatalf("expected AllowVisibilities=[public trusted], got %v", cl.AllowVisibilities)
	}
	if len(cl.ImportTagsAny) != 1 || cl.ImportTagsAny[0] != "findings" {
		t.Fatalf("expected ImportTagsAny=[findings], got %v", cl.ImportTagsAny)
	}
	if len(cl.ExportTagsAny) != 1 || cl.ExportTagsAny[0] != "data" {
		t.Fatalf("expected ExportTagsAny=[data], got %v", cl.ExportTagsAny)
	}
}

func TestResolveReachablePeers_PrivateVisibilityStripped(t *testing.T) {
	// CRD enum already forbids "private" in Export rules, but the
	// resolver should still defensively strip it if present.
	peer := &sympoziumv1alpha1.Ensemble{
		ObjectMeta: metav1.ObjectMeta{Name: "sec", Namespace: "team-b", Labels: map[string]string{"tier": "prod"}},
		Spec: sympoziumv1alpha1.EnsembleSpec{
			SharedMemory: &sympoziumv1alpha1.SharedMemorySpec{
				Membrane: &sympoziumv1alpha1.MembraneSpec{
					Export: []sympoziumv1alpha1.MembraneExportRule{{
						Visibilities: []string{"public", "private", "trusted"},
						ToEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
							NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
							EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
						},
					}},
				},
			},
		},
	}
	peerNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-b", Labels: map[string]string{"env": "prod"}}}

	got, err := buildCase(t, membraneCase{
		callerNS:        "team-a",
		callerEnsemble:  "ops",
		callerNSLabels:  map[string]string{"env": "prod"},
		callerEnsLabels: map[string]string{"tier": "prod"},
		importRules: []sympoziumv1alpha1.MembraneImportRule{{
			FromEnsembles: sympoziumv1alpha1.EnsembleTargetSelector{
				NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"env": "prod"}},
				EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"tier": "prod"}},
			},
		}},
		peers:           []*sympoziumv1alpha1.Ensemble{peer},
		extraNamespaces: []*corev1.Namespace{peerNS},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 1 || len(got[0].Clauses) != 1 {
		t.Fatalf("expected single peer with single clause, got %#v", got)
	}
	for _, v := range got[0].Clauses[0].AllowVisibilities {
		if v == "private" {
			t.Fatalf("expected 'private' to be stripped, got %v", got[0].Clauses[0].AllowVisibilities)
		}
	}
}
