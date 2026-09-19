import type { CellnTool } from "@/lib/api";
import { Button } from "@/components/ui/button";
import { cn } from "@/lib/utils";

export interface BorrowedToolRef {
  name: string;
  revision: string;
}

export interface BorrowedToolsCatalogue {
  data?: CellnTool[];
  isLoading: boolean;
  isError: boolean;
  refetch: () => unknown;
}

/**
 * The "Borrow tools" step of the Create Agent wizard (Celln plane). The
 * catalogue is what may be borrowed: for a fleet backend, the shared tools its
 * policy lends; otherwise the tools installed in the namespace.
 */
export function BorrowedToolsStep({
  catalogue,
  fleet,
  selected,
  onChange,
  toolSupported,
  maxTools,
  stale,
}: {
  catalogue: BorrowedToolsCatalogue;
  /** True when the Agent runs on a fleet backend (shared, policy-lent tools). */
  fleet: boolean;
  selected: BorrowedToolRef[];
  onChange: (tools: BorrowedToolRef[]) => void;
  toolSupported: (tool: CellnTool) => boolean;
  /** The platform's hard cap on borrowed tools (24 shared, 16 namespaced). */
  maxTools: number;
  /** The selection names a revision the catalogue no longer offers. */
  stale: boolean;
}) {
  const all = catalogue.data || [];
  const supportedTools = all.filter(toolSupported);
  // Compatible tools first so the unsupported ones don't bury them.
  const tools = [...supportedTools, ...all.filter((tool) => !toolSupported(tool))];
  const ref = (tool: CellnTool): BorrowedToolRef => ({ name: tool.metadata.name, revision: tool.spec.revision });

  return (
    <div className="space-y-3" data-testid="create-agent-borrowed-tools">
      <h3 className="font-medium">Borrow tools for Celln</h3>
      <p className="text-sm text-muted-foreground">All compatible installed tools are selected by default; deselect any you do not want. This saves defaults, not permission grants. Effective operator/runtime/Agent permissions can be previewed on the Harness tab after the Agent exists and are checked again before execution.</p>
      {catalogue.isLoading && <p>Loading tool catalogue…</p>}
      {catalogue.isError && <p role="alert">Cannot load the tool catalogue. Retry before creating this Agent.</p>}
      {catalogue.isError && <Button type="button" onClick={() => catalogue.refetch()}>Retry catalogue</Button>}
      {!catalogue.isLoading && !catalogue.isError && catalogue.data?.length === 0 && <p>{fleet ? "The fleet policy lends this fleet backend no shared tools." : "No tools installed in this namespace."} An empty selection lends no tools.</p>}
      {all.length > 0 && (
        <>
          <div className="flex flex-wrap items-center gap-2">
            <span data-testid="borrowed-tools-count" className={cn("text-xs", selected.length > maxTools && "text-destructive")}>
              {selected.length} of {all.length} {fleet ? "lent" : "installed"} tools selected
              {all.length > maxTools && ` (at most ${maxTools} can be borrowed)`}
            </span>
            <span className="flex-1" />
            <Button type="button" size="sm" variant="outline" disabled={selected.length === Math.min(supportedTools.length, maxTools) && !stale} onClick={() => onChange(supportedTools.slice(0, maxTools).map(ref))}>Select all</Button>
            <Button type="button" size="sm" variant="outline" disabled={selected.length === 0} onClick={() => onChange([])}>Lend no tools</Button>
          </div>
          <div className="grid max-h-[45vh] grid-cols-1 gap-2 overflow-y-auto rounded border p-2 sm:grid-cols-2">
            {tools.map((tool) => {
              const isSelected = selected.some((item) => item.name === tool.metadata.name && item.revision === tool.spec.revision);
              const supported = toolSupported(tool);
              return (
                <label key={tool.metadata.name} title={tool.spec.description} className={cn("flex min-w-0 gap-2 rounded border p-2 text-xs", supported ? "cursor-pointer hover:bg-muted/50" : "opacity-50", isSelected && "border-primary bg-primary/5")}>
                  <input type="checkbox" className="mt-0.5 shrink-0" disabled={!supported || (!isSelected && selected.length >= maxTools)} checked={isSelected}
                    onChange={() => onChange(isSelected ? selected.filter((item) => item.name !== tool.metadata.name) : [...selected, ref(tool)])} />
                  <span className="min-w-0 flex-1">
                    <span className="block truncate font-medium">{tool.metadata.name}<span className="text-muted-foreground">@{tool.spec.revision}</span></span>
                    <span className="line-clamp-2 text-muted-foreground">{tool.spec.description}</span>
                    <span className="block truncate text-muted-foreground">{tool.spec.limits.timeoutMillis} ms · effects: {tool.spec.limits.effects}{!supported ? " · unsupported ABI/lane" : ""}</span>
                  </span>
                </label>
              );
            })}
          </div>
        </>
      )}
      {selected.length > maxTools && <p role="alert">Select at most {maxTools} tools to continue.</p>}
      <p className="text-xs text-muted-foreground">No shell, Python, host mounts or unrestricted network access is included.</p>
      {stale && <p role="alert">The catalogue changed. Clear the selection and choose current revisions.</p>}
    </div>
  );
}
