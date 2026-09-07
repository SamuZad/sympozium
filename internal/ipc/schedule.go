package ipc

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Schedule actions an agent may request. Mutations are brokered through the
// controller (the agent pod holds no Kubernetes RBAC of its own); the read
// actions exist so an agent can verify what the controller actually applied
// and monitor its own schedules without touching the Kubernetes API.
const (
	ScheduleActionCreate  = "create"
	ScheduleActionUpdate  = "update"
	ScheduleActionSuspend = "suspend"
	ScheduleActionResume  = "resume"
	ScheduleActionDelete  = "delete"
	ScheduleActionStatus  = "status"
	ScheduleActionList    = "list"
)

// ScheduleRequestPrefix / ScheduleResultPrefix name the files exchanged under
// /ipc/schedules/. The bridge relays every file that is not a result; the
// controller answers a request carrying an ID with result-<id>.json.
const (
	ScheduleRequestPrefix = "schedule-"
	ScheduleResultPrefix  = "result-"
)

// ScheduleRequest is written to /ipc/schedules/schedule-*.json by the
// schedule_task tool (agent-runner) or `sympozium-tool schedule`. The IPC
// bridge relays it on the schedule.upsert topic and the controller's
// ScheduleRouter applies it.
//
// ID is a correlation id chosen by the caller. When set, the router publishes
// a ScheduleResult on schedule.result.<agentRunID> and the bridge drops it at
// /ipc/schedules/result-<id>.json for the caller to pick up. Requests without
// an ID (older clients) are still applied, fire-and-forget.
type ScheduleRequest struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name,omitempty"` // required for every action except list
	Action   string `json:"action"`
	Schedule string `json:"schedule,omitempty"`
	Task     string `json:"task,omitempty"`
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	BaseURL  string `json:"baseURL,omitempty"`
}

// ScheduleResult is the controller's reply to a ScheduleRequest.
type ScheduleResult struct {
	ID     string `json:"id"`
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	// Error is set when OK is false (not found, validation, API error).
	Error string `json:"error,omitempty"`
	// Message is a short human-readable summary of what happened.
	Message string `json:"message,omitempty"`
	// Schedule is the applied state after a mutation, or the answer to status.
	Schedule *ScheduleInfo `json:"schedule,omitempty"`
	// Schedules is the answer to list.
	Schedules []ScheduleInfo `json:"schedules,omitempty"`
}

// ScheduleInfo is a read-only projection of a SympoziumSchedule (spec +
// status) plus the most recent AgentRun it produced. Names are reported both
// as the full CR name and as the short name the agent used, since the
// controller prefixes agent-created schedules with the agent name.
type ScheduleInfo struct {
	Name      string `json:"name"`
	ShortName string `json:"shortName,omitempty"`
	Namespace string `json:"namespace,omitempty"`
	AgentRef  string `json:"agentRef,omitempty"`
	// Source is "agent" for schedules created through this tool, otherwise
	// the operator / ensemble that stamped it (empty when unknown).
	Source string `json:"source,omitempty"`

	Schedule string `json:"schedule"`
	Task     string `json:"task,omitempty"`
	Type     string `json:"type,omitempty"`
	// Suspended mirrors spec.suspend — the authoritative pause flag.
	Suspended bool   `json:"suspended"`
	Model     string `json:"model,omitempty"`
	Provider  string `json:"provider,omitempty"`
	BaseURL   string `json:"baseURL,omitempty"`

	Phase       string     `json:"phase,omitempty"`
	LastRunTime *time.Time `json:"lastRunTime,omitempty"`
	NextRunTime *time.Time `json:"nextRunTime,omitempty"`
	LastRunName string     `json:"lastRunName,omitempty"`
	TotalRuns   int64      `json:"totalRuns"`
	CreatedAt   *time.Time `json:"createdAt,omitempty"`

	// LastRun summarises status.lastRunName's AgentRun when it still exists.
	LastRun *ScheduleRunInfo `json:"lastRun,omitempty"`
}

// ScheduleRunInfo is the slice of an AgentRun's status relevant to "did my
// schedule work": phase, timing, and the failure reason if any.
type ScheduleRunInfo struct {
	Name        string     `json:"name"`
	Phase       string     `json:"phase,omitempty"`
	StartedAt   *time.Time `json:"startedAt,omitempty"`
	CompletedAt *time.Time `json:"completedAt,omitempty"`
	Error       string     `json:"error,omitempty"`
	// Result is the run's final answer, truncated at the source.
	Result   string `json:"result,omitempty"`
	ExitCode *int32 `json:"exitCode,omitempty"`
}

// ScheduleShortName strips the "<agent>-" prefix the controller adds to
// agent-created schedules so the agent sees the name it chose.
func ScheduleShortName(fullName, agentName string) string {
	if agentName != "" && strings.HasPrefix(fullName, agentName+"-") && len(fullName) > len(agentName)+1 {
		return fullName[len(agentName)+1:]
	}
	return fullName
}

// FormatScheduleResult renders a ScheduleResult as compact text for an LLM
// tool result or a shell. Both the agent-runner tool and sympozium-tool use
// it so agents see the same shape regardless of harness.
func FormatScheduleResult(res ScheduleResult) string {
	if !res.OK {
		msg := res.Error
		if msg == "" {
			msg = "unknown error"
		}
		return fmt.Sprintf("Error: schedule %s failed: %s", res.Action, msg)
	}
	var b strings.Builder
	switch res.Action {
	case ScheduleActionList:
		if len(res.Schedules) == 0 {
			b.WriteString("No schedules found for this agent.")
			return b.String()
		}
		items := append([]ScheduleInfo(nil), res.Schedules...)
		sort.Slice(items, func(i, j int) bool { return items[i].Name < items[j].Name })
		fmt.Fprintf(&b, "%d schedule(s):\n", len(items))
		for _, s := range items {
			b.WriteString(formatScheduleLine(s))
		}
		return strings.TrimRight(b.String(), "\n")
	case ScheduleActionStatus:
		if res.Schedule == nil {
			return "Schedule not found."
		}
		return strings.TrimRight(formatScheduleBlock(*res.Schedule), "\n")
	default:
		if res.Message != "" {
			b.WriteString(res.Message)
		} else {
			fmt.Fprintf(&b, "Schedule %s applied.", res.Action)
		}
		if res.Schedule != nil {
			b.WriteString("\n")
			b.WriteString(formatScheduleBlock(*res.Schedule))
		}
		return strings.TrimRight(b.String(), "\n")
	}
}

func formatScheduleLine(s ScheduleInfo) string {
	name := s.ShortName
	if name == "" {
		name = s.Name
	}
	state := stateWord(s)
	var b strings.Builder
	fmt.Fprintf(&b, "- %s  cron=%q  state=%s  runs=%d", name, s.Schedule, state, s.TotalRuns)
	if s.LastRunTime != nil {
		fmt.Fprintf(&b, "  last=%s", s.LastRunTime.UTC().Format(time.RFC3339))
		if s.LastRun != nil && s.LastRun.Phase != "" {
			fmt.Fprintf(&b, " (%s)", s.LastRun.Phase)
		}
	}
	if s.NextRunTime != nil && !s.Suspended {
		fmt.Fprintf(&b, "  next=%s", s.NextRunTime.UTC().Format(time.RFC3339))
	}
	if s.Name != name {
		fmt.Fprintf(&b, "  cr=%s", s.Name)
	}
	b.WriteString("\n")
	if s.Task != "" {
		fmt.Fprintf(&b, "    task: %s\n", truncateOneLine(s.Task, 140))
	}
	return b.String()
}

func formatScheduleBlock(s ScheduleInfo) string {
	var b strings.Builder
	name := s.ShortName
	if name == "" {
		name = s.Name
	}
	fmt.Fprintf(&b, "Schedule %q (CR %s", name, s.Name)
	if s.Namespace != "" {
		fmt.Fprintf(&b, " in %s", s.Namespace)
	}
	b.WriteString(")\n")
	fmt.Fprintf(&b, "  cron:      %s\n", s.Schedule)
	fmt.Fprintf(&b, "  state:     %s", stateWord(s))
	if s.Phase != "" && !strings.EqualFold(s.Phase, stateWord(s)) {
		fmt.Fprintf(&b, " (phase %s)", s.Phase)
	}
	b.WriteString("\n")
	if s.Type != "" {
		fmt.Fprintf(&b, "  type:      %s\n", s.Type)
	}
	if s.Source != "" {
		fmt.Fprintf(&b, "  source:    %s\n", s.Source)
	}
	if s.Model != "" || s.Provider != "" || s.BaseURL != "" {
		fmt.Fprintf(&b, "  model:     %s\n", strings.TrimSpace(strings.Join(nonEmpty(s.Provider, s.Model, s.BaseURL), " ")))
	}
	fmt.Fprintf(&b, "  runs:      %d\n", s.TotalRuns)
	if s.LastRunTime != nil {
		fmt.Fprintf(&b, "  last run:  %s\n", s.LastRunTime.UTC().Format(time.RFC3339))
	}
	if s.NextRunTime != nil {
		fmt.Fprintf(&b, "  next run:  %s\n", s.NextRunTime.UTC().Format(time.RFC3339))
	}
	if s.CreatedAt != nil {
		fmt.Fprintf(&b, "  created:   %s\n", s.CreatedAt.UTC().Format(time.RFC3339))
	}
	if s.Task != "" {
		fmt.Fprintf(&b, "  task:      %s\n", truncateOneLine(s.Task, 400))
	}
	if r := s.LastRun; r != nil {
		fmt.Fprintf(&b, "  last AgentRun %s: %s", r.Name, orDash(r.Phase))
		if r.CompletedAt != nil {
			fmt.Fprintf(&b, " at %s", r.CompletedAt.UTC().Format(time.RFC3339))
		} else if r.StartedAt != nil {
			fmt.Fprintf(&b, " since %s", r.StartedAt.UTC().Format(time.RFC3339))
		}
		b.WriteString("\n")
		if r.Error != "" {
			fmt.Fprintf(&b, "    error:  %s\n", truncateOneLine(r.Error, 400))
		}
		if r.Result != "" {
			fmt.Fprintf(&b, "    result: %s\n", truncateOneLine(r.Result, 400))
		}
	} else if s.LastRunName != "" {
		fmt.Fprintf(&b, "  last AgentRun %s: no longer present\n", s.LastRunName)
	}
	return b.String()
}

func stateWord(s ScheduleInfo) string {
	if s.Suspended {
		return "suspended"
	}
	if s.Phase != "" {
		return strings.ToLower(s.Phase)
	}
	return "active"
}

func nonEmpty(vals ...string) []string {
	out := make([]string, 0, len(vals))
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			out = append(out, v)
		}
	}
	return out
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func truncateOneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
