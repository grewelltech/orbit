/**
 * The Conformance page's "use target" picker: a stored target's network
 * identity and AMF, as the conformance form takes them. Keys are not part
 * of it; conformance sends none.
 */
import type { NetworkIdentity } from "@/data/conformance";
import { defaultFor, getSlices, getValue, parseTargets, refsIn } from "./yamlio";

/** What a target fills in: the AMF, the identity, and any field left empty because it is a ${VAR}. */
export interface PickedTarget {
  amf: string;
  identity: NetworkIdentity;
  /** Fields the target sets with a ${VAR}, which the browser cannot expand. */
  unresolved: string[];
}

/** The form's values from a target's text, with the built-in defaults under it. */
export function pickTarget(yaml: string): PickedTarget | undefined {
  let doc;
  try {
    doc = parseTargets(yaml)[0]?.doc;
  } catch {
    return undefined;
  }
  if (!doc) return undefined;
  const unresolved: string[] = [];
  const text = (path: string, steps: string[], fallback = ""): string => {
    const v = getValue(doc, steps);
    const s = typeof v === "string" ? v : fallback;
    if (refsIn(s).length > 0) {
      unresolved.push(path);
      return "";
    }
    return s;
  };
  const slices = getSlices(doc, ["core", "slices"]);
  const defSlices = defaultFor("core.slices") as { sst: string; sd?: string }[] | undefined;
  const slice = slices[0] ?? { sst: defSlices?.[0]?.sst ?? "", sd: defSlices?.[0]?.sd ?? "" };
  return {
    amf: text("core.amf", ["core", "amf"]),
    identity: {
      mcc: text("core.plmn.mcc", ["core", "plmn", "mcc"]),
      mnc: text("core.plmn.mnc", ["core", "plmn", "mnc"]),
      tac: text("core.tac", ["core", "tac"], (defaultFor("core.tac") as string | undefined) ?? ""),
      sst: slice.sst,
      sd: slice.sd,
    },
    unresolved,
  };
}
