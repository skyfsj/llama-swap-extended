import { describe, expect, it } from "vitest";
import { auditRequestTranscript, auditResponseTranscript, collapseDataUrls, decodeAuditBytes, fetchAuditConversationBody, formatAuditBody } from "./audit";

describe("decodeAuditBytes", () => {
  it("decodes Go []byte JSON base64", () => {
    expect(decodeAuditBytes("eyJtZXNzYWdlIjoiaGkifQ==")).toBe('{"message":"hi"}');
  });

  it("keeps legacy plain strings", () => {
    expect(decodeAuditBytes("plain text response")).toBe("plain text response");
    expect(decodeAuditBytes("test")).toBe("test");
  });

  it("decodes typed byte arrays", () => {
    expect(decodeAuditBytes([0x68, 0x69])).toBe("hi");
  });
});

describe("formatAuditBody", () => {
  it("aggregates chat SSE deltas into a readable answer", () => {
    const body = [
      "event: message",
      'data: {"choices":[{"delta":{"content":"Hello "},"finish_reason":null}]}',
      "",
      "event: message",
      'data: {"choices":[{"delta":{"content":"world"},"finish_reason":"stop"}]}',
      "",
      "data: [DONE]",
      "",
    ].join("\n");

    const result = formatAuditBody(body);
    expect(result.structured).toBe(true);
    expect(result.text).toBe("Hello world");
    expect(result.finishReason).toBe("stop");
    expect(result.events).toHaveLength(3);
    expect(result.formatted).toContain('"content": "Hello "');
  });

  it("unwraps adapters that place the canonical payload in data", () => {
    const body = [
      "event: message",
      'data: {"data":{"choices":[{"delta":{"content":"answer"}}]}}',
      "",
    ].join("\n");

    expect(formatAuditBody(body).text).toBe("answer");
  });

  it("pretty-prints JSON request bodies", () => {
    const result = formatAuditBody('{"model":"local","messages":[{"role":"user","content":"ping"}]}');
    expect(result.structured).toBe(true);
    expect(result.formatted).toContain('\n  "model": "local"');
    expect(result.text).toBe("");
  });

  it("keeps plain text as the readable response", () => {
    const result = formatAuditBody("plain response");
    expect(result.structured).toBe(false);
    expect(result.text).toBe("plain response");
    expect(result.formatted).toBe("plain response");
  });
});

describe("audit conversation transcripts", () => {
  it("renders OpenAI messages and function calls without exposing JSON", () => {
    const transcript = auditRequestTranscript(JSON.stringify({
      messages: [
        { role: "system", content: "Answer concisely." },
        { role: "user", content: "What is the service status?" },
        {
          role: "assistant",
          content: "I will check it.",
          tool_calls: [{
            id: "call_status",
            type: "function",
            function: { name: "get_status", arguments: '{"service":"api","verbose":false}' },
          }],
        },
        { role: "tool", tool_call_id: "call_status", content: "healthy" },
      ],
    }));

    expect(transcript).toEqual([
      { kind: "message", role: "system", text: "Answer concisely." },
      { kind: "message", role: "user", text: "What is the service status?" },
      { kind: "message", role: "assistant", text: "I will check it." },
      {
        kind: "tool_call",
        id: "call_status",
        name: "get_status",
        fields: [
          { name: "service", value: "api" },
          { name: "verbose", value: "false" },
        ],
      },
      { kind: "tool_result", id: "call_status", name: "", text: "healthy" },
    ]);
  });

  it("turns streamed text and tool calls into a single response transcript", () => {
    const body = [
      'data: {"choices":[{"delta":{"content":"The service is "}}]}',
      "",
      'data: {"choices":[{"delta":{"content":"healthy."}}]}',
      "",
      'data: {"choices":[{"delta":{"tool_calls":[{"id":"call_restart","type":"function","function":{"name":"restart","arguments":"{\\\"force\\\":true}"}}]}}]}',
      "",
      "data: [DONE]",
      "",
    ].join("\n");

    expect(auditResponseTranscript(body)).toEqual([
      { kind: "message", role: "assistant", text: "The service is healthy." },
      {
        kind: "tool_call",
        id: "call_restart",
        name: "restart",
        fields: [{ name: "force", value: "true" }],
      },
    ]);
  });

  it("omits empty assistant placeholders before a tool call", () => {
    const transcript = auditRequestTranscript(JSON.stringify({
      messages: [{
        role: "assistant",
        content: " \n ",
        tool_calls: [{
          id: "call_lookup",
          type: "function",
          function: { name: "lookup", arguments: '{"query":"status"}' },
        }],
      }],
    }));

    expect(transcript).toEqual([{
      kind: "tool_call",
      id: "call_lookup",
      name: "lookup",
      fields: [{ name: "query", value: "status" }],
    }]);
  });
});

describe("audit media attachments", () => {
  const PNG = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==";

  it("renders image_url data URLs as image items", () => {
    const items = auditRequestTranscript(JSON.stringify({
      messages: [{ role: "user", content: [
        { type: "text", text: "看这张图" },
        { type: "image_url", image_url: { url: `data:image/png;base64,${PNG}` } },
      ]}],
    }));
    expect(items.some((item) => item.kind === "message" && item.text === "看这张图")).toBe(true);
    const image = items.find((item) => item.kind === "image");
    expect(image).toMatchObject({ kind: "image", label: "image/png", dataUrl: `data:image/png;base64,${PNG}` });
    expect(image && "sizeBytes" in image ? image.sizeBytes : 0).toBeGreaterThan(0);
  });

  it("renders Anthropic base64 sources and documents", () => {
    const items = auditRequestTranscript(JSON.stringify({
      messages: [{ role: "user", content: [
        { type: "image", source: { type: "base64", media_type: "image/jpeg", data: PNG } },
        { type: "document", source: { type: "base64", media_type: "application/pdf", data: "JVBERi0xLjQ=" } },
      ]}],
    }));
    expect(items).toEqual(expect.arrayContaining([
      expect.objectContaining({ kind: "image", label: "image/jpeg" }),
      expect.objectContaining({ kind: "file", label: "application/pdf" }),
    ]));
  });

  it("keeps tool results readable and surfaces their media", () => {
    const items = auditResponseTranscript(JSON.stringify({
      choices: [{ message: { role: "tool", tool_call_id: "call-1", content: [
        { type: "image_url", image_url: { url: `data:image/png;base64,${PNG}` } },
      ] } }],
    }));
    const text = items.filter((item) => item.kind === "tool_result").map((item) => ("text" in item ? item.text : "")).join("");
    expect(text).not.toContain(PNG);
    expect(items.some((item) => item.kind === "image")).toBe(true);
  });

  it("renders an image-returning tool whose result is a bare data URL string", () => {
    // An image-returning tool (view_image and friends) puts the whole data URL
    // in the result instead of wrapping it in a typed part. Skipping bare
    // strings rendered those images as nothing but the "[image/png 1.0 MB]"
    // text placeholder, even though the bytes were in the capture.
    const chatItems = auditResponseTranscript(JSON.stringify({
      choices: [{ message: { role: "tool", tool_call_id: "call-2", content: `data:image/png;base64,${PNG}` } }],
    }));
    expect(chatItems.find((item) => item.kind === "image")).toMatchObject({
      kind: "image", label: "image/png", dataUrl: `data:image/png;base64,${PNG}`,
    });

    const typedItems = auditRequestTranscript(JSON.stringify({
      messages: [{ role: "user", content: [
        { type: "tool_result", tool_use_id: "t1", content: `data:image/png;base64,${PNG}` },
      ] }],
    }));
    expect(typedItems.some((item) => item.kind === "image")).toBe(true);
  });

  it("does not treat ordinary tool text as media", () => {
    const items = auditResponseTranscript(JSON.stringify({
      choices: [{ message: { role: "tool", tool_call_id: "call-3", content: "healthy" } }],
    }));
    expect(items.some((item) => item.kind === "image" || item.kind === "file")).toBe(false);
  });

  it("collapses long data URLs in the raw view and keeps short values", () => {
    const long = `data:image/png;base64,${PNG.repeat(64)}`;
    const collapsed = collapseDataUrls(`{"img":"${long}"}`);
    expect(collapsed).not.toContain(PNG.slice(0, 32));
    expect(collapsed).toContain("data:image/png;base64,[");
    expect(collapseDataUrls(`{"tiny":"data:text/plain;base64,aGk="}`)).toContain("aGk=");
  });
});

describe("streamed transcript bodies", () => {
  // The detail view returns a body reference, so the bytes arrive raw from the
  // body endpoint rather than as Go's base64 JSON encoding.
  it("decodes raw Uint8Array bodies", () => {
    expect(decodeAuditBytes(new TextEncoder().encode('{"id":"resp_1"}'))).toBe('{"id":"resp_1"}');
    // Binary-safe: invalid UTF-8 must not throw the way the base64 path can.
    expect(decodeAuditBytes(new Uint8Array([0xff, 0xfe]))).toHaveLength(2);
  });

  it("streams a body from the conversation body endpoint", async () => {
    const requested: string[] = [];
    const original = globalThis.fetch;
    globalThis.fetch = (async (input: RequestInfo | URL) => {
      requested.push(String(input));
      return new Response(new TextEncoder().encode("body-bytes"), { status: 200 });
    }) as typeof fetch;
    try {
      const bytes = await fetchAuditConversationBody("34755", "response");
      expect(requested).toEqual(["/api/audit/conversations/34755/body/response"]);
      expect(decodeAuditBytes(bytes)).toBe("body-bytes");
    } finally {
      globalThis.fetch = original;
    }
  });

  it("surfaces a missing body as an error instead of an empty transcript", async () => {
    const original = globalThis.fetch;
    globalThis.fetch = (async () => new Response("body is unavailable", { status: 404 })) as typeof fetch;
    try {
      await expect(fetchAuditConversationBody("34755", "request")).rejects.toThrow("HTTP 404");
    } finally {
      globalThis.fetch = original;
    }
  });
});
