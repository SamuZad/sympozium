package cellninstall

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestKVMNodesSplitsByLabelValue(t *testing.T) {
	store := fake.NewClientBuilder().WithObjects(
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-b", Labels: map[string]string{KVMNodeLabel: "true"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "worker-a", Labels: map[string]string{KVMNodeLabel: "true"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "drained", Labels: map[string]string{KVMNodeLabel: "false"}}},
		&corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "control-plane"}},
	).Build()
	labelled, unlabelled, err := KVMNodes(context.Background(), store)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(labelled, ",") != "worker-a,worker-b" {
		t.Fatalf("labelled = %v", labelled)
	}
	if strings.Join(unlabelled, ",") != "control-plane,drained" {
		t.Fatalf("unlabelled = %v (an explicit false is not a fleet node)", unlabelled)
	}
}

func TestNoKVMNodeHintNamesTheCauseAndTheNodes(t *testing.T) {
	hint := NoKVMNodeHint([]string{"kind-control-plane"})
	for _, want := range []string{KVMNodeLabel + "=true", "/dev/kvm", "/boot", "Kind", "docker cp /boot/vmlinuz-$(uname -r)", "kubectl label node", "kind-control-plane"} {
		if !strings.Contains(hint, want) {
			t.Fatalf("hint lacks %q:\n%s", want, hint)
		}
	}
	if strings.Contains(NoKVMNodeHint(nil), "Nodes without the label") {
		t.Fatal("an empty node list must not print an empty listing")
	}
}
