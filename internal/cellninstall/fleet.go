package cellninstall

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/sympozium-ai/sympozium/internal/cellnparent"
	"github.com/zeebo/blake3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Names shared between the chart's fleet mode and the installer.
const (
	FleetParentTokenSecret      = "celln-router-parent"
	FleetParentClientsConfigMap = "celln-fleet-parent-clients"
	FleetConfigurationConfigMap = "celln-fleet-configuration"
	FleetModelCredentialSecret  = "celln-fleet-model-credential"
	FleetParentConfigSecret     = "celln-parent-config"
	// FleetJournalRoot is the controller's claim: journal/ and approvals/ live
	// here instead of on an owner node.
	FleetJournalRoot = "/var/lib/sympozium/celln-parent"

	fleetNamespace           = "celln-system"
	fleetControllerTokenFile = "/etc/sympozium/celln/token"
)

var (
	scopePattern       = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	digestImagePattern = regexp.MustCompile(`^[^@:\s,]+(:[0-9]+)?(/[^@\s,]+)?@sha256:[a-f0-9]{64}$`)
	blake3Pattern      = regexp.MustCompile(`^blake3:[a-f0-9]{64}$`)
)

// FleetOptions describes one per-node native fleet: every node labeled
// celln.dev/kvm=true prepares the same reviewed package under
// /var/lib/sympozium-celln/<scope> and serves parents from it.
type FleetOptions struct {
	Scope, Principal, Publisher string
	PackageImage, PackageHash   string
	ModelCredentialPath         string
}

// StatePath is the per-node authority location the chart derives from Scope.
func (o FleetOptions) StatePath() string { return "/var/lib/sympozium-celln/" + o.Scope }

func (o FleetOptions) validate() error {
	identities := o.Principal + o.Publisher
	if !scopePattern.MatchString(o.Scope) || !digestImagePattern.MatchString(o.PackageImage) || !blake3Pattern.MatchString(o.PackageHash) ||
		o.Principal == "" || o.Publisher == "" || strings.ContainsAny(identities, " \t\r\n,=") ||
		!filepath.IsAbs(o.ModelCredentialPath) || filepath.Clean(o.ModelCredentialPath) != o.ModelCredentialPath || filepath.Dir(o.ModelCredentialPath) == "/" || strings.ContainsAny(o.ModelCredentialPath, ",=") {
		return fmt.Errorf("fleet requires a DNS-label scope, a digest-pinned package image, a blake3 package hash, a publisher, a principal and a clean absolute model credential path in a dedicated directory")
	}
	return nil
}

// FleetValues renders the node-preparation phase. The controller is wired by
// ConfigureFleet only after the nodes have published the starter configuration.
func FleetValues(o FleetOptions) ([]string, error) {
	if err := o.validate(); err != nil {
		return nil, err
	}
	return []string{
		"celln.dispatcher.enabled=false",
		"celln.router.backends=null",
		"celln.router.parentTokenSecret=" + FleetParentTokenSecret,
		"celln.fleet.enabled=true",
		"celln.fleet.scope=" + o.Scope,
		"celln.fleet.package.image=" + o.PackageImage,
		"celln.fleet.package.hash=" + o.PackageHash,
		"celln.fleet.publisher=" + o.Publisher,
		"celln.fleet.principal=" + o.Principal,
		"celln.fleet.parentClientsConfigMap=" + FleetParentClientsConfigMap,
		"celln.fleet.configurationConfigMap=" + FleetConfigurationConfigMap,
		"celln.fleet.modelCredential.secret=" + FleetModelCredentialSecret,
		"celln.fleet.modelCredential.path=" + o.ModelCredentialPath,
	}, nil
}

// PrepareFleetTrust publishes the shared parent principal: the gateway's
// bearer credential and the hash-only client policy every owner installs. An
// existing pair is accepted only when the policy still matches the credential;
// a live principal is never replaced.
func PrepareFleetTrust(ctx context.Context, store client.Client, principal string) error {
	if principal == "" || strings.ContainsAny(principal, " \t\r\n") {
		return fmt.Errorf("bounded parent principal required")
	}
	var secret corev1.Secret
	err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetParentTokenSecret}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		raw := make([]byte, 32)
		if _, err := rand.Read(raw); err != nil {
			return err
		}
		token := base64.RawURLEncoding.EncodeToString(raw)
		policy, err := parentClientPolicy(principal, token)
		if err != nil {
			return err
		}
		for _, object := range []client.Object{
			&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: FleetParentTokenSecret, Namespace: fleetNamespace, Labels: fleetLabels()}, Data: map[string][]byte{"token": []byte(token)}},
			&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: FleetParentClientsConfigMap, Namespace: fleetNamespace, Labels: fleetLabels()}, Data: map[string]string{"trusted-parent-clients.json": policy}},
		} {
			if err := store.Create(ctx, object); err != nil {
				return fmt.Errorf("publish %s: %w; partial fleet trust retained, never replaced", object.GetName(), err)
			}
		}
		return nil
	case err != nil:
		return err
	}
	var policy corev1.ConfigMap
	if err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetParentClientsConfigMap}, &policy); err != nil {
		return fmt.Errorf("fleet credential exists without its client policy; reconcile %s manually", FleetParentClientsConfigMap)
	}
	expected, err := parentClientPolicy(principal, string(secret.Data["token"]))
	if err != nil || policy.Data["trusted-parent-clients.json"] != expected {
		return fmt.Errorf("existing fleet client policy does not match the gateway credential and principal; never rotate by replacement")
	}
	return nil
}

func parentClientPolicy(principal, token string) (string, error) {
	if len(token) < 24 || len(token) > 4096 || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("invalid parent credential")
	}
	raw, err := json.Marshal(map[string]any{"apiVersion": "celln.parent-clients/v1", "clients": []map[string]string{{"principal": principal, "tokenHash": fmt.Sprintf("blake3:%x", blake3.Sum256([]byte(token)))}}})
	return string(raw), err
}

func fleetLabels() map[string]string {
	return map[string]string{"app.kubernetes.io/part-of": "sympozium"}
}

// PublishFleetModelCredential copies one operator credential file into the
// Secret every dispatcher mounts at the model profile's path. The controller
// and guests never read it. An existing Secret is kept when the file is
// omitted or identical; it is never rotated by replacement.
func PublishFleetModelCredential(ctx context.Context, store client.Client, path string) error {
	var existing corev1.Secret
	err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetModelCredentialSecret}, &existing)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	if err == nil && path == "" {
		return nil
	}
	info, statErr := os.Lstat(path)
	if path == "" || !filepath.IsAbs(path) || statErr != nil || !info.Mode().IsRegular() || info.Size() > 4096 {
		return fmt.Errorf("bounded regular absolute model credential file required")
	}
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		return readErr
	}
	credential := strings.TrimRight(string(raw), "\r\n")
	if len(credential) < 8 || strings.ContainsAny(credential, "\r\n") {
		return fmt.Errorf("model credential file must hold one non-empty line")
	}
	if err == nil {
		if string(existing.Data["token"]) != credential {
			return fmt.Errorf("model credential Secret %s already exists with different content; omit the file to keep it", FleetModelCredentialSecret)
		}
		return nil
	}
	return store.Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: FleetModelCredentialSecret, Namespace: fleetNamespace, Labels: fleetLabels()}, Data: map[string][]byte{"token": []byte(credential)}})
}

var fleetConfigurationFiles = []string{"catalogue.json", "configured.json", "native-template.json"}

// ReadFleetConfiguration materializes the starter configuration a fleet node
// published into dir, for Install. It reports false until a node has
// published; an existing dir must already hold identical files.
func ReadFleetConfiguration(ctx context.Context, store client.Client, dir string) (bool, error) {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return false, fmt.Errorf("clean absolute configuration directory required")
	}
	var published corev1.ConfigMap
	if err := store.Get(ctx, types.NamespacedName{Namespace: fleetNamespace, Name: FleetConfigurationConfigMap}, &published); apierrors.IsNotFound(err) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	for _, name := range fleetConfigurationFiles {
		if content, ok := published.Data[name]; !ok || len(content) == 0 || len(content) > 1<<20 {
			return false, fmt.Errorf("published fleet configuration is incomplete: %s", name)
		}
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return false, err
	}
	for _, name := range fleetConfigurationFiles {
		path := filepath.Join(dir, name)
		if existing, err := os.ReadFile(path); err == nil {
			if string(existing) != published.Data[name] {
				return false, fmt.Errorf("existing %s differs from the published fleet configuration", name)
			}
			continue
		}
		if err := os.WriteFile(path, []byte(published.Data[name]), 0600); err != nil {
			return false, err
		}
	}
	return true, nil
}

// ConfigureFleet rebinds a completed Install to gateway issuance: the owner a
// parent lands on issues its permit, and the controller keeps only its own
// journal/approvals on a claim. It publishes the controller Secret and returns
// the values that wire the controller. An existing Secret is never replaced.
func ConfigureFleet(ctx context.Context, store client.Client, o Options) ([]string, error) {
	if o.ControllerNamespace == "" || o.OwnerTarget != ManagedRouterURL {
		return nil, fmt.Errorf("fleet parents are issued through the shared Celln gateway origin")
	}
	var registration cellnparent.RegistrationConfig
	if _, err := read(filepath.Join(o.OutputDir, "registrations.json"), &registration); err != nil {
		return nil, err
	}
	if registration.LocalProvisioner == nil || registration.RemoteProvisioner != nil || len(registration.HostTemplates) != 1 || len(registration.Registrations) != 0 {
		return nil, fmt.Errorf("fleet wiring requires a fresh local starter registration")
	}
	registration.Journal = filepath.Join(FleetJournalRoot, "journal")
	registration.Approvals = filepath.Join(FleetJournalRoot, "approvals")
	registration.RemoteProvisioner = &cellnparent.RemoteProvisioner{Journal: registration.Journal, Approvals: registration.Approvals, Target: o.OwnerTarget, TokenFile: fleetControllerTokenFile}
	registration.LocalProvisioner = nil
	data, err := json.Marshal(registration)
	if err != nil {
		return nil, err
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: FleetParentConfigSecret, Namespace: o.ControllerNamespace, Labels: fleetLabels()}, Data: map[string][]byte{"registrations.json": data}}
	if err := store.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("publish %s: %w; existing wiring is never replaced", secret.Name, err)
	}
	return []string{"celln.fleet.parentConfigSecret=" + FleetParentConfigSecret}, nil
}
