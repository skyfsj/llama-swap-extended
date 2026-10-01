export function errorMessageFromPayload(payload: unknown, fallback: string): string {
  if (payload && typeof payload === "object") {
    const record = payload as Record<string, unknown>;
    const error = record.error;
    if (typeof error === "string" && error.trim()) return error;
    if (error && typeof error === "object") {
      const message = (error as Record<string, unknown>).message;
      if (typeof message === "string" && message.trim()) return message;
    }
    const message = record.message;
    if (typeof message === "string" && message.trim()) return message;
  }
  return fallback;
}
