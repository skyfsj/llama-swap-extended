import { afterEach, describe, expect, it, vi } from "vitest";
import { streamChatCompletion } from "./chatApi";

afterEach(() => vi.unstubAllGlobals());

describe("streamChatCompletion usage metrics", () => {
  it("requests and exposes usage metadata without dropping the final done frame", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        [
          'data: {"choices":[{"delta":{"content":"OK"}}]}',
          "",
          'data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":3,"total_tokens":123}}',
          "",
          "data: [DONE]",
          "",
        ].join("\n"),
        { headers: { "Content-Type": "text/event-stream" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    const chunks = [];
    for await (const chunk of streamChatCompletion(
      "model-a",
      [{ role: "user", content: "hello" }],
      undefined,
      { include_usage: true, max_tokens: 3 },
    )) {
      chunks.push(chunk);
    }

    const request = fetchMock.mock.calls[0][1] as RequestInit;
    expect(JSON.parse(String(request.body))).toMatchObject({
      stream: true,
      stream_options: { include_usage: true },
    });
    expect(chunks).toContainEqual({
      content: "",
      reasoning_content: "",
      done: false,
      usage: { prompt_tokens: 120, completion_tokens: 3, total_tokens: 123 },
    });
    expect(chunks.at(-1)).toEqual({ content: "", done: true });
  });
});

// SSE framing tolerance: gin-based backends (simple-responder, some proxies)
// emit "data:{...}" without the optional space. Both forms must parse.
describe("streamChatCompletion SSE framing tolerance", () => {
  it("parses spaceless data fields per the SSE spec", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        [
          'event:message',
          'data:{"choices":[{"delta":{"content":"asdf"}}]}',
          "",
          "event:message",
          'data:{"choices":[{"delta":{"content":"asdf"}}]}',
          "",
          "event:message",
          "data: [DONE]",
          "",
        ].join("\n"),
        { headers: { "Content-Type": "text/event-stream" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    let content = "";
    for await (const chunk of streamChatCompletion(
      "model-a",
      [{ role: "user", content: "hello" }],
      undefined,
    )) {
      content += chunk.content;
    }
    expect(content).toBe("asdfasdf");
  });

  it("parses a mixed stream with both space and spaceless data fields", async () => {
    const fetchMock = vi.fn().mockResolvedValue(
      new Response(
        [
          'data:{"choices":[{"delta":{"content":"a"}}]}',
          "",
          'data: {"choices":[{"delta":{"content":"b"}}]}',
          "",
          "data:[DONE]",
          "",
        ].join("\n"),
        { headers: { "Content-Type": "text/event-stream" } },
      ),
    );
    vi.stubGlobal("fetch", fetchMock);

    let content = "";
    for await (const chunk of streamChatCompletion(
      "model-a",
      [{ role: "user", content: "hello" }],
      undefined,
    )) {
      content += chunk.content;
    }
    expect(content).toBe("ab");
  });
});

describe("streamChatCompletion error frames", () => {
  it("throws on chat.completions error frames instead of yielding an empty reply", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          [
            'data: {"error":{"message":"extension tool round limit reached","code":502}}',
            "",
          ].join("\n"),
          { headers: { "Content-Type": "text/event-stream" } },
        ),
      ),
    );

    await expect(async () => {
      for await (const _chunk of streamChatCompletion("model-a", [
        { role: "user", content: "hi" },
      ])) {
        // consume
      }
    }).rejects.toThrow("extension tool round limit reached (502)");
  });

  it("throws on anthropic error events", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          [
            'event: error',
            'data: {"type":"error","error":{"type":"api_error","message":"upstream disconnected"}}',
            "",
            "",
          ].join("\n"),
          { headers: { "Content-Type": "text/event-stream" } },
        ),
      ),
    );

    await expect(async () => {
      for await (const _chunk of streamChatCompletion(
        "model-a",
        [{ role: "user", content: "hi" }],
        undefined,
        { endpoint: "v1/messages" },
      )) {
        // consume
      }
    }).rejects.toThrow("upstream disconnected");
  });

  it("throws on responses.failed events with the response error message", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        new Response(
          [
            'event: response.failed',
            'data: {"response":{"error":{"code":"server_error","message":"engine crashed"}}}',
            "",
            "",
          ].join("\n"),
          { headers: { "Content-Type": "text/event-stream" } },
        ),
      ),
    );

    await expect(async () => {
      for await (const _chunk of streamChatCompletion(
        "model-a",
        [{ role: "user", content: "hi" }],
        undefined,
        { endpoint: "v1/responses" },
      )) {
        // consume
      }
    }).rejects.toThrow("engine crashed");
  });
});
