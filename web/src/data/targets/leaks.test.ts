/**
 * Keys that could reach a field without this page seeing them, every case
 * from the three reviews of #223 in one table. Each row either is refused
 * (REFUSE: nothing imported) or imports with the key moved to the tab
 * (MOVE). Either way no key is left in anything stored or exported, in a
 * notice, or in what parseBlob or withoutSecrets keep.
 */
import { describe, expect, it } from "vitest";
import { Alias, Document, parseDocument } from "yaml";
import * as act from "./actions";
import { importFile, readImportFile } from "./importexport";
import { parseBlob, saveTargets, storageChange, TARGETS_KEY, withoutSecrets, type KeyValueStore } from "./store";
import { MAX_FILE_BYTES, parseTargets, shapeProblem, SHARED_REFUSED, sharesNodes, strayKey, structureProblem, TOO_LARGE } from "./yamlio";

const KI = "AAAA1111222233334444555566667777";
const OPC = "BBBB1111222233334444555566667777";
const head = "version: 1\nkind: target\nname: t1\ncore:\n    amf: 10.0.0.1:38412\n";
const sub = (x: string) => 'subscribers:\n    - name: a\n      first_supi: "001010000000001"\n      count: 1\n' + x;

function mem(): KeyValueStore & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v), removeItem: (k) => void data.delete(k) };
}
const holdsKey = (s: string) => s.includes(KI) || s.includes(OPC) || s.includes("AAAA1111") || s.includes("222233334444555566667777");

type Want = "REFUSE" | "MOVE";
const rows: [string, Want, string][] = [
  // Round 1: anchors, aliases, merge keys, errors, nesting.
  ["aliasFromUnknown", "REFUSE", head + `x-keys: &k ${KI}\n` + sub("      ki: *k\n      opc: ${ORBIT_OPC}\n")],
  ["mergeKey", "REFUSE", head + `x-defaults: &d\n    ki: ${KI}\n    opc: ${OPC}\n` + sub("      <<: *d\n")],
  ["anchorShared", "REFUSE", head + sub(`      ki: &k ${KI}\n`) + '    - name: b\n      first_supi: "001010000000002"\n      count: 1\n      ki: *k\n'],
  ["anchorOnly", "REFUSE", head + sub(`      ki: &k ${KI}\n`)],
  ["inlineMerge", "REFUSE", head + sub(`      <<: {ki: ${KI}}\n`)],
  ["dupKey", "REFUSE", head + sub(`      ki: \${ORBIT_KI}\n      ki: ${KI}\n`)],
  ["pretty", "REFUSE", head + `subscribers:\n    - name: a\n      ki: ${KI}\n     opc: [unclosed\n`],
  ["deepNesting", "REFUSE", head + "x: " + "[".repeat(100000) + "]".repeat(100000) + "\n"],
  ["pollution", "REFUSE", head + "__proto__:\n    polluted: yes\nconstructor:\n    prototype:\n        polluted2: yes\n"],
  // Round 2: tags, key forms, shapes, documents.
  ["tagStr", "REFUSE", head + sub(`      ki: !!str ${KI}\n`)],
  ["tagCustom", "REFUSE", head + sub(`      ki: !custom ${KI}\n`)],
  ["tagBinary", "REFUSE", head + sub("      ki: !!binary QUFBQTExMTEyMjIyMzMzMzQ0NDQ1NTU1NjY2Njc3Nzc=\n")],
  ["flowMap", "MOVE", head + `subscribers: [{name: a, first_supi: "001010000000001", count: 1, ki: ${KI}}]\n`],
  ["complexKey", "MOVE", head + sub(`      ? ki\n      : ${KI}\n`)],
  ["quotedKey", "MOVE", head + sub(`      "ki": ${KI}\n`)],
  ["taggedKey", "REFUSE", head + sub(`      !!str ki: ${KI}\n`)],
  ["dupSubscribers", "REFUSE", head + sub("      ki: ${ORBIT_KI}\n") + `subscribers:\n    - name: b\n      ki: ${KI}\n`],
  ["dupQuotedSubscribers", "REFUSE", head + sub("      ki: ${ORBIT_KI}\n") + `"subscribers":\n    - name: b\n      ki: ${KI}\n`],
  ["multiDoc", "REFUSE", head + sub("      ki: ${ORBIT_KI}\n") + "---\n" + head + sub(`      ki: ${KI}\n`)],
  ["multiDocEmpty", "REFUSE", head + sub(`      ki: ${KI}\n`) + "---\n"],
  ["forgottenDash", "REFUSE", head + `subscribers:\n    name: a\n    ki: ${KI}\n`],
  ["listInList", "REFUSE", head + `subscribers:\n    - - name: a\n        ki: ${KI}\n`],
  ["respondersMap", "REFUSE", head + `app:\n    responders:\n        name: n6\n        token: orl-${KI}\n`],
  ["plainScalarMultiline", "MOVE", head + sub("      ki: AAAA1111\n        222233334444555566667777\n")],
  // Round 3: non-scalar values, keys as text, odd shapes.
  ["kiSeqValue", "REFUSE", head + sub(`      ki: [${KI}]\n`)],
  ["kiMapValue", "REFUSE", head + sub(`      ki: {${KI}: x}\n`)],
  ["kiBlockSeq", "REFUSE", head + sub(`      ki:\n        - ${KI}\n`)],
  ["tokenSeqValue", "REFUSE", head + `app:\n    responders:\n        - name: n6\n          token: [orl-${KI}]\n`],
  ["nonStringKey", "REFUSE", head + sub(`      ? [ki]\n      : ${KI}\n`)],
  ["secretAsKeyText", "REFUSE", head + `x-junk:\n    ${KI}:\n        ki: literal-AAAA\n`],
  ["secretAsKeyInList", "REFUSE", head + sub(`      ${KI}: 1\n      opc: \${ORBIT_OPC}\n`)],
  ["upperKI", "REFUSE", head + sub(`      KI: ${KI}\n`)],
  ["coreKi", "REFUSE", head.replace("core:\n", `core:\n    ki: ${KI}\n`)],
  ["strayNull", "REFUSE", head + "x:\n    ki: ~\n"],
  ["appScalar", "REFUSE", head + `app: ${KI}\n`],
  ["appSeq", "REFUSE", head + `app:\n    - responders: [{name: n6, token: ${KI}}]\n`],
  ["pairsTag", "REFUSE", head + `subscribers: !!pairs [{name: a, ki: ${KI}}]\n`],
  ["flowPairs", "MOVE", head + `subscribers: [name: a, ki: ${KI}]\n`],
  ["dollarNoBrace", "MOVE", head + sub("      ki: $ORBIT_KI\n")],
  ["refSuffix", "MOVE", head + sub("      ki: ${ORBIT_KI}x\n")],
];

describe("no key the page cannot see is kept, by any path", () => {
  for (const [name, want, text] of rows) {
    it(`${name}: ${want}`, () => {
      const s = { local: mem(), session: mem() };
      const r = act.importInto(s, [], text, false);
      for (const n of r.notices) expect(holdsKey(n), n).toBe(false);
      if (want === "REFUSE") expect(r.targets).toEqual([]);
      else expect(r.targets.length).toBeGreaterThan(0);
      for (const t of r.targets) expect(holdsKey(t.yaml)).toBe(false);
      expect(holdsKey(s.local.data.get(TARGETS_KEY) ?? "")).toBe(false);
      if (r.last) expect(holdsKey(act.exportTarget(s, r.targets, r.last, false)?.text ?? "")).toBe(false);
      const blob = JSON.stringify({ version: 1, targets: [{ name: "x", yaml: text }] });
      for (const t of parseBlob(blob)) expect(holdsKey(t.yaml)).toBe(false);
      const ev = storageChange({ key: TARGETS_KEY, newValue: blob, storageArea: s.local }, s.local);
      expect(holdsKey(JSON.stringify(ev))).toBe(false);
      const w = withoutSecrets(text);
      if (want === "REFUSE") expect(w).toBeUndefined();
      else expect(holdsKey(w ?? "")).toBe(false);
    });
  }

  it("a list root: each target in it is cleaned on import; as a stored blob it is no target", () => {
    const list = `- version: 1\n  kind: target\n  name: c\n- version: 1\n  kind: target\n  name: d\n  subscribers:\n    - name: a\n      ki: ${KI}\n`;
    const imported = importFile(list);
    expect(imported.map((t) => t.target.name)).toEqual(["c", "d"]);
    for (const t of imported) expect(holdsKey(t.target.yaml)).toBe(false);
    expect(parseBlob(JSON.stringify({ version: 1, targets: [{ name: "x", yaml: list }] }))).toEqual([]);
    expect(withoutSecrets(list)).toBeUndefined();
    // The list guard alone decides this one: every target in it is clean.
    expect(withoutSecrets("- version: 1\n  kind: target\n  name: a\n- version: 1\n  kind: target\n  name: b\n")).toBeUndefined();
  });

  it("an edit that would leave a key out of sight (a pool as one mapping) is refused whole", () => {
    const s = { local: mem(), session: mem() };
    let told = "";
    const r = act.editTarget(s, [{ name: "t1", yaml: head }], "t1", (d) => d.setIn(["subscribers"], d.createNode({ name: "a", ki: KI })), (w) => (told = w));
    expect(r).toBeUndefined();
    // Said, not dropped silently, and without the key.
    expect(told).toBe("edit not kept: subscribers is not a list of entries");
    expect(holdsKey([...s.local.data.values(), ...s.session.data.values()].join(""))).toBe(false);
  });

  it("saveTargets drops what it is handed with a hidden key", () => {
    const local = mem();
    saveTargets(local, [{ name: "t1", yaml: head + sub(`      ki: [${KI}]\n`) }]);
    expect(holdsKey(local.data.get(TARGETS_KEY) ?? "")).toBe(false);
  });
});

describe("files the CLI writes all pass the gate", () => {
  it("every round-trip file, CLI and dashboard written, imports with no refusal", async () => {
    const { readdirSync, readFileSync } = await import("node:fs");
    const dir = new URL("../../../../internal/target/testdata/roundtrip/", import.meta.url);
    let n = 0;
    for (const sub of ["cli/", "ui/"]) {
      for (const f of readdirSync(new URL(sub, dir))) {
        const text = readFileSync(new URL(sub + f, dir), "utf8");
        expect(() => importFile(text), sub + f).not.toThrow();
        n++;
      }
    }
    expect(n).toBeGreaterThanOrEqual(11);
  });
});

describe("each layer on its own", () => {
  const contents = (text: string) => parseDocument(text, { schema: "failsafe" }).contents;

  it("layer zero, the structure: only it refuses these (the other layers pass them)", () => {
    for (const text of [head + sub(`      KI: ${KI}\n`), head + "color: blue\n", head + sub("      ki: !!str x\n")]) {
      const c = contents(text);
      expect(structureProblem(c), text).toBeDefined();
      expect(shapeProblem(c)).toBeUndefined();
      expect(strayKey(c)).toBeUndefined();
      expect(() => parseTargets(text)).toThrow();
    }
  });

  it("a list field takes a list, as the CLI's decode does: a lone value is refused", () => {
    expect(structureProblem(contents(head.replace("core:\n", "core:\n    ue_pools: 10.250.0.0/16\n")))).toBe('core.ue_pools is not a list of values (write "- value")');
    expect(structureProblem(contents(head.replace("core:\n", "core:\n    ue_pools: [10.250.0.0/16]\n")))).toBeUndefined();
  });

  it("a tag is named as a tag, never quoted", () => {
    expect(structureProblem(contents(head + sub(`      ki: !custom${KI} x\n`)))).toBe("subscribers[0].ki has a YAML tag (!!…); write it plain");
    expect(structureProblem(contents(head + sub(`      !!str ki: x\n`)))).toBe("a key under subscribers[0] has a YAML tag (!!…); write it plain");
  });

  it("layer zero's reasons name schema fields and indexes only", () => {
    expect(structureProblem(contents(head + `x-junk:\n    ${KI}:\n        ki: x\n`))).toBe("an unknown field under the top");
    expect(structureProblem(contents(head + sub(`      ${KI}: 1\n`)))).toBe("an unknown field under subscribers[0]");
    expect(structureProblem(contents(head + sub(`      ki: [${KI}]\n`)))).toBe("subscribers[0].ki is not a single value");
    expect(structureProblem(contents(head + "core:\n    slices:\n        - sst: 1\n          x: 2\n"))).toBe("an unknown field under core.slices[0]");
  });

  it("shape: a key-holding list that is not a list of entries", () => {
    expect(shapeProblem(contents(head + `subscribers:\n    name: a\n    ki: ${KI}\n`))).toContain("subscribers is not a list of entries");
    // Only shape refuses this one of the two later layers: its ki is a ${VAR}.
    const viaVar = contents(head + "subscribers:\n    name: a\n    ki: ${ORBIT_KI}\n");
    expect(shapeProblem(viaVar)).toContain("subscribers is not a list of entries");
    expect(strayKey(viaVar)).toBeUndefined();
    expect(shapeProblem(contents(head + `app:\n    responders:\n        name: n6\n`))).toContain("app.responders is not a list of entries");
    expect(shapeProblem(contents(head + "app: [x]\n"))).toContain("app is not a mapping");
    expect(shapeProblem(contents(head + sub(`      ki: ${KI}\n`)))).toBeUndefined();
  });

  it("sweep: a key field anywhere but its own, or not one value; the path never quotes the file", () => {
    expect(strayKey(contents(head.replace("core:\n", `core:\n    ki: ${KI}\n`)))).toContain("a ki at core.ki");
    expect(strayKey(contents(head + sub(`      ki: [${KI}]\n`)))).toContain("the ki at subscribers[0].ki is not a single value");
    expect(strayKey(contents(head + `x-junk:\n    ${KI}:\n        ki: literal\n`))).toContain("a ki at ….….ki");
    expect(strayKey(contents(head + `y:\n    ? [a]\n    : 1\n`))).toContain("is not text");
    expect(strayKey(contents(head.replace("core:\n", "core:\n    ki: ${X}\n")))).toBeUndefined();
    expect(strayKey(contents(head + sub(`      ki: ${KI}\n`)))).toBeUndefined();
  });

  it("sharesNodes: an alias with no anchor, an anchor alone, a merge key alone", () => {
    const d = new Document({ a: 1 }, { schema: "failsafe" });
    d.set("b", new Alias("k"));
    expect(sharesNodes(d)).toBe(true);
    expect(sharesNodes(parseDocument("a: &k 1\n", { schema: "failsafe" }))).toBe(true);
    expect(sharesNodes(parseDocument(head + sub(`      <<: {ki: ${KI}}\n`), { schema: "failsafe" }))).toBe(true);
    expect(sharesNodes(parseDocument(head, { schema: "failsafe" }))).toBe(false);
    expect(() => parseTargets(head + "a: &k 1\n")).toThrow(SHARED_REFUSED);
  });
});

describe("errors never quote the file", () => {
  const cases = [head + sub(`      ki: \${ORBIT_KI}\n      ki: ${KI}\n`), head + `subscribers:\n    - name: a\n      ki: ${KI}\n     opc: [unclosed\n`];
  cases.forEach((text, i) => {
    it(`case ${i + 1}`, () => {
      let msg = "";
      try {
        parseTargets(text);
      } catch (e) {
        msg = (e as Error).message;
      }
      expect(msg).toMatch(/^YAML syntax error at line \d+, column \d+$/);
      expect(holdsKey(msg)).toBe(false);
    });
  });
});

describe("size and hostile files", () => {
  it("over 1 MiB of UTF-8 is refused before it is parsed; just under is read", () => {
    expect(() => parseTargets(head + "# " + "€".repeat(400_000) + "\n")).toThrow(TOO_LARGE);
    expect(() => parseTargets(head + "#" + "x".repeat(MAX_FILE_BYTES))).toThrow(TOO_LARGE);
    const under = head + "# " + "x".repeat(MAX_FILE_BYTES - head.length - 10) + "\n";
    expect(parseTargets(under)).toHaveLength(1);
  });

  it("a chosen file over the cap is refused by its size, never read", async () => {
    let read = false;
    const big = { name: "big.yaml", size: MAX_FILE_BYTES + 1, text: async () => ((read = true), "") };
    expect(await readImportFile(big)).toEqual({ refusal: `import: big.yaml is ${TOO_LARGE}` });
    expect(read).toBe(false);
    expect(await readImportFile({ name: "ok.yaml", size: 10, text: async () => head })).toEqual({ text: head });
  });

  it("an alias tree (billion laughs) is refused before it is expanded", () => {
    let y = head + "x0: &a0 [lol,lol,lol,lol,lol,lol,lol,lol,lol]\n";
    for (let i = 1; i < 12; i++) y += `x${i}: &a${i} [${Array(9).fill(`*a${i - 1}`).join(",")}]\n`;
    expect(() => parseTargets(y)).toThrow(SHARED_REFUSED);
  });

  it("__proto__ and constructor pollute nothing", () => {
    expect(() => importFile(head + "__proto__:\n    polluted: yes\n")).toThrow();
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
  });
});
