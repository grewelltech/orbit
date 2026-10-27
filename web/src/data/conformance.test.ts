import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import {
  ConformanceResultSchema,
  ConformanceTallySchema,
  ConformanceUpdateSchema,
  StaleExpectationSchema,
} from "@/gen/orbit/v1/conformance_pb";
import { ExpectedFailureSchema } from "@/gen/orbit/v1/plan_pb";
import {
  applyUpdate,
  buildRequest,
  failedLabel,
  identityComplete,
  paramsFromIdentity,
  DEFAULT_IDENTITY,
} from "./conformance";

describe("conformance request", () => {
  it("sends every identity field the user typed, PLMN as strings", () => {
    // The server has no PLMN default any more, so the dashboard must send
    // what the form holds — and MNC "01" must stay "01", never become 1.
    const req = buildRequest(
      paramsFromIdentity("10.0.0.1:38412", { mcc: "001", mnc: "01", tac: "7", sst: "2", sd: "abcdef" }),
    );
    expect(req.amfAddress).toBe("10.0.0.1:38412");
    expect(req.mcc).toBe("001");
    expect(req.mnc).toBe("01");
    expect(req.tac).toBe(7);
    expect(req.sst).toBe(2);
    expect(req.sd).toBe("abcdef");
  });

  it("sends an empty SD when the user leaves it blank (no SD, not a placeholder)", () => {
    const req = buildRequest(paramsFromIdentity("a:1", { ...DEFAULT_IDENTITY, mcc: "001", mnc: "01" }));
    expect(req.sd).toBe("");
    expect(req.tac).toBe(1);
    expect(req.sst).toBe(1);
  });

  it("refuses to run without a PLMN", () => {
    expect(identityComplete(DEFAULT_IDENTITY)).toBe(false);
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001" })).toBe(false);
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01" })).toBe(true);
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", tac: "x" })).toBe(false);
    // Only plain integers within the server's bounds are sent: TAC 1..0xFFFFFF, SST 0..255.
    for (const bad of ["1.5", "-3", "1e3", "", "0x10"]) {
      expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", tac: bad })).toBe(false);
      expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", sst: bad })).toBe(false);
    }
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", tac: "0" })).toBe(false);
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", tac: "16777216" })).toBe(false);
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", sst: "256" })).toBe(false);
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", sst: "0" })).toBe(true);
    expect(identityComplete({ ...DEFAULT_IDENTITY, mcc: "001", mnc: "01", tac: "16777215", sst: "255" })).toBe(true);
  });
});

describe("expected failures", () => {
  const base = {
    results: [],
    tally: undefined,
    expectedCore: "",
    stale: [],
    progress: undefined,
    running: true,
    done: false,
    error: undefined,
  };

  it("keeps the list's core and takes stale entries from the final update", () => {
    const mid = applyUpdate(
      base,
      create(ConformanceUpdateSchema, {
        index: 0,
        total: 2,
        expectedFailuresCore: "Example core",
        result: create(ConformanceResultSchema, {
          id: "A",
          verdict: "FAIL",
          expectedFailure: create(ExpectedFailureSchema, { finding: "EX-1", added: "2026-10-12", reason: "example" }),
        }),
      }),
    );
    expect(mid.expectedCore).toBe("Example core");
    expect(mid.results[0]?.verdict).toBe("FAIL");
    expect(mid.results[0]?.expectedFailure?.finding).toBe("EX-1");
    expect(mid.stale).toEqual([]);

    const done = applyUpdate(
      mid,
      create(ConformanceUpdateSchema, {
        done: true,
        expectedFailuresCore: "Example core",
        stale: [create(StaleExpectationSchema, { check: "B", finding: "EX-2", verdict: "PASS", observed: "ok" })],
      }),
    );
    expect(done.done).toBe(true);
    expect(done.stale.map((s) => s.finding)).toEqual(["EX-2"]);
  });

  it("labels the failed count with the expected ones only when a list applies", () => {
    const tally = create(ConformanceTallySchema, { failed: 3, expectedFailures: 2 });
    expect(failedLabel(tally, "Example core")).toBe("failed (2 expected)");
    expect(failedLabel(tally, "")).toBe("failed");
    expect(failedLabel(undefined, "Example core")).toBe("failed (0 expected)");
  });
});
