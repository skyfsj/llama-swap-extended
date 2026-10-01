import { requestJson } from "./http";

/** Upstream API protocol of a provider. */
export type ProviderType = "openai" | "anthropic";

/** One upstream API key entry (config shape). The secret never round-trips. */
export interface ProviderKeyEntry {
  value?: string;
  valueEnv?: string;
  weight?: number;
  priority?: number;
  maxConcurrent?: number;
  proxy?: string;
  disabled?: boolean;
}

/** One model mapping entry (config shape). */
export interface ProviderModelEntry {
  local: string;
  remote: string;
}

/** A provider as configured in the YAML providers: section. */
export interface ProviderConfigUI {
  type: ProviderType;
  endpoint: string;
  proxy?: string;
  priority?: number;
  weight?: number;
  sticky?: boolean;
  bareNamePolicy?: "local-first" | "provider-first";
  maxRetries?: number;
  disabled?: boolean;
  apiKeys: ProviderKeyEntry[];
  models: ProviderModelEntry[];
}

/** Runtime state of one credential as reported by /api/providers/status. */
export interface ProviderKeyStatus {
  id: string;
  disabled: boolean;
  unresolvedEnv: boolean;
  inflight: number;
  maxConcurrent: number;
  authCooldownUntil?: string;
  rateLimitCooldownUntil?: string;
  transientCooldownUntil?: string;
  lastError?: string;
  lastErrorAt?: string;
  lastUsed?: string;
}

export interface ProviderRouteStatus {
  providerID: string;
  modelID: string;
  remoteName: string;
  fqn: string;
  sticky: boolean;
  providerFirst: boolean;
  maxRetries: number;
  keys: ProviderKeyStatus[];
}

export interface ProvidersStatusResponse {
  providers: Array<{
    id: string;
    type: ProviderType;
    endpoint: string;
    proxy?: string;
    priority?: number;
    weight?: number;
    sticky?: boolean;
    bareNamePolicy?: string;
    maxRetries?: number;
    disabled?: boolean;
    modelCount: number;
    keyCount: number;
  }>;
  routes: ProviderRouteStatus[];
}

export interface ProvidersProbeRequest {
  type: ProviderType;
  endpoint: string;
  proxy?: string;
  apiKey?: string;
  timeoutSeconds?: number;
}

export interface ProvidersProbeResponse {
  models: string[];
}

/**
 * Normalizes one provider entry from the config JSON. Entries that fail
 * validation are dropped rather than rendered as partial data.
 */
export function normalizeProvider(value: unknown): ProviderConfigUI | null {
  if (!value || typeof value !== "object") return null;
  const record = value as Record<string, unknown>;
  const endpoint = typeof record.endpoint === "string" ? record.endpoint : "";
  if (endpoint.trim() === "") return null;
  const type: ProviderType = record.type === "anthropic" ? "anthropic" : "openai";
  const apiKeys = Array.isArray(record.apiKeys)
    ? (record.apiKeys
        .map((item: unknown): ProviderKeyEntry | null => {
          if (typeof item === "string") return { value: item };
          if (!item || typeof item !== "object") return null;
          const entry = item as Record<string, unknown>;
          const key: ProviderKeyEntry = {};
          if (typeof entry.value === "string" && entry.value !== "") key.value = entry.value;
          if (typeof entry.valueEnv === "string" && entry.valueEnv !== "") key.valueEnv = entry.valueEnv;
          if (typeof entry.weight === "number") key.weight = entry.weight;
          if (typeof entry.priority === "number") key.priority = entry.priority;
          if (typeof entry.maxConcurrent === "number") key.maxConcurrent = entry.maxConcurrent;
          if (typeof entry.proxy === "string" && entry.proxy !== "") key.proxy = entry.proxy;
          if (entry.disabled === true) key.disabled = true;
          if (!key.value && !key.valueEnv) return null;
          return key;
        })
        .filter((item): item is ProviderKeyEntry => item !== null))
    : [];
  const models = Array.isArray(record.models)
    ? (record.models
        .map((item: unknown): ProviderModelEntry | null => {
          if (typeof item === "string" && item.trim() !== "") return { local: item, remote: item };
          if (!item || typeof item !== "object") return null;
          const entry = item as Record<string, unknown>;
          // Shorthand mapping {localName: upstreamName} from the YAML form.
          const keys = Object.keys(entry);
          if (keys.length === 1 && keys[0] !== "local" && typeof entry[keys[0]] === "string" && entry[keys[0]] !== "") {
            return { local: keys[0], remote: entry[keys[0]] as string };
          }
          if (typeof entry.local !== "string" || entry.local.trim() === "") return null;
          const remote = typeof entry.remote === "string" && entry.remote.trim() !== "" ? entry.remote : entry.local;
          return { local: entry.local, remote };
        })
        .filter((item): item is ProviderModelEntry => item !== null))
    : [];
  return {
    type,
    endpoint,
    proxy: typeof record.proxy === "string" && record.proxy !== "" ? record.proxy : undefined,
    priority: typeof record.priority === "number" ? record.priority : undefined,
    weight: typeof record.weight === "number" ? record.weight : undefined,
    sticky: record.sticky === false ? false : true,
    bareNamePolicy: record.bareNamePolicy === "provider-first" ? "provider-first" : "local-first",
    maxRetries: typeof record.maxRetries === "number" ? record.maxRetries : undefined,
    disabled: record.disabled === true,
    apiKeys,
    models,
  };
}

/** Extracts the providers: section from a full config snapshot. */
export function normalizeProvidersSection(configValue: unknown): Record<string, ProviderConfigUI> {
  if (!configValue || typeof configValue !== "object") return {};
  const providers = (configValue as Record<string, unknown>).providers;
  if (!providers || typeof providers !== "object") return {};
  const out: Record<string, ProviderConfigUI> = {};
  for (const [id, entry] of Object.entries(providers as Record<string, unknown>)) {
    const normalized = normalizeProvider(entry);
    if (normalized) out[id] = normalized;
  }
  return out;
}

/** Builds a providers: section suitable for a config PATCH from UI state. */
export function providersSectionToConfig(providers: Record<string, ProviderConfigUI>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [id, p] of Object.entries(providers)) {
    out[id] = {
      type: p.type,
      endpoint: p.endpoint,
      ...(p.proxy ? { proxy: p.proxy } : {}),
      ...(p.priority !== undefined ? { priority: p.priority } : {}),
      ...(p.weight !== undefined ? { weight: p.weight } : {}),
      ...(p.sticky === false ? { sticky: false } : {}),
      ...(p.bareNamePolicy === "provider-first" ? { bareNamePolicy: "provider-first" } : {}),
      ...(p.maxRetries !== undefined ? { maxRetries: p.maxRetries } : {}),
      ...(p.disabled ? { disabled: true } : {}),
      apiKeys: p.apiKeys.map((k) => ({
        ...(k.value !== undefined ? { value: k.value } : {}),
        ...(k.valueEnv ? { valueEnv: k.valueEnv } : {}),
        ...(k.weight !== undefined ? { weight: k.weight } : {}),
        ...(k.priority !== undefined ? { priority: k.priority } : {}),
        ...(k.maxConcurrent !== undefined ? { maxConcurrent: k.maxConcurrent } : {}),
        ...(k.proxy ? { proxy: k.proxy } : {}),
        ...(k.disabled ? { disabled: true } : {}),
      })),
      models: p.models.map((m) => (m.local === m.remote ? m.local : { local: m.local, remote: m.remote })),
    };
  }
  return out;
}

/** Fetches the runtime status of all providers and their credentials. */
export async function fetchProvidersStatus(): Promise<ProvidersStatusResponse> {
  const payload = await requestJson<ProvidersStatusResponse>("/api/providers/status");
  return {
    providers: Array.isArray(payload?.providers) ? payload.providers : [],
    routes: Array.isArray(payload?.routes) ? payload.routes : [],
  };
}

/** Probes a draft provider: connectivity test + upstream model list. */
export async function probeProvider(req: ProvidersProbeRequest): Promise<ProvidersProbeResponse> {
  return requestJson<ProvidersProbeResponse>("/api/providers/probe", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(req),
  });
}

/** Clears cooldowns on one credential. */
export async function clearProviderKeyCooldown(keyID: string): Promise<void> {
  await requestJson(`/api/providers/keys/${encodeURIComponent(keyID)}/clear-cooldown`, {
    method: "POST",
  });
}

/** A full config snapshot as served by GET /api/config. */
export interface ConfigSnapshot {
  config?: Record<string, unknown>;
  yaml: string;
  etag: string;
  writable: boolean;
  restartRequired?: boolean;
  restartPaths?: string[];
}

/** Loads the full config snapshot (providers section included). */
export async function fetchConfigSnapshot(): Promise<ConfigSnapshot> {
  const response = await fetch("/api/config", { credentials: "same-origin" });
  const payload = await response.json().catch(() => ({}));
  if (!response.ok) {
    throw new Error(`${payload?.error ?? `HTTP ${response.status}`}`);
  }
  return payload as ConfigSnapshot;
}

/**
 * Saves the providers section by patching the full config: the patch
 * replaces /providers wholesale. A REDACTED sentinel left in a key value is
 * restored server-side; values the user typed override the stored secret.
 */
export async function saveProvidersSection(
  etag: string,
  providers: Record<string, ProviderConfigUI>,
): Promise<ConfigSnapshot> {
  const response = await fetch("/api/config", {
    method: "PATCH",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "If-Match": etag },
    body: JSON.stringify([{ op: "replace", path: "/providers", value: providersSectionToConfig(providers) }]),
  });
  const payload = await response.json().catch(() => ({}));
  if (response.status === 412) throw new Error("config conflict: reload and retry");
  if (!response.ok) {
    throw new Error(`${payload?.error ?? `HTTP ${response.status}`}`);
  }
  return payload as ConfigSnapshot;
}
