import { describe, expect, it, vi, beforeEach, afterEach } from "vitest";
import { get } from "svelte/store";
import { toast, type Toast } from "./toast";

function collect(): Toast[] {
  return get(toast);
}

describe("toast store", () => {
  beforeEach(() => {
    // The store is module-global: drain everything and clear the pause flag
    // so tests cannot leak state into each other.
    toast.reset();
    vi.useFakeTimers();
  });
  afterEach(() => {
    vi.useRealTimers();
  });

  it("stacks up to four and queues the rest", () => {
    for (let index = 0; index < 6; index++) toast.success(`m${index}`, { duration: 10000 });
    const visible = collect();
    expect(visible).toHaveLength(4);
    expect(visible.map((item) => item.message)).toEqual(["m0", "m1", "m2", "m3"]);
  });

  it("merges identical messages into a count badge", () => {
    toast.success("same", { duration: 10000 });
    toast.success("same", { duration: 10000 });
    const visible = collect();
    expect(visible).toHaveLength(1);
    expect(visible[0].count).toBe(2);
  });

  it("auto-dismisses the expired toast and promotes the queue", () => {
    // Only the first toast is short-lived; the rest outlive the tick window.
    toast.success("q0", { duration: 1000 });
    for (let index = 1; index < 5; index++) toast.success(`q${index}`, { duration: 10000 });
    expect(collect()).toHaveLength(4);
    vi.advanceTimersByTime(1200);
    expect(collect().map((item) => item.message)).toEqual(["q1", "q2", "q3", "q4"]);
  });

  it("keeps loading toasts until updated or dismissed", () => {
    const id = toast.loading("working");
    vi.advanceTimersByTime(30000);
    expect(collect()).toHaveLength(1);
    toast.update(id, { level: "success", message: "done" });
    expect(collect()[0]).toMatchObject({ level: "success", message: "done" });
    toast.dismiss(id);
    expect(collect()).toHaveLength(0);
  });

  it("pauses timers on hover and resumes afterwards", () => {
    toast.success("hold", { duration: 500 });
    vi.advanceTimersByTime(400);
    expect(collect()).toHaveLength(1);
    toast.setPaused(true);
    vi.advanceTimersByTime(2000);
    expect(collect()).toHaveLength(1);
    toast.setPaused(false);
    vi.advanceTimersByTime(200);
    expect(collect()).toHaveLength(0);
  });

  it("promise swaps loading to success and error outcomes", async () => {
    const ok = await toast.promise(Promise.resolve("payload"), {
      loading: "wait",
      success: (value) => `got ${value}`,
      error: "failed",
    });
    expect(ok).toBe("payload");
    expect(collect().at(-1)).toMatchObject({ level: "success", message: "got payload" });
    toast.dismiss(collect().at(-1)!.id);

    await expect(
      toast.promise(Promise.reject(new Error("boom")), { loading: "wait", success: "ok", error: (reason: unknown) => `err ${(reason as Error).message}` }),
    ).rejects.toThrow("boom");
    expect(collect().at(-1)).toMatchObject({ level: "error", message: "err boom" });
  });
});
