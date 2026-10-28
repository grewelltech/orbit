import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient, createRouterTransport } from "@connectrpc/connect";
import { RunService, StopRunResponseSchema, type StopRunRequest } from "@/gen/orbit/v1/run_pb";
import { stopConfirm, stoppable, stopRun, type StopState } from "./stop";

function server(fail?: string) {
  const got: StopRunRequest[] = [];
  const transport = createRouterTransport(({ service }) => {
    service(RunService, {
      stopRun(req) {
        got.push(req);
        if (fail) throw new ConnectError(fail, Code.NotFound);
        return create(StopRunResponseSchema, { run: { runId: req.runId } });
      },
    });
  });
  return { client: createClient(RunService, transport), got };
}

describe("Stop on the run view", () => {
  it("cancels the run (StopRun, not a finish): stopping, then stopped", async () => {
    const { client, got } = server();
    const seen: StopState[] = [];
    await stopRun(client, "run-1", (s) => seen.push(s));
    expect(got.map((r) => `${r.runId}:${r.finish}`)).toEqual(["run-1:false"]);
    expect(seen.map((s) => s.phase)).toEqual(["stopping", "stopped"]);
  });

  it("a refusal shows the server's reason", async () => {
    const { client } = server("no run run-9");
    const seen: StopState[] = [];
    await stopRun(client, "run-9", (s) => seen.push(s));
    expect(seen.at(-1)).toEqual({ phase: "failed", error: "no run run-9" });
  });

  it("is offered only for a run in flight", () => {
    for (const s of ["starting", "running", "draining"]) expect(stoppable(s), s).toBe(true);
    for (const s of ["idle", "complete", "failed", undefined]) expect(stoppable(s), String(s)).toBe(false);
  });

  it("finishes the run (StopRun finish: true), the confirm saying the gates are judged", async () => {
    const { client, got } = server();
    const asked: string[] = [];
    await stopRun(client, "run-1", () => undefined, "finish", (text) => (asked.push(text), true));
    expect(got.map((r) => `${r.runId}:${r.finish}`)).toEqual(["run-1:true"]);
    expect(asked).toEqual([stopConfirm("finish", "run-1")]);
    expect(asked[0]).toContain("gates are judged");
  });

  it("the cancel's confirm says the gates fail as did-not-complete", () => {
    expect(stopConfirm("cancel", "run-1")).toContain("failed as did-not-complete");
  });

  it("a declined confirm sends nothing and changes nothing", async () => {
    const { client, got } = server();
    const seen: StopState[] = [];
    for (const mode of ["finish", "cancel"] as const) await stopRun(client, "run-1", (s) => seen.push(s), mode, () => false);
    expect(got).toHaveLength(0);
    expect(seen).toEqual([]);
  });
});
