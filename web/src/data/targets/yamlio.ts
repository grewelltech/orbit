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
import { Document, isAlias, isMap, isPair, isScalar, isSeq, parseAllDocuments, Scalar, visit, YAMLMap, YAMLSeq, type Node } from "yaml";
import { DEFAULTS, NAMED_LISTS, SLICE_KEYS, TARGET_FIELDS, VALID_NAME, type FieldSpec } from "./schema.gen";

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

/** The largest file the page reads (1 MiB): a target file is a few KiB. */
export const MAX_FILE_BYTES = 1024 * 1024;

/**
 * Reads a target file: one target, or a list of them, as LoadFile takes.
 * A YAML syntax error fails the whole file; its message names the line
 * and column only, never quotes the text (which may hold a key).
 *
 * Anchors (&name), aliases (*name) and merge keys (<<) are refused. The
 * CLI resolves them, so a key can reach a field through one; this page
 * checks and moves keys field by field, and refuses what it cannot see.
 * `orbit target init` never writes them.
 */
export function parseTargets(text: string): ParsedTarget[] {
  if (utf8Length(text) > MAX_FILE_BYTES) throw new Error(TOO_LARGE);
  const docs = parseAllDocuments(text, { schema: "failsafe", prettyErrors: false });
  if (!Array.isArray(docs)) throw new Error("empty");
  const first = docs[0];
  if (!first || docs.length > 1) throw new Error(docs.length > 1 ? "one YAML document per file" : "empty");
  const bad = first.errors[0];
  if (bad) {
    const at = bad.pos?.[0] ?? 0;
    const before = text.slice(0, at).split("\n");
    throw new Error(`YAML syntax error at line ${before.length}, column ${(before[before.length - 1]?.length ?? 0) + 1}`);
  }
  if (sharesNodes(first)) throw new Error(SHARED_REFUSED);
  // Layer zero, the structure (structureProblem), refuses everything the
  // other two do and more: they are kept as defence in depth, each tested
  // on its own, though no input is known to get past layer zero to them.
  const root = first.contents;
  for (const t of isSeq(root) ? root.items : isMap(root) ? [root] : []) {
    const why = structureProblem(t) ?? shapeProblem(t) ?? strayKey(t);
    if (why) throw new Error(why);
  }
  const style = styleOf(text);
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

/** Why a file over MAX_FILE_BYTES is refused. */
export const TOO_LARGE = "larger than 1 MiB; a target file is a few KiB";

/** A text's size in UTF-8 bytes, as a file's on disk. */
export function utf8Length(text: string): number {
  return new TextEncoder().encode(text).length;
}

/** Every mapping a target document has, by pattern: the parents of its fields. */
const MAPPINGS: ReadonlySet<string> = new Set(
  TARGET_FIELDS.flatMap((f) => {
    const out: string[] = [];
    const parts = f.path.split(".");
    for (let i = 1; i < parts.length; i++) {
      const p = parts.slice(0, i).join(".");
      if (!p.endsWith("[]")) out.push(p);
    }
    return out;
  }),
);

/**
 * Layer zero, the gate: the document's structure is exactly the target
 * schema's, generated from Go (schema.gen.ts), as the CLI's strict decode
 * (KnownFields) requires. Every mapping key is a plain string; every key
 * is a known field at its path; every field has its kind (a scalar, a
 * mapping, a list); a key field is a scalar. No YAML tags. A value's
 * format (an SD's hex, a missing name) is not checked here: the editor
 * shows those. The reason is built from schema field names and list
 * indexes only, so it never quotes the file.
 */
export function structureProblem(target: unknown): string | undefined {
  const show = (path: readonly Step[]) => path.map((s) => (typeof s === "number" ? `[${s}]` : `.${s}`)).join("").replace(/^\./, "") || "the top";
  const tagged = (n: unknown) => (isScalar(n) || isMap(n) || isSeq(n)) && n.tag !== undefined;
  const plainKey = (k: unknown) => (typeof k === "string" ? k : isScalar(k) && !tagged(k) && typeof k.value === "string" ? k.value : undefined);
  const scalar = (n: unknown) => isScalar(n) && !tagged(n);
  const walk = (map: unknown, pattern: string, path: Step[]): string | undefined => {
    if (!isMap(map) || tagged(map)) return `${show(path)} is not a mapping`;
    for (const pair of map.items) {
      if (tagged(pair.key)) return `a key under ${show(path)} has a YAML tag (!!…); write it plain`;
      const k = plainKey(pair.key);
      if (k === undefined) return `a key under ${show(path)} is not plain text`;
      const pat = pattern ? `${pattern}.${k}` : k;
      const v = pair.value;
      const f = FIELDS.get(pat);
      // A tag, never quoted: a custom one is file text.
      if (tagged(v) && (LISTS.has(pat) || f || MAPPINGS.has(pat))) return `${show([...path, k])} has a YAML tag (!!…); write it plain`;
      if (LISTS.has(pat)) {
        if (!isSeq(v) || tagged(v)) return `${show([...path, k])} is not a list of entries`;
        for (let i = 0; i < v.items.length; i++) {
          const why = walk(v.items[i], pat + "[]", [...path, k, i]);
          if (why) return why;
        }
      } else if (f) {
        const here = show([...path, k]);
        if (f.type === "string" || f.type === "uint") {
          if (!scalar(v)) return `${here} is not a single value`;
        } else if (f.type === "strings") {
          // A list, as the CLI's decode wants: a lone value is refused there.
          if (!(isSeq(v) && !tagged(v) && v.items.every(scalar))) return `${here} is not a list of values (write "- value")`;
        } else if (f.type === "slices") {
          if (!isSeq(v) || tagged(v)) return `${here} is not a list of slices`;
          for (let i = 0; i < v.items.length; i++) {
            const e = v.items[i];
            if (!isMap(e) || tagged(e)) return `${here}[${i}] is not a slice ({sst, sd})`;
            for (const sp of e.items) {
              const sk = plainKey(sp.key);
              if (sk === undefined || !SLICE_KEYS.includes(sk)) return `an unknown field under ${here}[${i}]`;
              if (!scalar(sp.value)) return `${here}[${i}].${sk} is not a single value`;
            }
          }
        }
      } else if (MAPPINGS.has(pat)) {
        const why = walk(v, pat, [...path, k]);
        if (why) return why;
      } else {
        return `an unknown field under ${show(path)}`;
      }
    }
    return undefined;
  };
  return walk(target, "", []);
}

/** The lists whose elements hold keys, and the key fields each element has. */
const KEY_LISTS: readonly { list: readonly string[]; keys: readonly string[] }[] = [
  { list: ["subscribers"], keys: ["ki", "opc"] },
  { list: ["app", "responders"], keys: ["token"] },
];

/**
 * Layer one: a list that holds keys (subscribers, app.responders), when
 * present, is a sequence of mappings. Any other shape (a forgotten "-", a
 * list in a list) would hide its keys from secretSlots.
 */
export function shapeProblem(target: unknown): string | undefined {
  if (!isMap(target)) return undefined;
  for (const { list } of KEY_LISTS) {
    const parent = list.length > 1 ? target.getIn(list.slice(0, -1), true) : target;
    if (parent !== undefined && !isMap(parent)) return `${list.slice(0, -1).join(".")} is not a mapping; it holds ${list.join(".")}`;
    const n = target.getIn(list, true);
    if (n === undefined || (isScalar(n) && (n.value === null || n.value === ""))) continue;
    if (!isSeq(n) || n.items.some((e) => !isMap(e))) return `${list.join(".")} is not a list of entries (each "- name: …"); it holds keys, so it is refused as it is`;
  }
  return undefined;
}

/**
 * Layer two, the backstop: any key named ki, opc or token, at any depth,
 * whose value is not exactly a ${VAR} and which is not one of the fields
 * secretSlots knows (subscribers[i].ki/opc, app.responders[i].token). The
 * reason names the path, never the value.
 */
export function strayKey(target: unknown): string | undefined {
  let found: string | undefined;
  const known = (path: readonly Step[], key: string) =>
    KEY_LISTS.some(
      ({ list, keys }) => keys.includes(key) && path.length === list.length + 1 && list.every((k, i) => path[i] === k) && typeof path[list.length] === "number",
    );
  // A path as a notice shows it: list indexes in brackets, and any key
  // that is not shaped like a field name as "…", so a key written as a
  // mapping key is never quoted.
  const show = (path: readonly Step[]) =>
    path
      .map((s) => (typeof s === "number" ? `[${s}]` : /^[a-z_<][a-z0-9_<]{0,31}$/.test(s) ? s : "…"))
      .join(".")
      .replace(/\.\[/g, "[");
  const walk = (n: unknown, path: Step[]) => {
    if (found) return;
    if (isMap(n)) {
      for (const pair of n.items) {
        const k = keyText(pair.key);
        if (k === undefined) {
          found = `a mapping key at ${show(path) || "the top"} is not text; refused`;
          return;
        }
        const v = pair.value;
        if (/^(ki|opc|token)$/.test(k)) {
          const here = show([...path, k]);
          // A key's value is one scalar, wherever it is: a list or a
          // mapping there would hide a key from every other check.
          if (v !== null && v !== undefined && !isScalar(v)) {
            found = `the ${k} at ${here} is not a single value; refused so it cannot be kept in clear`;
            return;
          }
          const text = isScalar(v) ? String(v.value ?? "") : "";
          if (!known(path, k) && text !== "" && refName(text) === undefined) {
            found = `a ${k} at ${here} is outside the fields that hold keys; refused so it cannot be kept in clear`;
            return;
          }
        }
        walk(v, [...path, k]);
      }
    } else if (isSeq(n)) n.items.forEach((e, i) => walk(e, [...path, i]));
  };
  walk(target, []);
  return found;
}

/** Why a file with an anchor, alias or merge key is refused. */
export const SHARED_REFUSED = "anchors, aliases and merge keys are not supported: write the value out";

/** Whether doc has an anchor (&a), an alias (*a) or a merge key (<<) anywhere. */
export function sharesNodes(doc: Document): boolean {
  let found = false;
  visit(doc, (_, n) => {
    if (isAlias(n) || ((isScalar(n) || isMap(n) || isSeq(n)) && n.anchor) || (isPair(n) && keyText(n.key) === "<<")) {
      found = true;
      return visit.BREAK;
    }
    return undefined;
  });
  return found;
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

/**
 * The canonical path of steps in doc: named-list elements by name
 * (subscribers[main].ki), others by index; so is an element whose name is
 * not a plain selector (with '.', '[' or ']') or is shared with another
 * entry, so a name refused is never quoted in a path.
 */
export function canonicalPath(doc: Document, steps: readonly Step[]): string {
  let out = "";
  for (let i = 0; i < steps.length; i++) {
    const s = steps[i];
    if (typeof s === "number") {
      const list = patternOf(steps.slice(0, i));
      let name = LISTS.has(list) ? scalarText(doc.getIn([...steps.slice(0, i + 1), "name"], true)) : undefined;
      if (name !== undefined) {
        const all = doc.getIn(steps.slice(0, i), true);
        const shared = isSeq(all) && all.items.filter((el) => isMap(el) && scalarText(el.get("name", true)) === name).length > 1;
        if (shared || /[.[\]]/.test(name)) name = undefined;
      }
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
        // A name refused is told by its entry's index, never quoted: a key
        // typed as a name stays out of the problem list (the field itself
        // shows what was typed).
        const names = new Map<string, number>();
        v.items.forEach((el, i) => {
          const name = isMap(el) ? scalarText(el.get("name", true)) : undefined;
          if (!name) {
            at([...here, i], `${pat} entries need a name`);
            return;
          }
          if (/[.[\]]/.test(name)) at([...here, i], `${pat}[${i}].name may not contain '.', '[' or ']'`);
          const first = names.get(name);
          if (first !== undefined) at([...here, i], `${pat}[${i}].name is the same as ${pat}[${first}].name; entries need distinct names`);
          else names.set(name, i);
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
