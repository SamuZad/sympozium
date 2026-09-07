# Scheduled Tasks

`SympoziumSchedule` resources define cron-based recurring agent runs — perfect for automated cluster health checks, overnight alert reviews, resource right-sizing sweeps, or any domain-specific task.

## Example

```yaml
apiVersion: sympozium.ai/v1alpha1
kind: SympoziumSchedule
metadata:
  name: daily-standup
spec:
  agentRef: alice
  schedule: "0 9 * * *"        # every day at 9am
  type: heartbeat
  task: "Review overnight alerts and summarize status"
  includeMemory: true           # inject persistent memory
  concurrencyPolicy: Forbid     # skip if previous run still active
```

## Model Overrides

Runs created by a schedule inherit the Agent's model configuration. The optional
`model`, `provider`, and `baseURL` fields override it per schedule — useful for
running cheap heartbeats on an expensive agent, or vice versa:

```yaml
spec:
  agentRef: alice
  schedule: "*/30 * * * *"
  task: "Quick cluster health check"
  model: claude-haiku-4-5       # override just the model
```

Unset fields still inherit from the Agent (including auth, thinking, maxTokens,
and temperature). Agents can pass the same overrides via the `schedule_task`
tool or `sympozium-tool schedule --model/--provider/--base-url`.

## Agent-Managed Schedules

Agents can manage and inspect their own schedules with the `schedule_task` tool
(agent-runner) or `sympozium-tool schedule` (Codex / Claude Code harnesses). Agent
pods hold **no Kubernetes RBAC**; every action is brokered through the controller:

1. The tool writes a request to `/ipc/schedules/schedule-<id>.json`.
2. The IPC bridge sidecar publishes it on `schedule.upsert`.
3. The controller's schedule router applies it with its own RBAC and replies on
   `schedule.result.<agentRunID>` with the state it actually applied.
4. The bridge drops the reply at `/ipc/schedules/result-<id>.json`; the tool,
   which has been blocking on that file, returns it to the agent.

| Action | Effect | Reply carries |
|--------|--------|---------------|
| `create` | New `SympoziumSchedule` named `<agent>-<name>` (type `heartbeat`, `Forbid`, memory on). Falls back to `update` if it already exists — and says so. | Applied spec + CR name |
| `update` | Change cron / task / model / provider / baseURL. **Also clears `suspend`**; the reply lists `resumed` when that happened. | Applied spec |
| `suspend` / `resume` | Set or clear `spec.suspend`. Idempotent; a no-op is reported as "already …". | Applied suspend state |
| `delete` | Remove the CR. | Confirmed CR name |
| `status` | Read one schedule: cron, suspend state, phase, run counts, last/next run time, and the last AgentRun's phase, error and result excerpt. | `ScheduleInfo` |
| `list` | Every schedule in the namespace whose `agentRef` is this agent — agent-created and operator-created alike. `name` not needed. | `[]ScheduleInfo` |

Name resolution for everything but `create`: the agent-prefixed CR (`<agent>-<name>`)
is tried first, then the bare name **only if that schedule's `agentRef` is the
requesting agent**. Other agents' schedules are invisible even by exact name, so
the read path widens nothing beyond what the agent could already mutate.

Errors the controller hits (not found, validation, API failures) come back in the
reply instead of only landing in controller logs, so a tool call no longer reports
"deleted" for a schedule that never existed. If no reply arrives within 15 s
(controller predating this feature, bridge down), mutations return the old
optimistic message flagged *unconfirmed*, and `status`/`list` return an error.
`sympozium-tool schedule` exits 0 on success, 1 on a controller-reported error,
124 on no reply; `--json` prints the raw reply.

## Concurrency Policies

Concurrency policies work like `CronJob.spec.concurrencyPolicy` — a natural extension of Kubernetes semantics:

| Policy | Behaviour |
|--------|-----------|
| `Allow` | Multiple runs can execute concurrently |
| `Forbid` | Skip the scheduled run if the previous one is still active |
| `Replace` | Cancel the active run and start a new one |

## Heartbeat Presets

| Preset | Cron | Good for |
|--------|------|----------|
| Every 30 min | `*/30 * * * *` | Active incident monitoring, SRE on-call |
| Every hour | `0 * * * *` | General ops, default for most users |
| Every 6 hours | `0 */6 * * *` | Light-touch monitoring, cost-sensitive setups |
| Daily at 9 AM | `0 9 * * *` | Daily audits, reports, security scans |
| Disabled | — | On-demand only, no background activity |

## Managing Schedules

Change the heartbeat at any time through the TUI edit modal or by editing the CR:

```bash
kubectl edit sympoziumschedule <instance>-heartbeat
```

Or use the TUI slash command:

```
/schedule <instance> "*/30 * * * *" "Check cluster health every 30 minutes"
```
