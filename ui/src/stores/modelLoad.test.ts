import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { get } from "svelte/store";
import { handleLoadModel, onToggleLoad, pendingLoads } from "./modelLoad";
import { toast, type Toast } from "../lib/toast";
import type { Model } from "../lib/types";

function model(overrides: Partial<Model> = {}): Model {
  return { id: "m1", name: "Model One", state: "stopped", ...overrides } as Model;
}

function visible(): Toast[] {
  return get(toast);
}

function fetchStub(ok: boolean): void {
  vi.stubGlobal(
    "fetch",
    vi.fn(async () => ({
      ok,
      status: ok ? 200 : 500,
      json: async () => (ok ? {} : { error: "boom" }),
    })),
  );
}

beforeEach(() => {
  toast.reset();
  pendingLoads.set({});
});

afterEach(() => {
  vi.unstubAllGlobals();
  toast.reset();
  pendingLoads.set({});
});

describe("model load feedback", () => {
  it("confirms a start that the backend accepted", async () => {
    fetchStub(true);
    await handleLoadModel("m1");
    expect(visible().at(-1)).toMatchObject({ level: "success", message: expect.stringContaining("m1") });
  });

  it("surfaces a failed start instead of only logging it", async () => {
    fetchStub(false);
    await handleLoadModel("m1");
    const last = visible().at(-1);
    expect(last?.level).toBe("error");
    expect(last?.message).toContain("m1");
  });

  it("does not report a start the user cancelled", async () => {
    // loadModel resolves on abort, so the only way to stay silent is for
    // handleLoadModel to notice the controller was aborted.
    const controller = new AbortController();
    vi.stubGlobal(
      "fetch",
      vi.fn(async () => {
        controller.abort();
        throw new DOMException("aborted", "AbortError");
      }),
    );
    const originalController = globalThis.AbortController;
    vi.stubGlobal("AbortController", class extends originalController {
      constructor() {
        super();
        return controller;
      }
    } as unknown as typeof AbortController);
    await handleLoadModel("m1");
    vi.unstubAllGlobals();
    expect(visible()).toHaveLength(0);
  });

  it("clears the pending flag once the start settles", async () => {
    fetchStub(true);
    await handleLoadModel("m1");
    expect(get(pendingLoads)).toEqual({});
  });
});

describe("toggle load feedback", () => {
  it("reports a failed unload of a running model", async () => {
    fetchStub(false);
    onToggleLoad(model({ state: "ready" }));
    await vi.waitFor(() => expect(visible().at(-1)?.level).toBe("error"));
  });

  it("reports a successful unload of a running model", async () => {
    fetchStub(true);
    onToggleLoad(model({ state: "ready" }));
    await vi.waitFor(() => expect(visible().at(-1)?.level).toBe("success"));
  });

  it("stays silent when cancelling an in-flight start", async () => {
    // The abort path deliberately keeps the best-effort unload quiet: the user
    // asked for the start to go away, and it did.
    let release: (() => void) | undefined;
    vi.stubGlobal(
      "fetch",
      vi.fn((input: string) => {
        if (String(input).includes("/api/models/unload/")) {
          return Promise.resolve({ ok: true, status: 200, json: async () => ({}) });
        }
        return new Promise((resolve) => {
          release = () => resolve({ ok: true, status: 200, json: async () => ({}) });
        });
      }),
    );
    const loading = handleLoadModel("m1");
    // handleLoadModel sets the pending flag synchronously before awaiting.
    expect(get(pendingLoads)).toEqual({ m1: true });

    onToggleLoad(model({ state: "stopped" }));
    expect(get(pendingLoads)).toEqual({ m1: true });
    release?.();
    await loading;
    expect(visible()).toHaveLength(0);
  });
});
