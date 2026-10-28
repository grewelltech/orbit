/**
 * The Launch page (?launch): build a run against a stored target, start
 * it, or take it to the CLI.
 *
 * The run is a run file (kind: run), edited here field by field or as
 * YAML. The server resolves the target and the run file as the CLI
 * resolves the same files (RunService.PlanRun) and builds the plan Start
 * sends; it also gives the command and the file to save
 * (RunService.RenderRun). Keys stay ${VAR}s in the target, their values
 * in this tab, and go to the server only with a plan.
 *
 * ?conformance opens it on the conformance template (the suite's own page
 * before this one): a target made here from an AMF and a network identity
 * runs it, the checks that need a UE skipped.
 */
import { Panel } from "@/components/Panel";
import { addWorkload, workloads } from "@/data/launch/form";
import { TEMPLATES } from "@/data/launch/runschema.gen";
import { useState } from "react";
import { quickTargetProblem, useLaunch, type QuickTarget } from "@/data/launch/useLaunch";
import { carriesSecrets, varsPromptNote, whyNotStart } from "@/data/launch/resolve";
import { loadToken } from "@/data/transport";
import { secretSlots, refName } from "@/data/targets/yamlio";
import { parseStored } from "@/data/targets/actions";
import { PageLinks } from "./PageLinks";
import { buttonStyle, inputStyle, TextField } from "./targets/fields";
import { CommandPanel } from "./launch/CommandPanel";
import { StartedRuns } from "./launch/StartedRuns";
import { ProvenancePanel } from "./ProvenancePanel";
import { LAUNCH_NO_ENV } from "@/data/provenance";
import { RunCard } from "./launch/RunCard";
import { WorkloadCard } from "./launch/WorkloadCard";

/** A target from an AMF and a network identity, for a suite with no UE (and a first target). */
function QuickTargetForm({ onAdd, taken }: { onAdd: (q: QuickTarget) => void; taken: readonly string[] }) {
  const [q, setQ] = useState<QuickTarget>({ name: taken.includes("core") ? "" : "core", amf: "", mcc: "", mnc: "", sst: "1", sd: "", tac: "" });
  const field = (k: keyof QuickTarget, label: string, placeholder: string, width: string) => (
    <label className="o-label flex flex-col gap-0.5" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
      {label}
      <input
        aria-label={`new target ${label}`}
        value={q[k]}
        placeholder={placeholder}
        onChange={(e) => setQ({ ...q, [k]: e.target.value })}
        className="o-label px-2 py-0.5"
        style={{ ...inputStyle, width, textTransform: "none" }}
      />
    </label>
  );
  const problem = quickTargetProblem(q);
  const ready = problem === undefined;
  return (
    <div className="flex flex-col gap-1">
      <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
        a target from an AMF and its network identity: no keys, so a suite skips the checks that need a UE
      </span>
      <div className="flex flex-wrap items-end gap-2">
        {field("name", "name", "core", "8rem")}
        {field("amf", "AMF", "host:port", "12rem")}
        {field("mcc", "MCC", "001", "4rem")}
        {field("mnc", "MNC", "01", "4rem")}
        {field("tac", "TAC", "1", "6rem")}
        {field("sst", "SST", "1", "3rem")}
        {field("sd", "SD", "hex, optional", "7rem")}
        <button type="button" disabled={!ready} title={problem} className="o-label cursor-pointer px-2 py-1 disabled:opacity-50" style={buttonStyle} onClick={() => ready && onAdd(q)}>
          add target
        </button>
      </div>
    </div>
  );
}

export function Launch({ preset }: { preset?: string }) {
  const l = useLaunch(undefined, undefined, { preset });
  const [quick, setQuick] = useState(false);
  const t = l.target;
  const s = l.state;
  const ws = workloads(l.doc);
  // The target's keys are set on the Targets page; the other ${VAR}s here.
  const parsed = t ? parseStored(t) : undefined;
  const keyRefs = new Set(parsed ? secretSlots(parsed.doc).map((x) => refName(x.value)).filter((v): v is string => v !== undefined) : []);
  const missingKeys = l.refs.filter((r) => keyRefs.has(r) && !l.values[r]);
  const prompts = l.refs.filter((r) => !keyRefs.has(r));
  const why = whyNotStart({
    loc: window.location,
    token: loadToken(),
    hasTarget: t !== undefined,
    missingKeys,
    unsetVars: prompts.filter((r) => !l.values[r]),
    workloads: ws.length,
    carriesSecrets: t !== undefined && carriesSecrets({ targetYaml: t.yaml, runYaml: l.runYaml }),
  });
  const err = s.planError;

  return (
    <div className="flex h-dvh flex-col overflow-hidden" style={{ background: "var(--o-bg)" }}>
      <header
        className="flex flex-wrap items-center gap-x-6 gap-y-2 border-b px-4 py-2.5"
        style={{ borderColor: "var(--o-border)", background: "var(--o-surface)" }}
      >
        <span style={{ fontFamily: "var(--o-font-mono)", fontWeight: 600, letterSpacing: "var(--o-tracking-title)", color: "var(--o-ink)" }}>ORBIT</span>
        <PageLinks current="launch" />
        <span className="o-label ml-auto" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
          the server resolves this as `orbit run` resolves the same files; the CLI's $ORBIT_* overrides do not apply here.
        </span>
      </header>

      {l.notice && (
        <div className="flex items-start gap-3 border-b px-4 py-2" role="status" style={{ borderColor: "var(--o-border)", background: "var(--o-surface-2)" }}>
          <span className="o-label flex-1" style={{ color: "var(--o-amber)", textTransform: "none" }}>
            {l.notice}
          </span>
          <button type="button" className="o-label cursor-pointer px-2" style={buttonStyle} onClick={() => l.notify(undefined)}>
            dismiss
          </button>
        </div>
      )}

      <main className="grid flex-1 grid-cols-1 content-start items-start gap-3 overflow-auto p-3 lg:grid-cols-[minmax(0,1fr)_minmax(0,1fr)]">
        <div className="flex min-w-0 flex-col gap-3">
          <Panel title="target and template">
            <div className="flex flex-col gap-2">
              <label className="o-label flex items-center gap-2" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                target
                <select aria-label="target" className="o-label px-2 py-0.5" style={{ ...inputStyle, textTransform: "none" }} value={t?.name ?? ""} onChange={(e) => l.selectTarget(e.target.value)}>
                  {l.targets.map((x) => (
                    <option key={x.name} value={x.name}>
                      {x.name}
                    </option>
                  ))}
                </select>
                <a href="?targets" className="o-label" style={{ color: "var(--o-ink-3)" }}>
                  edit targets
                </a>
                <button type="button" className="o-label cursor-pointer px-2" style={buttonStyle} onClick={() => setQuick((v) => !v)}>
                  new from AMF + PLMN
                </button>
              </label>
              {(quick || l.targets.length === 0) && (
                <QuickTargetForm
                  taken={l.targets.map((x) => x.name)}
                  onAdd={(q) => {
                    const notices = l.addTarget(q);
                    l.notify(notices[0]);
                    setQuick(false);
                  }}
                />
              )}
              <label className="o-label flex items-center gap-2" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                start from
                <select
                  aria-label="template"
                  className="o-label px-2 py-0.5"
                  style={{ ...inputStyle, textTransform: "none" }}
                  value={l.template}
                  onChange={(e) => {
                    if (window.confirm("Start the form over from this template? The run file as it is now is replaced.")) l.chooseTemplate(e.target.value);
                  }}
                >
                  {Object.keys(TEMPLATES).map((n) => (
                    <option key={n} value={n}>
                      template {n}
                    </option>
                  ))}
                  <option value="">an empty run file</option>
                </select>
              </label>
              {prompts.length > 0 && (
                <div className="flex flex-col gap-1" aria-label="variables">
                  <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                    {varsPromptNote(l.remembered)}
                  </span>
                  {prompts.map((v) => (
                    <label key={v} className="grid grid-cols-[14rem_1fr] items-center gap-2">
                      <span className="o-label" style={{ color: l.values[v] ? "var(--o-ink-2)" : "var(--o-amber)", textTransform: "none" }}>{`\${${v}}`}</span>
                      <TextField value={l.values[v] ?? ""} ariaLabel={v} onCommit={(value) => l.setValue(v, value)} />
                    </label>
                  ))}
                </div>
              )}
            </div>
          </Panel>

          {ws.map((w) => (
            <WorkloadCard key={`${w.index}:${w.name}`} doc={l.doc} edit={l.edit} w={w} values={l.values} onNotice={(n) => l.notify(n)} />
          ))}
          <div className="flex gap-2">
            <button type="button" className="o-label cursor-pointer px-2 py-1" style={buttonStyle} onClick={() => l.edit((d) => void addWorkload(d, "population"))}>
              + population
            </button>
            <button type="button" className="o-label cursor-pointer px-2 py-1" style={buttonStyle} onClick={() => l.edit((d) => void addWorkload(d, "conformance"))}>
              + conformance suite
            </button>
          </div>
          <RunCard doc={l.doc} edit={l.edit} />
        </div>

        <div className="flex min-w-0 flex-col gap-3">
          <Panel title="start" live={s.busy !== undefined} meta={s.busy === "planning" ? "resolving on the server" : s.busy === "starting" ? "starting" : s.plan ? "plan resolved" : undefined}>
            <div className="flex flex-col gap-2">
              <div className="flex flex-wrap items-center gap-3">
                <button
                  type="button"
                  disabled={why !== undefined || s.busy !== undefined}
                  title={why}
                  className="o-label cursor-pointer px-3 py-1 disabled:cursor-not-allowed disabled:opacity-50"
                  style={{ color: "var(--o-bg)", background: "var(--o-accent)", borderRadius: "var(--o-radius)" }}
                  onClick={() => void l.start()}
                >
                  start{t ? ` against ${t.name}` : ""}
                </button>
                <button
                  type="button"
                  disabled={why !== undefined || s.busy !== undefined}
                  className="o-label cursor-pointer px-2 py-1 disabled:cursor-not-allowed disabled:opacity-50"
                  style={buttonStyle}
                  onClick={() => void l.plan()}
                >
                  check the plan
                </button>
              </div>
              {why && (
                <span className="o-label" role="note" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                  {why}
                </span>
              )}
              {err && (
                <span className="o-label" role="alert" style={{ color: "var(--o-error)", textTransform: "none" }}>
                  {err.at?.path ? `${err.at.doc} ${err.at.path}: ${err.at.reason}` : err.message}
                </span>
              )}
              {s.plan && !err && (
                <span className="o-label" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                  {`${s.plan.response.plan?.workloads.length ?? 0} workload(s), ${s.plan.response.plan?.gates.length ?? 0} gate(s), ${s.plan.response.provenance.length} fields resolved; Start sends this plan as it is`}
                </span>
              )}
            </div>
          </Panel>
          <StartedRuns runs={l.started} />
          {s.plan && (
            <ProvenancePanel provenance={s.plan.response.provenance} notices={s.plan.response.notices} resolved footnote={LAUNCH_NO_ENV} />
          )}
          <CommandPanel state={s} runYaml={l.runYaml} targetName={t?.name ?? "target"} replaceText={(text, keep) => l.replaceText(text, keep ? l.template : "")} onNotice={(n) => l.notify(n)} />
        </div>
      </main>
    </div>
  );
}
