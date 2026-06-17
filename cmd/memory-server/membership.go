package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

// agentSALabelEnsemble matches what the ensemble controller stamps on every
// generated Agent.
const agentSALabelEnsemble = "sympozium.ai/ensemble"

// agentServiceAccountSuffix mirrors AgentServiceAccountName() in
// internal/controller/agent_controller.go. Kept here rather than imported
// to keep this binary's link graph small.
const agentServiceAccountSuffix = "-agent"

// membership describes everything the request handlers need to know about
// a caller's relationship to scopes and trust peers.
type membership struct {
	AgentName    string   // empty if the SA does not identify an Agent
	EnsembleName string   // empty if the Agent is not part of an ensemble
	TrustPeers   []string // sibling agent names in the same ensemble
	// ReachablePeers lists peer Ensembles (cluster-wide) whose entries the
	// caller may read via the membrane Export/Import two-sided opt-in,
	// each annotated with the per-rule-pair access clauses that gate row
	// visibility. Populated when the caller's ensemble has Membrane.Import
	// rules and at least one matching peer has a reciprocal Membrane.Export.
	ReachablePeers []reachablePeer
}

// membershipResolver answers membership questions by reading Agent/Ensemble
// CRDs from the cluster. Results are LRU-cached with a TTL because
// memberships change rarely compared to memory-server traffic.
type membershipResolver struct {
	client ctrlclient.Client
	cache  *lru.Cache[string, cachedMembership]
	ttl    time.Duration
}

type cachedMembership struct {
	m         membership
	expiresAt time.Time
}

func newScheme() (*runtime.Scheme, error) {
	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(sympoziumv1alpha1.AddToScheme(scheme))
	return scheme, nil
}

func newCtrlClient() (ctrlclient.Client, error) {
	cfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("in-cluster config: %w", err)
	}
	scheme, err := newScheme()
	if err != nil {
		return nil, err
	}
	return ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
}

func newMembershipResolver(c ctrlclient.Client, ttl time.Duration, size int) (*membershipResolver, error) {
	if size <= 0 {
		size = 1024
	}
	cache, err := lru.New[string, cachedMembership](size)
	if err != nil {
		return nil, err
	}
	return &membershipResolver{client: c, cache: cache, ttl: ttl}, nil
}

// Resolve returns the membership for the given identity. Only successful
// lookups are cached; errors (missing Agent CR, transient apiserver
// failures, unknown caller) are retried on every call so a recovering
// Agent becomes reachable as soon as it exists, instead of being blocked
// for the full positive TTL.
func (r *membershipResolver) Resolve(ctx context.Context, id identity) (membership, error) {
	key := id.String()
	if v, ok := r.cache.Get(key); ok && time.Now().Before(v.expiresAt) {
		return v.m, nil
	}
	m, err := r.lookup(ctx, id)
	if err == nil {
		r.cache.Add(key, cachedMembership{m: m, expiresAt: time.Now().Add(r.ttl)})
	}
	return m, err
}

func (r *membershipResolver) lookup(ctx context.Context, id identity) (membership, error) {
	// Convention: an Agent named X owns ServiceAccount "X-agent" in the
	// same namespace. We trust the convention for the first hop, then
	// confirm by reading the Agent and checking the SA owner reference.
	if !strings.HasSuffix(id.ServiceAccountName, agentServiceAccountSuffix) {
		return membership{}, fmt.Errorf("service account %q is not a sympozium agent identity", id)
	}
	agentName := strings.TrimSuffix(id.ServiceAccountName, agentServiceAccountSuffix)
	if agentName == "" {
		return membership{}, fmt.Errorf("malformed agent service account %q", id)
	}

	var agent sympoziumv1alpha1.Agent
	if err := r.client.Get(ctx, ctrlclient.ObjectKey{Namespace: id.Namespace, Name: agentName}, &agent); err != nil {
		return membership{}, fmt.Errorf("get agent %s/%s: %w", id.Namespace, agentName, err)
	}

	m := membership{AgentName: agent.Name}

	if ensembleName, ok := agent.Labels[agentSALabelEnsemble]; ok && ensembleName != "" {
		m.EnsembleName = ensembleName

		var siblings sympoziumv1alpha1.AgentList
		err := r.client.List(ctx, &siblings,
			ctrlclient.InNamespace(id.Namespace),
			ctrlclient.MatchingLabels{agentSALabelEnsemble: ensembleName},
		)
		if err != nil {
			return m, fmt.Errorf("list ensemble siblings: %w", err)
		}
		peers := make([]string, 0, len(siblings.Items))
		for _, s := range siblings.Items {
			if s.Name == agent.Name {
				continue
			}
			peers = append(peers, s.Name)
		}
		m.TrustPeers = peers

		var ensemble sympoziumv1alpha1.Ensemble
		if err := r.client.Get(ctx, ctrlclient.ObjectKey{Namespace: id.Namespace, Name: ensembleName}, &ensemble); err != nil {
			// Ensemble missing or unreadable: keep what we have, skip
			// reachable-peer resolution. Don't fail the whole lookup
			// — same-ensemble reads still work.
			return m, nil
		}
		reachable, err := resolveReachablePeers(ctx, r.client, id.Namespace, &ensemble)
		if err != nil {
			return m, fmt.Errorf("resolve reachable peers: %w", err)
		}
		m.ReachablePeers = reachable
	}
	return m, nil
}

// visibilitiesFor returns the set of visibility labels the caller is
// permitted to read for a given target Agent. The owner of a private scope
// can see everything they wrote; trust peers can see public+trusted;
// strangers can see only public.
func visibilitiesFor(callerAgent, targetAgent string, trustPeers []string) []string {
	if callerAgent == targetAgent {
		return []string{"public", "trusted", "private"}
	}
	for _, p := range trustPeers {
		if p == targetAgent {
			return []string{"public", "trusted"}
		}
	}
	return []string{"public"}
}


