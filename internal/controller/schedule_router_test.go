package controller

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/go-logr/logr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/eventbus"
	"github.com/sympozium-ai/sympozium/internal/ipc"
)

const (
	testAgent     = "helper"
	testNamespace = "agents"
	testRunID     = "run-1"
)

func newScheduleRouterFixture(t *testing.T, objs ...client.Object) (*ScheduleRouter, *recordingEventBus, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := sympoziumv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}
	agent := &sympoziumv1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: testAgent, Namespace: testNamespace}}
	all := append([]client.Object{agent}, objs...)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(all...).Build()
	bus := &recordingEventBus{}
	return &ScheduleRouter{Client: cl, EventBus: bus, Log: logr.Discard()}, bus, cl
}

func sendScheduleRequest(t *testing.T, sr *ScheduleRouter, req ipc.ScheduleRequest) {
	t.Helper()
	meta := map[string]string{"instanceName": testAgent, "agentRunID": testRunID}
	event, err := eventbus.NewEvent(eventbus.TopicScheduleUpsert, meta, req)
	if err != nil {
		t.Fatalf("NewEvent: %v", err)
	}
	sr.handleScheduleEvent(context.Background(), event)
}

func lastReply(t *testing.T, bus *recordingEventBus) ipc.ScheduleResult {
	t.Helper()
	if len(bus.published) == 0 {
		t.Fatal("expected a schedule.result reply, got none")
	}
	rec := bus.published[len(bus.published)-1]
	if want := eventbus.TopicScheduleResult + "." + testRunID; rec.Topic != want {
		t.Fatalf("reply topic = %q, want %q", rec.Topic, want)
	}
	var res ipc.ScheduleResult
	if err := json.Unmarshal(rec.Event.Data, &res); err != nil {
		t.Fatalf("decode reply: %v", err)
	}
	return res
}

func scheduleFor(agentRef, name string, source string) *sympoziumv1alpha1.SympoziumSchedule {
	labels := map[string]string{}
	if source != "" {
		labels["sympozium.ai/source"] = source
	}
	return &sympoziumv1alpha1.SympoziumSchedule{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: testNamespace, Labels: labels},
		Spec: sympoziumv1alpha1.SympoziumScheduleSpec{
			AgentRef: agentRef,
			Schedule: "0 9 * * *",
			Task:     "task for " + name,
			Type:     "heartbeat",
		},
	}
}

func TestScheduleRouter_CreateRepliesWithAppliedState(t *testing.T) {
	sr, bus, cl := newScheduleRouterFixture(t)

	sendScheduleRequest(t, sr, ipc.ScheduleRequest{
		ID: "r1", Name: "daily", Action: ipc.ScheduleActionCreate,
		Schedule: "0 9 * * 1-5", Task: "Summarise incidents", Model: "claude-haiku-4-5",
	})

	var created sympoziumv1alpha1.SympoziumSchedule
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "helper-daily"}, &created); err != nil {
		t.Fatalf("expected helper-daily to be created in %s: %v", testNamespace, err)
	}
	if created.Spec.AgentRef != testAgent || created.Spec.Model != "claude-haiku-4-5" {
		t.Errorf("unexpected spec: %+v", created.Spec)
	}

	res := lastReply(t, bus)
	if !res.OK || res.ID != "r1" || res.Action != ipc.ScheduleActionCreate {
		t.Fatalf("unexpected reply: %+v", res)
	}
	if res.Schedule == nil || res.Schedule.Name != "helper-daily" || res.Schedule.ShortName != "daily" {
		t.Fatalf("reply should carry the applied CR name and short name: %+v", res.Schedule)
	}
	if res.Schedule.Schedule != "0 9 * * 1-5" || res.Schedule.Suspended {
		t.Errorf("reply spec mismatch: %+v", res.Schedule)
	}
	if !strings.Contains(res.Message, `"daily"`) {
		t.Errorf("message should name the schedule: %q", res.Message)
	}
}

func TestScheduleRouter_CreateOnExistingBecomesUpdateAndSaysSo(t *testing.T) {
	sr, bus, _ := newScheduleRouterFixture(t, scheduleFor(testAgent, "helper-daily", "agent"))

	sendScheduleRequest(t, sr, ipc.ScheduleRequest{
		ID: "r2", Name: "daily", Action: ipc.ScheduleActionCreate, Schedule: "*/5 * * * *", Task: "new task",
	})
	res := lastReply(t, bus)
	if !res.OK {
		t.Fatalf("expected ok, got %+v", res)
	}
	if !strings.Contains(res.Message, "already existed") {
		t.Errorf("agent must be told create turned into update: %q", res.Message)
	}
	if res.Schedule == nil || res.Schedule.Schedule != "*/5 * * * *" {
		t.Errorf("applied cron not reflected: %+v", res.Schedule)
	}
}

func TestScheduleRouter_MutationsOnMissingScheduleReportErrors(t *testing.T) {
	for _, action := range []string{ipc.ScheduleActionUpdate, ipc.ScheduleActionSuspend, ipc.ScheduleActionResume, ipc.ScheduleActionDelete, ipc.ScheduleActionStatus} {
		t.Run(action, func(t *testing.T) {
			sr, bus, _ := newScheduleRouterFixture(t)
			sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "r", Name: "ghost", Action: action, Task: "x"})
			res := lastReply(t, bus)
			if res.OK {
				t.Fatalf("%s on a missing schedule must fail, got %+v", action, res)
			}
			if !strings.Contains(res.Error, "not found") {
				t.Errorf("error should say not found: %q", res.Error)
			}
		})
	}
}

func TestScheduleRouter_StatusResolvesNamesAndScopesToAgent(t *testing.T) {
	failedRun := &sympoziumv1alpha1.AgentRun{
		ObjectMeta: metav1.ObjectMeta{Name: "helper-daily-run", Namespace: testNamespace},
		Status: sympoziumv1alpha1.AgentRunStatus{
			Phase: sympoziumv1alpha1.AgentRunPhase("Failed"),
			Error: "boom: provider returned 529",
		},
	}
	mine := scheduleFor(testAgent, "helper-daily", "agent")
	mine.Spec.Suspend = true
	mine.Status = sympoziumv1alpha1.SympoziumScheduleStatus{
		Phase:       "Suspended",
		LastRunName: "helper-daily-run",
		LastRunTime: &metav1.Time{Time: time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)},
		TotalRuns:   12,
	}
	operator := scheduleFor(testAgent, "ops-sweep", "")
	theirs := scheduleFor("other-agent", "other-agent-daily", "agent")
	sr, bus, _ := newScheduleRouterFixture(t, mine, operator, theirs, failedRun)

	// Short name resolves to the agent-prefixed CR and carries status + last run.
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "s1", Name: "daily", Action: ipc.ScheduleActionStatus})
	res := lastReply(t, bus)
	if !res.OK || res.Schedule == nil {
		t.Fatalf("status daily: %+v", res)
	}
	got := res.Schedule
	if got.Name != "helper-daily" || got.ShortName != "daily" || !got.Suspended || got.TotalRuns != 12 || got.Phase != "Suspended" {
		t.Errorf("status projection wrong: %+v", got)
	}
	if got.LastRunTime == nil || !got.LastRunTime.Equal(time.Date(2026, 9, 7, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("lastRunTime not carried: %v", got.LastRunTime)
	}
	if got.LastRun == nil || got.LastRun.Phase != "Failed" || !strings.Contains(got.LastRun.Error, "529") {
		t.Errorf("last run failure reason not surfaced: %+v", got.LastRun)
	}

	// Bare CR name of an operator-created schedule targeting this agent resolves too.
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "s2", Name: "ops-sweep", Action: ipc.ScheduleActionStatus})
	res = lastReply(t, bus)
	if !res.OK || res.Schedule == nil || res.Schedule.Name != "ops-sweep" || res.Schedule.ShortName != "ops-sweep" {
		t.Fatalf("status ops-sweep: %+v", res)
	}

	// Another agent's schedule is invisible even by exact CR name.
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "s3", Name: "other-agent-daily", Action: ipc.ScheduleActionStatus})
	res = lastReply(t, bus)
	if res.OK || !strings.Contains(res.Error, "not found") {
		t.Fatalf("other agent's schedule leaked: %+v", res)
	}
}

func TestScheduleRouter_ListIsScopedToRequestingAgent(t *testing.T) {
	sr, bus, _ := newScheduleRouterFixture(t,
		scheduleFor(testAgent, "helper-zeta", "agent"),
		scheduleFor(testAgent, "ops-sweep", ""),
		scheduleFor("other-agent", "other-agent-daily", "agent"),
	)
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "l1", Action: ipc.ScheduleActionList})
	res := lastReply(t, bus)
	if !res.OK {
		t.Fatalf("list failed: %+v", res)
	}
	if len(res.Schedules) != 2 {
		t.Fatalf("expected 2 schedules for %s, got %d: %+v", testAgent, len(res.Schedules), res.Schedules)
	}
	// Sorted by CR name; short names strip the agent prefix only where present.
	if res.Schedules[0].Name != "helper-zeta" || res.Schedules[0].ShortName != "zeta" {
		t.Errorf("first item: %+v", res.Schedules[0])
	}
	if res.Schedules[1].Name != "ops-sweep" || res.Schedules[1].ShortName != "ops-sweep" {
		t.Errorf("second item: %+v", res.Schedules[1])
	}
}

func TestScheduleRouter_ListDoesNotRequireName(t *testing.T) {
	sr, bus, _ := newScheduleRouterFixture(t)
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "l2", Action: ipc.ScheduleActionList})
	res := lastReply(t, bus)
	if !res.OK || len(res.Schedules) != 0 {
		t.Fatalf("empty list should succeed: %+v", res)
	}
}

func TestScheduleRouter_SuspendResumeReflectAppliedState(t *testing.T) {
	sr, bus, cl := newScheduleRouterFixture(t, scheduleFor(testAgent, "helper-daily", "agent"))

	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "p1", Name: "daily", Action: ipc.ScheduleActionSuspend})
	res := lastReply(t, bus)
	if !res.OK || res.Schedule == nil || !res.Schedule.Suspended {
		t.Fatalf("suspend reply should show suspended=true: %+v", res)
	}
	var cr sympoziumv1alpha1.SympoziumSchedule
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "helper-daily"}, &cr); err != nil || !cr.Spec.Suspend {
		t.Fatalf("CR not suspended: err=%v suspend=%v", err, cr.Spec.Suspend)
	}

	// Idempotent: suspending again is reported, not silently swallowed.
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "p2", Name: "daily", Action: ipc.ScheduleActionSuspend})
	res = lastReply(t, bus)
	if !res.OK || !strings.Contains(res.Message, "already suspended") {
		t.Fatalf("expected already-suspended message: %+v", res)
	}

	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "p3", Name: "daily", Action: ipc.ScheduleActionResume})
	res = lastReply(t, bus)
	if !res.OK || res.Schedule == nil || res.Schedule.Suspended {
		t.Fatalf("resume reply should show suspended=false: %+v", res)
	}
}

func TestScheduleRouter_UpdateReportsImplicitResume(t *testing.T) {
	s := scheduleFor(testAgent, "helper-daily", "agent")
	s.Spec.Suspend = true
	sr, bus, _ := newScheduleRouterFixture(t, s)

	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "u1", Name: "daily", Action: ipc.ScheduleActionUpdate, Task: "new task"})
	res := lastReply(t, bus)
	if !res.OK || res.Schedule == nil {
		t.Fatalf("update failed: %+v", res)
	}
	if res.Schedule.Suspended {
		t.Error("update clears suspend; reply must reflect that")
	}
	if !strings.Contains(res.Message, "resumed") || !strings.Contains(res.Message, "task") {
		t.Errorf("message should list what changed, including the implicit resume: %q", res.Message)
	}
	if res.Schedule.Task != "new task" {
		t.Errorf("applied task not reflected: %q", res.Schedule.Task)
	}
}

func TestScheduleRouter_UpdateWithNothingToChangeIsRejected(t *testing.T) {
	sr, bus, _ := newScheduleRouterFixture(t, scheduleFor(testAgent, "helper-daily", "agent"))
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "u2", Name: "daily", Action: ipc.ScheduleActionUpdate})
	res := lastReply(t, bus)
	if res.OK || !strings.Contains(res.Error, "at least one") {
		t.Fatalf("expected validation error, got %+v", res)
	}
}

func TestScheduleRouter_DeleteConfirmsCRName(t *testing.T) {
	sr, bus, cl := newScheduleRouterFixture(t, scheduleFor(testAgent, "helper-daily", "agent"))
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "d1", Name: "daily", Action: ipc.ScheduleActionDelete})
	res := lastReply(t, bus)
	if !res.OK || !strings.Contains(res.Message, "helper-daily") {
		t.Fatalf("delete reply should confirm the CR name: %+v", res)
	}
	var cr sympoziumv1alpha1.SympoziumSchedule
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "helper-daily"}, &cr); err == nil {
		t.Fatal("schedule still exists after delete")
	}
}

func TestScheduleRouter_LegacyRequestWithoutIDIsAppliedSilently(t *testing.T) {
	sr, bus, cl := newScheduleRouterFixture(t)
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{Name: "daily", Action: ipc.ScheduleActionCreate, Schedule: "0 9 * * *", Task: "t"})
	var cr sympoziumv1alpha1.SympoziumSchedule
	if err := cl.Get(context.Background(), client.ObjectKey{Namespace: testNamespace, Name: "helper-daily"}, &cr); err != nil {
		t.Fatalf("legacy create must still apply: %v", err)
	}
	if len(bus.published) != 0 {
		t.Fatalf("no reply expected for requests without an id, got %d", len(bus.published))
	}
}

func TestScheduleRouter_UnknownActionIsReported(t *testing.T) {
	sr, bus, _ := newScheduleRouterFixture(t)
	sendScheduleRequest(t, sr, ipc.ScheduleRequest{ID: "x1", Name: "daily", Action: "explode"})
	res := lastReply(t, bus)
	if res.OK || !strings.Contains(res.Error, "unknown action") {
		t.Fatalf("expected unknown action error, got %+v", res)
	}
}
