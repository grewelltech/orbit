import { describe, expect, it } from "vitest";
import { isSecretName, parseRun, renderRun } from "./runyaml";
import { RUN_FIELDS, SECRET_NAME_VECTORS, TEMPLATES } from "./runschema.gen";

const ok = (text: string) => expect(() => parseRun(text)).not.toThrow();
const refused = (text: string, why: string | RegExp) => expect(() => parseRun(text)).toThrow(why);

describe("isSecretName, as internal/secrets has it", () => {
  for (const [name, want] of Object.entries(SECRET_NAME_VECTORS)) {
    it(`${name}: ${want}`, () => expect(isSecretName(name)).toBe(want));
  }
});

describe("the run-file gate", () => {
  it("every template reads, and writes back as read", () => {
    for (const [name, text] of Object.entries(TEMPLATES)) {
      const doc = parseRun(text);
      expect(parseRun(renderRun(doc)).toJSON(), name).toEqual(doc.toJSON());
    }
  });

  it("every run-file field the schema has reads at its path", () => {
    // One document setting every scalar and list field once.
    for (const f of RUN_FIELDS) {
      if (f.type === "any") continue;
      const value = f.type === "strings" ? "[a]" : f.type === "bool" ? "true" : f.type === "string" ? "x" : "1";
      const parts = f.path.split(".");
      let yaml = "kind: run\n";
      let indent = "";
      for (let i = 0; i < parts.length; i++) {
        const p = parts[i] ?? "";
        const last = i === parts.length - 1;
        if (p.endsWith("[]")) {
          yaml += `${indent}${p.slice(0, -2)}:\n${indent}  - name: n\n`;
          indent += "    ";
        } else if (last) {
          // A list entry's name is written with the entry.
          if (!(p === "name" && (parts[i - 1] ?? "").endsWith("[]"))) yaml += `${indent}${p}: ${value}\n`;
        } else {
          yaml += `${indent}${p}:\n`;
          indent += "  ";
        }
      }
      expect(() => parseRun(yaml), f.path).not.toThrow();
    }
  });

  const pop = "kind: run\nworkloads:\n  - name: pop\n";
  const rows: [string, string, string | RegExp][] = [
    ["another kind", "kind: target\nname: x\n", "kind is not run"],
    ["no kind", "workloads: []\n", "kind is not run"],
    ["version 2", "kind: run\nversion: 2\n", "version is not 1"],
    ["an unknown key", "kind: run\nnope: 1\n", "an unknown field under the top"],
    ["a target field", "kind: run\ncore: {amf: x}\n", "an unknown field under the top"],
    ["an unknown workload field", pop + "    nope: 1\n", "an unknown field under workloads[0]"],
    ["a workload with no name", "kind: run\nworkloads:\n  - attach: {count: 1}\n", "workloads[0] has no name"],
    ["workloads not a list", "kind: run\nworkloads: {pop: {}}\n", "workloads is not a list"],
    ["a mapping where a value goes", pop + "    attach: {count: {a: 1}}\n", "workloads[0].attach.count is not a single value"],
    ["a tag", pop + "    attach: {count: !!int 1}\n", "has a YAML tag"],
    ["a tagged key", "kind: run\n!!str until: {complete: true}\n", "has a YAML tag"],
    ["an alias", "kind: run\nrun: {name: &a x}\nuntil: {duration: *a}\n", "anchors, aliases"],
    ["a merge key", "kind: run\nx: &m {complete: true}\nuntil: {<<: *m}\n", "anchors, aliases"],
    ["two documents", "kind: run\n---\nkind: run\n", "one YAML document"],
    ["a syntax error", "kind: run\nworkloads: [\n", /line \d+, column \d+/],
    // Whole values: their own fields.
    ["an unknown behaviour", pop + "    behaviors: {teleport: {}}\n", "an unknown field under workloads[0].behaviors"],
    ["a bad handover", pop + "    behaviors: {mobility: {handover: WARP}}\n", "is not one of HANDOVER_KIND_XN"],
    ["a token written out", pop + "    behaviors: {traffic: {bulk: {token: abc123}}}\n", "holds a secret; write it as a ${VAR}"],
    ["a secret-named app param written out", pop + "    behaviors: {traffic: {mix: [{name: m, app: {app: x, params: {X-Api-Key: k1}}}]}}\n", "holds a secret"],
    // Names Go reads as secrets, though not shaped like a field: a dot or a
    // space splits words there too.
    ["an api.key param written out", pop + "    behaviors: {traffic: {mix: [{name: m, app: {app: x, params: {api.key: 00112233445566778899aabbccddeeff}}}]}}\n", "holds a secret"],
    ["a 'my token' param written out", pop + '    behaviors: {traffic: {mix: [{name: m, app: {app: x, params: {"my token": t0k}}}]}}\n', "holds a secret"],
    ["a token as a list", pop + "    behaviors: {traffic: {bulk: {token: [a]}}}\n", "is not a single value"],
    ["a bad start phase", pop + "    start: {after: x, phase: never}\n", "is not one of started"],
    ["an unknown start key", pop + "    start: {before: x}\n", "an unknown field under workloads[0].start"],
    ["an expected path", "kind: run\nworkloads:\n  - name: s\n    conformance: {expected: lab.yaml}\n", "pick the file"],
    ["an unknown expected key", "kind: run\nworkloads:\n  - name: s\n    conformance: {expected: {core: c, vendor: x}}\n", "an unknown field under workloads[0].conformance.expected"],
    ["an unknown entry key", "kind: run\nworkloads:\n  - name: s\n    conformance: {expected: {entries: [{check: X, why: y}]}}\n", "an unknown field under workloads[0].conformance.expected.entries[0]"],
  ];
  for (const [name, text, why] of rows) it(`refuses ${name}`, () => refused(text, why));

  it("allows a token as a ${VAR}, and behaviors: {}", () => {
    ok(pop + '    behaviors: {traffic: {bulk: {ues: 1, token: "${APP_TOKEN}"}}}\n');
    ok(pop + "    behaviors: {}\n");
  });

  it("allows an inline expected list and a start", () => {
    ok(
      "kind: run\nworkloads:\n  - name: pop\n  - name: s\n    start: {after: pop, phase: attached}\n    conformance:\n      expected: {core: c, version: v, entries: [{check: X, observed: o, finding: EX-1, added: 2026-10-28, reason: r}]}\n",
    );
  });

  it("never quotes the file in a reason", () => {
    for (const text of [
      pop + "    behaviors: {traffic: {bulk: {token: s3cr3t}}}\n",
      pop + "    s3cr3t: 1\n",
      "kind: run\ns3cr3t: {a: 1}\n",
      pop + "    behaviors: {traffic: {mix: [{name: m, app: {app: x, params: {s3cr3t_key: s3cr3t}}}]}}\n",
    ]) {
      try {
        parseRun(text);
        throw new Error("not refused");
      } catch (e) {
        expect(String(e)).not.toContain("s3cr3t");
      }
    }
  });

  it("refuses more than 1 MiB", () => refused("kind: run\n# " + "x".repeat(1024 * 1024) + "\n", "larger than 1 MiB"));
});
