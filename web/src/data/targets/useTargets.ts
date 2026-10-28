/**
 * The Targets page's state: the stored targets, the one being edited, and
 * its keys, kept in step with storage and with other tabs. Every change is
 * one of the functions in actions.ts; this hook only holds their results.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import type { Document } from "yaml";
import * as act from "./actions";
import { browserLocal, browserSession, keysFor, loadTargets, remembers, storageChange, type KeyValueStore, type KeyValues, type StoredTarget } from "./store";
import { checkTarget, refName, setValue, type Issue, type Step, type WriteStyle } from "./yamlio";

export type { ExportFile } from "./actions";

/** The target being edited: its document and what the checks say. */
export interface Editing {
  name: string;
  doc: Document;
  style: WriteStyle;
  issues: Issue[];
  keys: KeyValues;
  remembered: boolean;
}

/** Builds the page's state over the given stores (the browser's by default). */
export function useTargets(local: KeyValueStore | undefined = browserLocal(), session: KeyValueStore | undefined = browserSession()) {
  const stores = useMemo<act.Stores>(() => ({ local, session }), [local, session]);
  const [targets, setTargets] = useState<StoredTarget[]>(() => loadTargets(local));
  const [selected, setSelected] = useState<string | undefined>(() => targets[0]?.name);
  const [keysVersion, setKeysVersion] = useState(0);
  const [notices, setNotices] = useState<string[]>([]);
  const latest = useRef(targets);
  latest.current = targets;

  // Another tab's changes: the targets blob (already cleaned of keys) and
  // remembered keys. Nothing from there reaches this tab's session keys;
  // a target deleted there loses its keys here.
  useEffect(() => {
    const on = (ev: StorageEvent) => {
      const c = storageChange(ev, local);
      if (c?.kind === "targets") {
        act.targetsFromElsewhere(stores, latest.current, c.targets);
        setTargets(c.targets);
        setKeysVersion((v) => v + 1);
      } else if (c?.kind === "keys") setKeysVersion((v) => v + 1);
    };
    window.addEventListener("storage", on);
    return () => window.removeEventListener("storage", on);
  }, [local, stores]);

  const current = targets.find((t) => t.name === selected) ?? targets[0];
  const editing = useMemo<Editing | undefined>(() => {
    const p = current && act.parseStored(current);
    if (!current || !p) return undefined;
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

  const took = useCallback((r: act.Edited | undefined) => {
    if (!r) return;
    setTargets(r.targets);
    setSelected(r.name);
    setKeysVersion((v) => v + 1);
  }, []);

  const edit = useCallback(
    (change: (doc: Document) => void) => editing && took(act.editTarget(stores, targets, editing.name, change)),
    [editing, stores, targets, took],
  );
  const setField = useCallback((steps: readonly Step[], value: string | readonly string[]) => edit((d) => setValue(d, steps, value)), [edit]);

  const setKey = useCallback(
    (variable: string, value: string) => {
      if (!editing) return;
      act.setKey(stores, editing.name, variable, value);
      setKeysVersion((v) => v + 1);
    },
    [editing, stores],
  );

  /** A secret slot's value typed in the editor: held as its ${VAR}'s key, or moved out as one. */
  const setSecret = useCallback(
    (steps: readonly Step[], cur: string, value: string) => {
      const v = refName(cur);
      if (v !== undefined) setKey(v, value);
      else edit((d) => setValue(d, steps, value));
    },
    [edit, setKey],
  );

  const remember = useCallback(
    (on: boolean) => {
      if (!editing) return;
      act.setRemember(stores, editing.name, on, (w) => window.confirm(w));
      setKeysVersion((v) => v + 1);
    },
    [editing, stores],
  );

  return {
    targets,
    editing,
    notices,
    dismissNotices: () => setNotices([]),
    /** Shows notices of the page's own (a file refused before it is read). */
    notify: (n: string[]) => setNotices(n),
    select: setSelected,
    create: () => took(act.createTarget(stores, targets)),
    duplicate: () => editing && took(act.duplicateTarget(stores, targets, editing.name)),
    remove: (name: string) => {
      const next = act.removeTarget(stores, targets, name);
      setTargets(next);
      setSelected(next[0]?.name);
      setKeysVersion((v) => v + 1);
    },
    edit,
    setField,
    setKey,
    setSecret,
    remember,
    forget: () => remember(false),
    importText: (text: string, replace: boolean) => {
      const r = act.importInto(stores, targets, text, replace);
      setTargets(r.targets);
      if (r.last) setSelected(r.last);
      setKeysVersion((v) => v + 1);
      setNotices(r.notices);
    },
    exportCurrent: (withKeys: boolean): act.ExportOnce =>
      editing ? act.exportOnce(stores, targets, editing.name, withKeys) : { file: undefined, withKeys: false },
  };
}
