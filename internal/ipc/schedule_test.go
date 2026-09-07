package ipc

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
)

func TestScheduleShortName(t *testing.T) {
	cases := []struct{ full, agent, want string }{
		{"helper-daily", "helper", "daily"},
		{"helper-daily-report", "helper", "daily-report"},
		{"ops-sweep", "helper", "ops-sweep"},
		{"helper-", "helper", "helper-"}, // degenerate: never return ""
		{"helper", "helper", "helper"},
		{"helper-daily", "", "helper-daily"},
	}
	for _, c := range cases {
		if got := ScheduleShortName(c.full, c.agent); got != c.want {
			t.Errorf("ScheduleShortName(%q, %q) = %q, want %q", c.full, c.agent, got, c.want)
		}
	}
}

func TestFormatScheduleResult(t *testing.T) {
	last := time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)
	next := last.Add(24 * time.Hour)

	t.Run("error", func(t *testing.T) {
		got := FormatScheduleResult(ScheduleResult{Action: ScheduleActionDelete, OK: false, Error: `schedule "x" not found`})
		if !strings.HasPrefix(got, "Error: schedule delete failed:") || !strings.Contains(got, "not found") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("empty list", func(t *testing.T) {
		if got := FormatScheduleResult(ScheduleResult{Action: ScheduleActionList, OK: true}); !strings.Contains(got, "No schedules") {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("list", func(t *testing.T) {
		got := FormatScheduleResult(ScheduleResult{Action: ScheduleActionList, OK: true, Schedules: []ScheduleInfo{
			{Name: "ops-sweep", ShortName: "ops-sweep", Schedule: "0 * * * *", TotalRuns: 3, Task: "sweep things"},
			{Name: "helper-daily", ShortName: "daily", Schedule: "0 9 * * *", Suspended: true, TotalRuns: 12,
				LastRunTime: &last, NextRunTime: &next, LastRun: &ScheduleRunInfo{Phase: "Failed"}},
		}})
		if !strings.HasPrefix(got, "2 schedule(s):") {
			t.Fatalf("header missing: %q", got)
		}
		// Sorted by CR name: helper-daily before ops-sweep.
		if strings.Index(got, "- daily") > strings.Index(got, "- ops-sweep") {
			t.Errorf("expected sorted output:\n%s", got)
		}
		if !strings.Contains(got, "state=suspended") || !strings.Contains(got, "last=2026-09-07T09:00:00Z (Failed)") {
			t.Errorf("state/last run missing:\n%s", got)
		}
		if strings.Contains(got, "next=2026-09-08") {
			t.Errorf("suspended schedules must not advertise a next run:\n%s", got)
		}
		if !strings.Contains(got, "cr=helper-daily") {
			t.Errorf("CR name should be shown when it differs from the short name:\n%s", got)
		}
		if !strings.Contains(got, "task: sweep things") {
			t.Errorf("task line missing:\n%s", got)
		}
	})

	t.Run("status", func(t *testing.T) {
		code := int32(1)
		got := FormatScheduleResult(ScheduleResult{Action: ScheduleActionStatus, OK: true, Schedule: &ScheduleInfo{
			Name: "helper-daily", ShortName: "daily", Namespace: "agents", Schedule: "0 9 * * *", Phase: "Active",
			Model: "claude-haiku-4-5", Provider: "anthropic", TotalRuns: 12, LastRunTime: &last, NextRunTime: &next,
			Task:    strings.Repeat("long task ", 100),
			LastRun: &ScheduleRunInfo{Name: "helper-daily-run", Phase: "Failed", CompletedAt: &last, Error: "boom", ExitCode: &code},
		}})
		for _, want := range []string{
			`Schedule "daily" (CR helper-daily in agents)`,
			"cron:      0 9 * * *",
			"state:     active",
			"model:     anthropic claude-haiku-4-5",
			"runs:      12",
			"last run:  2026-09-07T09:00:00Z",
			"next run:  2026-09-08T09:00:00Z",
			"last AgentRun helper-daily-run: Failed at 2026-09-07T09:00:00Z",
			"error:  boom",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("status output missing %q:\n%s", want, got)
			}
		}
		if !strings.Contains(got, "…") {
			t.Errorf("long task should be truncated:\n%s", got)
		}
	})

	t.Run("mutation", func(t *testing.T) {
		got := FormatScheduleResult(ScheduleResult{Action: ScheduleActionCreate, OK: true,
			Message:  `Schedule "daily" created.`,
			Schedule: &ScheduleInfo{Name: "helper-daily", ShortName: "daily", Schedule: "0 9 * * *"}})
		if !strings.HasPrefix(got, `Schedule "daily" created.`) || !strings.Contains(got, "CR helper-daily") {
			t.Fatalf("got %q", got)
		}
	})
}

func TestIsScheduleRequestFile(t *testing.T) {
	cases := map[string]bool{
		"schedule-1234.json":    true,
		"legacy.json":           true,
		"result-abc.json":       false,
		"result-abc.json.tmp":   false,
		"schedule-1234.json~":   false,
		"schedule-1234.partial": false,
	}
	for name, want := range cases {
		if got := isScheduleRequestFile(name); got != want {
			t.Errorf("isScheduleRequestFile(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestWriteScheduleResult(t *testing.T) {
	base := t.TempDir()
	if err := os.MkdirAll(filepath.Join(base, DirSchedules), 0o755); err != nil {
		t.Fatal(err)
	}
	b := &Bridge{BasePath: base, Log: logr.Discard()}

	b.writeScheduleResult([]byte(`{"id":"abc","ok":true,"action":"list"}`))
	data, err := os.ReadFile(filepath.Join(base, DirSchedules, "result-abc.json"))
	if err != nil {
		t.Fatalf("result file not written: %v", err)
	}
	if !strings.Contains(string(data), `"ok":true`) {
		t.Fatalf("unexpected content: %s", data)
	}
	if _, err := os.Stat(filepath.Join(base, DirSchedules, "result-abc.json.tmp")); !os.IsNotExist(err) {
		t.Fatal("temp file should have been renamed away")
	}

	// No id → nothing written.
	b.writeScheduleResult([]byte(`{"ok":true}`))
	entries, _ := os.ReadDir(filepath.Join(base, DirSchedules))
	if len(entries) != 1 {
		t.Fatalf("expected exactly one result file, got %d", len(entries))
	}
}
