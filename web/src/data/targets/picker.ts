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
  // In document order, so unresolved lists the fields as the file has them.
  const amf = text("core.amf", ["core", "amf"]);
  const mcc = text("core.plmn.mcc", ["core", "plmn", "mcc"]);
  const mnc = text("core.plmn.mnc", ["core", "plmn", "mnc"]);
  const tac = text("core.tac", ["core", "tac"], (defaultFor("core.tac") as string | undefined) ?? "");
  const slices = getSlices(doc, ["core", "slices"]);
  const defSlices = defaultFor("core.slices") as { sst: string; sd?: string }[] | undefined;
  const first = slices[0] ?? { sst: defSlices?.[0]?.sst ?? "", sd: defSlices?.[0]?.sd ?? "" };
  const plain = (path: string, v: string) => {
    if (refsIn(v).length === 0) return v;
    unresolved.push(path);
    return "";
  };
  const slice = { sst: plain("core.slices[0].sst", first.sst), sd: plain("core.slices[0].sd", first.sd) };
  return { amf, identity: { mcc, mnc, tac, sst: slice.sst, sd: slice.sd }, unresolved };
}

/** The conformance form's fields a target sets, and what it left unresolved. */
export interface PickedForm {
  amf: string;
  identity: NetworkIdentity;
  unresolved: string[];
}

/**
 * The form after picking a target: every field the target controls is set
 * from it, and one it leaves unresolved or empty is cleared, never kept
 * from the target picked before (a run must not go to the last target's
 * AMF). A target that does not parse leaves the form as it was.
 */
export function pickInto(form: PickedForm, yaml: string | undefined): PickedForm {
  const p = yaml === undefined ? undefined : pickTarget(yaml);
  if (!p) return form;
  return { amf: p.amf, identity: p.identity, unresolved: p.unresolved };
}
