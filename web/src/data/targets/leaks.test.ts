/**
 * Keys that could reach a field without this page seeing them (the review
 * of #223): anchors, aliases and merge keys, a list where one target goes,
 * and error messages that quote the file. Each case must leave no key in
 * anything stored or exported.
 */
import { describe, expect, it } from "vitest";
import { importFile } from "./importexport";
import { parseBlob, saveTargets, storageChange, TARGETS_KEY, withoutSecrets, type KeyValueStore } from "./store";
import { MAX_FILE_BYTES, parseTargets, SHARED_REFUSED } from "./yamlio";

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
    for (const name of ["aliasFromUnknown", "mergeKey", "anchorShared", "anchorOnly"]) {
      expect(() => parseTargets(leaks[name] ?? ""), name).toThrow(SHARED_REFUSED);
    }
  });

  it("a list's second target with a key: stored only through import, each alone and cleaned", () => {
    const imported = importFile(leaks.listRoot ?? "");
    expect(imported.map((t) => t.target.name)).toEqual(["c", "d"]);
    expect(imported[1]?.moved.map((m) => m.variable)).toEqual(["ORBIT_KI"]);
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

  it("a file over 1 MiB is refused before it is parsed", () => {
    const big = head + "#" + "x".repeat(MAX_FILE_BYTES);
    expect(() => parseTargets(big)).toThrow("larger than 1 MiB");
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
