import { describe, expect, it } from "vitest";
import { createThrottledSaver } from "./throttledSave";

/**
 * A deterministic stand-in for setTimeout/clearTimeout: the handle is the timer
 * object itself, so cancelling is exact and the clock only moves when the test
 * says so.
 */
function fakeTimers() {
  let clock = 10_000;
  const timers = new Set<{ at: number; fn: () => void }>();
  return {
    now: () => clock,
    schedule(fn: () => void, ms: number) {
      const timer = { at: clock + ms, fn };
      timers.add(timer);
      return timer;
    },
    clear(timer: { at: number; fn: () => void }) {
      timers.delete(timer);
    },
    advance(ms: number) {
      clock += ms;
      for (const timer of [...timers]) {
        if (timer.at <= clock) {
          timers.delete(timer);
          timer.fn();
        }
      }
    },
    get pending() {
      return timers.size;
    },
  };
}

function harness(intervalMs = 2000) {
  const timers = fakeTimers();
  const stored: string[] = [];
  let serializations = 0;
  const source = { text: "" };
  const saver = createThrottledSaver(
    (value) => stored.push(value),
    () => {
      serializations++;
      return JSON.stringify(source);
    },
    intervalMs,
    timers.now,
    timers.schedule.bind(timers),
    timers.clear.bind(timers),
  );
  return {
    saver,
    stored,
    source,
    timers,
    serializations: () => serializations,
    advance: (ms: number) => timers.advance(ms),
  };
}

/**
 * The saver's contract: the write happens at most once per interval, a deferred
 * write is never dropped, and the value written is always the latest one. That
 * last property is what makes per-token serialization unnecessary.
 */
describe("createThrottledSaver", () => {
  it("writes immediately on the first change", () => {
    const h = harness();
    h.source.text = "hello";
    h.saver.touch();
    expect(h.stored).toEqual([`{"text":"hello"}`]);
    expect(h.serializations()).toBe(1);
    expect(h.timers.pending).toBe(0);
  });

  it("serializes once per interval no matter how many changes arrive", () => {
    const h = harness();
    h.source.text = "a";
    h.saver.touch();
    for (let i = 0; i < 50; i++) {
      h.source.text = "a" + i;
      h.saver.touch();
    }
    // The 50 changes inside the window cost no serialization.
    expect(h.serializations()).toBe(1);
    h.advance(2000);
    expect(h.serializations()).toBe(2);
    expect(h.stored).toEqual([`{"text":"a"}`, `{"text":"a49"}`]);
  });

  it("keeps the latest value in the deferred write", () => {
    const h = harness();
    h.source.text = "first";
    h.saver.touch();
    h.source.text = "second";
    h.saver.touch();
    h.advance(1500);
    h.source.text = "third";
    h.saver.touch();
    h.advance(500);
    expect(h.stored[1]).toBe(`{"text":"third"}`);
  });

  it("flush writes the current value even inside the window", () => {
    const h = harness();
    h.source.text = "streaming";
    h.saver.touch();
    h.source.text = "done";
    h.saver.touch();
    expect(h.saver.flush()).toBe(true);
    expect(h.stored[1]).toBe(`{"text":"done"}`);
    expect(h.timers.pending).toBe(0);
  });

  it("flush is a no-op when nothing changed", () => {
    const h = harness();
    h.source.text = "x";
    h.saver.touch();
    expect(h.saver.flush()).toBe(false);
    expect(h.stored).toHaveLength(1);
  });

  it("cancel drops a deferred write", () => {
    const h = harness();
    h.source.text = "a";
    h.saver.touch();
    h.source.text = "b";
    h.saver.touch();
    h.saver.cancel();
    h.advance(5000);
    expect(h.stored).toEqual([`{"text":"a"}`]);
  });

  it("a serialization failure surfaces and does not wedge the saver", () => {
    const timers = fakeTimers();
    const stored: string[] = [];
    let fail = true;
    const saver = createThrottledSaver(
      (value) => stored.push(value),
      () => {
        if (fail) throw new Error("cannot serialize");
        return "ok";
      },
      2000,
      timers.now,
      timers.schedule.bind(timers),
      timers.clear.bind(timers),
    );
    expect(() => saver.touch()).toThrow("cannot serialize");
    fail = false;
    // The failed attempt must not have consumed the change: the next touch
    // still writes, and the saver is not wedged.
    saver.touch();
    expect(stored).toEqual(["ok"]);
  });

  it("a long stream writes at the interval, not per token", () => {
    const h = harness();
    // Ten seconds of tokens arriving every 16 ms.
    for (let elapsed = 0; elapsed < 10_000; elapsed += 16) {
      h.source.text = `token-${elapsed}`;
      h.saver.touch();
      h.advance(16);
    }
    // Roughly one write per interval, and the last value is durable.
    expect(h.stored.length).toBeGreaterThanOrEqual(4);
    expect(h.stored.length).toBeLessThanOrEqual(7);
    expect(h.stored[h.stored.length - 1]).toBe(`{"text":"token-9984"}`);
  });
});
