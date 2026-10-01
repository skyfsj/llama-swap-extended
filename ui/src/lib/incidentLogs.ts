import { errorMessageFromPayload } from "./apiError";

export type IncidentLogKind = "inference-crash" | "request-error";

export interface IncidentLogEntry {
  name: string;
  kind: IncidentLogKind;
  model?: string;
  createdAt: string;
  size: number;
}

export interface IncidentLogResponse {
  items: IncidentLogEntry[];
  maxFiles: number;
}

export async function fetchIncidentLogs(): Promise<IncidentLogResponse> {
  const response = await fetch("/api/logs/incidents", { credentials: "same-origin" });
  const payload = await response.json().catch(() => ({})) as Record<string, unknown>;
  if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));

  const items = Array.isArray(payload.items)
    ? payload.items.filter((item): item is IncidentLogEntry => {
      if (!item || typeof item !== "object") return false;
      const entry = item as Partial<IncidentLogEntry>;
      return typeof entry.name === "string"
        && (entry.kind === "inference-crash" || entry.kind === "request-error")
        && typeof entry.createdAt === "string";
    })
    : [];
  const maxFiles = typeof payload.maxFiles === "number" && Number.isFinite(payload.maxFiles)
    ? payload.maxFiles
    : 5;
  return { items, maxFiles };
}

export function incidentLogURL(name: string): string {
  return `/api/logs/incidents/${encodeURIComponent(name)}`;
}

export async function fetchIncidentLog(name: string): Promise<string> {
  const response = await fetch(incidentLogURL(name), { credentials: "same-origin" });
  const body = await response.text();
  if (!response.ok) {
    let payload: unknown = body;
    try {
      payload = JSON.parse(body);
    } catch {
      // Keep the plain-text server error as the useful message.
    }
    throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
  }
  return body;
}
