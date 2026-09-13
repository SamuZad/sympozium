package cellnauthority

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const preparedVersion = "sympozium.ai/celln-prepared-operation-v1"
const preparedDataKey = "operation.json"
const finalDecisionDataKey = "decision.json"
const finalPreparationDataKey = "preparation.json"

// PreparedOperation is immutable controller-owned intent, not a credential or
// native admission receipt. Publication must precede registration or dispatch.
type PreparedOperation struct {
	APIVersion     string                 `json:"apiVersion"`
	Resolution     PlatformResolution     `json:"resolution"`
	ResolveRequest PlatformResolveRequest `json:"resolveRequest"`
}

type StoredPreparation struct {
	Name      string
	UID       types.UID
	Operation PreparedOperation
}

// FinalizedPreparation is the immutable, gateway-pinned decision paired with
// the exact protected preparation that preceded every external side effect.
type FinalizedPreparation struct {
	Name            string
	UID             types.UID
	PreparationName string
	PreparationUID  types.UID
	Decision        PlatformDecision
}

// PreparedStore must use a protected control-plane namespace, never a tenant
// namespace. Reader must be the uncached APIReader. Namespace RBAC is part of
// deployment qualification; this library cannot infer it from namespace names.
type PreparedStore struct {
	Writer    client.Client
	Reader    client.Reader
	Namespace string
	Resolver  PlatformResolver
}

func (s PreparedStore) Prepare(ctx context.Context, runKey types.NamespacedName, request PlatformResolveRequest) (*StoredPreparation, error) {
	if s.Writer == nil || s.Reader == nil || s.Namespace == "" || s.Namespace == runKey.Namespace || request.ClusterID == "" {
		return nil, fmt.Errorf("protected preparation store is unavailable")
	}
	var run api.AgentRun
	if err := s.Reader.Get(ctx, runKey, &run); err != nil {
		return nil, err
	}
	var ns corev1.Namespace
	if err := s.Reader.Get(ctx, types.NamespacedName{Name: run.Namespace}, &ns); err != nil {
		return nil, err
	}
	if run.UID == "" || ns.UID == "" {
		return nil, fmt.Errorf("persisted resource identities are required")
	}
	turnUID := ""
	if request.TurnKey != nil {
		if request.TurnKey.Namespace != run.Namespace {
			return nil, fmt.Errorf("cross-namespace turn refused")
		}
		var turn api.AgentRunTurn
		if err := s.Reader.Get(ctx, *request.TurnKey, &turn); err != nil {
			return nil, err
		}
		if turn.UID == "" {
			return nil, fmt.Errorf("persisted turn identity is required")
		}
		turnUID = string(turn.UID)
	}
	keyBytes, _ := json.Marshal([]string{preparedVersion, request.ClusterID, string(ns.UID), string(run.UID), turnUID})
	hash := sha256.Sum256(keyBytes)
	name := "celln-op-" + hex.EncodeToString(hash[:])
	frozen, err := s.Load(ctx, name)
	if err == nil {
		return validatePreparationSource(frozen, request.ClusterID, ns, run)
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	// Always use this store's uncached reader, including when callers accidentally
	// supplied a resolver backed by the controller cache.
	resolver := s.Resolver
	resolver.Reader = s.Reader
	resolved, err := resolver.Resolve(ctx, runKey, request)
	if err != nil {
		return nil, err
	}
	candidate := PreparedOperation{APIVersion: preparedVersion, Resolution: *resolved, ResolveRequest: resolved.Request}
	raw, err := json.Marshal(candidate)
	if err != nil {
		return nil, err
	}
	if len(raw) > 262144 {
		return nil, fmt.Errorf("prepared operation exceeds bound")
	}
	immutable := true
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: name, Labels: map[string]string{"sympozium.ai/celln-prepared": "true"}}, Immutable: &immutable, Data: map[string]string{preparedDataKey: string(raw)}}
	if err := s.Writer.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	// The API winner, not this reconciliation's candidate, owns the original
	// clock and allowance. A racing loser must never dispatch its local decision.
	frozen, err = s.Load(ctx, name)
	if err != nil {
		return nil, err
	}
	return validatePreparationSource(frozen, request.ClusterID, ns, run)
}

func (s PreparedStore) Load(ctx context.Context, name string) (*StoredPreparation, error) {
	if s.Reader == nil || s.Namespace == "" || len(name) != len("celln-op-")+64 || name[:len("celln-op-")] != "celln-op-" {
		return nil, fmt.Errorf("invalid preparation reference")
	}
	if _, err := hex.DecodeString(name[len("celln-op-"):]); err != nil {
		return nil, fmt.Errorf("invalid preparation reference")
	}
	var cm corev1.ConfigMap
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, &cm); err != nil {
		return nil, err
	}
	if cm.UID == "" || cm.Immutable == nil || !*cm.Immutable || cm.Labels["sympozium.ai/celln-prepared"] != "true" || len(cm.BinaryData) != 0 || len(cm.Data) != 1 {
		return nil, fmt.Errorf("invalid protected preparation")
	}
	raw := cm.Data[preparedDataKey]
	if len(raw) == 0 || len(raw) > 262144 {
		return nil, fmt.Errorf("invalid protected preparation")
	}
	var op PreparedOperation
	if err := json.Unmarshal([]byte(raw), &op); err != nil {
		return nil, fmt.Errorf("invalid protected preparation")
	}
	if op.APIVersion != preparedVersion || op.Resolution.Execution == nil {
		return nil, fmt.Errorf("unsupported protected preparation")
	}
	if err := ValidatePreparedBindings(op); err != nil {
		return nil, err
	}
	op.Resolution.Request = op.ResolveRequest
	op.Resolution.Decision.Route.CredentialSourceRef = op.Resolution.Execution.CredentialSourceRef
	return &StoredPreparation{Name: name, UID: cm.UID, Operation: op}, nil
}

// FinalName deterministically identifies the single final decision allowed for
// a preparation. It contains no tenant-controlled names or credentials.
func (s PreparedStore) FinalName(prepared *StoredPreparation) (string, error) {
	if prepared == nil || prepared.Name == "" || prepared.UID == "" {
		return "", fmt.Errorf("persisted preparation identity is required")
	}
	raw, _ := json.Marshal([]string{"sympozium.ai/celln-final-decision-v1", prepared.Name, string(prepared.UID)})
	hash := sha256.Sum256(raw)
	return "celln-final-" + hex.EncodeToString(hash[:]), nil
}

// Finalize publishes the gateway-pinned decision as a second immutable object.
// A racing or restarted reconciler must use the API winner and may never replace
// it with a decision carrying a different credential UID or authority window.
func (s PreparedStore) Finalize(ctx context.Context, prepared *StoredPreparation, decision PlatformDecision) (*FinalizedPreparation, error) {
	if s.Writer == nil || s.Reader == nil || s.Namespace == "" {
		return nil, fmt.Errorf("protected preparation store is unavailable")
	}
	name, err := s.FinalName(prepared)
	if err != nil {
		return nil, err
	}
	if existing, loadErr := s.LoadFinal(ctx, name, prepared); loadErr == nil {
		if !sameFinalDecision(existing.Decision, decision) {
			return nil, fmt.Errorf("conflicting final decision already exists")
		}
		return existing, nil
	} else if !apierrors.IsNotFound(loadErr) {
		return nil, loadErr
	}
	if err := decision.ReadyForSigning(); err != nil {
		return nil, err
	}
	if err := validateFinalDecision(prepared.Operation.Resolution.Decision, decision); err != nil {
		return nil, err
	}
	decisionRaw, err := json.Marshal(decision)
	if err != nil {
		return nil, err
	}
	bindingRaw, _ := json.Marshal(struct {
		APIVersion string    `json:"apiVersion"`
		Name       string    `json:"name"`
		UID        types.UID `json:"uid"`
	}{"sympozium.ai/celln-final-preparation-binding-v1", prepared.Name, prepared.UID})
	immutable := true
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: name, Labels: map[string]string{"sympozium.ai/celln-final": "true"}}, Immutable: &immutable, Data: map[string]string{finalDecisionDataKey: string(decisionRaw), finalPreparationDataKey: string(bindingRaw)}}
	if err := s.Writer.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
		return nil, err
	}
	stored, err := s.LoadFinal(ctx, name, prepared)
	if err != nil {
		return nil, err
	}
	if !sameFinalDecision(stored.Decision, decision) {
		return nil, fmt.Errorf("conflicting final decision won publication race")
	}
	return stored, nil
}

// LoadFinal reads a final decision through the uncached reader and verifies its
// immutable binding to the original preparation.
func (s PreparedStore) LoadFinal(ctx context.Context, name string, prepared *StoredPreparation) (*FinalizedPreparation, error) {
	if s.Reader == nil || s.Namespace == "" || prepared == nil || len(name) != len("celln-final-")+64 || name[:len("celln-final-")] != "celln-final-" {
		return nil, fmt.Errorf("invalid final preparation reference")
	}
	if _, err := hex.DecodeString(name[len("celln-final-"):]); err != nil {
		return nil, fmt.Errorf("invalid final preparation reference")
	}
	expectedName, err := s.FinalName(prepared)
	if err != nil || name != expectedName {
		return nil, fmt.Errorf("final decision name does not match its preparation")
	}
	var cm corev1.ConfigMap
	if err := s.Reader.Get(ctx, types.NamespacedName{Namespace: s.Namespace, Name: name}, &cm); err != nil {
		return nil, err
	}
	if cm.UID == "" || cm.Immutable == nil || !*cm.Immutable || cm.Labels["sympozium.ai/celln-final"] != "true" || len(cm.BinaryData) != 0 || len(cm.Data) != 2 {
		return nil, fmt.Errorf("invalid protected final decision")
	}
	var binding struct {
		APIVersion string    `json:"apiVersion"`
		Name       string    `json:"name"`
		UID        types.UID `json:"uid"`
	}
	if err := json.Unmarshal([]byte(cm.Data[finalPreparationDataKey]), &binding); err != nil || binding.APIVersion != "sympozium.ai/celln-final-preparation-binding-v1" || binding.Name != prepared.Name || binding.UID != prepared.UID {
		return nil, fmt.Errorf("final decision preparation binding mismatch")
	}
	var decision PlatformDecision
	if err := json.Unmarshal([]byte(cm.Data[finalDecisionDataKey]), &decision); err != nil {
		return nil, fmt.Errorf("invalid protected final decision")
	}
	if err := decision.ReadyForSigning(); err != nil {
		return nil, err
	}
	if err := validateFinalDecision(prepared.Operation.Resolution.Decision, decision); err != nil {
		return nil, err
	}
	return &FinalizedPreparation{Name: name, UID: cm.UID, PreparationName: prepared.Name, PreparationUID: prepared.UID, Decision: decision}, nil
}

func sameFinalDecision(a, b PlatformDecision) bool {
	aRaw, aErr := a.Canonical()
	bRaw, bErr := b.Canonical()
	return aErr == nil && bErr == nil && string(aRaw) == string(bRaw)
}

func validateFinalDecision(base, final PlatformDecision) error {
	if err := final.ReadyForSigning(); err != nil {
		return err
	}
	expected, err := base.FinalizeCredentialSource(func() string {
		if final.Route.CredentialSource != nil {
			return final.Route.CredentialSource.SecretUID
		}
		return ""
	}())
	if err != nil || !sameFinalDecision(expected, final) {
		return fmt.Errorf("final decision changes authority beyond the gateway credential UID pin")
	}
	return nil
}

func validatePreparationSource(stored *StoredPreparation, cluster string, ns corev1.Namespace, run api.AgentRun) (*StoredPreparation, error) {
	source := stored.Operation.Resolution.Execution.Source
	digest, err := digestJSON(run.Spec)
	if err != nil {
		return nil, err
	}
	if source.ClusterID != cluster || source.Namespace != ns.Name || source.NamespaceUID != string(ns.UID) || source.RunUID != string(run.UID) || source.RunName != run.Name || source.RunSpecSHA256 != digest {
		return nil, deny(ReasonPolicyContracted, "prepared operation no longer matches the run identity/specification")
	}
	return stored, nil
}

// ValidateCurrent binds a loaded preparation back to the live run and namespace
// identities before status reads, results, cancellation, or cleanup.
func (s PreparedStore) ValidateCurrent(ctx context.Context, runKey types.NamespacedName, stored *StoredPreparation) error {
	if s.Reader == nil || stored == nil {
		return fmt.Errorf("protected preparation validation is unavailable")
	}
	var run api.AgentRun
	if err := s.Reader.Get(ctx, runKey, &run); err != nil {
		return err
	}
	var ns corev1.Namespace
	if err := s.Reader.Get(ctx, types.NamespacedName{Name: runKey.Namespace}, &ns); err != nil {
		return err
	}
	_, err := validatePreparationSource(stored, stored.Operation.Resolution.Execution.Source.ClusterID, ns, run)
	return err
}
