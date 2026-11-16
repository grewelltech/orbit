import { afterEach, describe, expect, it, vi } from "vitest";
import { Code, ConnectError, createRouterTransport } from "@connectrpc/connect";
import { RunService, RunState } from "@/gen/orbit/v1/run_pb";
import { ConnectSource } from "./connect";
import type { SourceState } from "./types";

afterEach(() => {
  vi.useRealTimers();
});

/** Collects a source's state changes, without repeats. */
function watch(src: ConnectSource): SourceState[] {
  const seen: SourceState[] = [];
  src.onState((s) => {
    if (seen[seen.length - 1] !== s) seen.push(s);
  });
  return seen;
}

describe("ConnectSource connection state", () => {
  it.each([
    ["a missing or wrong token", Code.Unauthenticated, "unauthorized"],
    ["a lockout", Code.ResourceExhausted, "unauthorized"],
    ["an unreachable server", Code.Unavailable, "disconnected"],
  ] as const)("reports %s on a refused listRuns as %s, then recovers", async (_name, code, want) => {
    vi.useFakeTimers();
    let refuse = true;
    const transport = createRouterTransport((router) =>
      router.service(RunService, {
        listRuns: () => {
          if (refuse) throw new ConnectError("refused", code);
          return { runs: [] };
        },
      }),
    );
    const src = new ConnectSource({ transport });
    const seen = watch(src);
    src.start();
    await vi.advanceTimersByTimeAsync(10);
    expect(src.state()).toBe(want);

    refuse = false; // the token was entered, or the server came back
    await vi.advanceTimersByTimeAsync(2500);
    expect(src.state()).toBe("connected");
    expect(seen).toEqual(["connecting", want, "connecting", "connected"]);
    src.stop();
  });

  it("reports a stream refused for its token as unauthorized, not as an error", async () => {
    vi.useFakeTimers();
    const transport = createRouterTransport((router) =>
      router.service(RunService, {
        listRuns: () => ({ runs: [{ runId: "r1", state: RunState.RUNNING }] }),
        async *runTelemetry() {
          throw new ConnectError("invalid bearer token", Code.Unauthenticated);
        },
        async *runEvents() {
          throw new ConnectError("invalid bearer token", Code.Unauthenticated);
        },
      }),
    );
    const src = new ConnectSource({ transport });
    const seen = watch(src);
    src.start();
    await vi.advanceTimersByTimeAsync(10);
    expect(src.state()).toBe("unauthorized");
    expect(seen).not.toContain("error");
    src.stop();
  });
});
