package agentexecution

import (
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"strings"
	"testing"
)

func TestSharedCatalogueNeverFallsThroughLegacyResolution(t *testing.T) {
	for _, tools := range [][]api.CellnCatalogueToolRef{nil, {}, {{Name: "legacy", Revision: "v1"}}} {
		selection := &api.CellnCatalogueSelection{ToolRefs: tools, ClusterToolRefs: []api.ClusterCellnToolRef{{Name: "shared", Revision: "v1"}}}
		for _, model := range []string{"", "model"} {
			result, err := Resolve(nil, Input{Backend: "celln", CellnSelection: selection, Model: model})
			if err == nil || !strings.Contains(err.Error(), "AUTH_PROTOCOL_UNSUPPORTED") || result.Backend != "" {
				t.Fatalf("shared scope entered legacy/default-model path: %+v %v", result, err)
			}
		}
	}
}

// Platform runs select the shared catalogue with the namespace's model
// connection; that is the only shape allowed through, enduring or one-shot,
// and it keeps the explicit (empty) legacy tool list and the connection
// rather than a provider.
func TestEnduringPlatformSelectionIsAcceptedOnlyInItsExactShape(t *testing.T) {
	shared := []api.ClusterCellnToolRef{{Name: "celln-trial-workspace-read", Revision: "v1"}}
	enduring := &api.EnduringRunSpec{LeaseSeconds: 600, MaxTurns: 8, MaxModelRequests: 24, MaxOutputTokens: 8192}
	ok := Input{Backend: "celln", ExecutionLifecycle: "enduring", Enduring: enduring, Model: "qwen.gguf", ModelConnectionRef: "celln-native", CellnSelection: &api.CellnCatalogueSelection{RuntimeRef: "celln-native", ToolRefs: []api.CellnCatalogueToolRef{}, ClusterToolRefs: shared}}
	result, err := Resolve(nil, ok)
	if err != nil || result.Backend != "celln" || result.ModelConnectionRef != "celln-native" || result.Provider != "" || len(result.CellnSelection.ClusterToolRefs) != 1 {
		t.Fatalf("enduring platform selection refused or altered: %+v %v", result, err)
	}
	oneShot := ok
	oneShot.ExecutionLifecycle, oneShot.Enduring = "", nil
	if result, err := Resolve(nil, oneShot); err != nil || result.ExecutionLifecycle != "" || result.Enduring != nil || result.ModelConnectionRef != "celln-native" || len(result.CellnSelection.ClusterToolRefs) != 1 {
		t.Fatalf("one-shot platform selection refused or altered: %+v %v", result, err)
	}
	for name, mutate := range map[string]func(*Input){
		"mixed legacy tools": func(in *Input) {
			in.CellnSelection.ToolRefs = []api.CellnCatalogueToolRef{{Name: "legacy", Revision: "v1"}}
		},
		"no connection":   func(in *Input) { in.ModelConnectionRef = "" },
		"inline provider": func(in *Input) { in.Provider = "openai" },
	} {
		in := ok
		selection := *ok.CellnSelection
		in.CellnSelection = &selection
		mutate(&in)
		if _, err := Resolve(nil, in); err == nil || !strings.Contains(err.Error(), "AUTH_PROTOCOL_UNSUPPORTED") {
			t.Fatalf("%s accepted: %v", name, err)
		}
	}
}
