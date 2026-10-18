/**
 * Extracts a human message from an error body. The server answers with the
 * OpenAI-compatible envelope {src, error: {message, type, code}}, but a few
 * endpoints still return {error: "text"}, a bare {message}, or a validation
 * document {issues: [{path, message}]}. Returning a string for every shape
 * keeps callers from rendering "[object Object]".
 */
export function errorMessageFromPayload(payload: unknown, fallback: string): string {
  if (typeof payload === "string" && payload.trim()) return payload;
  if (payload && typeof payload === "object") {
    const record = payload as Record<string, unknown>;
    const error = record.error;
    if (typeof error === "string" && error.trim()) return error;
    if (error && typeof error === "object") {
      const nested = error as Record<string, unknown>;
      if (typeof nested.message === "string" && nested.message.trim()) return nested.message;
      // A doubly wrapped envelope: {error: {error: {message}}}.
      const deeper = nested.error;
      if (typeof deeper === "string" && deeper.trim()) return deeper;
      if (deeper && typeof deeper === "object") {
        const message = (deeper as Record<string, unknown>).message;
        if (typeof message === "string" && message.trim()) return message;
      }
    }
    const message = record.message;
    if (typeof message === "string" && message.trim()) return message;

    const issues = record.issues ?? record.errors;
    if (Array.isArray(issues) && issues.length > 0) {
      const first = issues[0];
      if (typeof first === "string" && first.trim()) return first;
      if (first && typeof first === "object") {
        const issue = first as Record<string, unknown>;
        const text = typeof issue.message === "string" ? issue.message.trim() : "";
        const path = typeof issue.path === "string" ? issue.path.trim() : "";
        if (text !== "") return path !== "" ? `${path}: ${text}` : text;
      }
    }
  }
  return fallback;
}
