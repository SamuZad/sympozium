// "Why did this fail": turns the conditions, owner outcome and turn results a
// run already carries into one plain explanation and what to do about it. Pure
// and browser-only — it reads nothing the API has not already returned, and it
// is the single source for failure wording in the console.
import type { AgentRun, AgentRunTurn, CellnPlatformProfile, Condition } from "@/lib/api";

export type DiagnosisSeverity = "error" | "warning" | "info";

export type DiagnosisKind =
  | "admission-refused"
  | "admission-pending"
  | "continuation-withheld"
  | "parent-lost"
  | "create-refused"
  | "reconciliation-required"
  | "turn-failed"
  | "run-failed";

export type DiagnosisAction =
  | { kind: "link"; label: string; to: string }
  /** Restart the conversation on a new parent (api.runs.continue); the component decides whether it applies. */
  | { kind: "continue"; label: string };

export interface DiagnosisStep {
  label: string;
  detail: string;
  action?: DiagnosisAction;
}

export interface Diagnosis {
  kind: DiagnosisKind;
  severity: DiagnosisSeverity;
  title: string;
  /** One plain sentence. */
  cause: string;
  /** Stable machine code when there is one (AUTH_*, owner status, harness error). */
  code?: string;
  /** Raw condition / outcome text, truncated. */
  evidence: string[];
  nextSteps: DiagnosisStep[];
}

export interface DiagnosisContext {
  /** Fleet runtime profiles (api.cellnPlatform.profiles) — lets a tool refusal name the tool. */
  profiles?: CellnPlatformProfile[];
}

const EVIDENCE_LIMIT = 600;
const LOST_STATUSES = new Set(["ContextLost", "Stopped", "TeardownUncertain"]);

function truncate(text: string, limit = EVIDENCE_LIMIT): string {
  const clean = text.trim();
  return clean.length > limit ? `${clean.slice(0, limit - 1)}…` : clean;
}

/** A condition counts only for the generation it observed, as everywhere else in the console. */
function currentConditions(run: AgentRun): Condition[] {
  const generation = run.metadata.generation;
  return (run.status?.conditions || []).filter((condition) =>
    generation === undefined || condition.observedGeneration === undefined || condition.observedGeneration === generation);
}

function conditionText(condition: Condition): string {
  return `${condition.type}=${condition.status}${condition.reason ? ` (${condition.reason})` : ""}${condition.message ? `: ${condition.message}` : ""}`;
}

function agentTab(run: AgentRun, tab: "harness" | "chat"): string {
  return `/agents/${encodeURIComponent(run.spec.agentRef)}?tab=${tab}`;
}

function harnessStep(run: AgentRun, label: string, detail: string): DiagnosisStep {
  return { label, detail, action: { kind: "link", label: "Open the Agent's Harness tab", to: agentTab(run, "harness") } };
}

function newConversationStep(run: AgentRun, detail: string): DiagnosisStep {
  return { label: "Start a new conversation", detail, action: { kind: "link", label: "Open the Agent's Chat tab", to: agentTab(run, "chat") } };
}

const replaceRunStep: DiagnosisStep = {
  label: "Delete this run, then start a new one",
  detail: "A run keeps the selection it was created with, so the fix only reaches a new run. Delete this pending one first so two runs never compete for the same admission.",
};

const waitStep: DiagnosisStep = {
  label: "Leave this run in place",
  detail: "Admission is retried automatically; once the policy side is fixed this run starts on its own. Do not create a replacement while it is still pending.",
};

// ── Admission refusals ───────────────────────────────────────────────────────

interface AuthEntry {
  title: string;
  cause: (facts: AuthFacts) => string;
  steps: (run: AgentRun, facts: AuthFacts) => DiagnosisStep[];
}

interface AuthFacts {
  code: string;
  /** The resolver's own sentence, when the controller could share it. */
  detail: string;
  /** Tools the run borrows that the fleet profile does not lend. */
  unknownTools: string[];
  profile?: CellnPlatformProfile;
}

/**
 * One entry per reason code the platform resolver can return
 * (internal/cellnauthority/platform_resolver.go) plus AUTH_PROTOCOL_UNSUPPORTED.
 * The person reading this usually IS the operator, so every entry says what to
 * change, not whom to ask.
 */
export const AUTH_REASONS: Record<string, AuthEntry> = {
  AUTH_TOOL_UNKNOWN: {
    title: "Admission refused: a tool is not lent by the fleet policy",
    cause: ({ unknownTools }) => unknownTools.length
      ? `The run borrows ${unknownTools.length === 1 ? "a tool" : "tools"} the fleet policy does not lend at that revision: ${unknownTools.join(", ")}.`
      : "The run borrows a tool the fleet policy does not lend (unknown tool, or a revision the policy does not list).",
    steps: (run, { unknownTools, profile }) => [
      harnessStep(run, unknownTools.length ? `Remove ${unknownTools.join(", ")} from the Agent's tools` : "Remove the unknown tool from the Agent's tools",
        profile?.tools.length
          ? `Agent → Harness. The profile "${profile.name}" lends only: ${profile.tools.map((tool) => `${tool.name}@${tool.revision}`).join(", ")}.`
          : "Agent → Harness. Keep only tools the fleet profile lists, at the exact revision it lists."),
      { label: "Or lend the tool", detail: "If the run should have it, add the tool and revision to the CellnExecutionPolicy that selects this namespace (kubectl edit cellnexecutionpolicy)." },
      replaceRunStep,
    ],
  },
  AUTH_TOOL_ORDER_MISMATCH: {
    title: "Admission refused: the tool selection is not exact",
    cause: () => "The run's tool list repeats a name, omits an exact revision, or changed between being resolved and being admitted.",
    steps: (run) => [
      harnessStep(run, "Re-save the Agent's tools", "Agent → Harness. Pick each tool once, at the revision the fleet profile lists."),
      replaceRunStep,
    ],
  },
  AUTH_POLICY_CONTRACTED: {
    title: "Admission refused: the run no longer matches what policy grants",
    cause: ({ detail }) => /persona/i.test(detail)
      ? "The run's system prompt differs from the persona bound to its fleet runtime profile."
      : detail
      ? `The run asks for something the fleet policy or runtime profile does not grant: ${detail}.`
      : "The run asks for something the fleet policy or runtime profile does not grant, or one of them changed while the run was being admitted.",
    steps: (run, { detail, profile }) => /persona/i.test(detail) ? [
      newConversationStep(run, "The Chat tab sends the profile's system prompt verbatim, which is what the policy requires."),
      { label: "Creating runs through the API?", detail: `Send spec.systemPrompt exactly as the profile${profile ? ` "${profile.name}"` : ""} returns it from /api/v1/celln-platform/profiles — no edits, no extra whitespace.` },
      replaceRunStep,
    ] : [
      harnessStep(run, "Re-select the fleet backend", "Agent → Harness. Choosing the backend again rebinds the Agent to the profile's current revision, model route and tools."),
      { label: "Check the policy still permits this", detail: "The CellnExecutionPolicy selecting this namespace must allow the runtime profile revision and the enduring lifecycle (kubectl get cellnexecutionpolicy -o yaml)." },
      replaceRunStep,
    ],
  },
  AUTH_POLICY_WITHDRAWN: {
    title: "Admission refused: no fleet policy covers this run",
    cause: ({ detail }) => detail
      ? `Nothing currently authorises this run: ${detail}.`
      : "Nothing currently authorises this run: no execution policy selects its namespace, or the Agent, runtime or runtime profile it depends on is missing.",
    steps: (run) => [
      { label: "Make a policy select this namespace", detail: `Label namespace "${run.metadata.namespace || "default"}" so a CellnExecutionPolicy's namespaceSelector matches it, or widen the selector (kubectl get cellnexecutionpolicy -o yaml).` },
      harnessStep(run, "Check the Agent's backend still exists", "Agent → Harness. If the runtime or its profile was removed, choose a fleet backend again to recreate the wrappers."),
      waitStep,
    ],
  },
  AUTH_ROUTE_MISMATCH: {
    title: "Admission refused: the model route is not permitted",
    cause: ({ detail }) => detail
      ? `The run's model route is not one the fleet policy permits: ${detail}.`
      : "The run's model connection and model are not the exact route the fleet policy permits, or the connection is disabled or does not list that model.",
    steps: (run, { profile }) => [
      harnessStep(run, "Choose a model the profile offers", profile
        ? `Agent → Harness. The profile "${profile.name}" runs ${profile.model} on ${profile.provider}; the run must use that route through its model connection, with no inline override.`
        : "Agent → Harness. Pick the fleet backend again so the Agent uses the profile's own model connection and model, with no inline override."),
      replaceRunStep,
    ],
  },
  AUTH_LIMIT_OUT_OF_RANGE: {
    title: "Admission refused: a limit is outside what policy allows",
    cause: ({ detail }) => detail
      ? `The run asks for a budget or size the policy does not allow: ${detail}.`
      : "The run's task size or requested budget (lease, turns, model requests, output tokens) is outside the policy's range.",
    steps: (run, { profile }) => [
      { label: "Ask for less, or a shorter task", detail: profile
        ? `The profile "${profile.name}" allows at most ${profile.ceilings.maxTurns} turns, ${profile.ceilings.maxModelRequests} model requests, ${profile.ceilings.maxOutputTokens} output tokens and a ${profile.ceilings.leaseSeconds}s lease — and the budget must still afford one full turn.`
        : "Keep spec.enduring within the profile's ceilings while still affording one full turn, and keep the first message short." },
      newConversationStep(run, "The Chat tab asks for the profile's default budget, which is always in range."),
      replaceRunStep,
    ],
  },
  AUTH_LIFECYCLE_INVALID: {
    title: "Admission refused: the run asks for something fleet runs cannot do",
    cause: ({ detail }) => detail
      ? `Fleet execution does not support what this run or its Agent requests: ${detail}.`
      : "The run or its Agent requests pods, delegation, lifecycle hooks, extra context or a non-text task, which fleet execution does not support.",
    steps: (run) => [
      harnessStep(run, "Remove the unsupported settings", "Agent → Harness / Lifecycle. Drop lifecycle hooks, delegation and pod-level settings, and send a plain text task — or run this Agent on a non-fleet backend."),
      replaceRunStep,
    ],
  },
  AUTH_NAMESPACE_UID_MISMATCH: {
    title: "Admission refused: the namespace changed identity",
    cause: () => "The namespace was deleted and recreated (or changed) while this run was being admitted, so the grant no longer names it.",
    steps: () => [replaceRunStep],
  },
  AUTH_PARENT_TURN_MISMATCH: {
    title: "Admission refused: the parent or turn identity does not match",
    cause: () => "The turn or parent this admission was issued for is not the one now asking — typically a run or turn that was recreated under the same name.",
    steps: (run) => [newConversationStep(run, "The old identity cannot be re-admitted; a new conversation gets its own."), replaceRunStep],
  },
  AUTH_PROTOCOL_UNSUPPORTED: {
    title: "Admission refused: shared tools need fleet-mediated admission",
    cause: () => "The run selects shared catalogue tools but reached the legacy admission path, which cannot honour them and refuses rather than dropping them.",
    steps: (run) => [
      { label: "Enable fleet admission for this install", detail: "Shared (cluster) tools are only admitted through the platform resolver; check that the controller runs with Celln platform admission configured." },
      harnessStep(run, "Or remove the shared tools", "Agent → Harness. Without clusterToolRefs the run can use the legacy path."),
      replaceRunStep,
    ],
  },
};

function defaultAuthEntry(code: string): AuthEntry {
  const credential = code.startsWith("AUTH_CRED_") || ["AUTH_ISS_MISMATCH", "AUTH_AUD_MISMATCH", "AUTH_SUBJECT_MISMATCH", "AUTH_OPERATION_MISMATCH", "AUTH_DECISION_DIGEST_MISMATCH", "AUTH_VERSION_UNSUPPORTED"].includes(code);
  const expired = /EXPIRED|TIME_|WINDOW/.test(code);
  return {
    title: `Admission refused (${code})`,
    cause: ({ detail }) => credential
      ? "The admission credential the controller presented was not accepted by the node (wrong key, issuer or audience)."
      : expired
      ? "The admission was issued but not used inside its time window, usually because clocks differ or the node was slow to respond."
      : detail ? `Platform policy refused this run: ${detail}.` : `Platform policy refused this run with ${code}.`,
    steps: () => credential ? [
      { label: "Check the fleet signing key", detail: "The controller's issuer key and the dispatcher's trusted key must be the same generation; re-run the fleet install step that distributes it." },
      waitStep,
    ] : expired ? [
      { label: "Check node clocks and load", detail: "Make sure controller and node clocks agree (NTP) and the dispatcher is responsive." },
      waitStep,
    ] : [
      { label: "Compare the run with its fleet profile", detail: "Namespace, runtime profile, tools and model route must all be ones the CellnExecutionPolicy permits." },
      waitStep,
    ],
  };
}

const REFUSAL = /refused admission \((AUTH_[A-Z0-9_]+)\)(?::\s*(.*?))?\.\s+Ask the operator/s;

export function parseAdmissionRefusal(message: string | undefined): { code: string; detail: string } | null {
  if (!message) return null;
  const match = REFUSAL.exec(message);
  if (match) return { code: match[1], detail: (match[2] || "").trim() };
  const bare = /\b(AUTH_[A-Z0-9_]+)\b(?::\s*([^\n]*))?/.exec(message);
  return bare ? { code: bare[1], detail: (bare[2] || "").trim().replace(/\.$/, "") } : null;
}

function profileFor(run: AgentRun, profiles: CellnPlatformProfile[] | undefined): CellnPlatformProfile | undefined {
  const runtime = run.spec.cellnSelection?.runtimeRef;
  return profiles?.find((profile) => (runtime && profile.wrapper === runtime) || profile.agent === run.spec.agentRef);
}

/** Names the refused tool: from the resolver's sentence if shared, else by comparing the selection with what the profile lends. */
export function unknownTools(run: AgentRun, detail: string, profile: CellnPlatformProfile | undefined): string[] {
  const named = [...detail.matchAll(/tool "([^"]+)"/g)].map((match) => match[1]);
  if (named.length) return [...new Set(named)];
  if (!profile) return [];
  const lent = new Set(profile.tools.map((tool) => `${tool.name}@${tool.revision}`));
  const selection = run.spec.cellnSelection;
  return [
    ...(selection?.clusterToolRefs || []).filter((tool) => !lent.has(`${tool.name}@${tool.revision}`)).map((tool) => `${tool.name}@${tool.revision}`),
    // Legacy namespaced tools are never lent by a shared profile.
    ...(selection?.clusterToolRefs?.length ? [] : (selection?.toolRefs || []).map((tool) => `${tool.name}@${tool.revision}`)),
  ];
}

function diagnoseAdmission(run: AgentRun, condition: Condition, context: DiagnosisContext): Diagnosis {
  const refusal = parseAdmissionRefusal(condition.message);
  const evidence = [truncate(conditionText(condition))];
  if (!refusal) {
    return {
      kind: "admission-pending", severity: "info", title: "Waiting for admission",
      cause: "No operator-prepared parent registration with current grants matches this run yet, so nothing has started.",
      evidence,
      nextSteps: [
        { label: "Check that this Agent runs on a fleet backend", detail: "A run is admitted either by fleet policy or by a parent registration prepared for it on a node. If neither exists, it waits here.", action: { kind: "link", label: "Open the Agent's Harness tab", to: agentTab(run, "harness") } },
        waitStep,
      ],
    };
  }
  const profile = profileFor(run, context.profiles);
  const facts: AuthFacts = { ...refusal, profile, unknownTools: refusal.code === "AUTH_TOOL_UNKNOWN" ? unknownTools(run, refusal.detail, profile) : [] };
  const entry = AUTH_REASONS[refusal.code] || defaultAuthEntry(refusal.code);
  return { kind: "admission-refused", severity: "error", code: refusal.code, title: entry.title, cause: entry.cause(facts), evidence, nextSteps: entry.steps(run, facts) };
}

// ── Lost parents ─────────────────────────────────────────────────────────────

function lostEvidence(run: AgentRun, conditions: (Condition | undefined)[]): string[] {
  const outcome = run.status?.cellnParent?.ownerOutcome;
  return [
    ...conditions.filter((condition): condition is Condition => Boolean(condition)).map(conditionText),
    outcome ? `ownerOutcome: status=${outcome.status} reachedReady=${outcome.reachedReady}${outcome.observedAt ? ` observedAt=${outcome.observedAt}` : ""}` : "",
    run.status?.error ? `status.error: ${run.status.error}` : "",
  ].filter(Boolean).map((text) => truncate(text));
}

const restartStep: DiagnosisStep = {
  label: "Restart the conversation elsewhere",
  detail: "Starts a new parent on any node with capacity, seeded with the recorded exchanges. This run is removed once the new one exists.",
  action: { kind: "continue", label: "Restart elsewhere" },
};

function diagnoseLost(run: AgentRun, status: string, ready: Condition | undefined, withheld: Condition | undefined): Diagnosis {
  const parent = run.status?.cellnParent;
  const continuedBy = parent?.continuedBy;
  const neverReady = parent?.ownerOutcome && !parent.ownerOutcome.reachedReady;
  const evidence = lostEvidence(run, [withheld, ready]);
  if (withheld) {
    return {
      kind: "continuation-withheld", severity: "error", code: withheld.reason, title: "Lost again — not continued automatically",
      cause: "This run was already an automatic continuation and lost its parent before taking a single follow-up, so the controller stopped continuing it to avoid a loop.",
      evidence,
      nextSteps: [
        { label: "Check the node before retrying", detail: "Two losses in a row usually mean the node cannot keep a parent alive (memory pressure, a draining node, or a harness that exits on start). Look at the dispatcher on that node first." },
        newConversationStep(run, "Recorded answers stay on this run; a new conversation starts clean on any node with capacity."),
        restartStep,
      ],
    };
  }
  if (status === "TeardownUncertain") {
    return {
      kind: "parent-lost", severity: "warning", code: status, title: "Parent teardown is unconfirmed",
      cause: "Parent teardown is unconfirmed: the node that owned this parent has not confirmed it stopped, so it may still hold memory and a model slot.",
      evidence,
      nextSteps: [
        { label: "Confirm the old parent is gone", detail: "Check the dispatcher on the owning node (or wait for the node to report back). This run is not continued automatically, because the old parent may still be live." },
        newConversationStep(run, "Once the node confirms, or if you accept the old parent may linger until its lease ends, start a new conversation. Recorded answers stay here."),
      ],
    };
  }
  const base = { kind: "parent-lost" as const, code: status, evidence };
  if (continuedBy) {
    return {
      ...base, severity: "info", title: status === "Stopped" ? "Parent stopped — conversation continued" : "Context lost — conversation continued",
      cause: status === "Stopped"
        ? `The parent has stopped; the conversation continues as ${continuedBy} on another node, seeded with what was said here.`
        : `Live harness context was lost; the conversation continues as ${continuedBy} on another node, seeded with what was said here.`,
      nextSteps: [{
        label: "Carry on in the continuation", detail: "Nothing to fix: send your next message there. It remembers the recorded exchanges, not the harness's unsaved working state.",
        action: { kind: "link", label: `Open ${continuedBy}`, to: `/runs/${encodeURIComponent(continuedBy)}` },
      }],
    };
  }
  return {
    ...base, severity: "error", title: status === "Stopped" ? "The parent has stopped" : "Live harness context was lost",
    cause: status === "Stopped"
      ? "The parent has stopped. Recorded answers remain available; this conversation cannot accept more turns."
      : neverReady
      ? "Live harness context was lost while the parent was still warming up, before it was ever ready. Recorded answers remain available, but this parent cannot resume."
      : "Live harness context was lost. Recorded answers remain available, but this parent cannot resume.",
    nextSteps: [
      restartStep,
      ...(status === "Stopped" ? [{ label: "If this was the lease", detail: "A parent stops when its lease or turn budget runs out, or when its node drains. A restarted conversation gets a fresh lease under policy." }] : []),
      ...(neverReady ? [{ label: "If it keeps dying on start", detail: "A parent that never reaches Ready points at the node or harness image rather than the conversation — check the dispatcher log on the owning node." }] : []),
    ],
  };
}

// ── Failed turns ─────────────────────────────────────────────────────────────

interface HarnessEntry { match: RegExp; title: string; cause: string; steps: (run: AgentRun, initial: boolean) => DiagnosisStep[] }

function retryStep(run: AgentRun, initial: boolean, label: string, detail: string): DiagnosisStep {
  return initial
    ? { ...newConversationStep(run, `${detail} The first turn failed, so this parent accepts no more messages.`), label }
    : { label, detail: `${detail} The parent is still alive — send the next message below.` };
}

export const HARNESS_ERRORS: HarnessEntry[] = [
  {
    match: /tool call budget exhausted/i,
    title: "Turn failed: too many tool calls for one turn",
    cause: "The agent used up the per-turn tool call limit before it produced an answer, so the turn was discarded.",
    steps: (run, initial) => [
      retryStep(run, initial, "Ask for one action per message", "Each turn allows only a few tool calls. Split the request (“fetch X”, then “now summarise it”) so each message needs one."),
      { label: "Nothing was committed", detail: "A failed turn records no answer; work the tools already did is not replayed." },
    ],
  },
  {
    match: /final answer is empty or exceeds limit/i,
    title: "Turn failed: the answer was empty or too long",
    cause: "The agent's final answer was empty or exceeded the turn's answer size bound, so no result was committed.",
    steps: (run, initial) => [
      retryStep(run, initial, "Ask for a shorter answer", "Ask for a summary, a fixed number of bullet points, or one part at a time."),
      { label: "If answers are routinely cut", detail: "The bound comes from the conversation's output token budget; a new conversation can ask for a larger one within the profile's ceiling." },
    ],
  },
  {
    match: /context capacity exceeded|context (window|length)/i,
    title: "Turn failed: the conversation no longer fits the model's context",
    cause: "The retained conversation plus this message exceeded the harness's context capacity.",
    steps: (run) => [restartStep, newConversationStep(run, "Or start clean if the earlier exchanges are no longer needed.")],
  },
  {
    match: /cancel/i,
    title: "Turn cancelled",
    cause: "The turn was cancelled before it committed a result; the parent itself was not stopped.",
    steps: (run, initial) => [retryStep(run, initial, "Send the message again if you still need it", "A cancelled turn records no answer and is never replayed.")],
  },
];

/** The harness's own last error line, without the event JSON that precedes it. */
export function harnessError(answer: string): string {
  const marker = answer.lastIndexOf("CELLN_HARNESS_ERROR");
  return (marker >= 0 ? answer.slice(marker + "CELLN_HARNESS_ERROR".length) : answer).trim();
}

function failedTurn(run: AgentRun, turns: AgentRunTurn[]): { answer: string; initial: boolean } | null {
  const ordered = [...turns].sort((a, b) => (a.metadata.creationTimestamp || "").localeCompare(b.metadata.creationTimestamp || "") || a.metadata.name.localeCompare(b.metadata.name));
  const last = ordered[ordered.length - 1]?.status?.execution?.result;
  // Only the latest exchange matters: an older failure followed by an answer needs no explanation.
  if (last) return last.succeeded ? null : { answer: last.answer, initial: false };
  if (ordered.length) return null;
  const initial = run.status?.cellnParent?.initialTurn?.result;
  return initial && !initial.succeeded ? { answer: initial.answer, initial: true } : null;
}

function diagnoseTurn(run: AgentRun, failure: { answer: string; initial: boolean }): Diagnosis {
  const reason = harnessError(failure.answer);
  const entry = HARNESS_ERRORS.find((candidate) => candidate.match.test(reason));
  const evidence = [truncate(`${failure.initial ? "initialTurn" : "turn"}.result.answer: ${failure.answer}`)];
  if (entry) return { kind: "turn-failed", severity: "warning", code: reason.slice(0, 80), title: entry.title, cause: entry.cause, evidence, nextSteps: entry.steps(run, failure.initial) };
  return {
    kind: "turn-failed", severity: "warning", title: failure.initial ? "The first turn failed" : "The last turn failed",
    cause: reason ? `The turn ended without a committed answer: ${truncate(reason, 200)}` : "The turn ended without a committed answer.",
    evidence,
    nextSteps: [retryStep(run, failure.initial, "Rephrase and try again", "Nothing was committed, so the turn is not replayed automatically.")],
  };
}

// ── Entry point ──────────────────────────────────────────────────────────────

export function diagnoseRun(run: AgentRun, turns: AgentRunTurn[] = [], context: DiagnosisContext = {}): Diagnosis | null {
  const conditions = currentConditions(run);
  const parent = run.status?.cellnParent;
  const ready = conditions.find((condition) => condition.type === "CellnParentReady");
  const notReady = ready?.status === "False" ? ready : undefined;

  if (!parent && notReady?.reason === "AdmissionPending") return diagnoseAdmission(run, notReady, context);

  const withheld = conditions.find((condition) => condition.type === "CellnContinuation" && condition.status === "False" && condition.reason === "LostBeforeFollowUp");
  const ownerStatus = notReady?.reason && LOST_STATUSES.has(notReady.reason) ? notReady.reason
    : parent?.ownerOutcome && LOST_STATUSES.has(parent.ownerOutcome.status) ? parent.ownerOutcome.status : undefined;
  if (ownerStatus || withheld) return diagnoseLost(run, ownerStatus || "ContextLost", notReady, withheld);

  if (notReady?.reason === "ReconciliationRequired") {
    return {
      kind: "reconciliation-required", severity: "warning", code: "ReconciliationRequired", title: "The parent's outcome is unconfirmed",
      cause: "The original parent's outcome is unconfirmed: its node did not answer, so sending is paused and no work will be replayed automatically.",
      evidence: lostEvidence(run, [notReady]),
      nextSteps: [
        { label: "Give the node a moment", detail: "The controller keeps asking the owning node every few seconds; when it answers, this conversation resumes or is reported lost." },
        { label: "If it stays like this", detail: "Check that the dispatcher on the owning node is running and reachable from the controller. Do not start a replacement while the original may still be live." },
      ],
    };
  }

  if (notReady?.reason === "CreateRefused" ||parent?.ownerOutcome?.status === "CreateRefused") {
    return {
      kind: "create-refused", severity: "error", code: "CreateRefused", title: "The node refused to start this parent",
      cause: "The owner node refused this parent — it had no capacity left, or did not accept the admission — so nothing was started and this run is never retried.",
      evidence: lostEvidence(run, [notReady]),
      nextSteps: [newConversationStep(run, "A new run is placed on any node with capacity. If every node refuses, free a parent slot (end an idle conversation) or add a node.")],
    };
  }

  const failure = failedTurn(run, turns);
  if (failure) return diagnoseTurn(run, failure);

  const phase = run.status?.phase;
  if (phase !== "Failed" && phase !== "Refused") return null;
  const firstFalse = conditions.find((condition) => condition.status === "False");
  const error = run.status?.error?.trim();
  return {
    kind: "run-failed", severity: "error", title: phase === "Refused" ? "Run refused" : "Run failed",
    cause: error ? truncate(error, 240) : firstFalse?.message ? truncate(firstFalse.message, 240) : "The run ended in failure without recording a reason.",
    evidence: [error ? `status.error: ${error}` : "", firstFalse ? conditionText(firstFalse) : "", run.status?.exitCode ? `exitCode: ${run.status.exitCode}` : ""].filter(Boolean).map((text) => truncate(text)),
    nextSteps: [
      { label: "Read the recorded error", detail: "The Result tab and the evidence below hold everything the controller recorded for this run." },
      { label: "Run it again once fixed", detail: "A failed run is never retried automatically.", action: { kind: "link", label: `Open ${run.spec.agentRef}`, to: `/agents/${encodeURIComponent(run.spec.agentRef)}` } },
    ],
  };
}
