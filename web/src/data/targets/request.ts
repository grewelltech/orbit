/**
 * A target as a probe sends it: the document's fields on the wire
 * (orbit.v1.ResolvedTarget), each ${VAR} replaced by the value this tab
 * holds for it. The browser has no environment: a ${VAR} with no value
 * here is not sent, it is asked for (VarPrompt). Defaults and formats are
 * the server's: it resolves what is sent as the CLI loads a file.
 */
import { create } from "@bufbuild/protobuf";
import type { Document } from "yaml";
import { ResolvedTargetSchema, type ResolvedTarget } from "@/gen/orbit/v1/target_pb";
import type { KeyValues } from "./store";
import { getSlices, getValue, listLength } from "./yamlio";

/** What a probe would send, or the variables it still needs. */
export interface TargetRequest {
  target: ResolvedTarget;
  /** ${VAR}s the document names with no value in values, in document order. */
  unresolved: string[];
}

/** A number as the CLI reads it: decimal, or 0x/0o/0b. NaN for anything else. */
export function parseNumber(text: string): number {
  if (/^(0|[1-9][0-9]*)$/.test(text)) return Number(text);
  if (/^0x[0-9A-Fa-f]+$/.test(text)) return parseInt(text.slice(2), 16);
  if (/^0o[0-7]+$/.test(text)) return parseInt(text.slice(2), 8);
  if (/^0b[01]+$/.test(text)) return parseInt(text.slice(2), 2);
  return NaN;
}

/** Builds the request target from doc, ${VAR}s replaced from values. */
export function requestTarget(doc: Document, values: Readonly<KeyValues>): TargetRequest {
  const unresolved: string[] = [];
  const sub = (s: string): string =>
    s.replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (ref, name: string) => {
      const v = values[name];
      if (v === undefined || v === "") {
        if (!unresolved.includes(name)) unresolved.push(name);
        return ref;
      }
      return v;
    });
  const str = (...steps: (string | number)[]): string => {
    const v = getValue(doc, steps);
    return typeof v === "string" ? sub(v) : "";
  };
  const strs = (...steps: (string | number)[]): string[] => {
    const v = getValue(doc, steps);
    return (Array.isArray(v) ? v : typeof v === "string" ? [v] : []).map(sub);
  };
  const num = (...steps: (string | number)[]): number => {
    const v = getValue(doc, steps);
    const n = typeof v === "string" ? parseNumber(v) : 0;
    return Number.isFinite(n) ? n : 0;
  };
  const pools = Array.from({ length: listLength(doc, ["subscribers"]) }, (_, i) => ({
    name: str("subscribers", i, "name"),
    firstSupi: str("subscribers", i, "first_supi"),
    count: num("subscribers", i, "count"),
    ki: str("subscribers", i, "ki"),
    opc: str("subscribers", i, "opc"),
    routingIndicator: str("subscribers", i, "routing_indicator"),
  }));
  const responders = Array.from({ length: listLength(doc, ["app", "responders"]) }, (_, i) => ({
    name: str("app", "responders", i, "name"),
    control: str("app", "responders", i, "control"),
    token: str("app", "responders", i, "token"),
    dataIp: str("app", "responders", i, "data_ip"),
  }));
  const target = create(ResolvedTargetSchema, {
    name: str("name"),
    core: {
      amf: str("core", "amf"),
      plmn: { mcc: str("core", "plmn", "mcc"), mnc: str("core", "plmn", "mnc") },
      tac: num("core", "tac"),
      slices: getSlices(doc, ["core", "slices"]).map((s) => ({ sst: Number.isFinite(parseNumber(s.sst)) ? parseNumber(s.sst) : 0, sd: sub(s.sd) })),
      dnn: str("core", "dnn"),
      profile: str("core", "profile"),
      upfN3: str("core", "upf_n3"),
      uePools: strs("core", "ue_pools"),
      upfN6: str("core", "upf_n6"),
      n6Subnet: str("core", "n6_subnet"),
    },
    subscribers: pools,
    ran: {
      node: str("ran", "node"),
      gnbName: str("ran", "gnb_name"),
      n2SourceIps: strs("ran", "n2_source_ips"),
      n3Ips: strs("ran", "n3_ips"),
      gnbIds: { first: num("ran", "gnb_ids", "first"), count: num("ran", "gnb_ids", "count"), bits: num("ran", "gnb_ids", "bits") },
    },
    app: { responders },
  });
  return { target, unresolved };
}

/** Every ${VAR} a document names that values has no value for. */
export function unresolvedRefs(doc: Document, values: Readonly<KeyValues>): string[] {
  return requestTarget(doc, values).unresolved;
}
