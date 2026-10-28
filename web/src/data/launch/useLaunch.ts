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
import { browserLocal, browserSession, keysFor, loadTargets, remembers, storageChange, type KeyValueStore } from "../targets/store";
import { startFrom } from "./form";
import { LAUNCH_IDLE, LaunchRunner, type LaunchState } from "./launch";
import { formKey, refsOf, type LaunchInput } from "./resolve";
import { parseRun, renderRun } from "./runyaml";

/** The draft run file, kept per tab so a reload keeps the form (never a key: a run file holds none). */
const DRAFT_KEY = "orbit.launch.draft";

interface Draft {
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

export function useLaunch(local: KeyValueStore | undefined = browserLocal(), session: KeyValueStore | undefined = browserSession(), baseUrl?: string) {
  const stores = useMemo<act.Stores>(() => ({ local, session }), [local, session]);
  const [targets, setTargets] = useState(() => loadTargets(local));
  const initial = useMemo(() => loadDraft(session), [session]);
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
    /** Whether the target's values are remembered in this browser (U1's opt-in), not only held in this tab. */
    remembered: target ? (void keysVersion, remembers(local, target.name)) : false,
    /** The ${VAR}s the target and run file name, for the prompt. */
    refs: input ? refsOf(input) : [],
    /** A ${VAR}'s value, held in this tab with the target's keys (U1). */
    setValue: (variable: string, value: string) => {
      if (!target) return;
      act.setKey(stores, target.name, variable, value);
      setKeysVersion((v) => v + 1);
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
