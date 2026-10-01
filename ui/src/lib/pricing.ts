export interface PricingCatalogResponse {
  providers: string[];
  models: string[];
}

export interface PricingPrice {
  provider: string;
  model: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  reasoning: number;
  syncedAt?: string;
}

export interface PricingPricesResponse {
  data: PricingPrice[];
  page: number;
  limit: number;
  total: number;
  total_pages: number;
  etag?: string;
  syncedAt?: string;
  source?: string;
}

export interface PricingPriceInput {
  provider: string;
  model: string;
  input: number;
  output: number;
  cacheRead: number;
  cacheWrite: number;
  reasoning: number;
}

export interface PricingSyncResponse {
  changed: boolean;
  etag?: string;
  count: number;
  syncedAt: string;
}

export async function getPricingCatalog(options: { provider?: string; query?: string; limit?: number } = {}): Promise<PricingCatalogResponse> {
  const params = new URLSearchParams();
  if (options.provider?.trim()) params.set("provider", options.provider.trim());
  if (options.query?.trim()) params.set("q", options.query.trim());
  params.set("limit", String(options.limit ?? 200));
  const response = await fetch(`/api/pricing/catalog?${params.toString()}`);
  if (!response.ok) {
    const payload = await response.json().catch(() => ({})) as { error?: string };
    throw new Error(payload.error ?? `Unable to load pricing catalog (HTTP ${response.status})`);
  }
  const payload = await response.json() as Partial<PricingCatalogResponse>;
  return {
    providers: Array.isArray(payload.providers) ? payload.providers.filter((item): item is string => typeof item === "string") : [],
    models: Array.isArray(payload.models) ? payload.models.filter((item): item is string => typeof item === "string") : [],
  };
}

async function readPricingError(response: Response, fallback: string): Promise<Error> {
  const payload = await response.json().catch(() => ({})) as { error?: string };
  return new Error(payload.error ?? `${fallback} (HTTP ${response.status})`);
}

export async function getPricingPrices(options: { provider?: string; query?: string; page?: number; limit?: number } = {}): Promise<PricingPricesResponse> {
  const params = new URLSearchParams();
  if (options.provider?.trim()) params.set("provider", options.provider.trim());
  if (options.query?.trim()) params.set("q", options.query.trim());
  params.set("page", String(options.page ?? 1));
  params.set("limit", String(options.limit ?? 50));
  const response = await fetch(`/api/pricing/prices?${params.toString()}`);
  if (!response.ok) throw await readPricingError(response, "Unable to load pricing rows");
  const payload = await response.json() as Partial<PricingPricesResponse>;
  return {
    data: Array.isArray(payload.data) ? payload.data.filter((item): item is PricingPrice => isPricingPrice(item)) : [],
    page: typeof payload.page === "number" ? payload.page : 1,
    limit: typeof payload.limit === "number" ? payload.limit : 50,
    total: typeof payload.total === "number" ? payload.total : 0,
    total_pages: typeof payload.total_pages === "number" ? payload.total_pages : 0,
    etag: typeof payload.etag === "string" ? payload.etag : undefined,
    syncedAt: typeof payload.syncedAt === "string" ? payload.syncedAt : undefined,
    source: typeof payload.source === "string" ? payload.source : undefined,
  };
}

export async function savePricingPrice(price: PricingPriceInput): Promise<PricingPrice> {
  const response = await fetch("/api/pricing/prices", {
    method: "PUT",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(price),
  });
  if (!response.ok) throw await readPricingError(response, "Unable to save pricing row");
  return await response.json() as PricingPrice;
}

export async function deletePricingPrice(provider: string, model: string): Promise<void> {
  const params = new URLSearchParams({ provider, model });
  const response = await fetch(`/api/pricing/prices?${params.toString()}`, { method: "DELETE" });
  if (!response.ok) throw await readPricingError(response, "Unable to delete pricing row");
}

export async function syncPricing(): Promise<PricingSyncResponse> {
  const response = await fetch("/api/pricing/sync", { method: "POST" });
  if (!response.ok) throw await readPricingError(response, "Unable to synchronize pricing");
  return await response.json() as PricingSyncResponse;
}

function isPricingPrice(value: unknown): value is PricingPrice {
  if (!value || typeof value !== "object") return false;
  const item = value as Partial<PricingPrice>;
  return typeof item.provider === "string"
    && typeof item.model === "string"
    && typeof item.input === "number"
    && typeof item.output === "number"
    && typeof item.cacheRead === "number"
    && typeof item.cacheWrite === "number"
    && typeof item.reasoning === "number";
}
