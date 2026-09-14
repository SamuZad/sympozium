import { useState } from "react";
import type { Agent, AgentRun } from "@/lib/api";
import { useCreateRun } from "@/hooks/use-api";
import { CellnConversation } from "@/components/celln-conversation";
import { Button } from "@/components/ui/button";
import { Card, CardContent, CardHeader, CardTitle } from "@/components/ui/card";

const DEFAULT_ENDURING = {
  leaseSeconds: 600,
  maxTurns: 8,
  maxModelRequests: 24,
  maxOutputTokens: 8192,
};

/**
 * Interactive chat for a native Celln Agent. Each conversation is an enduring
 * AgentRun (its own host-native parent with its own context); the first message
 * is the initial turn and follow-up turns are durable AgentRunTurn records. An
 * Agent may hold any number of conversations at once; the platform, not this
 * view, decides capacity.
 */
export function CellnAgentConversation({
  agent,
  parents = [],
}: {
  agent: Agent;
  /** This Agent's enduring runs, newest first. */
  parents?: AgentRun[];
}) {
  const createRun = useCreateRun();
  const [message, setMessage] = useState("");
  const [error, setError] = useState("");
  const [selected, setSelected] = useState<string>("");
  const [composing, setComposing] = useState(false);

  const execution = agent.spec.execution;
  const runtimeRef =
    agent.spec.runtimeRef || execution?.cellnSelection?.runtimeRef || "";
  const toolRefs = execution?.cellnSelection?.toolRefs || [];
  const clusterToolRefs = execution?.cellnSelection?.clusterToolRefs || [];
  const model = execution?.model || agent.spec.agents?.default?.model || "";
  const modelConnectionRef = execution?.modelConnectionRef;
  const provider = modelConnectionRef ? undefined : execution?.provider;
  const limits = execution?.enduring || DEFAULT_ENDURING;

  const current = parents.find((run) => run.metadata.name === selected) || parents[0];
  const showComposer = composing || parents.length === 0;

  function start() {
    const text = message.trim();
    if (!text || createRun.isPending) return;
    setError("");
    createRun.mutate(
      {
        agentRef: agent.metadata.name,
        task: text,
        backend: "celln",
        model: model || undefined,
        modelConnectionRef,
        provider,
        cellnSelection: { runtimeRef: runtimeRef || undefined, toolRefs, ...(clusterToolRefs.length ? { clusterToolRefs } : {}) },
        timeout: `${limits.leaseSeconds}s`,
        executionLifecycle: "enduring",
        enduring: limits,
      },
      {
        onSuccess: (run) => {
          setMessage("");
          setComposing(false);
          if (run?.metadata?.name) setSelected(run.metadata.name);
        },
        onError: (err) =>
          setError(
            err instanceof Error
              ? err.message
              : "Could not start the conversation",
          ),
      },
    );
  }

  const conversationList = parents.length > 0 && (
    <div className="flex flex-wrap items-center gap-2" data-testid="celln-agent-conversations">
      {parents.map((run) => {
        const active = !showComposer && run.metadata.name === current?.metadata.name;
        return (
          <Button
            key={run.metadata.uid || run.metadata.name}
            size="sm"
            variant={active ? "default" : "outline"}
            onClick={() => { setSelected(run.metadata.name); setComposing(false); }}
            title={run.spec.task ? String(run.spec.task).slice(0, 200) : undefined}
          >
            {run.metadata.name}
            <span className="ml-2 text-xs opacity-70">{run.status?.phase || "Pending"}</span>
          </Button>
        );
      })}
      <Button size="sm" variant={showComposer ? "default" : "secondary"} data-testid="celln-agent-new-conversation" onClick={() => setComposing(true)}>
        New conversation
      </Button>
    </div>
  );

  if (!showComposer && current) {
    return (
      <div className="space-y-3">
        {conversationList}
        <CellnConversation run={current} />
      </div>
    );
  }

  return (
    <div className="space-y-3">
    {conversationList}
    <Card data-testid="celln-agent-conversation">
      <CardHeader>
        <CardTitle className="text-base">
          Enduring Celln conversation
        </CardTitle>
      </CardHeader>
      <CardContent className="space-y-3">
        <p className="text-sm text-muted-foreground">
          Start an enduring native parent. The first message runs as the initial
          turn; each follow-up turn runs in a disposable child cell. Every
          conversation is its own parent with its own context, so you can keep
          several open for this Agent at once.
        </p>
        <label className="block space-y-2">
          <span className="text-sm font-medium">First message</span>
          <textarea
            data-testid="celln-agent-first-message"
            className="min-h-24 w-full rounded border bg-background p-2 text-sm"
            value={message}
            onChange={(event) => setMessage(event.target.value)}
            disabled={createRun.isPending}
            placeholder="Ask the parent a question to begin…"
          />
        </label>
        <Button
          data-testid="celln-agent-start"
          onClick={start}
          disabled={createRun.isPending || message.trim().length === 0}
        >
          {createRun.isPending ? "Starting…" : "Start conversation"}
        </Button>
        {error && (
          <p role="alert" className="text-sm text-destructive">
            {error}
          </p>
        )}
        {parents.length > 0 && (
          <Button size="sm" variant="ghost" onClick={() => setComposing(false)}>
            Back to conversations
          </Button>
        )}
      </CardContent>
    </Card>
    </div>
  );
}
