import { errorMessageFromPayload } from "./apiError";

// HttpApiError carries the HTTP status and the parsed payload of a failed API
// call, so callers can branch on status (409 conflicts, 403 scopes, 423 etag
// mismatches) without matching error strings.
export class HttpApiError extends Error {
  readonly status: number;
  readonly payload: Record<string, unknown>;

  constructor(message: string, status: number, payload: Record<string, unknown> = {}) {
    super(message);
    this.name = "HttpApiError";
    this.status = status;
    this.payload = payload;
  }
}

// requestJson is the shared fetch helper for the control-plane API clients:
// it always sends same-origin credentials (session-cookie authentication),
// tolerates empty or invalid JSON bodies, and throws HttpApiError with a
// message derived from the payload envelope. Client modules wrap it to attach
// their own error types or payload diagnostics.
export async function requestJson<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, { credentials: "same-origin", ...init });
  const payload = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok) {
    throw new HttpApiError(
      errorMessageFromPayload(payload, `HTTP ${response.status}`),
      response.status,
      payload,
    );
  }
  return payload as T;
}
