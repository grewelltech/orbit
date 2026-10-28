/**
 * What the Launch page sends: the stored target's text and the run file's,
 * as the server parses files (RunService.PlanRun), with the value of each
 * ${VAR} they name from this tab, and the keyless RenderRun for the
 * command and the file to save. The browser has no environment: a ${VAR}
 * with no value in the tab is not sent, it is asked for.
 */
import type { MessageInitShape } from "@bufbuild/protobuf";
import type { ResolveRequestSchema } from "@/gen/orbit/v1/target_pb";
import type { RenderRunRequestSchema } from "@/gen/orbit/v1/run_pb";
import type { KeyValues } from "../targets/store";
import { isScalar, parseDocument, visit } from "yaml";
import { keysBlocked } from "../targets/probe";
import { parseTargets, refName, refsIn, secretSlots } from "../targets/yamlio";

export type PlanRequest = MessageInitShape<typeof ResolveRequestSchema>;
export type RenderRequest = MessageInitShape<typeof RenderRunRequestSchema>;

/** What the page has to send a run from. */
export interface LaunchInput {
  /** The stored target's text: its keys are ${VAR}s (U1 keeps them in the tab). */
  targetName: string;
  targetYaml: string;
  /** The run file as the page writes it. */
  runYaml: string;
  /** The template the run file started from, "" for none. */
  template: string;
  /** This tab's values: the target's keys and any ${VAR} the page asked for. */
  values: Readonly<KeyValues>;
}

/**
 * Every ${VAR} the target's and the run file's values name, first seen
 * first: values only, as the CLI expands them, so one in a comment or a
 * key is no variable.
 */
export function refsOf(input: Pick<LaunchInput, "targetYaml" | "runYaml">): string[] {
  const out: string[] = [];
  for (const text of [input.targetYaml, input.runYaml]) {
    visit(parseDocument(text, { schema: "failsafe" }), {
      Pair(_, pair) {
        const v = pair.value;
        if (isScalar(v)) for (const r of refsIn(String(v.value ?? ""))) if (!out.includes(r)) out.push(r);
      },
      Seq(_, seq) {
        for (const e of seq.items) if (isScalar(e)) for (const r of refsIn(String(e.value ?? ""))) if (!out.includes(r)) out.push(r);
      },
    });
  }
  return out;
}

/**
 * Whether a request built from input carries a secret, as the server's
 * check reads it: any ${VAR} value (each is marked secret), or a key
 * written out in the target's secret slots. A run file holds none (the
 * gate refuses one), and the page sends no --set.
 */
export function carriesSecrets(input: Pick<LaunchInput, "targetYaml" | "runYaml">): boolean {
  if (refsOf(input).length > 0) return true;
  const doc = parseDocument(input.targetYaml, { schema: "failsafe" });
  return secretSlots(doc).some((s) => s.value !== "" && refName(s.value) === undefined);
}

/**
 * Why this page may not send input's run (Start, check the plan), or
 * undefined: keysBlocked's rule (loopback, or https with the API token),
 * but only for a request that carries a secret. A keyless run (a target
 * made from an AMF and a PLMN, say) goes from any dashboard, as the old
 * conformance page's did; the server's own check stays the real one.
 */
export function startBlocked(loc: { protocol: string; hostname: string }, token: string, input: Pick<LaunchInput, "targetYaml" | "runYaml">): string | undefined {
  return carriesSecrets(input) ? keysBlocked(loc, token) : undefined;
}

/** The line over Launch's ${VAR} prompt: where the values are kept, as the target's keys are. */
export function varsPromptNote(remembered: boolean): string {
  return `the target and run file name these; their values are ${remembered ? "remembered in this browser" : "held in this tab"}:`;
}

/** What decides whether Launch may send its run (Start, check the plan). */
export interface StartState {
  loc: { protocol: string; hostname: string };
  token: string;
  hasTarget: boolean;
  /** The target's key ${VAR}s with no value in the tab (set on the Targets page). */
  missingKeys: readonly string[];
  /** Any other ${VAR} with no value (asked for on this page). */
  unsetVars: readonly string[];
  workloads: number;
  /** Whether the request carries a secret (carriesSecrets): only then does keysBlocked's rule apply. */
  carriesSecrets: boolean;
}

/**
 * Why Launch may not send its run, or undefined: a target first; then,
 * for a run that carries a secret, keysBlocked's rule (the server's own
 * host, or https with the API token); then every ${VAR} with a value;
 * then a workload. A keyless run goes from any dashboard (startBlocked).
 */
export function whyNotStart(s: StartState): string | undefined {
  if (!s.hasTarget) return "make a target first: new from AMF + PLMN above, or on the Targets page";
  const blocked = s.carriesSecrets ? keysBlocked(s.loc, s.token) : undefined;
  if (blocked) return blocked;
  if (s.missingKeys.length > 0) return `set ${s.missingKeys.map((v) => `\${${v}}`).join(", ")} on the Targets page first`;
  if (s.unsetVars.length > 0) return `set ${s.unsetVars.map((v) => `\${${v}}`).join(", ")} first: the browser has no environment to take them from`;
  if (s.workloads === 0) return "add a workload";
  return undefined;
}

/** What Launch has when it decides whether its run may start (startStateFor). */
export interface StartInput {
  /** The picked target's document, as stored; undefined for none. */
  targetYaml: string | undefined;
  runYaml: string;
  /** Every ${VAR} the target and run file name (refsOf). */
  refs: readonly string[];
  values: Readonly<Record<string, string>>;
  workloads: number;
  loc: StartState["loc"];
  token: string;
}

/**
 * The StartState Launch's run is judged by (whyNotStart), and the ${VAR}s
 * the page asks for: a ${VAR} at one of the target's key slots is set on
 * the Targets page (missingKeys when it has no value); any other is asked
 * for here (prompts; unsetVars when it has no value). carriesSecrets is
 * the target and run file as sent.
 */
export function startStateFor(input: StartInput): { state: StartState; prompts: string[] } {
  const target = input.targetYaml;
  let keyRefs = new Set<string>();
  if (target !== undefined) {
    try {
      const [p] = parseTargets(target);
      if (p) keyRefs = new Set(secretSlots(p.doc).map((x) => refName(x.value)).filter((v): v is string => v !== undefined));
    } catch {
      /* a target that does not parse has no key slots to read; PlanRun says what is wrong with it */
    }
  }
  const prompts = input.refs.filter((r) => !keyRefs.has(r));
  return {
    prompts,
    state: {
      loc: input.loc,
      token: input.token,
      hasTarget: target !== undefined,
      missingKeys: input.refs.filter((r) => keyRefs.has(r) && !input.values[r]),
      unsetVars: prompts.filter((r) => !input.values[r]),
      workloads: input.workloads,
      carriesSecrets: target !== undefined && carriesSecrets({ targetYaml: target, runYaml: input.runYaml }),
    },
  };
}

/** The PlanRun request, or the ${VAR}s it still needs a value for. */
export function planRequest(input: LaunchInput): { request: PlanRequest; unresolved: [] } | { request?: undefined; unresolved: string[] } {
  const refs = refsOf(input);
  const unresolved = refs.filter((r) => !input.values[r]);
  if (unresolved.length > 0) return { unresolved };
  return {
    request: {
      targetYaml: input.targetYaml,
      runYaml: input.runYaml,
      template: input.template,
      vars: refs.map((name) => ({ name, value: input.values[name] ?? "" })),
    },
    unresolved: [],
  };
}

/**
 * The RenderRun request: keyless, so always sent. The target goes as the
 * page keeps it (its keys ${VAR}s, no values), so the server can tell
 * whether a template's fill applies (the fleet's latency probe).
 */
export function renderRequest(input: Pick<LaunchInput, "targetName" | "targetYaml" | "runYaml" | "template">): RenderRequest {
  return { runYaml: input.runYaml, targetName: input.targetName, template: input.template, targetYaml: input.targetYaml };
}

/**
 * The form state a plan is for: Start sends a plan only when it was
 * resolved from exactly what the page shows now (target, run file,
 * template and the values sent), else it resolves again first. Held in
 * memory only, as the plan is.
 */
export function formKey(input: LaunchInput): string {
  const vals = refsOf(input).map((r) => [r, input.values[r] ?? ""]);
  return JSON.stringify([input.targetName, input.targetYaml, input.runYaml, input.template, vals]);
}

/** A canonical path, as the server's refusals name a field. */
const PATH = String.raw`[a-z_][\w.[\]-]*`;

/**
 * Where a refusal points, from the server's InvalidArgument: when parsing,
 * "invalid run: <file>:<line>: <path>: <reason>"; when resolving, "invalid
 * run: <path>: <reason> (<layer> <file>:<line>)". The documents go by the
 * files they would be (lab1.yaml, lab1.run.yaml). Undefined when the
 * message names no document or field.
 */
export function refusedAt(message: string): { doc: "target" | "run file"; line?: number; path?: string; reason: string } | undefined {
  const docOf = (file: string) => (file.endsWith(".run.yaml") || file === "run.yaml" ? "run file" : "target");
  const parsed = new RegExp(`^invalid run: ([\\w.-]+\\.yaml):(\\d+): (?:(${PATH}): )?(.*)$`, "s").exec(message);
  if (parsed) return { doc: docOf(parsed[1] ?? ""), line: Number(parsed[2]), path: parsed[3], reason: parsed[4] ?? "" };
  const resolved = new RegExp(`^invalid run: (${PATH}): (.*?)(?: \\((?:default|target|run file|env|flag) ([\\w.-]+\\.yaml):(\\d+)\\))?$`, "s").exec(message);
  if (resolved) {
    const path = resolved[1] ?? "";
    const file = resolved[3];
    const doc = file ? docOf(file) : /^(workloads|gates|until|run)[.[]/.test(path) ? "run file" : "target";
    return { doc, line: resolved[4] ? Number(resolved[4]) : undefined, path, reason: resolved[2] ?? "" };
  }
  return undefined;
}
