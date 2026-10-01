import { requestJson } from "./http";
import type {
  RuntimeCatalogResponse,
  RuntimeDetail,
  RuntimeReadiness,
  RuntimeStatus,
} from "./types";

export interface RuntimeListResponse {
  data?: RuntimeStatus[];
}

// request re-exports the shared control-plane fetch helper so the call sites
// below stay terse and all clients share one error/credentials contract.
const request = requestJson;

export function fetchRuntimes(): Promise<RuntimeListResponse> {
  return request<RuntimeListResponse>("/api/runtimes");
}

export function fetchRuntimeDetail(name: string): Promise<RuntimeDetail> {
  return request<RuntimeDetail>(`/api/runtimes/${encodeURIComponent(name)}`);
}

export function fetchRuntimeReadiness(name: string, version = ""): Promise<RuntimeReadiness> {
  const query = version.trim() ? `?version=${encodeURIComponent(version.trim())}` : "";
  return request<RuntimeReadiness>(`/api/runtimes/${encodeURIComponent(name)}/readiness${query}`);
}

export function fetchRuntimeCatalog(name: string): Promise<RuntimeCatalogResponse> {
  return request<RuntimeCatalogResponse>(`/api/runtimes/${encodeURIComponent(name)}/versions`);
}

export async function postRuntimeAction<T = unknown>(name: string, path: string, body?: unknown): Promise<T> {
  return request<T>(`/api/runtimes/${encodeURIComponent(name)}/${path}`, {
    method: "POST",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

export async function deleteRuntimeVersion(name: string, version: string): Promise<void> {
  await request<unknown>(`/api/runtimes/${encodeURIComponent(name)}/versions/${encodeURIComponent(version)}`, {
    method: "DELETE",
  });
}

export interface RuntimeLogResponse {
  runtime: string;
  operationId?: string;
  output: string;
}

export function fetchRuntimeLog(name: string): Promise<RuntimeLogResponse> {
  return request<RuntimeLogResponse>(`/api/runtimes/${encodeURIComponent(name)}/logs`);
}

export interface LMCacheServerLogResponse {
  logPath: string;
  size: number;
  truncated: boolean;
  output: string;
}

export function fetchLMCacheServerLog(): Promise<LMCacheServerLogResponse> {
  return request<LMCacheServerLogResponse>("/api/lmcache/server/logs");
}
