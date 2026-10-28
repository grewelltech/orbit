import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient, createRouterTransport } from "@connectrpc/connect";
import { ProbeStepSchema, TargetService } from "@/gen/orbit/v1/target_pb";
import { ended, IDLE, keysBlocked, ProbeRunner, refusedField, summary, withStep, type ProbeState } from "./probe";

const step = (index: number, status: string) => create(ProbeStepSchema, { index, name: `step ${index}`, status });

describe("keysBlocked", () => {
  const rows: [string, string, string, boolean][] = [
    ["https, a token", "https:", "orbit.lab", true],
    ["http on loopback", "http:", "127.0.0.1", true],
    ["http on localhost", "http:", "localhost", true],
    ["http on [::1]", "http:", "[::1]", true],
    ["http across the network", "http:", "192.0.2.10", false],
    ["http by name", "http:", "orbit.lab", false],
  ];
  for (const [name, protocol, hostname, ok] of rows) {
    it(name, () => expect(keysBlocked({ protocol, hostname }, "tok") === undefined).toBe(ok));
  }
  it("no token", () => expect(keysBlocked({ protocol: "https:", hostname: "orbit.lab" }, "")).toContain("no API token"));
  it("says why, as the CLI does", () => expect(keysBlocked({ protocol: "http:", hostname: "192.0.2.10" }, "t")).toContain("--tls auto"));
});

describe("the probe's state", () => {
  it("steps in index order; the summary counts OK and SKIP", () => {
    let s: ProbeState = { phase: "running", steps: [] };
    for (const x of [step(2, "OK"), step(1, "OK"), step(3, "FAIL"), step(4, "SKIP")]) s = withStep(s, x);
    expect(s.steps.map((x) => x.index)).toEqual([1, 2, 3, 4]);
    s = ended(s, undefined, false);
    expect(summary(s, false)).toBe("2 of 4 steps passed (1 skipped)");
    expect(summary(IDLE, false)).toBeUndefined();
  });

  it("core only, all OK: the CLI's line", () => {
    const s = ended({ phase: "running", steps: [step(1, "OK"), step(2, "OK"), step(3, "OK")] }, undefined, false);
    expect(summary(s, true)).toBe("core OK; user plane not checked (core only)");
  });

  it("an InvalidArgument names the field, in the server's form (internal/server's TestProbeSentTarget pins it)", () => {
    const err = new ConnectError(`invalid target: core.slices: [1/xyz] SD "xyz" is not 6 hex digits`, Code.InvalidArgument);
    const s = ended({ phase: "running", steps: [] }, err, false);
    expect(s.phase).toBe("failed");
    expect(s.field).toEqual({ path: "core.slices", reason: `[1/xyz] SD "xyz" is not 6 hex digits` });
    expect(refusedField("invalid target: subscribers[main].ki: <redacted> is not 32 hex digits")).toEqual({
      path: "subscribers[main].ki",
      reason: "<redacted> is not 32 hex digits",
    });
    expect(ended({ phase: "running", steps: [] }, new ConnectError("held by run-7", Code.FailedPrecondition), false).field).toBeUndefined();
  });

  it("an abort is cancelled, not a failure", () => {
    expect(ended({ phase: "running", steps: [] }, new Error("aborted"), true).phase).toBe("cancelled");
  });
});

describe("ProbeRunner over a server stream", () => {
  /** A TargetService whose probe sends two steps, then waits until the client goes. */
  function server() {
    let closed = false;
    const transport = createRouterTransport(({ service }) => {
      service(TargetService, {
        async *probe(_req, ctx) {
          yield step(1, "OK");
          yield step(2, "OK");
          await new Promise<void>((resolve) => ctx.signal.addEventListener("abort", () => resolve()));
          closed = true;
        },
      });
    });
    return { client: createClient(TargetService, transport), closed: () => closed };
  }

  it("streams steps as they come; dispose (leaving the page) ends the stream, as cancelled", async () => {
    const { client, closed } = server();
    let s: ProbeState = IDLE;
    const r = new ProbeRunner(client, (f) => (s = f(s)));
    const done = r.run({ targetName: "lab1" });
    for (let i = 0; i < 50 && s.steps.length < 2; i++) await new Promise((res) => setTimeout(res, 5));
    expect(s.steps.map((x) => x.index)).toEqual([1, 2]);
    expect(s.phase).toBe("running");
    r.dispose();
    await done;
    expect(r.signal?.aborted).toBe(true);
    expect(s.phase).toBe("cancelled");
    for (let i = 0; i < 50 && !closed(); i++) await new Promise((res) => setTimeout(res, 5));
    expect(closed()).toBe(true);
  });

  it("a new probe aborts the one before", async () => {
    const { client } = server();
    let s: ProbeState = IDLE;
    const r = new ProbeRunner(client, (f) => (s = f(s)));
    const first = r.run({});
    const sig = r.signal;
    const second = r.run({});
    expect(sig?.aborted).toBe(true);
    r.cancel();
    await Promise.all([first, second]);
    expect(s.phase).toBe("cancelled");
  });
});
