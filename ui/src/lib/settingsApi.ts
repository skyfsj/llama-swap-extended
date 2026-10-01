export interface SettingsField {
  path: string;
  label: string;
  hint?: string;
  component: string;
  editor?: string;
  sensitive?: boolean;
  legacy?: boolean;
  advanced?: boolean;
  hidden?: boolean;
  default?: unknown;
  enum?: string[];
  provider?: string;
  required?: boolean;
  min?: number;
  max?: number;
}

export interface SettingsSchema {
  version: number;
  sections: Record<string, SettingsField[]>;
  sectionLabels?: Record<string, string>;
  schema: Record<string, unknown>;
}

export interface SettingsSource {
  path: string;
  writable: boolean;
  managed: boolean;
}

export interface SettingsSnapshot {
  revision: string;
  etag: string;
  config: Record<string, unknown>;
  yaml: string;
  sources: SettingsSource[];
  sourceYaml?: Record<string, string>;
  ownership?: Record<string, string>;
  writable: boolean;
  warnings?: SettingsDiagnostic[];
  pendingRestart: boolean;
  restartPaths?: string[];
  schemaVersion: number;
}

import { errorMessageFromPayload } from "./apiError";

export interface SettingsDiagnostic {
  code?: string;
  path?: string;
  sourceId?: string;
  severity?: string;
  message: string;
}

export interface SettingsChange {
  op: "add" | "replace" | "remove" | "test" | "reset";
  path: string;
  value?: unknown;
  sourceId?: string;
}

export interface SettingsSourceDraft {
  sourceId: string;
  yaml: string;
}

export interface SettingsDraft {
  mode: "structured" | "sources" | "restore";
  changes?: SettingsChange[];
  sources?: SettingsSourceDraft[];
  restoreRevision?: string;
  message?: string;
}

export interface SettingsImpact {
  class: string;
  paths?: string[];
}

export interface SettingsPreview {
  valid: boolean;
  baseEtag: string;
  previewHash: string;
  diff?: SettingsChange[];
  diagnostics?: SettingsDiagnostic[];
  impacts?: SettingsImpact[];
  migration?: string[];
  sourceTargets?: string[];
}

export interface SettingsRevision {
  id: string;
  parent?: string;
  createdAt: string;
  actor: { id: string; origin: string };
  message?: string;
  origin: string;
  result: string;
  changed?: string[];
  impacts?: SettingsImpact[];
}

export interface SettingsCommitResult {
  snapshot: SettingsSnapshot;
  revision: SettingsRevision;
  preview: SettingsPreview;
}

export interface SettingsOption {
  value: string;
  label: string;
}

export interface SettingsPermission {
  read: boolean;
  write: boolean;
  raw: boolean;
  history: boolean;
  models?: string[];
}

export type ApiError = Error & { status?: number; diagnostics?: SettingsDiagnostic[] };

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  const response = await fetch(url, { credentials: "same-origin", ...init });
  const payload = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  if (!response.ok) {
    let message = `HTTP ${response.status}`;
    let diagnostics: SettingsDiagnostic[] | undefined;
    if (payload && typeof payload === "object") {
      if (Array.isArray(payload.diagnostics)) {
        diagnostics = payload.diagnostics as SettingsDiagnostic[];
        const messages = diagnostics
          .filter((item) => item.severity !== "warning")
          .map((item) => item.message)
          .filter((item): item is string => typeof item === "string" && item.length > 0);
        if (messages.length > 0) message = messages.join("; ");
      } else {
        message = errorMessageFromPayload(payload, `HTTP ${response.status}`);
      }
    }
    const error = new Error(message) as ApiError;
    error.status = response.status;
    error.diagnostics = diagnostics;
    throw error;
  }
  return payload as T;
}

export function fetchSettingsSchema(): Promise<SettingsSchema> {
  return request<SettingsSchema>("/api/settings/schema");
}

export function fetchSettingsOptions(provider: string): Promise<{ items: SettingsOption[] }> {
  return request<{ items: SettingsOption[] }>(`/api/settings/options/${encodeURIComponent(provider)}`);
}

export function fetchSettingsPermission(): Promise<SettingsPermission> {
  return request<SettingsPermission>("/api/settings/permission");
}

export function fetchSettings(): Promise<SettingsSnapshot> {
  return request<SettingsSnapshot>("/api/settings/config");
}

export function previewSettings(draft: SettingsDraft): Promise<SettingsPreview> {
  return request<SettingsPreview>("/api/settings/config/preview", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(draft),
  });
}

export function commitSettings(snapshot: SettingsSnapshot, draft: SettingsDraft, previewHash: string): Promise<SettingsCommitResult> {
  return request<SettingsCommitResult>("/api/settings/config/commit", {
    method: "POST",
    headers: { "Content-Type": "application/json", "If-Match": snapshot.etag },
    body: JSON.stringify({ draft, previewHash }),
  });
}

export function fetchSettingsHistory(limit = 100): Promise<{ items: SettingsRevision[] }> {
  return request<{ items: SettingsRevision[] }>(`/api/settings/config/history?limit=${limit}`);
}
