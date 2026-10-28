/**
 * Target files in the browser: read, checked, edited and written back as the
 * CLI reads them (internal/target/load.go).
 *
 * Every scalar is read as its text (the YAML failsafe schema), as the CLI
 * reads a node: `mcc: 001` stays "001" rather than becoming the number 1.
 * Edits go through the yaml Document, so comments, key order and the
 * quoting of untouched values survive, and a file the CLI wrote comes back
 * byte for byte when nothing in it changed.
 *
 * The checks here are structure only: known keys, the right shapes, numbers
 * written as numbers, names. Formats (an SD of six hex digits, a CIDR) are
 * the server's to judge; it is the authority, and says which field.
 */
import { Document, isMap, isScalar, isSeq, parseAllDocuments, Scalar, YAMLMap, YAMLSeq, type Node } from "yaml";
import { DEFAULTS, NAMED_LISTS, TARGET_FIELDS, VALID_NAME, type FieldSpec } from "./schema.gen";

/** What the name rule allows, in words. */
export const VALID_NAME_TEXT = "letters, digits, '-' and '_', starting with a letter or digit, at most 63";

/** A step into a document: a mapping key or a list index. */
export type Step = string | number;

/** One problem with a document, on the CLI's canonical path (subscribers[main].ki). */
export interface Issue {
  path: string;
  message: string;
}

const FIELDS: ReadonlyMap<string, FieldSpec> = new Map(TARGET_FIELDS.map((f) => [f.path, f]));
const LISTS: ReadonlySet<string> = new Set(NAMED_LISTS);

/** The field at a pattern (subscribers[].ki), if the schema has one. */
export function fieldAt(pattern: string): FieldSpec | undefined {
  return FIELDS.get(pattern);
}

const ENV_REF = /\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g;
const PURE_REF = /^\$\{([A-Za-z_][A-Za-z0-9_]*)\}$/;

/** The variable a value is exactly a reference to (`${ORBIT_KI}` → ORBIT_KI), else undefined. */
export function refName(value: string): string | undefined {
  return PURE_REF.exec(value)?.[1];
}

/** Every ${VAR} a value names, in order. */
export function refsIn(value: string): string[] {
  return [...value.matchAll(ENV_REF)].map((m) => m[1] ?? "");
}

/** How a document is written: 4-space indents and indented lists, as `orbit target init` writes. */
export interface WriteStyle {
  indent: number;
}

const INIT_STYLE: WriteStyle = { indent: 4 };

/** The indent a file uses: its first indented line's, else init's. */
export function styleOf(text: string): WriteStyle {
  const m = /^\n?(?:.*\n)*?( +)\S/m.exec(text);
  const n = m?.[1]?.length ?? 0;
  return n >= 2 && n <= 8 ? { indent: n } : INIT_STYLE;
}

/** The text of doc, written in style. */
export function render(doc: Document, style: WriteStyle = INIT_STYLE): string {
  return doc.toString({ indent: style.indent, indentSeq: true, lineWidth: 0, flowCollectionPadding: false });
}

/** One target read from a file, with the text it came as when it stands alone. */
export interface ParsedTarget {
  doc: Document;
  /** The file's own text when the file is this one target; undefined for a list's member. */
  text?: string;
  style: WriteStyle;
}

/**
 * Reads a target file: one target, or a list of them, as LoadFile takes.
 * A YAML syntax error fails the whole file.
 */
export function parseTargets(text: string): ParsedTarget[] {
  const docs = parseAllDocuments(text, { schema: "failsafe", prettyErrors: true });
  if (!Array.isArray(docs)) throw new Error("empty");
  const first = docs[0];
  if (!first || docs.length > 1) throw new Error(docs.length > 1 ? "one YAML document per file" : "empty");
  if (first.errors.length > 0) throw new Error(first.errors[0]?.message ?? "not YAML");
  const style = styleOf(text);
  const root = first.contents;
  if (isMap(root)) return [{ doc: first, text, style }];
  if (isSeq(root)) {
    return root.items.map((item) => {
      const d = new Document(null, { schema: "failsafe" });
      d.contents = item as Node;
      return { doc: d, style };
    });
  }
  throw new Error("want a target or a list of targets");
}

/** The pattern of a path: list indexes as [] (subscribers[].ki). */
function patternOf(steps: readonly Step[]): string {
  let out = "";
  for (const s of steps) {
    if (typeof s === "number") out += "[]";
    else out += (out === "" ? "" : ".") + s;
  }
  return out;
}

/** The canonical path of steps in doc: named-list elements by name (subscribers[main].ki), others by index. */
export function canonicalPath(doc: Document, steps: readonly Step[]): string {
  let out = "";
  for (let i = 0; i < steps.length; i++) {
    const s = steps[i];
    if (typeof s === "number") {
      const list = patternOf(steps.slice(0, i));
      const name = LISTS.has(list) ? scalarText(doc.getIn([...steps.slice(0, i + 1), "name"], true)) : undefined;
      out += `[${name ?? s}]`;
    } else {
      out += (out === "" ? "" : ".") + s;
    }
  }
  return out;
}

/** A scalar node's text; undefined for anything else, or a YAML null. */
function scalarText(n: unknown): string | undefined {
  if (!isScalar(n)) return undefined;
  if (n.value === null || n.value === undefined) return undefined;
  return String(n.value);
}

/** A mapping key's text: a key node's, or a key set from code as a plain string. */
function keyText(k: unknown): string | undefined {
  return typeof k === "string" ? k : scalarText(k);
}

/** True when a plain scalar's text would be read as a YAML null by the CLI. */
function nullText(s: string): boolean {
  return s === "" || s === "~" || /^(null|Null|NULL)$/.test(s);
}

/** Number text the CLI takes: decimal, or 0x/0o/0b, with no leading zero. */
function numberIssue(n: Scalar): string | undefined {
  const text = String(n.value ?? "");
  if (refName(text) !== undefined || refsIn(text).length > 0) {
    return `${text} is not expanded in numeric fields; set it with a flag or an env-layer binding`;
  }
  const leading = /^0[0-9]+$/.test(text) ? `${text} has a leading zero; write ${text.replace(/^0+/, "") || "0"}, or 0x… for hex` : "";
  if (n.type === Scalar.QUOTE_DOUBLE || n.type === Scalar.QUOTE_SINGLE) {
    return `"${text}" is quoted; write the number unquoted` + (leading ? "; " + leading : "");
  }
  if (leading) return leading;
  if (!/^(0|[1-9][0-9]*|0x[0-9A-Fa-f]+|0o[0-7]+|0b[01]+)$/.test(text)) return `${JSON.stringify(text)}: want a number`;
  return undefined;
}

/**
 * The structure checks a target document gets before anything else: known
 * keys, shapes, numbers, list names, and the document fields (version 1,
 * kind target, a valid name). An empty list is a document that loads.
 */
export function checkTarget(doc: Document): Issue[] {
  const issues: Issue[] = [];
  const at = (steps: readonly Step[], message: string) => issues.push({ path: canonicalPath(doc, steps) || "(document)", message });
  const root = doc.contents;
  if (!isMap(root)) {
    at([], "a target is a mapping");
    return issues;
  }
  const walk = (map: YAMLMap, steps: Step[]) => {
    for (const pair of map.items) {
      const key = keyText(pair.key);
      if (key === undefined) {
        at(steps, "a key that is not a string");
        continue;
      }
      const here = [...steps, key];
      const pat = patternOf(here);
      const v = pair.value;
      if (LISTS.has(pat)) {
        if (!isSeq(v)) {
          at(here, `${pat} is a list`);
          continue;
        }
        const names = new Set<string>();
        v.items.forEach((el, i) => {
          const name = isMap(el) ? scalarText(el.get("name", true)) : undefined;
          if (!name) {
            at([...here, i], `${pat} entries need a name`);
            return;
          }
          if (/[.[\]]/.test(name)) at([...here, i], `${pat} name ${JSON.stringify(name)} may not contain '.', '[' or ']'`);
          if (names.has(name)) at([...here, i], `${pat} entries need distinct names (${JSON.stringify(name)} twice)`);
          names.add(name);
          if (isMap(el)) walk(el, [...here, i]);
        });
        continue;
      }
      const f = FIELDS.get(pat);
      if (!f) {
        if (isMap(v) && [...FIELDS.keys()].some((p) => p.startsWith(pat + "."))) walk(v, here);
        else at(here, `unknown key ${pat}`);
        continue;
      }
      checkField(f, v, here, at);
    }
  };
  walk(root, []);
  const version = scalarText(doc.get("version", true));
  if (version !== "1") at(["version"], version === undefined ? "version is required (1)" : `version ${version}: want 1`);
  const kind = scalarText(doc.get("kind", true));
  if (kind !== "target") at(["kind"], kind === undefined ? "kind is required (target)" : `kind ${JSON.stringify(kind)}: want target`);
  const name = scalarText(doc.get("name", true));
  if (!name) at(["name"], "name is required");
  else if (!VALID_NAME.test(name)) at(["name"], `name ${JSON.stringify(name)}: ${VALID_NAME_TEXT}`);
  const node = scalarText(doc.getIn(["ran", "node"], true));
  if (node) at(["ran", "node"], `ran.node ${JSON.stringify(node)}: leave it empty`);
  return issues;
}

function checkField(f: FieldSpec, v: unknown, here: Step[], at: (s: readonly Step[], m: string) => void) {
  switch (f.type) {
    case "string":
      if (!isScalar(v) || scalarText(v) === undefined) at(here, `${f.path} is a string`);
      return;
    case "uint":
      if (!isScalar(v)) at(here, `${f.path} is a number`);
      else {
        const why = numberIssue(v);
        if (why) at(here, why);
      }
      return;
    case "strings":
      if (isScalar(v)) return;
      if (!isSeq(v)) {
        at(here, `${f.path} is a list of strings`);
        return;
      }
      v.items.forEach((e, i) => {
        if (scalarText(e) === undefined) at([...here, i], `${f.path} entries are strings`);
      });
      return;
    case "slices":
      if (!isSeq(v)) {
        at(here, `${f.path} is a list of {sst, sd}`);
        return;
      }
      v.items.forEach((e, i) => {
        if (!isMap(e)) {
          at([...here, i], "a slice is {sst, sd}");
          return;
        }
        for (const pair of e.items) {
          const k = keyText(pair.key);
          if (k === "sst") {
            const why = isScalar(pair.value) ? numberIssue(pair.value) : "sst is a number";
            if (why) at([...here, i, "sst"], "slice sst: " + why);
          } else if (k === "sd") {
            if (scalarText(pair.value) === undefined) at([...here, i, "sd"], "slice sd is a string");
          } else {
            at([...here, i], `unknown key ${f.path}[].${k ?? "?"}`);
          }
        }
      });
      return;
  }
}

/** The value at steps as text, a list of texts, or undefined when absent. */
export function getValue(doc: Document, steps: readonly Step[]): string | string[] | undefined {
  const n = doc.getIn(steps, true);
  if (isSeq(n)) return n.items.map((e) => scalarText(e) ?? "");
  return scalarText(n);
}

/** A scalar for text, quoted where the CLI would otherwise read a null. */
function scalar(text: string): Scalar {
  const s = new Scalar(text);
  if (nullText(text)) s.type = Scalar.QUOTE_DOUBLE;
  return s;
}

/**
 * Sets the value at steps; an empty value (or list) removes the key, and a
 * mapping left empty goes with it. An existing scalar keeps its quoting.
 */
export function setValue(doc: Document, steps: readonly Step[], value: string | readonly string[]): void {
  if (value === "" || (Array.isArray(value) && value.length === 0)) {
    removeValue(doc, steps);
    return;
  }
  if (typeof value === "string") {
    const cur = doc.getIn(steps, true);
    if (isScalar(cur)) {
      cur.value = value;
      if (nullText(value)) cur.type = Scalar.QUOTE_DOUBLE;
      return;
    }
    ensureParents(doc, steps);
    doc.setIn(steps, scalar(value));
    return;
  }
  const seq = new YAMLSeq();
  for (const v of value) seq.items.push(scalar(v));
  ensureParents(doc, steps);
  doc.setIn(steps, seq);
}

/** Sets the slices list whole: [{sst, sd?}]. */
export function setSlices(doc: Document, steps: readonly Step[], slices: readonly { sst: string; sd?: string }[]): void {
  if (slices.length === 0) {
    removeValue(doc, steps);
    return;
  }
  const seq = new YAMLSeq();
  for (const s of slices) {
    const m = new YAMLMap();
    m.set("sst", new Scalar(s.sst));
    if (s.sd) m.set("sd", scalar(s.sd));
    seq.items.push(m);
  }
  ensureParents(doc, steps);
  doc.setIn(steps, seq);
}

/** The slices at steps, each as {sst, sd}. */
export function getSlices(doc: Document, steps: readonly Step[]): { sst: string; sd: string }[] {
  const n = doc.getIn(steps, true);
  if (!isSeq(n)) return [];
  return n.items.map((e) => ({
    sst: isMap(e) ? (scalarText(e.get("sst", true)) ?? "") : "",
    sd: isMap(e) ? (scalarText(e.get("sd", true)) ?? "") : "",
  }));
}

/** Mappings on the way to steps, created where missing. */
function ensureParents(doc: Document, steps: readonly Step[]) {
  for (let i = 1; i < steps.length; i++) {
    const parent = steps.slice(0, i);
    if (doc.getIn(parent, true) === undefined && typeof steps[i] === "string") doc.setIn(parent, new YAMLMap());
  }
}

/** Removes the value at steps and any mapping it leaves empty. */
export function removeValue(doc: Document, steps: readonly Step[]): void {
  if (doc.getIn(steps, true) === undefined) return;
  doc.deleteIn(steps);
  for (let i = steps.length - 1; i > 0; i--) {
    const parent = doc.getIn(steps.slice(0, i), true);
    if (isMap(parent) && parent.items.length === 0) doc.deleteIn(steps.slice(0, i));
    else break;
  }
}

/** Adds an element to a named list, named name, and returns its index. */
export function addElement(doc: Document, list: readonly Step[], name: string): number {
  const cur = doc.getIn(list, true);
  const el = new YAMLMap();
  el.set("name", scalar(name));
  if (isSeq(cur)) {
    cur.items.push(el);
    return cur.items.length - 1;
  }
  const seq = new YAMLSeq();
  seq.items.push(el);
  ensureParents(doc, list);
  doc.setIn(list, seq);
  return 0;
}

/** Removes element i of a list, and the list when it is left empty. */
export function removeElement(doc: Document, list: readonly Step[], i: number): void {
  const cur = doc.getIn(list, true);
  if (!isSeq(cur)) return;
  cur.items.splice(i, 1);
  if (cur.items.length === 0) removeValue(doc, list);
}

/** How many elements the list at steps holds. */
export function listLength(doc: Document, steps: readonly Step[]): number {
  const cur = doc.getIn(steps, true);
  return isSeq(cur) ? cur.items.length : 0;
}

/** One secret field of a document: where it is and what it holds. */
export interface SecretSlot {
  steps: Step[];
  path: string;
  /** subscribers[].ki, subscribers[].opc or app.responders[].token. */
  pattern: string;
  value: string;
}

/** The secret fields a document sets, in document order. */
export function secretSlots(doc: Document): SecretSlot[] {
  const out: SecretSlot[] = [];
  for (const f of TARGET_FIELDS) {
    if (!f.secret) continue;
    const [list, key] = f.path.split("[].") as [string, string];
    const steps: Step[] = list.split(".");
    const n = listLength(doc, steps);
    for (let i = 0; i < n; i++) {
      const at = [...steps, i, key];
      const v = scalarText(doc.getIn(at, true));
      if (v !== undefined && v !== "") out.push({ steps: at, path: canonicalPath(doc, at), pattern: f.path, value: v });
    }
  }
  return out;
}

/** The variable a secret slot gets when its literal is moved out: init's names first. */
function varFor(slot: SecretSlot, taken: ReadonlySet<string>): string {
  const i = slot.steps[slot.steps.length - 2] as number;
  const base =
    slot.pattern === "subscribers[].ki" ? "ORBIT_KI" : slot.pattern === "subscribers[].opc" ? "ORBIT_OPC" : "ORBIT_RESPONDER_TOKEN";
  const first = base === "ORBIT_RESPONDER_TOKEN" ? `${base}_${i}` : i === 0 ? base : `${base}_${i}`;
  if (!taken.has(first)) return first;
  for (let n = i + 1; ; n++) {
    const v = `${base}_${n}`;
    if (!taken.has(v)) return v;
  }
}

/** A literal secret moved out of a document: where it was, and the variable that now stands for it. */
export interface MovedSecret {
  path: string;
  variable: string;
  value: string;
}

/**
 * Replaces every literal secret in doc (a key or token written in clear) by
 * a ${VAR} reference, and returns what it moved, so the caller keeps the
 * values in the tab. A value that is already exactly ${VAR} stays.
 */
export function moveSecrets(doc: Document): MovedSecret[] {
  const slots = secretSlots(doc);
  const taken = new Set<string>();
  for (const s of slots) for (const r of refsIn(s.value)) taken.add(r);
  const moved: MovedSecret[] = [];
  for (const s of slots) {
    if (refName(s.value) !== undefined) continue;
    const variable = varFor(s, taken);
    taken.add(variable);
    setValue(doc, s.steps, "${" + variable + "}");
    moved.push({ path: s.path, variable, value: s.value });
  }
  return moved;
}

/** Every ${VAR} a document names, outside the secrets too, in document order. */
export function documentRefs(doc: Document): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  const visit = (n: unknown) => {
    if (isScalar(n)) {
      for (const r of refsIn(String(n.value ?? ""))) if (!seen.has(r)) seen.add(r), out.push(r);
    } else if (isMap(n)) for (const p of n.items) visit(p.value);
    else if (isSeq(n)) for (const e of n.items) visit(e);
  };
  visit(doc.contents);
  return out;
}

/**
 * The file text to export. By default secrets stay as their ${VAR}
 * references, as `orbit target init` writes without --write-keys. With
 * keys, each secret that is exactly ${VAR} and has a value in values is
 * written in clear.
 */
export function exportText(doc: Document, style: WriteStyle, opts: { keys?: Readonly<Record<string, string>> } = {}): string {
  if (!opts.keys) return render(doc, style);
  const copy = doc.clone();
  for (const s of secretSlots(copy)) {
    const v = refName(s.value);
    const lit = v === undefined ? undefined : opts.keys[v];
    if (lit !== undefined) setValue(copy, s.steps, lit);
  }
  return render(copy, style);
}

/** A file name for a target's export: its name, which the name rule keeps file-safe, or undefined when it is not valid. */
export function exportFileName(name: string): string | undefined {
  return VALID_NAME.test(name) ? `${name}.yaml` : undefined;
}

/** The defaults a target falls back to (target.DefaultInput), by path. */
export function defaultFor(path: string): unknown {
  return DEFAULTS[path];
}

/** A new target document, as `orbit target init` starts one. */
export function newTarget(name: string): Document {
  const doc = new Document(null, { schema: "failsafe" });
  const m = new YAMLMap();
  m.set("version", new Scalar("1"));
  m.set("kind", new Scalar("target"));
  m.set("name", scalar(name));
  doc.contents = m;
  return doc;
}

/** The target's name as the document has it. */
export function nameOf(doc: Document): string {
  return scalarText(doc.get("name", true)) ?? "";
}
