import { afterEach, describe, expect, vi, it } from "vitest";
import { parseDebugChatStream, runDebugTurn, type DebugStreamEvent } from "./extensionDebugChat";

function sseResponse(events: string[], headers: Record<string, string> = {}): Response {
  const encoder = new TextEncoder();
  const stream = new ReadableStream<Uint8Array>({
    start(controller) {
      for (const event of events) controller.enqueue(encoder.encode(event));
      controller.close();
    },
  });
  return new Response(stream, { status: 200, headers });
}

function chatChunk(delta: Record<string, unknown>, finish?: string): string {
  return `data: ${JSON.stringify({ choices: [{ delta, finish_reason: finish ?? null }] })}\n\n`;
}

afterEach(() => {
  vi.unstubAllGlobals();
});

describe("parseDebugChatStream", () => {
  it("assembles content, reasoning and finish reason", async () => {
    const reader = sseResponse([
      chatChunk({ content: "He" }),
      chatChunk({ content: "llo", reasoning_content: "thinking" }),
      chatChunk({}, "stop"),
      "data: [DONE]\n\n",
    ]).body!.getReader();
    const events: DebugStreamEvent[] = [];
    for await (const event of parseDebugChatStream(reader)) events.push(event);
    expect(events.some((event) => event.content === "He")).toBe(true);
    expect(events.some((event) => event.reasoning === "thinking")).toBe(true);
    const final = events.at(-1);
    expect(final?.done).toBe(true);
    expect(final?.finish).toBe("stop");
  });

  it("reassembles streamed tool_call deltas by index", async () => {
    const reader = sseResponse([
      chatChunk({ tool_calls: [{ index: 0, function: { name: "sea", arguments: "{\"q" } }] }),
      chatChunk({ tool_calls: [{ index: 0, function: { arguments: "\":\"hi\"}" } }] }, "tool_calls"),
      "data: [DONE]\n\n",
    ]).body!.getReader();
    const events: DebugStreamEvent[] = [];
    for await (const event of parseDebugChatStream(reader)) events.push(event);
    const tool = events.find((event) => event.toolName);
    expect(tool?.toolName).toBe("sea");
    expect(tool?.toolArguments).toBe('{"q":"hi"}');
  });
});

describe("runDebugTurn", () => {
  it("streams deltas, then attaches the finished artifact", async () => {
    const fetchMock = vi.fn(async (url: string | URL | Request) => {
      const target = String(url);
      if (target.includes("/debug/chat")) {
        return sseResponse([chatChunk({ content: "ok" }), "data: [DONE]\n\n"], { "X-Extension-Debug-ID": "req-1" });
      }
      if (target.includes("/debug/artifact?request=req-1")) {
        return new Response(JSON.stringify({ steps: [{ kind: "hook", hook: "onRequest", logs: ["ran"] }], truncated: false, done: true }), { status: 200 });
      }
      return new Response("not found", { status: 404 });
    });
    vi.stubGlobal("fetch", fetchMock);

    const deltas: string[] = [];
    const turn = await runDebugTurn({ id: "ext", request: { model: "m", messages: [] }, onDelta: (delta) => deltas.push(delta) });
    expect(deltas).toEqual(["ok"]);
    expect(turn.text).toBe("ok");
    expect(turn.clientToolCalls).toEqual([]);
    expect(turn.artifact?.steps).toHaveLength(1);
    expect(turn.artifact?.done).toBe(true);
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("surfaces client tool calls and retries until the artifact is done", async () => {
    let artifactPolls = 0;
    const fetchMock = vi.fn(async (url: string | URL | Request) => {
      const target = String(url);
      if (target.includes("/debug/chat")) {
        return sseResponse([
          chatChunk({ tool_calls: [{ index: 0, function: { name: "client_tool", arguments: "{}" } }] }, "tool_calls"),
          "data: [DONE]\n\n",
        ], { "X-Extension-Debug-ID": "req-2" });
      }
      if (target.includes("/debug/artifact?request=req-2")) {
        artifactPolls += 1;
        const done = artifactPolls >= 2;
        return new Response(JSON.stringify({ steps: [], truncated: false, done }), { status: 200 });
      }
      return new Response("not found", { status: 404 });
    });
    vi.stubGlobal("fetch", fetchMock);

    const turn = await runDebugTurn({ id: "ext", request: { model: "m", messages: [] } });
    expect(turn.clientToolCalls).toEqual([{ name: "client_tool", arguments: "{}" }]);
    expect(artifactPolls).toBeGreaterThanOrEqual(2);
    expect(turn.artifact?.done).toBe(true);
  });

  it("throws with the response detail when the endpoint fails", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response("boom", { status: 502 })));
    await expect(runDebugTurn({ id: "ext", request: { model: "m", messages: [] } })).rejects.toThrow("HTTP 502");
  });
});
