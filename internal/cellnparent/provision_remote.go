package cellnparent

import (
	"context"
	"fmt"

	"github.com/sympozium-ai/sympozium/internal/cellnauthority"
)

// RemoteProvisioner asks the owner that the Celln gateway binds to an
// incarnation to issue its permit and launch profile over the authenticated
// parent route, so the controller shares no authority root with any dispatcher
// and owners can form a fleet. Journal and Approvals remain controller-durable
// operator state that every replica must retain and share.
type RemoteProvisioner struct {
	Journal   string `json:"journal"`
	Approvals string `json:"approvals"`
	Target    string `json:"target"`
	TokenFile string `json:"tokenFile"`
	CAFile    string `json:"caFile,omitempty"`
}

// ProvisionAndApprove pins the owner/plan choice before contacting the owner,
// then revalidates live intent before publishing the one-use registration. A
// transport or owner failure preserves the durable choice; nothing is retried.
func (p RemoteProvisioner) ProvisionAndApprove(ctx context.Context, loader cellnauthority.Loader, intent ProvisionIntent, template HostProvisionTemplate) (RunApproval, error) {
	owner := ownerIssuer{Journal: p.Journal, Approvals: p.Approvals, Target: p.Target, TokenFile: p.TokenFile, CAFile: p.CAFile}
	return provisionAndApprove(ctx, loader, intent, template, owner, "sympozium.ai/celln-parent-remote-choice-v1", p, p.issue)
}

// The owner binds the plan to the principal it authenticates from the gateway's
// parent credential; BuildHostProvisionPlan already required the template's
// callers to equal that principal, so it is not sent separately.
func (p RemoteProvisioner) issue(ctx context.Context, plan []byte, _, expected string) (localProvisionResult, error) {
	transport, err := newOwnerClient(p.Target, p.TokenFile, p.CAFile)
	if err != nil {
		return localProvisionResult{}, err
	}
	defer transport.Close()
	launch, err := transport.Provision(ctx, plan, expected)
	if err != nil {
		return localProvisionResult{}, fmt.Errorf("remote parent issuer failed; preserve issuance state: %w", err)
	}
	return localProvisionResult{APIVersion: "celln.parent-provisioned/v1", LaunchProfile: launch, Incarnation: expected}, nil
}
