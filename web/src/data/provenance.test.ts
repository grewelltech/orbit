import { readdirSync, readFileSync } from "node:fs";
import { join } from "node:path";
import { describe, expect, it } from "vitest";
import { fromJson } from "@bufbuild/protobuf";
import { GetRunConfigResponseSchema } from "@/gen/orbit/v1/run_pb";
import { ALL_ROWS, filterRows, madeBy, MASKED, noticeText, rows } from "./provenance";

/**
 * Real GetRunConfig answers, written by Go under -update-runconfig:
 * env-over-target (internal/cli TestEnvOverTargetOnAPastRun: ORBIT_MCC and
 * ORBIT_MNC beat an AMF-only target's 001/01) and keys-and-leases
 * (internal/server TestGetRunConfig: keys, a key a layer overrode, and the
 * server's own entries); highest-file-and-same-value (internal/plan: a
 * notice naming the run file over the target, and one whose values match).
 */
const dir = join(__dirname, "testdata", "runconfig");
const read = (name: string) => fromJson(GetRunConfigResponseSchema, JSON.parse(readFileSync(join(dir, `${name}.json`), "utf8")));
const envOverTarget = read("env-over-target");
const keys = read("keys-and-leases");

describe("the Resolved config panel's rows", () => {
  it("one row per field, with its layer and source; what it beat struck under it, lowest first", () => {
    const mcc = rows(envOverTarget.provenance).find((r) => r.path === "core.plmn.mcc");
    expect(mcc).toMatchObject({ value: "208", layer: "env", source: "env $ORBIT_MCC", commandLine: true });
    expect(mcc?.struck).toEqual([{ value: "001", layer: "target", source: "target lab/orbit.yaml:6" }]);
    const amf = rows(envOverTarget.provenance).find((r) => r.path === "core.amf");
    expect(amf).toMatchObject({ layer: "target", commandLine: false, struck: [] });
  });

  it("a secret's value and every value it beat are only ever the mask", () => {
    const ki = rows(keys.provenance).find((r) => r.path === "subscribers[main].ki");
    expect(ki?.value).toBe(MASKED);
    expect(ki?.struck.length).toBeGreaterThan(0);
    for (const s of ki?.struck ?? []) expect(s.value).toBe(MASKED);
    // Nowhere in any row is a value that looks like a key.
    for (const r of [...rows(keys.provenance), ...rows(envOverTarget.provenance)]) {
      for (const v of [r.value, ...r.struck.map((s) => s.value)]) expect(v).not.toMatch(/^[0-9a-f]{32}$/i);
    }
  });

  it("the server's own entries show as the server's", () => {
    expect(rows(keys.provenance).some((r) => r.layer === "server")).toBe(true);
  });

  it("filters: text in the path, only what was overridden, only env or flag", () => {
    const rs = rows(envOverTarget.provenance);
    expect(filterRows(rs, { ...ALL_ROWS, text: "PLMN" }).map((r) => r.path)).toEqual(["core.plmn.mcc", "core.plmn.mnc"]);
    expect(filterRows(rs, { ...ALL_ROWS, commandLineOnly: true }).map((r) => r.path)).toEqual(["core.plmn.mcc", "core.plmn.mnc"]);
    const overridden = filterRows(rs, { ...ALL_ROWS, overriddenOnly: true });
    expect(overridden.length).toBeGreaterThan(0);
    expect(overridden.every((r) => r.struck.length > 0)).toBe(true);
    expect(filterRows(rs, ALL_ROWS)).toHaveLength(rs.length);
  });
});

/**
 * Every fixture's notices, each as Go's plan.NoticeText words it: written
 * next to the fixture (<name>.text.json) by the same -update-runconfig run.
 */
const fixtures = readdirSync(dir)
  .filter((f) => f.endsWith(".json") && !f.endsWith(".text.json"))
  .map((f) => f.slice(0, -".json".length));

describe("the notices", () => {
  it("cover a notice of each kind: env over a target, over a run file, and the same value", () => {
    const all = fixtures.flatMap((name) => JSON.parse(readFileSync(join(dir, `${name}.text.json`), "utf8")) as string[]);
    expect(all.some((t) => / overrode target .* \(001\)$/.test(t))).toBe(true);
    expect(all.some((t) => / overrode run file /.test(t))).toBe(true);
    expect(all.some((t) => t.endsWith(" (same value)"))).toBe(true);
  });

  it.each(fixtures)("in %s, say exactly what the CLI says", (name) => {
    const want = JSON.parse(readFileSync(join(dir, `${name}.text.json`), "utf8")) as string[];
    expect(read(name).notices.map(noticeText)).toEqual(want);
  });
});

describe("what the plan says of how it was made (worded from the plan, not the client)", () => {
  it("env or flag layers", () => expect(madeBy(envOverTarget.provenance, true)).toBe("this plan has env or flag layers"));
  it("none", () => expect(madeBy(keys.provenance, true)).toBe("no env or flag layers"));
  it("without the resolver", () => expect(madeBy([], false)).toContain("started without the resolver"));
});
