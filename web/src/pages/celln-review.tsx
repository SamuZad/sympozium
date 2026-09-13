import { useEffect, useState } from "react";
import { taskText } from "@/lib/utils";
import { api, getToken, type AgentRun } from "@/lib/api";
import { useAuth } from "@/components/auth-provider";
import { CellnConversation } from "@/components/celln-conversation";
import { CellnScopedExecution } from "@/components/celln-scoped-execution";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

async function request<T>(path: string, body?: unknown, method = "POST"): Promise<T> {
  const response = await fetch(path, { method: body === undefined ? "GET" : method, headers: { Authorization: `Bearer ${getToken()}`, "Content-Type": "application/json" }, ...(body === undefined ? {} : { body: JSON.stringify(body) }) });
  if (!response.ok) throw new Error((await response.text()).slice(0, 512));
  return response.status === 204 ? undefined as T : response.json();
}

type Profile = { mode: string; provider: string; model: string; namespace: string; timeout: string; enduring?: AgentRun["spec"]["enduring"]; tools: { name: string; revision: string }[] };

export function CellnReviewPage() {
  const { logout } = useAuth();
  const [runs, setRuns] = useState<AgentRun[]>([]);
  const [selected, setSelected] = useState("");
  const [task, setTask] = useState("Tell me where Cairo is.");
  const [profiles, setProfiles] = useState<Profile[]>([]);
  const [mode, setMode] = useState("enduring");
  const [error, setError] = useState("");
  const [offline, setOffline] = useState(false);
  const [busy, setBusy] = useState(false);
  const [probe, setProbe] = useState("");
  const [pending, setPending] = useState<{ mode: string; task: string; requestId: string } | null>(() => {
    try { return JSON.parse(sessionStorage.getItem("celln-review-create") || "null"); } catch { return null; }
  });
  async function refresh() {
    try {
      const result = await request<{ items: AgentRun[] }>("/api/v1/review/runs");
      setRuns(result.items.sort((a, b) => (b.metadata.creationTimestamp || "").localeCompare(a.metadata.creationTimestamp || "")));
      setOffline(false);
    } catch { setOffline(true); }
  }
  useEffect(() => { void refresh(); const timer = setInterval(() => void refresh(), 2000); return () => clearInterval(timer); }, []);
  useEffect(() => { request<{ profiles: Profile[] }>("/api/v1/review/config").then((config) => setProfiles(config.profiles)).catch(() => setError("Approved execution profiles unavailable; new execution is disabled.")); }, []);
  const profile = profiles.find((item) => item.mode === mode);
  const run = runs.find((value) => value.metadata.uid === selected) || runs[0];
  const scope = run?.status?.cellnScoped;
  const enduring = run?.spec.executionLifecycle === "enduring";
  const neverStarted = Boolean(!scope && run?.status?.conditions?.some((condition) => condition.type === "CellnScopedExecution" && condition.status === "False" && ["AdmissionRefused", "CancelledBeforeAdmission"].includes(condition.reason || "")));
  const stopped = Boolean(run?.metadata.deletionTimestamp && (scope?.cleanupConfirmed || neverStarted));
  async function create() {
    setBusy(true); setError(""); setProbe("");
    const input = pending || { mode, task, requestId: crypto.randomUUID().replace(/-/g, "") };
    // Retain the exact request before networking. A lost response never creates
    // another incarnation; retry explicitly reuses the original identity.
    sessionStorage.setItem("celln-review-create", JSON.stringify(input)); setPending(input);
    try {
      const created = await request<AgentRun>("/api/v1/review/runs", input);
      setSelected(created.metadata.uid || ""); setPending(null); sessionStorage.removeItem("celln-review-create"); await refresh();
    } catch (error) { setError(`Creation not confirmed. Retry checks the same request, not a new run: ${String(error)}`); }
    finally { setBusy(false); }
  }
  async function stop() {
    if (!run?.metadata.uid || !window.confirm("Stop this exact run and its descendants? Its evidence record will remain until cleanup is confirmed.")) return;
    setError("");
    try { await api.runs.deleteEnduring(run.metadata.name, run.metadata.namespace!, run.metadata.uid); await refresh(); }
    catch (error) { setError(`Stop unconfirmed; inspect the original run, do not infer teardown: ${String(error)}`); }
  }
  async function archive() {
    if (!run?.metadata.uid || !stopped || !window.confirm("Remove this confirmed-stopped review record? Download its evidence first if needed.")) return;
    try { await request(`/api/v1/runs/${run.metadata.name}/archive?namespace=${run.metadata.namespace}`, { uid: run.metadata.uid }); await refresh(); }
    catch (error) { setError(String(error)); }
  }
  async function checkLimit() {
    if (!run?.metadata.uid) return;
    setProbe("");
    try {
      const history = await api.runs.turns(run.metadata.name, run.metadata.namespace!);
      if (history.runUID !== run.metadata.uid || history.continue || history.items.length < (run.spec.enduring?.maxTurns || 1) - 1) {
        setProbe("Complete the allowed continuation first. No probe was submitted."); return;
      }
      await api.runs.submitTurn(run.metadata.name, run.metadata.namespace!, { runUID: run.metadata.uid, requestId: `review-limit-${run.metadata.uid}`, message: "Check the original turn ceiling without granting additional authority." });
      setProbe("A turn record was accepted; inspect its actual outcome. Do not assume refusal or cleanup.");
    } catch (error) { setProbe(`API response (not gateway usage telemetry): ${String(error)}`); }
  }
  function download() {
    if (!run) return;
    const url = URL.createObjectURL(new Blob([JSON.stringify(run, null, 2)], { type: "application/json" }));
    const a = document.createElement("a"); a.href = url; a.download = `${run.metadata.name}.json`; a.click(); URL.revokeObjectURL(url);
  }
  return <main className="mx-auto max-w-6xl space-y-6 p-6">
    <header className="flex justify-between gap-4"><div><h1 className="text-2xl font-bold">Celln workspace</h1><p className="text-muted-foreground">Ask, use native tools, and continue conversations in isolated KVM cells.</p></div><Button variant="outline" onClick={logout}>Log out</Button></header>
    <p className="rounded border border-amber-500 p-3 text-sm">MVP preview on framework. Model credentials stay in the gateway, outside the UI and native cells. Each conversation has fixed limits; refreshing never resets its budget. Production security/recovery acceptance remains outstanding.</p>
    {offline && <p role="alert">Observation unavailable. Last-known evidence is shown; execution and cleanup are not inferred.</p>}
    {error && <p role="alert" className="text-red-400">{error}</p>}
    <Card><CardHeader><CardTitle>Start an execution</CardTitle></CardHeader><CardContent className="space-y-3">
      <label className="block">Execution <select data-testid="review-mode" className="ml-3 rounded border bg-background p-2" value={mode} disabled={!!pending || busy} onChange={(e) => { setMode(e.target.value); setTask(e.target.value === "direct" ? "celln" : "Tell me where Cairo is."); }}><option value="direct">Direct tool — no model</option><option value="model">Single answer</option><option value="enduring">Conversation</option></select></label>
      <p className="text-sm" data-testid="workspace-model">{mode === "direct" ? "No model — native tool execution" : `Model: ${profile?.model || "Loading…"} · Provider: ${profile?.provider || "Loading…"}`}</p>
      {mode !== "direct" && <p className="text-sm" data-testid="workspace-tools">Available tool: uppercase. Tool calls are optional; ask factual questions directly or explicitly request the tool. The signed operator bundle fixes the available tool set.</p>}
      <label className="block">Your message<Input data-testid="review-task" value={task} disabled={!!pending || busy} maxLength={2048} onChange={(e) => setTask(e.target.value)} /></label>
      <p className="text-sm text-muted-foreground">{profile?.enduring ? `${profile.enduring.maxTurns} total turns · ${profile.enduring.maxModelRequests} model requests · ${profile.enduring.maxOutputTokens} output-token allowance · ${profile.enduring.leaseSeconds}s parent lifetime. Initial turn counts toward the ceiling.` : `One bounded execution; deadline ${profile?.timeout || "loading"}.`} These are configured ceilings, not remaining-usage telemetry. {profile?.model === "review-uppercase" && "Warning: this profile is a deterministic fixture, not a real LLM."}</p>
      <Button data-testid="review-create" onClick={() => void create()} disabled={busy || offline || !profile || !task.trim()}>{busy ? "Submitting…" : pending ? "Check / retry original request" : "Start fresh run"}</Button>
    </CardContent></Card>
    <section className="grid gap-5 md:grid-cols-[260px_1fr]">
      <aside className="space-y-2"><h2 className="font-semibold">Execution history</h2>{runs.length === 0 && <p>No executions yet.</p>}{runs.map((value) => <button key={value.metadata.uid} data-testid="review-history" className={`block w-full break-all rounded border p-3 text-left text-xs ${run?.metadata.uid === value.metadata.uid ? "border-blue-500" : ""}`} onClick={() => { setSelected(value.metadata.uid || ""); setProbe(""); }}><strong>{taskText(value.spec.task).slice(0, 60)}</strong><br /><span className="text-muted-foreground">{value.metadata.name}</span><br />{value.metadata.deletionTimestamp ? value.status?.cellnScoped?.cleanupConfirmed ? "Stopped — cleanup confirmed" : "Stopping — cleanup unconfirmed" : value.status?.phase || "Pending"}</button>)}</aside>
      {run && <div className="min-w-0 space-y-4" data-testid="review-selected"><h2 className="break-all text-lg font-semibold">{run.metadata.name}</h2><p className="break-all font-mono text-xs">Namespace: {run.metadata.namespace} · UID: {run.metadata.uid}</p>
        <div className="flex flex-wrap gap-2"><Button variant="outline" onClick={download}>Download root evidence</Button><Button data-testid="review-stop" variant="destructive" onClick={() => void stop()} disabled={offline || !!run.metadata.deletionTimestamp}>Stop and retain evidence</Button>{stopped && <Button data-testid="review-archive" variant="outline" onClick={() => void archive()} disabled={offline}>Remove stopped record</Button>}</div>
        {run.status?.error && <p role="alert" data-testid="workspace-run-error" className="rounded border border-red-500 p-3">{run.status.error}</p>}
        <p className="text-sm text-muted-foreground">Model: {run.spec.model?.model || "None — direct tool"}</p>
        {run.metadata.deletionTimestamp && <p data-testid="review-stop-status" role="status">{stopped ? neverStarted ? "Cancelled before admission — no native execution started. This is not a VM teardown receipt." : "Stopped — native cleanup confirmed. This UI-owned evidence hold retains the record; no work can resume." : "Stopping — cleanup is not yet confirmed. The recovery record is retained."}</p>}
        {scope && !enduring && <CellnScopedExecution compact status={scope} mode={enduring ? "enduring" : "one-shot"} condition={run.status?.conditions?.find((c) => c.type === "CellnScopedExecution")} />}
        {scope?.output && <Card><CardHeader><CardTitle>Initial result</CardTitle></CardHeader><CardContent><pre data-testid="review-result" className="whitespace-pre-wrap break-words">{scope.output}</pre></CardContent></Card>}
        {enduring && <><CellnConversation key={run.metadata.uid} run={run} observationUnavailable={offline} retainEvidence compactEvidence /><Button data-testid="review-limit" variant="outline" disabled={offline || !!run.metadata.deletionTimestamp || scope?.nativePhase !== "Running"} onClick={() => void checkLimit()}>Check API refusal after the allowed continuation</Button>{probe && <p data-testid="review-limit-result" role="status">{probe}</p>}</>}
        <details><summary>Raw public run status and configured limits</summary><pre className="overflow-auto text-xs">{JSON.stringify({ spec: run.spec, status: run.status }, null, 2)}</pre></details>
      </div>}
    </section>
  </main>;
}
