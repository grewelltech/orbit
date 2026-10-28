import { describe, expect, it } from "vitest";
import { create } from "@bufbuild/protobuf";
import { Code, ConnectError, createClient, createRouterTransport } from "@connectrpc/connect";
import { ProbeStepSchema, TargetService } from "@/gen/orbit/v1/target_pb";
import { ended, IDLE, keysBlocked, ProbeRunner, refusedField, summary, varsNote, withStep, type ProbeState } from "./probe";
import { authInterceptor, type AuthRefusal } from "../transport";

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
  it("no token, from another host over https", () => expect(keysBlocked({ protocol: "https:", hostname: "orbit.lab" }, "")).toContain("no API token"));
  // On the server's own host no token is asked for: a server that wants
  // one answers Unauthenticated, and the token prompt opens.
  for (const hostname of ["127.0.0.1", "localhost", "[::1]"]) {
    it(`no token on loopback (${hostname})`, () => expect(keysBlocked({ protocol: "http:", hostname }, "")).toBeUndefined());
  }
  it("no token, http across the network: still the TLS reason", () => expect(keysBlocked({ protocol: "http:", hostname: "192.0.2.10" }, "")).toContain("--tls auto"));
  it("says why, as the CLI does", () => expect(keysBlocked({ protocol: "http:", hostname: "192.0.2.10" }, "t")).toContain("--tls auto"));
});

describe("the panel's ${VAR} line", () => {
  it("held in this tab", () => expect(varsNote(false)).toContain("held in this tab"));
  it("remembered in this browser when the target remembers its keys", () => {
    expect(varsNote(true)).toContain("remembered in this browser");
    expect(varsNote(true)).not.toContain("this tab");
  });
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

  const until = async (ok: () => boolean) => {
    for (let i = 0; i < 50 && !ok(); i++) await new Promise((res) => setTimeout(res, 5));
  };

  it("streams steps as they come; Cancel ends the stream, as cancelled", async () => {
    const { client, closed } = server();
    let s: ProbeState = IDLE;
    const r = new ProbeRunner(client, (f) => (s = f(s)));
    const done = r.run({ targetName: "lab1" });
    await until(() => s.steps.length >= 2);
    expect(s.steps.map((x) => x.index)).toEqual([1, 2]);
    expect(s.phase).toBe("running");
    r.cancel();
    await done;
    expect(r.signal?.aborted).toBe(true);
    expect(s.phase).toBe("cancelled");
    await until(closed);
    expect(closed()).toBe(true);
  });

  it("dispose (leaving the page) ends the stream and writes nothing after it", async () => {
    const { client, closed } = server();
    let s: ProbeState = IDLE;
    let writes = 0;
    const r = new ProbeRunner(client, (f) => ((s = f(s)), writes++));
    const done = r.run({ targetName: "lab1" });
    await until(() => s.steps.length >= 2);
    const before = writes;
    r.dispose();
    await done;
    await until(closed);
    expect(closed()).toBe(true);
    expect(writes).toBe(before);
  });

  it("another target mid-probe: the old stream's end is not shown on the new target", async () => {
    // useProbe on a target change: dispose, then the new target's IDLE.
    const { client, closed } = server();
    let s: ProbeState = IDLE;
    const r = new ProbeRunner(client, (f) => (s = f(s)));
    const done = r.run({ targetName: "lab1" });
    await until(() => s.steps.length >= 2);
    r.dispose();
    s = IDLE;
    await done;
    await until(closed);
    expect(s).toEqual(IDLE);
  });

  it("a new probe aborts the one before; the first's end is not written on the second", async () => {
    const { client } = server();
    let s: ProbeState = IDLE;
    const r = new ProbeRunner(client, (f) => (s = f(s)));
    const first = r.run({});
    const sig = r.signal;
    const second = r.run({});
    expect(sig?.aborted).toBe(true);
    await first;
    expect(s.phase).toBe("running");
    await until(() => s.steps.length >= 2);
    r.cancel();
    await second;
    expect(s.phase).toBe("cancelled");
  });

  it("a server that wants a token: Unauthenticated opens the token prompt, and the probe fails", async () => {
    const refused: AuthRefusal[] = [];
    const transport = createRouterTransport(
      ({ service }) => {
        service(TargetService, {
          async *probe() {
            throw new ConnectError("missing token", Code.Unauthenticated);
          },
        });
      },
      { transport: { interceptors: [authInterceptor(() => "", (why) => refused.push(why))] } },
    );
    let s: ProbeState = IDLE;
    await new ProbeRunner(createClient(TargetService, transport), (f) => (s = f(s))).run({});
    expect(refused).toEqual(["missing"]);
    expect(s.phase).toBe("failed");
  });
});
