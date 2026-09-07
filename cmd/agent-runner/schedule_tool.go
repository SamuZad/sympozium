package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// ipcSchedulesDir is where schedule requests and controller replies are
// exchanged with the IPC bridge. Overridable in tests.
var ipcSchedulesDir = "/ipc/schedules"

// scheduleResultTimeout bounds how long schedule_task waits for the
// controller's reply. The round trip is file → bridge → NATS → controller →
// NATS → bridge → file and normally completes well under a second; the budget
// is generous so a busy controller still answers. Past it the tool degrades
// to the historical fire-and-forget behaviour so an older control plane that
// never replies does not break agents.
var scheduleResultTimeout = 15 * time.Second

// scheduleTaskTool writes a schedule request to /ipc/schedules/ for the IPC
// bridge to relay to the controller, then blocks for the controller's reply so
// the agent learns what was actually applied (final CR name, suspend state,
// errors such as "not found") instead of an optimistic guess. The read actions
// (status, list) exist purely to give agents a brokered view of their own
// schedules — agent pods hold no Kubernetes RBAC.
func scheduleTaskTool(args map[string]any) string {
	name, _ := args["name"].(string)
	action, _ := args["action"].(string)
	schedule, _ := args["schedule"].(string)
	task, _ := args["task"].(string)
	model, _ := args["model"].(string)
	provider, _ := args["provider"].(string)
	baseURL, _ := args["baseURL"].(string)
	action = strings.ToLower(strings.TrimSpace(action))
	name = strings.TrimSpace(name)

	if action == "" {
		return "Error: 'action' is required (create, update, suspend, resume, delete, status, list)"
	}
	if name == "" && action != ipc.ScheduleActionList {
		return "Error: 'name' is required — a short unique name for this schedule"
	}

	// Validate required fields per action.
	switch action {
	case ipc.ScheduleActionCreate:
		if schedule == "" {
			return "Error: 'schedule' is required for create (cron expression, e.g. '0 */3 * * *')"
		}
		if task == "" {
			return "Error: 'task' is required for create — what should the agent do each time?"
		}
	case ipc.ScheduleActionUpdate:
		if schedule == "" && task == "" && model == "" && provider == "" && baseURL == "" {
			return "Error: 'schedule', 'task', 'model', 'provider' and/or 'baseURL' required for update — provide what you want to change"
		}
	case ipc.ScheduleActionSuspend, ipc.ScheduleActionResume, ipc.ScheduleActionDelete, ipc.ScheduleActionStatus, ipc.ScheduleActionList:
		// Only name (+ action) needed; list needs neither.
	default:
		return fmt.Sprintf("Error: unknown action '%s' — use create, update, suspend, resume, delete, status, or list", action)
	}

	id := fmt.Sprintf("%d", time.Now().UnixNano())
	req := ipc.ScheduleRequest{
		ID:       id,
		Name:     name,
		Action:   action,
		Schedule: schedule,
		Task:     task,
		Model:    model,
		Provider: provider,
		BaseURL:  baseURL,
	}
	data, err := json.Marshal(req)
	if err != nil {
		return fmt.Sprintf("Error marshalling schedule request: %v", err)
	}

	_ = os.MkdirAll(ipcSchedulesDir, 0o755)
	reqPath := filepath.Join(ipcSchedulesDir, fmt.Sprintf("%s%s.json", ipc.ScheduleRequestPrefix, id))
	resPath := filepath.Join(ipcSchedulesDir, fmt.Sprintf("%s%s.json", ipc.ScheduleResultPrefix, id))
	if err := os.WriteFile(reqPath, data, 0o644); err != nil {
		return fmt.Sprintf("Error writing schedule file: %v", err)
	}
	log.Printf("Wrote schedule request %s: name=%s action=%s schedule=%s", id, name, action, schedule)

	if res, ok := waitForScheduleResult(resPath, scheduleResultTimeout); ok {
		_ = os.Remove(reqPath)
		_ = os.Remove(resPath)
		return ipc.FormatScheduleResult(res)
	}

	// No reply: the control plane is either slow or predates schedule
	// replies. Reads have nothing to fall back on; mutations were still
	// relayed, so report them the way older versions did, flagged as
	// unconfirmed.
	log.Printf("No schedule result for request %s within %s", id, scheduleResultTimeout)
	switch action {
	case ipc.ScheduleActionStatus, ipc.ScheduleActionList:
		return fmt.Sprintf("Error: timed out after %s waiting for schedule %s. The controller may not support schedule queries yet, or the IPC bridge is not running.", scheduleResultTimeout, action)
	}
	return legacyScheduleMessage(name, action, schedule, task) +
		fmt.Sprintf(" (Unconfirmed: no reply from the control plane within %s — verify with action 'status'.)", scheduleResultTimeout)
}

// legacyScheduleMessage is the optimistic confirmation earlier versions
// returned as soon as the request file was written.
func legacyScheduleMessage(name, action, schedule, task string) string {
	switch action {
	case ipc.ScheduleActionCreate:
		return fmt.Sprintf("Schedule '%s' created with cron '%s'. The task will run automatically on this interval.", name, schedule)
	case ipc.ScheduleActionUpdate:
		parts := []string{}
		if schedule != "" {
			parts = append(parts, fmt.Sprintf("schedule='%s'", schedule))
		}
		if task != "" {
			parts = append(parts, "task updated")
		}
		return fmt.Sprintf("Schedule '%s' updated: %s", name, strings.Join(parts, ", "))
	case ipc.ScheduleActionSuspend:
		return fmt.Sprintf("Schedule '%s' suspended. It will not fire until resumed.", name)
	case ipc.ScheduleActionResume:
		return fmt.Sprintf("Schedule '%s' resumed. Next run will fire according to the cron expression.", name)
	case ipc.ScheduleActionDelete:
		return fmt.Sprintf("Schedule '%s' deleted.", name)
	default:
		return fmt.Sprintf("Schedule '%s' action '%s' submitted.", name, action)
	}
}

// waitForScheduleResult polls for the controller's reply file until it
// appears or the timeout elapses. The bridge writes replies atomically, but a
// malformed read is retried once anyway, mirroring the exec tool.
func waitForScheduleResult(resPath string, timeout time.Duration) (ipc.ScheduleResult, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(resPath)
		if err == nil && len(data) > 0 {
			var res ipc.ScheduleResult
			if json.Unmarshal(data, &res) == nil {
				return res, true
			}
			time.Sleep(100 * time.Millisecond)
			if data, err = os.ReadFile(resPath); err == nil && json.Unmarshal(data, &res) == nil {
				return res, true
			}
			return ipc.ScheduleResult{}, false
		}
		time.Sleep(100 * time.Millisecond)
	}
	return ipc.ScheduleResult{}, false
}
