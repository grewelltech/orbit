/**
 * One workload of the run: a population (attach, topology, behaviours) or
 * a conformance suite (categories, the expected-failures list). Fields the
 * form has no control for (a traffic mix, a start) stay in the run file as
 * written, and the YAML editor reaches them.
 */
import { useEffect, useMemo, useState } from "react";
import { createClient } from "@connectrpc/connect";
import type { Document } from "yaml";
import { Panel } from "@/components/Panel";
import { ConformanceService } from "@/gen/orbit/v1/conformance_pb";
import { orbitTransport } from "@/data/transport";
import { EXPECTED_TOO_LARGE, expectedEntries, inlineExpected, removeWorkload, setCategories, setKeepUEs, type WorkloadView } from "@/data/launch/form";
import { BEHAVIOR_FIELDS } from "@/data/launch/runschema.gen";
import { readImportFile } from "@/data/targets/importexport";
import { getValue, MAX_FILE_BYTES, type Step } from "@/data/targets/yamlio";
import { buttonStyle, FieldRow } from "../targets/fields";
import { RunCheck, RunChoice, RunText, type Edit } from "./RunFields";

/** The categories the server's suite has (ListConformanceTests), fetched once per page. */
function useCategories(): string[] {
  const [cats, setCats] = useState<string[]>([]);
  const client = useMemo(() => createClient(ConformanceService, orbitTransport()), []);
  useEffect(() => {
    let live = true;
    client
      .listConformanceTests({})
      .then((r) => live && setCats([...new Set(r.tests.map((t) => t.category))].sort()))
      .catch(() => {
        /* no server, or a token prompt: the categories stay a free list */
      });
    return () => {
      live = false;
    };
  }, [client]);
  return cats;
}

/** A behaviours field's control, from its schema entry (runschema.gen.ts). */
function BehaviorField({ doc, edit, base, path, values }: { doc: Document; edit: Edit; base: Step[]; path: string; values: Readonly<Record<string, string>> }) {
  const f = BEHAVIOR_FIELDS.find((x) => x.path === path);
  if (!f) return null;
  const steps = [...base, ...path.split(".")];
  if (f.type === "enum") return <RunChoice doc={doc} edit={edit} steps={steps} label={path} options={f.values ?? []} />;
  if (f.secret) {
    const v = getValue(doc, steps);
    const ref = typeof v === "string" ? /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/.exec(v)?.[1] : undefined;
    return (
      <RunText
        doc={doc}
        edit={edit}
        steps={steps}
        label={path}
        placeholder="${APP_TOKEN}: a token goes as a variable, its value in this tab"
        problem={ref && !values[ref] ? `\${${ref}} has no value in this tab yet` : undefined}
      />
    );
  }
  return <RunText doc={doc} edit={edit} steps={steps} label={path} />;
}

export function WorkloadCard({ doc, edit, w, values, onNotice }: { doc: Document; edit: Edit; w: WorkloadView; values: Readonly<Record<string, string>>; onNotice: (n: string) => void }) {
  const base: Step[] = ["workloads", w.index];
  const categories = useCategories();
  const keeps = doc.getIn([...base, "behaviors"], true) !== undefined;
  const picked = getValue(doc, [...base, "conformance", "categories"]);
  const chosen = Array.isArray(picked) ? picked : [];
  const entries = expectedEntries(doc, w.index);

  return (
    <Panel
      title={`${w.kind} · ${w.name}`}
      meta={
        <button type="button" className="o-label cursor-pointer px-2" style={buttonStyle} onClick={() => edit((d) => removeWorkload(d, w.index))}>
          remove
        </button>
      }
    >
      <div className="flex flex-col">
        <RunText doc={doc} edit={edit} steps={[...base, "name"]} label="name" />
        {w.kind === "population" ? (
          <>
            <RunText doc={doc} edit={edit} steps={[...base, "attach", "count"]} label="UEs" placeholder="attach.count" />
            <RunText doc={doc} edit={edit} steps={[...base, "attach", "rate"]} label="rate (/s)" placeholder="0: as fast as concurrency allows" />
            <RunText doc={doc} edit={edit} steps={[...base, "attach", "ramp"]} label="ramp" placeholder="start:end:seconds, instead of rate" />
            <RunChoice doc={doc} edit={edit} steps={[...base, "attach", "arrival"]} label="arrival" options={["uniform", "poisson"]} />
            <RunText doc={doc} edit={edit} steps={[...base, "attach", "concurrency"]} label="concurrency" />
            <RunText doc={doc} edit={edit} steps={[...base, "topology", "gnb_count"]} label="gNBs" />
            <RunCheck doc={doc} edit={edit} steps={[...base, "pdu_session"]} label="PDU session" />
            <RunText doc={doc} edit={edit} steps={[...base, "subscriber_pool"]} label="subscriber pool" placeholder="the target's first" />
            <FieldRow label="keep the UEs" path={`${base.join(".")}.behaviors`}>
              <input
                type="checkbox"
                aria-label="keep the UEs (behaviors)"
                checked={keeps}
                onChange={(e) => edit((d) => setKeepUEs(d, w.index, e.target.checked))}
              />
            </FieldRow>
            {keeps && (
              <div className="ml-4 flex flex-col border-l pl-3" style={{ borderColor: "var(--o-border)" }}>
                {["latency.target", "latency.interval_ms", "latency.ues", "mobility.model", "mobility.speed_mps", "mobility.handover"].map((p) => (
                  <BehaviorField key={p} doc={doc} edit={edit} base={[...base, "behaviors"]} path={p} values={values} />
                ))}
                {["traffic.bulk.ues", "traffic.bulk.uplink_bps", "traffic.bulk.downlink_bps", "traffic.bulk.responder", "traffic.bulk.token"].map((p) => (
                  <BehaviorField key={p} doc={doc} edit={edit} base={[...base, "behaviors"]} path={p} values={values} />
                ))}
                <span className="o-label py-1" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                  a traffic mix and app cohorts: edit them in the run file below
                </span>
              </div>
            )}
          </>
        ) : (
          <>
            <FieldRow label="categories" path={`${base.join(".")}.conformance.categories`}>
              <span className="flex flex-wrap gap-x-3 gap-y-1">
                {(categories.length > 0 ? categories : chosen).map((c) => (
                  <label key={c} className="o-label flex items-center gap-1" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                    <input
                      type="checkbox"
                      checked={chosen.includes(c)}
                      onChange={(e) => edit((d) => setCategories(d, w.index, e.target.checked ? [...chosen, c] : chosen.filter((x) => x !== c)))}
                    />
                    {c}
                  </label>
                ))}
                <span className="o-label" style={{ color: "var(--o-ink-3)", textTransform: "none" }}>
                  {chosen.length === 0 ? "none ticked: every category" : ""}
                </span>
              </span>
            </FieldRow>
            <RunText doc={doc} edit={edit} steps={[...base, "conformance", "per_test_timeout_ms"]} label="per-test timeout (ms)" />
            <RunText doc={doc} edit={edit} steps={[...base, "conformance", "n3_bind"]} label="N3 bind" placeholder="ip:port; empty: the GTP-U checks SKIP" />
            <RunText doc={doc} edit={edit} steps={[...base, "conformance", "traffic"]} label="echo peer" placeholder="host:port" />
            <FieldRow label="expected failures" path={`${base.join(".")}.conformance.expected`}>
              <span className="flex items-center gap-2">
                <span className="o-label" style={{ color: "var(--o-ink-2)", textTransform: "none" }}>
                  {entries === undefined ? "none" : `${entries} entr${entries === 1 ? "y" : "ies"}, inline`}
                </span>
                <label className="o-label cursor-pointer px-2" style={buttonStyle}>
                  pick a file
                  <input
                    type="file"
                    accept=".yaml,.yml"
                    className="hidden"
                    onChange={async (e) => {
                      const f = e.target.files?.[0];
                      e.target.value = "";
                      if (!f) return;
                      if (f.size > MAX_FILE_BYTES) return onNotice(`expected-failures file not read: ${EXPECTED_TOO_LARGE}`);
                      const r = await readImportFile(f);
                      if ("refusal" in r) return onNotice(`expected-failures file not read: ${r.refusal}`);
                      edit((d) => inlineExpected(d, w.index, r.text));
                    }}
                  />
                </label>
              </span>
            </FieldRow>
          </>
        )}
      </div>
    </Panel>
  );
}
