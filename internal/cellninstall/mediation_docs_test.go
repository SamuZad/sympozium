package cellninstall

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// The mediation guide's walkthrough is only useful if its objects are valid:
// every YAML example decodes strictly into the type it claims to be, the
// objects refer to one another, and the declared route admits the connection.
func TestMediationGuideExamplesAreValid(t *testing.T) {
	raw, err := os.ReadFile("../../docs/guides/celln-mediated-model-access.md")
	if err != nil {
		t.Fatal(err)
	}
	var (
		secret     *corev1.Secret
		connection *api.ModelConnection
		runtime    *api.AgentRuntime
		agent      *api.Agent
		record     *MediationRecord
	)
	for _, match := range regexp.MustCompile("(?s)```yaml\n(.*?)```").FindAllStringSubmatch(string(raw), -1) {
		block := match[1]
		var head struct {
			Kind  string `json:"kind"`
			Celln *struct {
				Mediation *MediationRecord `json:"mediation"`
			} `json:"celln"`
		}
		if err := yaml.Unmarshal([]byte(block), &head); err != nil {
			t.Fatalf("example is not YAML: %v\n%s", err, block)
		}
		strict := func(into any) {
			t.Helper()
			if err := yaml.UnmarshalStrict([]byte(block), into); err != nil {
				t.Fatalf("%s example: %v\n%s", head.Kind, err, block)
			}
		}
		switch {
		case head.Kind == "Secret":
			secret = &corev1.Secret{}
			strict(secret)
		case head.Kind == "ModelConnection":
			connection = &api.ModelConnection{}
			strict(connection)
		case head.Kind == "AgentRuntime":
			runtime = &api.AgentRuntime{}
			strict(runtime)
		case head.Kind == "Agent":
			agent = &api.Agent{}
			strict(agent)
		case head.Celln != nil && head.Celln.Mediation != nil && head.Celln.Mediation.Routes != nil:
			// The values example: exactly the record's fields, strictly.
			var values struct {
				Celln struct {
					Mediation MediationRecord `json:"mediation"`
				} `json:"celln"`
			}
			strict(&values)
			record = &values.Celln.Mediation
		}
	}
	if secret == nil || connection == nil || runtime == nil || agent == nil || record == nil {
		t.Fatalf("the guide lost an example: secret=%v connection=%v runtime=%v agent=%v values=%v", secret != nil, connection != nil, runtime != nil, agent != nil, record != nil)
	}
	if err := ValidateMediatedRoutes(record.Routes); err != nil || len(record.Routes) == 0 {
		t.Fatalf("values example: %v", err)
	}
	spec := connection.Spec
	if err := spec.Validate(); err != nil {
		t.Fatalf("ModelConnection example: %v", err)
	}
	key := "OPENAI_API_KEY"
	if spec.Protocol == "anthropic-messages" {
		key = "ANTHROPIC_API_KEY"
	}
	if _, ok := secret.StringData[key]; !ok || len(secret.StringData) != 1 || spec.SecretRef != secret.Name || secret.Namespace != connection.Namespace {
		t.Fatalf("the Secret example must hold exactly %s and be the connection's secretRef: %+v", key, secret.StringData)
	}
	origin, err := api.ModelEndpointOrigin(spec.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	admitted := false
	for _, declared := range record.Routes {
		route, _ := declared.PolicyRoute()
		admitted = admitted || (route.Provider == spec.Provider && route.Protocol == spec.Protocol && slices.Contains(route.EndpointOrigins, origin) && !slices.ContainsFunc(spec.Models, func(m string) bool { return !slices.Contains(route.Models, m) }))
	}
	if !admitted {
		t.Fatalf("the declared routes do not admit the connection example %+v", spec)
	}
	if reason := agent.Spec.Execution.Validate(); reason != "" {
		t.Fatalf("Agent example: %s", reason)
	}
	execution := agent.Spec.Execution
	if agent.Namespace != connection.Namespace || execution.ModelConnectionRef != connection.Name || !slices.Contains(spec.Models, execution.Model) || agent.Spec.RuntimeRef != runtime.Name || execution.CellnSelection.RuntimeRef != runtime.Name || runtime.Namespace != agent.Namespace {
		t.Fatalf("the examples do not refer to one another: %+v", agent.Spec)
	}
	if len(agent.Spec.AuthRefs) != 1 || agent.Spec.AuthRefs[0].Secret != secret.Name || !strings.EqualFold(agent.Spec.AuthRefs[0].Provider, spec.Provider) {
		t.Fatalf("authRefs must grant the connection's Secret: %+v", agent.Spec.AuthRefs)
	}
	if runtime.Spec.CellnProfileRef == nil || runtime.Spec.CellnProfileRef.Name != PlatformProfileName("starter", "native") {
		t.Fatalf("runtime wrapper example: %+v", runtime.Spec)
	}
}
