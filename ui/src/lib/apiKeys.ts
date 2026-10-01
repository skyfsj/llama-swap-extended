import { requestJson } from "./http";

// request re-exports the shared control-plane fetch helper so every call site
// shares one error/credentials contract.
const request = requestJson;

/** The two credential kinds the control plane distinguishes. */
export type ApiKeyKind = "management" | "access";

export interface APIKey {
  id: string;
  name: string;
  kind: ApiKeyKind;
  parentId?: string;
  scopes: string[];
  models: string[];
  allowedIps: string[];
  maxConcurrency: number;
  group: string;
  allowManagementLogin: boolean;
  createdAt: string;
  expiresAt?: string;
  lastUsedAt?: string;
  revokedAt?: string;
  /** Live in-flight requests, reported by the control plane. */
  activeRequests?: number;
}

export interface APIKeyListResponse {
  data?: APIKey[];
}

export interface APIKeyCreateRequest {
  name: string;
  kind: ApiKeyKind;
  parentId?: string;
  models?: string[];
  allowedIps?: string[];
  maxConcurrency?: number;
  group?: string;
  allowManagementLogin?: boolean;
  expiresAt?: string | null;
}

export interface APIKeyCreateResponse {
  key: string;
  record: APIKey;
}

export interface APIKeyUpdateRequest {
  name?: string;
  models?: string[];
  allowedIps?: string[];
  maxConcurrency?: number;
  group?: string;
  allowManagementLogin?: boolean;
  expiresAt?: string | null;
}

/**
 * Normalizes one key row from the control plane. The list deliberately keeps
 * rows that fail normalization out rather than rendering partial data, and the
 * secret is never part of this projection.
 */
export function normalizeAPIKey(value: unknown): APIKey | null {
  if (!value || typeof value !== "object") return null;
  const record = value as Record<string, unknown>;
  if (typeof record.id !== "string" || record.id.trim() === "") return null;
  return {
    id: record.id,
    name: typeof record.name === "string" ? record.name : "",
    kind: record.kind === "access" ? "access" : "management",
    parentId: typeof record.parentId === "string" && record.parentId ? record.parentId : undefined,
    scopes: Array.isArray(record.scopes) ? record.scopes.filter((item): item is string => typeof item === "string") : ["inference"],
    models: Array.isArray(record.models) ? record.models.filter((item): item is string => typeof item === "string") : [],
    allowedIps: Array.isArray(record.allowedIps) ? record.allowedIps.filter((item): item is string => typeof item === "string") : [],
    maxConcurrency: typeof record.maxConcurrency === "number" ? record.maxConcurrency : 0,
    group: typeof record.group === "string" ? record.group : "",
    allowManagementLogin: record.allowManagementLogin === true,
    createdAt: typeof record.createdAt === "string" ? record.createdAt : "",
    expiresAt: typeof record.expiresAt === "string" ? record.expiresAt : undefined,
    lastUsedAt: typeof record.lastUsedAt === "string" ? record.lastUsedAt : undefined,
    revokedAt: typeof record.revokedAt === "string" ? record.revokedAt : undefined,
    activeRequests: typeof record.activeRequests === "number" ? record.activeRequests : 0,
  };
}

export async function fetchAPIKeys(includeRevoked = false): Promise<APIKey[]> {
  const query = includeRevoked ? "?include_revoked=true" : "";
  const payload = await request<APIKeyListResponse>(`/api/keys${query}`);
  if (!Array.isArray(payload?.data)) return [];
  return payload.data.map(normalizeAPIKey).filter((key): key is APIKey => key !== null);
}

export async function createAPIKey(request_: APIKeyCreateRequest): Promise<APIKeyCreateResponse> {
  return request<APIKeyCreateResponse>("/api/keys", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(request_),
  });
}

export async function updateAPIKey(id: string, update: APIKeyUpdateRequest): Promise<APIKey> {
  return request<APIKey>(`/api/keys/${encodeURIComponent(id)}`, {
    method: "PATCH",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(update),
  });
}

export async function revokeAPIKey(id: string): Promise<void> {
  await request<unknown>(`/api/keys/${encodeURIComponent(id)}`, { method: "DELETE" });
}

export async function rotateAPIKey(id: string): Promise<APIKeyCreateResponse> {
  return request<APIKeyCreateResponse>(`/api/keys/${encodeURIComponent(id)}/rotate`, { method: "POST" });
}
