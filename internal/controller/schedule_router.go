// Package controller contains the schedule router which handles agent-initiated
// schedule requests. When an agent calls the schedule_task tool (or
// `sympozium-tool schedule`), the IPC bridge publishes a schedule.upsert event
// to NATS. This router subscribes to those events and creates, updates,
// suspends, resumes, deletes, or reads SympoziumSchedule CRDs on the agent's
// behalf, then answers on schedule.result.<agentRunID> so the agent learns
// what was actually applied.
//
// Agent pods deliberately hold no Kubernetes RBAC; this router is the only
// thing that touches the API for them, and it scopes every read to schedules
// whose agentRef is the requesting agent.
package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	sympoziumv1alpha1 "github.com/sympozium-ai/sympozium/api/v1alpha1"
	"github.com/sympozium-ai/sympozium/internal/eventbus"
	"github.com/sympozium-ai/sympozium/internal/ipc"
)

// scheduleResultMaxRunText bounds the last run's result/error text carried
// back to the agent so a verbose run cannot bloat the reply.
const scheduleResultMaxRunText = 1000

// ScheduleRouter subscribes to schedule.upsert events from the IPC bridge
// and creates/modifies/reads SympoziumSchedule CRDs so agents can manage and
// monitor their own heartbeats.
type ScheduleRouter struct {
	Client   client.Client
	EventBus eventbus.EventBus
	Log      logr.Logger
}

// Start begins listening for schedule upsert events. It blocks until ctx is cancelled.
func (sr *ScheduleRouter) Start(ctx context.Context) error {
	sr.Log.Info("Starting schedule router")

	ch, err := sr.EventBus.Subscribe(ctx, eventbus.TopicScheduleUpsert)
	if err != nil {
		return fmt.Errorf("subscribing to %s: %w", eventbus.TopicScheduleUpsert, err)
	}

	for {
		select {
		case <-ctx.Done():
			sr.Log.Info("Schedule router shutting down")
			return nil
		case event := <-ch:
			sr.handleScheduleEvent(ctx, event)
		}
	}
}

// handleScheduleEvent processes a single schedule request from an agent and,
// when the request carries an id, replies with the outcome.
func (sr *ScheduleRouter) handleScheduleEvent(ctx context.Context, event *eventbus.Event) {
	instanceName := event.Metadata["instanceName"]
	agentRunID := event.Metadata["agentRunID"]

	var req ipc.ScheduleRequest
	if err := json.Unmarshal(event.Data, &req); err != nil {
		sr.Log.Error(err, "failed to unmarshal schedule request")
		return
	}
	req.Action = strings.ToLower(strings.TrimSpace(req.Action))
	req.Name = strings.TrimSpace(req.Name)

	if req.Action == "" || (req.Name == "" && req.Action != ipc.ScheduleActionList) {
		sr.Log.Info("Ignoring schedule request with missing name or action")
		sr.reply(ctx, agentRunID, req, errorResult(req, "name and action are required"))
		return
	}

	namespace := sr.resolveNamespace(ctx, instanceName)

	sr.Log.Info("Processing schedule request",
		"instance", instanceName,
		"name", req.Name,
		"action", req.Action,
		"schedule", req.Schedule,
	)

	var res ipc.ScheduleResult
	switch req.Action {
	case ipc.ScheduleActionCreate:
		res = sr.createSchedule(ctx, namespace, instanceName, req)
	case ipc.ScheduleActionUpdate:
		res = sr.updateSchedule(ctx, namespace, instanceName, req)
	case ipc.ScheduleActionSuspend:
		res = sr.suspendSchedule(ctx, namespace, instanceName, req, true)
	case ipc.ScheduleActionResume:
		res = sr.suspendSchedule(ctx, namespace, instanceName, req, false)
	case ipc.ScheduleActionDelete:
		res = sr.deleteSchedule(ctx, namespace, instanceName, req)
	case ipc.ScheduleActionStatus:
		res = sr.statusSchedule(ctx, namespace, instanceName, req)
	case ipc.ScheduleActionList:
		res = sr.listSchedules(ctx, namespace, instanceName, req)
	default:
		sr.Log.Info("Unknown schedule action", "action", req.Action)
		res = errorResult(req, fmt.Sprintf("unknown action %q", req.Action))
	}
	sr.reply(ctx, agentRunID, req, res)
}

// resolveNamespace finds the namespace of the requesting Agent. Falls back
// to "default" when the instance is unknown, matching historical behaviour.
func (sr *ScheduleRouter) resolveNamespace(ctx context.Context, instanceName string) string {
	namespace := "default"
	if instanceName == "" {
		return namespace
	}
	var instances sympoziumv1alpha1.AgentList
	if err := sr.Client.List(ctx, &instances); err == nil {
		for i := range instances.Items {
			if instances.Items[i].Name == instanceName {
				return instances.Items[i].Namespace
			}
		}
	}
	return namespace
}

// reply publishes the outcome on the per-run result topic. Requests without
// an id come from older clients that never read replies; skip them.
func (sr *ScheduleRouter) reply(ctx context.Context, agentRunID string, req ipc.ScheduleRequest, res ipc.ScheduleResult) {
	if req.ID == "" || agentRunID == "" {
		return
	}
	res.ID = req.ID
	res.Action = req.Action
	topic := fmt.Sprintf("%s.%s", eventbus.TopicScheduleResult, agentRunID)
	evt, err := eventbus.NewEvent(topic, map[string]string{
		"agentRunID": agentRunID,
		"requestId":  req.ID,
	}, res)
	if err != nil {
		sr.Log.Error(err, "failed to create schedule result event")
		return
	}
	if err := sr.EventBus.Publish(ctx, topic, evt); err != nil {
		sr.Log.Error(err, "failed to publish schedule result", "topic", topic)
	}
}

func errorResult(req ipc.ScheduleRequest, msg string) ipc.ScheduleResult {
	return ipc.ScheduleResult{ID: req.ID, Action: req.Action, OK: false, Error: msg}
}

// agentScheduleName is the CR name for an agent-created schedule: the
// controller prefixes the agent's short name so schedules from different
// agents in one namespace never collide.
func agentScheduleName(instanceName, shortName string) string {
	return fmt.Sprintf("%s-%s", instanceName, shortName)
}

// resolveSchedule finds the SympoziumSchedule an agent means by name. It tries
// the agent-prefixed name first (what create produces), then the bare name —
// but only if that schedule targets this agent, so an agent can see and
// manage operator-created schedules aimed at it and nothing else.
func (sr *ScheduleRouter) resolveSchedule(ctx context.Context, namespace, instanceName, name string) (*sympoziumv1alpha1.SympoziumSchedule, error) {
	candidates := []string{agentScheduleName(instanceName, name)}
	if name != candidates[0] {
		candidates = append(candidates, name)
	}
	for _, candidate := range candidates {
		existing := &sympoziumv1alpha1.SympoziumSchedule{}
		err := sr.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: candidate}, existing)
		if err != nil {
			if errors.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if existing.Spec.AgentRef != "" && existing.Spec.AgentRef != instanceName {
			// Belongs to another agent: invisible to this one.
			continue
		}
		return existing, nil
	}
	return nil, errors.NewNotFound(sympoziumv1alpha1.GroupVersion.WithResource("sympoziumschedules").GroupResource(), name)
}

// createSchedule creates a new SympoziumSchedule CR.
func (sr *ScheduleRouter) createSchedule(ctx context.Context, namespace, instanceName string, req ipc.ScheduleRequest) ipc.ScheduleResult {
	if strings.TrimSpace(req.Schedule) == "" || strings.TrimSpace(req.Task) == "" {
		return errorResult(req, "create requires both schedule (cron) and task")
	}
	name := agentScheduleName(instanceName, req.Name)
	schedule := &sympoziumv1alpha1.SympoziumSchedule{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			Labels: map[string]string{
				"sympozium.ai/instance": instanceName,
				"sympozium.ai/source":   "agent",
			},
		},
		Spec: sympoziumv1alpha1.SympoziumScheduleSpec{
			AgentRef:          instanceName,
			Schedule:          req.Schedule,
			Task:              req.Task,
			Type:              "heartbeat",
			ConcurrencyPolicy: "Forbid",
			IncludeMemory:     true,
			Model:             req.Model,
			Provider:          req.Provider,
			BaseURL:           req.BaseURL,
		},
	}

	if err := sr.Client.Create(ctx, schedule); err != nil {
		if errors.IsAlreadyExists(err) {
			sr.Log.Info("Schedule already exists, updating instead", "name", name)
			res := sr.updateSchedule(ctx, namespace, instanceName, req)
			if res.OK {
				res.Message = fmt.Sprintf("Schedule %q already existed; applied your values as an update.", req.Name)
			}
			return res
		}
		sr.Log.Error(err, "failed to create SympoziumSchedule", "name", name)
		return errorResult(req, fmt.Sprintf("create %s: %v", name, err))
	}

	sr.Log.Info("Created SympoziumSchedule from agent request",
		"name", name,
		"schedule", req.Schedule,
		"instance", instanceName,
	)
	return ipc.ScheduleResult{
		OK:       true,
		Message:  fmt.Sprintf("Schedule %q created with cron %q. The task will run automatically on this interval.", req.Name, req.Schedule),
		Schedule: sr.scheduleInfo(ctx, schedule, instanceName),
	}
}

// updateSchedule patches an existing SympoziumSchedule with new schedule/task.
func (sr *ScheduleRouter) updateSchedule(ctx context.Context, namespace, instanceName string, req ipc.ScheduleRequest) ipc.ScheduleResult {
	if req.Schedule == "" && req.Task == "" && req.Model == "" && req.Provider == "" && req.BaseURL == "" {
		return errorResult(req, "update requires at least one of schedule, task, model, provider, baseURL")
	}
	existing, err := sr.resolveSchedule(ctx, namespace, instanceName, req.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			sr.Log.Info("Schedule not found for update", "name", req.Name)
			return errorResult(req, fmt.Sprintf("schedule %q not found (use action=list to see your schedules)", req.Name))
		}
		sr.Log.Error(err, "failed to get SympoziumSchedule for update", "name", req.Name)
		return errorResult(req, fmt.Sprintf("get %s: %v", req.Name, err))
	}

	var changed []string
	if req.Schedule != "" {
		existing.Spec.Schedule = req.Schedule
		changed = append(changed, fmt.Sprintf("schedule=%q", req.Schedule))
	}
	if req.Task != "" {
		existing.Spec.Task = req.Task
		changed = append(changed, "task")
	}
	if req.Model != "" {
		existing.Spec.Model = req.Model
		changed = append(changed, "model="+req.Model)
	}
	if req.Provider != "" {
		existing.Spec.Provider = req.Provider
		changed = append(changed, "provider="+req.Provider)
	}
	if req.BaseURL != "" {
		existing.Spec.BaseURL = req.BaseURL
		changed = append(changed, "baseURL="+req.BaseURL)
	}
	// Ensure it's not suspended when updating.
	wasSuspended := existing.Spec.Suspend
	existing.Spec.Suspend = false
	if wasSuspended {
		changed = append(changed, "resumed")
	}

	if err := sr.Client.Update(ctx, existing); err != nil {
		sr.Log.Error(err, "failed to update SympoziumSchedule", "name", existing.Name)
		return errorResult(req, fmt.Sprintf("update %s: %v", existing.Name, err))
	}

	sr.Log.Info("Updated SympoziumSchedule from agent request", "name", existing.Name)
	return ipc.ScheduleResult{
		OK:       true,
		Message:  fmt.Sprintf("Schedule %q updated: %s.", req.Name, strings.Join(changed, ", ")),
		Schedule: sr.scheduleInfo(ctx, existing, instanceName),
	}
}

// suspendSchedule sets or clears the Suspend flag on a SympoziumSchedule.
func (sr *ScheduleRouter) suspendSchedule(ctx context.Context, namespace, instanceName string, req ipc.ScheduleRequest, suspend bool) ipc.ScheduleResult {
	existing, err := sr.resolveSchedule(ctx, namespace, instanceName, req.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			sr.Log.Info("Schedule not found for suspend/resume", "name", req.Name)
			return errorResult(req, fmt.Sprintf("schedule %q not found (use action=list to see your schedules)", req.Name))
		}
		sr.Log.Error(err, "failed to get SympoziumSchedule", "name", req.Name)
		return errorResult(req, fmt.Sprintf("get %s: %v", req.Name, err))
	}

	action := "resumed"
	if suspend {
		action = "suspended"
	}
	if existing.Spec.Suspend == suspend {
		return ipc.ScheduleResult{
			OK:       true,
			Message:  fmt.Sprintf("Schedule %q was already %s.", req.Name, action),
			Schedule: sr.scheduleInfo(ctx, existing, instanceName),
		}
	}

	existing.Spec.Suspend = suspend
	if err := sr.Client.Update(ctx, existing); err != nil {
		sr.Log.Error(err, "failed to suspend/resume SympoziumSchedule", "name", existing.Name, "suspend", suspend)
		return errorResult(req, fmt.Sprintf("%s %s: %v", req.Action, existing.Name, err))
	}

	sr.Log.Info("SympoziumSchedule "+action, "name", existing.Name)
	msg := fmt.Sprintf("Schedule %q %s.", req.Name, action)
	if suspend {
		msg += " It will not fire until resumed."
	} else {
		msg += " Next run will fire according to the cron expression."
	}
	return ipc.ScheduleResult{
		OK:       true,
		Message:  msg,
		Schedule: sr.scheduleInfo(ctx, existing, instanceName),
	}
}

// deleteSchedule removes a SympoziumSchedule CR.
func (sr *ScheduleRouter) deleteSchedule(ctx context.Context, namespace, instanceName string, req ipc.ScheduleRequest) ipc.ScheduleResult {
	existing, err := sr.resolveSchedule(ctx, namespace, instanceName, req.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			sr.Log.Info("Schedule not found for deletion", "name", req.Name)
			return errorResult(req, fmt.Sprintf("schedule %q not found — nothing deleted (use action=list to see your schedules)", req.Name))
		}
		sr.Log.Error(err, "failed to get SympoziumSchedule for deletion", "name", req.Name)
		return errorResult(req, fmt.Sprintf("get %s: %v", req.Name, err))
	}

	if err := sr.Client.Delete(ctx, existing); err != nil {
		sr.Log.Error(err, "failed to delete SympoziumSchedule", "name", existing.Name)
		return errorResult(req, fmt.Sprintf("delete %s: %v", existing.Name, err))
	}

	sr.Log.Info("Deleted SympoziumSchedule", "name", existing.Name)
	return ipc.ScheduleResult{
		OK:      true,
		Message: fmt.Sprintf("Schedule %q (CR %s) deleted.", req.Name, existing.Name),
	}
}

// statusSchedule reports one schedule's applied spec, controller status and
// last run.
func (sr *ScheduleRouter) statusSchedule(ctx context.Context, namespace, instanceName string, req ipc.ScheduleRequest) ipc.ScheduleResult {
	existing, err := sr.resolveSchedule(ctx, namespace, instanceName, req.Name)
	if err != nil {
		if errors.IsNotFound(err) {
			return errorResult(req, fmt.Sprintf("schedule %q not found (use action=list to see your schedules)", req.Name))
		}
		sr.Log.Error(err, "failed to get SympoziumSchedule for status", "name", req.Name)
		return errorResult(req, fmt.Sprintf("get %s: %v", req.Name, err))
	}
	return ipc.ScheduleResult{
		OK:       true,
		Schedule: sr.scheduleInfo(ctx, existing, instanceName),
	}
}

// listSchedules reports every schedule in the namespace that targets the
// requesting agent — agent-created and operator-created alike — and nothing
// belonging to other agents.
func (sr *ScheduleRouter) listSchedules(ctx context.Context, namespace, instanceName string, req ipc.ScheduleRequest) ipc.ScheduleResult {
	var all sympoziumv1alpha1.SympoziumScheduleList
	if err := sr.Client.List(ctx, &all, client.InNamespace(namespace)); err != nil {
		sr.Log.Error(err, "failed to list SympoziumSchedules", "namespace", namespace)
		return errorResult(req, fmt.Sprintf("list schedules: %v", err))
	}
	var infos []ipc.ScheduleInfo
	for i := range all.Items {
		s := &all.Items[i]
		if s.Spec.AgentRef != instanceName {
			continue
		}
		infos = append(infos, *sr.scheduleInfo(ctx, s, instanceName))
	}
	sort.Slice(infos, func(i, j int) bool { return infos[i].Name < infos[j].Name })
	return ipc.ScheduleResult{
		OK:        true,
		Message:   fmt.Sprintf("%d schedule(s) target agent %q.", len(infos), instanceName),
		Schedules: infos,
	}
}

// scheduleInfo projects a SympoziumSchedule (and its last AgentRun, when
// still present) into the read-only shape returned to agents.
func (sr *ScheduleRouter) scheduleInfo(ctx context.Context, s *sympoziumv1alpha1.SympoziumSchedule, instanceName string) *ipc.ScheduleInfo {
	info := &ipc.ScheduleInfo{
		Name:        s.Name,
		ShortName:   ipc.ScheduleShortName(s.Name, instanceName),
		Namespace:   s.Namespace,
		AgentRef:    s.Spec.AgentRef,
		Source:      s.Labels["sympozium.ai/source"],
		Schedule:    s.Spec.Schedule,
		Task:        s.Spec.Task,
		Type:        s.Spec.Type,
		Suspended:   s.Spec.Suspend,
		Model:       s.Spec.Model,
		Provider:    s.Spec.Provider,
		BaseURL:     s.Spec.BaseURL,
		Phase:       s.Status.Phase,
		LastRunTime: metaTime(s.Status.LastRunTime),
		NextRunTime: metaTime(s.Status.NextRunTime),
		LastRunName: s.Status.LastRunName,
		TotalRuns:   s.Status.TotalRuns,
	}
	if !s.CreationTimestamp.IsZero() {
		info.CreatedAt = metaTime(&s.CreationTimestamp)
	}
	if s.Status.LastRunName != "" {
		var run sympoziumv1alpha1.AgentRun
		if err := sr.Client.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: s.Status.LastRunName}, &run); err == nil {
			info.LastRun = &ipc.ScheduleRunInfo{
				Name:        run.Name,
				Phase:       string(run.Status.Phase),
				StartedAt:   metaTime(run.Status.StartedAt),
				CompletedAt: metaTime(run.Status.CompletedAt),
				Error:       truncateText(run.Status.Error, scheduleResultMaxRunText),
				Result:      truncateText(run.Status.Result, scheduleResultMaxRunText),
				ExitCode:    run.Status.ExitCode,
			}
		}
	}
	return info
}

func metaTime(t *metav1.Time) *time.Time {
	if t == nil || t.IsZero() {
		return nil
	}
	tt := t.Time
	return &tt
}

func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
