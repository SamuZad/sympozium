package main

import (
	"bytes"
	"strings"
	"testing"
	"text/template"
)

// TestMigrationTemplateRenders pins the only substitution point in the
// migration templates: the pgvector column width. If someone adds a new
// {{.Foo}} placeholder without wiring Foo into migrationVars, the
// missingkey=error option will turn it into a test failure here.
func TestMigrationTemplateRenders(t *testing.T) {
	tests := []struct {
		name    string
		dim     int
		wantSub string
	}{
		{name: "default 1536", dim: 1536, wantSub: "vector(1536)"},
		{name: "small 768", dim: 768, wantSub: "vector(768)"},
		{name: "tiny 4", dim: 4, wantSub: "vector(4)"},
	}
	data, err := migrationsFS.ReadFile("migrations/001_initial.sql.tmpl")
	if err != nil {
		t.Fatalf("read template: %v", err)
	}
	tmpl, err := template.New("001").Option("missingkey=error").Parse(string(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := tmpl.Execute(&buf, migrationVars{Dimension: tt.dim}); err != nil {
				t.Fatalf("execute: %v", err)
			}
			rendered := buf.String()
			if !strings.Contains(rendered, tt.wantSub) {
				t.Errorf("rendered output missing %q\nfirst 400 chars:\n%s", tt.wantSub, rendered[:min(400, len(rendered))])
			}
		})
	}
}
