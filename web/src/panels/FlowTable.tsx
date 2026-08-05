/**
 * FlowTable — the UEs actually carrying traffic, busiest first.
 *
 * The server ranks and truncates (MaxReportedFlows), so this renders what it
 * was sent and states the untruncated total. A population run has far more
 * flows than a table should hold, and a list that quietly showed the first
 * hundred would read as the whole run.
 */
import { Panel } from "@/components/Panel";
import type { Flow } from "@/data/types";
import { bps } from "@/lib/format";

export interface FlowTableProps {
  flows: Flow[];
  /** Flows carrying traffic before truncation. */
  total: number;
  className?: string;
}

function rate(v: number): string {
  const f = bps(v);
  return `${f.value} ${f.unit}`;
}

export function FlowTable({ flows, total, className }: FlowTableProps) {
  const truncated = total > flows.length;
  return (
    <Panel
      title="Active flows"
      live
      flush
      className={className}
      meta={
        <span className="o-label" style={{ color: "var(--o-ink-3)" }}>
          {truncated ? `${flows.length} of ${total} busiest` : `${total}`}
        </span>
      }
    >
      <div className="h-full min-h-0 overflow-auto">
        {flows.length === 0 ? (
          <p className="o-label px-3 py-3">no flows carrying traffic</p>
        ) : (
          <table className="w-full border-collapse text-left">
            <thead>
              <tr className="o-label" style={{ color: "var(--o-ink-3)" }}>
                <th className="px-3 py-1 font-normal">UE</th>
                <th className="px-2 py-1 font-normal">app</th>
                <th className="px-2 py-1 font-normal">cohort</th>
                <th className="px-2 py-1 text-right font-normal">uplink</th>
                <th className="px-2 py-1 text-right font-normal">downlink</th>
              </tr>
            </thead>
            <tbody>
              {flows.map((f) => (
                <tr key={`${f.supi}|${f.app}`} style={{ borderTop: "1px solid var(--o-border)" }}>
                  <td className="px-3 py-1 font-mono text-xs">{f.supi}</td>
                  <td className="px-2 py-1 text-xs">{f.app}</td>
                  <td className="px-2 py-1 text-xs" style={{ color: "var(--o-ink-3)" }}>
                    {f.cohort || "—"}
                  </td>
                  <td className="px-2 py-1 text-right font-mono text-xs">{rate(f.uplinkBps)}</td>
                  <td className="px-2 py-1 text-right font-mono text-xs">{rate(f.downlinkBps)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </div>
      {truncated && (
        <p className="o-label px-3 py-1" style={{ color: "var(--o-ink-3)" }}>
          {total - flows.length} more not shown
        </p>
      )}
    </Panel>
  );
}
