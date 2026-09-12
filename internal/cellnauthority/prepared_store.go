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
