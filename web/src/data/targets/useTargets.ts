/**
 * The Targets page's state: the stored targets, the one being edited, and
 * its keys, kept in step with storage and with other tabs.
 */
import { useCallback, useEffect, useMemo, useState } from "react";
import type { Document } from "yaml";
import { freeName, importFile, movedNotice } from "./importexport";
import {
  browserLocal,
  browserSession,
  forgetKeys,
  keysFor,
  loadTargets,
  rememberKeys,
  remembers,
  renameKeys,
  saveTargets,
  setTabKeys,
  storageChange,
  tabKeys,
  type KeyValueStore,
  type KeyValues,
  type StoredTarget,
} from "./store";
import {
  checkTarget,
  exportFileName,
  exportText,
  moveSecrets,
  nameOf,
  newTarget,
  parseTargets,
  refName,
  render,
  setValue,
  VALID_NAME_TEXT,
  type Issue,
  type Step,
  type WriteStyle,
} from "./yamlio";
import { VALID_NAME } from "./schema.gen";

/** The target being edited: its document and what the checks say. */
export interface Editing {
  name: string;
  doc: Document;
  style: WriteStyle;
  issues: Issue[];
  keys: KeyValues;
  remembered: boolean;
}

/** A file to hand the user. */
export interface ExportFile {
  fileName: string;
  text: string;
}

function parse(t: StoredTarget): { doc: Document; style: WriteStyle } | undefined {
  try {
    const [p] = parseTargets(t.yaml);
    return p ? { doc: p.doc, style: p.style } : undefined;
  } catch {
    return undefined;
  }
}

/** Builds the page's state over the given stores (the browser's by default). */
export function useTargets(local: KeyValueStore | undefined = browserLocal(), session: KeyValueStore | undefined = browserSession()) {
  const [targets, setTargets] = useState<StoredTarget[]>(() => loadTargets(local));
  const [selected, setSelected] = useState<string | undefined>(() => targets[0]?.name);
  const [keysVersion, setKeysVersion] = useState(0);
  const [notices, setNotices] = useState<string[]>([]);

  // Another tab's changes: the targets blob (already cleaned of keys) and
  // remembered keys. Nothing from there reaches this tab's session keys.
  useEffect(() => {
    const on = (ev: StorageEvent) => {
      const c = storageChange(ev, local);
      if (c?.kind === "targets") setTargets(c.targets);
      else if (c?.kind === "keys") setKeysVersion((v) => v + 1);
    };
    window.addEventListener("storage", on);
    return () => window.removeEventListener("storage", on);
  }, [local]);

  const commit = useCallback(
    (next: StoredTarget[]) => {
      setTargets(next);
      saveTargets(local, next);
    },
    [local],
  );

  const current = targets.find((t) => t.name === selected) ?? targets[0];
  const editing = useMemo<Editing | undefined>(() => {
    if (!current) return undefined;
    const p = parse(current);
    if (!p) return undefined;
    void keysVersion;
    return {
      name: current.name,
      doc: p.doc,
      style: p.style,
      issues: checkTarget(p.doc),
      keys: keysFor(local, session, current.name),
      remembered: remembers(local, current.name),
    };
  }, [current, keysVersion, local, session]);

  const names = useMemo(() => new Set(targets.map((t) => t.name)), [targets]);

  /** Applies an edit to the current target's document and stores it. */
  const edit = useCallback(
    (change: (doc: Document) => void) => {
      if (!editing) return;
      change(editing.doc);
      // A key typed into a field is moved out before the text is stored.
      const moved = moveSecrets(editing.doc);
      if (moved.length > 0) {
        setTabKeys(session, editing.name, { ...tabKeys(session, editing.name), ...Object.fromEntries(moved.map((m) => [m.variable, m.value])) });
        setKeysVersion((v) => v + 1);
      }
      let name = nameOf(editing.doc) || editing.name;
      if (name !== editing.name && (names.has(name) || !VALID_NAME.test(name))) name = editing.name;
      if (name !== editing.name) renameKeys(local, session, editing.name, name);
      const yaml = render(editing.doc, editing.style);
      commit(targets.map((t) => (t.name === editing.name ? { name, yaml } : t)));
      setSelected(name);
    },
    [editing, names, targets, commit, local, session],
  );

  const setField = useCallback((steps: readonly Step[], value: string | readonly string[]) => edit((d) => setValue(d, steps, value)), [edit]);

  /** Sets a key for the current target, in this tab (and in the browser when it remembers keys). */
  const setKey = useCallback(
    (variable: string, value: string) => {
      if (!editing) return;
      const tab = { ...tabKeys(session, editing.name) };
      if (value === "") delete tab[variable];
      else tab[variable] = value;
      setTabKeys(session, editing.name, tab);
      if (editing.remembered) rememberKeys(local, editing.name, { ...keysFor(local, session, editing.name) });
      setKeysVersion((v) => v + 1);
    },
    [editing, local, session],
  );

  /** A secret slot's value typed in the editor: held as its ${VAR}'s key. */
  const setSecret = useCallback(
    (steps: readonly Step[], current: string, value: string) => {
      const v = refName(current);
      if (v !== undefined) setKey(v, value);
      else edit((d) => setValue(d, steps, value));
    },
    [edit, setKey],
  );

  const remember = useCallback(
    (on: boolean) => {
      if (!editing) return;
      if (on) rememberKeys(local, editing.name, keysFor(local, session, editing.name));
      else forgetKeys(local, session, editing.name);
      setKeysVersion((v) => v + 1);
    },
    [editing, local, session],
  );

  const forget = useCallback(() => {
    if (!editing) return;
    forgetKeys(local, session, editing.name);
    setKeysVersion((v) => v + 1);
  }, [editing, local, session]);

  const create = useCallback(
    (wanted = "target") => {
      const name = freeName(VALID_NAME.test(wanted) ? wanted : "target", names);
      commit([...targets, { name, yaml: render(newTarget(name)) }]);
      setSelected(name);
    },
    [names, targets, commit],
  );

  const duplicate = useCallback(() => {
    if (!editing) return;
    const name = freeName(editing.name, names);
    const copy = editing.doc.clone();
    setValue(copy, ["name"], name);
    commit([...targets, { name, yaml: render(copy, editing.style) }]);
    setSelected(name);
  }, [editing, names, targets, commit]);

  const remove = useCallback(
    (name: string) => {
      forgetKeys(local, session, name);
      const next = targets.filter((t) => t.name !== name);
      commit(next);
      setSelected(next[0]?.name);
    },
    [targets, commit, local, session],
  );

  /**
   * Imports a file. A clashing name replaces the stored target when replace
   * is set, and is imported under a free name otherwise. Literal keys move
   * to this tab, each with a notice.
   */
  const importText = useCallback(
    (text: string, replace: boolean) => {
      let read;
      try {
        read = importFile(text);
      } catch (e) {
        setNotices([`import: ${e instanceof Error ? e.message : String(e)}`]);
        return;
      }
      let next = [...targets];
      const out: string[] = [];
      let last: string | undefined;
      for (const r of read) {
        let { name, yaml } = r.target;
        if (!VALID_NAME.test(name)) {
          out.push(`import: a target named ${JSON.stringify(name)} was skipped: ${VALID_NAME_TEXT}`);
          continue;
        }
        if (next.some((t) => t.name === name)) {
          if (replace) {
            next = next.filter((t) => t.name !== name);
            forgetKeys(local, session, name);
          } else {
            const free = freeName(name, new Set(next.map((t) => t.name)));
            const p = parse({ name, yaml });
            if (p) {
              setValue(p.doc, ["name"], free);
              yaml = render(p.doc, p.style);
            }
            out.push(`import: ${name} exists; imported as ${free}`);
            name = free;
          }
        }
        if (Object.keys(r.keys).length > 0) setTabKeys(session, name, { ...tabKeys(session, name), ...r.keys });
        out.push(...r.moved.map((m) => `${name}: ${movedNotice(m)}`));
        if (r.issues.length > 0) out.push(`${name}: ${r.issues.length} problem${r.issues.length === 1 ? "" : "s"} to fix in the editor`);
        next.push({ name, yaml });
        last = name;
      }
      commit(next);
      if (last) setSelected(last);
      setKeysVersion((v) => v + 1);
      setNotices(out);
    },
    [targets, commit, local, session],
  );

  /** The current target as a file; keys in clear only when asked. */
  const exportCurrent = useCallback(
    (withKeys: boolean): ExportFile | undefined => {
      if (!editing || !current) return undefined;
      const fileName = exportFileName(editing.name);
      if (!fileName) return undefined;
      const text = withKeys ? exportText(editing.doc, editing.style, { keys: editing.keys }) : current.yaml;
      return { fileName, text };
    },
    [editing, current],
  );

  return {
    targets,
    editing,
    notices,
    dismissNotices: () => setNotices([]),
    select: setSelected,
    create,
    duplicate,
    remove,
    edit,
    setField,
    setKey,
    setSecret,
    remember,
    forget,
    importText,
    exportCurrent,
  };
}
