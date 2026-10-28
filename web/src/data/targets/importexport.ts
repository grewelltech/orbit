/**
 * Import and export of target files: what the Targets page does with a
 * file the user gives it, and what it hands back.
 */
import { checkTarget, moveSecrets, nameOf, parseTargets, render, type Issue, type MovedSecret } from "./yamlio";
import type { KeyValues, StoredTarget } from "./store";

/** One target read from an imported file. */
export interface ImportedTarget {
  target: StoredTarget;
  /** Structure problems; the target is still imported, so it can be fixed in the editor. */
  issues: Issue[];
  /** Literal secrets the file held, now ${VAR}s; their values go to the tab (keys). */
  moved: MovedSecret[];
  keys: KeyValues;
}

/**
 * Reads an imported file: one target or a list. A target's stored text is
 * the file's own when the file is that one target and held no literal
 * secret, so it comes back byte for byte; otherwise it is the document
 * written in the file's indent.
 */
export function importFile(text: string): ImportedTarget[] {
  return parseTargets(text).map((p) => {
    const issues = checkTarget(p.doc);
    const moved = moveSecrets(p.doc);
    const keys: KeyValues = {};
    for (const m of moved) keys[m.variable] = m.value;
    const yaml = p.text !== undefined && moved.length === 0 ? p.text : render(p.doc, p.style);
    return { target: { name: nameOf(p.doc), yaml }, issues, moved, keys };
  });
}

/** The one-line notice for a literal secret an import moved into the tab. */
export function movedNotice(m: MovedSecret): string {
  const field = m.path.slice(m.path.lastIndexOf(".") + 1);
  return `${field} (${m.path}) moved to this tab as \${${m.variable}}; the file had it in clear`;
}

/** Where a name clash on import leaves the imported target: replacing the old one, or under a new name. */
export function freeName(name: string, taken: ReadonlySet<string>): string {
  if (!taken.has(name)) return name;
  for (let i = 2; ; i++) {
    const n = `${name}-${i}`.slice(0, 63);
    if (!taken.has(n)) return n;
  }
}
