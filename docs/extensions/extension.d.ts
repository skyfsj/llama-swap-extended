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
  http: {
    /** One allowlisted HTTP request. `timeoutMs` (1000-60000) overrides the
        default 8s client timeout; the hook budget still caps the whole call.
        Headers are lowercased with repeated values joined by ", ". */
    fetch(url: string, options?: {
      method?: string;
      headers?: Record<string, string>;
      body?: string;
      timeoutMs?: number;
    }): Promise<{
      status: number;
      ok: boolean;
      /** URL after redirects. */
      url: string;
      headers: Record<string, string>;
      text(): string;
      json(): unknown;
    }>;
  };
  /** Image processing, run by the daemon rather than in the sandbox: a byte
      loop in the worker VM manages about six million array element touches a
      second, while image/png and image/jpeg analyse a 1.4 MP image in about
      115 ms, and they read formats a script should not attempt (progressive and
      CMYK JPEG, interlaced PNG, every bit depth). Images are base64 text because
      the worker protocol is JSON lines and cannot carry raw bytes. */
  images: {
    /** One image operation. Resolves to the metadata, plus `image` (base64),
        `mediaType` and `bytes` for info/crop/resize, or `analysis` for analyze.
        Ceilings the daemon enforces: 32 MiB input, 25M pixels in and out, 5 MiB
        encoded result. A tool call reaches less than the input ceiling: the whole
        call is one 8 MiB worker line shared with the bundle, so an image argument
        tops out near 5.9 MiB of decoded data. */
    apply(request: {
      operation: "info" | "analyze" | "crop" | "resize";
      /** A bare base64 payload or a data URL. */
      image: string;
      /** Apply the JPEG's EXIF orientation first. Default true. */
      autoOrient?: boolean;
      crop?: { x: number; y: number; width: number; height: number };
      resize?: { width?: number; height?: number; filter?: "auto" | "box" | "bilinear" | "nearest" };
      /** Clockwise degrees: 0, 90, 180 or 270. */
      rotate?: number;
      flip?: "none" | "horizontal" | "vertical";
      /** Colour count for analyze. */
      colors?: number;
      /** Pixel sample for analyze. */
      samples?: number;
      format?: "png" | "jpeg";
      /** JPEG quality, 1-100. */
      quality?: number;
      /** Optional tightening of the daemon's own ceilings. */
      maxPixels?: number;
      maxOutputPixels?: number;
      maxInputBytes?: number;
    }): Promise<Record<string, unknown>>;
  };
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
  /**
   * "client" hands the call to the API caller; "server" runs it here through
   * onToolCall. On Anthropic Messages and Responses a server call is reported
   * back to the caller as a server_tool_use/mcp_tool_result pair or an mcp_call
   * item and replayed with the conversation; on Chat Completions and Gemini it
   * stays hidden and runs when the caller returns with the conversation.
   */
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

/** Context passed to a bound tool handler, identical to the hook context. */
export type BoundHandlerContext = ExtensionContext;

export interface ToolSpec {
  /** Tool name the model calls; must be unique within the extension. */
  name: string;
  description?: string;
  parameters?: Record<string, unknown>;
  strict?: boolean;
  /** "server" (the default for bound handlers) or "client". */
  execution?: "client" | "server";
  /** Bound handler: parsed arguments plus the request context. */
  handler(args: Record<string, unknown>, ctx: ExtensionContext): unknown;
}

export interface DefinedTool extends ToolSpec {
  readonly __llamaSwapTool: true;
}

/**
 * Declares a tool whose handler the runtime dispatches by name. Equivalent to
 * the legacy `{type, function, execution: "server"}` entry plus an
 * `onToolCall` branch, without the manual name matching.
 */
export function defineTool(spec: ToolSpec): DefinedTool;

/** Marks the default export; kept for symmetry with the runtime contract. */
export function defineExtension<T extends Record<string, any>>(definition: T): T;

/**
 * A registry that refuses duplicate tool names. Useful for extensions that
 * install several tools: a duplicate is caught when the extension loads rather
 * than being silently skipped at call time.
 */
export interface ToolRegistry {
  add(tool: DefinedTool): DefinedTool;
  tools(): DefinedTool[];
  names(): string[];
}
export function createToolRegistry(): ToolRegistry;

/** Collapses a value into a single trimmed line. */
export function collapseText(value: unknown): string;
/** Parses an integer and clamps it into [min, max]. */
export function clampCount(value: unknown, fallback: number, min: number, max: number): number;

/** Where a credential is placed in a request. */
export interface SearchCredential {
  setting: string;
  label?: string;
  header?: string;
  bearer?: boolean;
  body?: string;
  query?: string;
}

export interface HttpRequestPlan {
  url: string;
  method?: string;
  headers?: Record<string, string>;
  body?: unknown;
}

export interface SearchResultItem {
  title?: string;
  url?: string;
  snippet?: string;
  content?: string;
  /** Rendered as a separate line each, before the summary fields. */
  highlights?: unknown;
  summary?: string;
  date?: string;
  source?: string;
  score?: unknown;
  meta?: string[];
  /** Fully custom body lines; takes precedence over the summary fields. */
  lines?: string[];
}

/**
 * Declares a web search tool: the SDK validates the query and credential, sends
 * the request, shapes HTTP failures into a readable message, renders the
 * numbered result list and truncates it. A provider only supplies request() and
 * parse().
 */
export function searchTool(spec: {
  name: string;
  title: string;
  description: string;
  parameters?: Record<string, unknown>;
  maxQueryLength?: number;
  queryParameter?: string;
  credential: SearchCredential;
  request(args: { query: string; config: Record<string, any>; arguments: Record<string, any> }): HttpRequestPlan;
  parse(payload: any): SearchResultItem[];
  preface?(payload: any): string | string[] | undefined;
  emptyMessage?(query: string): string;
  maxChars?: number;
  statusAdvice?: Record<number, string>;
}): DefinedTool;

/** Declares a server-side fetch tool: a URL becomes a title plus Markdown. */
export function fetchTool(spec: {
  name: string;
  title: string;
  description: string;
  credential: SearchCredential;
  request(args: { url: string; config: Record<string, any>; arguments: Record<string, any> }): HttpRequestPlan;
  parse(payload: any): { title?: string; content?: string };
  maxUrlLength?: number;
  maxChars?: number;
  statusAdvice?: Record<number, string>;
}): DefinedTool;

/** Shapes an HTTP failure into one readable line, with retry advice. */
export function apiErrorMessage(
  status: number,
  body: string,
  options?: { provider?: string; advice?: Record<number, string> },
): string;
