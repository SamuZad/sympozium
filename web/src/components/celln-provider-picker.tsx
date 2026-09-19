import { useEffect, useRef, useState, type ComponentType } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Bot, Check, Loader2 } from "lucide-react";
import type { CellnPlatformProfile } from "@/lib/api";
import { useAddCellnFleetBackend, useCellnFleetBackends, useCellnPlatformProfiles } from "@/hooks/use-api";
import { backendLabel } from "@/components/celln-backend-picker";
import { CellnModelParametersField, CellnModelParametersSummary } from "@/components/celln-model-parameters";
import { parseModelParameters } from "@/lib/model-parameters";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

export interface ProviderChoice {
  value: string;
  label: string;
  defaultModel: string;
  icon: ComponentType<{ className?: string }>;
}

// Providers the fleet can add without an installer run (cellninstall.FleetModel).
// Anything else goes through "custom" with an explicit endpoint and protocol.
const FLEET_PRESETS = [
  { value: "deepseek", label: "DeepSeek", model: "deepseek-chat" },
  { value: "openai", label: "OpenAI", model: "gpt-4o-mini" },
  { value: "anthropic", label: "Anthropic", model: "claude-sonnet-5" },
  { value: "llama-server", label: "llama-server", model: "" },
  { value: "custom", label: "Custom", model: "" },
];
const PRESET_VALUES = new Set(FLEET_PRESETS.map((p) => p.value));

// The server reports an added backend's progress as a state string; only the
// prefixes (pending, configuring, error) and the exact "ready" are contract.
const STAGES = ["Recorded", "Nodes configuring", "Publishing to namespaces"];
const GRACE_SECONDS = 90;

/** How many of STAGES are complete for a backend state. */
function stagesDone(state: string): number {
  if (state === "ready") return 3;
  if (state.startsWith("configuring")) return 2;
  return 1;
}

function uniqueName(base: string, taken: Set<string>): string {
  if (!taken.has(base)) return base;
  for (let i = 2; ; i++) if (!taken.has(`${base}-${i}`)) return `${base}-${i}`;
}

/**
 * The Provider step for a Celln parent on the shared platform. Providers are
 * offered exactly as on the Kubernetes plane; each is served by a fleet model
 * backend. Choosing one with a ready backend binds the Agent to it; choosing
 * one without adds a backend to the fleet (the key goes to the fleet, never to
 * this namespace) and binds to it once the nodes have configured it.
 */
export function CellnProviderPicker({
  providers,
  provider,
  selected,
  onProvider,
  onProfile,
}: {
  providers: ProviderChoice[];
  provider: string;
  selected?: CellnPlatformProfile;
  onProvider: (provider: string) => void;
  onProfile: (profile: CellnPlatformProfile) => void;
}) {
  const qc = useQueryClient();
  const profiles = useCellnPlatformProfiles();
  const backends = useCellnFleetBackends();
  const add = useAddCellnFleetBackend();
  const list = profiles.data || [];
  const backendList = backends.data || [];

  // Fleet backends may use any provider identifier; show those too.
  const extra = [...new Set(list.map((p) => p.provider).filter((value) => !PRESET_VALUES.has(value)))];
  const options = [
    ...FLEET_PRESETS.map((preset) => ({ ...preset, icon: providers.find((p) => p.value === preset.value)?.icon || Bot })),
    ...extra.map((value) => ({ value, label: value, model: "", icon: providers.find((p) => p.value === value)?.icon || Bot })),
  ];
  const preset = FLEET_PRESETS.find((p) => p.value === provider);
  const matching = list.filter((p) => p.provider === provider);
  // Backends seen in progress while this step is open, by name, with when each
  // state prefix was first seen (the server does not report timestamps).
  const seen = useRef(new Map<string, number>());
  const [added, setAdded] = useState<string[]>([]);
  const [now, setNow] = useState(() => Date.now());
  const hasProfile = (b: { profile: string }) => list.some((p) => p.name === b.profile);
  // In progress: not ready yet, or ready but its profile has not loaded here
  // (only for a backend watched from this step, so the add form does not flash).
  const pending = backendList.filter(
    (b) => (b.provider === provider || added.includes(b.name)) && (b.state !== "ready" || (seen.current.has(`${b.name}:pending`) && !hasProfile(b))),
  );
  for (const b of pending) {
    const phase = b.state.startsWith("error") ? "error" : b.state.startsWith("configuring") ? "configuring" : b.state === "ready" ? "ready" : "pending";
    if (!seen.current.has(`${b.name}:pending`)) seen.current.set(`${b.name}:pending`, Date.now());
    if (!seen.current.has(`${b.name}:${phase}`)) seen.current.set(`${b.name}:${phase}`, Date.now());
  }
  const readyWatched = pending.some((b) => b.state === "ready");
  const waiting = pending.some((b) => !b.state.startsWith("error"));
  useEffect(() => {
    if (!waiting) return;
    const timer = setInterval(() => setNow(Date.now()), 1000);
    return () => clearInterval(timer);
  }, [waiting]);

  const [name, setName] = useState("");
  const [model, setModel] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [protocol, setProtocol] = useState("openai-chat");
  const [customProvider, setCustomProvider] = useState("");
  const [credential, setCredential] = useState("");
  const [skipProbe, setSkipProbe] = useState(false);
  const [parameters, setParameters] = useState("");
  const [error, setError] = useState("");

  // A backend the nodes finished configuring gets its profile; refresh so it
  // becomes selectable without leaving the step.
  const readyWithoutProfile = backendList.some((b) => b.state === "ready" && !list.some((p) => p.name === b.profile));
  useEffect(() => {
    if (!readyWithoutProfile) return;
    qc.invalidateQueries({ queryKey: ["celln-platform-profiles"] });
    if (!readyWatched) return;
    // The profile of a backend watched here may trail its "ready" state; keep
    // asking until it shows.
    const timer = setInterval(() => qc.invalidateQueries({ queryKey: ["celln-platform-profiles"] }), 3000);
    return () => clearInterval(timer);
  }, [readyWithoutProfile, readyWatched, qc]);

  // A backend added from this step binds as soon as it is ready, also when its
  // provider identifier differs from the choice made here (Custom).
  useEffect(() => {
    const name = added.find((candidate) => backendList.some((b) => b.name === candidate && b.state === "ready" && list.some((p) => p.name === b.profile)));
    if (!name) return;
    const profile = list.find((p) => p.name === backendList.find((b) => b.name === name)?.profile);
    setAdded((current) => current.filter((candidate) => candidate !== name));
    if (profile && selected?.name !== profile.name) onProfile(profile);
  }, [added, backendList, list, selected, onProfile]);

  // Bind to the provider's backend as soon as one exists.
  useEffect(() => {
    if (matching.length > 0 && selected?.provider !== provider) onProfile(matching[0]);
  }, [matching, selected, provider, onProfile]);

  function pick(value: string) {
    setError("");
    const chosen = FLEET_PRESETS.find((p) => p.value === value);
    setModel(chosen?.model || "");
    setName(uniqueName(value, new Set(backendList.map((b) => b.name))));
    setEndpoint("");
    setCredential("");
    setSkipProbe(false);
    setParameters("");
    add.reset();
    const served = list.filter((p) => p.provider === value);
    if (served.length > 0) onProfile(served[0]);
    else onProvider(value);
  }

  function submit() {
    setError("");
    const custom = provider === "custom";
    const trimmedEndpoint = endpoint.trim();
    const parsedParameters = parseModelParameters(parameters);
    const body = {
      name: name.trim(),
      provider: custom ? customProvider.trim() || "custom" : provider,
      model: model.trim() || undefined,
      endpoint: trimmedEndpoint || undefined,
      protocol: custom ? protocol : undefined,
      allowInsecure: trimmedEndpoint.toLowerCase().startsWith("http://") || undefined,
      credential: credential || undefined,
      skipPreflight: skipProbe || undefined,
      parameters: parsedParameters.parameters,
    };
    // The precise rule is shown under the JSON field.
    if (parsedParameters.error) return setError("Fix the model parameters first.");
    if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(body.name)) return setError("Name the fleet backend with a DNS label, e.g. claude.");
    if (provider !== "llama-server" && !credential) return setError(`${preset?.label || provider} needs an API key.`);
    if ((provider === "llama-server" || custom) && !body.endpoint) return setError("Give the server's address, e.g. http://framework:8080.");
    if (!body.model && (!detectable || skipProbe)) return setError(skipProbe ? "Give the model name when the probe is skipped." : "Give the model name.");
    add.mutate(body, { onSuccess: () => { setCredential(""); setParameters(""); setAdded((current) => [...current, body.name]); } });
  }

  const needsEndpoint = provider === "llama-server" || provider === "custom";
  // An OpenAI-compatible server lists its models, so the API can detect one.
  const detectable = provider === "llama-server" || (provider === "custom" && protocol === "openai-chat");
  const current = selected && selected.provider === provider ? selected : undefined;
  const currentParameters = current ? backendList.find((b) => b.profile === current.name)?.parameters : undefined;

  return (
    <div className="space-y-4" data-testid="platform-model-route">
      <div className="space-y-2">
        <Label>AI Provider</Label>
        <Select value={options.some((o) => o.value === provider) ? provider : ""} onValueChange={pick}>
          <SelectTrigger><SelectValue placeholder="Select an AI provider…" /></SelectTrigger>
          <SelectContent>
            {options.map((o) => {
              const count = list.filter((p) => p.provider === o.value).length;
              return (
                <SelectItem key={o.value} value={o.value}>
                  <span className="flex items-center gap-2">
                    <o.icon className="h-4 w-4 shrink-0" />
                    {o.label}
                    <span className="text-xs text-muted-foreground">{count > 0 ? `(${count} fleet backend${count === 1 ? "" : "s"})` : "(add a fleet backend)"}</span>
                  </span>
                </SelectItem>
              );
            })}
          </SelectContent>
        </Select>
      </div>

      {profiles.isLoading && <p className="text-xs text-muted-foreground">Loading fleet backends…</p>}

      {matching.length > 1 && (
        <div className="space-y-2">
          <Label>Fleet backend</Label>
          <Select value={current?.name || ""} onValueChange={(value) => { const profile = matching.find((p) => p.name === value); if (profile) onProfile(profile); }}>
            <SelectTrigger><SelectValue placeholder="Choose a fleet backend" /></SelectTrigger>
            <SelectContent>{matching.map((p) => <SelectItem key={p.name} value={p.name}>{backendLabel(p)}</SelectItem>)}</SelectContent>
          </Select>
        </div>
      )}

      {current && (
        <div className="space-y-1 rounded-md border p-3 text-xs" data-testid="fleet-backend-model">
          <p className="text-sm">Model: <span className="font-mono">{current.model}</span> — fixed by fleet backend <span className="font-medium">{current.backend}</span>
            {currentParameters && <> · <CellnModelParametersSummary parameters={currentParameters} testId="fleet-backend-parameters" /></>}
          </p>
          <p className="text-muted-foreground">
            Served at {current.endpoint} with the key the fleet holds for it. No key or model is entered here; to use another model, add a fleet backend for it. The fleet policy caps the ceilings.
          </p>
        </div>
      )}

      {provider && pending.length > 0 && (matching.length === 0 || pending.some((b) => added.includes(b.name))) && (
        <div role="status" className="space-y-3 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-xs" data-testid="fleet-backend-progress">
          {pending.map((b) => {
            const failed = b.state.startsWith("error");
            const done = stagesDone(b.state);
            const configuring = b.state.startsWith("configuring");
            const since = seen.current.get(`${b.name}:${configuring ? "configuring" : "pending"}`) || now;
            const elapsed = Math.max(0, Math.round((now - since) / 1000));
            return (
              <div key={b.name} className="space-y-1.5">
                <p><span className="font-medium">{b.name}</span> — {b.model}</p>
                {failed ? (
                  <p className="whitespace-pre-wrap break-words text-red-500">{b.state}</p>
                ) : (
                  <>
                    <ol className="flex flex-wrap items-center gap-x-3 gap-y-1">
                      {STAGES.map((stage, i) => (
                        <li key={stage} data-stage={i < done ? "done" : i === done ? "active" : "todo"} className={i <= done ? "flex items-center gap-1 text-foreground" : "flex items-center gap-1 text-muted-foreground"}>
                          {i < done ? <Check className="h-3 w-3 text-emerald-500" /> : i === done ? <Loader2 className="h-3 w-3 animate-spin" /> : <span className="inline-block h-3 w-3 rounded-full border" />}
                          {stage}
                        </li>
                      ))}
                    </ol>
                    {done === 1 && <p className="text-muted-foreground">Waiting for every fleet node to configure it ({elapsed}s). This usually takes under a minute.</p>}
                    {done === 2 && (
                      <p className="text-muted-foreground">
                        Waiting about {GRACE_SECONDS} seconds for the {b.provider === "llama-server" ? "configuration" : "key"} to reach the running dispatchers, then publishing the fleet backend to every namespace ({elapsed}s of about {GRACE_SECONDS}s). Nothing is stuck.
                      </p>
                    )}
                    {done === 3 && <p className="text-muted-foreground">Ready; loading it…</p>}
                  </>
                )}
              </div>
            );
          })}
          <p className="text-muted-foreground">This step continues on its own once the fleet backend is ready. Running conversations are not restarted.</p>
        </div>
      )}

      {provider && !profiles.isLoading && matching.length === 0 && pending.every((b) => b.state.startsWith("error")) && (
        <div className="space-y-3 rounded-md border p-3" data-testid="celln-provider-add-backend">
          <p className="text-xs text-muted-foreground">
            The fleet has no fleet backend for {preset?.label || provider} yet. Add one: the {needsEndpoint ? "endpoint is probed" : "key is probed once and published to the fleet"}, every node configures it, and every namespace is offered it.
          </p>
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="space-y-1 text-sm"><span>Fleet backend name</span><Input value={name} onChange={(e) => setName(e.target.value)} placeholder={provider} /></label>
            <label className="space-y-1 text-sm"><span>Model</span><Input value={model} onChange={(e) => setModel(e.target.value)} placeholder={preset?.model || (detectable ? "detected from the server" : "the model the server serves")} /></label>
            {provider === "custom" && (
              <>
                <label className="space-y-1 text-sm"><span>AI provider id</span><Input value={customProvider} onChange={(e) => setCustomProvider(e.target.value)} placeholder="custom" /></label>
                <label className="space-y-1 text-sm">
                  <span>Protocol</span>
                  <Select value={protocol} onValueChange={setProtocol}>
                    <SelectTrigger><SelectValue /></SelectTrigger>
                    <SelectContent>
                      <SelectItem value="openai-chat">OpenAI chat completions</SelectItem>
                      <SelectItem value="anthropic-messages">Anthropic messages</SelectItem>
                    </SelectContent>
                  </Select>
                </label>
              </>
            )}
            {needsEndpoint && (
              <label className="space-y-1 text-sm sm:col-span-2"><span>Chat endpoint</span><Input value={endpoint} onChange={(e) => setEndpoint(e.target.value)} placeholder={protocol === "anthropic-messages" && provider === "custom" ? "https://HOST/v1/messages" : "http://HOST:8080"} /></label>
            )}
            {provider !== "llama-server" && (
              <label className="space-y-1 text-sm sm:col-span-2"><span>API key</span><Input type="password" autoComplete="off" value={credential} onChange={(e) => setCredential(e.target.value)} placeholder="published once to the fleet, never shown again" /></label>
            )}
          </div>
          {endpoint.trim().toLowerCase().startsWith("http://") && <p className="text-xs text-amber-500">Plain HTTP is approved for this fleet backend; use it on private networks only.</p>}
          {needsEndpoint && (
            <label className="flex items-start gap-2 text-xs text-muted-foreground">
              <input type="checkbox" className="mt-0.5" checked={skipProbe} onChange={(e) => setSkipProbe(e.target.checked)} />
              <span>Skip the probe from the control plane. Use this when only the fleet nodes can reach the server; the model name is then required.</span>
            </label>
          )}
          <CellnModelParametersField value={parameters} onChange={setParameters} showThinking={provider === "llama-server" || (provider === "custom" && protocol === "openai-chat")} />
          {(error || add.error) && <p role="alert" className="whitespace-pre-wrap break-words text-xs text-red-500">{error || add.error?.message}</p>}
          <Button type="button" size="sm" disabled={add.isPending} onClick={submit}>
            {add.isPending ? "Probing and recording…" : "Add to the fleet"}
          </Button>
        </div>
      )}
    </div>
  );
}
