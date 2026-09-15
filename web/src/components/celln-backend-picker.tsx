import { useState } from "react";
import { api, type Agent, type AgentRuntime, type CellnPlatformProfile } from "@/lib/api";
import { useCellnPlatformProfiles, usePatchAgent } from "@/hooks/use-api";
import { Label } from "@/components/ui/label";
import { Select, SelectContent, SelectItem, SelectTrigger, SelectValue } from "@/components/ui/select";

/** One line naming a fleet backend the way an operator thinks of it. */
export function backendLabel(profile: CellnPlatformProfile): string {
  const protocol = profile.endpoint.endsWith("/messages") ? " · anthropic-messages" : "";
  return `${profile.backend} — ${profile.provider} / ${profile.model}${protocol}`;
}

/** The platform profile an Agent currently runs on, by its wrapper runtime. */
export function profileForAgent(agent: Agent, profiles: CellnPlatformProfile[] | undefined): CellnPlatformProfile | undefined {
  const runtimeRef = agent.spec.runtimeRef || agent.spec.execution?.cellnSelection?.runtimeRef || "";
  return (profiles || []).find((profile) => profile.wrapper === runtimeRef);
}

/**
 * Picks the fleet backend (provider, model, endpoint) an Agent runs on. Every
 * backend the namespace's policy admits is offered; choosing one makes sure
 * the namespace has that backend's wrapper objects and rebinds the Agent's
 * runtime, model connection, model and shared tools to it. The lifecycle and
 * any borrowed-tool choice are kept.
 */
export function CellnBackendPicker({ agent, runtimes }: { agent: Agent; runtimes: AgentRuntime[] }) {
  const profiles = useCellnPlatformProfiles();
  const patchAgent = usePatchAgent();
  const [switching, setSwitching] = useState<string | null>(null);
  const [error, setError] = useState<string | null>(null);
  const list = profiles.data || [];
  if (profiles.isLoading || list.length === 0) return null;
  const current = profileForAgent(agent, list);
  const execution = agent.spec.execution;
  const lifecycle = execution?.executionLifecycle || "enduring";
  const selectedRuntime = runtimes.find((runtime) => runtime.metadata.name === agent.spec.runtimeRef);
  const legacyNative = !!selectedRuntime && !selectedRuntime.spec.cellnProfileRef;

  async function choose(name: string) {
    const profile = list.find((candidate) => candidate.name === name);
    if (!profile || switching) return;
    setSwitching(name);
    setError(null);
    try {
      const wrappers = await api.cellnPlatform.ensureWrappers(profile.name);
      // Keep what the Agent already lends from this policy; otherwise lend
      // every shared tool the policy allows, as the installer's sample run does.
      const clusterToolRefs = execution?.cellnSelection?.clusterToolRefs?.length ? execution.cellnSelection.clusterToolRefs : profile.tools;
      await patchAgent.mutateAsync({
        name: agent.metadata.name,
        data: {
          runtimeRef: wrappers.runtime,
          execution: {
            backend: "celln",
            executionLifecycle: lifecycle,
            modelConnectionRef: wrappers.connection,
            model: profile.model,
            cellnSelection: { runtimeRef: wrappers.runtime, toolRefs: [], clusterToolRefs },
            ...(lifecycle === "enduring" ? { enduring: execution?.enduring || profile.sessionDefaults } : {}),
          },
        },
      });
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    } finally {
      setSwitching(null);
    }
  }

  return (
    <div className="space-y-2" data-testid="agent-backend-picker">
      <Label>Model backend</Label>
      <Select value={current?.name || ""} onValueChange={choose} disabled={!!switching || patchAgent.isPending}>
        <SelectTrigger>
          <SelectValue placeholder={legacyNative ? "Namespace runtime (not a fleet backend)" : "Choose a backend"} />
        </SelectTrigger>
        <SelectContent>
          {list.map((profile) => (
            <SelectItem key={profile.name} value={profile.name}>
              {backendLabel(profile)}
            </SelectItem>
          ))}
        </SelectContent>
      </Select>
      {current ? (
        <p className="text-xs text-muted-foreground">
          Runs on <span className="font-medium text-foreground">{current.endpoint}</span> with the fleet's key for this backend; wrapper <code>{current.wrapper}</code>, connection <code>{current.wrapper}</code>. Up to {Math.round(current.ceilings.leaseSeconds / 3600)} h and {current.ceilings.maxTurns} turns per conversation. Every backend of the fleet serves one-shot and enduring runs alike.
        </p>
      ) : (
        <p className="text-xs text-muted-foreground">The namespace's fleet offers {list.length} backend{list.length === 1 ? "" : "s"}. Choosing one creates its wrapper objects here on first use; no YAML.</p>
      )}
      {switching && <p className="text-xs text-muted-foreground">Switching to {switching}…</p>}
      {error && <p role="alert" className="text-xs text-red-400">{error}</p>}
    </div>
  );
}
