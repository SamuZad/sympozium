package main

import (
	"strings"
	"testing"
)

func TestRefreshUpgradeValuesMovesDefaultPins(t *testing.T) {
	vals := map[string]interface{}{
		"image": map[string]interface{}{"tag": "v0.10.1"},
		"celln": map[string]interface{}{
			"enabled": true,
			"image":   map[string]interface{}{"repository": defaultCellnInstallerRepo, "tag": "v0.10.1"},
			"router": map[string]interface{}{"image": map[string]interface{}{
				"repository": defaultCellnRouterRepo, "tag": "v0.5.1",
			}},
			"fleet": map[string]interface{}{"enabled": true, "package": map[string]interface{}{"image": "old@sha256:aa"}},
		},
	}
	starter := cellnStarter{Image: "new@sha256:bb", PackageHash: "blake3:cc", Publisher: "pub"}
	notes := refreshUpgradeValues(vals, "", "v0.11.0", starter)

	if _, ok := nestedString(vals, "image", "tag"); ok {
		t.Errorf("image.tag kept; want it dropped so the chart appVersion applies")
	}
	if got, _ := nestedString(vals, "celln", "image", "tag"); got != "v0.11.0" {
		t.Errorf("celln.image.tag = %q, want v0.11.0", got)
	}
	if got, _ := nestedString(vals, "celln", "router", "image", "tag"); got != defaultCellnRouterTag {
		t.Errorf("celln.router.image.tag = %q, want %s", got, defaultCellnRouterTag)
	}
	if got, _ := nestedString(vals, "celln", "fleet", "package", "image"); got != "old@sha256:aa" {
		t.Errorf("fleet package changed to %q; upgrade must leave it to install --celln-fleet-replace-package", got)
	}
	if !strings.Contains(strings.Join(notes, "\n"), "--celln-fleet-replace-package") {
		t.Errorf("notes do not flag the starter package change: %v", notes)
	}
}

func TestRefreshUpgradeValuesKeepsOperatorPins(t *testing.T) {
	vals := map[string]interface{}{
		"celln": map[string]interface{}{
			"enabled": true,
			"image":   map[string]interface{}{"repository": "localhost:5000/installer", "tag": "dev"},
			"router": map[string]interface{}{"image": map[string]interface{}{
				"repository": defaultCellnRouterRepo, "digest": "sha256:dd",
			}},
		},
	}
	refreshUpgradeValues(vals, "custom", "v0.11.0", cellnStarter{})
	if got, _ := nestedString(vals, "image", "tag"); got != "custom" {
		t.Errorf("image.tag = %q, want --image-tag custom", got)
	}
	if got, _ := nestedString(vals, "celln", "image", "tag"); got != "dev" {
		t.Errorf("custom installer image tag moved to %q", got)
	}
	if _, ok := nestedString(vals, "celln", "router", "image", "tag"); ok {
		t.Errorf("digest-pinned router gained a tag")
	}
}

func TestRefreshUpgradeValuesWithoutCelln(t *testing.T) {
	vals := map[string]interface{}{"celln": map[string]interface{}{"enabled": false}}
	if notes := refreshUpgradeValues(vals, "", "v0.11.0", cellnStarter{}); len(notes) != 0 {
		t.Fatalf("notes = %v, want none", notes)
	}
	if _, ok := nestedString(vals, "celln", "image", "tag"); ok {
		t.Fatal("disabled Celln gained an image tag")
	}
}
