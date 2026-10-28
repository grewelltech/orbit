/**
 * The JoinSpec a target gives a join token, as `orbit target add-responder
 * -t` takes it: core.ue_pools, core.upf_n6 and core.n6_subnet, with their
 * ${VAR}s filled from this tab's values (the browser has no environment);
 * and the management address, given or the server's own. The server checks
 * each value's form (JoinSpec.Validate).
 */
import type { Document } from "yaml";
import { getValue } from "../targets/yamlio";

export interface JoinSpecInit {
  uePools: string[];
  upfN6: string;
  n6Subnet: string;
  managementAddress: string;
}

/** What a spec still needs: the target fields it lacks, and the ${VAR}s with no value. */
export interface SpecMissing {
  fields: string[];
  vars: string[];
}

/** The spec target doc gives, or what it still needs. management is the address the app host reaches the server at ("" for the server's own). */
export function joinSpecFor(doc: Document, values: Readonly<Record<string, string>>, management: string): { spec: JoinSpecInit } | { missing: SpecMissing } {
  const missing: SpecMissing = { fields: [], vars: [] };
  const fill = (text: string): string =>
    text.replace(/\$\{([A-Za-z_][A-Za-z0-9_]*)\}/g, (ref, name: string) => {
      const v = values[name];
      if (v === undefined || v === "") {
        if (!missing.vars.includes(name)) missing.vars.push(name);
        return ref;
      }
      return v;
    });
  const read = (key: string): string[] => {
    const v = getValue(doc, ["core", key]);
    const list = (Array.isArray(v) ? v : v === undefined ? [] : [v]).map((x) => x.trim()).filter((x) => x !== "");
    if (list.length === 0) missing.fields.push(`core.${key}`);
    return list.map(fill);
  };
  const uePools = read("ue_pools");
  const [upfN6 = ""] = read("upf_n6");
  const [n6Subnet = ""] = read("n6_subnet");
  if (missing.fields.length > 0 || missing.vars.length > 0) return { missing };
  return { spec: { uePools, upfN6, n6Subnet, managementAddress: management.trim() } };
}
