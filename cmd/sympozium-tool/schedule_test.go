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

func TestBuildScheduleRequest(t *testing.T) {
	cases := []struct {
		name     string
		args     [7]string // name, action, schedule, task, model, provider, baseURL
		wantErr  string
		wantAct  string
		wantName string
	}{
		{name: "missing action", args: [7]string{"x", "", "", "", "", "", ""}, wantErr: "--action is required"},
		{name: "status needs name", args: [7]string{"", "status", "", "", "", "", ""}, wantErr: "--name is required"},
		{name: "list needs no name", args: [7]string{"", "list", "", "", "", "", ""}, wantAct: "list"},
		{name: "create needs cron", args: [7]string{"x", "create", "", "t", "", "", ""}, wantErr: "--schedule is required"},
		{name: "create needs task", args: [7]string{"x", "create", "* * * * *", "", "", "", ""}, wantErr: "--task is required"},
		{name: "update needs a change", args: [7]string{"x", "update", "", "", "", "", ""}, wantErr: "at least one"},
		{name: "update model only is fine", args: [7]string{"x", "update", "", "", "claude-haiku-4-5", "", ""}, wantAct: "update", wantName: "x"},
		{name: "unknown action", args: [7]string{"x", "explode", "", "", "", "", ""}, wantErr: "unknown --action"},
		{name: "action normalised", args: [7]string{" daily ", " STATUS ", "", "", "", "", ""}, wantAct: "status", wantName: "daily"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			a := tc.args
			req, err := buildScheduleRequest(a[0], a[1], a[2], a[3], a[4], a[5], a[6])
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if req.Action != tc.wantAct || req.Name != tc.wantName {
				t.Fatalf("got action=%q name=%q, want %q/%q", req.Action, req.Name, tc.wantAct, tc.wantName)
			}
		})
	}
}

func TestWaitForScheduleResult(t *testing.T) {
	dir := t.TempDir()
	resPath := filepath.Join(dir, "result-1.json")

	// Times out when nothing arrives.
	if _, ok := waitForScheduleResult(resPath, 150*time.Millisecond); ok {
		t.Fatal("expected timeout")
	}

	// Picks up a reply written while waiting.
	go func() {
		time.Sleep(100 * time.Millisecond)
		out, _ := json.Marshal(ipc.ScheduleResult{ID: "1", Action: "status", OK: true,
			Schedule: &ipc.ScheduleInfo{Name: "helper-daily", ShortName: "daily", Schedule: "0 9 * * *"}})
		_ = os.WriteFile(resPath, out, 0o644)
	}()
	res, ok := waitForScheduleResult(resPath, 3*time.Second)
	if !ok || !res.OK || res.Schedule == nil || res.Schedule.Name != "helper-daily" {
		t.Fatalf("unexpected result: ok=%v %+v", ok, res)
	}
}

func TestCmdSchedule_RoundTripExitCodes(t *testing.T) {
	dir := t.TempDir()
	orig := schedulesDir
	schedulesDir = dir
	t.Cleanup(func() { schedulesDir = orig })

	answer := func(res ipc.ScheduleResult) {
		go func() {
			deadline := time.Now().Add(5 * time.Second)
			for time.Now().Before(deadline) {
				matches, _ := filepath.Glob(filepath.Join(dir, ipc.ScheduleRequestPrefix+"*.json"))
				for _, m := range matches {
					data, err := os.ReadFile(m)
					if err != nil {
						continue
					}
					var req ipc.ScheduleRequest
					if json.Unmarshal(data, &req) != nil || req.ID == "" {
						continue
					}
					res.ID, res.Action = req.ID, req.Action
					out, _ := json.Marshal(res)
					_ = os.WriteFile(filepath.Join(dir, ipc.ScheduleResultPrefix+req.ID+".json"), out, 0o644)
					return
				}
				time.Sleep(20 * time.Millisecond)
			}
		}()
	}

	answer(ipc.ScheduleResult{OK: true, Schedules: []ipc.ScheduleInfo{{Name: "helper-daily", ShortName: "daily", Schedule: "0 9 * * *"}}})
	if code := cmdSchedule([]string{"--action", "list", "--timeout", "3"}); code != 0 {
		t.Fatalf("list exit = %d, want 0", code)
	}

	answer(ipc.ScheduleResult{OK: false, Error: `schedule "ghost" not found`})
	if code := cmdSchedule([]string{"--action", "delete", "--name", "ghost", "--timeout", "3"}); code != 1 {
		t.Fatalf("controller error exit = %d, want 1", code)
	}

	// No reply: reads fail with 124, mutations degrade to 0.
	if code := cmdSchedule([]string{"--action", "status", "--name", "daily", "--timeout", "1"}); code != 124 {
		t.Fatalf("read timeout exit = %d, want 124", code)
	}
	if code := cmdSchedule([]string{"--action", "suspend", "--name", "daily", "--timeout", "1"}); code != 0 {
		t.Fatalf("unconfirmed mutation exit = %d, want 0", code)
	}

	// Validation failures exit 2 without writing anything.
	if code := cmdSchedule([]string{"--action", "status"}); code != 2 {
		t.Fatalf("validation exit = %d, want 2", code)
	}
}
