import { describe, expect, it } from "vitest";
import { readPendingImport } from "./importlink";

const KI = "5122250214c33e723a5dd523fc145fc0";

/** base64url (no padding), as the testbed writes it: base64 with +/ -> -_ and = stripped. */
function b64url(text: string): string {
  const b64 = btoa(String.fromCharCode(...new TextEncoder().encode(text)));
  return b64.replace(/\+/g, "-").replace(/\//g, "_").replace(/=+$/, "");
}

const withRef =
  "version: 1\nkind: target\nname: lxd\nsubscribers:\n  - name: main\n    first_supi: \"001010000000001\"\n    count: 10\n    ki: ${ORBIT_KI}\n";
const withKey =
  "version: 1\nkind: target\nname: lxd\nsubscribers:\n  - name: main\n    first_supi: \"001010000000001\"\n    count: 10\n    ki: " + KI + "\n";
// A target file with several targets is one YAML document: a list, not
// `---`-separated documents (docs/targets.md), which the testbed's import
// link writes as a sequence.
const twoDocs =
  "- version: 1\n  kind: target\n  name: lxd\n  subscribers:\n    - name: main\n      first_supi: \"001010000000001\"\n      count: 10\n      ki: " +
  KI +
  "\n- version: 1\n  kind: target\n  name: lxd-load\n  core:\n    tac: 1\n";

describe("readPendingImport", () => {
  it("is undefined for no fragment or a non-import one", () => {
    expect(readPendingImport("")).toBeUndefined();
    expect(readPendingImport("#run=abc")).toBeUndefined();
    expect(readPendingImport("#import=")).toBeUndefined();
  });

  it("decodes a base64url payload and lists its target names", () => {
    const r = readPendingImport("#import=" + b64url(withRef));
    expect(r && "text" in r && r.names).toEqual(["lxd"]);
    expect(r && "text" in r && r.text).toContain("name: lxd");
  });

  it("reads several documents and reports each name", () => {
    const r = readPendingImport("#import=" + b64url(twoDocs));
    expect(r && "text" in r && r.names).toEqual(["lxd", "lxd-load"]);
  });

  it("hasKeys is false when every secret is a ${VAR}, true for a literal key", () => {
    const ref = readPendingImport("#import=" + b64url(withRef));
    expect(ref && "text" in ref && ref.hasKeys).toBe(false);
    const key = readPendingImport("#import=" + b64url(withKey));
    expect(key && "text" in key && key.hasKeys).toBe(true);
  });

  it("refuses a payload that is not base64url", () => {
    const r = readPendingImport("#import=!!!!not base64!!!!");
    expect(r && "refusal" in r && r.refusal).toMatch(/base64url/);
  });

  it("refuses a payload that does not parse as a target file", () => {
    const r = readPendingImport("#import=" + b64url("version: 1\nkind: target\nname: [unterminated\n"));
    expect(r && "refusal" in r).toBe(true);
  });

  it("works whether or not the hash keeps its leading #", () => {
    const withHash = readPendingImport("#import=" + b64url(withRef));
    const noHash = readPendingImport("import=" + b64url(withRef));
    expect(withHash).toEqual(noHash);
  });
});
