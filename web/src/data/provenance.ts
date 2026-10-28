/**
 * A run's resolved config as the Resolved config panel shows it: one row
 * per field (its final value, the layer and source that set it), each value
 * a lower layer had and lost struck through under it, and the notices for
 * an env var that beat a file. Read from GetRunConfig (a run, live or
 * past) or PlanRun (Launch, before Start): the same FieldProvenance either
 * way. Values come masked from the server: a key is "<redacted>" in every
 * cell, struck through or not.
 */
import { ProvenanceLayer, type FieldProvenance } from "@/gen/orbit/v1/target_pb";
import type { OverrideNotice } from "@/gen/orbit/v1/run_pb";

/** The mask every secret value arrives as. */
export const MASKED = "<redacted>";

/** A layer as a chip shows it. */
export function layerLabel(l: ProvenanceLayer): string {
  switch (l) {
    case ProvenanceLayer.DEFAULT:
      return "default";
    case ProvenanceLayer.TARGET:
      return "target";
    case ProvenanceLayer.RUN_FILE:
      return "run file";
    case ProvenanceLayer.ENV:
      return "env";
    case ProvenanceLayer.FLAG:
      return "flag";
    case ProvenanceLayer.LEASE:
      return "server";
    default:
      return "?";
  }
}

export interface Struck {
  value: string;
  layer: string;
  source: string;
}

export interface Row {
  path: string;
  value: string;
  layer: string;
  source: string;
  /** The values lower layers set and this one replaced, lowest first. */
  struck: Struck[];
  /** Set by an env var or a flag: the layers a command line brings. */
  commandLine: boolean;
}

/** The panel's rows, in the order the server gives them (by path). */
export function rows(prov: readonly FieldProvenance[]): Row[] {
  return prov.map((p) => {
    const layer = p.source?.layer ?? ProvenanceLayer.UNSPECIFIED;
    return {
      path: p.path,
      value: p.value,
      layer: layerLabel(layer),
      source: p.source?.detail ?? "",
      struck: p.overridden.map((o) => ({ value: o.value, layer: layerLabel(o.source?.layer ?? ProvenanceLayer.UNSPECIFIED), source: o.source?.detail ?? "" })),
      commandLine: layer === ProvenanceLayer.ENV || layer === ProvenanceLayer.FLAG,
    };
  });
}

export interface RowFilter {
  /** Rows whose path holds this text. */
  text: string;
  /** Only rows that replaced a lower layer's value. */
  overriddenOnly: boolean;
  /** Only rows an env var or a flag set. */
  commandLineOnly: boolean;
}

export const ALL_ROWS: RowFilter = { text: "", overriddenOnly: false, commandLineOnly: false };

/** The rows f keeps. */
export function filterRows(rs: readonly Row[], f: RowFilter): Row[] {
  const text = f.text.trim().toLowerCase();
  return rs.filter((r) => (!text || r.path.toLowerCase().includes(text)) && (!f.overriddenOnly || r.struck.length > 0) && (!f.commandLineOnly || r.commandLine));
}

/** A notice as the panel shows it, worded as the CLI prints it. */
export function noticeText(n: OverrideNotice): string {
  const text = `${n.path}: ${n.winner?.detail ?? ""} (${n.value}) overrode ${n.overridden?.detail ?? ""} (${n.overriddenValue})`;
  // sameValue, never the values compared: a secret's are two masks.
  return n.sameValue ? `${text} (same value)` : text;
}

/**
 * What the plan says about how it was made (the C2 ruling): worded from
 * what the provenance holds, never as a claim about which client sent it.
 */
export function madeBy(prov: readonly FieldProvenance[], resolved: boolean): string {
  if (!resolved) return "started without the resolver (orbit runs start-load, start-fleet): no provenance";
  const commandLine = prov.some((p) => p.source?.layer === ProvenanceLayer.ENV || p.source?.layer === ProvenanceLayer.FLAG);
  return commandLine ? "this plan has env or flag layers" : "no env or flag layers (as from Launch)";
}

/** The line under Launch's panel: the browser has no env layer. */
export const LAUNCH_NO_ENV =
  "no env layer here: run from a shell, an exported $ORBIT_* would override these values (orbit run … --explain shows it)";
