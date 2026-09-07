package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// schedulesDir is where schedule requests and controller replies are
// exchanged with the IPC bridge. Overridable in tests.
var schedulesDir = "/ipc/schedules"

// defaultScheduleTimeout bounds how long `schedule` waits for the controller's
// reply before degrading to fire-and-forget (mutations) or failing (reads).
const defaultScheduleTimeout = 15

// --- schedule ---------------------------------------------------------------

func cmdSchedule(argv []string) int {
	fs := flag.NewFlagSet("schedule", flag.ContinueOnError)
	name := fs.String("name", "", "Schedule name (required for every action except list)")
	action := fs.String("action", "", "create | update | suspend | resume | delete | status | list (required)")
	schedule := fs.String("schedule", "", "Cron expression (required for create; optional for update)")
	task := fs.String("task", "", "Task description fired on each run (required for create)")
	model := fs.String("model", "", "Optional model override for runs created by this schedule")
	provider := fs.String("provider", "", "Optional provider override for runs created by this schedule")
	baseURL := fs.String("base-url", "", "Optional provider API endpoint override for runs created by this schedule")
	jsonOut := fs.Bool("json", false, "Print the controller's reply as JSON instead of text")
	timeout := fs.Int("timeout", defaultScheduleTimeout, "Seconds to wait for the controller's reply (0 = fire-and-forget)")
	fs.Usage = func() {
		fmt.Fprintln(os.Stderr, `Usage: sympozium-tool schedule --action <create|update|suspend|resume|delete|status|list> [--name NAME] [--schedule CRON] [--task "..."] [--model MODEL] [--provider PROVIDER] [--base-url URL] [--json] [--timeout SECS]

Writes /ipc/schedules/schedule-<id>.json. The IPC bridge relays it to the controller,
which applies it to the corresponding SympoziumSchedule and replies with the state it
actually applied (final CR name, cron, suspend flag, run counts, last run outcome).
This command prints that reply and exits 0 when the controller reports success,
1 when it reports an error (e.g. "not found"), 124 when no reply arrives in time.

Reads:
  status  One schedule's applied spec, controller status and last AgentRun outcome.
  list    Every schedule targeting this agent (agent- and operator-created). No --name.

Model, provider, and base URL default to the agent's own configuration when omitted.

Examples:
  sympozium-tool schedule --action list
  sympozium-tool schedule --name daily-report --action status
  sympozium-tool schedule --name daily-report --action create --schedule "0 9 * * 1-5" --task "Summarise yesterday's incidents"
  sympozium-tool schedule --name daily-report --action update --model claude-haiku-4-5
  sympozium-tool schedule --name daily-report --action suspend`)
		fs.PrintDefaults()
	}
	if err := fs.Parse(argv); err != nil {
		return 2
	}

	req, err := buildScheduleRequest(*name, *action, *schedule, *task, *model, *provider, *baseURL)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		return 2
	}

	if err := os.MkdirAll(schedulesDir, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "error: cannot create %s: %v\n", schedulesDir, err)
		return 1
	}
	req.ID = fmt.Sprintf("%d", time.Now().UnixNano())
	reqPath := filepath.Join(schedulesDir, ipc.ScheduleRequestPrefix+req.ID+".json")
	resPath := filepath.Join(schedulesDir, ipc.ScheduleResultPrefix+req.ID+".json")
	data, err := json.Marshal(req)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: marshal request: %v\n", err)
		return 1
	}
	if err := os.WriteFile(reqPath, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", reqPath, err)
		return 1
	}

	isRead := req.Action == ipc.ScheduleActionStatus || req.Action == ipc.ScheduleActionList
	if *timeout <= 0 {
		if isRead {
			fmt.Fprintln(os.Stderr, "error: status/list need a reply; use a positive --timeout")
			return 2
		}
		fmt.Printf("Schedule %q %s submitted (not waiting for confirmation).\n", req.Name, req.Action)
		return 0
	}

	res, ok := waitForScheduleResult(resPath, time.Duration(*timeout)*time.Second)
	if !ok {
		if isRead {
			fmt.Fprintf(os.Stderr, "error: timed out after %ds waiting for schedule %s — the controller may not support schedule queries yet, or the IPC bridge is not running\n", *timeout, req.Action)
			_ = os.Remove(reqPath)
			return 124
		}
		// Mutations were still relayed; report as older versions did, flagged.
		fmt.Printf("Schedule %q %s submitted.\n", req.Name, req.Action)
		fmt.Fprintf(os.Stderr, "warning: no confirmation from the control plane within %ds — verify with --action status\n", *timeout)
		return 0
	}
	_ = os.Remove(reqPath)
	_ = os.Remove(resPath)

	if *jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		_ = enc.Encode(res)
	} else {
		fmt.Println(ipc.FormatScheduleResult(res))
	}
	if !res.OK {
		return 1
	}
	return 0
}

// buildScheduleRequest validates the flags for the requested action and
// returns the request to relay (without an ID).
func buildScheduleRequest(name, action, schedule, task, model, provider, baseURL string) (ipc.ScheduleRequest, error) {
	action = strings.ToLower(strings.TrimSpace(action))
	name = strings.TrimSpace(name)
	if action == "" {
		return ipc.ScheduleRequest{}, fmt.Errorf("--action is required (create|update|suspend|resume|delete|status|list)")
	}
	if name == "" && action != ipc.ScheduleActionList {
		return ipc.ScheduleRequest{}, fmt.Errorf("--name is required for --action %s", action)
	}
	switch action {
	case ipc.ScheduleActionCreate:
		if schedule == "" {
			return ipc.ScheduleRequest{}, fmt.Errorf("--schedule is required for create")
		}
		if task == "" {
			return ipc.ScheduleRequest{}, fmt.Errorf("--task is required for create")
		}
	case ipc.ScheduleActionUpdate:
		if schedule == "" && task == "" && model == "" && provider == "" && baseURL == "" {
			return ipc.ScheduleRequest{}, fmt.Errorf("update needs at least one of --schedule, --task, --model, --provider, --base-url")
		}
	case ipc.ScheduleActionSuspend, ipc.ScheduleActionResume, ipc.ScheduleActionDelete, ipc.ScheduleActionStatus, ipc.ScheduleActionList:
		// name + action only (list: action only).
	default:
		return ipc.ScheduleRequest{}, fmt.Errorf("unknown --action %q (use create|update|suspend|resume|delete|status|list)", action)
	}
	return ipc.ScheduleRequest{
		Name:     name,
		Action:   action,
		Schedule: schedule,
		Task:     task,
		Model:    model,
		Provider: provider,
		BaseURL:  baseURL,
	}, nil
}

// waitForScheduleResult polls for the controller's reply file until it
// appears or the timeout elapses.
func waitForScheduleResult(resPath string, timeout time.Duration) (ipc.ScheduleResult, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		data, err := os.ReadFile(resPath)
		if err == nil && len(data) > 0 {
			var res ipc.ScheduleResult
			if json.Unmarshal(data, &res) == nil {
				return res, true
			}
			// Partial write — retry once after a short sleep.
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
