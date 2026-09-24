// Package harness holds the Sympozium-side plumbing shared by every CLI
// harness shim (harness-codex, harness-claude-code, …).
//
// A harness shim is the agent container entrypoint when an Agent / AgentRun
// selects a third-party coding agent instead of the built-in agent-runner.
// Whatever CLI it wraps, every shim has to do the same Sympozium work:
//
//   - assemble agent context from mounted skills, the persona system prompt,
//     channel metadata, and pre-downloaded inbound attachments;
//   - document the `sympozium-tool` shell CLI so the wrapped agent can reach
//     Sympozium capabilities (channels, schedules, sidecar exec, memory);
//   - discover the local MCP bridge endpoint and wait for it to be ready;
//   - scan the final answer for files it produced and attach them;
//   - write /ipc/output/result.json so the controller can surface the result;
//   - emit Sympozium-conventional OTel spans and metrics tagged by harness.
//
// This package owns those pieces. Each shim keeps only what is specific to
// the CLI it wraps: config-file format, credential materialisation, exec
// flags, and output parsing.
package harness

import (
	"fmt"
	"os"
	"strings"
)

// EnvOr returns the value of the environment variable key, or def when it is
// unset or empty.
func EnvOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// FirstNonEmpty returns the first non-empty string in vals, or "".
func FirstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// Logf writes a single diagnostic line to stderr prefixed with the harness
// name (e.g. "harness-codex: …"), matching the convention the controller's
// log scraping and the docs use.
func Logf(name, format string, args ...any) {
	fmt.Fprintf(os.Stderr, "harness-"+name+": "+format+"\n", args...)
}

// AppendResourceAttribute appends a key=value entry to OTEL_RESOURCE_ATTRIBUTES
// (the standard OTel SDK env var) unless a value for that key is already
// present. Every wrapped CLI's OTel SDK reads this env var natively, so
// attributes added here attach to every metric / log / span it emits.
func AppendResourceAttribute(key, value string) {
	if key == "" || value == "" {
		return
	}
	prefix := key + "="
	current := os.Getenv("OTEL_RESOURCE_ATTRIBUTES")
	for _, pair := range strings.Split(current, ",") {
		if strings.HasPrefix(strings.TrimSpace(pair), prefix) {
			return
		}
	}
	entry := prefix + value
	if current == "" {
		_ = os.Setenv("OTEL_RESOURCE_ATTRIBUTES", entry)
		return
	}
	_ = os.Setenv("OTEL_RESOURCE_ATTRIBUTES", current+","+entry)
}
