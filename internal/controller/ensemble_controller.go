package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/eventbus"
	"github.com/sympozium-ai/sympozium/pkg/memoryclient"
)

const ensembleFinalizer = "sympozium.ai/ensemble-finalizer"

// ensembleManagedLabels is the closed set of metadata labels that the
// Ensemble controller owns on a generated Agent. Every other label
// (e.g. Argo tracking-id, operator-added selectors) is left alone on
// updates.
var ensembleManagedLabels = []string{
	"sympozium.ai/ensemble",
	"sympozium.ai/agent-config",
	"sympozium.ai/provider",
}

// EnsembleReconciler reconciles Ensemble objects.
// It stamps out Agents, SympoziumSchedules, and memory
// ConfigMaps for each persona defined in the pack.
type EnsembleReconciler struct {
	client.Client
	Scheme       *runtime.Scheme
	Log          logr.Logger
	EventBus     eventbus.EventBus
	DensityCache *DensityCache // optional: set when llmfit DaemonSet is enabled

	// MemoryClient writes ensemble seed memories to the central memory-server.
	// Nil when the memory subsystem is disabled (no MEMORY_SERVER_URL); in
	// that case reconcileMemorySeeds is a no-op so personas still come up.
	MemoryClient *memoryclient.Client
}

// defaultObservabilitySpec builds an ObservabilitySpec from env vars injected
// by the Helm chart / kustomize, falling back to sensible defaults matching the
// built-in OTel collector's service address.
func defaultObservabilitySpec() *sympoziumv1alpha1.ObservabilitySpec {
	enabled := strings.EqualFold(os.Getenv("SYMPOZIUM_DEFAULT_OTEL_ENABLED"), "true")
	endpoint := os.Getenv("SYMPOZIUM_DEFAULT_OTEL_ENDPOINT")
	if endpoint == "" {
		endpoint = "sympozium-otel-collector.sympozium-system.svc:4317"
	}
	protocol := os.Getenv("SYMPOZIUM_DEFAULT_OTEL_PROTOCOL")
	if protocol == "" {
		protocol = "grpc"
	}
	serviceName := os.Getenv("SYMPOZIUM_DEFAULT_OTEL_SERVICE_NAME")
	if serviceName == "" {
		serviceName = "sympozium"
	}
	return &sympoziumv1alpha1.ObservabilitySpec{
		Enabled:      enabled,
		OTLPEndpoint: endpoint,
		OTLPProtocol: protocol,
		ServiceName:  serviceName,
		ResourceAttributes: map[string]string{
			"deployment.environment": "cluster",
			"k8s.cluster.name":       "unknown",
		},
	}
}

func isManagedEnsembleAuthSecret(ensembleName, secretName string, labels map[string]string) bool {
	if strings.TrimSpace(secretName) == "" {
		return false
	}
	if labels != nil && labels["sympozium.ai/ensemble"] == ensembleName {
		return true
	}
	if secretName == ensembleName+"-credentials" {
		return true
	}
	// TUI-created naming convention: <pack>-<provider>-key
	if strings.HasPrefix(secretName, ensembleName+"-") && strings.HasSuffix(secretName, "-key") {
		return true
	}
	return false
}

func (r *EnsembleReconciler) deleteManagedAuthSecrets(ctx context.Context, pack *sympoziumv1alpha1.Ensemble) (int, error) {
	if pack == nil {
		return 0, nil
	}
	seen := make(map[string]struct{}, len(pack.Spec.AuthRefs))
	deleted := 0
	for _, ref := range pack.Spec.AuthRefs {
		name := strings.TrimSpace(ref.Secret)
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		sec := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Name: name, Namespace: pack.Namespace}, sec); err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return deleted, err
		}
		if !isManagedEnsembleAuthSecret(pack.Name, name, sec.Labels) {
			continue
		}
		if err := r.Delete(ctx, sec); err != nil && !errors.IsNotFound(err) {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// +kubebuilder:rbac:groups=sympozium.ai,resources=ensembles,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sympozium.ai,resources=ensembles/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sympozium.ai,resources=ensembles/finalizers,verbs=update

// Reconcile handles Ensemble create/update/delete events.
func (r *EnsembleReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("ensemble", req.NamespacedName)

	pack := &sympoziumv1alpha1.Ensemble{}
	if err := r.Get(ctx, req.NamespacedName, pack); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !pack.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, log, pack)
	}

	// Add finalizer
	if !controllerutil.ContainsFinalizer(pack, ensembleFinalizer) {
		controllerutil.AddFinalizer(pack, ensembleFinalizer)
		if err := r.Update(ctx, pack); err != nil {
			return ctrl.Result{}, err
		}
	}

	// If the pack is not enabled, clean up any previously created
	// resources and mark the pack as Inactive (catalog-only).
	if !pack.Spec.Enabled {
		log.Info("Ensemble is not enabled, cleaning up any existing resources")
		for _, persona := range pack.Spec.AgentConfigs {
			if err := r.cleanupPersona(ctx, log, pack, &persona); err != nil {
				log.Error(err, "Failed to clean up persona for disabled pack", "persona", persona.Name)
			}
		}

		// Wait for stamped resources to actually disappear before deleting auth secrets.
		var instList sympoziumv1alpha1.AgentList
		if err := r.List(ctx, &instList, client.InNamespace(pack.Namespace), client.MatchingLabels{"sympozium.ai/ensemble": pack.Name}); err != nil {
			return ctrl.Result{}, err
		}
		var schedList sympoziumv1alpha1.SympoziumScheduleList
		if err := r.List(ctx, &schedList, client.InNamespace(pack.Namespace), client.MatchingLabels{"sympozium.ai/ensemble": pack.Name}); err != nil {
			return ctrl.Result{}, err
		}
		if len(instList.Items) > 0 || len(schedList.Items) > 0 {
			log.Info("Waiting for persona resources to terminate before auth secret cleanup",
				"instancesRemaining", len(instList.Items),
				"schedulesRemaining", len(schedList.Items))
			return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
		}
		if len(pack.Spec.AuthRefs) > 0 {
			deleted, err := r.deleteManagedAuthSecrets(ctx, pack)
			if err != nil {
				return ctrl.Result{}, err
			}
			if deleted > 0 {
				log.Info("Deleted managed Ensemble auth secrets", "count", deleted)
			}
		}

		pack.Status.Phase = "Inactive"
		pack.Status.AgentConfigCount = len(pack.Spec.AgentConfigs)
		pack.Status.InstalledCount = 0
		pack.Status.InstalledAgentConfigs = nil
		pack.Status.SharedMemoryReady = false
		pack.Status.AllAgentsServing = false
		pack.Status.StimulusDelivered = false
		if err := r.Status().Update(ctx, pack); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// Resolve modelRef once for the whole ensemble.
	var modelEndpoint string
	if pack.Spec.ModelRef != "" {
		model, err := ResolveModelRef(ctx, r.Client, pack.Spec.ModelRef, pack.Namespace)
		if err != nil {
			log.Info("Model not found for modelRef, waiting", "modelRef", pack.Spec.ModelRef)
			return ctrl.Result{RequeueAfter: 10_000_000_000}, nil // 10s
		}
		if model.Status.Phase != sympoziumv1alpha1.ModelPhaseReady {
			log.Info("Model not ready, waiting", "modelRef", pack.Spec.ModelRef, "phase", model.Status.Phase)
			return ctrl.Result{RequeueAfter: 10_000_000_000}, nil
		}
		modelEndpoint = model.Status.Endpoint
	}

	// Validate the relationship graph for cycles and stimulus constraints before proceeding.
	if err := validateRelationshipGraph(pack.Spec.AgentConfigs, pack.Spec.Relationships, pack.Spec.Stimulus); err != nil {
		log.Error(err, "Invalid relationship graph")
		pack.Status.Phase = "Error"
		if statusErr := r.Status().Update(ctx, pack); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{}, nil
	}

	// Reconcile each persona → instance + schedule + memory
	var installed []sympoziumv1alpha1.InstalledAgentConfig
	var installErr error
	for i, persona := range pack.Spec.AgentConfigs {
		// Skip personas that have been excluded (disabled via TUI).
		if isExcluded(persona.Name, pack.Spec.ExcludeAgentConfigs) {
			if err := r.cleanupPersona(ctx, log, pack, &persona); err != nil {
				log.Error(err, "Failed to clean up excluded persona", "persona", persona.Name)
			}
			continue
		}
		ip, err := r.reconcileAgentConfig(ctx, log, pack, &persona, i, modelEndpoint)
		if err != nil {
			log.Error(err, "Failed to reconcile persona", "persona", persona.Name)
			installErr = err
			continue
		}
		installed = append(installed, ip)
	}

	// Reconcile shared memory infrastructure for the pack.
	if err := r.reconcileSharedMemory(ctx, log, pack); err != nil {
		log.Error(err, "Failed to reconcile shared memory")
		installErr = err
	}

	// Update status
	pack.Status.AgentConfigCount = len(pack.Spec.AgentConfigs)
	pack.Status.InstalledCount = len(installed)
	pack.Status.InstalledAgentConfigs = installed
	if installErr != nil {
		pack.Status.Phase = "Error"
	} else {
		pack.Status.Phase = "Ready"
	}

	// Check if all agents are ready for stimulus delivery.
	if pack.Spec.Stimulus != nil && installErr == nil {
		if err := r.reconcileStimulus(ctx, log, pack); err != nil {
			log.Error(err, "Failed to reconcile stimulus")
		}
	}

	if err := r.Status().Update(ctx, pack); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, installErr
}

// reconcileAgentConfig ensures the Agent and optional
// SympoziumSchedule exist for one persona.
func (r *EnsembleReconciler) reconcileAgentConfig(
	ctx context.Context,
	log logr.Logger,
	pack *sympoziumv1alpha1.Ensemble,
	persona *sympoziumv1alpha1.AgentConfigSpec,
	personaIndex int,
	modelEndpoint string,
) (sympoziumv1alpha1.InstalledAgentConfig, error) {
	instanceName := pack.Name + "-" + persona.Name
	ip := sympoziumv1alpha1.InstalledAgentConfig{
		Name:         persona.Name,
		InstanceName: instanceName,
	}

	// --- Agent ---
	existingInst := &sympoziumv1alpha1.Agent{}
	err := r.Get(ctx, client.ObjectKey{Name: instanceName, Namespace: pack.Namespace}, existingInst)
	if errors.IsNotFound(err) {
		inst := r.buildAgent(pack, persona, instanceName, modelEndpoint)
		if err := ctrl.SetControllerReference(pack, inst, r.Scheme); err != nil {
			return ip, fmt.Errorf("set owner ref on instance: %w", err)
		}
		log.Info("Creating Agent for persona", "instance", instanceName, "persona", persona.Name)
		if err := r.Create(ctx, inst); err != nil {
			return ip, fmt.Errorf("create instance %s: %w", instanceName, err)
		}
	} else if err != nil {
		return ip, fmt.Errorf("get instance %s: %w", instanceName, err)
	} else {
		// The Ensemble fully owns the Agent's Spec — compare the desired
		// spec (rebuilt fresh from pack + persona) against the live one as
		// a single unit. This way every AgentConfig/AgentSpec field
		// propagates automatically when its source field on the Ensemble
		// changes; new fields require no per-field comparison block.
		desired := r.buildAgent(pack, persona, instanceName, modelEndpoint)

		needsUpdate := false

		// Reconcile only Ensemble-managed labels; preserve everything else
		// (Argo tracking-ids, operator-added selectors, etc.).
		for _, k := range ensembleManagedLabels {
			wantValue, wantSet := desired.Labels[k]
			haveValue, haveSet := existingInst.Labels[k]
			switch {
			case wantSet && (!haveSet || haveValue != wantValue):
				if existingInst.Labels == nil {
					existingInst.Labels = map[string]string{}
				}
				existingInst.Labels[k] = wantValue
				needsUpdate = true
			case !wantSet && haveSet:
				delete(existingInst.Labels, k)
				needsUpdate = true
			}
		}

		if !equality.Semantic.DeepEqual(existingInst.Spec, desired.Spec) {
			existingInst.Spec = desired.Spec
			needsUpdate = true
		}

		if needsUpdate {
			log.Info("Updating pack-level settings on existing instance", "instance", instanceName)
			if err := r.Update(ctx, existingInst); err != nil {
				return ip, fmt.Errorf("update instance %s: %w", instanceName, err)
			}
		}
	}
	// Instance is now up to date — users own other fields after creation.

	// --- Memory seeds ---
	// Always invoked when persona.Memory is set so the reconciler can also
	// garbage-collect seeds that have been edited or removed from the spec.
	if persona.Memory != nil {
		if err := r.reconcileMemorySeeds(ctx, log, pack, persona, instanceName); err != nil {
			log.Error(err, "Failed to seed memory", "instance", instanceName)
			// Non-fatal: continue
		}
	}

	// --- SympoziumSchedule ---
	schedName := instanceName + "-schedule"
	if persona.Schedule != nil {
		ip.ScheduleName = schedName

		desired := r.buildSchedule(pack, persona, instanceName, schedName, personaIndex)
		existingSched := &sympoziumv1alpha1.SympoziumSchedule{}
		err := r.Get(ctx, client.ObjectKey{Name: schedName, Namespace: pack.Namespace}, existingSched)
		if errors.IsNotFound(err) {
			if err := ctrl.SetControllerReference(pack, desired, r.Scheme); err != nil {
				return ip, fmt.Errorf("set owner ref on schedule: %w", err)
			}
			log.Info("Creating SympoziumSchedule for persona", "schedule", schedName, "persona", persona.Name)
			if err := r.Create(ctx, desired); err != nil {
				return ip, fmt.Errorf("create schedule %s: %w", schedName, err)
			}
		} else if err != nil {
			return ip, fmt.Errorf("get schedule %s: %w", schedName, err)
		} else {
			needsUpdate := false
			if !reflect.DeepEqual(existingSched.Spec, desired.Spec) {
				existingSched.Spec = desired.Spec
				needsUpdate = true
			}
			if existingSched.Labels == nil {
				existingSched.Labels = map[string]string{}
			}
			for k, v := range desired.Labels {
				if existingSched.Labels[k] != v {
					existingSched.Labels[k] = v
					needsUpdate = true
				}
			}
			if needsUpdate {
				log.Info("Updating SympoziumSchedule for persona", "schedule", schedName, "persona", persona.Name)
				if err := r.Update(ctx, existingSched); err != nil {
					return ip, fmt.Errorf("update schedule %s: %w", schedName, err)
				}
			}
		}
	} else {
		// Persona no longer has a schedule configured — remove any stale one.
		existingSched := &sympoziumv1alpha1.SympoziumSchedule{}
		err := r.Get(ctx, client.ObjectKey{Name: schedName, Namespace: pack.Namespace}, existingSched)
		if err == nil {
			log.Info("Deleting stale SympoziumSchedule for persona", "schedule", schedName, "persona", persona.Name)
			if err := r.Delete(ctx, existingSched); err != nil && !errors.IsNotFound(err) {
				return ip, fmt.Errorf("delete stale schedule %s: %w", schedName, err)
			}
		} else if !errors.IsNotFound(err) {
			return ip, fmt.Errorf("get stale schedule %s: %w", schedName, err)
		}
	}

	return ip, nil
}

// buildAgent creates a Agent spec from a persona definition.
func (r *EnsembleReconciler) buildAgent(
	pack *sympoziumv1alpha1.Ensemble,
	persona *sympoziumv1alpha1.AgentConfigSpec,
	instanceName string,
	modelEndpoint string,
) *sympoziumv1alpha1.Agent {
	model := persona.Model
	if model == "" {
		model = "gpt-4o" // sensible default; overridden by onboarding
	}

	baseURL := pack.Spec.BaseURL
	authRefs := pack.Spec.AuthRefs

	// If a cluster-local Model is referenced, override provider settings.
	if modelEndpoint != "" {
		baseURL = modelEndpoint
		model = pack.Spec.ModelRef
		authRefs = nil // no auth needed for cluster-internal inference
	}

	// Per-persona provider/baseURL overrides take precedence.
	if persona.BaseURL != "" {
		baseURL = persona.BaseURL
	}
	if persona.Provider != "" {
		// Find the matching auth secret for this provider from the ensemble's refs.
		var matched []sympoziumv1alpha1.SecretRef
		for _, ref := range pack.Spec.AuthRefs {
			if ref.Provider == persona.Provider {
				matched = append(matched, ref)
			}
		}
		if len(matched) > 0 {
			authRefs = matched
		}
	}

	// Merge provider headers: ensemble-level base, persona-level overrides.
	providerHeaders := mergeProviderHeaders(pack.Spec.ProviderHeaders, persona.ProviderHeaders)
	providerHeadersSecretRef := pack.Spec.ProviderHeadersSecretRef
	if persona.ProviderHeadersSecretRef != "" {
		providerHeadersSecretRef = persona.ProviderHeadersSecretRef
	}

	labels := map[string]string{
		"sympozium.ai/ensemble":     pack.Name,
		"sympozium.ai/agent-config": persona.Name,
	}
	if persona.Provider != "" {
		labels["sympozium.ai/provider"] = persona.Provider
	}

	inst := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      instanceName,
			Namespace: pack.Namespace,
			Labels:    labels,
		},
		Spec: sympoziumv1alpha1.AgentSpec{
			Agents: sympoziumv1alpha1.AgentsSpec{
				Default: sympoziumv1alpha1.AgentConfig{
					Model:                    model,
					BaseURL:                  baseURL,
					ProviderHeaders:          providerHeaders,
					ProviderHeadersSecretRef: providerHeadersSecretRef,
					AgentSandbox:             pack.Spec.AgentSandbox,
					Lifecycle:                persona.Lifecycle,
					Tolerations:              persona.Tolerations,
					Subagents:                persona.Subagents,
					Env:                      persona.Env,
					Thinking:                 persona.Thinking,
					MaxTokens:                persona.MaxTokens,
					Temperature:              persona.Temperature,
					RunTimeout:               persona.RunTimeout,
				},
			},
			AuthRefs: authRefs,
			Memory: &sympoziumv1alpha1.MemorySpec{
				Enabled:      true,
				SystemPrompt: persona.SystemPrompt,
			},
			Observability: defaultObservabilitySpec(),
			Volumes:       pack.Spec.Volumes,
			VolumeMounts:  pack.Spec.VolumeMounts,
			Workspace:     resolveWorkspaceSpec(pack.Spec.Workspace, persona.Workspace),
			Harness:       resolveHarness(pack.Spec.Harness, persona.Harness),
		},
	}

	// Skills — skip "mcp-bridge" which is a sidecar marker, not a SkillPack.
	for _, s := range persona.Skills {
		if s == "mcp-bridge" {
			continue
		}
		ref := sympoziumv1alpha1.SkillRef{
			SkillPackRef: s,
		}
		// Apply pack-level skill params if configured (e.g. repo for github-gitops).
		if pack.Spec.SkillParams != nil {
			if params, ok := pack.Spec.SkillParams[s]; ok && len(params) > 0 {
				ref.Params = params
			}
		}
		inst.Spec.Skills = append(inst.Spec.Skills, ref)
	}

	// Ensure memory skill is always attached.
	hasMemory := false
	for _, s := range inst.Spec.Skills {
		if s.SkillPackRef == "memory" {
			hasMemory = true
			break
		}
	}
	if !hasMemory {
		inst.Spec.Skills = append(inst.Spec.Skills, sympoziumv1alpha1.SkillRef{
			SkillPackRef: "memory",
		})
	}

	// Channels
	for _, ch := range persona.Channels {
		cs := buildChannelSpec(pack, persona, ch)
		inst.Spec.Channels = append(inst.Spec.Channels, cs)
	}

	// MCP servers — persona-level configuration, mirrors how Skills works.
	inst.Spec.MCPServers = persona.MCPServers

	// Policy — use the pack's policy ref if set.
	inst.Spec.PolicyRef = pack.Spec.PolicyRef

	// Web endpoint — add the web-endpoint skill instead of the legacy field.
	if persona.WebEndpoint != nil && persona.WebEndpoint.Enabled {
		params := map[string]string{}
		if persona.WebEndpoint.Hostname != "" {
			params["hostname"] = persona.WebEndpoint.Hostname
		}
		inst.Spec.Skills = append(inst.Spec.Skills, sympoziumv1alpha1.SkillRef{
			SkillPackRef: "web-endpoint",
			Params:       params,
		})
	}

	return inst
}

// buildSchedule creates a SympoziumSchedule from a persona's schedule config.
// personaIndex is used to stagger interval-based schedules so that personas in
// the same pack don't fire simultaneously and contend for a shared LLM.
func (r *EnsembleReconciler) buildSchedule(
	pack *sympoziumv1alpha1.Ensemble,
	persona *sympoziumv1alpha1.AgentConfigSpec,
	instanceName, schedName string,
	personaIndex int,
) *sympoziumv1alpha1.SympoziumSchedule {
	cron := persona.Schedule.Cron
	if cron == "" {
		// Stagger each persona by 2 minutes to avoid GPU contention on local LLMs.
		// For a 5-min interval with 7 personas this gives offsets 0,2,4,1,3,0,2 —
		// at most 2 agents overlap instead of all 7 firing simultaneously.
		staggerMin := personaIndex * 2
		cron = intervalToCron(persona.Schedule.Interval, staggerMin)
	}

	return &sympoziumv1alpha1.SympoziumSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      schedName,
			Namespace: pack.Namespace,
			Labels: map[string]string{
				"sympozium.ai/ensemble":     pack.Name,
				"sympozium.ai/agent-config": persona.Name,
			},
		},
		Spec: sympoziumv1alpha1.SympoziumScheduleSpec{
			AgentRef:          instanceName,
			Schedule:          cron,
			Task:              r.buildScheduleTask(pack, persona),
			Type:              persona.Schedule.Type,
			ConcurrencyPolicy: "Forbid",
			IncludeMemory:     true,
		},
	}
}

// buildScheduleTask constructs the task string for a persona's schedule.
// If the pack has a TaskOverride, it prepends the team-level directive.
func (r *EnsembleReconciler) buildScheduleTask(
	pack *sympoziumv1alpha1.Ensemble,
	persona *sympoziumv1alpha1.AgentConfigSpec,
) string {
	base := persona.Schedule.Task
	if pack.Spec.TaskOverride != "" {
		return fmt.Sprintf("TEAM OBJECTIVE: %s\n\nYOUR ROLE TASK: %s", pack.Spec.TaskOverride, base)
	}
	return base
}

// reconcileMemorySeeds writes the persona's declared seed memories into the
// central memory-server. Idempotency is tracked via an annotation on the
// generated Agent listing the SHA-256 prefixes of seeds already posted,
// so re-reconciles never duplicate seeds and newly added seeds are picked up
// on the next pass without rewriting old ones.
//
// Every posted seed is tagged with "seed-hash:<hash>" so that when a seed is
// edited or removed from the persona spec, this reconciler can locate and
// delete the orphan via the memory-server's admin delete-by-tags endpoint.
//
// When the memory subsystem is disabled (no MemoryClient configured) this is
// a no-op so personas still come up cleanly in memory-less deployments.
func (r *EnsembleReconciler) reconcileMemorySeeds(
	ctx context.Context,
	log logr.Logger,
	pack *sympoziumv1alpha1.Ensemble,
	persona *sympoziumv1alpha1.AgentConfigSpec,
	instanceName string,
) error {
	if r.MemoryClient == nil {
		log.V(1).Info("Memory subsystem disabled; skipping seed reconcile", "instance", instanceName)
		return nil
	}
	if persona == nil || persona.Memory == nil {
		return nil
	}

	// Read the generated Agent so we can dedupe via its annotation. The
	// Agent is created by reconcileAgentConfig earlier in this reconcile
	// pass, so it should always exist by the time we get here.
	inst := &sympoziumv1alpha1.Agent{}
	if err := r.Get(ctx, client.ObjectKey{Name: instanceName, Namespace: pack.Namespace}, inst); err != nil {
		return fmt.Errorf("get instance %s: %w", instanceName, err)
	}

	const seededAnno = "sympozium.ai/memory-seeds-applied"
	applied := parseSeedHashSet(inst.GetAnnotations()[seededAnno])

	// Build the desired hash set so we can both skip already-posted seeds
	// and identify orphans to garbage-collect.
	desired := make(map[string]string, len(persona.Memory.Seeds)) // hash -> content
	for _, seed := range persona.Memory.Seeds {
		seed = strings.TrimSpace(seed)
		if seed == "" {
			continue
		}
		desired[seedHash(seed)] = seed
	}

	ttlDays := 0
	if persona.Memory.SeedTTLDays != nil && *persona.Memory.SeedTTLDays > 0 {
		ttlDays = *persona.Memory.SeedTTLDays
	}

	added := 0
	for hash, seed := range desired {
		if _, ok := applied[hash]; ok {
			continue
		}
		_, err := r.MemoryClient.Store(ctx, memoryclient.StoreRequest{
			Scope:     "agent",
			AgentName: instanceName,
			Content:   seed,
			TTLDays:   ttlDays,
			Tags: []string{
				"seed",
				"ensemble:" + pack.Name,
				"persona:" + persona.Name,
				"seed-hash:" + hash,
			},
		})
		if err != nil {
			if added > 0 {
				if patchErr := patchSeedAnnotation(ctx, r.Client, inst, seededAnno, applied); patchErr != nil {
					log.Error(patchErr, "Failed to persist seed annotation after partial write", "instance", instanceName)
				}
			}
			return fmt.Errorf("post seed to memory-server: %w", err)
		}
		applied[hash] = struct{}{}
		added++
	}

	// Garbage-collect seeds that were previously applied but are no longer
	// in the spec (edited or removed). We only consider hashes recorded in
	// our annotation so we never touch user-authored memories.
	removed := 0
	for hash := range applied {
		if _, stillWanted := desired[hash]; stillWanted {
			continue
		}
		_, err := r.MemoryClient.DeleteByTags(ctx, memoryclient.DeleteByTagsRequest{
			Namespace: pack.Namespace,
			Scope:     "agent",
			AgentName: instanceName,
			RequireTags: []string{
				"seed",
				"ensemble:" + pack.Name,
				"persona:" + persona.Name,
				"seed-hash:" + hash,
			},
		})
		if err != nil {
			// Best-effort GC: log and continue so a transient delete
			// failure doesn't block the rest of the reconcile.
			log.Error(err, "Failed to GC orphaned seed", "instance", instanceName, "hash", hash)
			continue
		}
		delete(applied, hash)
		removed++
	}

	if added == 0 && removed == 0 {
		return nil
	}
	log.Info("Reconciled persona memory seeds",
		"instance", instanceName,
		"added", added,
		"removed", removed,
		"totalApplied", len(applied),
	)
	return patchSeedAnnotation(ctx, r.Client, inst, seededAnno, applied)
}

// seedHash returns a stable 16-char prefix of the SHA-256 hex digest of a
// seed string. 16 hex chars = 64 bits of entropy, comfortably collision-free
// for the small per-persona seed sets we expect.
func seedHash(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	return hex.EncodeToString(sum[:])[:16]
}

// parseSeedHashSet parses the comma-separated hash list stored in the
// idempotency annotation.
func parseSeedHashSet(raw string) map[string]struct{} {
	out := make(map[string]struct{})
	if raw == "" {
		return out
	}
	for _, p := range strings.Split(raw, ",") {
		p = strings.TrimSpace(p)
		if p != "" {
			out[p] = struct{}{}
		}
	}
	return out
}

// patchSeedAnnotation merges the supplied hash set into the Agent's
// annotation map using a server-side strategic merge patch.
func patchSeedAnnotation(ctx context.Context, c client.Client, inst *sympoziumv1alpha1.Agent, key string, applied map[string]struct{}) error {
	hashes := make([]string, 0, len(applied))
	for h := range applied {
		hashes = append(hashes, h)
	}
	sort.Strings(hashes)

	patch := client.MergeFrom(inst.DeepCopy())
	if inst.Annotations == nil {
		inst.Annotations = map[string]string{}
	}
	inst.Annotations[key] = strings.Join(hashes, ",")
	return c.Patch(ctx, inst, patch)
}

// intervalToCron converts a human-readable interval to a cron expression.
// offsetMin staggers the schedule by shifting the minute field, so that
// personas in the same pack don't all fire simultaneously and contend for
// a shared LLM (especially important with local models like LM Studio).
func intervalToCron(interval string, offsetMin int) string {
	switch strings.ToLower(strings.TrimSpace(interval)) {
	case "1m", "1min":
		return "* * * * *" // can't stagger 1-minute intervals
	case "5m", "5min":
		return fmt.Sprintf("%d/5 * * * *", offsetMin%5)
	case "10m", "10min":
		return fmt.Sprintf("%d/10 * * * *", offsetMin%10)
	case "15m", "15min":
		return fmt.Sprintf("%d/15 * * * *", offsetMin%15)
	case "30m", "30min":
		return fmt.Sprintf("%d/30 * * * *", offsetMin%30)
	case "1h", "60m":
		return fmt.Sprintf("%d * * * *", offsetMin%60)
	case "2h":
		return fmt.Sprintf("%d */2 * * *", offsetMin%60)
	case "3h":
		return fmt.Sprintf("%d */3 * * *", offsetMin%60)
	case "4h":
		return fmt.Sprintf("%d */4 * * *", offsetMin%60)
	case "6h":
		return fmt.Sprintf("%d */6 * * *", offsetMin%60)
	case "12h":
		return fmt.Sprintf("%d */12 * * *", offsetMin%60)
	case "24h", "1d":
		return fmt.Sprintf("%d 0 * * *", offsetMin%60)
	default:
		// If it already looks like a cron expression, return as-is
		if strings.Contains(interval, " ") {
			return interval
		}
		return fmt.Sprintf("%d * * * *", offsetMin%60) // default: hourly
	}
}

// isExcluded checks whether a persona name appears in the exclusion list.
func isExcluded(name string, excludes []string) bool {
	for _, e := range excludes {
		if e == name {
			return true
		}
	}
	return false
}

// cleanupPersona deletes the Instance and Schedule for a persona that has
// been excluded from the pack. Memory rows in the central memory-server
// are left intact.
func (r *EnsembleReconciler) cleanupPersona(
	ctx context.Context,
	log logr.Logger,
	pack *sympoziumv1alpha1.Ensemble,
	persona *sympoziumv1alpha1.AgentConfigSpec,
) error {
	instanceName := pack.Name + "-" + persona.Name

	// Cancel active AgentRuns and delete all runs for this persona.
	var runList sympoziumv1alpha1.AgentRunList
	if err := r.List(ctx, &runList, client.InNamespace(pack.Namespace), client.MatchingLabels{"sympozium.ai/instance": instanceName}); err == nil {
		for i := range runList.Items {
			run := &runList.Items[i]
			switch run.Status.Phase {
			case sympoziumv1alpha1.AgentRunPhaseRunning,
				sympoziumv1alpha1.AgentRunPhaseAwaitingDelegate,
				sympoziumv1alpha1.AgentRunPhasePending,
				sympoziumv1alpha1.AgentRunPhaseServing:
				log.Info("Cancelling running AgentRun for disabled persona", "agentrun", run.Name)
				if run.Status.PodName != "" {
					pod := &corev1.Pod{}
					if err := r.Get(ctx, client.ObjectKey{Name: run.Status.PodName, Namespace: pack.Namespace}, pod); err == nil {
						if err := r.Delete(ctx, pod); err != nil && !errors.IsNotFound(err) {
							log.Error(err, "Failed to delete pod for cancelled AgentRun", "pod", run.Status.PodName)
						}
					}
				}
				run.Status.Phase = sympoziumv1alpha1.AgentRunPhaseFailed
				if err := r.Status().Update(ctx, run); err != nil && !errors.IsNotFound(err) {
					log.Error(err, "Failed to mark AgentRun as failed", "agentrun", run.Name)
				}
			}
			// Delete all runs (active or terminal) so the ensemble starts clean on re-enable.
			if err := r.Delete(ctx, run); err != nil && !errors.IsNotFound(err) {
				log.Error(err, "Failed to delete AgentRun for disabled persona", "agentrun", run.Name)
			}
		}
	}

	// Delete Agent
	inst := &sympoziumv1alpha1.Agent{}
	if err := r.Get(ctx, client.ObjectKey{Name: instanceName, Namespace: pack.Namespace}, inst); err == nil {
		log.Info("Deleting excluded persona instance", "instance", instanceName)
		if err := r.Delete(ctx, inst); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete instance %s: %w", instanceName, err)
		}
	}

	// Delete SympoziumSchedule
	schedName := instanceName + "-schedule"
	sched := &sympoziumv1alpha1.SympoziumSchedule{}
	if err := r.Get(ctx, client.ObjectKey{Name: schedName, Namespace: pack.Namespace}, sched); err == nil {
		log.Info("Deleting excluded persona schedule", "schedule", schedName)
		if err := r.Delete(ctx, sched); err != nil && !errors.IsNotFound(err) {
			return fmt.Errorf("delete schedule %s: %w", schedName, err)
		}
	}

	return nil
}

// reconcileDelete cleans up resources owned by the Ensemble.
func (r *EnsembleReconciler) reconcileDelete(
	ctx context.Context,
	log logr.Logger,
	pack *sympoziumv1alpha1.Ensemble,
) (ctrl.Result, error) {
	log.Info("Reconciling Ensemble deletion")

	// Owner references handle cascade deletion of instances and schedules.
	// Memory rows in the central memory-server are intentionally not purged
	// on ensemble deletion; operators must call the admin delete endpoint to
	// avoid accidental data loss.

	controllerutil.RemoveFinalizer(pack, ensembleFinalizer)
	if err := r.Update(ctx, pack); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// mergeProviderHeaders merges ensemble-level and persona-level provider headers.
// Persona keys take precedence on collision. Returns nil if both inputs are empty.
func mergeProviderHeaders(ensembleHeaders, personaHeaders map[string]string) map[string]string {
	if len(ensembleHeaders) == 0 && len(personaHeaders) == 0 {
		return nil
	}
	merged := make(map[string]string)
	for k, v := range ensembleHeaders {
		merged[k] = v
	}
	for k, v := range personaHeaders {
		merged[k] = v
	}
	return merged
}

// resolveWorkspaceSpec picks the WorkspaceSpec to apply to a generated
// Agent: the persona-level override wins when non-nil; otherwise the
// ensemble-level default is used (which may itself be nil — meaning the
// agent gets the legacy emptyDir behaviour).
func resolveWorkspaceSpec(ensembleWS, personaWS *sympoziumv1alpha1.WorkspaceSpec) *sympoziumv1alpha1.WorkspaceSpec {
	if personaWS != nil {
		out := *personaWS
		return &out
	}
	if ensembleWS != nil {
		out := *ensembleWS
		return &out
	}
	return nil
}

// resolveHarness picks the harness identifier to stamp on a generated
// Agent: a non-empty persona value wins; otherwise the ensemble-level
// value is used. Empty on both layers means "use the built-in
// agent-runner".
func resolveHarness(ensembleH, personaH string) string {
	if personaH != "" {
		return personaH
	}
	return ensembleH
}

// buildChannelSpec computes the desired ChannelSpec for a given channel type
// from pack and persona configuration. Persona-level overrides take priority
// over ensemble-level defaults for AccessControl and Triggers.
func buildChannelSpec(pack *sympoziumv1alpha1.Ensemble, persona *sympoziumv1alpha1.AgentConfigSpec, ch string) sympoziumv1alpha1.ChannelSpec {
	cs := sympoziumv1alpha1.ChannelSpec{Type: ch}
	if pack.Spec.ChannelConfigs != nil {
		if secretName, ok := pack.Spec.ChannelConfigs[ch]; ok && secretName != "" {
			cs.ConfigRef = sympoziumv1alpha1.SecretRef{Secret: secretName}
		}
	}
	if persona.ChannelAccessControl != nil {
		if ac, ok := persona.ChannelAccessControl[ch]; ok {
			cs.AccessControl = ac
		}
	}
	if cs.AccessControl == nil && pack.Spec.ChannelAccessControl != nil {
		if ac, ok := pack.Spec.ChannelAccessControl[ch]; ok {
			cs.AccessControl = ac
		}
	}
	if persona.ChannelTriggers != nil {
		if tr, ok := persona.ChannelTriggers[ch]; ok {
			cs.Triggers = tr
		}
	}
	if cs.Triggers == nil && pack.Spec.ChannelTriggers != nil {
		if tr, ok := pack.Spec.ChannelTriggers[ch]; ok {
			cs.Triggers = tr
		}
	}
	// Slack-specific options: persona-level overrides take priority
	// over ensemble-level. Only applied to the slack channel type.
	if ch == "slack" {
		if persona.SlackOptions != nil {
			cs.Slack = persona.SlackOptions
		} else if pack.Spec.SlackOptions != nil {
			cs.Slack = pack.Spec.SlackOptions
		}
	}
	if v, ok := pack.Spec.ChannelVolumes[ch]; ok {
		cs.Volumes = v
	}
	if vm, ok := pack.Spec.ChannelVolumeMounts[ch]; ok {
		cs.VolumeMounts = vm
	}
	return cs
}

// buildDesiredSkills computes the desired skills list for a persona, matching
// the logic in buildAgent. This is used to reconcile skills on existing Agents.
func buildDesiredSkills(pack *sympoziumv1alpha1.Ensemble, persona *sympoziumv1alpha1.AgentConfigSpec) []sympoziumv1alpha1.SkillRef {
	var skills []sympoziumv1alpha1.SkillRef
	for _, s := range persona.Skills {
		if s == "mcp-bridge" {
			continue
		}
		ref := sympoziumv1alpha1.SkillRef{
			SkillPackRef: s,
		}
		if pack.Spec.SkillParams != nil {
			if params, ok := pack.Spec.SkillParams[s]; ok && len(params) > 0 {
				ref.Params = params
			}
		}
		skills = append(skills, ref)
	}

	// Ensure memory skill is always attached.
	hasMemory := false
	for _, s := range skills {
		if s.SkillPackRef == "memory" {
			hasMemory = true
			break
		}
	}
	if !hasMemory {
		skills = append(skills, sympoziumv1alpha1.SkillRef{
			SkillPackRef: "memory",
		})
	}

	// Web endpoint skill.
	if persona.WebEndpoint != nil && persona.WebEndpoint.Enabled {
		params := map[string]string{}
		if persona.WebEndpoint.Hostname != "" {
			params["hostname"] = persona.WebEndpoint.Hostname
		}
		skills = append(skills, sympoziumv1alpha1.SkillRef{
			SkillPackRef: "web-endpoint",
			Params:       params,
		})
	}

	return skills
}

// skillRefsEqual compares two SkillRef slices for equality.
func skillRefsEqual(a, b []sympoziumv1alpha1.SkillRef) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].SkillPackRef != b[i].SkillPackRef || a[i].ConfigMapRef != b[i].ConfigMapRef {
			return false
		}
		if len(a[i].Params) != len(b[i].Params) {
			return false
		}
		for k, v := range a[i].Params {
			if b[i].Params[k] != v {
				return false
			}
		}
	}
	return true
}

// reconcileSharedMemory mirrors spec.sharedMemory.enabled into
// status.sharedMemoryReady. With the central memory-server there is no
// per-Ensemble infrastructure to provision; the ensemble scope is a
// logical pool inside the cluster-wide service.
func (r *EnsembleReconciler) reconcileSharedMemory(_ context.Context, _ logr.Logger, pack *sympoziumv1alpha1.Ensemble) error {
	pack.Status.SharedMemoryReady = pack.Spec.SharedMemory != nil && pack.Spec.SharedMemory.Enabled
	return nil
}

// reconcileStimulus checks whether all agents in the ensemble are ready
// (Running or Serving phase). On the transition edge (not-all-ready →
// all-ready), it creates an AgentRun targeting the stimulus relationship
// target. We accept "Running" (not just "Serving") because the stimulus
// typically creates the *first* AgentRun for the target agent.
func (r *EnsembleReconciler) reconcileStimulus(ctx context.Context, log logr.Logger, pack *sympoziumv1alpha1.Ensemble) error {
	// Count agents whose infrastructure is ready (Running or Serving).
	// "Running" means the Agent CRD is reconciled and memory deployments
	// are up; "Serving" means it additionally has active AgentRun pods.
	// The stimulus creates the *first* AgentRun, so we cannot require
	// Serving — that would be a deadlock.
	readyCount := 0
	for _, ip := range pack.Status.InstalledAgentConfigs {
		var agent sympoziumv1alpha1.Agent
		if err := r.Get(ctx, types.NamespacedName{Name: ip.InstanceName, Namespace: pack.Namespace}, &agent); err != nil {
			continue
		}
		if agent.Status.Phase == "Running" || agent.Status.Phase == "Serving" {
			readyCount++
		}
	}

	allReady := readyCount > 0 && readyCount == len(pack.Status.InstalledAgentConfigs)
	prevAllReady := pack.Status.AllAgentsServing
	pack.Status.AllAgentsServing = allReady

	// Detect the edge: not-all-ready → all-ready.
	if !prevAllReady && allReady && !pack.Status.StimulusDelivered {
		if err := r.deliverStimulus(ctx, log, pack, "readiness"); err != nil {
			return err
		}
	}

	return nil
}

// deliverStimulus creates an AgentRun for the stimulus target agent.
func (r *EnsembleReconciler) deliverStimulus(ctx context.Context, log logr.Logger, pack *sympoziumv1alpha1.Ensemble, triggerSource string) error {
	// Resolve stimulus relationship target.
	var targetPersona string
	for _, rel := range pack.Spec.Relationships {
		if rel.Type == "stimulus" {
			targetPersona = rel.Target
			break
		}
	}
	if targetPersona == "" {
		return fmt.Errorf("stimulus spec configured but no stimulus relationship found")
	}

	targetAgentName := pack.Name + "-" + targetPersona

	// Look up the target agent instance.
	var targetInst sympoziumv1alpha1.Agent
	if err := r.Get(ctx, types.NamespacedName{Name: targetAgentName, Namespace: pack.Namespace}, &targetInst); err != nil {
		return fmt.Errorf("stimulus target agent %q not found: %w", targetAgentName, err)
	}

	// Create the AgentRun.
	runName := fmt.Sprintf("%s-stimulus-%d", targetAgentName, time.Now().UnixMilli()%100000)
	agentRun := &sympoziumv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: pack.Namespace,
			Labels: map[string]string{
				"sympozium.ai/instance":       targetAgentName,
				"sympozium.ai/ensemble":       pack.Name,
				"sympozium.ai/stimulus":       "true",
				"sympozium.ai/trigger-source": triggerSource,
			},
		},
		Spec: sympoziumv1alpha1.AgentRunSpec{
			AgentRef: targetAgentName,
			Task:     pack.Spec.Stimulus.Prompt,
			AgentID:  fmt.Sprintf("stimulus-%s", pack.Spec.Stimulus.Name),
			Model: sympoziumv1alpha1.ModelSpec{
				Provider:                 resolveProvider(&targetInst),
				Model:                    targetInst.Spec.Agents.Default.Model,
				BaseURL:                  targetInst.Spec.Agents.Default.BaseURL,
				AuthSecretRef:            resolveAuthSecret(&targetInst),
				ProviderHeaders:          targetInst.Spec.Agents.Default.ProviderHeaders,
				ProviderHeadersSecretRef: targetInst.Spec.Agents.Default.ProviderHeadersSecretRef,
				Thinking:                 targetInst.Spec.Agents.Default.Thinking,
				MaxTokens:                targetInst.Spec.Agents.Default.MaxTokens,
				Temperature:              targetInst.Spec.Agents.Default.Temperature,
			},
			Skills:           targetInst.Spec.Skills,
			ImagePullSecrets: targetInst.Spec.ImagePullSecrets,
			Volumes:          targetInst.Spec.Volumes,
			VolumeMounts:     targetInst.Spec.VolumeMounts,
			Tolerations:      targetInst.Spec.Agents.Default.Tolerations,
			Env:              targetInst.Spec.Agents.Default.Env,
			Workspace:        targetInst.Spec.Workspace,
			Harness:          targetInst.Spec.Harness,
		},
	}

	if err := r.Create(ctx, agentRun); err != nil {
		return fmt.Errorf("failed to create stimulus AgentRun: %w", err)
	}

	log.Info("Delivered stimulus",
		"run", runName,
		"target", targetPersona,
		"trigger", triggerSource,
		"generation", pack.Status.StimulusGeneration+1)

	pack.Status.StimulusDelivered = true
	pack.Status.StimulusGeneration++

	// Publish event to the bus if available.
	if r.EventBus != nil {
		r.EventBus.Publish(ctx, eventbus.TopicStimulusDelivered, &eventbus.Event{
			Topic:     eventbus.TopicStimulusDelivered,
			Timestamp: time.Now(),
			Metadata: map[string]string{
				"ensemble":      pack.Name,
				"target":        targetPersona,
				"triggerSource": triggerSource,
				"runName":       runName,
			},
		})
	}

	return nil
}

// validateRelationshipGraph checks that all relationship source/target names
// reference existing personas and that the sequential edges form a DAG (no
// cycles). Delegation and supervision edges are not checked for cycles because
// delegation is on-demand and supervision has no runtime effect.
// It also validates stimulus relationship constraints.
func validateRelationshipGraph(personas []sympoziumv1alpha1.AgentConfigSpec, relationships []sympoziumv1alpha1.AgentConfigRelationship, stimulus *sympoziumv1alpha1.StimulusSpec) error {
	if len(relationships) == 0 && stimulus == nil {
		return nil
	}

	// Build the set of valid persona names.
	names := make(map[string]bool, len(personas))
	for _, p := range personas {
		names[p.Name] = true
	}

	// Validate stimulus relationships.
	stimulusRelCount := 0
	for _, rel := range relationships {
		if rel.Type == "stimulus" {
			stimulusRelCount++
		}
	}
	if stimulusRelCount > 1 {
		return fmt.Errorf("at most one stimulus relationship is allowed per ensemble, found %d", stimulusRelCount)
	}
	if stimulusRelCount == 1 && stimulus == nil {
		return fmt.Errorf("stimulus relationship defined but no stimulus spec configured")
	}
	if stimulus != nil && stimulusRelCount == 0 {
		return fmt.Errorf("stimulus spec configured but no stimulus relationship defined")
	}
	if stimulus != nil {
		if strings.TrimSpace(stimulus.Prompt) == "" {
			return fmt.Errorf("stimulus prompt must not be empty")
		}
		for _, rel := range relationships {
			if rel.Type == "stimulus" {
				if rel.Source != stimulus.Name {
					return fmt.Errorf("stimulus relationship source %q must match stimulus name %q", rel.Source, stimulus.Name)
				}
				if !names[rel.Target] {
					return fmt.Errorf("stimulus relationship references unknown persona %q (target)", rel.Target)
				}
				break
			}
		}
	}

	// Validate references and build the adjacency list for sequential edges.
	adj := make(map[string][]string)
	for _, rel := range relationships {
		if rel.Type == "stimulus" {
			continue // stimulus source is not a persona, skip persona name check
		}
		if !names[rel.Source] {
			return fmt.Errorf("relationship references unknown persona %q (source)", rel.Source)
		}
		if !names[rel.Target] {
			return fmt.Errorf("relationship references unknown persona %q (target)", rel.Target)
		}
		if rel.Type == "sequential" {
			adj[rel.Source] = append(adj[rel.Source], rel.Target)
		}
	}

	// DFS cycle detection using coloring: 0=white, 1=gray, 2=black.
	color := make(map[string]int, len(names))
	var path []string

	var dfs func(node string) error
	dfs = func(node string) error {
		color[node] = 1 // gray — currently visiting
		path = append(path, node)
		for _, next := range adj[node] {
			if color[next] == 1 {
				// Found a cycle — build the cycle path for the error message.
				cycleStart := 0
				for i, n := range path {
					if n == next {
						cycleStart = i
						break
					}
				}
				cycle := append(path[cycleStart:], next)
				return fmt.Errorf("cycle detected in sequential pipeline: %s", strings.Join(cycle, " -> "))
			}
			if color[next] == 0 {
				if err := dfs(next); err != nil {
					return err
				}
			}
		}
		path = path[:len(path)-1]
		color[node] = 2 // black — done
		return nil
	}

	for name := range names {
		if color[name] == 0 {
			if err := dfs(name); err != nil {
				return err
			}
		}
	}
	return nil
}

// SetupWithManager registers the controller.
func (r *EnsembleReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sympoziumv1alpha1.Ensemble{}).
		Owns(&sympoziumv1alpha1.Agent{}).
		Owns(&sympoziumv1alpha1.SympoziumSchedule{}).
		Complete(r)
}
