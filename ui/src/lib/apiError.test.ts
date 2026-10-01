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
