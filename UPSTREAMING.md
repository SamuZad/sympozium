# Upstreaming Plan

Tracking document for contributing the work on this fork's `dev` branch back to
`sympozium-ai/sympozium` (upstream), bit by bit.

**This file is fork-only. Never include it in an upstream PR.**

- Fork point (merge base): `6479fe5` — 2026-06-12
- Delta analyzed: `dev` @ `663b55f` (2026-08-28) vs `upstream/main` @ `f61e861` (2026-09-01)
- 26 unique commits, 188 files, +23,378 / −6,896. `git cherry upstream/main origin/dev`
  confirms zero equivalent patches upstream.
- Upstream has moved **244 commits** past the fork point. Regenerate this picture any time with:

```sh
git fetch origin && git fetch upstream
git log --oneline upstream/main..origin/dev     # ours, not upstreamed
git log --oneline origin/dev..upstream/main     # theirs, we don't have
git cherry upstream/main origin/dev             # '-' means an equivalent patch landed upstream
```

## Ground rules for every PR

1. **Rebuild, don't replay.** Branch from `upstream/main` and re-apply the change as a
   clean topic branch (cherry-pick + fix up, or re-implement). Several dev commits bundle
   unrelated concerns (noted per batch below) and must be split.
2. **Check upstream first.** Upstream is active in adjacent areas (see "Upstream landscape"
   below). Before opening a PR, re-check whether the problem is already solved or the
   surrounding code was refactored.
3. **History hygiene.** `ce08cf9` accidentally committed a 2,646-line log file (removed in
   `a27c1bf`). Any batch containing it must be squashed so the file never appears.
4. **CRD regen travels with its PR.** Every API-type change regenerates
   `config/crd/bases/`, `charts/sympozium/crds/`, and `charts/sympozium-crds/templates/`
   in the same commit.
5. **Update the checkboxes here** as PRs are opened/merged, and record the upstream PR number.

## Upstream landscape (what changed while we diverged)

Things upstream built in those 244 commits that affect our strategy:

- **AgentHarness / AgentRuntime system** — upstream now has an admin-owned `AgentRuntime`
  CRD, an approved-runtime registry with digest-pinned adapter images, runtime selection
  at agent/run creation, runtime inheritance across entrypoints, persistent
  `HarnessSession` chat, harness NATS ACL restrictions, and credential allowlists.
  This is upstream's answer to the same problem as our `harness` enum field. **Our codex
  work should be re-packaged as an adapter/AgentRuntime for their system, not proposed
  as a competing `harness` field** (Batch 10).
- **Per-run identity isolation** (`feat/per-run-identity-boundary`) — per-run Kubernetes
  identities may invalidate our SA-naming-convention auth (`<agent>-agent`,
  `<base>-channel`) used by the artifact server and memory server. Verify before Batches 9/11.
- **Memory**: no storage rework upstream — they added `MEMORY_AUTO_STORE` opt-out,
  admin-only memory delete, and `injectMemory` for sandbox runs. Our Postgres/pgvector
  rework is still novel but must absorb those behaviors (Batch 9).
- **NATS**: upstream added per-run NATS ACLs, authenticated density subscriber, and
  bounded core-NATS publish flushes. Complementary to our JetStream resilience work, but
  `internal/eventbus/nats.go` will conflict (Batch 2).
- **Controller refactor** `837324e` converged AgentRun/Ensemble create/update paths —
  overlaps in spirit with our whole-spec propagation fix (`324da9b`); verify ours is
  still needed after rebase (Batch 4).
- **agent-runner tools**: upstream added `edit_file`, ranged `read_file`, tool-result
  compaction, JSONL logging — expect conflicts in `cmd/agent-runner/tools.go` (Batches 5/6).
- **Celln backend** — a new hermetic execution backend for AgentRuns; touches spawning
  paths our tolerations/harness changes also touch.
- **Polymorphic `spec.task`** (string or object) — touches many run-creation sites we
  also touch.

## Not yet triaged

New on `dev` since the 2026-09-01 analysis — not part of any batch yet:

- `3c2aa80` claude code harness - v1 — likely folds into Batch 10 (as a second adapter
  for upstream's AgentRuntime system alongside codex).
- `ee683d6` make scheduling more feature complete — likely extends Batch 8.

## Contribution batches

Ordered by effort/dependency: small standalone fixes first, architectural proposals last.

---

### Batch 1 — Tolerations
- [x] Branch prepared (2026-09-07): `feat/tolerations` off `upstream/main` @ `027682d`,
      changes left **uncommitted** in the working tree for the author to commit/push.
      Adaptations vs the fork commits: tolerations added to upstream's new
      `BuildStimulusRun` (stimulus.go) and `buildAgentPodTemplate` (shared by Job +
      agentSandbox paths), plus the TUI run creator; `convergenceFixture` gained a
      toleration for the parity test; fork-only `ensemble_update_propagation_test.go`
      dropped (never existed upstream — its assertion is covered by the parity/convergence
      tests); `TestBuildJob_*Tolerations` updated to the new `buildJob` signature.
      Full `go test -short ./...` green.
- [x] PR opened: sympozium-ai/sympozium#440 · merged: 2026-09-11 (`df23b2f`)

**Source:** `eb5268f`, `f9a6789`
**Scope:** `tolerations` on Agent config, AgentRun spec, and Ensemble personas; propagated
through every pod-creation path (runs, channels, web endpoints, schedules, sandbox pools).
Tests included. Pairs with `nodeSelector` for tainted/GPU pools.
**Prep:** none — self-contained, easiest first PR.
**Conflict risk:** low (Celln backend added spawning paths; check for a new site to propagate to).

---

### Batch 2 — NATS/JetStream resilience (rescoped)
- [x] Branch prepared (2026-09-11): `fix/nats-startup-resilience` off `upstream/main`
      @ `990f9bc`, changes left **uncommitted** for the author to commit/push.
      `go test -race -short ./internal/eventbus/` green; full `-short` suite green except
      `internal/cellnparent` `TestRegisteredParentBindsLiveSelectionWithoutWidening`,
      which fails identically on pristine `upstream/main` on this macOS machine while
      upstream CI (Linux) is green — environment-specific, package untouched.
- [x] PR opened: sympozium-ai/sympozium#484 (commit `c7cbcf3`; the real-server test
      `TestNATSRealServerLazyStartupRecoversWhenServerAppearsLater` was added after that
      commit and needs a follow-up push) · merged: ____

**Source:** `e1c5d53` (NATS half only), `620eede`, `d366567`
**Superseded — do not re-propose.** Upstream's `f341b37` "recover NATS subscriptions
after restart/recreate" (#254) independently landed: `MaxReconnects(-1)`,
disconnect/reconnect/closed handlers, consumer + stream recreation on fetch error,
`msgs.Error()` batch-error detection, `ensureStream`/`createConsumer` factoring, and
`Term()` for poison messages (better than our `Nak`). The fork's generation-counted
single-flight resync is an optimization upstream's simpler approach doesn't need.
**What remains novel (this is the PR):**
1. `InactiveThreshold: 1h` on the ephemeral subscribe consumer — upstream uses the server
   default (~5s), so any fetch gap reaps the consumer and the `DeliverNew` recreate drops
   every event published in between.
2. Publish-side recovery — on stream-gone errors (`ErrStreamNotFound`,
   `ErrNoStreamResponse`, `ErrNoResponders`) recreate the stream via `ensureStream` and
   retry once, instead of failing in the reconnect→ensureStream window.
3. Lazy startup for long-lived processes — `NewNATSEventBus` (controller) now returns a
   usable bus when NATS is unreachable at boot and its `Subscribe` returns a live channel
   that creates its consumer once NATS answers. Upstream today logs "channel routing
   disabled" and runs with routing off for the life of the process if NATS isn't up
   within ~20s of boot; a failed initial `Subscribe` in a router takes the whole manager
   down. `NewNATSEventBusWithContext` (API server, per-request subscribers) keeps its
   fail-fast contract unchanged.
Tests in `nats_resilience_test.go`: no-server tests in upstream's style (reserved loopback
port, shrunken provisioning knobs) plus an opt-in real-server test
`TestNATSRealServerLazyStartupRecoversWhenServerAppearsLater` (bus + subscription created
before NATS exists, server started afterwards, stream deleted under a live connection).
Both it and upstream's `TestNATSRealServerReconnectAfterStartupContextExpires` pass under
`-race` locally with `SYMPOZIUM_NATS_SERVER` pointing at a `go install`ed
`github.com/nats-io/nats-server/v2` binary. Implemented as edits on upstream's file, not a
cherry-pick — the fork's `nats_test.go` targets fork-only internals and was not ported.

---

### Batch 3 — Channel reliability: failure replies + instance filtering
- [ ] PR opened: ____ · merged: ____

**Source:** `e1c5d53` (router half), `8552fb7`
**Scope:** (a) failed channel-originated runs reply into the originating Slack/Telegram
thread with actionable messages (timeout advice points at `runTimeout`); (b)
`BaseChannel.SubscribeOutbound` filters outbound events by `instanceName` — fixes
cross-instance mis-delivery when multiple channel pods share the topic.
**Prep:** these are separable into two small PRs if upstream prefers.
**Conflict risk:** low–medium.

---

### Batch 4 — Controller robustness: sidecar fast-fail, propagation, mounts, runTimeout
- [ ] PR opened: ____ · merged: ____

**Source:** `324da9b`, `419895c` (runtimeout half only)
**Scope, separable into up to 4 PRs:**
1. Sidecar failure fast-fail: crashed/ImagePullBackOff sidecars fail the run immediately
   with actionable per-cause diagnostics instead of burning the run timeout.
2. Whole-spec Ensemble→Agent propagation (`equality.Semantic.DeepEqual` on rebuilt spec;
   managed-labels-only reconciliation). **Superseded**: upstream's `837324e` already
   assigns the whole spec and guards it with `ensemble_parity_test.go` — drop this item.
   Upstream also already has persona-level `runTimeout` (seen during Batch 7 rebase),
   so verify how much of item 4 below remains needed.
3. `/skills` parent mount writable — fixes exit-128 StartError under
   `ReadOnlyRootFilesystem` (regression test included).
4. `runTimeout` honored at all four run-creation sites via `EffectiveRunTimeout()`
   (was hardcoded 10m) + persona-level `runTimeout` field.
**Prep:** `419895c` bundles a memory-server migration retry — that part belongs to Batch 9.
**Conflict risk:** medium (controller files are active upstream).

---

### Batch 5 — Skills rework + tool-executor consolidation
- [ ] PR opened: ____ · merged: ____

**Source:** `173a809`, `4e51b4c`, `b921a8e` (three natural PRs, in this order)
**Scope:**
1. Lazy skill loading: compact catalog in the system prompt + new `skills` tool that
   fetches full bodies on demand; per-pack `/skills/<pack>/` layout; YAML frontmatter
   rendering in the SkillPack controller.
2. `execute_command` target resolution: validate against `SYMPOZIUM_SKILL_TARGETS` with
   case-insensitive + unique-suffix matching, advertise via enum, fail fast with the list
   of valid targets instead of hanging.
3. Tool-executor consolidation: canonical `tool-executor.sh` embedded in the controller,
   shipped as a per-namespace ConfigMap mounted into all sidecars — stock images work
   with no custom build. Note: adds ConfigMap RBAC in agent namespaces; llmfit timeout
   clamp changes 180s→120s (flag in the PR description).
**Conflict risk:** medium in `cmd/agent-runner/tools.go` (upstream added new native tools).

---

### Batch 6 — Model tuning: sampling controls + reasoning
- [ ] PR opened: ____ · merged: ____

**Source:** `7959938`, `c943d55`
**Scope, separable into 3 PRs:**
1. `maxTokens` / `temperature` / `thinking` on Agent config, AgentRun model spec, and
   Ensemble personas; propagation at every run-creation site; Anthropic thinking budgets
   + history round-trip fix; OpenAI `reasoning_effort` + dual MaxTokens fields.
   Includes the latent-bug fix where built agent env (`RUN_TIMEOUT`, OTel) was never
   attached to the container.
2. Reasoning-effort graceful degradation: one-retry latch when a backend rejects
   `reasoning_effort` (`c943d55`).
3. Channel Deployment drift reconciliation: channel spec edits (secrets, Slack options,
   image bumps) propagate to running deployments with no spurious update churn.
**Conflict risk:** medium (provider files, run-creation sites touched by polymorphic task).

---

### Batch 7 — Stable session keys + persistent workspaces
- [x] Local branches ready (2026-09-01): `feat/stable-session-keys` (PR 1) and
      `feat/workspace-sessions` (PR 2, stacked on PR 1). Rebuilt on `upstream/main`
      @ `c3bab88` (#407); builds clean, full `go test -short ./...` passes.
      (Upstream's own CI was red at `f61e861`/#406 — `TestCreateInstance_
      NoHardcodedOTLPEndpoint` — and green again at #407; we rebased past it.)
      The codex Dockerfile hunk from `75171fc` was dropped (Batch 10).
      `convergenceFixture` in `ensemble_parity_test.go` gained a `Workspace`
      value to satisfy upstream's ensemble-expressibility parity test.
- [x] PR 1 opened: sympozium-ai/sympozium#408 · merged: 2026-09-07 (`13ce3a3`)
- [x] PR 2 opened: sympozium-ai/sympozium#409 · merged: 2026-09-07 (`30c8ead`)
      Note: GitHub's native stacked PRs don't work here — stacks can't chain across
      forks (base must be a branch in the upstream repo) and the upstream repo has
      not enabled the preview. Classic draft-note stacking instead.

**Source:** `e9d2946`, then `d1f00f3` + `14bb6cf` + `75171fc` (workspace half) + `5a9a73f`
**Scope, two PRs in strict order:**
1. `internal/sessionkey` package: stable structured session keys
   (`chan:slack:C123:<thread>`, `sched:<name>`, per-instance web endpoints, MCP session
   IDs) replacing nanosecond-unique keys. **Behavioral change PR** — threads/schedules
   become continuous sessions; sell this on memory-continuity grounds.
2. `WorkspaceSession` CRD + controller (per-(agent, sessionKey) PVC, hash-based naming,
   idle-TTL sweeper, grow-only resize, cascade delete), FIFO session-lock admission
   (deadlock fix from `14bb6cf` — required, not optional), `workspace-marker` init
   container as a Go subcommand (`75171fc`; the shell version breaks distroless — take
   the fixed version only), and the `sympozium workspace` CLI (list/show/delete/exec).
**Prep:** `75171fc` also pins the codex Dockerfile version — that part belongs to Batch 10.
Check interaction with upstream's persistent `HarnessSession` — related concept for chat
sessions; ours is storage-level and should complement it, but say so in the PR.
**Conflict risk:** medium.

---

### Batch 8 — Per-schedule model overrides
- [ ] PR opened: ____ · merged: ____

**Source:** `663b55f`
**Scope:** `model` / `provider` / `baseURL` on `SympoziumScheduleSpec`, applied by the
schedule controller (auth stays with the Agent); `schedule_task` agent tool + IPC router
parameters; docs.
**Prep:** the commit also touches `cmd/sympozium-tool` and `cmd/harness-codex` (fork-only
binaries) — drop those hunks unless Batch 10 has landed.
**Conflict risk:** low.

---

### Batch 9 — Memory rework (architectural — RFC first)
- [ ] RFC/issue opened: ____ · PR opened: ____ · merged: ____

**Source:** `d005157`, `419895c` (migrate-retry half)
**Scope:** replace the three legacy memory mechanisms (log-scraped MEMORY.md ConfigMap,
per-agent SQLite, per-ensemble shared SQLite) with one cluster-wide memory-server on
Postgres + pgvector: TokenReview identity, server-side membrane enforcement,
cross-ensemble import/export selectors, hybrid RRF retrieval, embeddings config
(OpenAI/Azure/Ollama), `pkg/memoryclient`, 3 scoped agent tools, idempotent ensemble
seeds with GC, Helm (server + migration job + optional bundled pgvector + RDS IAM).
**Prep — fix our known gaps before proposing:**
- [ ] Cross-ensemble matcher requires both selectors while validation allows one
      (valid single-selector rules silently share nothing) — align them.
- [ ] `SharedMemorySpec.AccessRules` retained in API but enforced nowhere — enforce or remove.
- [ ] `WORKFLOW_MEMBRANE_VISIBILITY` read but never set by any controller — wire or drop.
- [ ] Remove the drifted duplicate `migrations/001_initial.sql.tmpl` at repo root.
- [ ] Absorb upstream behaviors: `MEMORY_AUTO_STORE` opt-out, admin-only delete,
      sandbox-run memory injection, managed-server image reconcile.
- [ ] Re-verify SA-based identity against upstream's per-run identity isolation.
- [ ] Docs/CHANGELOG drift (env var names, membrane YAML shape, auto-store attribution).
**Conflict risk:** highest of all batches. This deletes upstream-maintained code; open an
issue/RFC and get maintainer buy-in on the architecture before writing the PR.

---

### Batch 10 — Codex runtime (re-package for upstream's AgentRuntime system)
- [ ] Strategy agreed with upstream: ____ · PR opened: ____ · merged: ____

**Source:** `ce08cf9`, `513c409`, `148522b`, `75171fc` (Dockerfile pin), plus the
claude-code harness work (2026-09, uncommitted at time of writing)
**Scope:** the valuable, portable pieces:
- `internal/harness` — the shared shim plumbing (skills/channel-context assembly,
  `sympozium-tool` docs, MCP bridge discovery + readiness wait, final-answer attachment
  scanning, IPC result, OTel). Both shims below build on it; propose it as the
  adapter SDK for exec-style runtimes.
- `cmd/harness-codex` entrypoint (auth.json, AGENTS.md from skills + system prompt +
  channel context, config.toml rendering, `codex exec`, IPC result) and
  `images/harness-codex` — **as an adapter image for upstream's AgentRuntime registry**
  (digest-pinned, per their hardening rules), not via our `harness` enum field.
- `cmd/harness-claude-code` entrypoint (CLAUDE_CONFIG_DIR on the workspace PVC,
  CLAUDE.md + `--append-system-prompt` context, MODEL_*→ANTHROPIC_*/CLAUDE_CODE_*
  env mapping, `--mcp-config` bridge, `claude -p` stream-json parsing with token/tool
  metrics, `--continue` session continuity keyed off SESSION_KEY) and
  `images/harness-claude-code` — same adapter packaging. Docs: `docs/guides/harnesses.md`.
  Note upstream's `harness-reference` image and `AgentHarness` docs are the shape to match.
- `cmd/sympozium-tool` (send-message, schedule, exec with target resolution,
  memory-*, get-attachment) — the shell bridge any exec-style runtime needs.
- `internal/mcpbridge/local_http.go` local HTTP MCP server + the bridge readiness fix
  (`148522b`: bind before discovery, gate on readiness) — the readiness fix may be
  upstreamable standalone even sooner.
- OTel: collector-contrib OTTL transforms normalizing codex metrics,
  `gen_ai.client.token.usage` histogram.
**Prep:** squash out the stray log file; drop our `harness` CRD field in favor of
upstream's runtime selection; drop the dead `claude-code` enum value; map our image
selection onto their adapter catalog. Study their reference adapter
(`build-harness-publish-reference-adapter-image`) first.
**Conflict risk:** high conceptually (their system, our port), low mechanically once re-packaged.

---

### Batch 11 — Artifact service + file attachments
- [ ] PR opened: ____ · merged: ____

**Source:** `a27c1bf`, `f990264`, `3365f6e`, `003fb32`
**Scope:** `cmd/artifact-server` (TokenReview auth + LRU cache, capability IDs, TTL/orphan
pruning, Helm deployment/PVC/RBAC); `Attachments` on IPC + channel types; agent-runner
`send_channel_message` attachments; Slack 3-step external upload (with the `f990264`
form-encoding/validation fixes); inbound Slack files → artifacts →
`INBOUND_ATTACHMENTS` → `/workspace/attachments/`; `internal/artifact` client package.
**Prep — finish the WIP before proposing:**
- [ ] Attachment-only Slack messages (file, no text) are dropped before ingest — handle
      `file_share` / empty-text events.
- [ ] Slack-only — decide whether upstream wants at least one more channel or an explicit
      capability flag per channel.
- [ ] Deduplicate upload/fetch client code (Slack channel + sympozium-tool onto
      `internal/artifact`).
- [ ] Add tests for `internal/artifact` (`MaterializeInbound`, ingest, annotation→env plumbing).
- [ ] Inbound artifacts never explicitly deleted (TTL only) — decide lifecycle.
- [ ] Echo-loop risk: harness-codex response scanning can re-upload files from
      `/workspace/attachments/` — exclude inbound dir from `buildResponseAttachments`.
- [ ] Re-verify sibling-SA authorization (`<base>-agent` / `<base>-channel`) against
      upstream's per-run identity isolation.
**Depends on:** Batch 10 strategy for the harness-codex attachment scanning parts
(agent-runner + channel parts are independent).
**Conflict risk:** medium.

---

## Status board

| # | Batch | Size | Risk | Status |
|---|-------|------|------|--------|
| 1 | Tolerations | S | low | **merged** (#440) |
| 2 | NATS resilience (rescoped) | S | low | branch ready (uncommitted), not pushed |
| 3 | Channel reliability | S | low | not started |
| 4 | Controller robustness | M | medium | not started |
| 5 | Skills + tool executor | M | medium | not started |
| 6 | Model tuning | M | medium | not started |
| 7 | Session keys + workspaces | L | medium | **merged** (#408, #409) |
| 8 | Schedule model overrides | S | low | not started |
| 9 | Memory rework | XL | RFC required | not started |
| 10 | Codex runtime (as adapter) | L | strategy required | not started |
| 11 | Artifact service + attachments | L | WIP to finish | not started |
