import { get } from "svelte/store";
import { afterEach, describe, expect, it, vi } from "vitest";
import {
  activeProfile,
  activityRevision,
  backendProgress,
  fetchPlaygroundModels,
  fetchProfiles,
  getHardware,
  getActivity,
  getActivityStats,
  getModelLoadConflicts,
  handleAPIEventMessage,
  hasListedModels,
  inFlightRequests,
  inflightRequestEntries,
  models,
  playgroundModels,
  profileModels,
  profiles,
  runtimeLogs,
  selectorModels,
  setActiveProfile,
  uiConfig,
} from "./api";

afterEach(() => {
  vi.unstubAllGlobals();
  models.set([]);
  playgroundModels.set([]);
  profiles.set([]);
  activeProfile.set(null);
  backendProgress.set({});
  runtimeLogs.set({});
});

describe("hardware api", () => {
  it("fetches the hardware snapshot", async () => {
    const snapshot = {
      schema_version: 1,
      captured_at: "2026-08-03T12:00:00Z",
      capture: { scope: "inference_host", method: "detected", detector: { name: "llama-swap", version: "246" } },
      architecture: { name: "x86_64" },
      operating_system: { family: "linux", name: "Ubuntu", version: "24.04", kernel: "6.8" },
      environment: { kind: "native", name: null, version: null },
      cpu: { vendor: "AMD", model: "Ryzen", socket_count: 1, physical_core_count: 16, logical_thread_count: 32 },
      memory: { capacity_bytes: 1024 },
      accelerators: [],
    };
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: true, json: async () => snapshot }));
    await expect(getHardware()).resolves.toEqual(snapshot);
    expect(fetch).toHaveBeenCalledWith("/api/hardware");
  });

  it("rejects unavailable hardware", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 503 }));
    await expect(getHardware()).rejects.toThrow("Failed to fetch hardware: 503");
  });
});

describe("activity api", () => {
  it("limits the main activity views to currently configured models", async () => {
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => ({}) });
    vi.stubGlobal("fetch", fetchMock);

    await getActivity({ configuredOnly: true });
    await getActivityStats({ configuredOnly: true });

    expect(fetchMock).toHaveBeenNthCalledWith(1, "/api/metrics/activity?configured_only=true");
    expect(fetchMock).toHaveBeenNthCalledWith(2, "/api/metrics/stats?configured_only=true");
  });
});

describe("model load conflict api", () => {
  it("returns the server's numeric model conflict list", async () => {
    const response = {
      conflicts: [{ id: "old/model", name: "Old model", state: "ready" }],
    };
    const fetchMock = vi.fn().mockResolvedValue({ ok: true, json: async () => response });
    vi.stubGlobal("fetch", fetchMock);

    await expect(getModelLoadConflicts("target/model")).resolves.toEqual(response.conflicts);
    expect(fetchMock).toHaveBeenCalledWith("/api/models/conflicts/target%2Fmodel");
  });

  it("does not turn a failed conflict check into an implicit load", async () => {
    vi.stubGlobal("fetch", vi.fn().mockResolvedValue({ ok: false, status: 503 }));

    await expect(getModelLoadConflicts("target")).rejects.toThrow("Failed to check model conflicts: 503");
  });
});

describe("api store event handling", () => {
  it("parses inflight request entries", () => {
    inFlightRequests.set(0);
    inflightRequestEntries.set([]);

    handleAPIEventMessage(
      JSON.stringify({
        type: "inflight",
        data: JSON.stringify({
          operation: "snapshot",
          requests: [
            {
              id: "7",
              timestamp: "2026-07-03T00:00:00Z",
              model: "m1",
              req_path: "/v1/chat/completions",
              method: "POST",
              req_headers: { "User-Agent": "test-agent" },
              remote_ip: "203.0.113.9",
              resp_headers: {},
              resp_bytes: 0,
              elapsed_ms: 125,
              metadata: { source: "test" },
            },
          ],
        }),
      })
    );

    expect(get(inFlightRequests)).toBe(1);
    expect(get(inflightRequestEntries)).toEqual([
      {
        id: "7",
        timestamp: "2026-07-03T00:00:00Z",
        model: "m1",
        req_path: "/v1/chat/completions",
        method: "POST",
        req_headers: { "User-Agent": "test-agent" },
        remote_ip: "203.0.113.9",
        resp_headers: {},
        resp_bytes: 0,
        elapsed_ms: 125,
        client_received_at_ms: expect.any(Number),
        metadata: { source: "test" },
      },
    ]);
  });

  it("upserts and removes inflight entries by id", () => {
    handleAPIEventMessage(JSON.stringify({
      type: "inflight",
      data: JSON.stringify({
        operation: "upsert",
        request: {
          id: "7",
          timestamp: "2026-07-03T00:00:00Z",
          model: "m1",
          req_path: "/v1/chat/completions",
          method: "POST",
          req_headers: {},
          remote_ip: "203.0.113.9",
          resp_headers: { "Content-Type": "text/event-stream" },
          resp_bytes: 42,
          elapsed_ms: 250,
        },
      }),
    }));

    expect(get(inflightRequestEntries)).toHaveLength(1);
    expect(get(inflightRequestEntries)[0].resp_bytes).toBe(42);

    handleAPIEventMessage(JSON.stringify({
      type: "inflight",
      data: JSON.stringify({ operation: "remove", id: "7" }),
    }));
    expect(get(inflightRequestEntries)).toEqual([]);
    expect(get(inFlightRequests)).toBe(0);
  });

  it("parses UI activity configuration", () => {
    handleAPIEventMessage(JSON.stringify({
      type: "uiConfig",
      data: JSON.stringify({ activity: { session_id: ["X-Trace-ID"] } }),
    }));
    expect(get(uiConfig).activity.session_id).toEqual(["X-Trace-ID"]);
  });

  it("increments activity revision for activity events", () => {
    activityRevision.set(0);

    handleAPIEventMessage(
      JSON.stringify({
        type: "activity",
        data: JSON.stringify({ id: 42 }),
      })
    );

    expect(get(activityRevision)).toBe(1);
  });

  it("applies profile change events", () => {
    activeProfile.set(null);
    handleAPIEventMessage(JSON.stringify({
      type: "profileChanged",
      data: JSON.stringify({ active: "coding" }),
    }));
    expect(get(activeProfile)).toBe("coding");
  });

  it("keeps the latest backend progress event by model or runtime", () => {
    handleAPIEventMessage(JSON.stringify({
      type: "backendProgress",
      data: JSON.stringify({ model: "m1", phase: "downloading", progress: 0.5, message: "fetching" }),
    }));
    handleAPIEventMessage(JSON.stringify({
      type: "backendProgress",
      data: JSON.stringify({ runtime: "vllm", phase: "active", progress: 1 }),
    }));
    expect(get(backendProgress)).toEqual({
      m1: { model: "m1", phase: "downloading", progress: 0.5, message: "fetching" },
      vllm: { runtime: "vllm", phase: "active", progress: 1 },
    });
  });

  it("keeps streamed runtime output separate from the latest phase", () => {
    handleAPIEventMessage(JSON.stringify({
      type: "backendProgress",
      data: JSON.stringify({ runtime: "vllm", phase: "building", progress: 0.2, message: "compiling" }),
    }));
    handleAPIEventMessage(JSON.stringify({
      type: "backendProgress",
      data: JSON.stringify({ runtime: "vllm", operationId: "op-1", phase: "log", progress: 0, outputStream: "stderr", output: "error: first line\n" }),
    }));
    handleAPIEventMessage(JSON.stringify({
      type: "backendProgress",
      data: JSON.stringify({ runtime: "vllm", operationId: "op-1", phase: "log", progress: 0, outputStream: "stderr", output: "error: second line\n" }),
    }));

    expect(get(runtimeLogs)).toEqual({ vllm: "error: first line\nerror: second line\n" });
    expect(get(backendProgress).vllm?.phase).toBe("building");
  });

  it("loads and switches profiles", async () => {
    const mockFetch = vi.fn()
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({
          active: null,
          profiles: [{ id: "coding", description: "Coding", pins: { llm: "real" } }],
        }),
      })
      .mockResolvedValueOnce({
        ok: true,
        json: async () => ({ active: "coding" }),
      });
    vi.stubGlobal("fetch", mockFetch);

    await fetchProfiles();
    expect(get(profiles)).toHaveLength(1);
    expect(get(activeProfile)).toBeNull();

    await setActiveProfile("coding");
    expect(get(activeProfile)).toBe("coding");
    expect(mockFetch).toHaveBeenLastCalledWith("/api/profiles/active", expect.objectContaining({
      method: "PUT",
      body: JSON.stringify({ name: "coding" }),
    }));
  });

  it("loads Playground models and virtual model types from v1/models", async () => {
    const mockFetch = vi.fn().mockResolvedValue({
      ok: true,
      json: async () => ({
        data: [
          {
            id: "real",
            name: "Real",
            capabilities: { vision: true },
            context_length: 128000,
            meta: { llamaswap: { type: "model", aliases: ["variant", "alternate"] } },
          },
          {
            id: "variant",
            meta: { llamaswap: { type: "alias", modelID: "real" } },
          },
          {
            id: "remote/remote-model",
            meta: { llamaswap: { type: "peer", peerID: "remote" } },
          },
          {
            id: "pool",
            meta: {
              llamaswap: {
                type: "selector",
                strategy: "spillover",
                targets: ["real", "remote/remote-model"],
                spillover: 4,
              },
            },
          },
          {
            id: "public",
            meta: { llamaswap: { type: "profile" } },
          },
        ],
      }),
    });
    vi.stubGlobal("fetch", mockFetch);

    await fetchPlaygroundModels();

    expect(mockFetch).toHaveBeenCalledWith("/v1/models");
    expect(get(playgroundModels).map((model) => model.id)).not.toContain("variant");
    expect(get(playgroundModels).find((model) => model.id === "real")).toMatchObject({
      aliases: ["variant", "alternate"],
      capabilities: { vision: true },
      context_length: 128000,
      playgroundType: "model",
    });
    expect(get(playgroundModels).find((model) => model.id === "remote/remote-model")).toMatchObject({
      peerID: "remote",
      playgroundType: "peer",
    });
    expect(get(selectorModels).map((model) => model.id)).toEqual(["pool"]);
    expect(get(selectorModels)[0]).toMatchObject({
      strategy: "spillover",
      targets: ["real", "remote/remote-model"],
      spillover: 4,
    });
    expect(get(profileModels).map((model) => model.id)).toEqual(["public"]);
    expect(get(hasListedModels)).toBe(true);

    playgroundModels.set([]);
    expect(get(selectorModels)).toEqual([]);
    expect(get(profileModels)).toEqual([]);
    expect(get(hasListedModels)).toBe(false);
  });

  it("coalesces overlapping Playground model refreshes", async () => {
    type ModelResponse = {
      ok: boolean;
      json: () => Promise<{ data: [] }>;
    };
    let resolveFirst!: (response: ModelResponse) => void;
    const firstResponse = new Promise<ModelResponse>((resolve) => {
      resolveFirst = resolve;
    });
    const mockFetch = vi.fn()
      .mockReturnValueOnce(firstResponse)
      .mockResolvedValue({
        ok: true,
        json: async () => ({ data: [] }),
      });
    vi.stubGlobal("fetch", mockFetch);

    const first = fetchPlaygroundModels();
    const overlapping = fetchPlaygroundModels();

    expect(overlapping).toBe(first);
    expect(mockFetch).toHaveBeenCalledTimes(1);

    resolveFirst({
      ok: true,
      json: async () => ({ data: [] }),
    });
    await first;

    await vi.waitFor(() => expect(mockFetch).toHaveBeenCalledTimes(2));
  });
});
