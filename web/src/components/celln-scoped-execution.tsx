import type { CellnScopedStatus, Condition } from "@/lib/api";

function EvidenceValue({ children }: { children?: string }) {
  return <p className="break-all font-mono text-xs">{children || "Not reported"}</p>;
}

export function CellnScopedExecution({
  status,
  condition,
  mode,
  label = "Native execution",
}: {
  status: CellnScopedStatus;
  condition?: Condition;
  mode: "one-shot" | "enduring" | "turn";
  label?: string;
}) {
  const cleanup = status.cleanupConfirmed
    ? "Confirmed by the scoped cleanup record"
    : status.nativePhase && !["Succeeded", "Failed", "Refused", "Cancelled"].includes(status.nativePhase)
    ? "Not confirmed — the native execution is still active or awaiting an outcome"
    : "Not confirmed — a terminal phase alone does not prove teardown";
  const confirmation = condition
    ? `${condition.status} · ${condition.reason || "No reason reported"}${condition.message ? ` — ${condition.message}` : ""}`
    : "No current controller confirmation recorded";

  return <div className="space-y-3 rounded border p-3 text-sm" data-testid="celln-scoped-execution">
    <div className="flex flex-wrap items-center justify-between gap-2">
      <p className="font-medium">{label}</p>
      <span className="rounded bg-muted px-2 py-1 text-xs font-medium" data-testid="celln-scoped-mode">
        {mode === "one-shot" ? "One-shot scoped cell" : mode === "enduring" ? "Enduring scoped parent" : "Scoped child turn"}
      </span>
    </div>
    <dl className="grid gap-3 sm:grid-cols-2">
      <div><dt className="text-muted-foreground">Native phase</dt><dd><EvidenceValue>{status.nativePhase}</EvidenceValue></dd></div>
      <div><dt className="text-muted-foreground">Receipt digest</dt><dd><EvidenceValue>{status.receiptDigest}</EvidenceValue></dd></div>
      <div><dt className="text-muted-foreground">Receiver</dt><dd><EvidenceValue>{status.receiverId}</EvidenceValue></dd></div>
      <div><dt className="text-muted-foreground">Receiver owner</dt><dd><EvidenceValue>{status.owner}</EvidenceValue></dd></div>
      {mode !== "one-shot" && <div className="sm:col-span-2"><dt className="text-muted-foreground">Parent incarnation</dt><dd><EvidenceValue>{status.parentIncarnation}</EvidenceValue></dd></div>}
      <div><dt className="text-muted-foreground">Native parent ID</dt><dd><EvidenceValue>{status.parentId}</EvidenceValue></dd></div>
      <div><dt className="text-muted-foreground">Native child ID</dt><dd><EvidenceValue>{status.childId}</EvidenceValue></dd></div>
      <div><dt className="text-muted-foreground">Native cell ID</dt><dd><EvidenceValue>{status.cellId}</EvidenceValue></dd></div>
      {mode === "turn" && <div><dt className="text-muted-foreground">Turn ID</dt><dd><EvidenceValue>{status.turnId}</EvidenceValue></dd></div>}
    </dl>
    <div className="space-y-1">
      <p data-testid="celln-scoped-confirmation"><span className="text-muted-foreground">Controller confirmation:</span> {confirmation}</p>
      <p data-testid="celln-scoped-cleanup"><span className="text-muted-foreground">Cleanup:</span> {cleanup}</p>
      {status.gatewayRegistrationAttempted && <p><span className="text-muted-foreground">Gateway registration:</span> {status.gatewayRegistered ? "Confirmed" : "Attempted; confirmation unavailable"}</p>}
      {(status.executionProvenance || status.substrateProvenance) && <p className="text-xs text-muted-foreground">Native execution provenance is recorded. Raw provenance is not rendered because it may contain operational details.</p>}
    </div>
  </div>;
}
