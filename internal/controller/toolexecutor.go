package controller

import (
	"context"
	_ "embed"
	"fmt"

	"github.com/go-logr/logr"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The generic tool-executor loop is embedded in the controller and shipped
// to agent namespaces as a ConfigMap. It is mounted (mode 0755) into every
// skill sidecar container, so any stock image with bash, jq, and coreutils
// (e.g. bitnami/kubectl) can serve as a skill sidecar without a specialized
// Sympozium image.
//
//go:embed assets/tool-executor.sh
var toolExecutorScript string

const (
	// toolExecutorConfigMapName is the per-namespace ConfigMap holding the
	// embedded tool-executor script.
	toolExecutorConfigMapName = "sympozium-tool-executor"

	// toolExecutorScriptKey is the ConfigMap data key / mounted filename.
	toolExecutorScriptKey = "tool-executor.sh"

	// toolExecutorVolumeName is the reserved pod volume name for the mount.
	toolExecutorVolumeName = "tool-executor"

	// toolExecutorMountPath is where the script directory is mounted inside
	// skill sidecar containers.
	toolExecutorMountPath = "/sympozium/bin"

	// ToolExecutorScriptPath is the full path of the mounted script. Used as
	// the default sidecar command when a SkillPack does not specify one.
	ToolExecutorScriptPath = toolExecutorMountPath + "/" + toolExecutorScriptKey
)

// ensureToolExecutorConfigMap creates or updates the tool-executor ConfigMap
// in the given namespace so skill sidecars can mount the executor script.
// The ConfigMap is shared by all AgentRuns in the namespace, so it carries no
// owner reference; data is reconciled on every call so controller upgrades
// roll out script changes automatically.
func (r *AgentRunReconciler) ensureToolExecutorConfigMap(ctx context.Context, log logr.Logger, namespace string) error {
	desired := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      toolExecutorConfigMapName,
			Namespace: namespace,
			Labels: map[string]string{
				"sympozium.ai/component":  "tool-executor",
				"sympozium.ai/managed-by": "sympozium",
			},
		},
		Data: map[string]string{
			toolExecutorScriptKey: toolExecutorScript,
		},
	}

	existing := &corev1.ConfigMap{}
	err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: toolExecutorConfigMapName}, existing)
	if err != nil {
		if !errors.IsNotFound(err) {
			return fmt.Errorf("getting tool-executor ConfigMap: %w", err)
		}
		if err := r.Create(ctx, desired); err != nil && !errors.IsAlreadyExists(err) {
			return fmt.Errorf("creating tool-executor ConfigMap: %w", err)
		}
		log.V(1).Info("Created tool-executor ConfigMap", "namespace", namespace)
		return nil
	}

	if existing.Data[toolExecutorScriptKey] != toolExecutorScript {
		existing.Data = desired.Data
		if err := r.Update(ctx, existing); err != nil {
			return fmt.Errorf("updating tool-executor ConfigMap: %w", err)
		}
		log.V(1).Info("Updated tool-executor ConfigMap", "namespace", namespace)
	}
	return nil
}
