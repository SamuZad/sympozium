package charts

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	packageA = "blake3:" + "aa"
	packageB = "blake3:" + "bb"
)

func configMap(annotations map[string]string, data map[string]string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{"name": "celln-fleet-configuration", "resourceVersion": "7", "annotations": annotations},
		"data":     data,
	}
}

func nodeBody(pkg, scope string, data map[string]string) map[string]any {
	return map[string]any{"metadata": map[string]any{"annotations": map[string]string{
		"celln.sympozium.ai/package": pkg, "celln.sympozium.ai/scope": scope, "celln.sympozium.ai/published-by": "node-1",
	}}, "data": data}
}

// publish runs the node's publish decision (files/celln/fleet-publish.py) and
// returns the merge patch it chose, or the refusal.
func publish(t *testing.T, published, body map[string]any) (map[string]any, string) {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	dir := t.TempDir()
	write := func(name string, v any) string {
		raw, _ := json.Marshal(v)
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	patchPath := filepath.Join(dir, "patch.json")
	out, err := exec.Command("python3", "sympozium/files/celln/fleet-publish.py", write("published.json", published), write("body.json", body), patchPath).CombinedOutput()
	if err != nil {
		return nil, strings.TrimSpace(string(out))
	}
	raw, _ := os.ReadFile(patchPath)
	if len(raw) == 0 {
		return map[string]any{}, ""
	}
	var patch map[string]any
	if err := json.Unmarshal(raw, &patch); err != nil {
		t.Fatalf("patch %s: %v", raw, err)
	}
	return patch, ""
}

func TestFleetPublishExtendsTheSamePackageOnly(t *testing.T) {
	published := configMap(map[string]string{"celln.sympozium.ai/package": packageA, "celln.sympozium.ai/scope": "starter"},
		map[string]string{"native.catalogue.json": "cat-a"})
	patch, refusal := publish(t, published, nodeBody(packageA, "starter", map[string]string{"native.catalogue.json": "cat-a", "claude.catalogue.json": "cat-a-claude"}))
	if refusal != "" || patch["data"].(map[string]any)["claude.catalogue.json"] != "cat-a-claude" || patch["data"].(map[string]any)["native.catalogue.json"] != nil {
		t.Fatalf("a new backend of the same package was not added alone: %v %s", patch, refusal)
	}
	if patch, refusal := publish(t, published, nodeBody(packageA, "starter", map[string]string{"native.catalogue.json": "cat-a"})); refusal != "" || len(patch) != 0 {
		t.Fatalf("an identical publication changed something: %v %s", patch, refusal)
	}
	if _, refusal := publish(t, published, nodeBody(packageA, "starter", map[string]string{"native.catalogue.json": "other"})); !strings.Contains(refusal, "both carry package") {
		t.Fatalf("differing files of one package accepted: %s", refusal)
	}
}

func TestFleetPublishRecordsTheScopeOfALegacyPublication(t *testing.T) {
	// Published before backends were named and before scopes were recorded.
	published := configMap(map[string]string{"celln.sympozium.ai/package": packageA}, map[string]string{"catalogue.json": "cat-a"})
	patch, refusal := publish(t, published, nodeBody(packageA, "starter", map[string]string{"native.catalogue.json": "cat-a"}))
	if refusal != "" || patch["data"] != nil || patch["metadata"].(map[string]any)["annotations"].(map[string]any)["celln.sympozium.ai/scope"] != "starter" {
		t.Fatalf("legacy publication not recognised: %v %s", patch, refusal)
	}
}

func TestFleetPublishReplacesOnlyTheApprovedPackage(t *testing.T) {
	old := map[string]string{"celln.sympozium.ai/package": packageA, "celln.sympozium.ai/scope": "starter"}
	data := map[string]string{"native.catalogue.json": "cat-a", "gone.catalogue.json": "cat-a-gone"}
	ours := nodeBody(packageB, "starter", map[string]string{"native.catalogue.json": "cat-b"})

	if _, refusal := publish(t, configMap(old, data), ours); !strings.Contains(refusal, "--celln-fleet-replace-package") {
		t.Fatalf("unapproved package change not refused with the remedy: %s", refusal)
	}

	approved := map[string]string{"celln.sympozium.ai/desired-package": packageB, "celln.sympozium.ai/desired-scope": "starter"}
	for k, v := range old {
		approved[k] = v
	}
	patch, refusal := publish(t, configMap(approved, data), ours)
	if refusal != "" {
		t.Fatalf("approved replacement refused: %s", refusal)
	}
	meta := patch["metadata"].(map[string]any)
	patched := patch["data"].(map[string]any)
	if meta["resourceVersion"] != "7" || meta["annotations"].(map[string]any)["celln.sympozium.ai/package"] != packageB ||
		patched["native.catalogue.json"] != "cat-b" || patched["gone.catalogue.json"] != nil {
		t.Fatalf("replacement is not a conditional, complete swap: %v", patch)
	}
	if _, present := patched["gone.catalogue.json"]; !present {
		t.Fatal("a backend the new package lacks was not removed")
	}

	// A node still on the old package waits once the new one is approved…
	if _, refusal := publish(t, configMap(approved, data), nodeBody(packageA, "starter", map[string]string{"native.catalogue.json": "cat-a"})); refusal != "" {
		t.Fatalf("old node refused before the replacement landed: %s", refusal)
	}
	// …and never publishes over it afterwards.
	replaced := map[string]string{"celln.sympozium.ai/package": packageB, "celln.sympozium.ai/scope": "starter", "celln.sympozium.ai/desired-package": packageB, "celln.sympozium.ai/desired-scope": "starter"}
	if _, refusal := publish(t, configMap(replaced, map[string]string{"native.catalogue.json": "cat-b"}), nodeBody(packageA, "starter", map[string]string{"native.catalogue.json": "cat-a"})); !strings.Contains(refusal, "waits for its configure pod") {
		t.Fatalf("old node published over the approved package: %s", refusal)
	}
}

func TestFleetPublishReplacesOnApprovedScopeChange(t *testing.T) {
	annotations := map[string]string{"celln.sympozium.ai/package": packageA, "celln.sympozium.ai/scope": "starter"}
	ours := nodeBody(packageA, "second", map[string]string{"native.catalogue.json": "cat-a-second"})
	if _, refusal := publish(t, configMap(annotations, map[string]string{"native.catalogue.json": "cat-a"}), ours); refusal == "" {
		t.Fatal("unapproved scope change accepted")
	}
	annotations["celln.sympozium.ai/desired-package"] = packageA
	annotations["celln.sympozium.ai/desired-scope"] = "second"
	patch, refusal := publish(t, configMap(annotations, map[string]string{"native.catalogue.json": "cat-a"}), ours)
	if refusal != "" || patch["data"].(map[string]any)["native.catalogue.json"] != "cat-a-second" {
		t.Fatalf("approved scope change not replaced: %v %s", patch, refusal)
	}
}

// The configure pods carry the publish decision next to prepare.sh, and roll
// when the scope changes.
func TestFleetShipsThePublishDecision(t *testing.T) {
	raw, err := renderNativeParent(t, fleetValues())
	if err != nil {
		t.Fatalf("render: %v: %s", err, raw)
	}
	if !strings.Contains(string(raw), "fleet-publish.py: |") || !strings.Contains(string(raw), "def decide(published, body)") {
		t.Fatal("celln-fleet-prepare does not carry fleet-publish.py")
	}
	configure := decodeFleet(t, raw).daemonSets["celln-node-configure"].Spec.Template
	mounted := false
	for _, m := range configure.Spec.Containers[0].VolumeMounts {
		mounted = mounted || (m.MountPath == "/etc/celln-fleet/fleet-publish.py" && m.SubPath == "fleet-publish.py" && m.ReadOnly)
	}
	if !mounted || configure.Annotations["celln.sympozium.ai/scope"] != "starter" {
		t.Fatalf("publish decision not mounted or scope not on the pod template: %+v", configure.Annotations)
	}
}
