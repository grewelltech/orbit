import { describe, expect, it } from "vitest";
import {
  addElement,
  canonicalPath,
  checkTarget,
  documentRefs,
  exportFileName,
  exportText,
  getSlices,
  getValue,
  moveSecrets,
  newTarget,
  parseTargets,
  removeElement,
  render,
  secretSlots,
  setSlices,
  setValue,
} from "./yamlio";

const KI = "5122250214c33e723a5dd523fc145fc0";
const OPC = "981d464c7c52eb6e5036234984ad0bcf";

const doc = (text: string) => {
  const [p] = parseTargets(text);
  if (!p) throw new Error("no target");
  return p.doc;
};

const base = "version: 1\nkind: target\nname: lab1\ncore:\n    amf: 10.0.0.1:38412\n    plmn:\n        mcc: 001\n        mnc: \"01\"\n";

describe("parseTargets", () => {
  it("reads every scalar as its text: a plain 001 stays 001", () => {
    expect(getValue(doc(base), ["core", "plmn", "mcc"])).toBe("001");
  });

  it("takes one target or a list; anything else is refused", () => {
    expect(parseTargets(base)).toHaveLength(1);
    const list = parseTargets(`- ${base.split("\n").join("\n  ").trimEnd()}\n- version: 1\n  kind: target\n  name: lab2\n`);
    expect(list.map((p) => getValue(p.doc, ["name"]))).toEqual(["lab1", "lab2"]);
    expect(list[0]?.text).toBeUndefined();
    expect(() => parseTargets("just a string")).toThrow("want a target or a list of targets");
    expect(() => parseTargets("a: [1")).toThrow();
    expect(() => parseTargets("")).toThrow();
  });
});

describe("checkTarget", () => {
  const issues = (text: string) => checkTarget(doc(text)).map((i) => `${i.path}: ${i.message}`);
  const rows: { name: string; text: string; want: string[] }[] = [
    { name: "a good target", text: base, want: [] },
    { name: "an unknown key", text: base + "    color: blue\n", want: ["core.color: unknown key core.color"] },
    { name: "a quoted number", text: base + '    tac: "7"\n', want: ['core.tac: "7" is quoted; write the number unquoted'] },
    { name: "a leading zero", text: base + "    tac: 0010\n", want: ["core.tac: 0010 has a leading zero; write 10, or 0x… for hex"] },
    { name: "hex is a number", text: base + "    tac: 0x1f\n", want: [] },
    {
      name: "a ${VAR} in a number",
      text: base + "    tac: ${TAC}\n",
      want: ["core.tac: ${TAC} is not expanded in numeric fields; set it with a flag or an env-layer binding"],
    },
    { name: "not a number", text: base + "    tac: seven\n", want: ['core.tac: "seven": want a number'] },
    { name: "a ${VAR} in a string is kept", text: base.replace("10.0.0.1:38412", "${AMF}"), want: [] },
    {
      name: "pools need names, distinct and plain",
      text: base + "subscribers:\n    - first_supi: \"001010000000001\"\n    - name: a\n    - name: a\n    - name: b.c\n",
      want: [
        "subscribers[0]: subscribers entries need a name",
        "subscribers[a]: subscribers entries need distinct names (\"a\" twice)",
        "subscribers[b.c]: subscribers name \"b.c\" may not contain '.', '[' or ']'",
      ],
    },
    { name: "an unknown pool key, by the pool's name", text: base + "subscribers:\n    - name: main\n      key: x\n", want: ["subscribers[main].key: unknown key subscribers[].key"] },
    { name: "a slice's sst quoted", text: base + "    slices:\n        - sst: \"1\"\n", want: ['core.slices[0].sst: slice sst: "1" is quoted; write the number unquoted'] },
    { name: "the document fields", text: "version: 2\nkind: other\nname: \"-x\"\n", want: ["version: version 2: want 1", 'kind: kind "other": want target', 'name: name "-x": letters, digits, \'-\' and \'_\', starting with a letter or digit, at most 63'] },
    { name: "ran.node set", text: base + "ran:\n    node: x\n", want: ['ran.node: ran.node "x": leave it empty'] },
    { name: "a list where a string goes", text: base.replace("10.0.0.1:38412", "[a, b]"), want: ["core.amf: core.amf is a string"] },
  ];
  for (const r of rows) it(r.name, () => expect(issues(r.text)).toEqual(r.want));
});

describe("edits", () => {
  it("keeps an existing value's quoting, and quotes what would read as null", () => {
    const d = doc(base);
    setValue(d, ["core", "plmn", "mnc"], "001");
    setValue(d, ["ran", "gnb_name"], "null");
    setValue(d, ["core", "dnn"], "~");
    const out = render(d);
    expect(out).toContain('mnc: "001"');
    expect(out).toContain('gnb_name: "null"');
    expect(out).toContain('dnn: "~"');
    expect(checkTarget(doc(out))).toEqual([]);
  });

  it("an existing plain value set to text that reads as null is quoted (the CLI would read a null)", () => {
    const d = doc(base);
    setValue(d, ["core", "amf"], "null");
    setValue(d, ["core", "plmn", "mcc"], "~");
    const out = render(d);
    expect(out).toContain('amf: "null"');
    expect(out).toContain('mcc: "~"');
    expect(getValue(doc(out), ["core", "amf"])).toBe("null");
  });

  it("an empty value removes the key, and the mapping it empties", () => {
    const d = doc(base + "ran:\n    gnb_name: g\n");
    setValue(d, ["ran", "gnb_name"], "");
    expect(render(d)).not.toContain("ran:");
    setValue(d, ["core", "ue_pools"], []);
    expect(getValue(d, ["core", "ue_pools"])).toBeUndefined();
  });

  it("lists, slices and named elements", () => {
    const d = newTarget("t1");
    setValue(d, ["core", "ue_pools"], ["10.250.0.0/16", "10.251.0.0/16"]);
    setSlices(d, ["core", "slices"], [{ sst: "1", sd: "010203" }, { sst: "2" }]);
    const i = addElement(d, ["subscribers"], "main");
    setValue(d, ["subscribers", i, "first_supi"], "001010000000001");
    expect(getValue(d, ["core", "ue_pools"])).toEqual(["10.250.0.0/16", "10.251.0.0/16"]);
    expect(getSlices(d, ["core", "slices"])).toEqual([{ sst: "1", sd: "010203" }, { sst: "2", sd: "" }]);
    expect(canonicalPath(d, ["subscribers", 0, "first_supi"])).toBe("subscribers[main].first_supi");
    expect(checkTarget(d)).toEqual([]);
    removeElement(d, ["subscribers"], 0);
    expect(render(d)).not.toContain("subscribers");
  });
});

describe("secrets", () => {
  const withKeys =
    base +
    `subscribers:\n    - name: main\n      ki: ${KI}\n      opc: \${ORBIT_OPC}\n    - name: second\n      ki: ${KI}\n      opc: ${OPC}\n` +
    "app:\n    responders:\n        - name: n6\n          token: orl-literal\n        - name: n7\n          token: ${ORBIT_RESPONDER_TOKEN_1}\n";

  it("moves each literal to a ${VAR}: init's names for the first pool, numbered after; a ${VAR} already there stays", () => {
    const d = doc(withKeys);
    const moved = moveSecrets(d);
    expect(moved.map((m) => [m.path, m.variable, m.value])).toEqual([
      ["subscribers[main].ki", "ORBIT_KI", KI],
      ["subscribers[second].ki", "ORBIT_KI_1", KI],
      ["subscribers[second].opc", "ORBIT_OPC_1", OPC],
      ["app.responders[n6].token", "ORBIT_RESPONDER_TOKEN_0", "orl-literal"],
    ]);
    const out = render(d);
    expect(out).not.toContain(KI);
    expect(out).not.toContain(OPC);
    expect(out).not.toContain("orl-literal");
    expect(secretSlots(d).map((s) => s.value)).toEqual([
      "${ORBIT_KI}",
      "${ORBIT_KI_1}",
      "${ORBIT_OPC}",
      "${ORBIT_OPC_1}",
      "${ORBIT_RESPONDER_TOKEN_0}",
      "${ORBIT_RESPONDER_TOKEN_1}",
    ]);
  });

  it("never takes a variable the document already uses", () => {
    const d = doc(base + "subscribers:\n    - name: a\n      ki: ${ORBIT_KI_1}\n    - name: b\n      ki: literal\n");
    expect(moveSecrets(d).map((m) => m.variable)).toEqual(["ORBIT_KI_2"]);
  });

  it("a responder's variable stays with it when the responders are reordered", () => {
    const d = doc(base + "app:\n    responders:\n        - name: n6\n          token: t6\n        - name: n7\n          token: t7\n");
    moveSecrets(d);
    removeElement(d, ["app", "responders"], 0);
    expect(getValue(d, ["app", "responders", 0, "token"])).toBe("${ORBIT_RESPONDER_TOKEN_1}");
  });

  it("export writes the ${VAR}s, or with keys the values it holds", () => {
    const d = doc(withKeys);
    const moved = moveSecrets(d);
    const keys = Object.fromEntries(moved.map((m) => [m.variable, m.value]));
    expect(exportText(d, { indent: 4 })).not.toContain(KI);
    const clear = exportText(d, { indent: 4 }, { keys });
    expect(clear).toContain(`ki: ${KI}`);
    expect(clear).toContain("opc: ${ORBIT_OPC}"); // no value held: still the reference
    expect(render(d)).not.toContain(KI); // the export does not touch the document
  });

  it("lists every ${VAR} a document names", () => {
    expect(documentRefs(doc(withKeys.replace("10.0.0.1:38412", "${AMF}")))).toEqual(["AMF", "ORBIT_OPC", "ORBIT_RESPONDER_TOKEN_1"]);
  });
});

describe("exportFileName", () => {
  it("is the name, which the name rule keeps file-safe; anything else is refused", () => {
    expect(exportFileName("lab1")).toBe("lab1.yaml");
    expect(exportFileName("../etc/passwd")).toBeUndefined();
    expect(exportFileName("a b")).toBeUndefined();
    expect(exportFileName("")).toBeUndefined();
  });
});
