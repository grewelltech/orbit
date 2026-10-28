/**
 * The Launch page's state: the stored target the run goes against (U1's,
 * its keys in this tab), the run document the form edits, and the server's
 * answers (LaunchRunner). The document is the form's state: every edit is
 * made on a copy, which the run-file gate checks before it is kept.
 */
import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { createClient } from "@connectrpc/connect";
import type { Document } from "yaml";
import { RunService } from "@/gen/orbit/v1/run_pb";
import { orbitTransport } from "../transport";
import * as act from "../targets/actions";
import { browserLocal, browserSession, keysFor, loadTargets, storageChange, type KeyValueStore } from "../targets/store";
import { newTarget, render, setSlices, setValue } from "../targets/yamlio";
import { startFrom } from "./form";
import { LAUNCH_IDLE, LaunchRunner, type LaunchState } from "./launch";
import { formKey, refsOf, type LaunchInput } from "./resolve";
import { parseRun, renderRun } from "./runyaml";

/** The draft run file, kept per tab so a reload keeps the form (never a key: a run file holds none). */
export const DRAFT_KEY = "orbit.launch.draft";

export interface Draft {
  target?: string;
  template: string;
  yaml: string;
}

function loadDraft(session: KeyValueStore | undefined): { draft: Draft; doc: Document } {
  try {
    const raw = session?.getItem(DRAFT_KEY);
    if (raw) {
      const d = JSON.parse(raw) as Draft;
      if (typeof d.yaml === "string" && typeof d.template === "string") return { draft: d, doc: parseRun(d.yaml) };
    }
  } catch {
    /* a draft from an older page, or storage unavailable: start afresh */
  }
  return { draft: { template: "load", yaml: "" }, doc: startFrom("load") };
}

function saveDraft(session: KeyValueStore | undefined, d: Draft) {
  try {
    session?.setItem(DRAFT_KEY, JSON.stringify(d));
  } catch {
    /* storage unavailable: the form still works */
  }
}

/** What a new target from the Launch page holds: an AMF and a network identity, no keys. */
export interface QuickTarget {
  name: string;
  amf: string;
  mcc: string;
  mnc: string;
  sst: string;
  sd: string;
}

/**
 * The target a QuickTarget makes: core.amf, the PLMN and one slice. It has
 * no subscriber pool, so a suite run against it SKIPs the checks that need
 * a UE; add a pool on the Targets page for those.
 */
export function quickTargetText(q: QuickTarget): string {
  const d = newTarget(q.name.trim());
  setValue(d, ["core", "amf"], q.amf.trim());
  setValue(d, ["core", "plmn", "mcc"], q.mcc.trim());
  setValue(d, ["core", "plmn", "mnc"], q.mnc.trim());
  if (q.sst.trim()) setSlices(d, ["core", "slices"], [{ sst: q.sst.trim(), sd: q.sd.trim() || undefined }]);
  return render(d);
}

/**
 * The run the page opens on: the tab's draft, unless a preset names
 * another template (?conformance), which starts the form on that one.
 */
export function initialDraft(session: KeyValueStore | undefined, preset: string | undefined): { draft: Draft; doc: Document } {
  const d = loadDraft(session);
  if (preset !== undefined && d.draft.template !== preset) return { draft: { ...d.draft, template: preset, yaml: "" }, doc: startFrom(preset || undefined) };
  return d;
}

export interface LaunchOptions {
  /** A template the page opens on (?conformance opens the suite): the draft is kept only when it started from it. */
  preset?: string;
  baseUrl?: string;
}

export function useLaunch(local: KeyValueStore | undefined = browserLocal(), session: KeyValueStore | undefined = browserSession(), opts: LaunchOptions = {}) {
  const { preset, baseUrl } = opts;
  const stores = useMemo<act.Stores>(() => ({ local, session }), [local, session]);
  const [targets, setTargets] = useState(() => loadTargets(local));
  const initial = useMemo(() => initialDraft(session, preset), [session, preset]);
  const [targetName, setTargetName] = useState<string | undefined>(() => initial.draft.target ?? targets[0]?.name);
  const [template, setTemplate] = useState(initial.draft.template);
  const [doc, setDoc] = useState<Document>(initial.doc);
  const [keysVersion, setKeysVersion] = useState(0);
  const [notice, setNotice] = useState<string | undefined>();
  const [state, setState] = useState<LaunchState>(LAUNCH_IDLE);

  const client = useMemo(() => createClient(RunService, orbitTransport(baseUrl)), [baseUrl]);
  const runner = useMemo(() => new LaunchRunner(client, setState), [client]);
  useEffect(() => () => runner.dispose(), [runner]);

  // Targets and keys changed in another tab (the Targets page).
  useEffect(() => {
    const on = (ev: StorageEvent) => {
      const c = storageChange(ev, local);
      if (c?.kind === "targets") setTargets(c.targets);
      if (c) setKeysVersion((v) => v + 1);
    };
    window.addEventListener("storage", on);
    return () => window.removeEventListener("storage", on);
  }, [local]);

  const target = targets.find((t) => t.name === targetName) ?? targets[0];
  const runYaml = useMemo(() => renderRun(doc), [doc]);
  const values = useMemo(() => {
    void keysVersion;
    return target ? keysFor(local, session, target.name) : {};
  }, [target, keysVersion, local, session]);
  const input = useMemo<LaunchInput | undefined>(
    () => (target ? { targetName: target.name, targetYaml: target.yaml, runYaml, template, values } : undefined),
    [target, runYaml, template, values],
  );
  const latest = useRef(input);
  latest.current = input;

  // Each change: the plan held is for another state now; the command is rendered again.
  const key = input ? formKey(input) : "";
  useEffect(() => {
    runner.invalidate();
    if (!input) return;
    saveDraft(session, { target: input.targetName, template: input.template, yaml: input.runYaml });
    const t = setTimeout(() => void runner.render(input), 150);
    return () => clearTimeout(t);
    // key covers input.
  }, [key, runner]);

  /** Makes change on a copy of the document, kept only when the run-file gate passes it. */
  const edit = useCallback(
    (change: (d: Document) => void) => {
      const next = doc.clone();
      try {
        change(next);
        parseRun(renderRun(next));
      } catch (e) {
        setNotice(`edit not kept: ${e instanceof Error ? e.message : String(e)}`);
        return;
      }
      setNotice(undefined);
      setDoc(next);
    },
    [doc],
  );

  return {
    targets,
    target,
    selectTarget: setTargetName,
    template,
    /** Starts the form over from a template. */
    chooseTemplate: (name: string) => {
      setTemplate(name);
      setDoc(startFrom(name || undefined));
      setNotice(undefined);
    },
    doc,
    runYaml,
    edit,
    /** Replaces the run file with text (an imported file, or the YAML editor): kept when the gate passes it. */
    replaceText: (text: string, fromTemplate = "") => {
      try {
        const d = parseRun(text);
        setDoc(d);
        setTemplate(fromTemplate);
        setNotice(undefined);
        return true;
      } catch (e) {
        setNotice(`run file not read: ${e instanceof Error ? e.message : String(e)}`);
        return false;
      }
    },
    notice,
    notify: setNotice,
    values,
    /** The ${VAR}s the target and run file name, for the prompt. */
    refs: input ? refsOf(input) : [],
    /** A ${VAR}'s value, held in this tab with the target's keys (U1). */
    setValue: (variable: string, value: string) => {
      if (!target) return;
      act.setKey(stores, target.name, variable, value);
      setKeysVersion((v) => v + 1);
    },
    /** Adds a target from an AMF and a network identity (QuickTarget), and selects it; the notices say what happened. */
    addTarget: (q: QuickTarget): string[] => {
      const r = act.importInto(stores, targets, quickTargetText(q), false);
      setTargets(r.targets);
      if (r.last) setTargetName(r.last);
      return r.notices;
    },
    state,
    plan: () => (input ? runner.plan(input) : Promise.resolve(undefined)),
    start: async () => {
      const id = await runner.start(() => latest.current as LaunchInput, state.plan);
      if (id) window.location.assign(`?run=${encodeURIComponent(id)}`);
      return id;
    },
  };
}
