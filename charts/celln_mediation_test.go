package charts

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	cap "github.com/sympozium-ai/sympozium/internal/cellncapability"
	"github.com/sympozium-ai/sympozium/internal/cellnscoped"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const mediationImage = "modelGateway.image=registry.example/model-gateway@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"

// mediationValues is the complete operator input on top of an installed fleet.
func mediationValues() []string {
	return append(fleetValues(),
		"celln.mediation.enabled=true", "celln.mediation.clusterId=evaluation", "celln.mediation.issuer.keyId=mediation-test",
		mediationImage, "modelGateway.database.secretName=gateway-database",
		"modelGateway.egress[0].ports[0].protocol=TCP", "modelGateway.egress[0].ports[0].port=443",
	)
}

func without(values []string, prefixes ...string) []string {
	return slices.DeleteFunc(slices.Clone(values), func(v string) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(v, p) })
	})
}

// The chart generates a few random credentials per render; everything else
// must be stable for a disabled-equals-absent comparison.
var generated = regexp.MustCompile(`(?m)^(\s*(?:token|[a-z-]*password|checksum/[a-z-]+):).*$`)

func stable(raw []byte) string { return generated.ReplaceAllString(string(raw), "$1 X") }

type mediationRender struct {
	fleetRender
	configMaps      map[string]corev1.ConfigMap
	secrets         map[string]bool
	policies        map[string]networkingv1.NetworkPolicy
	clusterRoles    map[string]rbacv1.ClusterRole
	roles           map[string]rbacv1.Role
	roleBindings    map[string]rbacv1.RoleBinding
	clusterBindings map[string]bool
}

func decodeMediation(t *testing.T, raw []byte) mediationRender {
	t.Helper()
	out := mediationRender{fleetRender: decodeFleet(t, raw), configMaps: map[string]corev1.ConfigMap{}, secrets: map[string]bool{}, policies: map[string]networkingv1.NetworkPolicy{},
		clusterRoles: map[string]rbacv1.ClusterRole{}, roles: map[string]rbacv1.Role{}, roleBindings: map[string]rbacv1.RoleBinding{}, clusterBindings: map[string]bool{}}
	for _, document := range bytes.Split(raw, []byte("\n---")) {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name, Namespace string
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(document, &meta); err != nil {
			t.Fatalf("decode: %v", err)
		}
		key := meta.Metadata.Namespace + "/" + meta.Metadata.Name
		strict := func(target any) {
			if err := yaml.UnmarshalStrict(document, target); err != nil {
				t.Fatalf("%s %s: %v", meta.Kind, key, err)
			}
		}
		switch meta.Kind {
		case "ConfigMap":
			var o corev1.ConfigMap
			strict(&o)
			out.configMaps[key] = o
		case "Deployment":
			// decodeFleet is lenient; a misplaced field must fail here.
			strict(&appsv1.Deployment{})
		case "DaemonSet":
			strict(&appsv1.DaemonSet{})
		case "Secret":
			out.secrets[key] = true
		case "NetworkPolicy":
			var o networkingv1.NetworkPolicy
			strict(&o)
			out.policies[key] = o
		case "ClusterRole":
			var o rbacv1.ClusterRole
			strict(&o)
			out.clusterRoles[meta.Metadata.Name] = o
		case "Role":
			var o rbacv1.Role
			strict(&o)
			out.roles[key] = o
		case "RoleBinding":
			var o rbacv1.RoleBinding
			strict(&o)
			out.roleBindings[key] = o
		case "ClusterRoleBinding":
			out.clusterBindings[meta.Metadata.Name] = true
		}
	}
	return out
}

func TestMediationDisabledRendersNothing(t *testing.T) {
	// Every mediation input present, only the switch off: the render must be
	// the one an install without any of it gets, by default and on a fleet.
	inputs := without(mediationValues()[len(fleetValues()):], "celln.mediation.enabled", "modelGateway.image", "modelGateway.egress")
	inputs = append(inputs, "celln.mediation.enabled=false", "celln.mediation.receiver.url=https://node-a.example:9443", "modelGateway.namespaces[0]=team-a")
	for name, base := range map[string][]string{"defaults": nil, "fleet": fleetValues()} {
		plain := stable(mustRender(t, base))
		if got := stable(mustRender(t, append(slices.Clone(base), inputs...))); got != plain {
			t.Fatalf("%s: disabled mediation changed the render", name)
		}
		for _, leak := range []string{"CELLN_SCOPED_CONFIG", "--scoped-", "celln-scoped", "celln-mediation", "model-gateway"} {
			if strings.Contains(plain, leak) {
				t.Fatalf("%s: render without mediation mentions %q", name, leak)
			}
		}
	}
}

func container(t *testing.T, containers []corev1.Container, name string) corev1.Container {
	t.Helper()
	for _, c := range containers {
		if c.Name == name {
			return c
		}
	}
	t.Fatalf("no container %s", name)
	return corev1.Container{}
}

func mountPaths(c corev1.Container) map[string]string {
	out := map[string]string{}
	for _, m := range c.VolumeMounts {
		out[m.Name] = m.MountPath
	}
	return out
}

func volume(t *testing.T, spec corev1.PodSpec, name string) corev1.Volume {
	t.Helper()
	for _, v := range spec.Volumes {
		if v.Name == name {
			return v
		}
	}
	t.Fatalf("no volume %s", name)
	return corev1.Volume{}
}

func restricted(t *testing.T, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	if sc == nil || sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot || sc.RunAsUser == nil || *sc.RunAsUser != 65532 || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation ||
		sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" || len(sc.Capabilities.Add) != 0 || (sc.Privileged != nil && *sc.Privileged) {
		t.Fatalf("container %s is not non-root, read-only, drop ALL: %+v", c.Name, sc)
	}
}

func TestMediationWiresControllerDispatchersAndGatewayConsistently(t *testing.T) {
	raw := mustRender(t, mediationValues())
	r := decodeMediation(t, raw)
	const gatewayOrigin = "https://test-sympozium-model-gateway.sympozium-system.svc:8443"
	const receiverOrigin = "https://celln-scoped-receiver.celln-system.svc:9443"

	// Controller: CELLN_SCOPED_CONFIG names a configuration its own strict
	// loader accepts, and every file that names is a mounted path.
	manager := r.deployments["sympozium-controller-manager"].Spec.Template.Spec
	main := container(t, manager.Containers, "manager")
	env := map[string]string{}
	for _, e := range main.Env {
		env[e.Name] = e.Value
		if e.ValueFrom != nil && e.ValueFrom.SecretKeyRef != nil && strings.Contains(e.ValueFrom.SecretKeyRef.Name, "mediation") {
			t.Fatal("mediation credential exposed as an environment variable")
		}
	}
	if len(main.EnvFrom) != 0 || env["CELLN_SCOPED_CONFIG"] != "/etc/sympozium/celln-scoped/config.json" {
		t.Fatalf("scoped configuration not wired: %v", env["CELLN_SCOPED_CONFIG"])
	}
	var cfg cellnscoped.Config
	if err := cap.StrictDecode([]byte(r.configMaps["sympozium-system/sympozium-celln-scoped-config"].Data["config.json"]), &cfg); err != nil {
		t.Fatalf("controller refuses the rendered configuration: %v", err)
	}
	want := cellnscoped.Config{ClusterID: "evaluation", PreparationNamespace: "sympozium-system",
		Receiver: cellnscoped.EndpointConfig{URL: receiverOrigin, CAFile: "/etc/sympozium/celln-scoped-trust/ca.crt", TokenFile: "/run/sympozium/celln-scoped/receiver-token"},
		Gateway:  &cellnscoped.EndpointConfig{URL: gatewayOrigin, CAFile: "/etc/sympozium/celln-scoped-trust/ca.crt", TokenFile: "/run/sympozium/celln-scoped/gateway-token"},
		Issuer:   cellnscoped.IssuerConfig{Name: "sympozium-control-plane", KeyID: "mediation-test", PrivateKeyFile: "/run/sympozium/celln-scoped/issuer.key"}}
	if mustJSON(t, cfg) != mustJSON(t, want) {
		t.Fatalf("scoped configuration:\n got %s\nwant %s", mustJSON(t, cfg), mustJSON(t, want))
	}
	mounts := mountPaths(main)
	if mounts["celln-scoped-config"] != "/etc/sympozium/celln-scoped" || mounts["celln-scoped-trust"] != "/etc/sympozium/celln-scoped-trust" || mounts["celln-scoped-private"] != "/run/sympozium/celln-scoped" {
		t.Fatalf("manager mounts: %v", mounts)
	}
	if _, ok := mounts["celln-scoped-operator"]; ok {
		t.Fatal("the manager mounts the world-readable Secret volume; only the copy step may")
	}
	if v := volume(t, manager, "celln-scoped-operator"); v.Secret == nil || v.Secret.SecretName != "celln-mediation-controller" || len(v.Secret.Items) != 3 {
		t.Fatalf("controller Secret volume: %+v", v)
	}
	if v := volume(t, manager, "celln-scoped-private"); v.EmptyDir == nil || v.EmptyDir.Medium != corev1.StorageMediumMemory {
		t.Fatal("private files must live in memory")
	}
	if v := volume(t, manager, "celln-scoped-trust"); v.ConfigMap == nil || v.ConfigMap.Name != "celln-mediation-trust" {
		t.Fatalf("controller trust: %+v", v)
	}
	copyStep := container(t, manager.InitContainers, "celln-scoped-private-files")
	restricted(t, copyStep)
	for _, file := range []string{"issuer.key", "receiver-token", "gateway-token", "install -m 0400"} {
		if !strings.Contains(copyStep.Command[2], file) {
			t.Fatalf("copy step lacks %s", file)
		}
	}

	// Dispatchers: all scoped flags, the same issuer and gateway origin.
	owner := r.daemonSets["celln-node"].Spec.Template.Spec
	dispatcher := container(t, owner.Containers, "dispatcher")
	args := strings.Join(dispatcher.Args, " ")
	for _, flag := range []string{
		"--scoped-operator-token-file /etc/celln/scoped/operator-token",
		"--scoped-jwks-file /etc/celln/scoped-trust/..data/jwks.json",
		"--scoped-issuer " + cfg.Issuer.Name,
		"--scoped-gateway-origin " + cfg.Gateway.URL,
		"--scoped-gateway-ca /etc/celln/scoped-trust/ca.crt",
	} {
		if !strings.Contains(args, flag) {
			t.Fatalf("dispatcher lacks %q: %s", flag, args)
		}
	}
	if strings.Contains(args, "--scoped-parent-request-file") {
		t.Fatal("the parent request flag must be decided on the node, where the file may be absent")
	}
	mounts = mountPaths(dispatcher)
	if mounts["scoped-operator"] != "/etc/celln/scoped" || mounts["scoped-trust"] != "/etc/celln/scoped-trust" {
		t.Fatalf("dispatcher mounts: %v", mounts)
	}
	if _, ok := mounts["scoped-receiver-tls"]; ok {
		t.Fatal("the privileged dispatcher must not hold the receiver's TLS key")
	}
	if v := volume(t, owner, "scoped-operator"); v.Secret == nil || v.Secret.SecretName != "celln-mediation-node" || *v.Secret.DefaultMode != 0o400 || len(v.Secret.Items) != 1 || v.Secret.Items[0].Key != "operator-token" {
		t.Fatalf("operator token volume: %+v", v.Secret)
	}
	if v := volume(t, owner, "scoped-trust"); v.ConfigMap == nil || v.ConfigMap.Name != "celln-mediation-trust" {
		t.Fatalf("node trust: %+v", v)
	}
	edge := container(t, owner.Containers, "scoped-receiver")
	restricted(t, edge)
	if edge.SecurityContext.SeccompProfile == nil || edge.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("receiver edge lacks the RuntimeDefault seccomp profile")
	}
	if !strings.Contains(edge.Image, "/controller:v") || edge.Command[0] != "/celln-parent-proxy" {
		t.Fatalf("receiver edge image/command: %s %v", edge.Image, edge.Command)
	}
	edgeArgs := strings.Join(edge.Args, " ")
	for _, arg := range []string{"--scoped-receiver", "--listen 0.0.0.0:9443", "--backend http://127.0.0.1:8787", "--tls-cert /etc/celln/receiver-tls/tls.crt", "--tls-key /etc/celln/receiver-tls/tls.key"} {
		if !strings.Contains(edgeArgs, arg) {
			t.Fatalf("receiver edge lacks %q: %s", arg, edgeArgs)
		}
	}
	if len(edge.VolumeMounts) != 1 || edge.VolumeMounts[0].Name != "scoped-receiver-tls" {
		t.Fatalf("receiver edge mounts more than its certificate: %+v", edge.VolumeMounts)
	}
	service := r.services["celln-scoped-receiver"]
	if service.Namespace != "celln-system" || service.Spec.Selector["app.kubernetes.io/name"] != "celln-node" || service.Spec.Ports[0].Port != 9443 || service.Spec.Ports[0].TargetPort.StrVal != "scoped-https" || edge.Ports[0].Name != "scoped-https" {
		t.Fatalf("receiver Service: %+v", service.Spec)
	}

	// Gateway: same cluster, issuer and JWKS source; only file paths.
	gateway := r.deployments["test-sympozium-model-gateway"].Spec.Template.Spec
	gw := container(t, gateway.Containers, "model-gateway")
	// Unchanged from the claim-based gateway: identity at pod level.
	if sc := gateway.SecurityContext; !*sc.RunAsNonRoot || *sc.RunAsUser != 65532 || sc.FSGroup != nil || *gw.SecurityContext.AllowPrivilegeEscalation || !*gw.SecurityContext.ReadOnlyRootFilesystem || gw.SecurityContext.Capabilities.Drop[0] != "ALL" {
		t.Fatalf("gateway security context weakened: %+v %+v", sc, gw.SecurityContext)
	}
	if gw.Args[0] != "--config=/etc/model-gateway/configuration/gateway.json" || gw.Image != strings.TrimPrefix(mediationImage, "modelGateway.image=") {
		t.Fatalf("gateway container: %v %s", gw.Args, gw.Image)
	}
	var gatewayConfig map[string]any
	decoder := json.NewDecoder(strings.NewReader(r.configMaps["sympozium-system/test-sympozium-model-gateway-configuration"].Data["gateway.json"]))
	if err := decoder.Decode(&gatewayConfig); err != nil {
		t.Fatal(err)
	}
	wantGateway := map[string]any{"clusterId": cfg.ClusterID, "issuer": cfg.Issuer.Name, "listen": ":8443", "privateOrigins": []any{},
		"tlsCertificateFile": "/run/model-gateway/private/tls.crt", "tlsKeyFile": "/run/model-gateway/private/tls.key",
		"verificationKeysFile": "/etc/model-gateway/trust/jwks.json", "registrationTokenFile": "/run/model-gateway/private/registration-token", "databaseUrlFile": "/run/model-gateway/private/database-url"}
	if mustJSON(t, gatewayConfig) != mustJSON(t, wantGateway) {
		t.Fatalf("gateway.json:\n got %s\nwant %s", mustJSON(t, gatewayConfig), mustJSON(t, wantGateway))
	}
	mounts = mountPaths(gw)
	if mounts["configuration"] != "/etc/model-gateway/configuration" || mounts["trust"] != "/etc/model-gateway/trust" || mounts["private"] != "/run/model-gateway/private" || len(mounts) != 3 {
		t.Fatalf("gateway mounts: %v", mounts)
	}
	if v := volume(t, gateway, "trust"); v.ConfigMap == nil || v.ConfigMap.Name != "celln-mediation-trust" {
		t.Fatalf("gateway trust: %+v", v)
	}
	sources := volume(t, gateway, "operator").Projected.Sources
	if len(sources) != 2 || sources[0].Secret.Name != "celln-mediation-gateway" || sources[1].Secret.Name != "gateway-database" || sources[1].Secret.Items[0].Key != "database-url" || sources[1].Secret.Items[0].Path != "database-url" {
		t.Fatalf("gateway Secret sources: %+v", sources)
	}
	for _, v := range gateway.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Fatal("mediated gateway still depends on a claim")
		}
	}
	restricted(t, container(t, gateway.InitContainers, "private-files"))
	if gateway.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault || manager.SecurityContext.SeccompProfile.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Fatal("pod seccomp profile weakened")
	}

	// NetworkPolicy: the gateway admits the controller and the fleet owners;
	// the owners admit the controller on the TLS edge only.
	admits := func(policy networkingv1.NetworkPolicy, namespace, label, value string, port int32) bool {
		for _, rule := range policy.Spec.Ingress {
			for _, peer := range rule.From {
				if peer.NamespaceSelector == nil || peer.PodSelector == nil {
					t.Fatalf("%s: peer does not conjoin namespace and pod identity", policy.Name)
				}
				if peer.NamespaceSelector.MatchLabels["kubernetes.io/metadata.name"] == namespace && peer.PodSelector.MatchLabels[label] == value {
					for _, p := range rule.Ports {
						if p.Port.IntVal == port {
							return true
						}
					}
				}
			}
		}
		return false
	}
	gatewayPolicy := r.policies["sympozium-system/test-sympozium-model-gateway"]
	if !admits(gatewayPolicy, "celln-system", "app.kubernetes.io/name", "celln-node", 8443) || !admits(gatewayPolicy, "sympozium-system", "control-plane", "controller-manager", 8443) {
		t.Fatal("gateway does not admit the fleet dispatchers and the controller")
	}
	nodePolicy := r.policies["celln-system/celln-node-ingress"]
	if !admits(nodePolicy, "sympozium-system", "control-plane", "controller-manager", 9443) || admits(nodePolicy, "sympozium-system", "control-plane", "controller-manager", 8787) {
		t.Fatal("the controller must reach the receiver's TLS edge and not the plaintext dispatcher")
	}
	if !admits(nodePolicy, "celln-system", "app.kubernetes.io/name", "celln-router", 8787) || admits(nodePolicy, "celln-system", "app.kubernetes.io/name", "celln-router", 9443) {
		t.Fatal("router ingress changed")
	}

	// No key material is rendered: mediation adds exactly two ConfigMaps of
	// paths and public names, and not a single Secret.
	base := decodeMediation(t, mustRender(t, fleetValues()))
	for name := range r.secrets {
		if !base.secrets[name] {
			t.Fatalf("mediation rendered Secret %s; key material is operator-provided", name)
		}
	}
	added := []string{}
	for name, cm := range r.configMaps {
		if _, ok := base.configMaps[name]; ok {
			continue
		}
		added = append(added, name)
		for key, value := range cm.Data {
			for _, private := range []string{"PRIVATE", "BEGIN", "postgres://", "Bearer", `"d"`} {
				if strings.Contains(value, private) {
					t.Fatalf("ConfigMap %s key %s carries %q", name, key, private)
				}
			}
		}
	}
	slices.Sort(added)
	if mustJSON(t, added) != `["sympozium-system/sympozium-celln-scoped-config","sympozium-system/test-sympozium-model-gateway-configuration"]` {
		t.Fatalf("unexpected ConfigMaps: %v", added)
	}
	for _, leaked := range []string{"genPrivateKey", "genCA", "lookup"} {
		for _, file := range []string{"celln-mediation.yaml", "model-gateway.yaml"} {
			source, err := Sympozium.ReadFile("sympozium/templates/" + file)
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(source), leaked) {
				t.Fatalf("%s mints or looks up key material with %s", file, leaked)
			}
		}
	}
}

func TestMediationRefusesIncompleteOrInconsistentInput(t *testing.T) {
	full := mediationValues()
	for name, tc := range map[string]struct {
		values []string
		want   string
	}{
		"no fleet":            {append(without(full, "celln.fleet.enabled"), "celln.fleet.enabled=false"), "celln.mediation requires celln.enabled and celln.fleet.enabled"},
		"no cluster id":       {without(full, "celln.mediation.clusterId"), "celln.mediation.clusterId is required"},
		"no key id":           {without(full, "celln.mediation.issuer.keyId"), "celln.mediation.issuer.keyId is required"},
		"another issuer":      {append(slices.Clone(full), "celln.mediation.issuer.name=tenant-issuer"), "celln.mediation.issuer.name must stay sympozium-control-plane"},
		"no controller key":   {append(slices.Clone(full), "celln.mediation.controllerSecret="), "celln.mediation.controllerSecret must name"},
		"no gateway secret":   {append(slices.Clone(full), "celln.mediation.gatewaySecret="), "celln.mediation.gatewaySecret must name"},
		"no node secret":      {append(slices.Clone(full), "celln.mediation.nodeSecret="), "celln.mediation.nodeSecret must name"},
		"no trust":            {append(slices.Clone(full), "celln.mediation.trustConfigMap="), "celln.mediation.trustConfigMap must name"},
		"no database":         {without(full, "modelGateway.database.secretName"), "celln.mediation requires modelGateway.database.secretName"},
		"no database key":     {append(slices.Clone(full), "modelGateway.database.key="), "modelGateway.database.key must name"},
		"no gateway image":    {without(full, "modelGateway.image"), "modelGateway.image requires an immutable image digest"},
		"tagged image":        {append(without(full, "modelGateway.image"), "modelGateway.image=registry.example/model-gateway:latest"), "modelGateway.image must be pinned by sha256 digest"},
		"no egress":           {without(full, "modelGateway.egress"), "modelGateway.egress must explicitly declare"},
		"claim as well":       {append(slices.Clone(full), "modelGateway.configurationClaim=reviewed"), "unset modelGateway.configurationClaim"},
		"plaintext receiver":  {append(slices.Clone(full), "celln.mediation.receiver.url=http://node-a:8787"), "celln.mediation.receiver.url must be an origin-only HTTPS URL"},
		"receiver with path":  {append(slices.Clone(full), "celln.mediation.receiver.url=https://node-a:9443/v1/scoped"), "celln.mediation.receiver.url must be an origin-only HTTPS URL"},
		"dispatcher port":     {append(slices.Clone(full), "celln.mediation.receiver.port=8787"), "celln.mediation.receiver.port must be"},
		"template elsewhere":  {append(slices.Clone(full), "celln.mediation.parentRequestFile=/etc/celln/parent.json"), "celln.mediation.parentRequestFile must be a file under the node state directory /var/lib/sympozium-celln/starter"},
		"template traversal":  {append(slices.Clone(full), "celln.mediation.parentRequestFile=/var/lib/sympozium-celln/starter/../other/parent.json"), "celln.mediation.parentRequestFile must be"},
		"duplicate namespace": {append(slices.Clone(full), "modelGateway.namespaces[0]=team-a", "modelGateway.namespaces[1]=team-a"), "modelGateway.namespaces must be unique namespace names"},
	} {
		raw, err := renderNativeParent(t, tc.values)
		if err == nil {
			t.Fatalf("%s: rendered", name)
		}
		if !strings.Contains(string(raw), tc.want) {
			t.Fatalf("%s: refusal lacks %q:\n%s", name, tc.want, raw)
		}
	}
}

func TestMediationReceiverOverrideReplacesTheService(t *testing.T) {
	r := decodeMediation(t, mustRender(t, append(mediationValues(), "celln.mediation.receiver.url=https://node-a.example:9443", "celln.mediation.preparationNamespace=celln-prepared")))
	if _, ok := r.services["celln-scoped-receiver"]; ok {
		t.Fatal("an explicit receiver still renders the single-node Service")
	}
	var cfg cellnscoped.Config
	if err := cap.StrictDecode([]byte(r.configMaps["sympozium-system/sympozium-celln-scoped-config"].Data["config.json"]), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Receiver.URL != "https://node-a.example:9443" || cfg.PreparationNamespace != "celln-prepared" {
		t.Fatalf("overrides not applied: %+v", cfg)
	}
}

// The gateway keeps its cluster-wide get unless the operator lists namespaces;
// a list narrows Secrets and ModelConnections to exactly those.
func TestGatewayNamespacesNarrowSecretAccess(t *testing.T) {
	wide := decodeMediation(t, mustRender(t, mediationValues()))
	role := wide.clusterRoles["test-sympozium-model-gateway"]
	if len(wide.roles) != len(decodeMediation(t, mustRender(t, fleetValues())).roles) || len(role.Rules) != 3 || !slices.Contains(role.Rules[0].Resources, "secrets") {
		t.Fatalf("default gateway RBAC changed: %+v", role.Rules)
	}
	narrow := decodeMediation(t, mustRender(t, append(mediationValues(), "modelGateway.namespaces[0]=team-a", "modelGateway.namespaces[1]=team-b")))
	for _, rule := range narrow.clusterRoles["test-sympozium-model-gateway"].Rules {
		if slices.Contains(rule.Resources, "secrets") || slices.Contains(rule.Resources, "modelconnections") {
			t.Fatal("cluster-wide Secret/ModelConnection access kept beside the namespace list")
		}
		if slices.Contains(rule.Resources, "namespaces") && mustJSON(t, rule.ResourceNames) != `["team-a","team-b"]` {
			t.Fatalf("namespace reads not limited to the list: %v", rule.ResourceNames)
		}
	}
	for _, ns := range []string{"team-a", "team-b"} {
		got, binding := narrow.roles[ns+"/test-sympozium-model-gateway"], narrow.roleBindings[ns+"/test-sympozium-model-gateway"]
		if len(got.Rules) != 2 || mustJSON(t, got.Rules[0].Verbs) != `["get"]` || mustJSON(t, got.Rules[1].Verbs) != `["get"]` {
			t.Fatalf("%s role: %+v", ns, got.Rules)
		}
		if len(binding.Subjects) != 1 || binding.Subjects[0].Name != "test-sympozium-model-gateway" || binding.Subjects[0].Namespace != "sympozium-system" {
			t.Fatalf("%s binding: %+v", ns, binding.Subjects)
		}
	}
	var gatewayConfig struct {
		ReadinessNamespaces []string `json:"readinessNamespaces"`
	}
	if err := json.Unmarshal([]byte(narrow.configMaps["sympozium-system/test-sympozium-model-gateway-configuration"].Data["gateway.json"]), &gatewayConfig); err != nil || mustJSON(t, gatewayConfig.ReadinessNamespaces) != `["team-a","team-b"]` {
		t.Fatalf("gateway readiness would still probe cluster-wide access: %v %v", gatewayConfig, err)
	}
}

// Celln refuses to start when --scoped-parent-request-file names a missing
// file, so the owner decides on the node: with the file, enduring scoped runs;
// without it, one-shot scoped dispatch only.
func TestMediationPassesTheParentRequestOnlyWhenTheNodeHasOne(t *testing.T) {
	for name, values := range map[string][]string{
		"auto":  mediationValues(),
		"fixed": append(mediationValues(), "celln.fleet.capacity=fixed", "celln.fleet.maxCells=4", "celln.fleet.memoryBytes=4294967296"),
	} {
		owner := decodeMediation(t, mustRender(t, values)).daemonSets["celln-node"].Spec.Template.Spec
		dispatcher := container(t, owner.Containers, "dispatcher")
		if len(dispatcher.Command) != 4 || dispatcher.Command[0] != "/bin/sh" {
			t.Fatalf("%s: dispatcher is not started through the deciding wrapper: %v", name, dispatcher.Command)
		}
		var request string
		for _, e := range dispatcher.Env {
			if e.Name == "SCOPED_PARENT_REQUEST" {
				request = e.Value
			}
		}
		if request != "/var/lib/sympozium-celln/starter/configuration-"+strings.Repeat("b", 64)+"-native/scoped-parent-request.json" {
			t.Fatalf("%s: default parent request path: %q", name, request)
		}
		wait := container(t, owner.InitContainers, "wait-prepared")
		if !strings.Contains(wait.Command[2], `$(dirname "$SCOPED_PARENT_REQUEST")/configured.json`) {
			t.Fatalf("%s: the owner may start before node configuration wrote the parent request", name)
		}
		dir := t.TempDir()
		fake := filepath.Join(dir, "celln")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		script := strings.ReplaceAll(dispatcher.Command[2], "/usr/local/bin/celln", fake)
		file := filepath.Join(dir, "scoped-parent-request.json")
		run := func() string {
			cmd := exec.Command("/bin/sh", "-ec", script, "celln-dispatcher", "dispatcher", "--listen", "0.0.0.0:8787")
			cmd.Env = append(os.Environ(), "SCOPED_PARENT_REQUEST="+file)
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s: wrapper: %v", name, err)
			}
			return string(out)
		}
		if out := run(); strings.Contains(out, "--scoped-parent-request-file") || !strings.HasPrefix(out, "dispatcher\n--listen\n") {
			t.Fatalf("%s: flag passed for a missing file, or arguments lost:\n%s", name, out)
		}
		if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
			t.Fatal(err)
		}
		if out := run(); !strings.Contains(out, "--scoped-parent-request-file\n"+file+"\n") {
			t.Fatalf("%s: flag not passed for a present file:\n%s", name, out)
		}
	}
}
