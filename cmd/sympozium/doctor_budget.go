package main

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/types"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/cellninstall"
)

// checkFleetBudget compares the scope's policy ceilings with what its turns
// cost. Every turn reserves the runtime profile's whole per-turn allowance
// from the parent's lifetime totals, so totals sized for a smaller allowance
// (3 requests / 1536 tokens a turn before the current starter package) run
// out before the turn ceiling does: the conversation ends early and the turn
// count the policy states is never reached.
func (d *doctor) checkFleetBudget(ctx context.Context) doctorFinding {
	const check = "Fleet turn budget"
	publication, err := cellninstall.ReadFleetPublication(ctx, d.client)
	if err != nil {
		return failed(check, err)
	}
	if !publication.Exists || publication.Scope == "" {
		return doctorFinding{Check: check, Status: statusPass, Summary: "no fleet configuration is published; an install sizes its own ceilings"}
	}
	_, policyName, _ := cellninstall.PlatformCatalogueNames(publication.Scope)
	var policy sympoziumv1alpha1.CellnExecutionPolicy
	if err := d.client.Get(ctx, types.NamespacedName{Name: policyName}, &policy); apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
		return doctorFinding{Check: check, Status: statusPass, Summary: fmt.Sprintf("policy %s is not published yet; the install sizes its ceilings", policyName)}
	} else if err != nil {
		return failed(check, err)
	}
	// The allowance in force is the published profile's; a profile that
	// states none is held to the current starter package's.
	turnRequests, turnTokens := int64(0), int64(0)
	for _, ref := range policy.Spec.RuntimeProfiles {
		var profile sympoziumv1alpha1.CellnRuntimeProfile
		if d.client.Get(ctx, types.NamespacedName{Name: ref.Ref.Name}, &profile) != nil || profile.Spec.Native == nil {
			continue
		}
		turnRequests, turnTokens = max(turnRequests, profile.Spec.Native.TurnModelRequests), max(turnTokens, profile.Spec.Native.TurnOutputTokens)
	}
	if turnRequests < 1 || turnTokens < 1 {
		turnRequests, turnTokens = sympoziumv1alpha1.TurnModelRequests, sympoziumv1alpha1.TurnOutputTokens
	}
	c := policy.Spec.Ceilings
	afforded := cellninstall.TurnsAfforded(c.MaxModelRequests, c.MaxOutputTokens, turnRequests, turnTokens)
	sized := fmt.Sprintf("policy %s allows %d turns with %d model requests and %d output tokens; a turn reserves %d requests and %d tokens", policyName, c.MaxTurns, c.MaxModelRequests, c.MaxOutputTokens, turnRequests, turnTokens)
	if c.MaxTurns < 1 || afforded >= c.MaxTurns {
		return doctorFinding{Check: check, Status: statusPass, Summary: sized}
	}
	return doctorFinding{Check: check, Status: statusWarn,
		Summary: fmt.Sprintf("%s, so a conversation ends after %d turns (%d requests and %d tokens per allowed turn)", sized, afforded, c.MaxModelRequests/c.MaxTurns, c.MaxOutputTokens/c.MaxTurns),
		Remedy: []string{
			fmt.Sprintf("sympozium install --celln-fleet-replace-package --celln-fleet-max-turns %d --celln-fleet-max-model-requests %d --celln-fleet-max-output-tokens %d   # ceilings are set when a scope is configured for a package and rewritten only when the install moves it to another package; live parents are lost", c.MaxTurns, cellninstall.CapFleetTotal(c.MaxTurns*turnRequests, cellninstall.MaxFleetModelRequests), cellninstall.CapFleetTotal(c.MaxTurns*turnTokens, cellninstall.MaxFleetOutputTokens)),
			fmt.Sprintf("or accept %d turns per conversation; 'Restart elsewhere' continues a conversation that ran out", afforded),
		}}
}
