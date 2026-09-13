package cellnauthority

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/zeebo/blake3"
)

func TestScopedParentIncarnationMatchesNativeWireAndSeparatesNamespaces(t *testing.T) {
	scope := `["celln.scoped-parent/v1","cluster-a","namespace-uid-a"]`
	wire, err := json.Marshal([]string{"celln.parent-run-incarnation/v1", scope, "run-uid"})
	if err != nil {
		t.Fatal(err)
	}
	sum := blake3.Sum256(wire)
	want := fmt.Sprintf("blake3:%x", sum)
	got, err := ScopedParentIncarnation("cluster-a", "namespace-uid-a", "run-uid")
	if err != nil || got != want {
		t.Fatalf("incarnation=%q want=%q err=%v", got, want, err)
	}
	other, err := ScopedParentIncarnation("cluster-a", "namespace-uid-b", "run-uid")
	if err != nil || other == got {
		t.Fatalf("namespace recreation shared a parent incarnation: %q %q, %v", got, other, err)
	}
}
