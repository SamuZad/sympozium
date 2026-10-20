package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// workspaceTmpDir is the directory under the workspace that holds per-run
// temp dirs.
const workspaceTmpDir = ".tmp"

// staleTmpAge is how old another run's temp dir must be before a new run
// removes it. Runs on a session PVC are normally sequential; the margin
// covers any that overlap.
const staleTmpAge = 24 * time.Hour

// UseWorkspaceForScratch moves HOME (when the image's is read-only) and
// TMPDIR onto the workspace volume, so the wrapped CLI and the tools it runs
// write their caches and temp files there rather than to /tmp. Failures are
// logged, not fatal: the run still works with the defaults.
func UseWorkspaceForScratch(name, workspace string) {
	if prev := os.Getenv("HOME"); EnsureWritableHome(workspace) != prev {
		Logf(name, "HOME %q is not writable; using %s", prev, workspace)
	}
	dir, err := UseWorkspaceTmp(name, workspace)
	if err != nil {
		Logf(name, "keeping default TMPDIR: %v", err)
		return
	}
	Logf(name, "TMPDIR=%s", dir)
}

// EnsureWritableHome makes sure $HOME points at a writable directory. Agent
// containers run with readOnlyRootFilesystem, so the image's /home/<user> is
// not writable, and tools that cache under $HOME (uv, pip, npm, matplotlib,
// git, the wrapped CLI itself) quietly fall back to /tmp instead. When the
// current HOME is unusable it is pointed at fallback (the workspace), so
// those caches land on the workspace volume. Returns the effective HOME.
func EnsureWritableHome(fallback string) string {
	if home := os.Getenv("HOME"); home != "" && dirWritable(home) {
		return home
	}
	_ = os.Setenv("HOME", fallback)
	return fallback
}

func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".harness-probe-*")
	if err != nil {
		return false
	}
	name := f.Name()
	_ = f.Close()
	_ = os.Remove(name)
	return true
}

// UseWorkspaceTmp points TMPDIR (and TMP/TEMP) at a per-run directory under
// <workspace>/.tmp/ so temp files from the wrapped CLI and every tool it runs
// land on the workspace volume instead of the pod's small /tmp emptyDir,
// which is shared with the SkillPack sidecars. Temp dirs other runs left on
// a persistent workspace are pruned once they are older than staleTmpAge.
// The current run's dir is kept after exit: files in it may still be
// referenced by a channel message the IPC bridge has yet to deliver.
//
// An operator-set TMPDIR is left alone. Returns the effective TMPDIR.
func UseWorkspaceTmp(name, workspace string) (string, error) {
	if dir := os.Getenv("TMPDIR"); dir != "" {
		return dir, nil
	}
	root := filepath.Join(workspace, workspaceTmpDir)
	pruneStaleTmp(name, root, time.Now().Add(-staleTmpAge))

	runID := FirstNonEmpty(os.Getenv("AGENT_RUN_ID"), "run")
	dir := filepath.Join(root, filepath.Base(runID))
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", dir, err)
	}
	for _, k := range []string{"TMPDIR", "TMP", "TEMP"} {
		_ = os.Setenv(k, dir)
	}
	return dir, nil
}

func pruneStaleTmp(name, root string, cutoff time.Time) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return
	}
	for _, e := range entries {
		info, err := e.Info()
		if err != nil || info.ModTime().After(cutoff) {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, e.Name())); err != nil {
			Logf(name, "could not prune stale temp dir %s: %v", e.Name(), err)
		}
	}
}

// WorkspaceSection steers the wrapped agent to keep the files it creates on
// the workspace volume. Left to themselves coding agents reach for /tmp,
// which in a Sympozium pod is a small emptyDir shared with the sidecars:
// it fills up (sidecar package caches live there too) and nothing in it
// outlives the pod.
func WorkspaceSection() string {
	return "# Files\n\n" +
		"Create every file you produce — scripts, downloaded or generated data, intermediate results, charts, " +
		"documents — under /workspace (your working directory), using subdirectories when there are several. " +
		"Do not write to /tmp: in this pod it is a small scratch volume shared with the SkillPack sidecars, it fills up quickly, " +
		"and it is discarded with the pod, while /workspace has more room and may persist to your next run. " +
		"`sympozium-tool exec` runs sidecar commands in /workspace by default, so sidecars that mount it see the same files; " +
		"use /tmp only to hand a file to or from a sidecar that cannot see /workspace. " +
		"$TMPDIR already points at a run-scoped directory under /workspace/.tmp for tools that need temp files.\n\n"
}
