import { afterEach, describe, expect, it, vi } from "vitest";
import { streamModelLog } from "./modelLogs";

interface OpenLogStream {
  response: Response;
  close: () => void;
}

function openLogStream(text: string): OpenLogStream {
  let streamController: ReadableStreamDefaultController<Uint8Array> | undefined;
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      streamController = controller;
      if (text) controller.enqueue(new TextEncoder().encode(text));
    },
  });

  return {
    response: new Response(body, { status: 200 }),
    close() {
      try {
        streamController?.close();
      } catch {
        // The consumer may already have cancelled the stream during cleanup.
      }
    },
  };
}

async function waitFor(predicate: () => boolean, timeoutMs = 3000): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, 10));
  }
  throw new Error("timed out waiting for model log stream");
}

describe("streamModelLog", () => {
  afterEach(() => vi.unstubAllGlobals());

  it("retries an initial HTTP failure until the model logger is available", async () => {
    const fetchMock = vi.fn<typeof fetch>();
    const recovered = openLogStream("model log\n");
    fetchMock.mockResolvedValueOnce(new Response("not ready", { status: 503 })).mockResolvedValueOnce(recovered.response);
    vi.stubGlobal("fetch", fetchMock);

    const values: string[] = [];
    const unsubscribe = streamModelLog("Qwen/Qwen3").subscribe((value) => values.push(value));
    try {
      await waitFor(() => values.includes("model log\n"));

      expect(fetchMock).toHaveBeenCalledTimes(2);
      expect(fetchMock.mock.calls[0][0]).toBe("/logs/stream/Qwen%2FQwen3");
      expect(fetchMock.mock.calls[0][1]).toEqual(expect.objectContaining({
        cache: "no-store",
        credentials: "same-origin",
      }));
    } finally {
      unsubscribe();
      recovered.close();
    }
  });

  it("replaces the previous tail after a stream disconnect instead of duplicating history", async () => {
    const fetchMock = vi.fn<typeof fetch>();
    const first = openLogStream("first\n");
    let second: OpenLogStream | undefined;
    let attempts = 0;
    fetchMock.mockImplementation(() => {
      if (attempts++ === 0) return Promise.resolve(first.response);
      second = openLogStream("first\nsecond\n");
      return Promise.resolve(second.response);
    });
    vi.stubGlobal("fetch", fetchMock);

    const values: string[] = [];
    const unsubscribe = streamModelLog("model").subscribe((value) => values.push(value));
    try {
      await waitFor(() => values.includes("first\n"));
      first.close();
      await waitFor(() => values.includes("first\nsecond\n"));

      expect(values.at(-1)).toBe("first\nsecond\n");
      expect(values.at(-1)).not.toContain("first\nfirst\n");
      expect(fetchMock).toHaveBeenCalledTimes(2);
    } finally {
      unsubscribe();
      first.close();
      second?.close();
    }
  });

  it("stops retrying after the last subscriber unsubscribes", async () => {
    const fetchMock = vi.fn<typeof fetch>().mockResolvedValue(new Response("not ready", { status: 503 }));
    vi.stubGlobal("fetch", fetchMock);

    const unsubscribe = streamModelLog("model").subscribe(() => undefined);
    await waitFor(() => fetchMock.mock.calls.length === 1);
    unsubscribe();
    const callsAtStop = fetchMock.mock.calls.length;

    await new Promise((resolve) => setTimeout(resolve, 350));
    expect(fetchMock.mock.calls.length).toBe(callsAtStop);
  });
});
