/**
 * "use <name> in <target>": a joined responder named in the target's
 * app.responders, by name only (the server fills the rest from its
 * registry, and the target keeps no key). A change for the Targets page's
 * edit (editTarget), so it passes the structure gate an edit does.
 */
import type { Document } from "yaml";
import { addElement, getValue, listLength } from "../targets/yamlio";

/** Whether target doc names responder name already. */
export function usesResponder(doc: Document, name: string): boolean {
  const n = listLength(doc, ["app", "responders"]);
  for (let i = 0; i < n; i++) if (getValue(doc, ["app", "responders", i, "name"]) === name) return true;
  return false;
}

/** The change that names responder name in a target: nothing when it already does. */
export function nameResponderIn(name: string): (doc: Document) => void {
  return (doc) => {
    if (!usesResponder(doc, name)) addElement(doc, ["app", "responders"], name);
  };
}
