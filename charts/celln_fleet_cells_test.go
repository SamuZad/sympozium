package charts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The node reporter reads a parent journal into turns with their worker
// cells and outcomes, and never copies a task or an answer.
func TestFleetCellsReportsParentsWithoutContent(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	root := t.TempDir()
	parent := filepath.Join(root, "parent-journal", strings.Repeat("d", 64))
	if err := os.MkdirAll(parent, 0700); err != nil {
		t.Fatal(err)
	}
	write := func(name string, v any) {
		raw, _ := json.Marshal(v)
		if err := os.WriteFile(filepath.Join(parent, name), raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	incarnation := "blake3:" + strings.Repeat("a", 64)
	write("parent.json", map[string]any{"version": 1, "parent": incarnation, "recovery": "context-lost-without-live-owner"})
	write("t1.reserved.json", map[string]any{"parent": incarnation, "child": "blake3:c1", "request": map[string]any{"turnId": "initial", "task": "SECRET TASK"}, "timeout_nanos": 60_000_000_000})
	write("t1.destroyed.json", map[string]any{"parent": incarnation, "child": "blake3:c1", "turnId": "initial", "succeeded": true, "answer": "SECRET ANSWER"})
	write("t1.committed.json", map[string]any{"parent": incarnation, "child": "blake3:c1", "turnId": "initial"})
	write("t2.reserved.json", map[string]any{"parent": incarnation, "child": "blake3:c2", "request": map[string]any{"turnId": "turn-2"}})
	write("t2.destroyed.json", map[string]any{"parent": incarnation, "child": "blake3:c2", "turnId": "turn-2", "succeeded": false, "answer": "SECRET"})

	script := `import importlib.util, json, sys
spec = importlib.util.spec_from_file_location("fc", "sympozium/files/celln/fleet-cells.py")
fc = importlib.util.module_from_spec(spec); spec.loader.exec_module(fc)
print(json.dumps(fc.parents(sys.argv[1])))`
	cmd := exec.Command("python3", "-c", script, root)
	// Importing the chart file must not leave bytecode inside the chart.
	cmd.Env = append(os.Environ(), "PYTHONDONTWRITEBYTECODE=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	if strings.Contains(string(out), "SECRET") {
		t.Fatalf("report copies turn content: %s", out)
	}
	var parents []struct {
		Incarnation string `json:"incarnation"`
		Turns       []struct {
			TurnID, Stage, Child string
			Succeeded            *bool
			TimeoutMs            int64
		} `json:"turns"`
	}
	if err := json.Unmarshal(out, &parents); err != nil || len(parents) != 1 || parents[0].Incarnation != incarnation || len(parents[0].Turns) != 2 {
		t.Fatalf("parents: %s %v", out, err)
	}
	first, second := parents[0].Turns[0], parents[0].Turns[1]
	if first.TurnID != "initial" || first.Stage != "committed" || first.Child != "blake3:c1" || first.Succeeded == nil || !*first.Succeeded || first.TimeoutMs != 60000 {
		t.Fatalf("committed turn: %+v", first)
	}
	if second.TurnID != "turn-2" || second.Stage != "child-destroyed" || second.Succeeded == nil || *second.Succeeded {
		t.Fatalf("failed turn: %+v", second)
	}
}

// Configure pods keep reporting cells after preparing the node, and may
// write only the fleet's two ConfigMaps.
func TestFleetRunsTheCellsReporter(t *testing.T) {
	raw, err := renderNativeParent(t, fleetValues())
	if err != nil {
		t.Fatalf("render: %v: %s", err, raw)
	}
	text := string(raw)
	if !strings.Contains(text, "fleet-cells.sh: |") || !strings.Contains(text, "fleet-cells.py: |") {
		t.Fatal("celln-fleet-prepare does not carry the cells reporter")
	}
	if !strings.Contains(text, `resourceNames: ["celln-fleet-configuration", "celln-fleet-cells"]`) {
		t.Fatal("node role does not name exactly the configuration and cells ConfigMaps")
	}
	configure := decodeFleet(t, raw).daemonSets["celln-node-configure"].Spec.Template.Spec.Containers[0]
	if !strings.Contains(strings.Join(configure.Command, " "), "prepare.sh && exec /bin/bash /etc/celln-fleet/fleet-cells.sh") {
		t.Fatalf("configure pod does not run the reporter: %v", configure.Command)
	}
	env := map[string]string{}
	for _, e := range configure.Env {
		env[e.Name] = e.Value
	}
	mounts := map[string]bool{}
	for _, m := range configure.VolumeMounts {
		mounts[m.MountPath] = m.ReadOnly
	}
	if env["FLEET_CELLS_CONFIGMAP"] != "celln-fleet-cells" || !mounts["/etc/celln-fleet/fleet-cells.sh"] || !mounts["/etc/celln-fleet/fleet-cells.py"] {
		t.Fatalf("reporter not wired: env=%v mounts=%v", env, mounts)
	}
}
