export interface SessionSnapshot {
  /** Caller session id (X-Session-ID or configured header); empty when absent. */
  id: string;
  /** Authenticated key id; "anonymous" on keyless deployments. */
  keyId: string;
  /** True when the deployment runs without API keys. */
  anonymous: boolean;
}

export type KVScope = "global" | "session" | "key";

export interface KVOptions {
  /** "global" (default) is per-extension, "session" per caller session, "key" per API key. */
  scope?: KVScope;
  /** Entry lifetime in seconds; ephemeral entries default to a sliding TTL. */
  ttlSeconds?: number;
}

export interface ExtensionContext {
  requestId: string;
  requestedModel: string;
  resolvedModel: string;
  profile: string;
  provider: string;
  endpoint: "chat.completions" | "responses" | "anthropic.messages";
  stream: boolean;
  /** Caller's UI locale, e.g. "zh-CN". */
  locale: string;
  extension: string;
  config: Record<string, unknown>;
  abortSignal: { aborted: boolean };
  log: { debug(message: string): void; info(message: string): void; warn(message: string): void; error(message: string): void };
  http: { fetch(url: string, options?: { method?: string; headers?: Record<string, string>; body?: string }): Promise<{ status: number; ok: boolean; text(): string; json(): unknown }> };
  files: { read(path: string): Promise<string>; write(path: string, content: string): Promise<boolean> };
  /** Key/value store. Ephemeral by default; "persistent" storage permission
      makes set() durable across restarts. Rejected in test mode. */
  kv: {
    get(key: string, options?: KVOptions): Promise<unknown | undefined>;
    set(key: string, value: unknown, options?: KVOptions): Promise<boolean>;
    delete(key: string, options?: KVOptions): Promise<boolean>;
    keys(prefix?: string, options?: Omit<KVOptions, "ttlSeconds">): Promise<string[]>;
  };
  /** Models the current caller may reach. */
  models: { id: string; name?: string }[];
  /** Caller identity snapshot, absent in single-request test mode. */
  session?: SessionSnapshot;
  /** One non-streaming model call on behalf of the caller. Extensions do not
      run inside the forwarded request; nested forwards are depth-limited
      (extensions.maxForwardDepth, default 2). */
  forward(model: string, request: { messages: unknown[] } & Record<string, unknown>): Promise<{ status: number; body: unknown }>;
  /** Caller's aggregate requests and tokens over the last 24h. */
  usage(): Promise<{ requests: number; inputTokens: number; outputTokens: number; window: string }>;
}

export interface FunctionTool {
  type: "function";
  function: { name: string; description?: string; parameters?: Record<string, unknown>; strict?: boolean };
  execution?: "client" | "server";
}

export interface ToolCall {
  id: string;
  name: string;
  arguments: Record<string, unknown>;
}

/** One declared setting. The UI renders a form from these. */
export interface SettingSpec {
  type: "string" | "number" | "boolean" | "select" | "password" | "textarea" | "list" | "map" | "json";
  label?: string;
  hint?: string;
  section?: string;
  default?: unknown;
  options?: string[];
  required?: boolean;
  secret?: boolean;
  min?: number;
  max?: number;
}

export interface Extension {
  tools?: FunctionTool[];
  /** Declared settings, rendered as a form in the Extensions UI. */
  settings?: Record<string, SettingSpec>;
  onRequest?(ctx: ExtensionContext, request: Record<string, unknown>): Record<string, unknown> | Promise<Record<string, unknown>>;
  onBeforeForward?(ctx: ExtensionContext, request: Record<string, unknown>): Record<string, unknown> | Promise<Record<string, unknown>>;
  onToolCall?(ctx: ExtensionContext, call: ToolCall): { content?: unknown; handled?: boolean } | Promise<{ content?: unknown; handled?: boolean }>;
  onToolResult?(ctx: ExtensionContext, result: Record<string, unknown>): Record<string, unknown> | Promise<Record<string, unknown>>;
  onResponse?(ctx: ExtensionContext, response: Record<string, unknown>): Record<string, unknown> | Promise<Record<string, unknown>>;
  onStreamEvent?(ctx: ExtensionContext, event: Record<string, unknown>): Record<string, unknown> | Promise<Record<string, unknown>>;
  onError?(ctx: ExtensionContext, error: { hook: string; error: string }): void | Promise<void>;
}
