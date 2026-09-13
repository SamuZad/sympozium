import { useEffect, useState } from "react";
import { useInfiniteQuery } from "@tanstack/react-query";
import { api, type AgentRun, type AgentRunTurn } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";
import { CellnScopedExecution } from "@/components/celln-scoped-execution";
import { taskText } from "@/lib/utils";

type Pending = { name: string; requestId: string; message: string };
const terminalNativePhases = new Set(["Succeeded", "Failed", "Refused", "Cancelled"]);

function currentTurnCondition(turn: AgentRunTurn) {
  return turn.status?.conditions?.find((condition) => condition.type === "CellnTurnComplete" &&
    turn.metadata.generation !== undefined && condition.observedGeneration === turn.metadata.generation);
}

function turnOutcomeCommitted(turn: AgentRunTurn) {
  if (turn.status?.execution?.result) return true;
  const scoped = turn.status?.cellnScoped;
  const condition = currentTurnCondition(turn);
  return Boolean(scoped?.nativePhase && terminalNativePhases.has(scoped.nativePhase) && condition?.status === "True" && condition.reason === "Committed");
}

export function CellnConversation({ run, observationUnavailable = false }: { run: AgentRun; observationUnavailable?: boolean }) {
  const uid = run.metadata.uid || "";
  const namespace = run.metadata.namespace || "default";
  const storageKey = `celln-turn:${namespace}:${uid}`;
  const deleteKey = `celln-delete:${namespace}:${uid}`;
  const cancelKey = `celln-cancel:${namespace}:${uid}`;
  const [cancelledRequests, setCancelledRequests] = useState<string[]>(() => {
    try {
      const stored: unknown = JSON.parse(sessionStorage.getItem(cancelKey) || "[]");
      return Array.isArray(stored) ? stored.filter((id): id is string => typeof id === "string") : [];
    } catch { return []; }
  });
  const [deleteRequested, setDeleteRequested] = useState(() => {
    try { return sessionStorage.getItem(deleteKey) === "requested"; } catch { return false; }
  });
  const [draft, setDraft] = useState("");
  const [error, setError] = useState("");
  const [sending, setSending] = useState(false);
  const [pending, setPending] = useState<Pending | null>(() => {
    try { return JSON.parse(sessionStorage.getItem(storageKey) || "null"); } catch { return null; }
  });
  const history = useInfiniteQuery({
    queryKey: ["parent-turns", namespace, run.metadata.name, uid],
    initialPageParam: "",
    queryFn: async ({ pageParam }) => {
      const page = await api.runs.turns(run.metadata.name, namespace, pageParam);
      if (page.runUID !== uid) throw new Error("Run identity changed. Reload the run before continuing.");
      return page;
    },
    getNextPageParam: (page) => page.continue || undefined,
    enabled: Boolean(uid),
    refetchInterval: 2000,
  });
  const turns = (history.data?.pages.flatMap((page) => page.items) || []).sort((a, b) =>
    (a.metadata.creationTimestamp || "").localeCompare(b.metadata.creationTimestamp || "") || a.metadata.name.localeCompare(b.metadata.name));
  const pendingTurn = turns.find((turn) => turn.metadata.name === pending?.name);
  const completedPending = pendingTurn && turnOutcomeCommitted(pendingTurn);
  useEffect(() => {
    if (completedPending) {
      sessionStorage.removeItem(storageKey);
      setPending(null);
      setError("");
    }
  }, [completedPending, storageKey]);
  const parent = run.status?.cellnParent;
  const scopedParent = run.status?.cellnScoped;
  const currentScopedCondition = run.status?.conditions?.find((condition) => condition.type === "CellnScopedExecution" &&
    run.metadata.generation !== undefined && condition.observedGeneration === run.metadata.generation);
  const scoped = Boolean(scopedParent || (!parent && currentScopedCondition));
  const conditionType = scoped ? "CellnScopedExecution" : "CellnParentReady";
  const parentCondition = run.status?.conditions?.find((condition) => condition.type === conditionType && run.metadata.generation !== undefined && condition.observedGeneration === run.metadata.generation);
  const admissionPending = !scoped && !parent && parentCondition?.status === "False" && parentCondition.reason === "AdmissionPending";
  const deleting = deleteRequested || Boolean(run.metadata.deletionTimestamp);
  const scopedReady = Boolean(scopedParent?.startAttempted && scopedParent.parentIncarnation && scopedParent.nativePhase === "Running" &&
    parentCondition?.status === "True" && parentCondition.reason === "EnduringParentReady");
  const legacyReady = Boolean(parent && parentCondition?.status === "True");
  const ready = !observationUnavailable && !deleting && run.status?.phase === "Running" && (scoped ? scopedReady : legacyReady);
  const requestedTurns = run.spec.enduring?.maxTurns || 1;
  const ceilingReached = scoped ? turns.length >= requestedTurns - 1 : Boolean(parent && parent.acceptedTurns >= requestedTurns - 1);
  const initialFailed = parent?.initialTurn?.result?.succeeded === false;
  const scopedActiveTurn = scoped ? turns.find((turn) => !turnOutcomeCommitted(turn) && !turn.status?.cellnScoped?.cleanupConfirmed) : undefined;
  const activeTurn = Boolean(parent?.activeTurn || scopedActiveTurn);
  const unavailableReason = parentCondition?.status === "False" ? parentCondition.reason : undefined;
  const lifecycleDetail = scoped && parentCondition?.status === "Unknown"
    ? parentCondition.message || "The scoped parent's outcome is unconfirmed. Sending remains disabled while the original owner is reconciled."
    : scoped && parentCondition?.status === "False"
    ? parentCondition.message || "Scoped parent admission or execution is not confirmed. No replacement work will be submitted."
    : unavailableReason === "ContextLost"
    ? "Live harness context was lost. Recorded answers remain available, but this parent cannot resume. It will not be silently recreated."
    : unavailableReason === "Stopped"
    ? "The parent has stopped. Recorded answers remain available; this conversation cannot accept more turns."
    : unavailableReason === "TeardownUncertain"
    ? "Parent teardown is unconfirmed. Ask the operator to reconcile the original owner; do not create replacement work or assume its resources are free."
    : unavailableReason === "ReconciliationRequired"
    ? "The original parent's outcome is unconfirmed. Sending is paused while its owner is reconciled; no work will be replayed automatically."
    : ready && initialFailed
    ? "The initial turn failed. Sending is disabled; inspect the recorded failure before creating any new work."
    : ready && activeTurn
    ? "One turn is already active or awaiting reconciliation. It must have a committed result before another turn can begin."
    : ready && ceilingReached
    ? scoped
      ? "The loaded turn records reach the requested turn ceiling, including the initial turn. Sending is disabled; this is not host budget-usage telemetry."
      : "The requested turn ceiling is exhausted, including the initial turn. Refreshing this page does not restore the budget."
    : "";
  const completeHistory = Boolean(history.data) && !history.hasNextPage;
  const canCompose = ready && !initialFailed && !activeTurn && !ceilingReached && !pending && !sending && completeHistory;
  const bytes = new TextEncoder().encode(draft).length;
  const initialConfirmed = scoped ? Boolean(scopedParent?.receiptDigest && scopedParent.output) : Boolean(parent?.initialTurn?.result?.succeeded);
  const canSend = ready && initialConfirmed && !activeTurn && !pending && !sending && !history.isError && completeHistory &&
    !ceilingReached && draft.trim().length > 0 && bytes <= 2048 && !draft.includes("\0");

  async function send() {
    if (!canSend) return;
    setSending(true);
    setError("");
    try {
      const requestId = crypto.randomUUID();
      const digest = await crypto.subtle.digest("SHA-256", new TextEncoder().encode(JSON.stringify([uid, requestId])));
      const name = `turn-${Array.from(new Uint8Array(digest), (byte) => byte.toString(16).padStart(2, "0")).join("")}`;
      const saved = { name, requestId, message: draft };
      // Persist identity before POST. Network failure never clears this record
      // or triggers another submission; subsequent polling reconciles it.
      sessionStorage.setItem(storageKey, JSON.stringify(saved));
      setPending(saved);
      await api.runs.submitTurn(run.metadata.name, namespace, { runUID: uid, requestId, message: draft });
      setDraft("");
      await history.refetch();
    } catch {
      setError("Submission could not be confirmed. Checking the original request; do not resend it.");
    } finally { setSending(false); }
  }

  function cancellationPending(turn: AgentRunTurn) {
    return Boolean(turn.spec.cancelRequested || (turn.metadata.uid && cancelledRequests.includes(turn.metadata.uid)));
  }

  function canCancel(turn: AgentRunTurn) {
    const turnScoped = turn.status?.cellnScoped;
    const scopedBinding = scoped && Boolean(turn.metadata.uid && scopedParent?.parentIncarnation &&
      turn.status?.parentIncarnation === scopedParent.parentIncarnation && turnScoped?.parentIncarnation === scopedParent.parentIncarnation &&
      turnScoped.turnId === turn.metadata.uid && turnScoped.startAttempted && turnScoped.nativePhase && !terminalNativePhases.has(turnScoped.nativePhase));
    const legacyBinding = Boolean(turn.metadata.uid && parent?.activeTurn?.uid === turn.metadata.uid && parent?.activeTurn?.name === turn.metadata.name &&
      turn.status?.execution?.attempted && !turn.status.execution.result);
    return ready && !history.isError && (scopedBinding || legacyBinding) && !cancellationPending(turn);
  }

  async function cancelTurn(turn: AgentRunTurn) {
    if (!canCancel(turn) || !turn.metadata.uid || !window.confirm("Cancel this turn's sub-cell? The persistent parent is not stopped. Wait for a committed result before sending another turn.")) return;
    try {
      const requests = [...cancelledRequests, turn.metadata.uid];
      // Keep the exact UID before POST so a refresh or lost response cannot
      // silently resend or target a new turn occupying the same parent slot.
      sessionStorage.setItem(cancelKey, JSON.stringify(requests));
      setCancelledRequests(requests);
      await api.runs.cancelTurn(run.metadata.name, namespace, turn.metadata.name, { runUID: uid, turnUID: turn.metadata.uid });
      await history.refetch();
    } catch {
      setError("Cancellation could not be confirmed. Checking the original turn; the request will not be resent automatically. Child teardown is not confirmed.");
    }
  }

  async function deleteRun() {
    if (!uid || deleting || !window.confirm("Delete this enduring run and its Kubernetes turn history? This requests parent/child teardown, not a pause. Live context cannot be resumed afterward.")) return;
    try {
      // Persist the intent before DELETE; uncertainty must not re-enable work
      // after refresh. The UID precondition protects against reused names.
      sessionStorage.setItem(deleteKey, "requested");
      setDeleteRequested(true);
      await api.runs.deleteEnduring(run.metadata.name, namespace, uid);
    } catch {
      setError("Deletion could not be confirmed. Reconcile this run's original UID; do not assume its parent has stopped.");
    }
  }

  const initial = parent?.initialTurn;
  return <Card data-testid="celln-conversation">
    <CardHeader><CardTitle>{scoped ? "Enduring scoped Celln conversation" : "Persistent Celln conversation"}</CardTitle></CardHeader>
    <CardContent className="space-y-4">
      <p className="text-sm text-muted-foreground">This is an enduring parent, not a one-shot cell. The parent retains live context and each follow-up runs in a disposable child cell. A recorded answer does not mean the parent is still available.</p>
      <p role="status">{ready ? "Parent initialized" : admissionPending ? "Waiting for parent admission — sending disabled" : "Parent unavailable or starting — sending disabled"}</p>
      {observationUnavailable && <p role="alert">Run status could not be refreshed. Recorded history is shown, but sending is disabled until the current run can be checked.</p>}
      {deleting && <p role="status" data-testid="celln-delete-pending">Deletion requested. Sending is disabled while the controller reconciles teardown. Acceptance of deletion is not confirmation that the parent has stopped.</p>}
      {lifecycleDetail && <p role="status" data-testid="celln-parent-lifecycle-detail">{lifecycleDetail}</p>}
      <p className="text-sm text-muted-foreground" data-testid="celln-parent-turn-limit">Requested ceiling: {requestedTurns} total turns, including the initial turn. The host may enforce stricter limits; this is not a guarantee of remaining capacity.</p>
      {admissionPending && <p className="text-sm" data-testid="celln-parent-admission">{parentCondition.message}</p>}
      {initial && <div className="space-y-2 rounded border p-3"><p className="whitespace-pre-wrap">You: {initial.message}</p><p className="whitespace-pre-wrap">{initial.result ? `${initial.result.succeeded ? "Agent" : "Initial turn failed"}: ${initial.result.answer}` : initial.attempted ? "Initial turn awaiting reconciliation" : "Initial turn queued"}</p></div>}
      {scopedParent && <>
        <div className="space-y-2 rounded border p-3">
          <p className="whitespace-pre-wrap">You: {taskText(run.spec.task)}</p>
          <p className="whitespace-pre-wrap">{scopedParent.output ? `Agent: ${scopedParent.output}` : scopedParent.startAttempted ? "Initial turn submitted — awaiting correlated output and receipt" : "Initial turn preparing"}</p>
        </div>
        <CellnScopedExecution status={scopedParent} condition={parentCondition} mode="enduring" label="Parent native execution" />
      </>}
      {turns.map((turn) => {
        const turnScoped = turn.status?.cellnScoped;
        const turnCondition = currentTurnCondition(turn);
        const scopedCommitted = Boolean(turnScoped?.nativePhase && terminalNativePhases.has(turnScoped.nativePhase) && turnCondition?.status === "True" && turnCondition.reason === "Committed");
        const turnComplete = turnOutcomeCommitted(turn);
        const turnText = turn.status?.execution?.result
          ? `${turn.status.execution.result.succeeded ? "Agent" : "Turn failed"}: ${turn.status.execution.result.answer}`
          : scopedCommitted && turnScoped?.nativePhase === "Succeeded" && turnScoped.output
          ? `Agent: ${turnScoped.output}`
          : scopedCommitted
          ? `Turn ${turnScoped?.nativePhase || "completed"}${turnScoped?.output ? `: ${turnScoped.output}` : ""}`
          : turnScoped?.startAttempted
          ? `Native phase: ${turnScoped.nativePhase || "start attempted; observation pending"}`
          : turn.status?.execution?.attempted ? "Submitted — awaiting committed result" : "Queued";
        const attempted = Boolean(turn.status?.execution?.attempted || turnScoped?.startAttempted);
        return <div key={turn.metadata.uid || turn.metadata.name} className="space-y-2 rounded border p-3">
        <p className="whitespace-pre-wrap">You: {turn.spec.message}</p>
        <p className="whitespace-pre-wrap">{turnText}</p>
        {!turnComplete && cancellationPending(turn) && <p role="status" data-testid="celln-turn-cancel-pending">Cancellation requested for this turn only. Waiting for the original parent's committed result; child teardown is not confirmed.</p>}
        {!turnComplete && attempted && <Button variant="outline" data-testid="celln-turn-cancel" disabled={!canCancel(turn)} onClick={() => cancelTurn(turn)}>Cancel turn</Button>}
        {turnCondition?.status !== "True" && turnCondition?.reason === "ReconciliationRequired" && <p role="status" data-testid="celln-turn-reconciliation">Turn admission or outcome is unconfirmed. The original request is retained; do not resubmit it. Ask the operator to reconcile this turn.</p>}
        {turnScoped && <CellnScopedExecution status={turnScoped} condition={turnCondition} mode="turn" label="Turn native execution" />}
      </div>})}
      {history.isError && <p role="alert">Turn history unavailable. Sending is disabled until history can be checked.</p>}
      {history.hasNextPage && <><p className="text-sm text-muted-foreground">Load the complete turn history before sending so this page can verify no prior turn is unresolved. This does not claim host budget usage.</p><Button variant="outline" disabled={history.isFetchingNextPage} onClick={() => history.fetchNextPage()}>Load more turns</Button></>}
      {pending && <p role="status">{pendingTurn ? "Waiting for the saved turn to complete." : `Checking unconfirmed request ${pending.requestId}. It will not be resubmitted automatically.`}</p>}
      {error && <p role="alert">{error}</p>}
      <label className="block space-y-2">Next message
        <textarea data-testid="celln-turn-message" className="min-h-24 w-full rounded border bg-background p-2" value={draft} onChange={(event) => setDraft(event.target.value)} disabled={!canCompose} />
      </label>
      <p className="text-sm text-muted-foreground">{bytes}/2048 UTF-8 bytes</p>
      <p className="text-sm text-muted-foreground">Retained conversation context is also bounded by the selected harness. A message below this input limit may still exceed its remaining context capacity.</p>
      <Button data-testid="celln-turn-send" disabled={!canSend} onClick={send}>Send turn</Button>
      <div className="border-t pt-4">
        <p className="text-sm text-muted-foreground">Deletion stops this parent and removes the Kubernetes run/turn history after cleanup. It is not pause/resume; privately retained host audit may remain.</p>
        <Button variant="destructive" data-testid="celln-delete-run" disabled={!uid || deleting || sending} onClick={deleteRun}>Delete run and stop parent</Button>
      </div>
    </CardContent>
  </Card>;
}
