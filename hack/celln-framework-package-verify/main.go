// Command celln-framework-package-verify verifies a package's exact
// MANIFEST.blake3 file set without installing any authority.
package main

import (
	"bufio"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/zeebo/blake3"
)

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: celln-framework-package-verify ABSOLUTE_PACKAGE")
		os.Exit(2)
	}
	if err := verify(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func verify(root string) error {
	if !filepath.IsAbs(root) {
		return errors.New("absolute package path required")
	}
	raw, err := os.ReadFile(filepath.Join(root, "MANIFEST.blake3"))
	if err != nil || len(raw) == 0 || len(raw) > 4<<20 {
		return errors.New("invalid MANIFEST.blake3")
	}
	want := map[string]string{}
	s := bufio.NewScanner(strings.NewReader(string(raw)))
	s.Buffer(make([]byte, 4096), 1<<20)
	for s.Scan() {
		hash, rel, ok := strings.Cut(s.Text(), "  ")
		decoded, decodeErr := hex.DecodeString(hash)
		clean := filepath.Clean(rel)
		if !ok || decodeErr != nil || len(decoded) != 32 || clean != rel || filepath.IsAbs(rel) || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return errors.New("invalid package manifest entry")
		}
		if _, exists := want[rel]; exists {
			return errors.New("duplicate package manifest entry")
		}
		want[rel] = strings.ToLower(hash)
	}
	if err := s.Err(); err != nil {
		return err
	}
	seen := map[string]bool{}
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || path == root {
			return walkErr
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return errors.New("package contains a symlink")
		}
		if entry.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil || rel == "MANIFEST.blake3" {
			return err
		}
		expected, ok := want[rel]
		if !ok {
			return fmt.Errorf("package manifest omits %q", rel)
		}
		contents, err := os.ReadFile(path)
		if err != nil || len(contents) > 64<<20 {
			return fmt.Errorf("invalid package member %q", rel)
		}
		if fmt.Sprintf("%x", blake3.Sum256(contents)) != expected {
			return fmt.Errorf("package member identity mismatch: %s", rel)
		}
		seen[rel] = true
		return nil
	})
	if err != nil {
		return err
	}
	if len(seen) != len(want) {
		return errors.New("package manifest names missing files")
	}
	return nil
}
