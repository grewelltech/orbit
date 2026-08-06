/**
 * RunPicker — which run the dashboard is showing.
 *
 * "Latest" follows whatever is live and moves on as new runs start, which is
 * what someone watching a testbed wants. Selecting a specific run pins it, so
 * an operator mid-investigation is not dragged onto a new run the moment
 * someone else starts one — but that has to be *visible*, or a pinned
 * dashboard looks stalled. Hence the "newer run available" hint.
 */
import type { RunSummary } from "@/data/runs";

export interface RunPickerProps {
  runs: RunSummary[];
  /** undefined = follow the latest. */
  selected: string | undefined;
  onSelect: (runId: string | undefined) => void;
}

const FOLLOW = "__latest__";

function label(r: RunSummary): string {
  const name = r.name || r.runId;
  return `${name} · ${r.kind} · ${r.state}`;
}

export function RunPicker({ runs, selected, onSelect }: RunPickerProps) {
  // Only meaningful while pinned: following the latest cannot be behind it.
  const newerAvailable =
    selected !== undefined &&
    runs.length > 0 &&
    runs[0] !== undefined &&
    runs[0].runId !== selected;

  return (
    <div className="flex items-center gap-1.5">
      <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
        run
      </span>
      <select
        value={selected ?? FOLLOW}
        onChange={(e) => onSelect(e.target.value === FOLLOW ? undefined : e.target.value)}
        className="o-label cursor-pointer border px-1.5 py-0.5"
        style={{
          color: "var(--o-ink-2)",
          background: "var(--o-surface-2)",
          borderColor: "var(--o-border)",
          borderRadius: "var(--o-radius)",
        }}
        aria-label="Run to display"
      >
        <option value={FOLLOW}>latest (follow)</option>
        {runs.map((r) => (
          <option key={r.runId} value={r.runId}>
            {label(r)}
          </option>
        ))}
      </select>
      {newerAvailable && (
        <button
          type="button"
          onClick={() => onSelect(undefined)}
          className="o-label cursor-pointer border px-1.5 py-0.5"
          style={{
            color: "var(--o-amber)",
            borderColor: "var(--o-border)",
            borderRadius: "var(--o-radius)",
          }}
          title="A newer run has started. Click to follow the latest again."
        >
          newer run available
        </button>
      )}
    </div>
  );
}
