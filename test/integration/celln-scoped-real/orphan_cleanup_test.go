package main

import (
	"context"
	"encoding/json"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"os"
	"path/filepath"
	"reflect"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"testing"
	"time"
)

// Operator-only cleanup of exact immutable records whose independent durable
// receiver outcome confirms teardown. No absent-row inference or finalizer edits.
func TestCleanupConfirmedReviewRecords(t *testing.T) {
	base, root := os.Getenv("CELLN_REVIEW_STATE"), os.Getenv("CELLN_REVIEW_NATIVE_ROOT")
	if base == "" || root == "" {
		t.Skip("explicit review state required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, _, _, err := liveClient(options{kubeconfig: filepath.Join(base, "kubeconfig"), contextName: "kubernetes-admin@kubernetes"})
	if err != nil {
		t.Fatal(err)
	}
	files, err := os.ReadDir(filepath.Join(root, "scoped/prepared"))
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		var prepared struct {
			ID, Owner           string
			Operation, Decision map[string]any
		}
		raw, err := os.ReadFile(filepath.Join(root, "scoped/prepared", file.Name()))
		if err != nil || json.Unmarshal(raw, &prepared) != nil {
			t.Fatal("invalid independent enrollment")
		}
		var status struct {
			ID, Owner        string
			CleanupConfirmed bool
		}
		raw, err = os.ReadFile(filepath.Join(root, "scoped/status", file.Name()))
		if err != nil || json.Unmarshal(raw, &status) != nil || !status.CleanupConfirmed || status.ID != prepared.ID || status.Owner != prepared.Owner {
			t.Fatal("native cleanup unconfirmed")
		}
		if os.Getenv("CELLN_REVIEW_CLEANUP_RESOURCES") == "1" {
			var binding struct {
				Resolution struct {
					ReadSet []struct{ Kind, Namespace, Name, UID string }
				}
			}
			raw, _ := json.Marshal(prepared.Operation)
			if json.Unmarshal(raw, &binding) != nil {
				t.Fatal("invalid original read set")
			}
			for _, ref := range binding.Resolution.ReadSet {
				switch ref.Kind {
				case "Agent", "AgentRuntime", "CellnExecutionPolicy", "CellnRuntimeProfile", "ClusterCellnTool", "ModelConnection":
				default:
					continue
				}
				if ref.Namespace != "" && ref.Namespace != "celln-review-a-495" && ref.Namespace != "celln-review-b-495" {
					t.Fatal("not a review object")
				}
				obj := &unstructured.Unstructured{}
				obj.SetGroupVersionKind(schema.GroupVersionKind{Group: "sympozium.ai", Version: "v1alpha1", Kind: ref.Kind})
				obj.SetName(ref.Name)
				obj.SetNamespace(ref.Namespace)
				uid := types.UID(ref.UID)
				if uid == "" {
					t.Fatal("missing independent object UID")
				}
				if err := client.IgnoreNotFound(c.Delete(ctx, obj, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})); err != nil {
					t.Fatal(err)
				}
			}
			var decision struct {
				Run   struct{ Namespace string }
				Route struct {
					CredentialSource *struct{ SecretName, SecretUID string }
				}
			}
			raw, _ = json.Marshal(prepared.Decision)
			if json.Unmarshal(raw, &decision) != nil {
				t.Fatal("invalid pinned credential identity")
			}
			if ref := decision.Route.CredentialSource; ref != nil {
				if decision.Run.Namespace != "celln-review-a-495" && decision.Run.Namespace != "celln-review-b-495" {
					t.Fatal("not a review namespace")
				}
				uid := types.UID(ref.SecretUID)
				if uid == "" {
					t.Fatal("missing original Secret UID")
				}
				obj := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: ref.SecretName, Namespace: decision.Run.Namespace}}
				if err := client.IgnoreNotFound(c.Delete(ctx, obj, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}})); err != nil {
					t.Fatal(err)
				}
			}
		}
		var records corev1.ConfigMapList
		if err := c.List(ctx, &records, client.InNamespace("celln-review-system-495")); err != nil {
			t.Fatal(err)
		}
		for i := range records.Items {
			cm := &records.Items[i]
			if cm.Immutable == nil || !*cm.Immutable {
				continue
			}
			var value map[string]any
			matching := (json.Unmarshal([]byte(cm.Data["operation.json"]), &value) == nil && reflect.DeepEqual(value, prepared.Operation))
			value = nil
			matching = matching || (json.Unmarshal([]byte(cm.Data["decision.json"]), &value) == nil && reflect.DeepEqual(value, prepared.Decision))
			if !matching {
				continue
			}
			uid := cm.UID
			if err := c.Delete(ctx, cm, &client.DeleteOptions{Preconditions: &metav1.Preconditions{UID: &uid}}); err != nil {
				t.Fatal(err)
			}
			t.Log("removed exact confirmed-cleanup protected record", cm.Name)
		}
	}
}
