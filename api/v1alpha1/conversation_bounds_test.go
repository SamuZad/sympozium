package v1alpha1_test

import (
	"os"
	"testing"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	extv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func crdSchema(t *testing.T, file string, path ...string) extv1.JSONSchemaProps {
	t.Helper()
	raw, err := os.ReadFile("../../config/crd/bases/" + file)
	if err != nil {
		t.Fatal(err)
	}
	var crd extv1.CustomResourceDefinition
	if err := yaml.Unmarshal(raw, &crd); err != nil {
		t.Fatal(err)
	}
	node := *crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, name := range path {
		if name == "[]" {
			node = *node.Items.Schema
			continue
		}
		next, ok := node.Properties[name]
		if !ok {
			t.Fatalf("%s: no %q in %v", file, name, path)
		}
		node = next
	}
	return node
}

// A kubebuilder marker cannot name a constant, so the generated schemas are
// held to the conversation bounds here: messages stay at 2048 bytes, answers
// take 8192 everywhere one is stored, and a runtime profile may report the
// current worker task bound.
func TestConversationBoundsMatchSchema(t *testing.T) {
	maxLength := func(file string, path ...string) int64 {
		t.Helper()
		node := crdSchema(t, file, path...)
		if node.MaxLength == nil {
			t.Fatalf("%s %v has no maxLength", file, path)
		}
		return *node.MaxLength
	}
	for _, answer := range [][]string{
		{"sympozium.ai_agentruns.yaml", "status", "cellnParent", "initialTurn", "result", "answer"},
		{"sympozium.ai_agentrunturns.yaml", "status", "execution", "result", "answer"},
		{"sympozium.ai_agentruns.yaml", "spec", "conversation", "seed", "[]", "assistant"},
	} {
		if got := maxLength(answer[0], answer[1:]...); got != api.MaxConversationAnswerBytes {
			t.Fatalf("%v: maxLength %d, want %d", answer, got, api.MaxConversationAnswerBytes)
		}
	}
	for _, message := range [][]string{
		{"sympozium.ai_agentruns.yaml", "status", "cellnParent", "initialTurn", "message"},
		{"sympozium.ai_agentrunturns.yaml", "status", "execution", "message"},
		{"sympozium.ai_agentrunturns.yaml", "spec", "message"},
		{"sympozium.ai_agentruns.yaml", "spec", "conversation", "seed", "[]", "user"},
	} {
		if got := maxLength(message[0], message[1:]...); got != api.MaxConversationMessageBytes {
			t.Fatalf("%v: maxLength %d, want %d", message, got, api.MaxConversationMessageBytes)
		}
	}
	for _, taskBytes := range [][]string{
		{"sympozium.ai_cellnruntimeprofiles.yaml", "spec", "limits", "taskBytes"},
		{"sympozium.ai_agentruntimes.yaml", "spec", "cellnLimits", "taskBytes"},
	} {
		node := crdSchema(t, taskBytes[0], taskBytes[1:]...)
		if node.Maximum == nil || int64(*node.Maximum) != api.MaxWorkerTaskBytes {
			t.Fatalf("%v: maximum %v, want %d", taskBytes, node.Maximum, api.MaxWorkerTaskBytes)
		}
	}
	// Lifetime totals keep their stored-object-compatible range: no minimum
	// was raised to the per-turn allowance.
	for field, maximum := range map[string]float64{"maxModelRequests": 6144, "maxOutputTokens": 3145728} {
		node := crdSchema(t, "sympozium.ai_agentruns.yaml", "spec", "enduring", field)
		if node.Minimum == nil || *node.Minimum != 0 || node.Maximum == nil || *node.Maximum != maximum {
			t.Fatalf("enduring.%s range changed: %v–%v", field, node.Minimum, node.Maximum)
		}
	}
}
