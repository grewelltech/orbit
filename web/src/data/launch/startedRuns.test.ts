import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError } from "@connectrpc/connect";
import { GetRunResponseSchema, RunState } from "@/gen/orbit/v1/run_pb";
import type { KeyValueStore } from "../targets/store";
import { loadStarted, pollStarted, recordStarted, settled, STARTED_KEY, STARTED_MAX, type StartedClient } from "./startedRuns";

function memory(init: Record<string, string> = {}): KeyValueStore {
  const m = new Map(Object.entries(init));
  return { getItem: (k) => m.get(k) ?? null, setItem: (k, v) => void m.set(k, v), removeItem: (k) => void m.delete(k) };
}

describe("the runs this tab started", () => {
  it("newest first, the last few, a run once", () => {
    const s = memory();
    for (let i = 1; i <= STARTED_MAX + 2; i++) recordStarted(s, { id: `run-${i}`, name: `n${i}` });
    // What the tab stores is capped, not only what is read back.
    expect((JSON.parse(s.getItem(STARTED_KEY) ?? "[]") as unknown[]).length).toBe(STARTED_MAX);
    recordStarted(s, { id: "run-5", name: "n5" });
    expect(loadStarted(s).map((r) => r.id)).toEqual(["run-5", "run-7", "run-6", "run-4", "run-3"]);
  });

  it("kept in the tab across a reload (the same store, read afresh)", () => {
    const s = memory();
    recordStarted(s, { id: "run-1", name: "" });
    expect(loadStarted(s)).toEqual([{ id: "run-1", name: "" }]);
  });

  it("nothing from a store that holds something else, or none", () => {
    expect(loadStarted(memory({ [STARTED_KEY]: "{not json" }))).toEqual([]);
    expect(loadStarted(memory({ [STARTED_KEY]: '[{"id": 1}]' }))).toEqual([]);
    expect(loadStarted(undefined)).toEqual([]);
  });
});

describe("reading the started runs' states", () => {
  /** A server answering each id from answers (a state, or a refusal), and the ids it was asked. */
  function server(answers: Record<string, RunState | ConnectError>) {
    const asked: string[] = [];
    const client: StartedClient = {
      getRun: async ({ runId }) => {
        asked.push(runId);
        const a = answers[runId];
        if (a instanceof ConnectError) throw a;
        return create(GetRunResponseSchema, { run: { runId, state: a } });
      },
    };
    return { client, asked };
  }
  const runs = ["run-1", "run-2", "run-3", "run-4", "run-5"].map((id) => ({ id, name: "" }));

  it("each run's state; a run the server does not know is gone; a refusal of who is asking is an auth error, not gone", async () => {
    const { client } = server({
      "run-1": RunState.RUNNING,
      "run-2": RunState.CANCELLED,
      "run-3": new ConnectError("no run run-3", Code.NotFound),
      "run-4": new ConnectError("token required", Code.Unauthenticated),
      "run-5": new ConnectError("connection refused", Code.Unavailable),
    });
    expect(await pollStarted(client, runs, {})).toEqual({
      "run-1": "running",
      "run-2": "cancelled",
      "run-3": "gone",
      "run-4": "auth error: token required",
      "run-5": "unreachable: connection refused",
    });
  });

  it("a settled run (ended, or gone) is not read again; the rest are", async () => {
    const { client, asked } = server({ "run-1": RunState.COMPLETE, "run-4": RunState.RUNNING, "run-5": RunState.RUNNING });
    const prev = { "run-1": "running", "run-2": "failed", "run-3": "gone", "run-4": "auth error: token required", "run-5": "unreachable: x" };
    const got = await pollStarted(client, runs, prev);
    expect(asked).toEqual(["run-1", "run-4", "run-5"]);
    expect(got).toEqual({ "run-1": "complete", "run-2": "failed", "run-3": "gone", "run-4": "running", "run-5": "running" });
  });

  it("settled: ended or gone, not in flight, not an error that may pass", () => {
    for (const s of ["complete", "failed", "cancelled", "gone"]) expect(settled(s), s).toBe(true);
    for (const s of ["starting", "running", "draining", "auth error: x", "unreachable: x", "?"]) expect(settled(s), s).toBe(false);
  });
});
