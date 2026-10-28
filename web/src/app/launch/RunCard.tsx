/**
 * The run as a whole: its name, how it stops (until) and the gates that
 * judge it, as the run file has them.
 */
import { useState } from "react";
import type { Document } from "yaml";
import { Panel } from "@/components/Panel";
import { durationOK, gates, GATE_PRESETS, getUntil, removeGate, setGate, setUntil, workloads, type Gate } from "@/data/launch/form";
import { GATE_METRICS, GATE_OPS, GATE_PROCEDURES, GATE_QUANTILES } from "@/data/launch/runschema.gen";
import { buttonStyle, FieldRow, inputStyle, TextField } from "../targets/fields";
import { RunChoice, RunText, type Edit } from "./RunFields";

const METRICS = [...GATE_METRICS, ...GATE_PROCEDURES.flatMap((p) => GATE_QUANTILES.map((q) => `${p}.${q}_ms`))];

const selectStyle = { ...inputStyle, textTransform: "none" as const };

function UntilField({ doc, edit }: { doc: Document; edit: Edit }) {
  const u = getUntil(doc) ?? { kind: "complete" as const };
  const [duration, setDuration] = useState(u.kind === "duration" ? u.duration : "10m");
  return (
    <FieldRow label="until" path="until" problem={u.kind === "duration" && !durationOK(u.duration) ? "a duration such as 30m or 2h" : undefined}>
      <span className="flex flex-wrap items-center gap-3">
        {(["complete", "duration", "forever"] as const).map((k) => (
          <label key={k} className="o-label flex items-center gap-1" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
            <input
              type="radio"
              name="until"
              checked={u.kind === k}
              onChange={() => edit((d) => setUntil(d, k === "duration" ? { kind: k, duration } : { kind: k }))}
            />
            {k === "complete" ? "every workload completes" : k === "forever" ? "stopped (orbit runs stop)" : "for"}
          </label>
        ))}
        {u.kind === "duration" && (
          <span className="w-24">
            <TextField
              value={duration}
              ariaLabel="until.duration"
              onCommit={(v) => {
                setDuration(v);
                if (durationOK(v)) edit((d) => setUntil(d, { kind: "duration", duration: v }));
              }}
            />
          </span>
        )}
      </span>
    </FieldRow>
  );
}

function GateRow({ g, edit, names }: { g: Gate; edit: Edit; names: string[] }) {
  const set = (patch: Partial<Gate>) => edit((d) => setGate(d, { ...g, ...patch }));
  return (
    <li className="flex min-w-0 flex-wrap items-center gap-2 py-1">
      <span className="o-label max-w-[11rem] truncate" style={{ color: "var(--o-ink-2)", textTransform: "none" }} title={g.name}>
        {g.name}
      </span>
      <select aria-label={`gates[${g.name}].workload`} className="o-label px-1" style={selectStyle} value={g.workload} onChange={(e) => set({ workload: e.target.value })}>
        <option value="">the run</option>
        {names.map((n) => (
          <option key={n} value={n}>
            {n}
          </option>
        ))}
      </select>
      {g.kind === "conformance" ? (
        <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
          conformance: no FAIL outside the expected list, no ERROR
        </span>
      ) : (
        <span className="flex flex-wrap items-center gap-1">
          <select aria-label={`gates[${g.name}].slo.metric`} className="o-label px-1" style={selectStyle} value={g.metric} onChange={(e) => set({ metric: e.target.value })}>
            {!METRICS.includes(g.metric) && <option value={g.metric}>{g.metric}</option>}
            {METRICS.map((m) => (
              <option key={m} value={m}>
                {m}
              </option>
            ))}
          </select>
          <select aria-label={`gates[${g.name}].slo.op`} className="o-label px-1" style={selectStyle} value={g.op} onChange={(e) => set({ op: e.target.value })}>
            <option value="">(by metric)</option>
            {GATE_OPS.map((o) => (
              <option key={o} value={o}>
                {o}
              </option>
            ))}
          </select>
          <span className="w-24">
            <TextField value={g.threshold} ariaLabel={`gates[${g.name}].slo.threshold`} onCommit={(v) => v.trim() && set({ threshold: v.trim() })} />
          </span>
        </span>
      )}
      <button type="button" className="o-label cursor-pointer px-2" style={buttonStyle} onClick={() => edit((d) => removeGate(d, g.name))}>
        remove
      </button>
    </li>
  );
}

export function RunCard({ doc, edit }: { doc: Document; edit: Edit }) {
  const gs = gates(doc);
  const names = workloads(doc).map((w) => w.name);
  const first = workloads(doc)[0];
  return (
    <Panel title="run" meta={`${gs.length} gate${gs.length === 1 ? "" : "s"}`}>
      <div className="flex flex-col">
        <RunText doc={doc} edit={edit} steps={["run", "name"]} label="name" placeholder="the server names it" />
        <RunChoice doc={doc} edit={edit} steps={["run", "event_verbosity"]} label="events" options={["normal", "verbose"]} />
        <UntilField doc={doc} edit={edit} />
        <FieldRow label="gates" path="gates">
          <span className="flex flex-col">
            <ul>
              {gs.map((g) => (
                <GateRow key={g.name} g={g} edit={edit} names={names} />
              ))}
            </ul>
            <span className="flex flex-wrap gap-2 pt-1">
              {GATE_PRESETS.map((p) => {
                const w = p.label === "conformance" ? (workloads(doc).find((x) => x.kind === "conformance")?.name ?? "") : (workloads(doc).find((x) => x.kind === "population")?.name ?? "");
                const gate = p.gate(w);
                const taken = gs.some((g) => g.name === gate.name);
                return (
                  <button
                    key={p.label}
                    type="button"
                    disabled={taken || !first}
                    className="o-label cursor-pointer px-2 py-0.5 disabled:cursor-not-allowed disabled:opacity-50"
                    style={buttonStyle}
                    onClick={() => edit((d) => setGate(d, gate))}
                  >
                    + {p.label}
                  </button>
                );
              })}
            </span>
          </span>
        </FieldRow>
      </div>
    </Panel>
  );
}
