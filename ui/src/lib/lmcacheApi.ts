import { HttpApiError, requestJson } from "./http";
import type {
  LMCacheDashboardResponse,
  LMCacheStatus,
  RuntimeCatalogResponse,
} from "./types";

export interface LMCacheConfigSnapshot {
  config?: Record<string, unknown>;
  yaml?: string;
  etag: string;
  writable: boolean;
  restartRequired?: boolean;
  restartPaths?: string[];
}

export class LMCacheApiError extends Error {
  status?: number;

  constructor(message: string, status?: number) {
    super(message);
    this.name = "LMCacheApiError";
    this.status = status;
  }
}

async function request<T>(url: string, init?: RequestInit): Promise<T> {
  try {
    return await requestJson<T>(url, init);
  } catch (cause) {
    if (cause instanceof HttpApiError) {
      throw new LMCacheApiError(cause.message, cause.status);
    }
    throw cause;
  }
}

export function fetchLMCacheStatus(): Promise<LMCacheStatus> {
  return request<LMCacheStatus>("/api/lmcache");
}

export function fetchLMCacheDashboard(): Promise<LMCacheDashboardResponse> {
  return request<LMCacheDashboardResponse>("/api/lmcache/dashboard");
}

export function fetchLMCacheCandidates(): Promise<RuntimeCatalogResponse> {
  return request<RuntimeCatalogResponse>("/api/runtimes/lmcache/versions");
}

export function fetchLMCacheConfig(): Promise<LMCacheConfigSnapshot> {
  return request<LMCacheConfigSnapshot>("/api/config");
}

export function postLMCacheAction<T = unknown>(path: string, body?: unknown): Promise<T> {
  return request<T>(`/api/lmcache/${path}`, {
    method: "POST",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
}

export function patchLMCacheConfig(
  snapshot: LMCacheConfigSnapshot,
  lmcache: Record<string, unknown>,
): Promise<LMCacheConfigSnapshot> {
  const config = snapshot.config ?? {};
  const hasLMCache = Object.prototype.hasOwnProperty.call(config, "lmcache")
    || Object.prototype.hasOwnProperty.call(config, "LMCache");
  const patch = [{
    op: hasLMCache ? "replace" : "add",
    path: "/lmcache",
    value: lmcache,
  }];
  return request<LMCacheConfigSnapshot>("/api/config", {
    method: "PATCH",
    headers: {
      "Content-Type": "application/json",
      "If-Match": snapshot.etag,
    },
    body: JSON.stringify(patch),
  }).then((result) => {
    const validation = result as LMCacheConfigSnapshot & {
      valid?: boolean;
      issues?: Array<{ message?: string }>;
    };
    if (validation.valid === false) {
      const detail = validation.issues?.map((issue) => issue.message).filter(Boolean).join("; ");
      throw new LMCacheApiError(detail || "LMCache configuration was rejected");
    }
    return result;
  });
}
