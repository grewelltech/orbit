import { describe, expect, it } from "vitest";
import { create, type MessageInitShape } from "@bufbuild/protobuf";
import { RunKind, RunSchema, RunState } from "@/gen/orbit/v1/run_pb";
import { Code, ConnectError } from "@connectrpc/connect";
import { __test, pollProblem, problemNotice } from "./runs";

const { summarise } = __test;

function run(fields: MessageInitShape<typeof RunSchema> = {}) {
  return create(RunSchema, { runId: "run-1", ...fields });
}

describe("summarise", () => {
  it("names every kind the proto defines", () => {
    // Guards against a numeric shortcut silently mislabelling later kinds.
    expect(summarise(run({ kind: RunKind.LOAD })).kind).toBe("load");
    expect(summarise(run({ kind: RunKind.FLEET })).kind).toBe("fleet");
    expect(summarise(run({ kind: RunKind.SCENARIO })).kind).toBe("scenario");
    expect(summarise(run({ kind: RunKind.CONFORMANCE })).kind).toBe("conformance");
    // MIXED must not fall through to "kind-5", which dropped a mixed run's
    // conformance half in the dashboard.
    expect(summarise(run({ kind: RunKind.MIXED })).kind).toBe("mixed");
  });

  it("treats pending, running and draining as active", () => {
    // "Active" drives the live/history distinction in the picker, so a
    // draining run must not read as finished while it is still emitting.
    for (const s of [RunState.PENDING, RunState.RUNNING, RunState.DRAINING]) {
      expect(summarise(run({ state: s })).active).toBe(true);
    }
    for (const s of [RunState.COMPLETE, RunState.FAILED, RunState.CANCELLED]) {
      expect(summarise(run({ state: s })).active).toBe(false);
    }
  });

  it("converts nanosecond stamps without overflowing", () => {
    // startedUnixNano is an int64/bigint; dividing before Number() keeps it
    // inside the safe-integer range.
    const s = summarise(run({ startedUnixNano: 1786025863806031318n }));
    expect(s.startedAt).toBe(1786025863806);
    expect(Number.isSafeInteger(s.startedAt)).toBe(true);
  });

  it("falls back to the id when a run has no name", () => {
    expect(summarise(run({ name: "" })).name).toBe("");
    expect(summarise(run({ name: "" })).runId).toBe("run-1");
  });
});

describe("poll problems", () => {
  it("tells a token refusal from an unreachable server", () => {
    expect(pollProblem(new ConnectError("a bearer token is required", Code.Unauthenticated))).toBe("unauthorized");
    expect(pollProblem(new ConnectError("locked", Code.ResourceExhausted))).toBe("unauthorized");
    expect(pollProblem(new ConnectError("HTTP status 502 Bad Gateway", Code.Unavailable))).toBe("unreachable");
    expect(pollProblem(new TypeError("Failed to fetch"))).toBe("unreachable");
  });

  it("never says the server is unreachable while it waits for the token", () => {
    for (const loaded of [false, true]) {
      const n = problemNotice("unauthorized", loaded);
      expect(n?.text).toMatch(/token/);
      expect(n?.text).not.toMatch(/reach/);
      expect(n?.warn).toBe(true);
    }
  });

  it("reports an unreachable server, as a warning once a list was loaded", () => {
    expect(problemNotice("unreachable", false)).toEqual({ text: "can't reach the server — retrying…", warn: false });
    expect(problemNotice("unreachable", true)?.warn).toBe(true);
    expect(problemNotice("unreachable", true)?.text).toMatch(/last known runs/);
  });

  it("shows nothing when the poll succeeded", () => {
    expect(problemNotice(null, false)).toBeNull();
    expect(problemNotice(null, true)).toBeNull();
  });
});
