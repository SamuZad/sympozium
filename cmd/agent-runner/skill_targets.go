package main

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// skillTargets is the canonical inventory of sidecar targets the LLM may
// address through execute_command's `target` argument. It is populated from
// the SYMPOZIUM_SKILL_TARGETS env var injected by the AgentRun controller and
// holds the same SkillPack `metadata.name` values that each sidecar's
// SYMPOZIUM_SKILL_PACK env var uses for matching. Empty when no skill
// sidecars are attached.
var skillTargets []string

// loadSkillTargets reads SYMPOZIUM_SKILL_TARGETS (comma-separated SkillPack
// names) and stores the normalized list in the package-level skillTargets.
func loadSkillTargets() {
	skillTargets = parseSkillTargets(os.Getenv("SYMPOZIUM_SKILL_TARGETS"))
}

func parseSkillTargets(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		v := normalizeSidecarTarget(p)
		if v == "" {
			continue
		}
		out = append(out, v)
	}
	return out
}

// resolveSkillTarget maps an LLM-supplied target (which may be a short skill
// name like "github-gitops") to the canonical SkillPack name a sidecar's
// tool-executor will match against (e.g. "agent-nxg-helper-github-gitops"),
// using `valid` as the inventory of attached sidecars.
//
//   - Empty input → "", nil (legacy: any sidecar may claim).
//   - Empty inventory → input passed through verbatim (callers that don't
//     populate the inventory keep their previous behaviour).
//   - Exact match → that value.
//   - Unique suffix match where a canonical entry equals input or ends with
//     "-<input>" → that canonical entry.
//   - Ambiguous suffix match or no match → error mentioning the inventory.
func resolveSkillTarget(input string, valid []string) (string, error) {
	in := normalizeSidecarTarget(input)
	if in == "" {
		return "", nil
	}
	if len(valid) == 0 {
		return in, nil
	}
	for _, t := range valid {
		if t == in {
			return t, nil
		}
	}
	var matches []string
	for _, t := range valid {
		if strings.HasSuffix(t, "-"+in) {
			matches = append(matches, t)
		}
	}
	if len(matches) == 1 {
		return matches[0], nil
	}
	if len(matches) > 1 {
		return "", fmt.Errorf("ambiguous target %q matches multiple sidecars: %s — use the full SkillPack name",
			input, strings.Join(matches, ", "))
	}
	return "", errors.New(unknownTargetMessage(input, valid))
}

func unknownTargetMessage(input string, valid []string) string {
	return fmt.Sprintf("unknown target %q. Available targets: %s",
		input, strings.Join(valid, ", "))
}
