/**
 * Run files (kind: run) as the Launch page reads and writes them: one YAML
 * document, edited in place, its structure checked against the run-file
 * schema generated from Go (runschema.gen.ts) before the page keeps it.
 * The server parses and checks what is sent again, as the CLI parses a
 * file; this gate is the page's own, so a file the page cannot show
 * field by field (an alias, a tag, a key written out) never reaches it.
 */
import { Document, isAlias, isMap, isPair, isScalar, isSeq, parseAllDocuments, visit, type Node } from "yaml";
import { refName, render as renderDoc, utf8Length, MAX_FILE_BYTES, TOO_LARGE, SHARED_REFUSED, type Step, type WriteStyle } from "../targets/yamlio";
import { BEHAVIOR_FIELDS, EXPECTED_ENTRY_KEYS, EXPECTED_KEYS, RUN_FIELDS, RUN_NAMED_LISTS, SECRET_NAMES, START_PHASES, type RunFieldSpec } from "./runschema.gen";

/** How the page writes a run file: the CLI's two-space indents. */
export const RUN_STYLE: WriteStyle = { indent: 2 };

/** The run document as text. */
export function renderRun(doc: Document): string {
  return renderDoc(doc, RUN_STYLE);
}

const FIELDS = new Map<string, RunFieldSpec>(RUN_FIELDS.map((f) => [f.path, f]));
const LISTS = new Set(RUN_NAMED_LISTS);

/** Every mapping a path pattern runs through: the parents of the fields. */
function parentsOf(paths: readonly string[]): Set<string> {
  const out = new Set<string>();
  for (const p of paths) {
    const parts = p.split(".");
    for (let i = 1; i < parts.length; i++) {
      const q = parts.slice(0, i).join(".");
      if (!q.endsWith("[]")) out.add(q);
    }
  }
  return out;
}
const MAPPINGS = parentsOf(RUN_FIELDS.map((f) => f.path));

const BEHAVIORS = new Map(BEHAVIOR_FIELDS.map((f) => [f.path, f]));
const BEHAVIOR_MAPPINGS = parentsOf(BEHAVIOR_FIELDS.map((f) => f.path));
const BEHAVIOR_LISTS = new Set(
  BEHAVIOR_FIELDS.flatMap((f) => {
    const out: string[] = [];
    const parts = f.path.split(".");
    for (let i = 1; i <= parts.length; i++) {
      const q = parts.slice(0, i).join(".");
      if (q.endsWith("[]")) out.push(q.slice(0, -2));
    }
    return out;
  }),
);

/** Splits a name into lower-case words at '-', '_' and a lower-to-upper change, as internal/secrets does. */
function words(name: string): string[] {
  const out: string[] = [];
  let cur = "";
  let prevLowerOrDigit = false;
  for (const ch of name) {
    if (ch === "-" || ch === "_") {
      if (cur) out.push(cur.toLowerCase());
      cur = "";
      prevLowerOrDigit = false;
      continue;
    }
    const upper = ch !== ch.toLowerCase() && ch === ch.toUpperCase();
    if (upper && prevLowerOrDigit && cur) {
      out.push(cur.toLowerCase());
      cur = "";
    }
    cur += ch;
    prevLowerOrDigit = !upper && (ch !== ch.toUpperCase() || /[0-9]/.test(ch));
  }
  if (cur) out.push(cur.toLowerCase());
  return out;
}

const SECRETS = new Set(SECRET_NAMES);

/** Whether a key names a secret (secrets.IsSecretName): the whole name or a word-aligned suffix. */
export function isSecretName(name: string): boolean {
  const w = words(name);
  for (let i = 0; i < w.length; i++) if (SECRETS.has(w.slice(i).join(""))) return true;
  return false;
}

/** A path as a reason shows it, from schema names and indexes only. */
function show(path: readonly Step[]): string {
  return (
    path
      .map((s) => (typeof s === "number" ? `[${s}]` : /^[a-z_][a-z0-9_]{0,31}$/.test(s) ? `.${s}` : ".…"))
      .join("")
      .replace(/^\./, "") || "the top"
  );
}

const tagged = (n: unknown) => (isScalar(n) || isMap(n) || isSeq(n)) && n.tag !== undefined;
const plainKey = (k: unknown) => (typeof k === "string" ? k : isScalar(k) && !tagged(k) && typeof k.value === "string" ? k.value : undefined);
const scalar = (n: unknown) => isScalar(n) && !tagged(n);
const text = (n: unknown) => (isScalar(n) ? String(n.value ?? "") : "");

/**
 * A secret's value, wherever a key names one: empty or exactly a ${VAR}.
 * The page holds keys only in the tab, so one written out is refused.
 */
function secretProblem(key: string, v: unknown, path: readonly Step[]): string | undefined {
  if (!isSecretName(key)) return undefined;
  if (v !== null && v !== undefined && !isScalar(v)) return `the ${show(path)} is not a single value; refused so a secret cannot be kept in clear`;
  const t = text(v);
  if (t !== "" && refName(t) === undefined) return `${show(path)} holds a secret; write it as a \${VAR}`;
  return undefined;
}

/** A mapping's pairs, each key plain text, or why not. */
function pairs(map: unknown, path: readonly Step[]): { key: string; value: unknown }[] | string {
  if (!isMap(map) || tagged(map)) return `${show(path)} is not a mapping`;
  const out: { key: string; value: unknown }[] = [];
  for (const p of map.items) {
    if (tagged(p.key)) return `a key under ${show(path)} has a YAML tag (!!…); write it plain`;
    const k = plainKey(p.key);
    if (k === undefined) return `a key under ${show(path)} is not plain text`;
    if (tagged(p.value)) return `${show([...path, k])} has a YAML tag (!!…); write it plain`;
    out.push({ key: k, value: p.value });
  }
  return out;
}

/** A workload's behaviors: the fields of orbit.v1.Behaviors, as BEHAVIOR_FIELDS has them. */
function behaviorsProblem(node: unknown, path: Step[]): string | undefined {
  const walk = (n: unknown, pattern: string, at: Step[]): string | undefined => {
    const ps = pairs(n, at);
    if (typeof ps === "string") return ps;
    for (const { key, value } of ps) {
      const pat = pattern ? `${pattern}.${key}` : key;
      const here = [...at, key];
      const f = BEHAVIORS.get(pat);
      if (BEHAVIOR_LISTS.has(pat)) {
        if (!isSeq(value)) return `${show(here)} is not a list of entries`;
        for (let i = 0; i < value.items.length; i++) {
          const why = walk(value.items[i], `${pat}[]`, [...here, i]);
          if (why) return why;
        }
      } else if (f) {
        if (f.type === "map") {
          const mp = pairs(value, here);
          if (typeof mp === "string") return mp;
          for (const e of mp) {
            if (!scalar(e.value)) return `${show(here)} values are single values`;
            const why = secretProblem(e.key, e.value, [...here, "…"]);
            if (why) return why;
          }
        } else if (f.type.startsWith("list:")) {
          if (!(isSeq(value) && value.items.every(scalar))) return `${show(here)} is not a list of values`;
        } else {
          if (!scalar(value)) return `${show(here)} is not a single value`;
          if (f.type === "enum" && !(f.values ?? []).includes(text(value))) return `${show(here)} is not one of ${(f.values ?? []).join(", ")}`;
          if (f.secret) {
            const why = secretProblem("token", value, here);
            if (why) return why;
          }
        }
      } else if (BEHAVIOR_MAPPINGS.has(pat)) {
        const why = walk(value, pat, here);
        if (why) return why;
      } else {
        return `an unknown field under ${show(at)}`;
      }
    }
    return undefined;
  };
  // behaviors: {} keeps the UEs idle; a mapping either way.
  return walk(node, "", path);
}

/** A workload's start: {after: <workload>, phase}. */
function startProblem(node: unknown, path: Step[]): string | undefined {
  const ps = pairs(node, path);
  if (typeof ps === "string") return ps;
  for (const { key, value } of ps) {
    if (key !== "after" && key !== "phase") return `an unknown field under ${show(path)}`;
    if (!scalar(value)) return `${show([...path, key])} is not a single value`;
    if (key === "phase" && !START_PHASES.includes(text(value))) return `${show([...path, key])} is not one of ${START_PHASES.join(", ")}`;
  }
  return undefined;
}

/** An inline expected-failures list; a path is refused, since the server reads no file. */
function expectedProblem(node: unknown, path: Step[]): string | undefined {
  if (isScalar(node)) return `${show(path)} names a file; pick the file so the page puts its list inline`;
  const ps = pairs(node, path);
  if (typeof ps === "string") return ps;
  for (const { key, value } of ps) {
    if (!EXPECTED_KEYS.includes(key)) return `an unknown field under ${show(path)}`;
    const here = [...path, key];
    if (key === "entries") {
      if (!isSeq(value)) return `${show(here)} is not a list of entries`;
      for (let i = 0; i < value.items.length; i++) {
        const es = pairs(value.items[i], [...here, i]);
        if (typeof es === "string") return es;
        for (const e of es) {
          if (!EXPECTED_ENTRY_KEYS.includes(e.key)) return `an unknown field under ${show([...here, i])}`;
          if (!scalar(e.value)) return `${show([...here, i, e.key])} is not a single value`;
        }
      }
    } else if (!scalar(value)) return `${show(here)} is not a single value`;
  }
  return undefined;
}

const WHOLE: Record<string, (n: unknown, path: Step[]) => string | undefined> = {
  "workloads[].behaviors": behaviorsProblem,
  "workloads[].start": startProblem,
  "workloads[].conformance.expected": expectedProblem,
};

/**
 * The gate: the document's structure is exactly the run-file schema's.
 * kind is run; every key is a known field at its path, with its kind; a
 * named list's entries are mappings with a name; a whole value (behaviors,
 * start, an inline expected list) has its own fields; no YAML tags; a
 * secret anywhere is a ${VAR}. The reason names schema paths and indexes
 * only, never the file's text.
 */
export function runProblem(root: unknown): string | undefined {
  const walk = (map: unknown, pattern: string, path: Step[]): string | undefined => {
    const ps = pairs(map, path);
    if (typeof ps === "string") return ps;
    for (const { key, value } of ps) {
      const pat = pattern ? `${pattern}.${key}` : key;
      const here = [...path, key];
      if (pattern === "" && (key === "kind" || key === "version")) {
        if (!scalar(value)) return `${key} is not a single value`;
        if (key === "kind" && text(value) !== "run") return "kind is not run: this page reads run files (kind: run)";
        if (key === "version" && text(value) !== "1") return "version is not 1";
        continue;
      }
      if (LISTS.has(pat)) {
        if (!isSeq(value)) return `${show(here)} is not a list of entries`;
        for (let i = 0; i < value.items.length; i++) {
          const el = value.items[i];
          if (!isMap(el) || tagged(el)) return `${show([...here, i])} is not an entry ({name: …})`;
          if (!scalar(el.get("name", true)) || text(el.get("name", true)) === "") return `${show([...here, i])} has no name`;
          const why = walk(el, `${pat}[]`, [...here, i]);
          if (why) return why;
        }
        continue;
      }
      const f = FIELDS.get(pat);
      if (f) {
        if (f.type === "any") {
          const check = WHOLE[pat];
          const why = check ? check(value, here) : `${show(here)} is a field this page does not edit`;
          if (why) return why;
        } else if (f.type === "strings") {
          if (!(scalar(value) || (isSeq(value) && value.items.every(scalar)))) return `${show(here)} is not a list of values`;
        } else if (!scalar(value)) return `${show(here)} is not a single value`;
        const why = secretProblem(key, value, here);
        if (why) return why;
      } else if (MAPPINGS.has(pat)) {
        const why = walk(value, pat, here);
        if (why) return why;
      } else {
        return `an unknown field under ${show(path)}`;
      }
    }
    return undefined;
  };
  if (!isMap(root)) return "a run file is a mapping (kind: run, workloads, …)";
  if (text(root.get("kind", true)) !== "run") return "kind is not run: this page reads run files (kind: run)";
  return walk(root, "", []);
}

/** Whether doc has an anchor, an alias or a merge key anywhere. */
function sharesNodes(doc: Document): boolean {
  let found = false;
  visit(doc, (_, n) => {
    if (isAlias(n) || ((isScalar(n) || isMap(n) || isSeq(n)) && n.anchor) || (isPair(n) && plainKey(n.key) === "<<")) {
      found = true;
      return visit.BREAK;
    }
    return undefined;
  });
  return found;
}

/**
 * Reads a run file as the page keeps it, or throws why not: at most 1 MiB,
 * one document, no YAML syntax error (named by line and column only), no
 * anchors or aliases, and the gate (runProblem).
 */
export function parseRun(source: string): Document {
  if (utf8Length(source) > MAX_FILE_BYTES) throw new Error(TOO_LARGE.replace("a target file", "a run file"));
  const docs = parseAllDocuments(source, { schema: "failsafe", prettyErrors: false });
  if (!Array.isArray(docs) || docs.length === 0) throw new Error("empty");
  if (docs.length > 1) throw new Error("one YAML document per file");
  const doc = docs[0] as Document;
  const bad = doc.errors[0];
  if (bad) {
    const at = bad.pos?.[0] ?? 0;
    const before = source.slice(0, at).split("\n");
    throw new Error(`YAML syntax error at line ${before.length}, column ${(before[before.length - 1]?.length ?? 0) + 1}`);
  }
  if (sharesNodes(doc)) throw new Error(SHARED_REFUSED);
  const why = runProblem(doc.contents as Node);
  if (why) throw new Error(why);
  return doc;
}
