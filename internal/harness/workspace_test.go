package harness

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEnsureWritableHome(t *testing.T) {
	ws := t.TempDir()

	writable := t.TempDir()
	t.Setenv("HOME", writable)
	if got := EnsureWritableHome(ws); got != writable {
		t.Fatalf("writable HOME should be kept, got %q", got)
	}

	// Read-only HOME (as with readOnlyRootFilesystem) falls back to the workspace.
	ro := filepath.Join(t.TempDir(), "ro")
	if err := os.Mkdir(ro, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(ro, 0o700) })
	if os.Getuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	t.Setenv("HOME", ro)
	if got := EnsureWritableHome(ws); got != ws || os.Getenv("HOME") != ws {
		t.Fatalf("read-only HOME should fall back to workspace, got %q (HOME=%q)", got, os.Getenv("HOME"))
	}

	t.Setenv("HOME", "")
	if got := EnsureWritableHome(ws); got != ws {
		t.Fatalf("empty HOME should fall back to workspace, got %q", got)
	}
}

func TestUseWorkspaceTmp(t *testing.T) {
	ws := t.TempDir()
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(k, "")
	}
	t.Setenv("AGENT_RUN_ID", "run-b")

	// Another run's temp dir from long ago is pruned; a recent one is kept.
	stale := filepath.Join(ws, ".tmp", "run-old")
	recent := filepath.Join(ws, ".tmp", "run-a")
	for _, d := range []string{stale, recent} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-2 * staleTmpAge)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}

	dir, err := UseWorkspaceTmp("test", ws)
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(ws, ".tmp", "run-b"); dir != want {
		t.Fatalf("TMPDIR = %q, want %q", dir, want)
	}
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		if os.Getenv(k) != dir {
			t.Errorf("%s = %q, want %q", k, os.Getenv(k), dir)
		}
	}
	if os.TempDir() != dir {
		t.Errorf("os.TempDir() = %q, want %q", os.TempDir(), dir)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Error("stale temp dir from another run should be pruned")
	}
	if _, err := os.Stat(recent); err != nil {
		t.Error("recent temp dir from another run must be kept")
	}
}

func TestUseWorkspaceTmp_OperatorTMPDIRWins(t *testing.T) {
	custom := t.TempDir()
	t.Setenv("TMPDIR", custom)
	ws := t.TempDir()
	dir, err := UseWorkspaceTmp("test", ws)
	if err != nil || dir != custom {
		t.Fatalf("dir = %q, err = %v; operator TMPDIR should win", dir, err)
	}
	if _, err := os.Stat(filepath.Join(ws, ".tmp")); !os.IsNotExist(err) {
		t.Error("workspace temp root should not be created when TMPDIR is set")
	}
}

func TestWorkspaceSectionSteersAwayFromTmp(t *testing.T) {
	s := WorkspaceSection()
	for _, want := range []string{"/workspace", "Do not write to /tmp", "$TMPDIR"} {
		if !strings.Contains(s, want) {
			t.Errorf("WorkspaceSection missing %q", want)
		}
	}
}
