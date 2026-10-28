/**
 * Polling a server read with backoff, as the run view's suite watch polls
 * GetRun: read every intervalMs; after a failure (the server restarting, a
 * token the prompt is asking for) wait twice as long each time, up to
 * maxBackoffMs, and back to intervalMs at the next answer. Only the signal
 * stops it.
 */
export interface PollOptions {
  intervalMs: number;
  maxBackoffMs: number;
  signal: AbortSignal;
}

/** Waits ms, or less if signal aborts first. */
function sleep(ms: number, signal: AbortSignal): Promise<void> {
  return new Promise((resolve) => {
    if (signal.aborted) return resolve();
    const t = setTimeout(done, ms);
    function done() {
      clearTimeout(t);
      signal.removeEventListener("abort", done);
      resolve();
    }
    signal.addEventListener("abort", done);
  });
}

/** Reads until o.signal aborts: each answer to onValue, each failure to onError. */
export async function pollWithBackoff<T>(read: () => Promise<T>, onValue: (v: T) => void, onError: (err: unknown) => void, o: PollOptions): Promise<void> {
  let wait = o.intervalMs;
  while (!o.signal.aborted) {
    try {
      const v = await read();
      if (o.signal.aborted) return;
      onValue(v);
      wait = o.intervalMs;
      await sleep(o.intervalMs, o.signal);
    } catch (err) {
      if (o.signal.aborted) return;
      onError(err);
      await sleep(wait, o.signal);
      wait = Math.min(wait * 2, o.maxBackoffMs);
    }
  }
}
