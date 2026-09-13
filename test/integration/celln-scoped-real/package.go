package main

import (
	"bufio"
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	api "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/zeebo/blake3"
	kyaml "k8s.io/apimachinery/pkg/util/yaml"
)

const packageVersion = "celln.framework-native-package/v1"

type packageResourceRef struct {
	Name, Revision string
}

type packageManifest struct {
	APIVersion string `json:"apiVersion"`
	Artifacts  []struct {
		Name, EntryPoint, Executable, Closure, Mote, Initrd, Toolfs, PublisherKey, SourceClosure string
	} `json:"artifacts"`
	Contracts struct{ Direct, Model, Tool string }                `json:"contracts"`
	Inputs    struct{ OneShot, Worker, Parent, Uppercase string } `json:"inputs"`
	Resources struct {
		File, ParentRequest string
		Profiles            []packageResourceRef
		Tools               []packageResourceRef
	} `json:"resources"`
	Schemas struct {
		Arguments, Result, Profile string
		MaxBlobBytes               int64
	} `json:"schemas"`
}

type parentArtifact struct {
	APIVersion string          `json:"apiVersion"`
	Source     json.RawMessage `json:"source"`
	Artifact   struct {
		Executable    api.CellnImmutableRef `json:"executable"`
		Closure       api.CellnImmutableRef `json:"closure"`
		Mote          api.CellnImmutableRef `json:"mote"`
		SourceClosure api.CellnImmutableRef `json:"sourceClosure"`
		PublisherKey  string                `json:"publisherKey"`
		EntryPoint    string                `json:"entryPoint"`
		Platform      string                `json:"platform"`
		Lane          string                `json:"lane"`
	} `json:"artifact"`
	Limits struct {
		MemoryBytes int64    `json:"memoryBytes"`
		Workspace   string   `json:"workspace"`
		Egress      []string `json:"egress"`
	} `json:"limits"`
	OperatorMetadataOnly bool `json:"operatorMetadataOnly"`
	RunAuthority         bool `json:"runAuthority"`
}

type nativePackage struct {
	Hash              string
	OneShot, Enduring api.CellnRuntimeProfile
	Uppercase         api.ClusterCellnTool
	ParentRequestFile string
	Parent            parentArtifact
}

func loadNativePackage(root string) (nativePackage, error) {
	var out nativePackage
	if !filepath.IsAbs(root) {
		return out, errors.New("artifact package must be absolute")
	}
	if err := verifyPackageFiles(root); err != nil {
		return out, err
	}
	packageRaw, err := readBounded(filepath.Join(root, "package.json"), 1<<20)
	if err != nil {
		return out, fmt.Errorf("read package.json: %w", err)
	}
	var manifest packageManifest
	if err := json.Unmarshal(packageRaw, &manifest); err != nil {
		return out, fmt.Errorf("decode package.json: %w", err)
	}
	out.Hash = fmt.Sprintf("blake3:%x", blake3.Sum256(packageRaw))
	if manifest.APIVersion != packageVersion || manifest.Contracts.Direct != "celln.json-direct/v1" || manifest.Contracts.Model != "celln.json-tools/v1" || manifest.Contracts.Tool != "celln.json-stdio/v1" || manifest.Resources.File != "resources.yaml" || manifest.Resources.ParentRequest != "parent-request.json" || len(manifest.Resources.Profiles) != 2 || len(manifest.Resources.Tools) != 1 || manifest.Resources.Tools[0].Name != "uppercase" || manifest.Schemas.Profile != "celln.tool-schema/v1" || manifest.Schemas.MaxBlobBytes != 32768 {
		return out, errors.New("package.json does not expose the fixed framework native package contract")
	}
	type artifactIdentity struct{ entry, executable, closure, mote, initrd, toolfs, publisher, sourceClosure string }
	artifacts := map[string]artifactIdentity{}
	for _, a := range manifest.Artifacts {
		if _, exists := artifacts[a.Name]; exists {
			return out, fmt.Errorf("duplicate package artifact %q", a.Name)
		}
		artifacts[a.Name] = artifactIdentity{a.EntryPoint, a.Executable, a.Closure, a.Mote, a.Initrd, a.Toolfs, a.PublisherKey, a.SourceClosure}
	}
	if len(artifacts) != 3 {
		return out, errors.New("package must expose exactly three native artifacts")
	}
	for name, expected := range map[string][2]string{"one-shot": {"/harness", manifest.Inputs.OneShot}, "enduring-worker": {"/worker", manifest.Inputs.Worker}, "parent": {"/parent", manifest.Inputs.Parent}} {
		a, ok := artifacts[name]
		if !ok || a.entry != expected[0] || a.executable != expected[1] || a.closure == "" || a.mote == "" || a.initrd == "" || a.toolfs == "" || a.publisher == "" || a.sourceClosure == "" {
			return out, fmt.Errorf("package artifact %q identity mismatch", name)
		}
		for file, want := range map[string]string{"executable": a.executable, "signed-closure.json": a.closure, "mote.json": a.mote, "initrd": a.initrd, "toolfs.ext2": a.toolfs} {
			got, hashErr := packageFileHash(filepath.Join(root, "artifacts", name, file))
			if hashErr != nil || got != want {
				return out, fmt.Errorf("package artifact %q member %q mismatch", name, file)
			}
		}
	}
	if manifest.Inputs.Uppercase == "" {
		return out, errors.New("package uppercase executable identity is empty")
	}

	resourcesRaw, err := readBounded(filepath.Join(root, manifest.Resources.File), 1<<20)
	if err != nil {
		return out, fmt.Errorf("read resources: %w", err)
	}
	decoder := kyaml.NewYAMLOrJSONDecoder(bytes.NewReader(resourcesRaw), 64<<10)
	seen := 0
	for {
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err == io.EOF {
			break
		} else if err != nil {
			return out, fmt.Errorf("decode resources: %w", err)
		}
		if len(bytes.TrimSpace(raw)) == 0 {
			continue
		}
		var kind struct{ APIVersion, Kind string }
		if err := json.Unmarshal(raw, &kind); err != nil || kind.APIVersion != api.GroupVersion.String() {
			return out, errors.New("resources contain an invalid API object")
		}
		switch kind.Kind {
		case "CellnRuntimeProfile":
			var value api.CellnRuntimeProfile
			if err := strictJSON(raw, &value); err != nil {
				return out, err
			}
			if value.Name == "celln-json-one-shot" {
				out.OneShot = value
			} else if value.Name == "celln-json-enduring" {
				out.Enduring = value
			} else {
				return out, fmt.Errorf("unexpected runtime profile %q", value.Name)
			}
		case "ClusterCellnTool":
			if err := strictJSON(raw, &out.Uppercase); err != nil {
				return out, err
			}
		default:
			return out, fmt.Errorf("unexpected package resource kind %q", kind.Kind)
		}
		seen++
	}
	if seen != 3 || out.OneShot.Name == "" || out.Enduring.Name == "" || out.Uppercase.Name != "uppercase" {
		return out, errors.New("resources must contain exactly the two runtimes and uppercase tool")
	}
	if manifest.Resources.Profiles[0].Name != "celln-json-one-shot" || manifest.Resources.Profiles[1].Name != "celln-json-enduring" || out.OneShot.Spec.Revision != manifest.Resources.Profiles[0].Revision || out.Enduring.Spec.Revision != manifest.Resources.Profiles[1].Revision || out.Uppercase.Spec.Revision != manifest.Resources.Tools[0].Revision || out.OneShot.Spec.Executable.Hash != artifacts["one-shot"].executable || out.OneShot.Spec.Closure.Hash != artifacts["one-shot"].closure || out.OneShot.Spec.Mote.Hash != artifacts["one-shot"].mote || out.OneShot.Spec.PublisherKey != artifacts["one-shot"].publisher || out.Enduring.Spec.Executable.Hash != artifacts["enduring-worker"].executable || out.Enduring.Spec.Closure.Hash != artifacts["enduring-worker"].closure || out.Enduring.Spec.Mote.Hash != artifacts["enduring-worker"].mote || out.Enduring.Spec.PublisherKey != artifacts["enduring-worker"].publisher || out.Uppercase.Spec.Executable.Hash != manifest.Inputs.Uppercase || out.Uppercase.Spec.InvocationABI != manifest.Contracts.Tool || out.Uppercase.Spec.ArgumentsSchema.Hash != manifest.Schemas.Arguments || out.Uppercase.Spec.ResultSchema.Hash != manifest.Schemas.Result {
		return out, errors.New("resources.yaml content does not match package.json identities")
	}
	parentRaw, err := readBounded(filepath.Join(root, manifest.Resources.ParentRequest), 64<<10)
	if err != nil {
		return out, fmt.Errorf("read parent request: %w", err)
	}
	if err := strictJSON(parentRaw, &out.Parent); err != nil {
		return out, fmt.Errorf("decode parent request: %w", err)
	}
	pa := artifacts["parent"]
	if out.Parent.APIVersion != "celln.native-scoped-parent-artifact/v1" || !out.Parent.OperatorMetadataOnly || out.Parent.RunAuthority || out.Parent.Artifact.Executable.Hash != pa.executable || out.Parent.Artifact.Closure.Hash != pa.closure || out.Parent.Artifact.Mote.Hash != pa.mote || out.Parent.Artifact.SourceClosure.Hash != pa.sourceClosure || out.Parent.Artifact.PublisherKey != pa.publisher || out.Parent.Artifact.EntryPoint != pa.entry || out.Parent.Artifact.Platform != "linux/amd64" || out.Parent.Artifact.Lane != "agent" || out.Parent.Limits.MemoryBytes < 1 || out.Parent.Limits.Workspace != "none" || len(out.Parent.Limits.Egress) != 0 {
		return out, errors.New("parent-request.json does not match the package parent artifact")
	}
	out.ParentRequestFile = filepath.Join(root, manifest.Resources.ParentRequest)
	return out, nil
}

func packageFileHash(path string) (string, error) {
	raw, err := readBounded(path, 64<<20)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("blake3:%x", blake3.Sum256(raw)), nil
}

func strictJSON(raw []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("trailing JSON value")
	}
	return nil
}

func verifyPackageFiles(root string) error {
	raw, err := readBounded(filepath.Join(root, "MANIFEST.blake3"), 4<<20)
	if err != nil {
		return fmt.Errorf("read package file manifest: %w", err)
	}
	expected := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		hash, rel, ok := strings.Cut(scanner.Text(), "  ")
		decoded, hexErr := hex.DecodeString(hash)
		clean := filepath.Clean(rel)
		if !ok || hexErr != nil || len(decoded) != 32 || clean != rel || filepath.IsAbs(rel) || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("invalid package file manifest entry")
		}
		if _, duplicate := expected[rel]; duplicate {
			return errors.New("duplicate package file manifest entry")
		}
		expected[rel] = strings.ToLower(hash)
	}
	if err := scanner.Err(); err != nil {
		return err
	}
	actual := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == root {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("package contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		if rel == "MANIFEST.blake3" {
			return nil
		}
		want, ok := expected[rel]
		if !ok {
			return fmt.Errorf("package manifest omits %q", rel)
		}
		contents, err := readBounded(path, 64<<20)
		if err != nil {
			return err
		}
		if fmt.Sprintf("%x", blake3.Sum256(contents)) != want {
			return fmt.Errorf("package file identity mismatch: %s", rel)
		}
		actual[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(actual) != len(expected) {
		return errors.New("package manifest names missing files")
	}
	return nil
}
