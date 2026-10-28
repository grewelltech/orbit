/**
 * JoinPanel — adds an N6 host to this server without a terminal on the
 * RAN box: a join token for the target's N6 network, as the one-liner to
 * run on the app host, which joins it as a responder; then "use <name> in
 * <target>". The one-liner holds a live token: it is asked for only over
 * https:// (joinBlocked), held in memory only, masked until revealed, and
 * revoked when the panel lets it go unused (another target, close, the page
 * going); it ends on its own at its TTL.
 */
import { useState } from "react";
import { Panel } from "@/components/Panel";
import type { RegisteredResponder } from "@/gen/orbit/v1/join_pb";
import { usesResponder } from "@/data/join/addToTarget";
import { joinBlocked } from "@/data/join/gate";
import { maskOneLiner } from "@/data/join/mask";
import { joinSpecFor } from "@/data/join/spec";
import { useJoin } from "@/data/join/useJoin";
import type { Editing } from "@/data/targets/useTargets";
import { loadToken } from "@/data/transport";
import { buttonStyle, inputStyle, TextField } from "./fields";

const TTLS = [
  { label: "5 min", seconds: 300 },
  { label: "10 min", seconds: 600 },
  { label: "30 min", seconds: 1800 },
  { label: "1 hour", seconds: 3600 },
];

/** Time left as the countdown says it. */
function left(ms: number): string {
  const s = Math.max(0, Math.ceil(ms / 1000));
  return s < 60 ? `${s}s` : `${Math.floor(s / 60)}m ${s % 60}s`;
}

export function JoinPanel({ editing, responders, use }: { editing: Editing; responders: readonly RegisteredResponder[]; use: (name: string) => void }) {
  const join = useJoin(editing.name, responders);
  const [name, setName] = useState("");
  const [ttl, setTTL] = useState(600);
  const [mgmt, setMgmt] = useState("");
  const [revealed, setRevealed] = useState(false);
  const [copied, setCopied] = useState(false);
  const s = join.state;
  const blocked = joinBlocked(window.location, loadToken());
  const spec = joinSpecFor(editing.doc, editing.keys, mgmt);
  const missing = "missing" in spec ? spec.missing : undefined;
  const why =
    blocked ??
    (missing && missing.fields.length > 0 ? `the target needs ${missing.fields.join(", ")} (the app host's routes and firewall come from them)` : undefined) ??
    (missing && missing.vars.length > 0 ? `set ${missing.vars.map((v) => `\${${v}}`).join(", ")} first` : undefined);
  const label = { color: "var(--o-ink-3)", textTransform: "none" as const };
  const usedBy = s.phase === "used" ? s.usedBy : undefined;
  return (
    <Panel title="add an N6 host" meta={s.phase === "ready" && s.expiresAt !== undefined ? `expires in ${left(s.expiresAt - join.now)}` : undefined}>
      <div className="flex flex-col gap-2">
        <span className="o-label" style={label}>
          a join token for this target's N6 network, as a one-liner to run on the app host: it sets the host up as a responder and registers it here
        </span>
        <div className="flex flex-wrap items-end gap-2">
          <label className="o-label flex flex-col gap-0.5" style={label}>
            name
            <TextField value={name} onCommit={setName} placeholder="the app host's hostname" ariaLabel="responder name" />
          </label>
          <label className="o-label flex flex-col gap-0.5" style={label}>
            lasts
            <select aria-label="join token lifetime" className="o-label px-2 py-1" style={{ ...inputStyle, textTransform: "none" }} value={ttl} onChange={(e) => setTTL(Number(e.target.value))}>
              {TTLS.map((t) => (
                <option key={t.seconds} value={t.seconds}>
                  {t.label}
                </option>
              ))}
            </select>
          </label>
          <label className="o-label flex flex-col gap-0.5" style={label}>
            this server, as the app host reaches it
            <TextField value={mgmt} onCommit={setMgmt} placeholder="host:port (default: the server's listen address)" ariaLabel="management address" />
          </label>
          <button
            type="button"
            disabled={why !== undefined || s.phase === "creating"}
            title={why}
            className="o-label cursor-pointer px-2 py-1 disabled:cursor-not-allowed disabled:opacity-50"
            style={buttonStyle}
            onClick={() => {
              if ("spec" in spec) {
                setRevealed(false);
                setCopied(false);
                void join.make(name.trim(), ttl, spec.spec);
              }
            }}
          >
            make a join one-liner
          </button>
        </div>
        {why && (
          <span className="o-label" role="note" style={label}>
            {why}
          </span>
        )}
        {s.phase === "ready" && s.line !== undefined && (
          <div className="flex flex-col gap-1">
            <pre className="o-num overflow-x-auto whitespace-pre-wrap break-all p-2" style={inputStyle} aria-label="join one-liner" onCopy={join.copiedByHand}>
              {revealed ? s.line : maskOneLiner(s.line, s.secret ?? "")}
            </pre>
            <div className="flex flex-wrap gap-2">
              <button
                type="button"
                className="o-label cursor-pointer px-2 py-0.5"
                style={buttonStyle}
                onClick={() => {
                  if (!revealed) join.reveal();
                  setRevealed((r) => !r);
                }}
              >
                {revealed ? "hide" : "reveal"}
              </button>
              <button
                type="button"
                className="o-label cursor-pointer px-2 py-0.5"
                style={buttonStyle}
                onClick={() => void join.copy().then(setCopied)}
              >
                {copied ? "copied" : "copy"}
              </button>
              <button type="button" className="o-label cursor-pointer px-2 py-0.5" style={{ ...buttonStyle, color: "var(--o-error)" }} onClick={() => void join.revoke()}>
                revoke
              </button>
              <button type="button" className="o-label cursor-pointer px-2 py-0.5" style={buttonStyle} onClick={() => void join.close()}>
                close
              </button>
            </div>
            <span className="o-label" role="note" style={label}>
              Run it on the app host only: it runs the join script as root. Anyone with it can join a host until it is used, revoked or expired.
            </span>
            {s.copied && (
              <span className="o-label" role="status" style={{ ...label, color: "var(--o-amber)" }}>
                shown or copied: it works until it is used or expires at {s.expiresAt === undefined ? "its expiry" : new Date(s.expiresAt).toLocaleTimeString()};
                Revoke stops it now. Closing this panel asks first; leaving the page keeps it.
              </span>
            )}
          </div>
        )}
        {usedBy && (
          <div className="flex flex-wrap items-center gap-2">
            <span className="o-label" role="status" style={label}>
              used by {usedBy}: it joined
            </span>
            {!usesResponder(editing.doc, usedBy) ? (
              <button type="button" className="o-label cursor-pointer px-2 py-0.5" style={buttonStyle} onClick={() => use(usedBy)}>
                use {usedBy} in {editing.name}
              </button>
            ) : (
              <span className="o-label" style={label}>
                {editing.name} uses it
              </span>
            )}
          </div>
        )}
        {s.note && s.phase !== "ready" && (
          <span className="o-label" role="status" style={{ ...label, color: s.phase === "failed" ? "var(--o-error)" : label.color }}>
            {s.note}
          </span>
        )}
      </div>
    </Panel>
  );
}
