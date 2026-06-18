package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseSkillTargets(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   \t\n", nil},
		{"single", "github-gitops", []string{"github-gitops"}},
		{"prefixed single", "agent-nxg-helper-github-gitops", []string{"agent-nxg-helper-github-gitops"}},
		{"multiple", "github-gitops,k8s-ops", []string{"github-gitops", "k8s-ops"}},
		{"mixed case + spaces", " Github-Gitops ,  K8S-OPS ", []string{"github-gitops", "k8s-ops"}},
		{"empty entries skipped", "github-gitops,,k8s-ops", []string{"github-gitops", "k8s-ops"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := parseSkillTargets(c.in)
			if !reflect.DeepEqual(got, c.want) {
				t.Fatalf("parseSkillTargets(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

func TestResolveSkillTarget(t *testing.T) {
	inv := []string{
		"agent-nxg-helper-github-gitops",
		"agent-nxg-helper-nxg-essential-tools",
	}

	cases := []struct {
		name      string
		input     string
		valid     []string
		want      string
		wantErr   bool
		errSubstr string
	}{
		{name: "empty input passes through", input: "", valid: inv, want: ""},
		{name: "empty inventory passes input through", input: "github-gitops", valid: nil, want: "github-gitops"},
		{name: "exact match", input: "agent-nxg-helper-github-gitops", valid: inv, want: "agent-nxg-helper-github-gitops"},
		{name: "exact match case-insensitive", input: "Agent-NXG-Helper-Github-Gitops", valid: inv, want: "agent-nxg-helper-github-gitops"},
		{name: "unique suffix match", input: "github-gitops", valid: inv, want: "agent-nxg-helper-github-gitops"},
		{name: "unique suffix match longer", input: "nxg-essential-tools", valid: inv, want: "agent-nxg-helper-nxg-essential-tools"},
		{name: "unknown target", input: "atlassian", valid: inv, wantErr: true, errSubstr: "unknown target"},
		{
			name:      "ambiguous suffix match",
			input:     "ops",
			valid:     []string{"alpha-ops", "beta-ops"},
			wantErr:   true,
			errSubstr: "ambiguous",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := resolveSkillTarget(c.input, c.valid)
			if c.wantErr {
				if err == nil {
					t.Fatalf("resolveSkillTarget(%q) returned no error, want error containing %q", c.input, c.errSubstr)
				}
				if c.errSubstr != "" && !strings.Contains(err.Error(), c.errSubstr) {
					t.Fatalf("resolveSkillTarget(%q) error = %q, want substring %q", c.input, err.Error(), c.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveSkillTarget(%q) unexpected error: %v", c.input, err)
			}
			if got != c.want {
				t.Fatalf("resolveSkillTarget(%q) = %q, want %q", c.input, got, c.want)
			}
		})
	}
}

// TestDefaultToolsAdvertisesAttachedTargets exercises defaultTools when the
// SYMPOZIUM_SKILL_TARGETS env is populated: the execute_command tool's
// `target` schema gains an `enum` constraint listing the canonical names,
// and the description mentions them so providers without enum support still
// pass the constraint to the LLM in plain text.
func TestDefaultToolsAdvertisesAttachedTargets(t *testing.T) {
	prev := skillTargets
	t.Cleanup(func() { skillTargets = prev })

	skillTargets = []string{"agent-nxg-helper-github-gitops", "agent-nxg-helper-nxg-essential-tools"}

	var def *ToolDef
	for i := range defaultTools() {
		td := defaultTools()[i]
		if td.Name == ToolExecuteCommand {
			def = &td
			break
		}
	}
	if def == nil {
		t.Fatalf("execute_command tool not found in defaultTools()")
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	target, _ := props["target"].(map[string]any)
	enum, ok := target["enum"].([]string)
	if !ok {
		t.Fatalf("target schema is missing enum when skillTargets is populated: %#v", target)
	}
	if !reflect.DeepEqual(enum, skillTargets) {
		t.Fatalf("target enum = %v, want %v", enum, skillTargets)
	}
	for _, name := range skillTargets {
		if !strings.Contains(def.Description, name) {
			t.Errorf("execute_command description missing target name %q: %s", name, def.Description)
		}
	}
}

// TestDefaultToolsOmitsEnumWhenNoTargets keeps defaultTools backwards
// compatible: when no sidecars are attached, target stays a free-form string
// and no enum is emitted (an empty enum would make some provider schemas
// invalid).
func TestDefaultToolsOmitsEnumWhenNoTargets(t *testing.T) {
	prev := skillTargets
	t.Cleanup(func() { skillTargets = prev })

	skillTargets = nil

	var def *ToolDef
	for i := range defaultTools() {
		td := defaultTools()[i]
		if td.Name == ToolExecuteCommand {
			def = &td
			break
		}
	}
	if def == nil {
		t.Fatalf("execute_command tool not found in defaultTools()")
	}
	props, _ := def.Parameters["properties"].(map[string]any)
	target, _ := props["target"].(map[string]any)
	if _, hasEnum := target["enum"]; hasEnum {
		t.Fatalf("target schema should not contain enum when skillTargets is empty: %#v", target)
	}
}
