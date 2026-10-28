/**
 * Keys that could reach a field without this page seeing them (the review
 * of #223): anchors, aliases and merge keys, a list where one target goes,
 * and error messages that quote the file. Each case must leave no key in
 * anything stored or exported.
 */
import { describe, expect, it } from "vitest";
import { importFile } from "./importexport";
import { parseBlob, saveTargets, storageChange, TARGETS_KEY, withoutSecrets, type KeyValueStore } from "./store";
import { MAX_FILE_BYTES, parseTargets, SHARED_REFUSED, shapeProblem, sharesNodes, strayKey, TOO_LARGE } from "./yamlio";
import { Alias, Document, parseDocument } from "yaml";

const KI = "AAAA1111222233334444555566667777";
const OPC = "BBBB1111222233334444555566667777";
const head = "version: 1\nkind: target\nname: t1\ncore:\n    amf: 10.0.0.1:38412\n";
const pool = '    - name: a\n      first_supi: "001010000000001"\n      count: 1\n';

function mem(): KeyValueStore & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v), removeItem: (k) => void data.delete(k) };
}
const holdsKey = (s: string) => s.includes(KI) || s.includes(OPC);

const leaks: Record<string, string> = {
  aliasFromUnknown: head + `x-keys: &k ${KI}\nsubscribers:\n${pool}      ki: *k\n      opc: \${ORBIT_OPC}\n`,
  mergeKey: head + `x-defaults: &d\n    ki: ${KI}\n    opc: ${OPC}\nsubscribers:\n${pool}      <<: *d\n`,
  anchorShared:
    head +
    `subscribers:\n${pool}      ki: &k ${KI}\n      opc: \${ORBIT_OPC}\n    - name: b\n      first_supi: "001010000000002"\n      count: 1\n      ki: *k\n      opc: \${ORBIT_OPC}\n`,
  anchorOnly: head + `subscribers:\n${pool}      ki: &k ${KI}\n`,
  listRoot: `- version: 1\n  kind: target\n  name: c\n- version: 1\n  kind: target\n  name: d\n  subscribers:\n    - name: a\n      ki: ${KI}\n`,
  // A list of the wrong shape: its keys are not where secretSlots looks.
  forgottenDash: head + `subscribers:\n    name: a\n    ki: ${KI}\n`,
  listInList: head + `subscribers:\n    - - name: a\n        ki: ${KI}\n`,
  respondersMap: head + `app:\n    responders:\n        name: n6\n        token: orl-${KI}\n`,
  // A key name at a depth no field has.
  kiInCore: head.replace("core:\n", `core:\n    ki: ${KI}\n`),
  // A merge key with its map inline: no anchor, no alias, but the CLI
  // merges it, so it is a key.
  inlineMerge: head + `subscribers:\n${pool}      <<: {ki: ${KI}}\n`,
};

describe("a key the page cannot see is never kept", () => {
  for (const [name, text] of Object.entries(leaks)) {
    it(name, () => {
      let imported: ReturnType<typeof importFile> = [];
      try {
        imported = importFile(text);
      } catch (e) {
        expect(holdsKey((e as Error).message)).toBe(false);
      }
      if (name !== "listRoot") expect(imported).toEqual([]); // refused outright
      if (name === "listRoot") expect(imported.every((t) => !holdsKey(t.target.yaml))).toBe(true);
      for (const t of imported) expect(holdsKey(t.target.yaml)).toBe(false);
      const local = mem();
      saveTargets(local, imported.map((t) => t.target));
      expect(holdsKey(local.data.get(TARGETS_KEY) ?? "")).toBe(false);
      const blob = JSON.stringify({ version: 1, targets: [{ name: "x", yaml: text }] });
      expect(parseBlob(blob)).toEqual([]);
      expect(withoutSecrets(text)).toBeUndefined();
      const ev = storageChange({ key: TARGETS_KEY, newValue: blob, storageArea: local }, local);
      expect(ev).toEqual({ kind: "targets", targets: [] });
    });
  }

  it("anchors, aliases and merge keys are refused with one message", () => {
    for (const name of ["aliasFromUnknown", "mergeKey", "anchorShared", "anchorOnly", "inlineMerge"]) {
      expect(() => parseTargets(leaks[name] ?? ""), name).toThrow(SHARED_REFUSED);
    }
  });

  it("a list's second target with a key: stored only through import, each alone and cleaned", () => {
    const imported = importFile(leaks.listRoot ?? "");
    expect(imported.map((t) => t.target.name)).toEqual(["c", "d"]);
    expect(imported[1]?.moved.map((m) => m.variable)).toEqual(["ORBIT_KI"]);
  });
});

describe("sharesNodes, each kind on its own", () => {
  it("an alias node, with no anchor in the document", () => {
    const d = new Document({ a: 1 }, { schema: "failsafe" });
    d.set("b", new Alias("k"));
    expect(sharesNodes(d)).toBe(true);
  });
  it("an anchor alone", () => expect(sharesNodes(parseDocument("a: &k 1\n", { schema: "failsafe" }))).toBe(true));
  it("a merge key alone (inline map)", () => expect(sharesNodes(parseDocument(leaks.inlineMerge ?? "", { schema: "failsafe" }))).toBe(true));
  it("none of them", () => expect(sharesNodes(parseDocument(head, { schema: "failsafe" }))).toBe(false));
});

describe("withoutSecrets' guards", () => {
  it("a list is no target, though every target in it is clean", () => {
    const clean = "- version: 1\n  kind: target\n  name: a\n- version: 1\n  kind: target\n  name: b\n";
    expect(parseTargets(clean)).toHaveLength(2);
    expect(withoutSecrets(clean)).toBeUndefined();
  });
});

describe("the two layers, each on its own", () => {
  const contents = (text: string) => parseDocument(text, { schema: "failsafe" }).contents;

  it("shape: a key-holding list that is not a list of entries", () => {
    expect(shapeProblem(contents(leaks.forgottenDash ?? ""))).toContain("subscribers is not a list of entries");
    expect(shapeProblem(contents(leaks.listInList ?? ""))).toContain("subscribers is not a list of entries");
    expect(shapeProblem(contents(leaks.respondersMap ?? ""))).toContain("app.responders is not a list of entries");
    expect(shapeProblem(contents(head + "app: [x]\n"))).toContain("app is not a mapping");
    expect(shapeProblem(contents(head + `subscribers:\n    - name: a\n      ki: ${KI}\n`))).toBeUndefined();
    expect(shapeProblem(contents(head + "subscribers:\n"))).toBeUndefined();
  });

  it("sweep: ki, opc or token anywhere but its field, unless a ${VAR}; the reason names the path, not the value", () => {
    expect(strayKey(contents(leaks.forgottenDash ?? ""))).toContain("a ki at subscribers.ki");
    expect(strayKey(contents(leaks.listInList ?? ""))).toContain("a ki at subscribers[0][0].ki");
    expect(strayKey(contents(leaks.respondersMap ?? ""))).toContain("a token at app.responders.token");
    expect(strayKey(contents(leaks.kiInCore ?? ""))).toContain("a ki at core.ki");
    expect(strayKey(contents(leaks.inlineMerge ?? ""))).toContain("a ki at subscribers[0].<<.ki");
    expect(strayKey(contents(head.replace("core:\n", "core:\n    ki: ${X}\n")))).toBeUndefined();
    expect(strayKey(contents(head + `subscribers:\n    - name: a\n      ki: ${KI}\n`))).toBeUndefined();
    for (const name of ["forgottenDash", "listInList", "respondersMap", "kiInCore", "inlineMerge"]) {
      expect(holdsKey(strayKey(contents(leaks[name] ?? "")) ?? "")).toBe(false);
    }
  });
});

describe("errors never quote the file", () => {
  const rows: Record<string, string> = {
    dupKey: head + `subscribers:\n${pool}      ki: \${ORBIT_KI}\n      ki: ${KI}\n`,
    pretty: head + `subscribers:\n    - name: a\n      ki: ${KI}\n     opc: [unclosed\n`,
  };
  for (const [name, text] of Object.entries(rows)) {
    it(name, () => {
      let msg = "";
      try {
        parseTargets(text);
      } catch (e) {
        msg = (e as Error).message;
      }
      expect(msg).toMatch(/^YAML syntax error at line \d+, column \d+$/);
      expect(holdsKey(msg)).toBe(false);
    });
  }
});

describe("hostile files", () => {
  it("an alias tree (billion laughs) is refused before it is expanded", () => {
    let y = head + "x0: &a0 [lol,lol,lol,lol,lol,lol,lol,lol,lol]\n";
    for (let i = 1; i < 12; i++) y += `x${i}: &a${i} [${Array(9).fill(`*a${i - 1}`).join(",")}]\n`;
    expect(() => parseTargets(y)).toThrow(SHARED_REFUSED);
  });

  it("a file over 1 MiB is refused before it is parsed, measured in bytes", () => {
    expect(() => parseTargets(head + "#" + "x".repeat(MAX_FILE_BYTES))).toThrow(TOO_LARGE);
    // 400k "€" is 1.2 MB of UTF-8, though only 400k UTF-16 units.
    expect(() => parseTargets(head + "#" + "€".repeat(400_000))).toThrow(TOO_LARGE);
  });

  it("deep nesting throws an error, not a crash", () => {
    expect(() => parseTargets(head + "x: " + "[".repeat(100000) + "]".repeat(100000) + "\n")).toThrow();
  });

  it("__proto__ and constructor keys pollute nothing", () => {
    importFile(head + "__proto__:\n    polluted: yes\nconstructor:\n    prototype:\n        polluted2: yes\n");
    expect(({} as Record<string, unknown>).polluted).toBeUndefined();
    expect(({} as Record<string, unknown>).polluted2).toBeUndefined();
  });
});
