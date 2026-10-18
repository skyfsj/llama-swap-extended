import { errorMessageFromPayload } from "./apiError";
import { requestJson } from "./http";

/** Upstream API protocol of a provider. */
export type ProviderType = "openai" | "openai-responses" | "anthropic" | "gemini" | "gemini-interactions";

/** Per-provider cooldown policy. */
export type ProviderCooldownPolicy = "inherit" | "enabled" | "disabled";

/** Cooldown policy of a provider as a plain policy string. */
export function cooldownPolicyOf(value: ProviderConfigUI["cooldown"]): ProviderCooldownPolicy {
  if (typeof value === "string") return value;
  if (value && typeof value === "object" && typeof value.policy === "string") {
    return value.policy;
  }
  return "inherit";
}

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

/** Normalizes a provider/adapter type, defaulting to the Completions surface. */
export function normalizeProviderType(value: unknown): ProviderType {
  switch (value) {
    case "anthropic":
    case "gemini":
    case "gemini-interactions":
    case "openai-responses":
      return value;
    default:
      return "openai";
  }
}

/** One model mapping entry (config shape). */
/** The capability block a provider model may declare (same shape as a local
 * model's): which modalities go in and come out, or a translation role. */
export interface ProviderModelCapabilities {
  in?: string[];
  out?: string[];
  translation?: boolean;
}

export interface ProviderModelEntry {
  local: string;
  remote: string;
  /** Extra client-facing names for the same upstream model. */
  aliases?: string[];
  /** Outbound proxy for this model only ("direct" bypasses all proxies). */
  proxy?: string;
  /**
   * What the upstream model accepts and produces. The playground files the
   * model under its tooling from this block; empty reads as a chat model.
   */
  capabilities?: ProviderModelCapabilities;
}

/** One upstream API surface of a provider. */
export interface ProviderAdapterUI {
  name?: string;
  type: ProviderType;
  endpoint: string;
  /** Literal request-path prefix rewrites applied before forwarding. */
  pathMap?: Record<string, string>;
  models: ProviderModelEntry[];
  /** Outbound proxy for this adapter ("direct" bypasses all proxies). */
  proxy?: string;
  /** Adapter-level credentials; omitted inherits the provider key set. */
  apiKeys?: ProviderKeyEntry[];
  /** Glob patterns used to route pulled models into this adapter. */
  modelMatch?: string[];
  /** Catch-all adapter for pulled models matching no other adapter. */
  default?: boolean;
}

/** Quota (subscription usage) configuration of a provider. */
export interface ProviderQuotaConfigUI {
  preset?: string;
  endpoint?: string;
  refreshInterval?: string;
  enforce?: { enabled?: boolean; threshold?: number; fallbackCooldown?: string };
}

/** One usage window of a credential as reported by /api/providers/quotas. */
export interface QuotaWindowStatus {
  id: string;
  label: string;
  /** Used share of the window (0-100), normalized by the server. */
  percent: number;
  remainingPercent: number;
  status?: string;
  resetsAt?: string;
}

export interface QuotaExtra {
  id: string;
  label: string;
  value: unknown;
}

export interface CredentialQuota {
  providerID: string;
  keyID: string;
  windows?: QuotaWindowStatus[];
  extras?: QuotaExtra[];
  fetchedAt?: string;
  error?: string;
  enforced?: boolean;
  quotaCooldownUntil?: string;
}

/** A provider as configured in the YAML providers: section. */
export interface ProviderConfigUI {
  /** Primary adapter dialect; derived from adapters[0] by normalization. */
  type: ProviderType;
  /** Primary adapter endpoint; derived from adapters[0] by normalization. */
  endpoint: string;
  /** Models of the primary adapter; the flat single-adapter shorthand. */
  models: ProviderModelEntry[];
  /** Every upstream surface; always materialized (at least one entry). */
  adapters: ProviderAdapterUI[];
  proxy?: string;
  priority?: number;
  weight?: number;
  sticky?: boolean;
  /** Keep the upstream model name callable next to the local name and aliases. */
  exposeUpstreamName?: boolean;
  bareNamePolicy?: "local-first" | "provider-first";
  maxRetries?: number;
  disabled?: boolean;
  /** Request headers set to the session id on upstream requests. */
  sessionHeaders?: string[];
  /** Static headers sent upstream (credential headers excluded). */
  headers?: Record<string, string>;
  /** Per-provider cooldown policy: inherit (default), enabled, disabled. */
  cooldown?: ProviderCooldownPolicy | { policy?: ProviderCooldownPolicy; auth?: string; authProbe?: string; rateLimit?: string; transient?: string };
  /** Prefix prepended to every exposed model name (default: none). */
  modelPrefix?: string;
  /** Provider template this entry was created from (provenance only). */
  template?: string;
  /** Subscription usage mapping shown as quota cards. */
  quota?: ProviderQuotaConfigUI;
  apiKeys: ProviderKeyEntry[];
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
  /** Built-in quota preset ids (providers.<id>.quota.preset values). */
  quotaPresets?: string[];
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
  const type: ProviderType = normalizeProviderType(record.type);
  // A multi-adapter provider carries no top-level endpoint; it is only
  // invalid when neither spelling supplies one.
  const hasAdapters = Array.isArray(record.adapters) && record.adapters.length > 0;
  if (endpoint.trim() === "" && !hasAdapters) return null;
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
  // Shared parser: aliases/proxy handling lives in one place for the flat
  // shorthand and the adapter model lists.
  const models = normalizeModelEntries(record.models);
  const adapters = normalizeAdapters(record, type, endpoint, models);
  return {
    // The primary adapter also fills the flat shorthand fields so existing
    // consumers (list rows, single-adapter display) keep working.
    type: adapters[0]?.type ?? type,
    endpoint: adapters[0]?.endpoint ?? endpoint,
    models: adapters[0]?.models ?? models,
    adapters,
    proxy: typeof record.proxy === "string" && record.proxy !== "" ? record.proxy : undefined,
    priority: typeof record.priority === "number" ? record.priority : undefined,
    weight: typeof record.weight === "number" ? record.weight : undefined,
    sticky: record.sticky === false ? false : true,
    exposeUpstreamName: record.exposeUpstreamName === false ? false : true,
    bareNamePolicy: record.bareNamePolicy === "provider-first" ? "provider-first" : "local-first",
    maxRetries: typeof record.maxRetries === "number" ? record.maxRetries : undefined,
    disabled: record.disabled === true,
    sessionHeaders: Array.isArray(record.sessionHeaders)
      ? (record.sessionHeaders.filter(
          (item): item is string => typeof item === "string" && item.trim() !== "",
        ).map((item) => item.trim()))
      : undefined,
    headers: normalizeHeaderMap(record.headers),
    cooldown: normalizeCooldown(record.cooldown),
    modelPrefix:
      typeof record.modelPrefix === "string" && record.modelPrefix !== "" ? record.modelPrefix : undefined,
    template: typeof record.template === "string" && record.template !== "" ? record.template : undefined,
    quota: normalizeQuota(record.quota),
    apiKeys,
  };
}

/** Builds the adapter list: the configured one, or the flat shorthand. */
export function normalizeAdapters(
  record: Record<string, unknown>,
  fallbackType: ProviderType,
  fallbackEndpoint: string,
  fallbackModels: ProviderModelEntry[],
): ProviderAdapterUI[] {
  const raw = Array.isArray(record.adapters) ? record.adapters : [];
  const adapters: ProviderAdapterUI[] = [];
  for (const item of raw) {
    if (!item || typeof item !== "object") continue;
    const entry = item as Record<string, unknown>;
    const endpoint = typeof entry.endpoint === "string" ? entry.endpoint : "";
    if (endpoint.trim() === "") continue;
    const adapter: ProviderAdapterUI = {
      type: normalizeProviderType(entry.type),
      endpoint,
      models: normalizeModelEntries(entry.models),
    };
    if (typeof entry.name === "string" && entry.name.trim() !== "") adapter.name = entry.name.trim();
    if (typeof entry.proxy === "string" && entry.proxy.trim() !== "") adapter.proxy = entry.proxy.trim();
    const pathMap = normalizeHeaderMap(entry.pathMap);
    if (pathMap) adapter.pathMap = pathMap;
    const modelMatch = Array.isArray(entry.modelMatch)
      ? entry.modelMatch.filter((pattern): pattern is string => typeof pattern === "string" && pattern.trim() !== "")
      : [];
    if (modelMatch.length > 0) adapter.modelMatch = modelMatch;
    if (entry.default === true) adapter.default = true;
    const keys = normalizeKeyEntries(entry.apiKeys);
    if (keys.length > 0) adapter.apiKeys = keys;
    adapters.push(adapter);
  }
  if (adapters.length > 0) return adapters;
  return [{ type: fallbackType, endpoint: fallbackEndpoint, models: fallbackModels }];
}

/** Matches a model ID against a glob pattern (* and ? only). */
export function matchesModelPattern(pattern: string, model: string): boolean {
  const escaped = pattern.replace(/[.+^${}()|[\]\\]/g, "\\$&").replace(/\*/g, ".*").replace(/\?/g, ".");
  try {
    return new RegExp(`^${escaped}$`).test(model);
  } catch {
    return false;
  }
}

/**
 * Distributes upstream model IDs across the adapters: models already served by
 * any adapter stay where they are, new ones go to the first adapter whose
 * modelMatch patterns accept them, and the rest to the default adapter (or the
 * first one when none is marked).
 */
export function distributeModels(
  adapters: ProviderAdapterUI[],
  models: string[],
): { adapters: ProviderAdapterUI[]; added: number; skipped: number } {
  const served = new Set<string>();
  for (const adapter of adapters) {
    for (const model of adapter.models) {
      served.add(model.local);
      for (const alias of model.aliases ?? []) served.add(alias);
    }
  }
  const next = adapters.map((adapter) => ({ ...adapter, models: [...adapter.models] }));
  const fallbackIndex = Math.max(
    0,
    next.findIndex((adapter) => adapter.default === true),
  );
  let added = 0;
  let skipped = 0;
  for (const model of models) {
    if (served.has(model)) {
      skipped += 1;
      continue;
    }
    const matchIndex = next.findIndex((adapter) => (adapter.modelMatch ?? []).some((pattern) => matchesModelPattern(pattern, model)));
    const target = next[matchIndex >= 0 ? matchIndex : fallbackIndex];
    if (!target) break;
    target.models.push({ local: model, remote: model });
    served.add(model);
    added += 1;
  }
  for (const adapter of next) {
    adapter.models.sort((a, b) => a.local.localeCompare(b.local));
  }
  return { adapters: next, added, skipped };
}

/** Normalizes a raw model list (shared by provider and adapter parsing). */
export function normalizeModelList(value: unknown): ProviderModelEntry[] {
  return normalizeModelEntries(value);
}

function normalizeModelEntries(value: unknown): ProviderModelEntry[] {
  if (!Array.isArray(value)) return [];
  return value
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
      const proxy = typeof entry.proxy === "string" && entry.proxy.trim() !== "" ? entry.proxy.trim() : undefined;
      const aliases = Array.isArray(entry.aliases)
        ? entry.aliases
            .filter((alias): alias is string => typeof alias === "string" && alias.trim() !== "")
            .map((alias) => alias.trim())
            .filter((alias) => alias !== entry.local)
        : [];
      const out: ProviderModelEntry = { local: entry.local, remote };
      if (aliases.length > 0) out.aliases = aliases;
      if (proxy) out.proxy = proxy;
      const capabilities = normalizeModelCapabilities(entry.capabilities);
      if (capabilities) out.capabilities = capabilities;
      return out;
    })
    .filter((item): item is ProviderModelEntry => item !== null);
}

/** Normalizes a capability block; undefined when nothing is declared. */
function normalizeModelCapabilities(value: unknown): ProviderModelCapabilities | undefined {
  if (!value || typeof value !== "object") return undefined;
  const entry = value as Record<string, unknown>;
  const list = (raw: unknown): string[] | undefined => {
    if (!Array.isArray(raw)) return undefined;
    const items = raw.filter((item): item is string => typeof item === "string" && item.trim() !== "").map((item) => item.trim());
    return items.length > 0 ? items : undefined;
  };
  const out: ProviderModelCapabilities = {};
  const inList = list(entry.in);
  const outList = list(entry.out);
  if (inList) out.in = inList;
  if (outList) out.out = outList;
  if (entry.translation === true) out.translation = true;
  return Object.keys(out).length > 0 ? out : undefined;
}

function normalizeKeyEntries(value: unknown): ProviderKeyEntry[] {
  if (!Array.isArray(value)) return [];
  return value
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
    .filter((item): item is ProviderKeyEntry => item !== null);
}

function normalizeCooldown(
  value: unknown,
): ProviderConfigUI["cooldown"] {
  if (typeof value === "string" && value !== "" && value !== "inherit") return value as ProviderCooldownPolicy;
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const record = value as Record<string, unknown>;
  const out: Exclude<ProviderConfigUI["cooldown"], string | undefined> = {};
  if (typeof record.policy === "string" && record.policy !== "" && record.policy !== "inherit") {
    out.policy = record.policy as ProviderCooldownPolicy;
  }
  for (const key of ["auth", "authProbe", "rateLimit", "transient"] as const) {
    if (typeof record[key] === "string" && record[key] !== "") out[key] = record[key] as string;
  }
  return Object.keys(out).length > 0 ? out : undefined;
}

function normalizeQuota(value: unknown): ProviderQuotaConfigUI | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const record = value as Record<string, unknown>;
  const quota: ProviderQuotaConfigUI = {};
  if (typeof record.preset === "string" && record.preset !== "") quota.preset = record.preset;
  if (typeof record.endpoint === "string" && record.endpoint !== "") quota.endpoint = record.endpoint;
  if (typeof record.refreshInterval === "string" && record.refreshInterval !== "") {
    quota.refreshInterval = record.refreshInterval;
  }
  if (record.enforce && typeof record.enforce === "object") {
    const enforce = record.enforce as Record<string, unknown>;
    const out: NonNullable<ProviderQuotaConfigUI["enforce"]> = {};
    if (enforce.enabled === true) out.enabled = true;
    if (typeof enforce.threshold === "number") out.threshold = enforce.threshold;
    if (typeof enforce.fallbackCooldown === "string" && enforce.fallbackCooldown !== "") {
      out.fallbackCooldown = enforce.fallbackCooldown;
    }
    if (Object.keys(out).length > 0) quota.enforce = out;
  }
  return Object.keys(quota).length > 0 ? quota : undefined;
}

function normalizeHeaderMap(value: unknown): Record<string, string> | undefined {
  if (!value || typeof value !== "object" || Array.isArray(value)) return undefined;
  const out: Record<string, string> = {};
  for (const [key, entry] of Object.entries(value as Record<string, unknown>)) {
    if (typeof entry === "string" && key.trim() !== "") out[key.trim()] = entry;
  }
  return Object.keys(out).length > 0 ? out : undefined;
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

/**
 * UI state accepted by the serializer: the flat shorthand alone is enough for
 * a single-adapter provider, so adapters may be omitted by callers that build
 * provider objects by hand.
 */
export type ProviderConfigInput = Omit<ProviderConfigUI, "adapters"> & {
  adapters?: ProviderAdapterUI[];
};

/** Builds a providers: section suitable for a config PATCH from UI state. */
export function providersSectionToConfig(
  providers: Record<string, ProviderConfigInput>,
): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [id, p] of Object.entries(providers)) {
    const adapters =
      p.adapters && p.adapters.length > 0
        ? p.adapters
        : [{ type: p.type, endpoint: p.endpoint, models: p.models }];
    const keys = p.apiKeys.map(serializeKey);
    // A single plain adapter keeps the flat shorthand so existing configs are
    // not rewritten on every save; anything adapter-only (extra adapters,
    // pathMap, modelMatch, per-adapter keys) uses the adapters form.
    const single = adapters.length === 1 && !hasAdapterExtras(adapters[0]);
    out[id] = {
      ...(single
        ? {
            type: adapters[0].type,
            endpoint: adapters[0].endpoint,
            models: adapters[0].models.map(serializeModel),
          }
        : { adapters: adapters.map(serializeAdapter) }),
      ...(p.proxy ? { proxy: p.proxy } : {}),
      ...(p.priority !== undefined ? { priority: p.priority } : {}),
      ...(p.weight !== undefined ? { weight: p.weight } : {}),
      ...(p.sticky === false ? { sticky: false } : {}),
      ...(p.exposeUpstreamName === false ? { exposeUpstreamName: false } : {}),
      ...(p.bareNamePolicy === "provider-first" ? { bareNamePolicy: "provider-first" } : {}),
      ...(p.maxRetries !== undefined ? { maxRetries: p.maxRetries } : {}),
      ...(p.disabled ? { disabled: true } : {}),
      ...(p.sessionHeaders && p.sessionHeaders.length > 0 ? { sessionHeaders: p.sessionHeaders } : {}),
      ...(p.headers && Object.keys(p.headers).length > 0 ? { headers: p.headers } : {}),
      ...(p.cooldown && !(typeof p.cooldown === "string" && p.cooldown === "inherit")
        ? { cooldown: p.cooldown }
        : {}),
      ...(p.modelPrefix ? { modelPrefix: p.modelPrefix } : {}),
      ...(p.template ? { template: p.template } : {}),
      ...(p.quota && Object.keys(p.quota).length > 0 ? { quota: p.quota } : {}),
      apiKeys: keys,
    };
  }
  return out;
}

function hasAdapterExtras(adapter: ProviderAdapterUI): boolean {
  return Boolean(
    adapter.proxy ||
      adapter.pathMap ||
      adapter.modelMatch?.length ||
      adapter.default ||
      (adapter.apiKeys && adapter.apiKeys.length > 0),
  );
}

function serializeModel(model: ProviderModelEntry): unknown {
  // The scalar shorthand cannot carry a proxy, aliases or capabilities: keep
  // the object form then.
  const capabilities = model.capabilities;
  if (!model.proxy && !model.aliases?.length && !capabilities && model.local === model.remote) return model.local;
  return {
    local: model.local,
    remote: model.remote,
    ...(model.aliases?.length ? { aliases: model.aliases } : {}),
    ...(model.proxy ? { proxy: model.proxy } : {}),
    ...(capabilities ? { capabilities } : {}),
  };
}

function serializeKey(key: ProviderKeyEntry): Record<string, unknown> {
  return {
    ...(key.value !== undefined ? { value: key.value } : {}),
    ...(key.valueEnv ? { valueEnv: key.valueEnv } : {}),
    ...(key.weight !== undefined ? { weight: key.weight } : {}),
    ...(key.priority !== undefined ? { priority: key.priority } : {}),
    ...(key.maxConcurrent !== undefined ? { maxConcurrent: key.maxConcurrent } : {}),
    ...(key.proxy ? { proxy: key.proxy } : {}),
    ...(key.disabled ? { disabled: true } : {}),
  };
}

function serializeAdapter(adapter: ProviderAdapterUI): Record<string, unknown> {
  return {
    ...(adapter.name ? { name: adapter.name } : {}),
    type: adapter.type,
    endpoint: adapter.endpoint,
    ...(adapter.proxy ? { proxy: adapter.proxy } : {}),
    ...(adapter.pathMap ? { pathMap: adapter.pathMap } : {}),
    ...(adapter.modelMatch?.length ? { modelMatch: adapter.modelMatch } : {}),
    ...(adapter.default ? { default: true } : {}),
    ...(adapter.apiKeys?.length ? { apiKeys: adapter.apiKeys.map(serializeKey) } : {}),
    models: adapter.models.map(serializeModel),
  };
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

/** One built-in provider template offered by the server. */
export interface ProviderTemplateSummary {
  id: string;
  name: string;
  description: string;
  adapters: Array<{ name?: string; type: ProviderType }>;
  /** The model list endpoint needs a key before models can be pulled. */
  requiresKey: boolean;
  quotaPreset?: string;
  sessionHeaders?: string[];
}

export interface ProviderTemplateApplyResult {
  /** Provider entry draft, already populated with the pulled models. */
  provider: Record<string, unknown>;
  pulled: number;
  assigned: Record<string, number>;
  warnings?: string[];
}

/** Lists the built-in provider templates. */
export async function listProviderTemplates(): Promise<ProviderTemplateSummary[]> {
  const payload = await requestJson<{ templates?: ProviderTemplateSummary[] }>("/api/providers/templates");
  return Array.isArray(payload?.templates) ? payload.templates : [];
}

/**
 * Applies a template: the server pulls the live model list and distributes it
 * across the template's adapters. With providerID set, the stored credential
 * of that provider authenticates the pull (re-pull flow).
 */
export async function applyProviderTemplate(
  id: string,
  options: { apiKey?: string; providerID?: string } = {},
): Promise<ProviderTemplateApplyResult> {
  return requestJson<ProviderTemplateApplyResult>(
    `/api/providers/templates/${encodeURIComponent(id)}/apply`,
    {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        ...(options.apiKey ? { apiKey: options.apiKey } : {}),
        ...(options.providerID ? { providerID: options.providerID } : {}),
      }),
    },
  );
}

/** Cached subscription quota snapshots (no upstream calls). */
export async function fetchProviderQuotas(): Promise<CredentialQuota[]> {
  const payload = await requestJson<{ credentials?: CredentialQuota[] }>("/api/providers/quotas");
  return Array.isArray(payload?.credentials) ? payload.credentials : [];
}

/** Refreshes quota for one credential or every credential of a provider. */
export async function refreshProviderQuotas(providerID: string, keyID?: string): Promise<CredentialQuota[]> {
  const payload = await requestJson<{ credentials?: CredentialQuota[] }>("/api/providers/quotas/refresh", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ providerID, ...(keyID ? { keyID } : {}) }),
  });
  return Array.isArray(payload?.credentials) ? payload.credentials : [];
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
    throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
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
    throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
  }
  return payload as ConfigSnapshot;
}

/** 某个供应商以哪个适配器（API 表面）提供该模型。 */
export interface ModelProviderSurface {
  providerID: string;
  /** 适配器名（该供应商只有一个隐式适配器时为空）。 */
  adapter: string;
  /** 命中该模型的客户端名（按请求名匹配 local 或 alias）。 */
  model: string;
}

/**
 * 找出所有提供指定模型的供应商及其适配器。
 *
 * 这是模型详情页要回答的问题：一个模型名可能同时挂在多个供应商上，
 * 一个供应商也可能在多个 API 表面上暴露它，所以"谁提供这个模型"是一个
 * 列表而不是单个值。匹配口径与后端一致——先看 local 名，再看 aliases，
 * 让按别名或带前缀的名字打开详情页也能列出正确的供应商。
 */
export function providersServingModel(
  providers: Record<string, ProviderConfigUI>,
  modelId: string,
): ModelProviderSurface[] {
  const wanted = modelId.trim();
  if (wanted === "") return [];
  const found: ModelProviderSurface[] = [];
  for (const providerID of Object.keys(providers).sort()) {
    const provider = providers[providerID];
    if (provider?.disabled) continue;
    const adapters = provider.adapters ?? [];
    for (const adapter of adapters) {
      for (const entry of adapter.models ?? []) {
        const names = [entry.local, ...(entry.aliases ?? [])];
        if (!names.includes(wanted)) continue;
        found.push({ providerID, adapter: adapter.name ?? "", model: entry.local });
      }
    }
  }
  return found;
}

/** JSON Pointer 转义：~ 与 / 是路径分隔语义，出现在 ID 里必须转义。 */
function pointerToken(value: string): string {
  return value.replaceAll("~", "~0").replaceAll("/", "~1");
}

/**
 * 只切换一个供应商的 enabled/disabled，不动它的任何其它字段。
 *
 * 这是卡片上快捷开关的关键：不能走"整段替换 /providers"那条路。整段回写会把
 * 每个密钥都重新提交一遍，而脱敏值（[REDACTED]）是按位置还原的——密钥列表的
 * 长度或顺序一旦不同，就会把别人的密钥填进这个位置。只发一个 disabled 成员的
 * 最小 patch 让这个问题根本不存在：请求体里没有任何密钥。
 *
 * 启用时优先 remove 而不是写入 false：字段是 omitempty，写死 false 会在 YAML 里
 * 留下 `disabled: false` 这种无意义残留（后端 remove 对不存在的成员会报错，
 * 所以用 keyPresent 指明当前是否真的有这个键，避免无效 patch）。
 */
export async function setProviderDisabled(
  etag: string,
  providerID: string,
  disabled: boolean,
  keyPresent: boolean,
): Promise<ConfigSnapshot> {
  const path = `/providers/${pointerToken(providerID)}/disabled`;
  const ops: Array<Record<string, unknown>> = disabled
    ? [{ op: "add", path, value: true }]
    : keyPresent
      ? [{ op: "remove", path }]
      : [];
  if (ops.length === 0) {
    // 已经处于启用状态且配置里本就没有该字段：无需请求，也就不会改动 etag。
    return await fetchConfigSnapshot();
  }
  const response = await fetch("/api/config", {
    method: "PATCH",
    credentials: "same-origin",
    headers: { "Content-Type": "application/json", "If-Match": etag },
    body: JSON.stringify(ops),
  });
  const payload = await response.json().catch(() => ({}));
  if (response.status === 412) throw new Error("config conflict: reload and retry");
  if (!response.ok) {
    throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
  }
  return payload as ConfigSnapshot;
}

/**
 * Prepare manually entered provider models for saving.
 *
 * A row that names only the upstream model takes that name as its
 * client-facing one — the same default the pull flow and the server apply — and
 * a row that names nothing at all is dropped. Without this an untouched row (or
 * one where only the upstream name was typed) reaches the server as an empty
 * local name and the whole save is rejected.
 */
export function normalizeProviderModels<T extends { local: string; remote: string }>(models: T[]): T[] {
  return models
    .map((model) => {
      const local = model.local.trim();
      const remote = model.remote.trim();
      return { ...model, local: local || remote, remote };
    })
    .filter((model) => model.local !== "");
}
