package cellnauthority

import (
	"fmt"
	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

// ValidatePreparedBindings checks public material against the frozen decision.
// It does not replace live policy revalidation before initial admission, native
// publisher/closure validation, or credential verification at the receiver.
func ValidatePreparedBindings(op PreparedOperation) error {
	m := op.Resolution.Execution
	if m == nil {
		return fmt.Errorf("missing prepared execution material")
	}
	d := op.Resolution.Decision
	s := m.Source
	if s.ClusterID != d.ClusterID || s.Namespace != d.Run.Namespace || s.NamespaceUID != d.Run.NamespaceUID || s.RunName != d.Run.Name || s.RunUID != d.Run.UID || s.RunSpecSHA256 != d.Run.SpecSHA256 {
		return fmt.Errorf("prepared source identity mismatch")
	}
	profileDigest, err := digestJSON(m.ProfileSpec)
	if err != nil {
		return err
	}
	runtimeDigest, err := digestJSON(struct {
		WrapperSpec   api.AgentRuntimeSpec `json:"wrapperSpec"`
		ProfileName   string               `json:"profileName"`
		ProfileUID    string               `json:"profileUid"`
		ProfileDigest string               `json:"profileDigest"`
	}{m.WrapperSpec, m.ProfileName, m.ProfileUID, profileDigest})
	if err != nil {
		return err
	}
	if runtimeDigest != d.Runtime.SpecSHA256 || m.ProfileSpec.Revision != d.Runtime.Revision || len(m.Tools) != len(d.Tools) {
		return fmt.Errorf("prepared catalogue identity mismatch")
	}
	for i, tool := range m.Tools {
		binding := d.Tools[i]
		if tool.Name != binding.Name || tool.Spec.Revision != binding.Revision || tool.Spec.Executable.Hash != binding.Hash {
			return fmt.Errorf("prepared ordered tool identity mismatch")
		}
	}
	payload := map[string]any{"apiVersion": "celln.sympozium.ai/execution-request-v1", "operation": d.Operation, "payload": m.Payload, "runUid": s.RunUID}
	if d.Parent != nil {
		payload["parentIncarnation"] = d.Parent.Incarnation
		if d.Parent.TurnID != nil {
			payload["turnId"] = *d.Parent.TurnID
		}
	}
	requestDigest, err := digestJSON(payload)
	if err != nil {
		return err
	}
	if requestDigest != d.RequestDigest {
		return fmt.Errorf("prepared payload identity mismatch")
	}
	if d.Parent != nil && d.Parent.TurnID != nil {
		if m.TurnUID == "" || m.TurnUID != *d.Parent.TurnID {
			return fmt.Errorf("prepared turn UID mismatch")
		}
	} else if m.TurnUID != "" {
		return fmt.Errorf("non-turn preparation carries a turn UID")
	}
	return nil
}
