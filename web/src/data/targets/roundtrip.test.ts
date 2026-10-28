/**
 * The round trip with the CLI, against the files in
 * internal/target/testdata/roundtrip:
 * - cli/*.yaml are files `orbit target init` wrote (and one written by
 *   hand); imported and exported unchanged, each comes back byte for byte.
 * - ui/<case>.yaml are what this page exports for each case in
 *   cases.json; internal/target's TestRoundTripDashboardFiles loads them
 *   with the CLI's loader and checks they resolve as the CLI's file with
 *   the same edits. UPDATE=1 rewrites them; otherwise a stale one fails.
 */
import { readFileSync, writeFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import { exportText, getValue, parseTargets, setValue, type Step } from "./yamlio";
import { importFile } from "./importexport";

const dir = new URL("../../../../internal/target/testdata/roundtrip/", import.meta.url);
const read = (p: string) => readFileSync(new URL(p, dir), "utf8");

interface Case {
  name: string;
  from: string;
  keys?: boolean;
  edits: { steps: Step[]; path: string; value: string | string[] }[];
}
const { cases } = JSON.parse(read("cases.json")) as { cases: Case[] };

describe("files the CLI wrote", () => {
  for (const name of ["minimal", "keys-as-vars", "handwritten"]) {
    it(`${name}: imported and exported unchanged, byte for byte`, () => {
      const text = read(`cli/${name}.yaml`);
      const [t] = importFile(text);
      expect(t?.issues).toEqual([]);
      expect(t?.moved).toEqual([]);
      // Stored as the file's own text, so an unedited export is the file.
      expect(t?.target.yaml).toBe(text);
      // Written out again from the document (as after an edit): init's
      // files come back byte for byte; a hand-written file keeps its
      // comments, but one space before a trailing comment.
      const [p] = parseTargets(text);
      const again = p ? exportText(p.doc, p.style) : "";
      if (name === "handwritten") expect(again).toBe(text.replace("${LAB_AMF}   #", "${LAB_AMF} #"));
      else expect(again).toBe(text);
    });
  }

  it("keys-written: the keys move to the tab; export writes ${ORBIT_KI}/${ORBIT_OPC}, or with keys the file as it was", () => {
    const text = read("cli/keys-written.yaml");
    const [t] = importFile(text);
    expect(t?.moved.map((m) => [m.path, m.variable])).toEqual([
      ["subscribers[main].ki", "ORBIT_KI"],
      ["subscribers[main].opc", "ORBIT_OPC"],
    ]);
    expect(t?.target.yaml).not.toContain(t?.keys.ORBIT_KI ?? "none");
    const [p] = parseTargets(t?.target.yaml ?? "");
    if (!p || !t) throw new Error("no target");
    const lines = (s: string) => s.split("\n");
    const without = lines(exportText(p.doc, p.style));
    const orig = lines(text);
    expect(without.length).toBe(orig.length);
    without.forEach((l, i) => {
      if (/^\s+(ki|opc):/.test(l)) expect(l).toMatch(/\$\{ORBIT_(KI|OPC)\}$/);
      else expect(l).toBe(orig[i]);
    });
    expect(exportText(p.doc, p.style, { keys: t.keys })).toBe(text);
  });
});

describe("the files this page exports for the CLI", () => {
  for (const c of cases) {
    it(c.name, () => {
      const [t] = importFile(read(`cli/${c.from}.yaml`));
      const [p] = parseTargets(t?.target.yaml ?? "");
      if (!p || !t) throw new Error("no target");
      for (const e of c.edits) {
        setValue(p.doc, e.steps, e.value);
        const got = getValue(p.doc, e.steps);
        expect(got ?? "").toEqual(e.value);
      }
      const out = exportText(p.doc, p.style, c.keys ? { keys: t.keys } : {});
      const path = `ui/${c.name}.yaml`;
      if (process.env.UPDATE) writeFileSync(new URL(path, dir), out);
      expect(out, `${path} is stale: UPDATE=1 npm test`).toBe(read(path));
    });
  }
});
