import { requestJson, HttpApiError } from "./http";

export interface ExtensionManifest {
  id: string;
  name: string;
  description?: string;
  enabled: boolean;
  priority: number;
  match: {
    models: string[];
    excludeModels: string[];
    profiles: string[];
    providers: string[];
    endpoints: string[];
  };
  permissions: {
    networkHosts: string[];
    readRoots: string[];
    writeRoots: string[];
    storage?: string;
  };
  timeout: string;
  maxCpuMillis: number;
  maxMemoryMiB: number;
  continueOnError: boolean;
  toolConflict: string;
  interceptClientTools: boolean;
  config?: Record<string, unknown>;
}

export interface ExtensionSettingField {
  key: string;
  label: string;
  hint?: string;
  labelTranslations?: Record<string, string>;
  hintTranslations?: Record<string, string>;
  component: string;
  options?: string[];
  default?: unknown;
  required?: boolean;
  secret?: boolean;
  min?: number;
  max?: number;
  section?: string;
}

export interface ExtensionDefinition {
  manifest: ExtensionManifest;
  files: Record<string, string>;
  /** Empty directories kept for structure; files inside live in `files`. */
  directories?: string[];
  etag: string;
  status: string;
  settings?: ExtensionSettingField[];
  lastError?: string;
}

export interface ExtensionDiagnostic {
  path: string;
  line: number;
  column: number;
  length: number;
  severity: string;
  message: string;
}

export const EXTENSION_ENTRY_POINT = "index.js";

export function blankExtensionManifest(id = ""): ExtensionManifest {
  return {
    id,
    name: "",
    enabled: false,
    priority: 100,
    match: { models: [], excludeModels: [], profiles: [], providers: [], endpoints: [] },
    permissions: { networkHosts: [], readRoots: [], writeRoots: [] },
    timeout: "10s",
    maxCpuMillis: 2000,
    maxMemoryMiB: 256,
    continueOnError: false,
    toolConflict: "skip",
    interceptClientTools: false,
    config: {},
  };
}

export function blankExtension(): ExtensionDefinition {
  return {
    manifest: blankExtensionManifest(),
    files: { [EXTENSION_ENTRY_POINT]: "export default {\n  tools: [],\n  async onRequest(ctx, request) {\n    return request;\n  }\n};\n" },
    etag: "",
    status: "new",
  };
}

export async function listExtensions(): Promise<ExtensionDefinition[]> {
  const result = await requestJson<{ data: ExtensionDefinition[] }>("/api/extensions");
  return result.data ?? [];
}

export async function getExtension(id: string): Promise<ExtensionDefinition> {
  return requestJson<ExtensionDefinition>(`/api/extensions/${encodeURIComponent(id)}`);
}

export async function saveExtension(
  draft: ExtensionDefinition,
): Promise<ExtensionDefinition> {
  const update = !!draft.etag;
  const url = update ? `/api/extensions/${encodeURIComponent(draft.manifest.id)}` : "/api/extensions";
  return requestJson<ExtensionDefinition>(url, {
    method: update ? "PUT" : "POST",
    headers: { "Content-Type": "application/json", ...(update ? { "If-Match": draft.etag } : {}) },
    body: JSON.stringify(draft),
  });
}

export async function deleteExtension(id: string, etag: string): Promise<void> {
  await requestJson(`/api/extensions/${encodeURIComponent(id)}`, {
    method: "DELETE",
    headers: { "If-Match": etag },
  });
}

export async function duplicateExtension(id: string, newId: string): Promise<ExtensionDefinition> {
  return requestJson<ExtensionDefinition>(
    `/api/extensions/${encodeURIComponent(id)}/duplicate`,
    { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id: newId }) },
  );
}

export async function reloadExtensions(): Promise<ExtensionDefinition[]> {
  const result = await requestJson<{ data: ExtensionDefinition[] }>("/api/extensions/reload", { method: "POST" });
  return result.data ?? [];
}

export interface ExtensionTestResult {
  durationMs?: number;
  matched: boolean;
  hooks: string[];
  requestBefore: Record<string, unknown>;
  requestAfter: Record<string, unknown>;
  injectedTools: unknown[];
  logs: string[];
  error?: string;
}

export async function testExtension(
  id: string,
  request: Record<string, unknown>,
  endpoint: string,
  options?: { signal?: AbortSignal; profile?: string; provider?: string; stream?: boolean; files?: Record<string, string> },
): Promise<ExtensionTestResult> {
  return requestJson<ExtensionTestResult>(
    `/api/extensions/${encodeURIComponent(id)}/test`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      signal: options?.signal,
      body: JSON.stringify({ context: { resolvedModel: request.model, requestedModel: request.model, endpoint, profile: options?.profile, provider: options?.provider, stream: options?.stream ?? false }, request, files: options?.files }),
    },
  );
}

export async function checkExtension(
  payload: { files: Record<string, string> } | { path: string; source: string },
): Promise<ExtensionDiagnostic[]> {
  const result = await requestJson<{ diagnostics: ExtensionDiagnostic[] }>("/api/extensions/check", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(payload),
  });
  return result.diagnostics ?? [];
}

export interface ExtensionPresetSetting {
  key: string;
  label: string;
  hint?: string;
  component: string;
  options?: string[];
  default?: unknown;
  required?: boolean;
  secret?: boolean;
  min?: number;
  max?: number;
}

export interface ExtensionPreset {
  id: string;
  name: string;
  description: string;
  category: string;
  tags?: string[];
  settings?: ExtensionPresetSetting[];
  hosts: string[];
  tools: string[];
  installed: boolean;
}

/** Result of the import preview: what the archive would install. */
export interface ExtensionImportPreview {
  manifest: {
    id: string;
    name: string;
    description: string;
    enabled: boolean;
    permissions: { networkHosts?: string[]; readRoots?: string[]; writeRoots?: string[]; storage?: string };
    match: { models?: string[]; profiles?: string[]; providers?: string[]; endpoints?: string[] };
  };
  files: string[];
  diagnostics: string[];
  /** true when the archive's id already exists */
  conflict: boolean;
}

export async function exportExtension(id: string): Promise<Blob> {
  const response = await fetch(`/api/extensions/export/${encodeURIComponent(id)}`, { credentials: "same-origin" });
  if (!response.ok) throw new Error(`HTTP ${response.status}`);
  return response.blob();
}

/** previewExtensionImport parses an uploaded archive without writing anything. */
export async function previewExtensionImport(file: File): Promise<ExtensionImportPreview> {
  const form = new FormData();
  form.append("file", file);
  return requestJson<ExtensionImportPreview>("/api/extensions/import/preview", { method: "POST", body: form });
}

/**
 * importExtension writes an uploaded archive. mode "replace" requires the
 * existing definition's etag; a rename is expressed with the id query.
 */
export async function importExtension(file: File, options: { mode?: "create" | "replace"; id?: string; etag?: string }): Promise<ExtensionDefinition> {
  const form = new FormData();
  form.append("file", file);
  const params = new URLSearchParams();
  if (options.mode) params.set("mode", options.mode);
  if (options.id) params.set("id", options.id);
  const query = params.toString();
  const headers: Record<string, string> = {};
  if (options.etag) headers["If-Match"] = options.etag;
  return requestJson<ExtensionDefinition>(`/api/extensions/import${query ? `?${query}` : ""}`, { method: "POST", headers, body: form });
}

export async function listPresets(): Promise<ExtensionPreset[]> {
  const result = await requestJson<{ data: ExtensionPreset[] }>("/api/extensions/presets");
  return result.data ?? [];
}

export async function installPreset(presetId: string, targetId?: string): Promise<ExtensionDefinition> {
  return requestJson<ExtensionDefinition>(
    `/api/extensions/presets/${encodeURIComponent(presetId)}/install`,
    { method: "POST", headers: { "Content-Type": "application/json" }, body: JSON.stringify({ id: targetId ?? "" }) },
  );
}

/** settingDiagnosticsFrom reads the per-field errors the server returns with a 422. */
export function settingDiagnosticsFrom(error: unknown): Record<string, string> {
  if (!(error instanceof HttpApiError)) return {};
  const diagnostics = error.payload?.diagnostics;
  if (!Array.isArray(diagnostics)) return {};
  const byKey: Record<string, string> = {};
  for (const entry of diagnostics) {
    const record = entry as { path?: unknown; message?: unknown };
    if (typeof record.path === "string" && typeof record.message === "string") {
      byKey[record.path] = record.message;
    }
  }
  return byKey;
}

/**
 * fetchExtensionLogHistory reads the persisted rolling log tail for an
 * extension (plain text, newest tail).
 */
export async function fetchExtensionLogHistory(id: string): Promise<string> {
  const response = await fetch(`/api/extensions/logs/${encodeURIComponent(id)}`, { credentials: "same-origin" });
  if (!response.ok) return "";
  return response.text();
}

/** subscribeExtensionLogStream opens the live SSE-ish chunked log stream. */
export async function* streamExtensionLogs(id: string, options?: { signal?: AbortSignal; noHistory?: boolean }): AsyncGenerator<string> {
  const query = options?.noHistory ? "?no-history" : "";
  const response = await fetch(`/api/extensions/logs/${encodeURIComponent(id)}/stream${query}`, {
    credentials: "same-origin",
    signal: options?.signal,
  });
  if (!response.ok || !response.body) throw new Error(`HTTP ${response.status}`);
  const reader = response.body.getReader();
  const decoder = new TextDecoder();
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) return;
      yield decoder.decode(value, { stream: true });
    }
  } finally {
    reader.releaseLock();
  }
}
