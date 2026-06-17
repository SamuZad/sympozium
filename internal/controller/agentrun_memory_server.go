package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/go-logr/logr"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/pkg/memoryclient"
)

// failureMemoryTTLDays is the TTL applied to controller-written failure
// breadcrumbs. Failure entries help the next handful of runs learn from
// past errors but become noise long-term; 30 days keeps recent failures
// searchable without clogging the agent's private scope forever. Can be
// promoted to MemorySpec.FailureTTLDays if per-agent tuning becomes useful.
const failureMemoryTTLDays = 30

// failurePersistTimeout caps each fire-and-forget memory POST. Failure
// persistence runs inside terminal state handlers on the controller's hot
// path, so a slow memory-server must never block reconciliation.
const failurePersistTimeout = 3 * time.Second

// persistFailureMemory stores a structured failure record in the central
// memory-server so subsequent runs of the same agent can search for and
// learn from past errors. Fire-and-forget: errors are logged at v=1 and
// never propagated to the caller.
//
// No-op when the memory subsystem is disabled (r.MemoryClient == nil) or
// when the AgentRun has no AgentRef (freestanding ad-hoc runs have no home
// scope to write into).
func (r *AgentRunReconciler) persistFailureMemory(ctx context.Context, log logr.Logger, agentRun *sympoziumv1alpha1.AgentRun, reason string) {
	if r.MemoryClient == nil {
		return
	}
	if agentRun.Spec.AgentRef == "" {
		return
	}

	task := agentRun.Spec.Task
	if len(task) > 500 {
		task = task[:500] + "..."
	}

	content := fmt.Sprintf(
		"## Failed AgentRun: %s\n**Task**: %s\n**Error**: %s\n**Timestamp**: %s\n**Instance**: %s",
		agentRun.Name,
		task,
		reason,
		time.Now().UTC().Format(time.RFC3339),
		agentRun.Spec.AgentRef,
	)

	storeCtx, cancel := context.WithTimeout(ctx, failurePersistTimeout)
	defer cancel()

	_, err := r.MemoryClient.Store(storeCtx, memoryclient.StoreRequest{
		Scope:     "agent",
		AgentName: agentRun.Spec.AgentRef,
		Content:   content,
		TTLDays:   failureMemoryTTLDays,
		Tags: []string{
			"failure",
			"agent-run",
			"run:" + agentRun.Name,
		},
	})
	if err != nil {
		log.V(1).Info("failed to persist failure memory", "err", err, "agentrun", agentRun.Name)
		return
	}

	log.Info("Persisted failure memory", "agentrun", agentRun.Name)
}
