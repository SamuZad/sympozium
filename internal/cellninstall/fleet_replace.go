package cellninstall

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Annotations on the published fleet configuration. The package and scope
// record what the nodes published; the desired pair is the installer's
// approval that the nodes may replace it (charts/sympozium/files/celln/
// fleet-publish.py reads them).
const (
	FleetScopeAnnotation          = "celln.sympozium.ai/scope"
	FleetDesiredPackageAnnotation = "celln.sympozium.ai/desired-package"
	FleetDesiredScopeAnnotation   = "celln.sympozium.ai/desired-scope"
)

// FleetPublication is the package and scope the fleet's nodes published.
type FleetPublication struct {
	Exists         bool
	Package, Scope string
}

// ReadFleetPublication reads what the nodes published. A configuration
// published before scopes were recorded takes its scope from the configure
// DaemonSet that published it.
func ReadFleetPublication(ctx context.Context, store client.Reader) (FleetPublication, error) {
	var published corev1.ConfigMap
	if err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetConfigurationConfigMap}, &published); apierrors.IsNotFound(err) {
		return FleetPublication{}, nil
	} else if err != nil {
		return FleetPublication{}, err
	}
	p := FleetPublication{Exists: true, Package: published.Annotations[packageAnnotation], Scope: published.Annotations[FleetScopeAnnotation]}
	if p.Scope == "" {
		if facts, err := ReadFleetFacts(ctx, store); err == nil {
			p.Scope = facts.Scope
		}
	}
	return p, nil
}

// Replaces reports whether installing package in scope would replace what
// the nodes published: a different package, or a different scope.
func (p FleetPublication) Replaces(scope, packageHash string) bool {
	return p.Exists && (p.Package != packageHash || (p.Scope != "" && p.Scope != scope))
}

// ReplacementRefusal explains a replacement the operator has not approved.
func (p FleetPublication) ReplacementRefusal(scope, packageHash string) error {
	return fmt.Errorf("the Celln fleet runs package %s in scope %s, and this install would move it to package %s in scope %s.\n"+
		"  Moving a scope ends every live parent on the fleet. To go ahead, rerun with --celln-fleet-replace-package.\n"+
		"  To keep the installed package instead, pin it with --celln-fleet-package-image, --celln-fleet-package-hash %s and --celln-fleet-publisher (from the release that installed it) and --celln-fleet-scope %s",
		p.Package, orUnrecorded(p.Scope), packageHash, scope, p.Package, orUnrecorded(p.Scope))
}

func orUnrecorded(s string) string {
	if s == "" {
		return "(unrecorded)"
	}
	return s
}

// ApproveFleetReplacement records that the nodes may replace the published
// configuration with packageHash in scope. It also records the published
// scope when the configuration predates that annotation, so a node still on
// the old package keeps recognising its own publication.
func ApproveFleetReplacement(ctx context.Context, store client.Client, current FleetPublication, scope, packageHash string) error {
	var published corev1.ConfigMap
	if err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetConfigurationConfigMap}, &published); err != nil {
		return err
	}
	patch := client.MergeFrom(published.DeepCopy())
	if published.Annotations == nil {
		published.Annotations = map[string]string{}
	}
	if published.Annotations[FleetScopeAnnotation] == "" && current.Scope != "" {
		published.Annotations[FleetScopeAnnotation] = current.Scope
	}
	published.Annotations[FleetDesiredPackageAnnotation] = packageHash
	published.Annotations[FleetDesiredScopeAnnotation] = scope
	return store.Patch(ctx, &published, patch)
}
