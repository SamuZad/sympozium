import type { Agent, AgentRuntime } from "./api";

export function persistentHarnessName(runtime: AgentRuntime): "Pi" | "Hermes" | undefined {
  if (runtime.spec.contractVersion !== "v1alpha2" || runtime.spec.session?.protocol !== "openai-chat") return undefined;
  const adapter = runtime.spec.image?.match(/\/(pi|hermes)@sha256:/)?.[1];
  return adapter === "pi" ? "Pi" : adapter === "hermes" ? "Hermes" : undefined;
}

export function persistentHarnesses(runtimes: AgentRuntime[]): AgentRuntime[] {
  return runtimes.filter((runtime) => persistentHarnessName(runtime) !== undefined);
}

/**
 * Native Celln runtimes use the AgentRuntime CRD but execute on the Celln plane,
 * not as a Kubernetes harness. They must not be presented as harnesses.
 */
export function isNativeCellnRuntime(runtime: AgentRuntime): boolean {
  // A fleet wrapper names its platform profile instead of carrying one.
  return !!runtime.spec.celln || !!runtime.spec.cellnProfileRef;
}

/**
 * An Agent that holds enduring native Celln conversations: each is its own
 * host-native parent, so it is a persistent chat like a harness session.
 */
export function isCellnEnduringAgent(agent: Agent, runtime: AgentRuntime | undefined): boolean {
  const execution = agent.spec.execution;
  // The platform's own wrapper Agents name only their fleet runtime; a fleet
  // profile always runs enduring parents unless the Agent says otherwise.
  if (runtime?.spec.cellnProfileRef && !execution) return true;
  return (
    (runtime?.spec.celln?.contractVersion === "celln.json-tools/v1" || Boolean(runtime?.spec.cellnProfileRef)) &&
    execution?.backend === "celln" &&
    execution?.executionLifecycle === "enduring"
  );
}

/** AgentRuntimes that represent Kubernetes/OCI harnesses (excludes native Celln runtimes). */
export function kubernetesHarnesses(runtimes: AgentRuntime[]): AgentRuntime[] {
  return runtimes.filter((runtime) => !isNativeCellnRuntime(runtime));
}
