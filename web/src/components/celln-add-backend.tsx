import { useState } from "react";
import { useAddCellnFleetBackend, useCellnFleetBackends } from "@/hooks/use-api";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

const PROVIDERS = [
  { value: "deepseek", label: "DeepSeek", model: "deepseek-chat", keyed: true },
  { value: "openai", label: "OpenAI", model: "gpt-4o-mini", keyed: true },
  { value: "anthropic", label: "Anthropic", model: "claude-sonnet-5", keyed: true },
  { value: "llama-server", label: "llama-server (local)", model: "", keyed: false },
] as const;

/**
 * Add a model backend to the running Celln fleet: the key is published once,
 * every node configures the backend from the admitted package, and it is
 * offered to every namespace when ready. Owners and conversations keep going.
 */
export function CellnAddBackend() {
  const backends = useCellnFleetBackends();
  const add = useAddCellnFleetBackend();
  const [open, setOpen] = useState(false);
  const [provider, setProvider] = useState<(typeof PROVIDERS)[number]["value"]>("deepseek");
  const [name, setName] = useState("");
  const [model, setModel] = useState("deepseek-chat");
  const [endpoint, setEndpoint] = useState("");
  const [credential, setCredential] = useState("");
  const [error, setError] = useState("");
  const preset = PROVIDERS.find((p) => p.value === provider)!;
  const list = backends.data || [];

  function pick(value: (typeof PROVIDERS)[number]["value"]) {
    setProvider(value);
    const chosen = PROVIDERS.find((p) => p.value === value)!;
    setModel(chosen.model);
    if (!name || PROVIDERS.some((p) => p.value === name)) setName(value);
  }

  function submit() {
    setError("");
    const body = {
      name: name.trim(),
      provider,
      model: model.trim() || undefined,
      endpoint: endpoint.trim() || undefined,
      allowInsecure: endpoint.trim().toLowerCase().startsWith("http://") || undefined,
      credential: credential || undefined,
    };
    if (!body.name) return setError("Give the backend a name (a DNS label, e.g. claude).");
    if (preset.keyed && !credential) return setError(`${preset.label} needs an API key.`);
    if (provider === "llama-server" && !body.endpoint) return setError("llama-server needs its address, e.g. http://framework:8080; the model is detected when left blank.");
    add.mutate(body, { onSuccess: () => { setOpen(false); setCredential(""); setName(""); } });
  }

  if (backends.isError) return null; // no fleet on this cluster
  return (
    <div className="space-y-2 rounded-md border p-3" data-testid="celln-add-backend">
      <div className="flex items-center justify-between gap-2">
        <div>
          <Label>Fleet model backends</Label>
          <p className="text-xs text-muted-foreground">Every backend is offered to every namespace; an Agent picks one above. Adding one never restarts a running conversation.</p>
        </div>
        <Button type="button" size="sm" variant="outline" onClick={() => setOpen(!open)}>{open ? "Close" : "Add a backend"}</Button>
      </div>
      {list.length > 0 && (
        <ul className="space-y-1 text-xs">
          {list.map((b) => (
            <li key={b.name} data-testid={`celln-backend-${b.name}`}>
              <span className="font-medium">{b.name}</span> — {b.provider} / {b.model} {b.source === "added" ? "(added)" : ""} · <span className={b.state === "ready" ? "text-green-600" : b.state.startsWith("error") ? "text-red-600" : "text-amber-600"}>{b.state}</span>
            </li>
          ))}
        </ul>
      )}
      {open && (
        <div className="grid gap-2 sm:grid-cols-2">
          <label className="space-y-1 text-sm">
            <span>Provider</span>
            <Select value={provider} onValueChange={(v) => pick(v as (typeof PROVIDERS)[number]["value"])}>
              <SelectTrigger><SelectValue /></SelectTrigger>
              <SelectContent>{PROVIDERS.map((p) => <SelectItem key={p.value} value={p.value}>{p.label}</SelectItem>)}</SelectContent>
            </Select>
          </label>
          <label className="space-y-1 text-sm"><span>Name</span><Input value={name} onChange={(e) => setName(e.target.value)} placeholder={provider} /></label>
          <label className="space-y-1 text-sm"><span>Model</span><Input value={model} onChange={(e) => setModel(e.target.value)} placeholder={preset.model || "detected from the server"} /></label>
          {provider === "llama-server" ? (
            <label className="space-y-1 text-sm"><span>Chat endpoint</span><Input value={endpoint} onChange={(e) => setEndpoint(e.target.value)} placeholder="http://HOST:8080" /></label>
          ) : (
            <label className="space-y-1 text-sm"><span>API key</span><Input type="password" value={credential} onChange={(e) => setCredential(e.target.value)} placeholder="published once to the fleet, never shown" /></label>
          )}
          {error && <p role="alert" className="text-xs text-red-600 sm:col-span-2">{error}</p>}
          <div className="sm:col-span-2">
            <Button type="button" size="sm" disabled={add.isPending} onClick={submit}>{add.isPending ? "Probing and recording…" : "Add to the fleet"}</Button>
          </div>
        </div>
      )}
    </div>
  );
}
