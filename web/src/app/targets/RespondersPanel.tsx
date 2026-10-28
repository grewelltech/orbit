/**
 * RespondersPanel — the N6 responders registered with this server (joined
 * from an app host), each with its state, when it was last seen, its
 * version and addresses, and Remove; and the join tokens still pending,
 * each with Revoke. No credential or loom token is ever shown: the server
 * sends none, and the rows hold only the fields responders.ts lists.
 */
import { useState } from "react";
import { Panel } from "@/components/Panel";
import { StatusDot, type Status } from "@/components/StatusDot";
import { responderRows, tokenRows } from "@/data/join/responders";
import type { useResponders } from "@/data/join/useResponders";
import { buttonStyle } from "./fields";

const STATUS: Record<string, Status> = { online: "ok", offline: "error", "never seen": "idle" };

export function RespondersPanel({ responders }: { responders: ReturnType<typeof useResponders> }) {
  const { state, removing, remove, revoke } = responders;
  const [said, setSaid] = useState<Record<string, string>>({});
  const now = state.at ?? Date.now();
  const rows = responderRows(state.responders, now);
  const pending = tokenRows(state.tokens, now);
  const note = (key: string, text: string | undefined) => text !== undefined && setSaid((m) => ({ ...m, [key]: text }));
  const label = { color: "var(--o-ink-3)", textTransform: "none" as const };
  return (
    <Panel title="responders" meta={state.at === undefined ? "…" : `${rows.length}`}>
      <div className="flex flex-col gap-2">
        {state.error && (
          <span className="o-label" role="alert" style={{ ...label, color: "var(--o-error)" }}>
            not read: {state.error}
          </span>
        )}
        {state.at !== undefined && rows.length === 0 && (
          <span className="o-label" style={label}>
            none registered: join an app host (orbit target add-responder, or orbit join create and its one-liner)
          </span>
        )}
        <ul className="flex flex-col gap-1.5" aria-label="responders">
          {rows.map((r) => (
            <li key={r.name} className="flex flex-col gap-0.5">
              <div className="flex items-center justify-between gap-2">
                <StatusDot status={STATUS[r.state] ?? "warn"} label={`${r.name} · ${r.state}`} />
                <button
                  type="button"
                  disabled={removing.includes(r.name)}
                  className="o-label cursor-pointer px-2 py-0.5 disabled:cursor-not-allowed disabled:opacity-50"
                  style={{ ...buttonStyle, color: "var(--o-error)" }}
                  onClick={() => void remove(r.name).then((t) => note(`r:${r.name}`, t))}
                >
                  {removing.includes(r.name) ? "removing…" : "remove"}
                </button>
              </div>
              <span className="o-label" style={label}>
                seen {r.seen}
                {r.version ? ` · ${r.version}` : ""}
              </span>
              <span className="o-label" style={label}>
                data {r.data} · control {r.control}
              </span>
              {said[`r:${r.name}`] && (
                <span className="o-label" role="status" style={label}>
                  {r.name}: {said[`r:${r.name}`]}
                </span>
              )}
            </li>
          ))}
        </ul>
        {Object.entries(said)
          .filter(([k]) => k.startsWith("r:") && !rows.some((r) => `r:${r.name}` === k))
          .map(([k, v]) => (
            <span key={k} className="o-label" role="status" style={label}>
              {k.slice(2)}: {v}
            </span>
          ))}
        {pending.length > 0 && (
          <div className="flex flex-col gap-1" aria-label="pending join tokens">
            <span className="o-label" style={label}>
              join tokens not yet used
            </span>
            {pending.map((p) => (
              <div key={p.id} className="flex items-center justify-between gap-2">
                <span className="o-label" style={label}>
                  {p.name} · {p.expiresIn === "expired" ? "expired" : `expires in ${p.expiresIn}`}
                </span>
                <button type="button" className="o-label cursor-pointer px-2 py-0.5" style={buttonStyle} onClick={() => void revoke(p.id).then((t) => note(`t:${p.id}`, t))}>
                  revoke
                </button>
              </div>
            ))}
          </div>
        )}
        {Object.entries(said)
          .filter(([k]) => k.startsWith("t:"))
          .map(([k, v]) => (
            <span key={k} className="o-label" role="status" style={label}>
              token {k.slice(2)}: {v}
            </span>
          ))}
      </div>
    </Panel>
  );
}
