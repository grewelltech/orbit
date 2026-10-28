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
import { refsIn } from "../targets/yamlio";

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
}

/**
 * Why Launch may not send its run, or undefined: a target first; then
 * keysBlocked's rule (the server's own host, or https with the API
 * token), since the run carries the target's keys; then every ${VAR} with
 * a value; then a workload.
 */
export function whyNotStart(s: StartState): string | undefined {
  if (!s.hasTarget) return "make a target first, on the Targets page";
  const blocked = keysBlocked(s.loc, s.token);
  if (blocked) return blocked;
  if (s.missingKeys.length > 0) return `set ${s.missingKeys.map((v) => `\${${v}}`).join(", ")} on the Targets page first`;
  if (s.unsetVars.length > 0) return `set ${s.unsetVars.map((v) => `\${${v}}`).join(", ")} first: the browser has no environment to take them from`;
  if (s.workloads === 0) return "add a workload";
  return undefined;
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
