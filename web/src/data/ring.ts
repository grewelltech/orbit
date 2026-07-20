/**
 * Fixed-capacity ring buffer for time-series samples.
 *
 * A monitoring dashboard left open for hours will ingest millions of samples;
 * an unbounded array is a guaranteed leak. Capacity is fixed at construction
 * and the oldest sample is dropped on overflow — bounded memory regardless of
 * run length.
 *
 * Writes are O(1) and allocation-free after construction, so ingest can run at
 * stream rate without producing garbage for the collector to chase.
 */
export class Ring<T> {
  private readonly buf: (T | undefined)[];
  private head = 0; // next write index
  private len = 0;

  constructor(readonly capacity: number) {
    if (capacity < 1) throw new Error(`Ring capacity must be >= 1, got ${capacity}`);
    this.buf = new Array<T | undefined>(capacity);
  }

  get size(): number {
    return this.len;
  }

  push(v: T): void {
    this.buf[this.head] = v;
    this.head = (this.head + 1) % this.capacity;
    if (this.len < this.capacity) this.len++;
  }

  /** Most recently pushed value, or undefined when empty. */
  last(): T | undefined {
    if (this.len === 0) return undefined;
    return this.buf[(this.head - 1 + this.capacity) % this.capacity];
  }

  /** Oldest-to-newest copy. Allocates — call once per render, not per sample. */
  toArray(): T[] {
    const out = new Array<T>(this.len);
    const start = (this.head - this.len + this.capacity) % this.capacity;
    for (let i = 0; i < this.len; i++) {
      out[i] = this.buf[(start + i) % this.capacity] as T;
    }
    return out;
  }

  /** Oldest-to-newest projection, avoiding an intermediate array. */
  map<U>(fn: (v: T, i: number) => U): U[] {
    const out = new Array<U>(this.len);
    const start = (this.head - this.len + this.capacity) % this.capacity;
    for (let i = 0; i < this.len; i++) {
      out[i] = fn(this.buf[(start + i) % this.capacity] as T, i);
    }
    return out;
  }

  clear(): void {
    this.buf.fill(undefined);
    this.head = 0;
    this.len = 0;
  }
}
