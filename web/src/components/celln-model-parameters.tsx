import { useState } from "react";
import { compactModelParameters, parseModelParameters, thinkingDisabled, withThinkingDisabled, type ModelParameters } from "@/lib/model-parameters";

/**
 * The "Advanced: model parameters" part of an add-a-fleet-backend form. The
 * JSON text is the one value: the "Disable thinking" checkbox reads and
 * rewrites the same object, so the two never disagree.
 */
export function CellnModelParametersField({ value, onChange, showThinking }: { value: string; onChange: (text: string) => void; showThinking: boolean }) {
  const [open, setOpen] = useState(false);
  const parsed = parseModelParameters(value);
  const object = currentObject(value);
  return (
    <details className="rounded-md border p-2 text-xs sm:col-span-2" data-testid="celln-model-parameters" open={open} onToggle={(e) => setOpen(e.currentTarget.open)}>
      <summary className="cursor-pointer select-none text-sm">Advanced: model parameters</summary>
      <div className="mt-2 space-y-2">
        <p className="text-muted-foreground">
          A JSON object the fleet sends with every model request of this fleet backend. The agent cannot see or change it, and it cannot be changed after the fleet backend is added. Needs a Celln release newer than v0.5.22 on the fleet nodes.
        </p>
        {showThinking && (
          <label className="flex items-start gap-2">
            <input
              type="checkbox"
              className="mt-0.5"
              data-testid="celln-disable-thinking"
              disabled={object === undefined}
              checked={thinkingDisabled(object)}
              onChange={(e) => {
                const next = withThinkingDisabled(object, e.target.checked);
                onChange(Object.keys(next).length ? JSON.stringify(next, null, 2) : "");
              }}
            />
            <span>
              <span className="text-foreground">Disable thinking (reasoning models)</span>
              <span className="block text-muted-foreground">Celln allows 512 output tokens per request; a reasoning model can spend them all thinking and return nothing.</span>
            </span>
          </label>
        )}
        <label className="block space-y-1">
          <span>Parameters (JSON)</span>
          <textarea
            className="min-h-20 w-full rounded-md border bg-transparent p-2 font-mono text-xs"
            data-testid="celln-model-parameters-json"
            spellCheck={false}
            value={value}
            onChange={(e) => onChange(e.target.value)}
            placeholder={'{"chat_template_kwargs": {"enable_thinking": false}}'}
          />
        </label>
        {parsed.error && <p role="alert" className="break-words text-red-500" data-testid="celln-model-parameters-error">{parsed.error}</p>}
      </div>
    </details>
  );
}

/** The object the text holds, valid for the fleet or not; undefined when it is not a JSON object. */
function currentObject(text: string): ModelParameters | undefined {
  if (!text.trim()) return {};
  try {
    const value: unknown = JSON.parse(text);
    return typeof value === "object" && value !== null && !Array.isArray(value) ? (value as ModelParameters) : undefined;
  } catch {
    return undefined;
  }
}

/** A backend's parameters on one line, truncated, with the whole object as the tooltip. */
export function CellnModelParametersSummary({ parameters, testId }: { parameters?: ModelParameters; testId?: string }) {
  const text = compactModelParameters(parameters);
  if (!text) return null;
  return (
    <span className="inline-block max-w-[24rem] truncate align-bottom font-mono text-muted-foreground" title={text} data-testid={testId}>
      sends {text}
    </span>
  );
}
