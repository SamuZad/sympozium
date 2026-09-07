package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// fakeScheduleController plays the bridge+controller: it watches the IPC dir
// for a request and answers it with the given result builder.
func fakeScheduleController(t *testing.T, dir string, answer func(req ipc.ScheduleRequest) ipc.ScheduleResult) {
	t.Helper()
	go func() {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			matches, _ := filepath.Glob(filepath.Join(dir, ipc.ScheduleRequestPrefix+"*.json"))
			for _, m := range matches {
				data, err := os.ReadFile(m)
				if err != nil || len(data) == 0 {
					continue
				}
				var req ipc.ScheduleRequest
				if json.Unmarshal(data, &req) != nil || req.ID == "" {
					continue
				}
				res := answer(req)
				res.ID, res.Action = req.ID, req.Action
				out, _ := json.Marshal(res)
				_ = os.WriteFile(filepath.Join(dir, ipc.ScheduleResultPrefix+req.ID+".json"), out, 0o644)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()
}

func withScheduleIPC(t *testing.T, timeout time.Duration) string {
	t.Helper()
	dir := t.TempDir()
	origDir, origTimeout := ipcSchedulesDir, scheduleResultTimeout
	ipcSchedulesDir, scheduleResultTimeout = dir, timeout
	t.Cleanup(func() { ipcSchedulesDir, scheduleResultTimeout = origDir, origTimeout })
	return dir
}

func TestScheduleTaskTool_RoundTripReturnsAppliedState(t *testing.T) {
	dir := withScheduleIPC(t, 3*time.Second)
	var seen ipc.ScheduleRequest
	fakeScheduleController(t, dir, func(req ipc.ScheduleRequest) ipc.ScheduleResult {
		seen = req
		return ipc.ScheduleResult{OK: true, Message: `Schedule "daily" created with cron "0 9 * * *".`,
			Schedule: &ipc.ScheduleInfo{Name: "helper-daily", ShortName: "daily", Schedule: "0 9 * * *"}}
	})

	out := scheduleTaskTool(map[string]any{"name": "daily", "action": "create", "schedule": "0 9 * * *", "task": "report"})

	if seen.Name != "daily" || seen.Action != "create" || seen.Schedule != "0 9 * * *" || seen.Task != "report" {
		t.Fatalf("request not relayed faithfully: %+v", seen)
	}
	if !strings.Contains(out, `Schedule "daily" created`) || !strings.Contains(out, "CR helper-daily") {
		t.Fatalf("tool output should be the controller's applied state:\n%s", out)
	}
	if strings.Contains(out, "Unconfirmed") {
		t.Fatalf("confirmed reply must not be flagged unconfirmed:\n%s", out)
	}
	// Request and reply files are cleaned up.
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatalf("expected IPC dir to be cleaned, found %d files", len(entries))
	}
}

func TestScheduleTaskTool_ControllerErrorIsSurfaced(t *testing.T) {
	dir := withScheduleIPC(t, 3*time.Second)
	fakeScheduleController(t, dir, func(ipc.ScheduleRequest) ipc.ScheduleResult {
		return ipc.ScheduleResult{OK: false, Error: `schedule "ghost" not found`}
	})
	out := scheduleTaskTool(map[string]any{"name": "ghost", "action": "delete"})
	if !strings.HasPrefix(out, "Error: schedule delete failed") || !strings.Contains(out, "not found") {
		t.Fatalf("controller error not surfaced:\n%s", out)
	}
}

func TestScheduleTaskTool_ListNeedsNoName(t *testing.T) {
	dir := withScheduleIPC(t, 3*time.Second)
	fakeScheduleController(t, dir, func(req ipc.ScheduleRequest) ipc.ScheduleResult {
		if req.Name != "" {
			t.Errorf("list should not send a name, got %q", req.Name)
		}
		return ipc.ScheduleResult{OK: true, Schedules: []ipc.ScheduleInfo{{Name: "helper-daily", ShortName: "daily", Schedule: "0 9 * * *"}}}
	})
	out := scheduleTaskTool(map[string]any{"action": "list"})
	if !strings.Contains(out, "1 schedule(s)") || !strings.Contains(out, "- daily") {
		t.Fatalf("list output unexpected:\n%s", out)
	}
}

func TestScheduleTaskTool_TimeoutDegradesGracefully(t *testing.T) {
	withScheduleIPC(t, 200*time.Millisecond) // nobody answers

	// Mutations keep the historical optimistic message, but flagged.
	out := scheduleTaskTool(map[string]any{"name": "daily", "action": "suspend"})
	if !strings.Contains(out, "Schedule 'daily' suspended") || !strings.Contains(out, "Unconfirmed") {
		t.Fatalf("unconfirmed mutation message unexpected:\n%s", out)
	}

	// Reads have nothing to fall back on.
	out = scheduleTaskTool(map[string]any{"name": "daily", "action": "status"})
	if !strings.HasPrefix(out, "Error: timed out") {
		t.Fatalf("read timeout should be an error:\n%s", out)
	}
}

func TestScheduleTaskTool_Validation(t *testing.T) {
	withScheduleIPC(t, time.Second)
	cases := map[string]map[string]any{
		"missing action":          {"name": "x"},
		"status without name":     {"action": "status"},
		"create without schedule": {"name": "x", "action": "create", "task": "t"},
		"create without task":     {"name": "x", "action": "create", "schedule": "* * * * *"},
		"update with nothing":     {"name": "x", "action": "update"},
		"unknown action":          {"name": "x", "action": "explode"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			if out := scheduleTaskTool(args); !strings.HasPrefix(out, "Error:") {
				t.Fatalf("expected validation error, got %q", out)
			}
		})
	}
}

func TestScheduleTaskToolDefAdvertisesReadActions(t *testing.T) {
	var found bool
	for _, def := range defaultTools() {
		if def.Name != ToolScheduleTask {
			continue
		}
		found = true
		props := def.Parameters["properties"].(map[string]any)
		action := props["action"].(map[string]any)
		enum := action["enum"].([]string)
		joined := strings.Join(enum, ",")
		for _, want := range []string{"status", "list", "create", "delete"} {
			if !strings.Contains(joined, want) {
				t.Errorf("action enum missing %q: %v", want, enum)
			}
		}
		required := def.Parameters["required"].([]string)
		if len(required) != 1 || required[0] != "action" {
			t.Errorf("only 'action' should be required (list needs no name), got %v", required)
		}
		if !strings.Contains(def.Description, "status") {
			t.Errorf("description should mention the read actions: %q", def.Description)
		}
	}
	if !found {
		t.Fatal("schedule_task tool definition not found")
	}
}
