package cellnauthority

import (
	"encoding/json"
	"fmt"

	"github.com/zeebo/blake3"
)

// ScopedParentIncarnation exactly matches Celln parent_permit::run_incarnation.
// The namespace UID prevents a deleted/recreated namespace from sharing a
// parent identity, while the run UID prevents name reuse within one namespace.
func ScopedParentIncarnation(clusterID, namespaceUID, runUID string) (string, error) {
	if clusterID == "" || namespaceUID == "" || runUID == "" || len(runUID) > 128 {
		return "", fmt.Errorf("cluster, namespace and run identities are required")
	}
	scopeRaw, err := json.Marshal([]string{"celln.scoped-parent/v1", clusterID, namespaceUID})
	if err != nil || len(scopeRaw) > 512 {
		return "", fmt.Errorf("scoped parent identity exceeds its bound")
	}
	wire, err := json.Marshal([]string{"celln.parent-run-incarnation/v1", string(scopeRaw), runUID})
	if err != nil {
		return "", fmt.Errorf("serialize scoped parent identity: %w", err)
	}
	sum := blake3.Sum256(wire)
	return fmt.Sprintf("blake3:%x", sum), nil
}
