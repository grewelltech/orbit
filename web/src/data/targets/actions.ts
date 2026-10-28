/**
 * What the Targets page does, as plain functions over its stores: each
 * takes the stored targets and returns the new list (already saved), so
 * the page's hook only holds the result. Testable without a browser.
 */
import type { Document } from "yaml";
import { freeName, importFile, movedNotice } from "./importexport";
import {
  forgetKeys,
  keysFor,
  rememberedKeys,
  rememberKeys,
  remembers,
  renameKeys,
  saveTargets,
  setTabKeys,
  tabKeys,
  type KeyValueStore,
  type KeyValues,
  type StoredTarget,
} from "./store";
import { documentRefs, exportFileName, exportText, moveSecrets, nameOf, newTarget, parseTargets, render, setValue, VALID_NAME_TEXT, type WriteStyle } from "./yamlio";
import { VALID_NAME } from "./schema.gen";

/** The page's two stores: localStorage and sessionStorage, or fakes. */
export interface Stores {
  local: KeyValueStore | undefined;
  session: KeyValueStore | undefined;
}

/** A stored target parsed for editing, or undefined when its text does not parse. */
export function parseStored(t: StoredTarget): { doc: Document; style: WriteStyle } | undefined {
  try {
    const [p] = parseTargets(t.yaml);
    return p ? { doc: p.doc, style: p.style } : undefined;
  } catch {
    return undefined;
  }
}

/** Keeps only the keys a target's document still names, in the tab and (when it remembers them) in the browser. */
function pruneKeys(s: Stores, name: string, doc: Document) {
  const used = new Set(documentRefs(doc));
  const keep = (v: KeyValues) => Object.fromEntries(Object.entries(v).filter(([k]) => used.has(k)));
  setTabKeys(s.session, name, keep(tabKeys(s.session, name)));
  if (remembers(s.local, name)) rememberKeys(s.local, name, keep(rememberedKeys(s.local, name)));
}

/** The result of an edit: the targets, and the name the edited one now has. */
export interface Edited {
  targets: StoredTarget[];
  name: string;
}

/**
 * Applies change to target name's document and stores it. A key typed into
 * a field is moved out to the tab (moveSecrets) before the text is stored;
 * a key no field names any more (its pool or responder removed) is dropped;
 * a rename to a free, valid name takes the keys with it, and any other
 * rename is undone.
 */
export function editTarget(s: Stores, targets: readonly StoredTarget[], name: string, change: (doc: Document) => void): Edited | undefined {
  const cur = targets.find((t) => t.name === name);
  const p = cur && parseStored(cur);
  if (!p) return undefined;
  change(p.doc);
  const moved = moveSecrets(p.doc);
  if (moved.length > 0) setTabKeys(s.session, name, { ...tabKeys(s.session, name), ...Object.fromEntries(moved.map((m) => [m.variable, m.value])) });
  let next = nameOf(p.doc) || name;
  if (next !== name && (targets.some((t) => t.name === next) || !VALID_NAME.test(next))) {
    next = name;
    setValue(p.doc, ["name"], name);
  }
  if (next !== name) renameKeys(s.local, s.session, name, next);
  pruneKeys(s, next, p.doc);
  const out = targets.map((t) => (t.name === name ? { name: next, yaml: render(p.doc, p.style) } : t));
  saveTargets(s.local, out);
  return { targets: out, name: next };
}

/** Sets one key for target name, in the tab, and in the browser when the target remembers its keys. */
export function setKey(s: Stores, name: string, variable: string, value: string): void {
  const tab = { ...tabKeys(s.session, name) };
  if (value === "") delete tab[variable];
  else tab[variable] = value;
  setTabKeys(s.session, name, tab);
  if (remembers(s.local, name)) rememberKeys(s.local, name, keysFor(s.local, s.session, name));
}

/** What the page asks before keys are written where any script on the origin can read them. */
export const REMEMBER_WARNING =
  "Remember this target's keys in this browser? They are stored unencrypted, readable by any script on this page's origin, until you Forget them.";

/**
 * Turns "remember keys in this browser" on, only once confirm (the page's
 * dialog, with REMEMBER_WARNING) says yes: declined, nothing is written.
 * Off is Forget. Returns whether keys are remembered now.
 */
export function setRemember(s: Stores, name: string, on: boolean, confirm: (warning: string) => boolean): boolean {
  if (!on) {
    forgetKeys(s.local, s.session, name);
    return false;
  }
  if (!confirm(REMEMBER_WARNING)) return remembers(s.local, name);
  rememberKeys(s.local, name, keysFor(s.local, s.session, name));
  return true;
}

/** Adds a new target under a free name. */
export function createTarget(s: Stores, targets: readonly StoredTarget[], wanted = "target"): Edited {
  const name = freeName(VALID_NAME.test(wanted) ? wanted : "target", new Set(targets.map((t) => t.name)));
  const out = [...targets, { name, yaml: render(newTarget(name)) }];
  saveTargets(s.local, out);
  return { targets: out, name };
}

/** Adds a copy of target name under a free name; its keys are not copied. */
export function duplicateTarget(s: Stores, targets: readonly StoredTarget[], name: string): Edited | undefined {
  const cur = targets.find((t) => t.name === name);
  const p = cur && parseStored(cur);
  if (!p) return undefined;
  const copy = freeName(name, new Set(targets.map((t) => t.name)));
  setValue(p.doc, ["name"], copy);
  const out = [...targets, { name: copy, yaml: render(p.doc, p.style) }];
  saveTargets(s.local, out);
  return { targets: out, name: copy };
}

/** Removes target name and its keys, from the browser and the tab. */
export function removeTarget(s: Stores, targets: readonly StoredTarget[], name: string): StoredTarget[] {
  forgetKeys(s.local, s.session, name);
  const out = targets.filter((t) => t.name !== name);
  saveTargets(s.local, out);
  return out;
}

/**
 * Another tab changed the targets: a target gone from the new list loses
 * its keys in this tab too, so a target made later under its name does not
 * inherit them.
 */
export function targetsFromElsewhere(s: Stores, before: readonly StoredTarget[], after: readonly StoredTarget[]): void {
  const still = new Set(after.map((t) => t.name));
  for (const t of before) if (!still.has(t.name)) setTabKeys(s.session, t.name, {});
}

/** What an import did. */
export interface Imported {
  targets: StoredTarget[];
  /** The last target imported, to select. */
  last?: string;
  notices: string[];
}

/**
 * Imports a file. A clashing name replaces the stored target when replace
 * is set (its old keys go), and is imported under a free name otherwise.
 * Literal keys move to the tab, each with a notice. A file that does not
 * read is a single notice: its first line, which names a line and column
 * and never quotes the text.
 */
export function importInto(s: Stores, targets: readonly StoredTarget[], text: string, replace: boolean): Imported {
  let read;
  try {
    read = importFile(text);
  } catch (e) {
    const msg = (e instanceof Error ? e.message : String(e)).split("\n")[0] ?? "";
    return { targets: [...targets], notices: [`import: ${msg}`] };
  }
  let out = [...targets];
  const notices: string[] = [];
  let last: string | undefined;
  for (const r of read) {
    let { name, yaml } = r.target;
    if (!VALID_NAME.test(name)) {
      notices.push(`import: a target named ${JSON.stringify(name)} was skipped: ${VALID_NAME_TEXT}`);
      continue;
    }
    if (out.some((t) => t.name === name)) {
      if (replace) {
        out = out.filter((t) => t.name !== name);
        forgetKeys(s.local, s.session, name);
      } else {
        const free = freeName(name, new Set(out.map((t) => t.name)));
        const p = parseStored({ name, yaml });
        if (!p) continue;
        setValue(p.doc, ["name"], free);
        yaml = render(p.doc, p.style);
        notices.push(`import: ${name} exists; imported as ${free}`);
        name = free;
      }
    }
    if (Object.keys(r.keys).length > 0) setTabKeys(s.session, name, { ...tabKeys(s.session, name), ...r.keys });
    notices.push(...r.moved.map((m) => `${name}: ${movedNotice(m)}`));
    if (r.issues.length > 0) notices.push(`${name}: ${r.issues.length} problem${r.issues.length === 1 ? "" : "s"} to fix in the editor`);
    out.push({ name, yaml });
    last = name;
  }
  saveTargets(s.local, out);
  return { targets: out, last, notices };
}

/** A file to hand the user. */
export interface ExportFile {
  fileName: string;
  text: string;
}

/** An export, and the "include keys" choice for the next one: always off again. */
export interface ExportOnce {
  file: ExportFile | undefined;
  withKeys: false;
}

/** exportTarget, and the keys choice reset: keys in clear are asked for each time. */
export function exportOnce(s: Stores, targets: readonly StoredTarget[], name: string, withKeys: boolean): ExportOnce {
  return { file: exportTarget(s, targets, name, withKeys), withKeys: false };
}

/**
 * Target name as a file. Its stored text, so an unedited import comes back
 * as it was, unless keys are asked for: then each ${VAR} with a key held is
 * written in clear.
 */
export function exportTarget(s: Stores, targets: readonly StoredTarget[], name: string, withKeys: boolean): ExportFile | undefined {
  const cur = targets.find((t) => t.name === name);
  const fileName = exportFileName(name);
  if (!cur || !fileName) return undefined;
  if (!withKeys) return { fileName, text: cur.yaml };
  const p = parseStored(cur);
  if (!p) return undefined;
  return { fileName, text: exportText(p.doc, p.style, { keys: keysFor(s.local, s.session, name) }) };
}
