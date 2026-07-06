// Package controller contains the reconciliation logic for Sympozium CRDs.
package controller

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/sessionkey"
)

const sympoziumInstanceFinalizer = "sympozium.ai/finalizer"

// ChannelServiceAccountName returns the per-Agent ServiceAccount name used
// by all channel Deployments owned by that Agent. Channel pods authenticate
// to external systems (e.g. Vault Secrets Store CSI) using their projected
// SA token, so a distinct identity per Agent lets operators write per-Agent
// Vault policies and audit channel activity by Agent.
func ChannelServiceAccountName(instance *sympoziumv1alpha1.Agent) string {
	return instance.Name + "-channel"
}

// AgentReconciler reconciles a Agent object.
type AgentReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Log      logr.Logger
	ImageTag string // release tag for Sympozium images

	// ArtifactServerURL is the base URL of the central artifact-server.
	// When set, it is injected into channel pods so they can download
	// agent-produced files by reference for delivery. Empty disables it.
	ArtifactServerURL string
}

// +kubebuilder:rbac:groups=sympozium.ai,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=sympozium.ai,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=sympozium.ai,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=secrets;configmaps;services;serviceaccounts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=httproutes,verbs=get;list;watch;create;update;patch;delete

// Reconcile handles Agent reconciliation.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("sympoziuminstance", req.NamespacedName)

	var instance sympoziumv1alpha1.Agent
	if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Handle deletion
	if !instance.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(&instance, sympoziumInstanceFinalizer) {
			log.Info("Cleaning up instance resources")
			if err := r.cleanupChannelDeployments(ctx, &instance); err != nil {
				return ctrl.Result{}, err
			}
			// Per-agent ServiceAccount and Role are owner-referenced and will
			// be garbage-collected by Kubernetes when the Agent is deleted.
			// Memory rows in the central memory-server are not deleted on
			// Agent delete by design (they may still be useful for audit /
			// ensemble pool). Operators can call the admin DELETE endpoint to
			// purge an agent's scope.
			patch := client.MergeFrom(instance.DeepCopy())
			controllerutil.RemoveFinalizer(&instance, sympoziumInstanceFinalizer)
			if err := r.Patch(ctx, &instance, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// Add finalizer if missing
	if !controllerutil.ContainsFinalizer(&instance, sympoziumInstanceFinalizer) {
		patch := client.MergeFrom(instance.DeepCopy())
		controllerutil.AddFinalizer(&instance, sympoziumInstanceFinalizer)
		if err := r.Patch(ctx, &instance, patch); err != nil {
			return ctrl.Result{}, err
		}
		// Re-fetch so subsequent operations use the latest resourceVersion.
		if err := r.Get(ctx, req.NamespacedName, &instance); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Capture status baseline before any status mutations so the merge patch
	// includes every field we touch (channels, phase, active-agent count).
	statusBase := instance.DeepCopy()

	// Reconcile channel deployments
	if err := r.reconcileChannels(ctx, &instance); err != nil {
		log.Error(err, "failed to reconcile channels")
		instance.Status.Phase = "Error"
		_ = r.Status().Patch(ctx, &instance, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Reconcile a per-Agent ServiceAccount. Every Agent gets its own
	// identity (`<agent-name>-agent`) so the memory-server can authenticate
	// it via TokenReview and resolve trust relationships from the Agent →
	// Ensemble graph.
	if err := r.reconcileAgentServiceAccount(ctx, log, &instance); err != nil {
		log.Error(err, "failed to reconcile per-agent ServiceAccount")
		instance.Status.Phase = "Error"
		_ = r.Status().Patch(ctx, &instance, client.MergeFrom(statusBase))
		return ctrl.Result{RequeueAfter: 30 * time.Second}, err
	}

	// Reconcile web endpoint
	if err := r.reconcileWebEndpoint(ctx, &instance); err != nil {
		log.Error(err, "failed to reconcile web endpoint")
	}

	// Count active agent pods
	activeCount, hasServing, err := r.countActiveAgentPods(ctx, &instance)
	if err != nil {
		log.Error(err, "failed to count agent pods")
	}

	// Update status
	if hasServing {
		instance.Status.Phase = "Serving"
	} else {
		instance.Status.Phase = "Running"
	}
	instance.Status.ActiveAgentPods = activeCount
	if err := r.Status().Patch(ctx, &instance, client.MergeFrom(statusBase)); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 60 * time.Second}, nil
}

// reconcileChannels ensures a Deployment exists for each configured channel.
func (r *AgentReconciler) reconcileChannels(ctx context.Context, instance *sympoziumv1alpha1.Agent) error {
	channelStatuses := make([]sympoziumv1alpha1.ChannelStatus, 0, len(instance.Spec.Channels))

	if len(instance.Spec.Channels) > 0 {
		if err := r.ensureChannelServiceAccount(ctx, instance); err != nil {
			return err
		}
	}

	for _, ch := range instance.Spec.Channels {
		deployName := fmt.Sprintf("%s-channel-%s", instance.Name, ch.Type)

		// Require the referenced Secret up-front, unless a CSI volume will
		// materialize it on first mount (e.g. Vault Secrets Store CSI).
		if ch.ConfigRef.Secret != "" && !channelMountsCSI(ch) {
			var secret corev1.Secret
			if err := r.Get(ctx, types.NamespacedName{
				Name:      ch.ConfigRef.Secret,
				Namespace: instance.Namespace,
			}, &secret); err != nil {
				if errors.IsNotFound(err) {
					channelStatuses = append(channelStatuses, sympoziumv1alpha1.ChannelStatus{
						Type:    ch.Type,
						Status:  "Error",
						Message: fmt.Sprintf("secret %q not found", ch.ConfigRef.Secret),
					})
					continue
				}
				return err
			}
		}

		// WhatsApp channels need a PVC for credential persistence (QR link survives restarts)
		if ch.Type == "whatsapp" {
			if err := r.ensureWhatsAppPVC(ctx, instance, deployName); err != nil {
				return err
			}
		}

		var deploy appsv1.Deployment
		err := r.Get(ctx, types.NamespacedName{
			Name:      deployName,
			Namespace: instance.Namespace,
		}, &deploy)

		if errors.IsNotFound(err) {
			// Create channel deployment
			deploy := r.buildChannelDeployment(instance, ch, deployName)
			if err := controllerutil.SetControllerReference(instance, deploy, r.Scheme); err != nil {
				return err
			}
			if err := r.Create(ctx, deploy); err != nil {
				return err
			}
			channelStatuses = append(channelStatuses, sympoziumv1alpha1.ChannelStatus{
				Type:   ch.Type,
				Status: "Pending",
			})
		} else if err != nil {
			return err
		} else {
			// Detect and apply spec drift so edits to ChannelSpec (secret
			// rotation, slack options, triggers, access control, volumes,
			// image tag upgrades) propagate to the running channel pod
			// without manual deletion.
			desired := r.buildChannelDeployment(instance, ch, deployName)
			if channelDeploymentDrift(&deploy, desired) {
				applyChannelDeploymentSpec(&deploy, desired)
				if err := r.Update(ctx, &deploy); err != nil {
					return fmt.Errorf("update channel deployment %s: %w", deployName, err)
				}
			}
			status := "Connected"
			if deploy.Status.ReadyReplicas == 0 {
				status = "Disconnected"
			}
			channelStatuses = append(channelStatuses, sympoziumv1alpha1.ChannelStatus{
				Type:   ch.Type,
				Status: status,
			})
		}
	}

	instance.Status.Channels = channelStatuses
	return nil
}

// buildChannelDeployment creates a Deployment spec for a channel pod.
func (r *AgentReconciler) buildChannelDeployment(
	instance *sympoziumv1alpha1.Agent,
	ch sympoziumv1alpha1.ChannelSpec,
	name string,
) *appsv1.Deployment {
	replicas := int32(1)
	tag := r.ImageTag
	if tag == "" {
		tag = "latest"
	}
	registry := os.Getenv("SYMPOZIUM_IMAGE_REGISTRY")
	if registry == "" {
		registry = "ghcr.io/sympozium-ai/sympozium"
	}
	image := fmt.Sprintf("%s/channel-%s:%s", registry, ch.Type, tag)

	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: instance.Namespace,
			Labels: map[string]string{
				"sympozium.ai/component": "channel",
				"sympozium.ai/channel":   ch.Type,
				"sympozium.ai/instance":  instance.Name,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"sympozium.ai/component": "channel",
					"sympozium.ai/channel":   ch.Type,
					"sympozium.ai/instance":  instance.Name,
				},
			},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"sympozium.ai/component": "channel",
						"sympozium.ai/channel":   ch.Type,
						"sympozium.ai/instance":  instance.Name,
					},
				},
				Spec: corev1.PodSpec{
					ServiceAccountName: ChannelServiceAccountName(instance),
					Containers: []corev1.Container{
						{
							Name:            "channel",
							Image:           image,
							ImagePullPolicy: corev1.PullIfNotPresent,
							Env: []corev1.EnvVar{
								{Name: "INSTANCE_NAME", Value: instance.Name},
								{Name: "EVENT_BUS_URL", Value: "nats://nats.sympozium-system.svc:4222"},
								{Name: "OTEL_EXPORTER_OTLP_ENDPOINT", Value: resolveOTelEndpoint(instance)},
								{Name: "OTEL_EXPORTER_OTLP_PROTOCOL", Value: "grpc"},
								{Name: "OTEL_SERVICE_NAME", Value: fmt.Sprintf("sympozium-channel-%s", ch.Type)},
							},
						},
					},
				},
			},
		},
	}

	// Inject the artifact-server URL so the channel pod can download
	// agent-produced files by reference and deliver them.
	if r.ArtifactServerURL != "" {
		c := &deploy.Spec.Template.Spec.Containers[0]
		c.Env = append(c.Env, corev1.EnvVar{Name: "ARTIFACT_SERVER_URL", Value: r.ArtifactServerURL})
	}

	// Inject channel credentials from secret (if referenced)
	if ch.ConfigRef.Secret != "" {
		deploy.Spec.Template.Spec.Containers[0].EnvFrom = []corev1.EnvFromSource{
			{
				SecretRef: &corev1.SecretEnvSource{
					LocalObjectReference: corev1.LocalObjectReference{
						Name: ch.ConfigRef.Secret,
					},
				},
			},
		}
	}

	// WhatsApp channels need a persistent volume for credential storage
	if ch.Type == "whatsapp" {
		pvcName := fmt.Sprintf("%s-data", name)
		deploy.Spec.Strategy = appsv1.DeploymentStrategy{
			Type: appsv1.RecreateDeploymentStrategyType, // prevent two pods mounting the same PVC
		}
		deploy.Spec.Template.Spec.Volumes = []corev1.Volume{
			{
				Name: "whatsapp-data",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
						ClaimName: pvcName,
					},
				},
			},
		}
		deploy.Spec.Template.Spec.Containers[0].VolumeMounts = []corev1.VolumeMount{
			{
				Name:      "whatsapp-data",
				MountPath: "/data",
			},
		}
	}

	// Slack-specific configuration (threading, allowed-triggers,
	// thread stickiness, access control). Slack pod gates inbound
	// messages itself before publishing to NATS; the controller's
	// channel router still applies AccessControl as defence in depth.
	if ch.Type == "slack" {
		c := &deploy.Spec.Template.Spec.Containers[0]
		if ch.Slack != nil {
			if ch.Slack.Threading {
				c.Env = append(c.Env, corev1.EnvVar{Name: "SLACK_THREADING", Value: "true"})
			}
			if ch.Slack.ThreadStickiness {
				c.Env = append(c.Env, corev1.EnvVar{Name: "SLACK_THREAD_STICKINESS", Value: "true"})
			}
			if len(ch.Slack.AllowedTriggers) > 0 {
				c.Env = append(c.Env, corev1.EnvVar{
					Name:  "SLACK_ALLOWED_TRIGGERS",
					Value: strings.Join(ch.Slack.AllowedTriggers, ","),
				})
			}
		}
		if ch.AccessControl != nil {
			ac := ch.AccessControl
			if len(ac.AllowedSenders) > 0 {
				c.Env = append(c.Env, corev1.EnvVar{Name: "SLACK_ALLOWED_SENDERS", Value: strings.Join(ac.AllowedSenders, ",")})
			}
			if len(ac.DeniedSenders) > 0 {
				c.Env = append(c.Env, corev1.EnvVar{Name: "SLACK_DENIED_SENDERS", Value: strings.Join(ac.DeniedSenders, ",")})
			}
			if len(ac.AllowedChats) > 0 {
				c.Env = append(c.Env, corev1.EnvVar{Name: "SLACK_ALLOWED_CHATS", Value: strings.Join(ac.AllowedChats, ",")})
			}
		}
	}

	// Per-channel volumes (e.g. CSI SecretProviderClass priming the
	// configRef Secret). Applied last so per-channel volumes are appended
	// alongside any built-in volumes (e.g. WhatsApp's credential PVC).
	if len(ch.Volumes) > 0 {
		deploy.Spec.Template.Spec.Volumes = append(deploy.Spec.Template.Spec.Volumes, ch.Volumes...)
	}
	if len(ch.VolumeMounts) > 0 {
		c := &deploy.Spec.Template.Spec.Containers[0]
		c.VolumeMounts = append(c.VolumeMounts, ch.VolumeMounts...)
	}

	return deploy
}

// channelDeploymentDrift compares the user-managed fields of an existing
// channel Deployment against the desired spec. Replicas and apiserver-set
// defaults (TerminationMessagePath, etc.) are intentionally excluded so
// scaling and server-side mutation don't trigger spurious updates.
func channelDeploymentDrift(existing, desired *appsv1.Deployment) bool {
	if !reflect.DeepEqual(existing.Spec.Strategy, desired.Spec.Strategy) {
		return true
	}
	if !reflect.DeepEqual(existing.Spec.Template.Spec.Volumes, desired.Spec.Template.Spec.Volumes) {
		return true
	}
	if len(existing.Spec.Template.Spec.Containers) != 1 || len(desired.Spec.Template.Spec.Containers) != 1 {
		return true
	}
	e := existing.Spec.Template.Spec.Containers[0]
	d := desired.Spec.Template.Spec.Containers[0]
	if e.Image != d.Image {
		return true
	}
	if !reflect.DeepEqual(e.Env, d.Env) {
		return true
	}
	if !reflect.DeepEqual(e.EnvFrom, d.EnvFrom) {
		return true
	}
	if !reflect.DeepEqual(e.VolumeMounts, d.VolumeMounts) {
		return true
	}
	return false
}

// applyChannelDeploymentSpec copies the user-managed fields from desired
// into existing in-place. Mirrors channelDeploymentDrift's field list.
func applyChannelDeploymentSpec(existing, desired *appsv1.Deployment) {
	existing.Spec.Strategy = desired.Spec.Strategy
	existing.Spec.Template.Spec.Volumes = desired.Spec.Template.Spec.Volumes
	if len(existing.Spec.Template.Spec.Containers) > 0 && len(desired.Spec.Template.Spec.Containers) > 0 {
		existing.Spec.Template.Spec.Containers[0].Image = desired.Spec.Template.Spec.Containers[0].Image
		existing.Spec.Template.Spec.Containers[0].Env = desired.Spec.Template.Spec.Containers[0].Env
		existing.Spec.Template.Spec.Containers[0].EnvFrom = desired.Spec.Template.Spec.Containers[0].EnvFrom
		existing.Spec.Template.Spec.Containers[0].VolumeMounts = desired.Spec.Template.Spec.Containers[0].VolumeMounts
	}
}

// ensureWhatsAppPVC creates a PVC for the WhatsApp credential store if it doesn't exist.
func (r *AgentReconciler) ensureWhatsAppPVC(ctx context.Context, instance *sympoziumv1alpha1.Agent, deployName string) error {
	pvcName := fmt.Sprintf("%s-data", deployName)
	var pvc corev1.PersistentVolumeClaim
	err := r.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: instance.Namespace}, &pvc)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return err
	}

	pvc = corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: instance.Namespace,
			Labels: map[string]string{
				"sympozium.ai/component": "channel",
				"sympozium.ai/channel":   "whatsapp",
				"sympozium.ai/instance":  instance.Name,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("256Mi"),
				},
			},
		},
	}

	if err := controllerutil.SetControllerReference(instance, &pvc, r.Scheme); err != nil {
		return err
	}

	r.Log.Info("Creating WhatsApp credential PVC", "name", pvcName)
	return r.Create(ctx, &pvc)
}

// channelMountsCSI reports whether the channel pod mounts any CSI volume.
// When true the controller skips the configRef Secret existence check because
// the Secret is expected to be materialized by the CSI driver (e.g. Vault
// Secrets Store CSI) on first pod mount. This is intentionally broad: any CSI
// volume triggers the bypass, since channel pods with CSI are overwhelmingly
// used for secret injection.
func channelMountsCSI(ch sympoziumv1alpha1.ChannelSpec) bool {
	for _, v := range ch.Volumes {
		if v.CSI != nil {
			return true
		}
	}
	return false
}

// ensureChannelServiceAccount creates the per-Agent channel ServiceAccount
// if it does not already exist. Channel Deployments reference this SA so
// workloads like Vault Secrets Store CSI can authenticate via the pod's SA
// token, and so operators can write per-Agent Vault policies. The SA is
// owner-referenced by the Agent so it is garbage-collected on Agent deletion.
func (r *AgentReconciler) ensureChannelServiceAccount(ctx context.Context, instance *sympoziumv1alpha1.Agent) error {
	saName := ChannelServiceAccountName(instance)
	sa := &corev1.ServiceAccount{}
	err := r.Get(ctx, client.ObjectKey{Name: saName, Namespace: instance.Namespace}, sa)
	if err == nil {
		return nil // already exists
	}
	if !errors.IsNotFound(err) {
		return fmt.Errorf("checking for channel service account: %w", err)
	}
	sa = &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: instance.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "sympozium",
				"sympozium.ai/component":       "channel-identity",
				"sympozium.ai/instance":        instance.Name,
			},
		},
	}
	if err := controllerutil.SetControllerReference(instance, sa, r.Scheme); err != nil {
		return fmt.Errorf("setting owner reference on channel SA: %w", err)
	}
	if err := r.Create(ctx, sa); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating channel service account: %w", err)
	}
	return nil
}

// cleanupChannelDeployments removes channel deployments owned by the instance.
func (r *AgentReconciler) cleanupChannelDeployments(ctx context.Context, instance *sympoziumv1alpha1.Agent) error {
	var deploys appsv1.DeploymentList
	if err := r.List(ctx, &deploys,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels{"sympozium.ai/instance": instance.Name, "sympozium.ai/component": "channel"},
	); err != nil {
		return err
	}

	for i := range deploys.Items {
		if err := r.Delete(ctx, &deploys.Items[i]); err != nil && !errors.IsNotFound(err) {
			return err
		}
	}

	return nil
}

// countActiveAgentPods counts running agent pods for this instance.
func (r *AgentReconciler) countActiveAgentPods(ctx context.Context, instance *sympoziumv1alpha1.Agent) (int, bool, error) {
	var runs sympoziumv1alpha1.AgentRunList
	if err := r.List(ctx, &runs,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels{"sympozium.ai/instance": instance.Name},
	); err != nil {
		return 0, false, err
	}

	count := 0
	hasServing := false
	for _, run := range runs.Items {
		if run.Status.Phase == sympoziumv1alpha1.AgentRunPhaseRunning ||
			run.Status.Phase == sympoziumv1alpha1.AgentRunPhaseServing {
			count++
		}
		if run.Status.Phase == sympoziumv1alpha1.AgentRunPhaseServing {
			hasServing = true
		}
	}
	return count, hasServing, nil
}

// AgentServiceAccountName returns the per-Agent ServiceAccount name used by
// agent pods. The "-agent" suffix prevents collisions with arbitrary user
// SAs in the namespace and makes the binding obvious to operators.
func AgentServiceAccountName(agent *sympoziumv1alpha1.Agent) string {
	return agent.Name + "-agent"
}

// reconcileAgentServiceAccount ensures a per-Agent ServiceAccount exists so
// agent pods run under a distinct identity. The memory-server uses the SA's
// default projected token (TokenReview server-side) to authenticate every
// memory request and derive trust relationships from the Agent → Ensemble
// graph. The SA is owner-referenced by the Agent so it is garbage-collected
// on Agent deletion.
func (r *AgentReconciler) reconcileAgentServiceAccount(ctx context.Context, log logr.Logger, instance *sympoziumv1alpha1.Agent) error {
	saName := AgentServiceAccountName(instance)

	var existing corev1.ServiceAccount
	err := r.Get(ctx, types.NamespacedName{Name: saName, Namespace: instance.Namespace}, &existing)
	if err == nil {
		return nil
	}
	if !errors.IsNotFound(err) {
		return err
	}

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: instance.Namespace,
			Labels: map[string]string{
				"sympozium.ai/instance":  instance.Name,
				"sympozium.ai/component": "agent-identity",
			},
		},
	}
	if err := controllerutil.SetControllerReference(instance, sa, r.Scheme); err != nil {
		return err
	}
	log.Info("Creating per-Agent ServiceAccount", "name", saName)
	return r.Create(ctx, sa)
}

// reconcileWebEndpoint ensures a server-mode AgentRun exists when the
// "web-endpoint" skill is present. The AgentRun controller handles creating
// the actual Deployment + Service.
func (r *AgentReconciler) reconcileWebEndpoint(ctx context.Context, instance *sympoziumv1alpha1.Agent) error {
	for _, skill := range instance.Spec.Skills {
		if skill.SkillPackRef == "web-endpoint" || skill.SkillPackRef == "skillpack-web-endpoint" {
			return r.ensureWebEndpointAgentRun(ctx, instance, skill)
		}
	}
	return nil
}

// ensureWebEndpointAgentRun checks for an existing server-mode AgentRun for
// this instance and creates one if none exists. The AgentRun controller will
// detect the RequiresServer sidecar and create a Deployment + Service.
func (r *AgentReconciler) ensureWebEndpointAgentRun(ctx context.Context, instance *sympoziumv1alpha1.Agent, webSkill sympoziumv1alpha1.SkillRef) error {
	// Check if a serving AgentRun already exists for this instance.
	var runs sympoziumv1alpha1.AgentRunList
	if err := r.List(ctx, &runs,
		client.InNamespace(instance.Namespace),
		client.MatchingLabels{
			"sympozium.ai/instance":  instance.Name,
			"sympozium.ai/component": "web-endpoint",
		},
	); err != nil {
		return fmt.Errorf("list web-endpoint agent runs: %w", err)
	}

	for _, run := range runs.Items {
		if run.DeletionTimestamp.IsZero() {
			return nil
		}
	}

	// No existing run — create one.
	runName := fmt.Sprintf("%s-web-endpoint", instance.Name)

	agentRun := &sympoziumv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runName,
			Namespace: instance.Namespace,
			Labels: map[string]string{
				"sympozium.ai/instance":  instance.Name,
				"sympozium.ai/component": "web-endpoint",
			},
		},
		Spec: sympoziumv1alpha1.AgentRunSpec{
			AgentRef:   instance.Name,
			AgentID:    "web-endpoint",
			SessionKey: sessionkey.ForWebEndpoint(instance.Name),
			Task:       "Serve HTTP requests for this instance",
			Mode:       "server",
			Model: sympoziumv1alpha1.ModelSpec{
				Provider:                 resolveProvider(instance),
				Model:                    instance.Spec.Agents.Default.Model,
				AuthSecretRef:            resolveAuthSecret(instance),
				ProviderHeaders:          instance.Spec.Agents.Default.ProviderHeaders,
				ProviderHeadersSecretRef: instance.Spec.Agents.Default.ProviderHeadersSecretRef,
				Thinking:                 instance.Spec.Agents.Default.Thinking,
				MaxTokens:                instance.Spec.Agents.Default.MaxTokens,
				Temperature:              instance.Spec.Agents.Default.Temperature,
			},
			Skills:           []sympoziumv1alpha1.SkillRef{webSkill},
			ImagePullSecrets: instance.Spec.ImagePullSecrets,
			Volumes:          instance.Spec.Volumes,
			VolumeMounts:     instance.Spec.VolumeMounts,
			Env:              instance.Spec.Agents.Default.Env,
			Workspace:        instance.Spec.Workspace,
			Harness:          instance.Spec.Harness,
		},
	}

	if instance.Spec.Agents.Default.BaseURL != "" {
		agentRun.Spec.Model.BaseURL = instance.Spec.Agents.Default.BaseURL
	}
	if len(instance.Spec.Agents.Default.NodeSelector) > 0 {
		agentRun.Spec.Model.NodeSelector = instance.Spec.Agents.Default.NodeSelector
	}
	if len(instance.Spec.Agents.Default.Tolerations) > 0 {
		agentRun.Spec.Tolerations = instance.Spec.Agents.Default.Tolerations
	}

	if err := controllerutil.SetControllerReference(instance, agentRun, r.Scheme); err != nil {
		return fmt.Errorf("set owner reference: %w", err)
	}

	if err := r.Create(ctx, agentRun); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("create web-endpoint AgentRun: %w", err)
	}

	r.Log.Info("created web-endpoint AgentRun", "instance", instance.Name, "run", runName)
	return nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&sympoziumv1alpha1.Agent{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Complete(r)
}
