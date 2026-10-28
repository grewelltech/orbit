/**
 * ProbePanel — the doctor (TargetService.Probe) for the target being
 * edited, from this page: the seven steps as the server runs them, each
 * with what to fix. The target goes as the document has it, each ${VAR}
 * replaced by this tab's value; the server resolves and checks it as the
 * CLI loads a file.
 */
import { useState } from "react";
import { Panel } from "@/components/Panel";
import { StatTile } from "@/components/StatTile";
import { StatusDot, type Status } from "@/components/StatusDot";
import { keysBlocked, summary, type ProbeState } from "@/data/targets/probe";
import type { ProbeRequestInit } from "@/data/targets/probe";
import { requestTarget } from "@/data/targets/request";
import type { Editing } from "@/data/targets/useTargets";
import { loadToken } from "@/data/transport";
import { documentRefs, refName, secretSlots } from "@/data/targets/yamlio";
import { buttonStyle, inputStyle, TextField } from "./fields";

const STEP_STATUS: Record<string, Status> = { OK: "ok", FAIL: "error", SKIP: "idle" };

export interface ProbePanelProps {
  editing: Editing;
  state: ProbeState;
  run: (req: ProbeRequestInit) => void;
  cancel: () => void;
  setKey: (variable: string, value: string) => void;
}

export function ProbePanel({ editing, state, run, cancel, setKey }: ProbePanelProps) {
  const [coreOnly, setCoreOnly] = useState(false);
  const [timeout, setTimeoutS] = useState("20");
  const req = requestTarget(editing.doc, editing.keys);
  const secrets = new Set(secretSlots(editing.doc).map((s) => refName(s.value)).filter((v): v is string => v !== undefined));
  // The ${VAR}s outside the keys (an AMF from the shell, say): asked for here.
  const vars = documentRefs(editing.doc).filter((v) => !secrets.has(v));
  const seconds = Number(timeout);
  const why =
    keysBlocked(window.location, loadToken()) ??
    (editing.issues.length > 0
      ? `fix the target's ${editing.issues.length} problem${editing.issues.length === 1 ? "" : "s"} first`
      : req.unresolved.length > 0
        ? `set ${req.unresolved.map((v) => `\${${v}}`).join(", ")} first: the browser has no environment to take them from`
        : !Number.isInteger(seconds) || seconds < 1 || seconds > 120
          ? "the step timeout is 1 to 120 seconds"
          : undefined);
  const running = state.phase === "running";
  const sum = summary(state, coreOnly);

  return (
    <Panel title="probe" live={running} meta={running ? "running on the server" : sum}>
      <div className="flex flex-col gap-3">
        {vars.length > 0 && (
          <div className="flex flex-col gap-1" aria-label="variables">
            <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
              the target names these; their values are held in this tab (keys are set in the target's fields):
            </span>
            {vars.map((v) => (
              <label key={v} className="grid grid-cols-[14rem_1fr] items-center gap-2">
                <span className="o-label" style={{ color: editing.keys[v] ? "var(--o-ink-2)" : "var(--o-amber)", textTransform: "none" }}>{`\${${v}}`}</span>
                <TextField value={editing.keys[v] ?? ""} ariaLabel={v} onCommit={(value) => setKey(v, value)} />
              </label>
            ))}
          </div>
        )}
        <div className="flex flex-wrap items-center gap-3">
          <label className="o-label flex items-center gap-2" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
            <input type="checkbox" checked={coreOnly} onChange={(e) => setCoreOnly(e.target.checked)} />
            core only (steps 1–3)
          </label>
          <label className="o-label flex items-center gap-2" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
            step timeout
            <input
              value={timeout}
              onChange={(e) => setTimeoutS(e.target.value)}
              aria-label="step timeout in seconds"
              className="o-label w-14 px-2 py-0.5"
              style={{ ...inputStyle, textTransform: "none" }}
            />
            s
          </label>
          {running ? (
            <button type="button" className="o-label cursor-pointer px-3 py-1" style={{ ...buttonStyle, color: "var(--o-amber)" }} onClick={cancel}>
              cancel
            </button>
          ) : (
            <button
              type="button"
              disabled={why !== undefined}
              title={why}
              className="o-label cursor-pointer px-3 py-1 disabled:cursor-not-allowed disabled:opacity-50"
              style={{ color: "var(--o-bg)", background: "var(--o-accent)", borderRadius: "var(--o-radius)" }}
              onClick={() => run({ target: req.target, targetName: editing.name, coreOnly, stepTimeoutMs: seconds * 1000 })}
            >
              probe {editing.name}
            </button>
          )}
        </div>
        {why && !running && (
          <span className="o-label" role="note" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
            {why}
          </span>
        )}
        {state.error && (
          <span className="o-label" role="alert" style={{ color: "var(--o-error)", textTransform: "none" }}>
            {state.field ? `${state.field.path}: ${state.field.reason}` : state.error}
          </span>
        )}
        {state.phase === "cancelled" && (
          <span className="o-label" style={{ color: "var(--o-amber)", textTransform: "none" }}>
            cancelled: the server deregisters the UE and releases what it held
          </span>
        )}
        {state.steps.length > 0 && (
          <ol className="flex flex-col gap-2">
            {state.steps.map((s) => (
              <li key={s.index} className="grid grid-cols-[1.5rem_12rem_1fr] gap-2">
                <span className="o-num" style={{ color: "var(--o-ink-3)" }}>
                  {s.index}
                </span>
                <StatusDot status={STEP_STATUS[s.status] ?? "idle"} label={`${s.name} ${s.status}`} />
                <span className="flex flex-col gap-0.5">
                  <span className="o-label" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                    {s.detail}
                  </span>
                  {s.fix && (
                    <span className="o-label" style={{ color: "var(--o-amber)", textTransform: "none" }}>
                      fix: {s.fix}
                    </span>
                  )}
                </span>
              </li>
            ))}
          </ol>
        )}
        {sum && <StatTile label="result" value={sum} tone={state.steps.some((x) => x.status === "FAIL") ? "error" : "ok"} />}
      </div>
    </Panel>
  );
}
