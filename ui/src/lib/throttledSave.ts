/**
 * Throttled persistence for the playground conversation.
 *
 * The conversation is a deep reactive object that is replaced on every streamed
 * token, so a naive `$effect` that serializes it runs once per token and costs
 * O(conversation length) each time — quadratic in a long chat. The throttle has
 * to gate the *write* without making the serialization per-token, which is what
 * `createThrottledSaver` does: it serializes only when it is actually about to
 * store.
 */

/** Minimum spacing between two writes, in milliseconds. */
export const SAVE_INTERVAL_MS = 2000;

export interface ThrottledSaver {
  /**
   * Records that the source changed. The write happens at most once per
   * interval; a change that arrives inside the window defers the write to the
   * end of that window rather than dropping it.
   */
  touch(): void;
  /** Writes immediately if a write is due, and returns whether it wrote. */
  flush(): boolean;
  /** Cancels a pending deferred write without writing. */
  cancel(): void;
}

/**
 * Builds a saver that writes `serialize()`'s result to `store` at most once per
 * `intervalMs`, and always keeps the latest value.
 *
 * The read of the source happens inside the write, not in the caller's reactive
 * scope, so a caller that touches on every change pays no serialization cost
 * until the write is actually due.
 */
export function createThrottledSaver<Handle = number>(
  store: (value: string) => void,
  serialize: () => string,
  intervalMs: number = SAVE_INTERVAL_MS,
  now: () => number = Date.now,
  schedule: (fn: () => void, ms: number) => Handle = ((fn, ms) =>
    (globalThis as { setTimeout?: (fn: () => void, ms: number) => number }).setTimeout?.(fn, ms)) as (
    fn: () => void,
    ms: number,
  ) => Handle,
  clear: (handle: Handle) => void = ((handle) =>
    (globalThis as { clearTimeout?: (handle: unknown) => void }).clearTimeout?.(handle)) as (
    handle: Handle,
  ) => void,
  // `schedule` and `clear` are only overridden by tests; the defaults talk to
  // the platform timers.
): ThrottledSaver {
  // The first change must write immediately, so the clock starts before any
  // measurable interval rather than at zero (which would defer the first write
  // by a whole interval).
  let lastWrite = Number.NEGATIVE_INFINITY;
  let pending: Handle | undefined;
  let dirty = false;

  const write = (): boolean => {
    if (!dirty) return false;
    // Serialize first: a serialization that throws must leave the value dirty
    // and the clock untouched, so the next attempt retries instead of waiting
    // out a full interval for a write that never happened.
    const value = serialize();
    dirty = false;
    lastWrite = now();
    store(value);
    return true;
  };

  // Drops a deferred write. `fromTimer` tells the timer's own callback apart
  // from a caller cancelling it, so the callback does not clear itself after it
  // has already fired.
  const unschedule = (fromTimer: boolean) => {
    if (pending === undefined) return;
    const handle = pending;
    pending = undefined;
    if (!fromTimer) {
      clear(handle);
    }
  };

  return {
    touch() {
      dirty = true;
      const elapsed = now() - lastWrite;
      if (elapsed >= intervalMs) {
        write();
        return;
      }
      if (pending !== undefined) return;
      pending = schedule(() => {
        unschedule(true);
        write();
      }, intervalMs - elapsed);
    },
    flush() {
      // A flush writes the current value even when the interval has not
      // elapsed, but only when there is something unwritten: a flush that
      // always writes would defeat the throttle entirely.
      unschedule(false);
      if (!dirty) return false;
      return write();
    },
    cancel() {
      dirty = false;
      unschedule(false);
    },
  };
}
