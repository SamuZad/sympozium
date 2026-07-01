package main

import (
	"strings"
	"testing"
)

func TestResolveExecTarget(t *testing.T) {
	inventory := "agent-nxg-helper-github-gitops,agent-nxg-helper-nxg-essential-tools"
	cases := []struct {
		name      string
		input     string
		inventory string
		want      string
		errSubstr string
	}{
		{name: "empty input", input: "", inventory: inventory, want: ""},
		{name: "legacy no inventory passthrough", input: "nxg-essential-tools", inventory: "", want: "nxg-essential-tools"},
		{name: "exact match", input: "agent-nxg-helper-nxg-essential-tools", inventory: inventory, want: "agent-nxg-helper-nxg-essential-tools"},
		{name: "unique suffix", input: "nxg-essential-tools", inventory: inventory, want: "agent-nxg-helper-nxg-essential-tools"},
		{name: "case and space normalized", input: " NXG-ESSENTIAL-TOOLS ", inventory: inventory, want: "agent-nxg-helper-nxg-essential-tools"},
		{
			name:      "ambiguous suffix",
			input:     "tools",
			inventory: "agent-a-tools,agent-b-tools",
			errSubstr: "ambiguous",
		},
		{
			name:      "unknown target",
			input:     "missing-tools",
			inventory: inventory,
			errSubstr: "unknown target",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveExecTarget(tc.input, tc.inventory)
			if tc.errSubstr != "" {
				if err == nil {
					t.Fatalf("resolveExecTarget returned nil error, want %q", tc.errSubstr)
				}
				if !strings.Contains(err.Error(), tc.errSubstr) {
					t.Fatalf("error = %q, want substring %q", err.Error(), tc.errSubstr)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveExecTarget: %v", err)
			}
			if got != tc.want {
				t.Fatalf("resolveExecTarget(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}
