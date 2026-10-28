/**
 * The Launch form's edits, each a change to the run document (kind: run)
 * the page holds: the document is the form's state, so what the page
 * exports and sends is what the form shows, and a run file imported whole
 * keeps what the form has no control for. Pure functions over a Document;
 * the page's hook (useLaunch) holds the result.
 */
import { Document, isMap, isScalar, isSeq, parseDocument, YAMLMap, YAMLSeq, Scalar } from "yaml";
import { addElement, getValue, listLength, removeElement, removeValue, setValue, utf8Length, MAX_FILE_BYTES, type Step } from "../targets/yamlio";
import { parseRun } from "./runyaml";
import { TEMPLATES } from "./runschema.gen";

/** A new run document: a template's text, or a run with nothing in it yet. */
export function startFrom(template: string | undefined): Document {
  return parseRun(template ? (TEMPLATES[template] ?? "kind: run\n") : "kind: run\n");
}

/** A workload is a population (attach, behaviors) or a conformance suite. */
export type WorkloadKind = "population" | "conformance";

export interface WorkloadView {
  index: number;
  name: string;
  kind: WorkloadKind;
}

/** The document's workloads, in order. */
export function workloads(doc: Document): WorkloadView[] {
  return Array.from({ length: listLength(doc, ["workloads"]) }, (_, i) => ({
    index: i,
    name: String(getValue(doc, ["workloads", i, "name"]) ?? ""),
    kind: doc.getIn(["workloads", i, "conformance"], true) !== undefined ? "conformance" : "population",
  }));
}

/** A name not yet taken in a named list: base, else base-2, base-3, …. */
export function freeName(taken: readonly string[], base: string): string {
  if (!taken.includes(base)) return base;
  for (let n = 2; ; n++) if (!taken.includes(`${base}-${n}`)) return `${base}-${n}`;
}

/** Adds a workload of kind, with the fields its kind needs, and returns its index. */
export function addWorkload(doc: Document, kind: WorkloadKind): number {
  const name = freeName(
    workloads(doc).map((w) => w.name),
    kind === "conformance" ? "suite" : "pop",
  );
  const i = addElement(doc, ["workloads"], name);
  if (kind === "conformance") {
    const c = new YAMLMap();
    c.set("categories", new YAMLSeq());
    c.flow = true;
    doc.setIn(["workloads", i, "conformance"], c);
  } else {
    setValue(doc, ["workloads", i, "attach", "count"], "10");
    setValue(doc, ["workloads", i, "attach", "rate"], "5");
  }
  return i;
}

/** Removes workload i, and the gates that judge it alone. */
export function removeWorkload(doc: Document, i: number): void {
  const name = String(getValue(doc, ["workloads", i, "name"]) ?? "");
  for (let g = listLength(doc, ["gates"]) - 1; g >= 0; g--) {
    if (getValue(doc, ["gates", g, "workload"]) === name) removeElement(doc, ["gates"], g);
  }
  removeElement(doc, ["workloads"], i);
}

/** How the run stops (until): one of three. */
export type Until = { kind: "complete" } | { kind: "duration"; duration: string } | { kind: "forever" };

/** The run's until, or undefined when the document leaves it to the default (complete). */
export function getUntil(doc: Document): Until | undefined {
  if (getValue(doc, ["until", "forever"]) === "true") return { kind: "forever" };
  const d = getValue(doc, ["until", "duration"]);
  if (typeof d === "string" && d !== "") return { kind: "duration", duration: d };
  const ms = getValue(doc, ["until", "duration_ms"]);
  if (typeof ms === "string" && ms !== "") return { kind: "duration", duration: `${ms}ms` };
  if (getValue(doc, ["until", "complete"]) === "true") return { kind: "complete" };
  return undefined;
}

/** Sets until to u alone: the other ways to stop are removed, as the run file allows one. */
export function setUntil(doc: Document, u: Until): void {
  removeValue(doc, ["until"]);
  const m = new YAMLMap();
  m.flow = true;
  if (u.kind === "complete") m.set("complete", new Scalar("true"));
  else if (u.kind === "forever") m.set("forever", new Scalar("true"));
  else m.set("duration", new Scalar(u.duration));
  doc.setIn(["until"], m);
}

/** A Go duration as time.ParseDuration takes one, above zero (30s, 2h, 1h30m). */
export function durationOK(s: string): boolean {
  return /^([0-9]+(\.[0-9]+)?(ns|us|µs|ms|s|m|h))+$/.test(s) && !/^(0+(\.0+)?(ns|us|µs|ms|s|m|h))+$/.test(s);
}

/** A gate as the form edits it. */
export interface Gate {
  name: string;
  /** The workload it judges; "" judges the run. */
  workload: string;
  kind: "slo" | "conformance";
  metric: string;
  op: string;
  threshold: string;
}

/** The document's gates, in order. */
export function gates(doc: Document): Gate[] {
  return Array.from({ length: listLength(doc, ["gates"]) }, (_, i) => {
    const s = (...p: Step[]) => String(getValue(doc, ["gates", i, ...p]) ?? "");
    return {
      name: s("name"),
      workload: s("workload"),
      kind: s("conformance") === "true" ? "conformance" : "slo",
      metric: s("slo", "metric"),
      op: s("slo", "op"),
      threshold: s("slo", "threshold"),
    };
  });
}

/** Sets gate g: replaces the gate of its name, or adds it. */
export function setGate(doc: Document, g: Gate): void {
  const names = gates(doc).map((x) => x.name);
  let i = names.indexOf(g.name);
  if (i < 0) i = addElement(doc, ["gates"], g.name);
  for (const k of ["workload", "conformance", "slo"]) removeValue(doc, ["gates", i, k]);
  if (g.workload) setValue(doc, ["gates", i, "workload"], g.workload);
  if (g.kind === "conformance") {
    setValue(doc, ["gates", i, "conformance"], "true");
    return;
  }
  const slo = new YAMLMap();
  slo.flow = true;
  slo.set("metric", new Scalar(g.metric));
  if (g.op) {
    const op = new Scalar(g.op);
    op.type = Scalar.QUOTE_DOUBLE; // <= and >= read as YAML otherwise
    slo.set("op", op);
  }
  const th = new Scalar(g.threshold);
  th.type = Scalar.QUOTE_DOUBLE;
  slo.set("threshold", th);
  doc.setIn(["gates", i, "slo"], slo);
}

/** Removes the gate named name. */
export function removeGate(doc: Document, name: string): void {
  const i = gates(doc).findIndex((g) => g.name === name);
  if (i >= 0) removeElement(doc, ["gates"], i);
}

/** The gates the form offers in one click, as the --slo-* flags and --gate conformance give them. */
export const GATE_PRESETS: readonly { label: string; gate: (workload: string) => Gate }[] = [
  {
    label: "attach success ≥ 0.99",
    gate: (w) => ({ name: "attach_success_ratio", workload: w, kind: "slo", metric: "attach.success_ratio", op: ">=", threshold: "0.99" }),
  },
  {
    label: "registration p99 ≤ 500ms",
    gate: (w) => ({ name: "registration_p99_ms", workload: w, kind: "slo", metric: "registration.p99_ms", op: "<=", threshold: "500ms" }),
  },
  { label: "attach p99 ≤ 1s", gate: (w) => ({ name: "attach_p99_ms", workload: w, kind: "slo", metric: "attach.p99_ms", op: "<=", threshold: "1s" }) },
  { label: "conformance", gate: (w) => ({ name: w ? `${w}_conformance` : "conformance", workload: w, kind: "conformance", metric: "", op: "", threshold: "" }) },
];

/**
 * The SLO presets for the whole run: the same gates with no workload, as
 * the --slo-* flags give them (their name is the metric's, gates[<name>]
 * with no workload), so the copied command says --slo-reg-p99 and the like.
 * A workload's gate of the same name and this one are one gate: either
 * takes the name.
 */
export const RUN_GATE_PRESETS: readonly { label: string; gate: () => Gate }[] = GATE_PRESETS.filter((p) => p.gate("").kind === "slo").map((p) => ({
  label: `${p.label}, the whole run`,
  gate: () => ({ ...p.gate(""), workload: "" }),
}));

/** Why an expected-failures file is not inlined. */
export const EXPECTED_TOO_LARGE = "larger than 1 MiB; an expected-failures list is a few KiB";

/** Why a picked expected-failures file is not read at all, by its size alone, or undefined. */
export function pickedFileRefusal(f: { size: number }): string | undefined {
  return f.size > MAX_FILE_BYTES ? EXPECTED_TOO_LARGE : undefined;
}

/**
 * Puts an expected-failures file's list inline at workload i, as the
 * server takes it (it reads no file). The file is capped at 1 MiB and goes
 * through the run file's gate in place; on a refusal the document is
 * unchanged and the reason (schema paths only) is thrown.
 */
export function inlineExpected(doc: Document, i: number, fileText: string): void {
  if (utf8Length(fileText) > MAX_FILE_BYTES) throw new Error(EXPECTED_TOO_LARGE);
  const parsed = parseDocument(fileText, { schema: "failsafe", prettyErrors: false });
  if (parsed.errors[0]) throw new Error("the expected-failures file is not valid YAML");
  const list = parsed.contents;
  if (!isMap(list)) throw new Error("the expected-failures file is not a mapping (core, version, entries)");
  const trial = doc.clone();
  trial.setIn(["workloads", i, "conformance", "expected"], list);
  // The whole document through the gate: the list's fields, a tag, an
  // alias, a secret written out, all as an imported run file would be.
  parseRun(trial.toString());
  doc.setIn(["workloads", i, "conformance", "expected"], trial.getIn(["workloads", i, "conformance", "expected"], true));
}

/** Whether workload i has an expected-failures list, and how many entries it holds. */
export function expectedEntries(doc: Document, i: number): number | undefined {
  const n = doc.getIn(["workloads", i, "conformance", "expected"], true);
  if (n === undefined) return undefined;
  const e = isMap(n) ? n.get("entries", true) : undefined;
  return isSeq(e) ? e.items.length : 0;
}

/** Whether the scalar at steps is "true". */
export function flag(doc: Document, steps: readonly Step[]): boolean {
  const n = doc.getIn(steps, true);
  return isScalar(n) && String(n.value) === "true";
}

/** Sets a bool field: true written, false removed (the run file's default). */
export function setFlag(doc: Document, steps: readonly Step[], on: boolean): void {
  if (on) setValue(doc, steps, "true");
  else removeValue(doc, steps);
}

/**
 * Sets a conformance workload's categories; none is `categories: []`
 * (every category), kept so the workload stays a conformance suite.
 */
export function setCategories(doc: Document, i: number, categories: readonly string[]): void {
  const seq = new YAMLSeq();
  seq.flow = true;
  for (const c of categories) seq.items.push(new Scalar(c));
  doc.setIn(["workloads", i, "conformance", "categories"], seq);
}

/** A population keeps its UEs (behaviors set, {} idle) or is a storm (none). */
export function setKeepUEs(doc: Document, i: number, keep: boolean): void {
  if (!keep) {
    removeValue(doc, ["workloads", i, "behaviors"]);
    return;
  }
  if (doc.getIn(["workloads", i, "behaviors"], true) !== undefined) return;
  const m = new YAMLMap();
  m.flow = true;
  doc.setIn(["workloads", i, "behaviors"], m);
}
