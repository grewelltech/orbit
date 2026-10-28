import { describe, expect, it } from "vitest";
import * as act from "./actions";
import { KEYS_PREFIX, keysFor, rememberKeys, remembers, SECRETS_KEY, setTabKeys, tabKeys, TARGETS_KEY, type KeyValueStore, type StoredTarget } from "./store";
import { getValue, removeElement, setValue } from "./yamlio";

const KI = "AAAA1111222233334444555566667777";

function mem(): KeyValueStore & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v), removeItem: (k) => void data.delete(k) };
}

function lab() {
  const s = { local: mem(), session: mem() };
  const yaml =
    "version: 1\nkind: target\nname: lab1\nsubscribers:\n    - name: main\n      ki: ${ORBIT_KI}\n" +
    "app:\n    responders:\n        - name: n6\n          token: ${ORBIT_RESPONDER_TOKEN_0}\n        - name: n7\n          token: ${ORBIT_RESPONDER_TOKEN_1}\n";
  const targets: StoredTarget[] = [{ name: "lab1", yaml }];
  return { s, targets };
}

const everywhere = (s: { local: { data: Map<string, string> }; session: { data: Map<string, string> } }) =>
  [...s.local.data.values(), ...s.session.data.values()].join("\n");

describe("editTarget", () => {
  it("a key typed into a secret field goes to the tab as a ${VAR}, never into the stored text", () => {
    const { s, targets } = lab();
    const r = act.editTarget(s, targets, "lab1", (d) => setValue(d, ["subscribers", 0, "ki"], KI));
    expect(r?.targets[0]?.yaml).toContain("ki: ${ORBIT_KI}");
    expect(r?.targets[0]?.yaml).not.toContain(KI);
    expect(s.local.data.get(TARGETS_KEY)).not.toContain(KI);
    expect(tabKeys(s.session, "lab1")).toEqual({ ORBIT_KI: KI });
  });

  it("removing a responder drops its ${VAR}'s key, in the tab and the browser", () => {
    const { s, targets } = lab();
    setTabKeys(s.session, "lab1", { ORBIT_KI: "k", ORBIT_RESPONDER_TOKEN_0: "t0", ORBIT_RESPONDER_TOKEN_1: "t1" });
    rememberKeys(s.local, "lab1", { ORBIT_RESPONDER_TOKEN_0: "t0", ORBIT_RESPONDER_TOKEN_1: "t1" });
    act.editTarget(s, targets, "lab1", (d) => removeElement(d, ["app", "responders"], 0));
    expect(tabKeys(s.session, "lab1")).toEqual({ ORBIT_KI: "k", ORBIT_RESPONDER_TOKEN_1: "t1" });
    expect(keysFor(s.local, undefined, "lab1")).toEqual({ ORBIT_RESPONDER_TOKEN_1: "t1" });
  });

  it("a rename takes the keys along; a rename to a taken or invalid name is undone", () => {
    const { s, targets } = lab();
    setTabKeys(s.session, "lab1", { ORBIT_KI: "k" });
    const r = act.editTarget(s, targets, "lab1", (d) => setValue(d, ["name"], "lab2"));
    expect(r?.name).toBe("lab2");
    expect(tabKeys(s.session, "lab2")).toEqual({ ORBIT_KI: "k" });
    expect(tabKeys(s.session, "lab1")).toEqual({});
    const both = [...(r?.targets ?? []), { name: "lab3", yaml: "version: 1\nkind: target\nname: lab3\n" }];
    for (const bad of ["lab3", "../x"]) {
      const u = act.editTarget(s, both, "lab2", (d) => setValue(d, ["name"], bad));
      expect(u?.name).toBe("lab2");
      expect(u?.targets.find((t) => t.name === "lab2")?.yaml).toContain("name: lab2");
    }
  });
});

describe("removeTarget", () => {
  it("removes the target and its keys, remembered and in the tab, and only its", () => {
    const { s, targets } = lab();
    rememberKeys(s.local, "lab1", { ORBIT_KI: KI });
    setTabKeys(s.session, "lab1", { ORBIT_KI: KI });
    setTabKeys(s.session, "other", { ORBIT_KI: "x" });
    const out = act.removeTarget(s, targets, "lab1");
    expect(out).toEqual([]);
    expect(s.local.data.has(KEYS_PREFIX + "lab1")).toBe(false);
    expect(tabKeys(s.session, "lab1")).toEqual({});
    expect(tabKeys(s.session, "other")).toEqual({ ORBIT_KI: "x" });
    expect(everywhere(s)).not.toContain(KI);
  });

  it("a target deleted in another tab loses its keys in this tab", () => {
    const { s, targets } = lab();
    setTabKeys(s.session, "lab1", { ORBIT_KI: KI });
    act.targetsFromElsewhere(s, targets, []);
    expect(s.session.data.has(SECRETS_KEY)).toBe(false);
  });
});

describe("export", () => {
  it("without keys: the stored text, no key, though the tab holds one", () => {
    const { s, targets } = lab();
    setTabKeys(s.session, "lab1", { ORBIT_KI: KI });
    const f = act.exportTarget(s, targets, "lab1", false);
    expect(f).toEqual({ fileName: "lab1.yaml", text: targets[0]?.yaml });
    expect(f?.text).not.toContain(KI);
  });

  it("with keys: in clear; and the choice is off again for the next export", () => {
    const { s, targets } = lab();
    setTabKeys(s.session, "lab1", { ORBIT_KI: KI });
    const r = act.exportOnce(s, targets, "lab1", true);
    expect(r.file?.text).toContain(`ki: ${KI}`);
    expect(r.withKeys).toBe(false);
  });
});

describe("remember keys", () => {
  it("is written only once confirmed; declined, nothing is written", () => {
    const { s } = lab();
    setTabKeys(s.session, "lab1", { ORBIT_KI: KI });
    let asked = "";
    expect(act.setRemember(s, "lab1", true, (w) => ((asked = w), false))).toBe(false);
    expect(asked).toBe(act.REMEMBER_WARNING);
    expect(remembers(s.local, "lab1")).toBe(false);
    expect([...s.local.data.values()].join("")).not.toContain(KI);
    expect(act.setRemember(s, "lab1", true, () => true)).toBe(true);
    expect(s.local.data.get(KEYS_PREFIX + "lab1")).toContain(KI);
    expect(act.setRemember(s, "lab1", false, () => true)).toBe(false);
    expect(s.local.data.has(KEYS_PREFIX + "lab1")).toBe(false);
  });
});

describe("importInto", () => {
  it("moves a literal key to the tab with a notice; the blob never has it", () => {
    const { s } = lab();
    const r = act.importInto(s, [], `version: 1\nkind: target\nname: t1\nsubscribers:\n    - name: a\n      ki: ${KI}\n`, false);
    expect(r.notices).toEqual(["t1: ki (subscribers[a].ki) moved to this tab as ${ORBIT_KI}; the file had it in clear"]);
    expect(tabKeys(s.session, "t1")).toEqual({ ORBIT_KI: KI });
    expect(s.local.data.get(TARGETS_KEY)).not.toContain(KI);
  });

  it("a clash: a free name, or replace (and the old keys go)", () => {
    const { s, targets } = lab();
    setTabKeys(s.session, "lab1", { ORBIT_KI: "old" });
    const text = "version: 1\nkind: target\nname: lab1\n";
    const kept = act.importInto(s, targets, text, false);
    expect(kept.targets.map((t) => t.name)).toEqual(["lab1", "lab1-2"]);
    expect(getValue(act.parseStored(kept.targets[1] as StoredTarget)!.doc, ["name"])).toBe("lab1-2");
    const replaced = act.importInto(s, targets, text, true);
    expect(replaced.targets.map((t) => t.name)).toEqual(["lab1"]);
    expect(tabKeys(s.session, "lab1")).toEqual({});
  });

  it("a file that does not read is one notice, its first line, with no text from the file", () => {
    const { s } = lab();
    const dup = `version: 1\nkind: target\nname: t1\nsubscribers:\n    - name: a\n      ki: \${ORBIT_KI}\n      ki: ${KI}\n`;
    const r = act.importInto(s, [], dup, false);
    expect(r.notices).toHaveLength(1);
    expect(r.notices[0]).toMatch(/^import: YAML syntax error at line \d+, column \d+$/);
    expect(r.notices[0]).not.toContain(KI);
    expect(everywhere(s)).not.toContain(KI);
  });
});
