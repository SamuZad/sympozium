package sidecartools

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

type executorResult struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	TimedOut bool   `json:"timedOut"`
}

// runExecutor starts ExecutorScript against a temporary IPC directory, writes
// the given requests, and returns the results that appear before the deadline.
// A request with no result was never claimed.
func runExecutor(t *testing.T, env []string, requests map[string]map[string]any, wait time.Duration) map[string]executorResult {
	t.Helper()
	for _, bin := range []string{"bash", "jq", "timeout", "base64"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not available, skipping tool-executor script test", bin)
		}
	}

	dir := t.TempDir()
	script := filepath.Join(dir, ExecutorScriptName)
	if err := os.WriteFile(script, []byte(ExecutorScript), 0o755); err != nil {
		t.Fatal(err)
	}
	ipc := filepath.Join(dir, "ipc")
	tools := filepath.Join(ipc, "tools")
	if err := os.MkdirAll(tools, 0o755); err != nil {
		t.Fatal(err)
	}
	for id, req := range requests {
		data, _ := json.Marshal(req)
		if err := os.WriteFile(filepath.Join(tools, "exec-request-"+id+".json"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	cmd := exec.Command("bash", script)
	cmd.Env = append(os.Environ(), append(env, "SYMPOZIUM_IPC_DIR="+ipc)...)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() {
		_ = os.WriteFile(filepath.Join(ipc, "done"), []byte("done"), 0o644)
		select {
		case <-exited:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			t.Error("tool-executor did not exit after the done marker")
		}
	})

	results := map[string]executorResult{}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) && len(results) < len(requests) {
		for id := range requests {
			data, err := os.ReadFile(filepath.Join(tools, "exec-result-"+id+".json"))
			if err != nil {
				continue
			}
			var res executorResult
			if err := json.Unmarshal(data, &res); err != nil {
				t.Fatalf("result %s is not valid JSON: %v\n%s", id, err, data)
			}
			results[id] = res
		}
		time.Sleep(50 * time.Millisecond)
	}
	return results
}

func TestExecutorScript_ShellAndArgvModes(t *testing.T) {
	results := runExecutor(t, nil, map[string]map[string]any{
		"shell": {"command": "printf 'a b' | tr a-z A-Z", "workDir": "/"},
		// One argv element containing a newline must stay one argument.
		"argv": {"argv": []string{"printf", "<%s>", "x\ny"}, "workDir": "/"},
	}, 10*time.Second)

	if got := results["shell"]; got.ExitCode != 0 || got.Stdout != "A B" {
		t.Errorf("shell mode result = %+v, want stdout %q", got, "A B")
	}
	if got := results["argv"]; got.ExitCode != 0 || got.Stdout != "<x\ny>" {
		t.Errorf("argv mode result = %+v, want stdout %q", got, "<x\ny>")
	}
}

// TOOL_EXECUTOR_MAX_TIMEOUT caps a request's own timeout; the llmfit image
// raises it to 180.
func TestExecutorScript_MaxTimeout(t *testing.T) {
	results := runExecutor(t, []string{"TOOL_EXECUTOR_MAX_TIMEOUT=1"}, map[string]map[string]any{
		"slow": {"command": "sleep 10", "timeout": 60, "workDir": "/"},
	}, 8*time.Second)

	got, ok := results["slow"]
	if !ok {
		t.Fatal("request was not cut off at TOOL_EXECUTOR_MAX_TIMEOUT")
	}
	if !got.TimedOut {
		t.Errorf("result = %+v, want timedOut", got)
	}
}

// A sidecar claims only requests addressed to its SkillPack, or to no one.
func TestExecutorScript_TargetRouting(t *testing.T) {
	results := runExecutor(t, []string{"SYMPOZIUM_SKILL_PACK=k8s-ops"}, map[string]map[string]any{
		"mine":   {"command": "echo mine", "target": "K8s-Ops", "workDir": "/"},
		"anyone": {"command": "echo anyone", "workDir": "/"},
		"other":  {"command": "echo other", "target": "github-gitops", "workDir": "/"},
	}, 3*time.Second)

	for _, id := range []string{"mine", "anyone"} {
		if _, ok := results[id]; !ok {
			t.Errorf("request %q was not claimed", id)
		}
	}
	if _, ok := results["other"]; ok {
		t.Error("request for another sidecar's target was claimed")
	}
}
