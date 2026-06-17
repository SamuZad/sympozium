package v1alpha1

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestValidateMembrane(t *testing.T) {
	tests := []struct {
		name      string
		spec      *MembraneSpec
		wantErrs  int
		wantMatch string // substring expected in joined errors (empty = don't check)
	}{
		{
			name:     "nil spec",
			spec:     nil,
			wantErrs: 0,
		},
		{
			name:     "empty spec",
			spec:     &MembraneSpec{},
			wantErrs: 0,
		},
		{
			name: "export with both selectors empty",
			spec: &MembraneSpec{
				Export: []MembraneExportRule{{
					Name: "bad",
					ToEnsembles: EnsembleTargetSelector{
						NamespaceSelector: &metav1.LabelSelector{},
						EnsembleSelector:  &metav1.LabelSelector{},
					},
				}},
			},
			wantErrs:  1,
			wantMatch: `export[0] ("bad")`,
		},
		{
			name: "export with both selectors nil",
			spec: &MembraneSpec{
				Export: []MembraneExportRule{{Name: "also-bad"}},
			},
			wantErrs:  1,
			wantMatch: "match every ensemble in the cluster",
		},
		{
			name: "export with namespace selector only — valid",
			spec: &MembraneSpec{
				Export: []MembraneExportRule{{
					Name: "prod-to-staging",
					ToEnsembles: EnsembleTargetSelector{
						NamespaceSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"env": "staging"},
						},
					},
				}},
			},
			wantErrs: 0,
		},
		{
			name: "export with ensemble selector only — valid",
			spec: &MembraneSpec{
				Export: []MembraneExportRule{{
					ToEnsembles: EnsembleTargetSelector{
						EnsembleSelector: &metav1.LabelSelector{
							MatchLabels: map[string]string{"team": "platform"},
						},
					},
				}},
			},
			wantErrs: 0,
		},
		{
			name: "export with private visibility — rejected",
			spec: &MembraneSpec{
				Export: []MembraneExportRule{{
					Name:         "leak",
					Visibilities: []string{"public", "private"},
					ToEnsembles: EnsembleTargetSelector{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"k": "v"}},
					},
				}},
			},
			wantErrs:  1,
			wantMatch: "private is never exportable",
		},
		{
			name: "import with empty selector — rejected",
			spec: &MembraneSpec{
				Import: []MembraneImportRule{{Name: "trust-all"}},
			},
			wantErrs:  1,
			wantMatch: "import[0]",
		},
		{
			name: "import with namespace+ensemble selectors — valid",
			spec: &MembraneSpec{
				Import: []MembraneImportRule{{
					FromEnsembles: EnsembleTargetSelector{
						NamespaceSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"trust": "high"}},
						EnsembleSelector:  &metav1.LabelSelector{MatchLabels: map[string]string{"team": "sre"}},
					},
				}},
			},
			wantErrs: 0,
		},
		{
			name: "multiple errors accumulate",
			spec: &MembraneSpec{
				Export: []MembraneExportRule{
					{Name: "e1"},                              // empty selector
					{Name: "e2", Visibilities: []string{"x"}}, // bad visibility + empty selector
				},
				Import: []MembraneImportRule{{Name: "i1"}}, // empty selector
			},
			wantErrs: 4,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			errs := ValidateMembrane(tt.spec)
			if len(errs) != tt.wantErrs {
				t.Fatalf("ValidateMembrane() got %d errors, want %d: %v", len(errs), tt.wantErrs, errs)
			}
			if tt.wantMatch != "" {
				var joined strings.Builder
				for _, e := range errs {
					joined.WriteString(e.Error())
					joined.WriteString("\n")
				}
				if !strings.Contains(joined.String(), tt.wantMatch) {
					t.Errorf("expected substring %q in errors, got:\n%s", tt.wantMatch, joined.String())
				}
			}
		})
	}
}
