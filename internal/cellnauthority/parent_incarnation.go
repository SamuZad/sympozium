package cellnauthority

import (
	"encoding/json"
	"fmt"

	"github.com/zeebo/blake3"
)

// ScopedParentScope is the owner-side provision-plan scope for one namespace:
// the same bytes Celln hashes with the run UID, so a deleted and recreated
// namespace can never share a parent identity.
func ScopedParentScope(clusterID, namespaceUID string) (string, error) {
	if clusterID == "" || namespaceUID == "" {
		return "", fmt.Errorf("cluster, namespace and run identities are required")
	}
	scopeRaw, err := json.Marshal([]string{"celln.scoped-parent/v1", clusterID, namespaceUID})
	if err != nil || len(scopeRaw) > 512 {
		return "", fmt.Errorf("scoped parent identity exceeds its bound")
	}
	return string(scopeRaw), nil
}

// ScopedParentIncarnation exactly matches Celln parent_permit::run_incarnation.
// The namespace UID prevents a deleted/recreated namespace from sharing a
// parent identity, while the run UID prevents name reuse within one namespace.
func ScopedParentIncarnation(clusterID, namespaceUID, runUID string) (string, error) {
	if runUID == "" || len(runUID) > 128 {
		return "", fmt.Errorf("cluster, namespace and run identities are required")
	}
	scope, err := ScopedParentScope(clusterID, namespaceUID)
	if err != nil {
		return "", err
	}
	wire, err := json.Marshal([]string{"celln.parent-run-incarnation/v1", scope, runUID})
	if err != nil {
		return "", fmt.Errorf("serialize scoped parent identity: %w", err)
	}
	sum := blake3.Sum256(wire)
	return fmt.Sprintf("blake3:%x", sum), nil
}
