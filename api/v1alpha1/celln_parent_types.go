package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// CellnParentBinding freezes operator-approved routing and run intent before
// creation. It contains no credential and is not itself execution authority.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="parent binding is immutable"
type CellnParentBinding struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Target string `json:"target"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=512
	Principal string `json:"principal"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	RunUID string `json:"runUID"`
	// +kubebuilder:validation:Pattern=`^sha256:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	SpecSHA256 string `json:"specSHA256"`
	// +kubebuilder:validation:Pattern=`^blake3:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	LaunchProfile string `json:"launchProfile"`
	// +kubebuilder:validation:Pattern=`^blake3:[0-9a-f]{64}$`
	// +kubebuilder:validation:MaxLength=71
	Incarnation string `json:"incarnation"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.initialTurn) || has(self.initialTurn)",message="initial parent turn cannot be removed"
// CellnParentStatus freezes admission before creation. AdmittedAt is set
// once with the first creation attempt and can never change after that.
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.admittedAt) || (has(self.admittedAt) && self.admittedAt == oldSelf.admittedAt)",message="parent admission time is immutable"
type CellnParentStatus struct {
	Binding CellnParentBinding `json:"binding"`
	// CreateAttempted must be persisted before POST. A lost response cannot clear
	// this bit; reconciliation must inspect the same owner instead of replaying.
	// +kubebuilder:validation:XValidation:rule="!oldSelf || self",message="parent creation attempt cannot be cleared"
	CreateAttempted bool `json:"createAttempted"`
	// AdmittedAt records when parent creation was first attempted. It anchors
	// the original execution lease: later tokens, turns or reconciles cannot
	// extend it, and expiry disables new turns. Immutability is enforced by
	// the CellnParentStatus-level validation rule.
	// +optional
	AdmittedAt *metav1.Time `json:"admittedAt,omitempty"`
	// +optional
	InitialTurn *CellnParentTurnStatus `json:"initialTurn,omitempty"`
	// AcceptedTurns counts subsequent turns; the initial turn uses one additional
	// host turn. Claims consume budget even if dispatch later becomes uncertain.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=1023
	// +kubebuilder:validation:XValidation:rule="self >= oldSelf",message="accepted turn count cannot decrease"
	AcceptedTurns int32 `json:"acceptedTurns"`
	// ActiveTurn remains until that exact child record contains a committed result.
	// +optional
	ActiveTurn *CellnActiveTurn `json:"activeTurn,omitempty"`
}

type CellnActiveTurn struct {
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=128
	UID string `json:"uid"`
}

// +kubebuilder:validation:XValidation:rule="!has(oldSelf.result) || has(self.result)",message="committed turn result cannot be removed"
// +kubebuilder:validation:XValidation:rule="!has(self.result) || self.attempted",message="turn result requires an attempted submission"
type CellnParentTurnStatus struct {
	// +kubebuilder:validation:MaxLength=64
	// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_-]{1,64}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="turn ID is immutable"
	ID string `json:"id"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="turn input is immutable"
	Message string `json:"message"`
	// +kubebuilder:validation:MaxLength=71
	// +kubebuilder:validation:Pattern=`^blake3:[0-9a-f]{64}$`
	// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="turn child is immutable"
	Child string `json:"child"`
	// +kubebuilder:validation:XValidation:rule="!oldSelf || self",message="turn attempt cannot be cleared"
	Attempted bool `json:"attempted"`
	// +optional
	Result *CellnParentTurnResult `json:"result,omitempty"`
}

// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="committed turn result is immutable"
type CellnParentTurnResult struct {
	Succeeded bool `json:"succeeded"`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=2048
	Answer string `json:"answer"`
}
