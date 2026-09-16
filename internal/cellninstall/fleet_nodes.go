package cellninstall

import (
	"context"
	"sort"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// KVMNodeLabel marks a node that may run cells; the node probe sets it to
// "true" on nodes with /dev/kvm and a kernel under /boot, and never changes
// a value an operator set.
const KVMNodeLabel = "celln.dev/kvm"

// KVMNodes splits the cluster's nodes into those carrying the fleet label
// with the value true and the rest, both sorted by name. The installer uses
// it to say why a fleet wait is not progressing: with no labelled node the
// DaemonSets have nowhere to run.
func KVMNodes(ctx context.Context, store client.Client) (labelled, unlabelled []string, err error) {
	var nodes corev1.NodeList
	if err := store.List(ctx, &nodes); err != nil {
		return nil, nil, err
	}
	for _, n := range nodes.Items {
		if n.Labels[KVMNodeLabel] == "true" {
			labelled = append(labelled, n.Name)
		} else {
			unlabelled = append(unlabelled, n.Name)
		}
	}
	sort.Strings(labelled)
	sort.Strings(unlabelled)
	return labelled, unlabelled, nil
}

// NoKVMNodeHint explains an empty fleet to an operator: what the probe looks
// for, the Kind case (its nodes ship without a kernel), and the manual label.
func NoKVMNodeHint(unlabelled []string) string {
	hint := "  No node carries " + KVMNodeLabel + "=true yet, so the fleet DaemonSets have nowhere to run. The node probe labels a node that has /dev/kvm and a kernel under /boot.\n" +
		"  Kind nodes ship without a kernel (Kind is a development environment only): copy the host's in and the probe labels the node within seconds:\n" +
		"    docker cp /boot/vmlinuz-$(uname -r) <kind-node>:/boot/\n" +
		"  A node you know can run cells can be labelled by hand: kubectl label node <name> " + KVMNodeLabel + "=true\n"
	if len(unlabelled) != 0 {
		hint += "  Nodes without the label:"
		for _, n := range unlabelled {
			hint += " " + n
		}
		hint += "\n"
	}
	return hint
}
