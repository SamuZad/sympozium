package main

import (
	"fmt"
	"os"
	"sort"
	"strings"
	"sync"
)

// skillRegistry maps qualified skill names ("<pack>/<skill>") to their
// indexed entries. Populated by registerSkills at startup, then read-only
// during tool dispatch.
var (
	skillRegistry   = map[string]skillIndexEntry{}
	skillRegistryMu sync.RWMutex
)

func registerSkills(skills []skillIndexEntry) {
	skillRegistryMu.Lock()
	defer skillRegistryMu.Unlock()
	skillRegistry = make(map[string]skillIndexEntry, len(skills))
	for _, s := range skills {
		skillRegistry[s.Name] = s
	}
}

func lookupSkill(name string) (skillIndexEntry, bool) {
	skillRegistryMu.RLock()
	defer skillRegistryMu.RUnlock()
	s, ok := skillRegistry[name]
	return s, ok
}

// skillsToolDef builds the `skills` tool. The catalog is embedded in the
// description so bodies stay off the wire until the model invokes a
// specific skill. Returns ok=false when no skills are loaded (an empty
// enum is invalid for some providers).
func skillsToolDef(skills []skillIndexEntry) (ToolDef, bool) {
	if len(skills) == 0 {
		return ToolDef{}, false
	}
	names := make([]string, 0, len(skills))
	var catalog strings.Builder
	for _, s := range skills {
		names = append(names, s.Name)
		fmt.Fprintf(&catalog, "- `%s`", s.Name)
		if s.Description != "" {
			fmt.Fprintf(&catalog, ": %s", s.Description)
		}
		catalog.WriteByte('\n')
	}
	sort.Strings(names)

	desc := "Load the full Markdown instructions for one of the agent's skills. " +
		"Each skill is a self-contained procedure (commands to run, files to read, " +
		"validation steps). Call this tool BEFORE attempting work that matches a " +
		"skill's description — the body contains the authoritative steps and " +
		"should not be guessed.\n\nAvailable skills:\n" + catalog.String()

	return ToolDef{
		Name:        ToolSkills,
		Description: desc,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Name of the skill to load (must match one of the listed skills).",
					"enum":        names,
				},
			},
			"required": []string{"command"},
		},
	}, true
}

func skillsTool(args map[string]any) string {
	cmd, _ := args["command"].(string)
	cmd = strings.TrimSpace(cmd)
	if cmd == "" {
		return "Error: 'command' is required — pass the name of one of the listed skills."
	}
	entry, ok := lookupSkill(cmd)
	if !ok {
		return fmt.Sprintf("Error: skill %q not found.", cmd)
	}
	data, err := os.ReadFile(entry.Path)
	if err != nil {
		return fmt.Sprintf("Error loading skill %q: %v", entry.Name, err)
	}
	_, body := splitFrontmatter(string(data))
	return strings.TrimSpace(body)
}
