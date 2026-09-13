package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadActualFrameworkNativePackage(t *testing.T) {
	path := os.Getenv("CELLN_FRAMEWORK_PACKAGE")
	if path == "" {
		path = "/tmp/celln-framework-package/target/framework-native-package"
	}
	if _, err := os.Stat(path); os.IsNotExist(err) {
		t.Skip("external framework package reference is not present")
	}
	pkg, err := loadNativePackage(path)
	if err != nil {
		t.Fatal(err)
	}
	if pkg.OneShot.Name != "celln-json-one-shot" || pkg.Enduring.Name != "celln-json-enduring" || pkg.Uppercase.Name != "uppercase" || pkg.Hash == "" {
		t.Fatalf("unexpected package identities: %+v", pkg)
	}
	parent := filepath.Join(t.TempDir(), "parent-template.json")
	if err := writeParentTemplate(parent, pkg); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(parent)
	if err != nil {
		t.Fatal(err)
	}
	var wrapper struct {
		APIVersion string `json:"apiVersion"`
		Reserved   uint64 `json:"reservedMemoryBytes"`
		Request    struct {
			ID    string `json:"id"`
			Tools []struct {
				Alias, Hash string
				Closure     struct{ Hash string }
			} `json:"tools"`
			Mote struct{ Hash string } `json:"mote"`
		} `json:"request"`
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		t.Fatal(err)
	}
	if wrapper.APIVersion != "celln.scoped-parent-template/v1" || wrapper.Request.ID != "$parent" || wrapper.Reserved <= uint64(pkg.Parent.Limits.MemoryBytes)*2 || len(wrapper.Request.Tools) != 1 || wrapper.Request.Tools[0].Alias != pkg.Parent.Artifact.EntryPoint || wrapper.Request.Tools[0].Hash != pkg.Parent.Artifact.Executable.Hash || wrapper.Request.Tools[0].Closure.Hash != pkg.Parent.Artifact.Closure.Hash || wrapper.Request.Mote.Hash != pkg.Parent.Artifact.Mote.Hash {
		t.Fatalf("parent wrapper did not preserve package artifact: %s", raw)
	}
}
