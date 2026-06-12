package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
)

// TestReconcileChannels_PropagatesConfigRefChange guards the historical bug
// where editing a ChannelSpec (e.g. rotating to a new bot-token secret) did
// not propagate to the running channel Deployment because the reconciler
// only ever Created and never Updated.
func TestReconcileChannels_PropagatesConfigRefChange(t *testing.T) {
	oldSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "old-secret", Namespace: "ns"},
	}
	newSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "new-secret", Namespace: "ns"},
	}
	instance := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"},
		Spec: sympoziumv1alpha1.AgentSpec{
			Channels: []sympoziumv1alpha1.ChannelSpec{
				{Type: "slack", ConfigRef: sympoziumv1alpha1.SecretRef{Secret: "old-secret"}},
			},
		},
	}

	r, cl := newChannelTestReconciler(t, instance, oldSecret, newSecret)
	ctx := context.Background()

	if err := r.reconcileChannels(ctx, instance); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	// Flip the configRef to point at the rotated secret.
	instance.Spec.Channels[0].ConfigRef.Secret = "new-secret"
	if err := r.reconcileChannels(ctx, instance); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var deploy appsv1.Deployment
	if err := cl.Get(ctx, types.NamespacedName{Name: "inst-channel-slack", Namespace: "ns"}, &deploy); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	if len(deploy.Spec.Template.Spec.Containers[0].EnvFrom) != 1 {
		t.Fatalf("expected 1 envFrom entry, got %+v", deploy.Spec.Template.Spec.Containers[0].EnvFrom)
	}
	if got := deploy.Spec.Template.Spec.Containers[0].EnvFrom[0].SecretRef.Name; got != "new-secret" {
		t.Errorf("envFrom secret = %q, want new-secret", got)
	}
}

// TestReconcileChannels_PropagatesSlackOptions covers the case where editing
// SlackOptions (threading, allowed triggers) on an existing instance must
// reach the running pod via env vars.
func TestReconcileChannels_PropagatesSlackOptions(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "slack-secret", Namespace: "ns"},
	}
	instance := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"},
		Spec: sympoziumv1alpha1.AgentSpec{
			Channels: []sympoziumv1alpha1.ChannelSpec{
				{Type: "slack", ConfigRef: sympoziumv1alpha1.SecretRef{Secret: "slack-secret"}},
			},
		},
	}

	r, cl := newChannelTestReconciler(t, instance, secret)
	ctx := context.Background()

	if err := r.reconcileChannels(ctx, instance); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}

	instance.Spec.Channels[0].Slack = &sympoziumv1alpha1.SlackChannelOptions{
		Threading:       true,
		AllowedTriggers: []string{"!ask", "!help"},
	}
	if err := r.reconcileChannels(ctx, instance); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}

	var deploy appsv1.Deployment
	if err := cl.Get(ctx, types.NamespacedName{Name: "inst-channel-slack", Namespace: "ns"}, &deploy); err != nil {
		t.Fatalf("get deployment: %v", err)
	}
	envByName := map[string]string{}
	for _, e := range deploy.Spec.Template.Spec.Containers[0].Env {
		envByName[e.Name] = e.Value
	}
	if envByName["SLACK_THREADING"] != "true" {
		t.Errorf("SLACK_THREADING = %q, want true", envByName["SLACK_THREADING"])
	}
	if envByName["SLACK_ALLOWED_TRIGGERS"] != "!ask,!help" {
		t.Errorf("SLACK_ALLOWED_TRIGGERS = %q, want \"!ask,!help\"", envByName["SLACK_ALLOWED_TRIGGERS"])
	}
}

// TestReconcileChannels_NoSpuriousUpdate ensures we don't churn the deployment
// on every reconcile when nothing has changed — important because we poll
// every 60s.
func TestReconcileChannels_NoSpuriousUpdate(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "secret", Namespace: "ns"},
	}
	instance := &sympoziumv1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "inst", Namespace: "ns"},
		Spec: sympoziumv1alpha1.AgentSpec{
			Channels: []sympoziumv1alpha1.ChannelSpec{
				{Type: "slack", ConfigRef: sympoziumv1alpha1.SecretRef{Secret: "secret"}},
			},
		},
	}

	r, cl := newChannelTestReconciler(t, instance, secret)
	ctx := context.Background()

	if err := r.reconcileChannels(ctx, instance); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var first appsv1.Deployment
	if err := cl.Get(ctx, types.NamespacedName{Name: "inst-channel-slack", Namespace: "ns"}, &first); err != nil {
		t.Fatalf("get first: %v", err)
	}

	if err := r.reconcileChannels(ctx, instance); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	var second appsv1.Deployment
	if err := cl.Get(ctx, types.NamespacedName{Name: "inst-channel-slack", Namespace: "ns"}, &second); err != nil {
		t.Fatalf("get second: %v", err)
	}

	if first.ResourceVersion != second.ResourceVersion {
		t.Errorf("deployment was updated unnecessarily (rv %s -> %s)", first.ResourceVersion, second.ResourceVersion)
	}
}

// TestChannelDeploymentDrift_DetectsImageChange guards the helper directly:
// an image-tag bump (e.g. controller upgrade) must be detected as drift.
func TestChannelDeploymentDrift_DetectsImageChange(t *testing.T) {
	existing := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: "old:v1"}},
				},
			},
		},
	}
	desired := &appsv1.Deployment{
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Image: "new:v2"}},
				},
			},
		},
	}
	if !channelDeploymentDrift(existing, desired) {
		t.Error("expected drift to be detected for image change")
	}
}

func TestChannelDeploymentDrift_DetectsEnvChange(t *testing.T) {
	mk := func(env ...corev1.EnvVar) *appsv1.Deployment {
		return &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{Image: "img:v1", Env: env}},
					},
				},
			},
		}
	}
	existing := mk(corev1.EnvVar{Name: "A", Value: "1"})
	desired := mk(corev1.EnvVar{Name: "A", Value: "1"}, corev1.EnvVar{Name: "B", Value: "2"})
	if !channelDeploymentDrift(existing, desired) {
		t.Error("expected drift when desired has additional env var")
	}
}

func TestChannelDeploymentDrift_NoDriftWhenIdentical(t *testing.T) {
	mk := func() *appsv1.Deployment {
		return &appsv1.Deployment{
			Spec: appsv1.DeploymentSpec{
				Template: corev1.PodTemplateSpec{
					Spec: corev1.PodSpec{
						Containers: []corev1.Container{{
							Image: "img:v1",
							Env:   []corev1.EnvVar{{Name: "A", Value: "1"}},
						}},
					},
				},
			},
		}
	}
	if channelDeploymentDrift(mk(), mk()) {
		t.Error("expected no drift for identical deployments")
	}
}
