import { useEffect, useState, type ComponentType } from "react";
import { useQueryClient } from "@tanstack/react-query";
import { Bot, Loader2 } from "lucide-react";
import type { CellnPlatformProfile } from "@/lib/api";
import { useAddCellnFleetBackend, useCellnFleetBackends, useCellnPlatformProfiles } from "@/hooks/use-api";
import { backendLabel } from "@/components/celln-backend-picker";
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
  const pending = backendList.filter((b) => b.provider === provider && b.state !== "ready");

  const [name, setName] = useState("");
  const [model, setModel] = useState("");
  const [endpoint, setEndpoint] = useState("");
  const [protocol, setProtocol] = useState("openai-chat");
  const [customProvider, setCustomProvider] = useState("");
  const [credential, setCredential] = useState("");
  const [skipProbe, setSkipProbe] = useState(false);
  const [error, setError] = useState("");

  // A backend the nodes finished configuring gets its profile; refresh so it
  // becomes selectable without leaving the step.
  const readyWithoutProfile = backendList.some((b) => b.state === "ready" && !list.some((p) => p.name === b.profile));
  useEffect(() => {
    if (readyWithoutProfile) qc.invalidateQueries({ queryKey: ["celln-platform-profiles"] });
  }, [readyWithoutProfile, backendList, qc]);

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
    add.reset();
    const served = list.filter((p) => p.provider === value);
    if (served.length > 0) onProfile(served[0]);
    else onProvider(value);
  }

  function submit() {
    setError("");
    const custom = provider === "custom";
    const trimmedEndpoint = endpoint.trim();
    const body = {
      name: name.trim(),
      provider: custom ? customProvider.trim() || "custom" : provider,
      model: model.trim() || undefined,
      endpoint: trimmedEndpoint || undefined,
      protocol: custom ? protocol : undefined,
      allowInsecure: trimmedEndpoint.toLowerCase().startsWith("http://") || undefined,
      credential: credential || undefined,
      skipPreflight: skipProbe || undefined,
    };
    if (!/^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(body.name)) return setError("Name the backend with a DNS label, e.g. claude.");
    if (provider !== "llama-server" && !credential) return setError(`${preset?.label || provider} needs an API key.`);
    if ((provider === "llama-server" || custom) && !body.endpoint) return setError("Give the server's address, e.g. http://framework:8080.");
    if (!body.model && (!detectable || skipProbe)) return setError(skipProbe ? "Give the model name when the probe is skipped." : "Give the model name.");
    add.mutate(body, { onSuccess: () => setCredential("") });
  }

  const needsEndpoint = provider === "llama-server" || provider === "custom";
  // An OpenAI-compatible server lists its models, so the API can detect one.
  const detectable = provider === "llama-server" || (provider === "custom" && protocol === "openai-chat");
  const current = selected && selected.provider === provider ? selected : undefined;

  return (
    <div className="space-y-4" data-testid="platform-model-route">
      <div className="space-y-2">
        <Label>AI Provider</Label>
        <Select value={options.some((o) => o.value === provider) ? provider : ""} onValueChange={pick}>
          <SelectTrigger><SelectValue placeholder="Select a provider…" /></SelectTrigger>
          <SelectContent>
            {options.map((o) => {
              const count = list.filter((p) => p.provider === o.value).length;
              return (
                <SelectItem key={o.value} value={o.value}>
                  <span className="flex items-center gap-2">
                    <o.icon className="h-4 w-4 shrink-0" />
                    {o.label}
                    <span className="text-xs text-muted-foreground">{count > 0 ? `(${count} fleet backend${count === 1 ? "" : "s"})` : "(add to fleet)"}</span>
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
            <SelectTrigger><SelectValue placeholder="Choose a backend" /></SelectTrigger>
            <SelectContent>{matching.map((p) => <SelectItem key={p.name} value={p.name}>{backendLabel(p)}</SelectItem>)}</SelectContent>
          </Select>
        </div>
      )}

      {current && (
        <p className="text-xs text-muted-foreground">
          Runs on fleet backend <span className="font-medium text-foreground">{current.backend}</span> ({current.model} at {current.endpoint}) with the key the fleet holds for it. No key is entered here; policy caps the ceilings.
        </p>
      )}

      {provider && matching.length === 0 && pending.length > 0 && (
        <div role="status" className="space-y-1 rounded-md border border-amber-500/30 bg-amber-500/5 p-3 text-xs">
          {pending.map((b) => (
            <p key={b.name} className="flex items-center gap-2">
              {!b.state.startsWith("error") && <Loader2 className="h-3 w-3 animate-spin" />}
              <span><span className="font-medium">{b.name}</span> — {b.model}: <span className={b.state.startsWith("error") ? "text-red-500" : ""}>{b.state}</span></span>
            </p>
          ))}
          <p className="text-muted-foreground">The fleet's nodes are configuring this backend; this step continues on its own once it is ready. Running conversations are not restarted.</p>
        </div>
      )}

      {provider && !profiles.isLoading && matching.length === 0 && pending.every((b) => b.state.startsWith("error")) && (
        <div className="space-y-3 rounded-md border p-3" data-testid="celln-provider-add-backend">
          <p className="text-xs text-muted-foreground">
            The fleet has no {preset?.label || provider} backend yet. Add one: the {needsEndpoint ? "endpoint is probed" : "key is probed once and published to the fleet"}, every node configures it, and every namespace is offered it.
          </p>
          <div className="grid gap-3 sm:grid-cols-2">
            <label className="space-y-1 text-sm"><span>Backend name</span><Input value={name} onChange={(e) => setName(e.target.value)} placeholder={provider} /></label>
            <label className="space-y-1 text-sm"><span>Model</span><Input value={model} onChange={(e) => setModel(e.target.value)} placeholder={preset?.model || (detectable ? "detected from the server" : "the model the server serves")} /></label>
            {provider === "custom" && (
              <>
                <label className="space-y-1 text-sm"><span>Provider id</span><Input value={customProvider} onChange={(e) => setCustomProvider(e.target.value)} placeholder="custom" /></label>
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
          {endpoint.trim().toLowerCase().startsWith("http://") && <p className="text-xs text-amber-500">Plain HTTP is approved for this backend; use it on private networks only.</p>}
          {needsEndpoint && (
            <label className="flex items-start gap-2 text-xs text-muted-foreground">
              <input type="checkbox" className="mt-0.5" checked={skipProbe} onChange={(e) => setSkipProbe(e.target.checked)} />
              <span>Skip the probe from the control plane. Use this when only the fleet nodes can reach the server; the model name is then required.</span>
            </label>
          )}
          {(error || add.error) && <p role="alert" className="whitespace-pre-wrap break-words text-xs text-red-500">{error || add.error?.message}</p>}
          <Button type="button" size="sm" disabled={add.isPending} onClick={submit}>
            {add.isPending ? "Probing and recording…" : "Add to the fleet"}
          </Button>
        </div>
      )}
    </div>
  );
}
