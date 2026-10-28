import { afterEach, describe, expect, it, vi } from "vitest";
import { pollWithBackoff } from "./poll";

describe("polling with backoff (fake timers)", () => {
  afterEach(() => vi.useRealTimers());

  it("the interval while it answers; after a failure twice as long each time, up to the cap; the interval again once it answers", async () => {
    vi.useFakeTimers();
    const calls: number[] = [];
    // Six failures, two answers, a failure, then answers.
    const script = [false, false, false, false, false, false, true, true, false, true, true];
    const ctrl = new AbortController();
    const seen: number[] = [];
    const errors: unknown[] = [];
    const done = pollWithBackoff(
      async () => {
        calls.push(Date.now());
        if (!(script[calls.length - 1] ?? true)) throw new Error("the server restarting");
        return calls.length;
      },
      (v) => seen.push(v),
      (e) => errors.push(e),
      { intervalMs: 100, maxBackoffMs: 1000, signal: ctrl.signal },
    );
    await vi.advanceTimersByTimeAsync(100 + 200 + 400 + 800 + 1000 + 1000 + 100 + 100 + 100 + 100);
    ctrl.abort();
    await done;
    const gaps = calls.slice(1).map((t, i) => t - (calls[i] ?? 0));
    expect(gaps.slice(0, 10)).toEqual([100, 200, 400, 800, 1000, 1000, 100, 100, 100, 100]);
    expect(errors.length).toBe(7);
    expect(seen.slice(0, 2)).toEqual([7, 8]);
  });

  it("abort stops it at once, mid-wait: no more reads", async () => {
    vi.useFakeTimers();
    let reads = 0;
    const ctrl = new AbortController();
    const done = pollWithBackoff(async () => ++reads, () => undefined, () => undefined, { intervalMs: 1000, maxBackoffMs: 5000, signal: ctrl.signal });
    await vi.advanceTimersByTimeAsync(10);
    ctrl.abort();
    await done;
    await vi.advanceTimersByTimeAsync(5000);
    expect(reads).toBe(1);
  });
});
