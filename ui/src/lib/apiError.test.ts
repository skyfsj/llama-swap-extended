import { describe, expect, it } from "vitest";
import { errorMessageFromPayload } from "./apiError";

describe("errorMessageFromPayload", () => {
  it("reads the message from an object-valued API error", () => {
    expect(errorMessageFromPayload({ error: { message: "保存失败" } }, "HTTP 400")).toBe("保存失败");
  });

  it("keeps string errors and falls back for unknown payloads", () => {
    expect(errorMessageFromPayload({ error: "invalid config" }, "HTTP 400")).toBe("invalid config");
    expect(errorMessageFromPayload({ error: {} }, "HTTP 400")).toBe("HTTP 400");
  });
});

describe("errorMessageFromPayload envelopes", () => {
  it("extracts the message from the server's envelope object", () => {
    // Exactly what PATCH /api/config returns on a validation failure.
    const payload = {
      src: "llama-swap",
      error: {
        message: "providers: adapters[0]: endpoint is required",
        type: "invalid_request_error",
        param: null,
        code: "bad_request",
      },
    };
    expect(errorMessageFromPayload(payload, "fallback")).toBe("providers: adapters[0]: endpoint is required");
  });

  it("renders validation issues with their path", () => {
    const payload = { valid: false, issues: [{ path: "/providers/go/adapters/0", message: "endpoint is required" }] };
    expect(errorMessageFromPayload(payload, "fallback")).toBe("/providers/go/adapters/0: endpoint is required");
  });

  it("still accepts legacy string errors and bare messages", () => {
    expect(errorMessageFromPayload({ error: "plain text" }, "fallback")).toBe("plain text");
    expect(errorMessageFromPayload({ message: "bare" }, "fallback")).toBe("bare");
    expect(errorMessageFromPayload(undefined, "fallback")).toBe("fallback");
    // Never render an object: an unknown shape falls back to the caller's text.
    expect(errorMessageFromPayload({ error: { unexpected: true } }, "fallback")).toBe("fallback");
  });
});
