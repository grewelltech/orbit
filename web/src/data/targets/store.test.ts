import { describe, expect, it } from "vitest";
import {
  forgetKeys,
  KEYS_PREFIX,
  keysFor,
  loadTargets,
  parseBlob,
  rememberKeys,
  remembers,
  renameKeys,
  saveTargets,
  SECRETS_KEY,
  setTabKeys,
  storageChange,
  tabKeys,
  TARGETS_KEY,
  type KeyValueStore,
} from "./store";
import { freeName, importFile, movedNotice } from "./importexport";

const KI = "5122250214c33e723a5dd523fc145fc0";

function mem(): KeyValueStore & { data: Map<string, string> } {
  const data = new Map<string, string>();
  return { data, getItem: (k) => data.get(k) ?? null, setItem: (k, v) => void data.set(k, v), removeItem: (k) => void data.delete(k) };
}

const blocked: KeyValueStore = {
  getItem: () => {
    throw new Error("blocked");
  },
  setItem: () => {
    throw new Error("blocked");
  },
  removeItem: () => {
    throw new Error("blocked");
  },
};

const target = (ki: string) =>
  `version: 1\nkind: target\nname: lab1\nsubscribers:\n    - name: main\n      ki: ${ki}\n      opc: \${ORBIT_OPC}\n`;

describe("the targets blob", () => {
  it("round-trips its targets, as their text", () => {
    const local = mem();
    saveTargets(local, [{ name: "lab1", yaml: target("${ORBIT_KI}") }]);
    expect(loadTargets(local)).toEqual([{ name: "lab1", yaml: target("${ORBIT_KI}") }]);
  });

  it("never holds a key: a literal is replaced by its ${VAR} on the way in and on the way out", () => {
    const local = mem();
    saveTargets(local, [{ name: "lab1", yaml: target(KI) }]);
    expect(local.data.get(TARGETS_KEY)).not.toContain(KI);
    local.data.set(TARGETS_KEY, JSON.stringify({ version: 1, targets: [{ name: "lab1", yaml: target(KI) }] }));
    const [t] = loadTargets(local);
    expect(t?.yaml).not.toContain(KI);
    expect(t?.yaml).toContain("ki: ${ORBIT_KI}");
  });

  it("an unreadable or foreign blob is no targets; blocked storage is no targets and no throw", () => {
    expect(parseBlob("not json")).toEqual([]);
    expect(parseBlob(JSON.stringify({ version: 2, targets: [] }))).toEqual([]);
    expect(parseBlob(JSON.stringify({ version: 1, targets: [{ name: "x", yaml: "a: [1" }] }))).toEqual([]);
    expect(loadTargets(blocked)).toEqual([]);
    expect(() => saveTargets(blocked, [])).not.toThrow();
    expect(loadTargets(undefined)).toEqual([]);
  });
});

describe("keys", () => {
  it("are held per tab, per target; remembered only on request; the tab's win", () => {
    const local = mem();
    const session = mem();
    setTabKeys(session, "lab1", { ORBIT_KI: "tab" });
    expect(tabKeys(session, "lab1")).toEqual({ ORBIT_KI: "tab" });
    expect(tabKeys(session, "lab2")).toEqual({});
    expect(remembers(local, "lab1")).toBe(false);
    rememberKeys(local, "lab1", { ORBIT_KI: "kept", ORBIT_OPC: "kept-opc" });
    expect(remembers(local, "lab1")).toBe(true);
    expect(keysFor(local, session, "lab1")).toEqual({ ORBIT_KI: "tab", ORBIT_OPC: "kept-opc" });
    expect(local.data.get(TARGETS_KEY)).toBeUndefined();
  });

  it("Forget removes orbit.targets.keys.<name> and the tab's keys for that target only", () => {
    const local = mem();
    const session = mem();
    rememberKeys(local, "lab1", { ORBIT_KI: "a" });
    rememberKeys(local, "lab2", { ORBIT_KI: "b" });
    setTabKeys(session, "lab1", { ORBIT_KI: "a" });
    forgetKeys(local, session, "lab1");
    expect(local.data.has(KEYS_PREFIX + "lab1")).toBe(false);
    expect(local.data.has(KEYS_PREFIX + "lab2")).toBe(true);
    expect(keysFor(local, session, "lab1")).toEqual({});
    expect(session.data.has(SECRETS_KEY)).toBe(false);
  });

  it("move with a rename", () => {
    const local = mem();
    const session = mem();
    rememberKeys(local, "a", { ORBIT_KI: "k" });
    setTabKeys(session, "a", { ORBIT_OPC: "o" });
    renameKeys(local, session, "a", "b");
    expect(keysFor(local, session, "a")).toEqual({});
    expect(keysFor(local, session, "b")).toEqual({ ORBIT_KI: "k", ORBIT_OPC: "o" });
  });
});

describe("storage events from another tab", () => {
  const local = mem();
  it("a new targets blob comes in, cleaned of any key it carries", () => {
    const ev = { key: TARGETS_KEY, newValue: JSON.stringify({ version: 1, targets: [{ name: "lab1", yaml: target(KI) }] }), storageArea: local };
    const c = storageChange(ev, local);
    expect(c?.kind).toBe("targets");
    expect(JSON.stringify(c)).not.toContain(KI);
  });

  it("a remembered-keys change names the target only; the page reads the value itself", () => {
    const c = storageChange({ key: KEYS_PREFIX + "lab1", newValue: JSON.stringify({ ORBIT_KI: KI }), storageArea: local }, local);
    expect(c).toEqual({ kind: "keys", name: "lab1" });
  });

  it("nothing else comes this way: other keys, the session's secrets, another storage area, a clear", () => {
    expect(storageChange({ key: "orbit.token", newValue: "t", storageArea: local }, local)).toBeUndefined();
    expect(storageChange({ key: SECRETS_KEY, newValue: JSON.stringify({ lab1: { ORBIT_KI: KI } }), storageArea: local }, local)).toBeUndefined();
    expect(storageChange({ key: TARGETS_KEY, newValue: "{}", storageArea: mem() }, local)).toBeUndefined();
    expect(storageChange({ key: null, newValue: null, storageArea: local }, local)).toBeUndefined();
  });
});

describe("import", () => {
  it("moves a literal key to the tab, with a notice; the stored text has the ${VAR}", () => {
    const [t] = importFile(target(KI));
    expect(t?.keys).toEqual({ ORBIT_KI: KI });
    expect(t?.target.yaml).not.toContain(KI);
    expect(t?.moved.map(movedNotice)).toEqual(["ki (subscribers[main].ki) moved to this tab as ${ORBIT_KI}; the file had it in clear"]);
  });

  it("a list imports each target; a clash gets a free name", () => {
    const list = importFile("- version: 1\n  kind: target\n  name: a\n- version: 1\n  kind: target\n  name: b\n");
    expect(list.map((t) => t.target.name)).toEqual(["a", "b"]);
    expect(freeName("a", new Set(["a", "a-2"]))).toBe("a-3");
    expect(freeName("c", new Set(["a"]))).toBe("c");
  });

  it("a target with problems is still imported, with them listed", () => {
    const [t] = importFile("version: 1\nkind: target\nname: x\ncore:\n    tac: \"1\"\n");
    expect(t?.issues.map((i) => i.path)).toEqual(["core.tac"]);
  });
});
