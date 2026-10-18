/**
 * @llama-swap/extension —— 扩展开发 SDK。
 *
 * 在扩展入口文件里这样引入：
 *
 * ```js
 * import { defineExtension, defineTool, createToolRegistry, searchTool } from "@llama-swap/extension";
 *
 * export default defineExtension({
 *   tools: [
 *     defineTool({
 *       name: "echo",
 *       description: "原样返回输入文本",
 *       parameters: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
 *       async handler(args, ctx) {
 *         return { content: String(args.text ?? "") };
 *       },
 *     }),
 *   ],
 *   async onRequest(ctx, request) { return request; },
 * });
 * ```
 *
 * 设计取向是「注册式处理器」：每个工具在声明时就把处理器登记到名字上，
 * 运行时按名字直接派发——不需要在 onToolCall 里手写 `if (call.name === ...)`。
 * 旧的 `tools` 数组 + onToolCall 写法继续可用，显式 onToolCall 优先级更高。
 *
 * 工具名要求全局唯一：同一个扩展内重名会在编译期报错；跨扩展重名（例如两个
 * 搜索器都叫 web_search）会被工具冲突策略静默跳过，导致"装了却没生效"。
 * 因此搜索类预设一律使用带提供方后缀的名字，如 web_search_exa，
 * 这样多个搜索器可以同时启用。
 */

/** 传给工具处理器的上下文，与 hook 上下文完全一致。 */
export type ExtensionContext = Record<string, any>;

/** 单个工具的声明。 */
export interface ToolSpec {
  /** 模型调用的工具名；必须在本扩展内唯一。 */
  name: string;
  /** 展示给模型的说明。 */
  description?: string;
  /** 参数的 JSON Schema。 */
  parameters?: Record<string, unknown>;
  strict?: boolean;
  /** "server"（绑定处理器的默认值）或 "client"。 */
  execution?: "client" | "server";
  /**
   * 绑定的处理器。入参是解析后的工具参数与请求上下文；
   * 返回 `{ content }`（工具结果），或返回普通值，会被包成 `{ content: value }`。
   */
  handler(args: Record<string, unknown>, ctx: ExtensionContext): unknown;
}

/** 已登记的工具：带标记位，运行时据此识别 SDK 声明。 */
export interface DefinedTool extends ToolSpec {
  readonly __llamaSwapTool: true;
}

export interface ExtensionDefinition {
  tools?: (DefinedTool | Record<string, any>)[];
  settings?: Record<string, any>;
  [hook: string]: any;
}

const TOOL_MARKER = "__llamaSwapTool";

/**
 * defineTool 声明一个带处理器的工具：处理器只存在于运行时，
 * 模型看到的是去掉处理器之后的函数声明，模型调用时由运行时直接执行。
 */
export function defineTool(spec: ToolSpec): DefinedTool {
  if (!spec || typeof spec !== "object") {
    throw new Error("defineTool 需要一个对象参数");
  }
  if (typeof spec.name !== "string" || spec.name.trim() === "") {
    throw new Error("defineTool 缺少工具名 name");
  }
  if (typeof spec.handler !== "function") {
    throw new Error(`defineTool(${spec.name}) 缺少 handler 函数`);
  }
  const tool: DefinedTool = {
    ...spec,
    execution: spec.execution ?? "server",
    [TOOL_MARKER]: true,
  } as DefinedTool;
  return Object.freeze(tool);
}

/**
 * defineExtension 标记默认导出，运行时据此做 SDK 专属归一化
 * （剥离工具处理器、把声明式写法转成线协议的工具定义）。
 */
export function defineExtension<T extends ExtensionDefinition>(definition: T): T {
  if (!definition || typeof definition !== "object") {
    throw new Error("defineExtension 需要一个对象参数");
  }
  return definition;
}

/** 工具注册表：登记处理器并在重复注册时立即失败。 */
export interface ToolRegistry {
  /** 登记一个工具；名字重复时抛错（包含已登记的名字列表）。 */
  add(tool: DefinedTool): DefinedTool;
  /** 按登记顺序取回全部工具，供 defineExtension 使用。 */
  tools(): DefinedTool[];
  /** 已登记的工具名。 */
  names(): string[];
}

/**
 * createToolRegistry 建一个工具注册表。
 *
 * 多工具的扩展（例如"搜索 + 抓取 + 余额"三件套）用注册表把声明摆在一处，
 * 重名问题在扩展加载时就能发现，而不是等到模型调用时被静默跳过。
 */
export function createToolRegistry(): ToolRegistry {
  const tools: DefinedTool[] = [];
  const seen: Record<string, boolean> = {};
  return {
    add(tool: DefinedTool): DefinedTool {
      if (!tool || typeof tool !== "object" || tool.__llamaSwapTool !== true) {
        throw new Error("registry.add 需要 defineTool/searchTool/fetchTool 的返回值");
      }
      if (seen[tool.name]) {
        throw new Error(`工具名重复：${tool.name}（同一扩展内必须唯一）`);
      }
      seen[tool.name] = true;
      tools.push(tool);
      return tool;
    },
    tools(): DefinedTool[] {
      return tools.slice();
    },
    names(): string[] {
      return Object.keys(seen);
    },
  };
}

// ---------------------------------------------------------------------------
// 通用小工具
// ---------------------------------------------------------------------------

/** 把任意值折成单行文本：去掉换行、压缩空白。 */
export function collapseText(value: unknown): string {
  return String(value === undefined || value === null ? "" : value)
    .replace(/\s+/g, " ")
    .trim();
}

/** 取出配置里的整数并夹到 [min, max]；非法值回落到 fallback。 */
export function clampCount(value: unknown, fallback: number, min: number, max: number): number {
  const parsed = Math.floor(Number(value));
  if (!isFinite(parsed)) {
    return fallback;
  }
  if (parsed < min) {
    return min;
  }
  if (parsed > max) {
    return max;
  }
  return parsed;
}

/** HTTP 状态码对应的处理建议，可被提供方覆盖。 */
export const DEFAULT_STATUS_ADVICE: Record<number, string> = {
  400: "请求格式或参数不合法，修正参数后重试。",
  401: "API Key 无效或缺失，检查扩展设置里的密钥。",
  402: "账户余额不足，充值后重试。",
  403: "请求被拒绝（可能是风控或权限限制），不要重试。",
  404: "上游没有找到可用内容，换一个 URL 或稍后重试。",
  408: "上游读取请求超时，稍后重试。",
  413: "请求体过大，减少参数体积后重试。",
  429: "触发限流（QPS 超限），等待后重试。",
  500: "上游内部错误，稍后重试。",
  502: "上游网关错误，稍后重试。",
  503: "上游暂不可用，稍后重试。",
  504: "上游下游超时，稍后重试。",
};

/**
 * apiErrorMessage 把一次失败的 HTTP 响应整形成一条可读错误。
 *
 * 不同提供方的错误信封五花八门（OpenAI 风格的 error.message、智谱的
 * error.code、火山引擎的 ResponseMetadata.Error、以及裸 message），
 * 这里统一抽取，再按状态码附上一句处理建议，方便模型据此决定是否重试。
 */
export function apiErrorMessage(
  status: number,
  body: string,
  options?: { provider?: string; advice?: Record<number, string> },
): string {
  const provider = (options && options.provider) || "请求";
  let message = collapseText(body).slice(0, 300);
  try {
    const parsed = JSON.parse(body);
    if (parsed && typeof parsed === "object") {
      const candidates: unknown[] = [
        parsed.error && typeof parsed.error === "object" ? (parsed.error as any).message : undefined,
        typeof parsed.error === "string" ? parsed.error : undefined,
        parsed.message,
        parsed.detail,
        parsed.base_resp && (parsed.base_resp as any).status_msg,
        parsed.ResponseMetadata && parsed.ResponseMetadata.Error && parsed.ResponseMetadata.Error.Message,
      ];
      for (const candidate of candidates) {
        if (typeof candidate === "string" && collapseText(candidate) !== "") {
          message = collapseText(candidate);
          break;
        }
      }
      const code = parsed.error && typeof parsed.error === "object" ? (parsed.error as any).code : undefined;
      if (typeof code === "string" && code !== "" && message.indexOf(code) < 0) {
        message = code + " " + message;
      }
    }
  } catch (ignored) {
    // 不是 JSON：原始 body 已是最好的信息。
  }

  const table = Object.assign({}, DEFAULT_STATUS_ADVICE, (options && options.advice) || {});
  let advice = "";
  if (table[status]) {
    advice = table[status];
  } else if (status >= 500) {
    advice = DEFAULT_STATUS_ADVICE[500];
  }

  let text = provider + " 请求失败：HTTP " + status;
  if (message !== "") {
    text += " - " + message;
  }
  if (advice !== "") {
    text += "（" + advice + "）";
  }
  return text;
}

/** 一次 HTTP 请求的声明形态，由提供方的 request() 返回。 */
export interface HttpRequestPlan {
  url: string;
  method?: string;
  headers?: Record<string, string>;
  /** 对象会被 JSON 序列化；字符串按原文发送；不传则无请求体。 */
  body?: unknown;
}

/**
 * applyCredential 把密钥放进请求。
 *
 * 支持四种常见位置：Authorization: Bearer、自定义请求头、JSON body 字段、
 * URL 查询参数——覆盖了主流搜索服务；提供方只要声明位置即可，无需各自拼装。
 */
function applyCredential(
  plan: HttpRequestPlan,
  credential: SearchCredential,
  value: string,
): HttpRequestPlan {
  const headers = Object.assign({}, plan.headers || {});
  if (credential.bearer) {
    headers["Authorization"] = "Bearer " + value;
  } else if (credential.header) {
    headers[credential.header] = value;
  }
  let url = plan.url;
  if (credential.query) {
    const separator = url.indexOf("?") >= 0 ? "&" : "?";
    url = url + separator + encodeURIComponent(credential.query) + "=" + encodeURIComponent(value);
  }
  let body = plan.body;
  if (credential.body && body && typeof body === "object") {
    body = Object.assign({}, body as Record<string, unknown>);
    (body as Record<string, unknown>)[credential.body] = value;
  }
  return { url, method: plan.method, headers, body };
}

/** 密钥在请求里的位置。 */
export interface SearchCredential {
  /** 设置项名字，例如 apiKey。 */
  setting: string;
  /** 报错时展示的名称，默认 "API Key"。 */
  label?: string;
  /** 放进这个请求头。 */
  header?: string;
  /** 用 Authorization: Bearer 发送。 */
  bearer?: boolean;
  /** 放进 JSON body 的这个字段。 */
  body?: string;
  /** 放进 URL 查询串的这个参数名。 */
  query?: string;
}

/** 单条搜索结果的规范化形态；提供方按需填字段。 */
export interface SearchResultItem {
  title?: string;
  url?: string;
  /** 简短摘要。 */
  snippet?: string;
  /** 较长的正文/摘录。 */
  content?: string;
  /** 多条摘录，逐条渲染。 */
  highlights?: unknown;
  /** 摘要字段的兜底（部分服务叫 summary）。 */
  summary?: string;
  /** 发布日期（原样展示）。 */
  date?: string;
  /** 来源站点名。 */
  source?: string;
  /** 相关度分数。 */
  score?: unknown;
  /** 额外元信息，渲染成一行 "a | b"。 */
  meta?: string[];
  /** 完全自定义的正文行（优先于其它正文字段）。 */
  lines?: string[];
}

/** 搜索提供方的声明。 */
export interface SearchProviderSpec {
  /** 工具名，必须全局唯一，例如 web_search_exa。 */
  name: string;
  /** 服务名，用于结果标题与错误信息，例如 "Exa"。 */
  title: string;
  /** 展示给模型的说明。 */
  description: string;
  /** 除 query 之外的额外参数 Schema；给定时会与 query 合并。 */
  parameters?: Record<string, unknown>;
  /** query 的长度上限；超出即报错（部分服务有硬限制）。 */
  maxQueryLength?: number;
  /** query 参数的字段名，默认 "query"。 */
  queryParameter?: string;
  /** 密钥位置。 */
  credential: SearchCredential;
  /**
   * 构造请求。args.query 是已校验的查询串，
   * args.config 是扩展设置，args.arguments 是模型传来的完整参数。
   */
  request(args: {
    query: string;
    config: Record<string, any>;
    arguments: Record<string, any>;
  }): HttpRequestPlan;
  /** 从响应 JSON 里取出结果数组。 */
  parse(payload: any): SearchResultItem[];
  /** 可选：结果之前的额外内容（例如 Tavily 的 answer、服务的提示语）。 */
  preface?(payload: any): string | string[] | undefined;
  /** 可选：无结果时的提示语。 */
  emptyMessage?(query: string): string;
  /** 可选：输出字符上限，默认 8000。 */
  maxChars?: number;
  /** 可选：状态码建议覆盖表。 */
  statusAdvice?: Record<number, string>;
}

/** 渲染结果数组：统一的编号列表格式。 */
function renderSearchResults(
  providerTitle: string,
  query: string,
  results: SearchResultItem[],
  preface: string[],
  maxChars: number,
): string {
  const lines: string[] = [providerTitle + " results for: " + query, ""];
  for (let index = 0; index < preface.length; index += 1) {
    if (collapseText(preface[index]) !== "") {
      lines.push(preface[index]);
    }
  }
  if (preface.length > 0) {
    lines.push("");
  }
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    lines.push(index + 1 + ". " + (collapseText(item.title) || "(untitled)"));
    if (item.url) {
      lines.push("   " + collapseText(item.url));
    }
    const meta: string[] = [];
    if (item.source) {
      meta.push(collapseText(item.source));
    }
    if (item.date) {
      meta.push(collapseText(item.date));
    }
    if (item.score !== undefined && item.score !== null && item.score !== "") {
      meta.push("score " + collapseText(item.score));
    }
    if (item.meta && item.meta.length > 0) {
      for (let part = 0; part < item.meta.length; part += 1) {
        if (collapseText(item.meta[part]) !== "") {
          meta.push(collapseText(item.meta[part]));
        }
      }
    }
    if (meta.length > 0) {
      lines.push("   " + meta.join(" | "));
    }
    if (item.lines && item.lines.length > 0) {
      for (let part = 0; part < item.lines.length; part += 1) {
        const text = collapseText(item.lines[part]);
        if (text !== "") {
          lines.push("   " + text);
        }
      }
    } else {
      const highlights = Array.isArray(item.highlights) ? item.highlights : [];
      if (highlights.length > 0) {
        for (let part = 0; part < highlights.length; part += 1) {
          const text = collapseText(highlights[part]);
          if (text !== "") {
            lines.push("   " + text);
          }
        }
      } else {
        const bodyText = collapseText(item.content) || collapseText(item.snippet) || collapseText(item.summary);
        if (bodyText !== "") {
          lines.push("   " + bodyText);
        }
      }
    }
    lines.push("");
  }
  return lines.join("\n").slice(0, maxChars);
}

/**
 * searchTool 声明一个搜索工具：注册一次，名字唯一，处理逻辑由 SDK 兜住。
 *
 * 提供方只需要回答三件事——请求怎么发、结果怎么取、额外的提示语是什么；
 * 查询校验、密钥检查、HTTP 错误整形（含重试建议）、无结果提示、
 * 编号列表渲染与长度截断都由这里统一处理。
 */
export function searchTool(spec: SearchProviderSpec): DefinedTool {
  if (!spec || typeof spec !== "object") {
    throw new Error("searchTool 需要一个对象参数");
  }
  const queryParameter = spec.queryParameter || "query";
  const properties: Record<string, unknown> = { query: { type: "string", description: "搜索查询串。" } };
  const required: string[] = ["query"];
  if (spec.parameters && spec.parameters.properties) {
    const extra = spec.parameters.properties as Record<string, unknown>;
    Object.keys(extra).forEach((key) => {
      if (key !== queryParameter) {
        properties[key] = extra[key];
      }
    });
  }
  if (spec.parameters && Array.isArray(spec.parameters.required)) {
    const declared = spec.parameters.required as string[];
    for (let index = 0; index < declared.length; index += 1) {
      if (required.indexOf(declared[index]) < 0) {
        required.push(declared[index]);
      }
    }
  }

  return defineTool({
    name: spec.name,
    description: spec.description,
    parameters: { type: "object", properties, required },
    async handler(args: Record<string, any>, ctx: ExtensionContext) {
      const query = collapseText(args[queryParameter]);
      if (query === "") {
        throw new Error(spec.name + ": query must not be empty");
      }
      if (spec.maxQueryLength && query.length > spec.maxQueryLength) {
        throw new Error(
          spec.name + ": query must be at most " + spec.maxQueryLength + " characters (got " + query.length + ")",
        );
      }
      const label = spec.credential.label || "API Key";
      const credentialValue = collapseText(ctx.config[spec.credential.setting]);
      if (credentialValue === "") {
        throw new Error(spec.name + ": set the " + label + " in the extension settings");
      }

      const defaultPlan = spec.request({ query, config: ctx.config, arguments: args });
      if (!defaultPlan || typeof defaultPlan.url !== "string" || defaultPlan.url === "") {
        throw new Error(spec.name + ": 提供方没有给出请求地址");
      }
      const plan = applyCredential(defaultPlan, spec.credential, credentialValue);
      const options: Record<string, any> = {
        method: plan.method || "GET",
        headers: Object.assign({ "Content-Type": "application/json" }, plan.headers || {}),
      };
      let timeoutMs = Number(ctx.config.requestTimeoutMs);
      if (isFinite(timeoutMs) && timeoutMs > 0) {
        options.timeoutMs = timeoutMs;
      }
      if (plan.body !== undefined && plan.body !== null) {
        options.body = typeof plan.body === "string" ? plan.body : JSON.stringify(plan.body);
      }

      if (ctx.log && typeof ctx.log.debug === "function") {
        ctx.log.debug(spec.name + " -> " + plan.url.split("?")[0] + " method=" + options.method + " query=" + query);
      }

      let response: any;
      try {
        response = await ctx.http.fetch(plan.url, options);
      } catch (error) {
        throw new Error(spec.title + " 无法访问：" + String(error === undefined ? "" : error));
      }

      const body = response.text();
      if (!response.ok) {
        throw new Error(apiErrorMessage(response.status, body, { provider: spec.title, advice: spec.statusAdvice }));
      }
      let payload: any;
      try {
        payload = JSON.parse(body);
      } catch (ignored) {
        throw new Error(spec.title + " 返回的内容不是 JSON");
      }
      if (!payload || typeof payload !== "object") {
        throw new Error(spec.title + " 返回的内容不是 JSON 对象");
      }

      const results = spec.parse(payload) || [];
      const preface: string[] = [];
      if (spec.preface) {
        const extra = spec.preface(payload);
        if (typeof extra === "string") {
          preface.push(extra);
        } else if (Array.isArray(extra)) {
          for (let index = 0; index < extra.length; index += 1) {
            preface.push(String(extra[index]));
          }
        }
      }
      if (results.length === 0) {
        if (spec.emptyMessage) {
          return { content: spec.emptyMessage(query) };
        }
        // 无结果不等于无信息：部分服务会在结果之外给出提示（例如改写后的
        // 查询、限流提醒），此时把它们一并交给模型。
        const lines = ["No " + spec.title + " results found for: " + query];
        const notes: string[] = [];
        for (let index = 0; index < preface.length; index += 1) {
          if (collapseText(preface[index]) !== "") {
            notes.push(preface[index]);
          }
        }
        if (notes.length > 0) {
          lines.push("");
          for (let index = 0; index < notes.length; index += 1) {
            lines.push(notes[index]);
          }
        }
        return { content: lines.join("\n") };
      }
      if (ctx.log && typeof ctx.log.info === "function") {
        ctx.log.info(spec.name + " -> " + results.length + " 条结果");
      }
      return { content: renderSearchResults(spec.title, query, results, preface, spec.maxChars || 8000) };
    },
  });
}

/** 抓取提供方的声明：服务端已把网页转成 Markdown，客户端只做请求与整形。 */
export interface FetchProviderSpec {
  /** 工具名，必须全局唯一，例如 web_fetch_deepseek。 */
  name: string;
  /** 服务名，用于错误信息，例如 "DeepSeek" 。 */
  title: string;
  description: string;
  credential: SearchCredential;
  /** 构造请求；url 是已校验的 http/https 地址。 */
  request(args: { url: string; config: Record<string, any>; arguments: Record<string, any> }): HttpRequestPlan;
  /** 从响应 JSON 里取出 { title, content }。 */
  parse(payload: any): { title?: string; content?: string };
  maxUrlLength?: number;
  maxChars?: number;
  statusAdvice?: Record<number, string>;
}

/**
 * fetchTool 声明一个「服务端抓取」工具（URL → 标题 + Markdown 正文）。
 *
 * 与 web-fetch 预设的区别：web-fetch 在扩展里自己下载并解析 HTML，
 * 不需要密钥、离线可读但只支持静态页面；fetchTool 走提供方的抓取接口，
 * 由对方负责渲染与正文抽取，因此需要密钥、按次计费，但拿到的是干净正文。
 */
export function fetchTool(spec: FetchProviderSpec): DefinedTool {
  if (!spec || typeof spec !== "object") {
    throw new Error("fetchTool 需要一个对象参数");
  }
  const maxUrlLength = spec.maxUrlLength || 8192;
  return defineTool({
    name: spec.name,
    description: spec.description,
    parameters: {
      type: "object",
      properties: { url: { type: "string", description: "要抓取的 http/https 地址。" } },
      required: ["url"],
      type: "object",
    },
    async handler(args: Record<string, any>, ctx: ExtensionContext) {
      const url = collapseText(args.url);
      if (url === "") {
        throw new Error(spec.name + ": url must not be empty");
      }
      if (url.length > maxUrlLength) {
        throw new Error(spec.name + ": url must be at most " + maxUrlLength + " characters (got " + url.length + ")");
      }
      if (!/^https?:\/\//i.test(url)) {
        throw new Error(spec.name + ": url 必须是 http 或 https 地址");
      }
      const label = spec.credential.label || "API Key";
      const credentialValue = collapseText(ctx.config[spec.credential.setting]);
      if (credentialValue === "") {
        throw new Error(spec.name + ": set the " + label + " in the extension settings");
      }

      const defaultPlan = spec.request({ url, config: ctx.config, arguments: args });
      const plan = applyCredential(defaultPlan, spec.credential, credentialValue);
      const options: Record<string, any> = {
        method: plan.method || "POST",
        headers: Object.assign({ "Content-Type": "application/json" }, plan.headers || {}),
      };
      let timeoutMs = Number(ctx.config.requestTimeoutMs);
      if (isFinite(timeoutMs) && timeoutMs > 0) {
        options.timeoutMs = timeoutMs;
      }
      if (plan.body !== undefined && plan.body !== null) {
        options.body = typeof plan.body === "string" ? plan.body : JSON.stringify(plan.body);
      }

      let response: any;
      try {
        response = await ctx.http.fetch(plan.url, options);
      } catch (error) {
        throw new Error(spec.title + " 无法访问：" + String(error === undefined ? "" : error));
      }
      const body = response.text();
      if (!response.ok) {
        throw new Error(apiErrorMessage(response.status, body, { provider: spec.title, advice: spec.statusAdvice }));
      }
      let payload: any;
      try {
        payload = JSON.parse(body);
      } catch (ignored) {
        throw new Error(spec.title + " 返回的内容不是 JSON");
      }
      if (!payload || typeof payload !== "object") {
        throw new Error(spec.title + " 返回的内容不是 JSON 对象");
      }

      const parsed = spec.parse(payload) || {};
      const title = collapseText(parsed.title);
      const content = String(parsed.content === undefined || parsed.content === null ? "" : parsed.content);
      if (collapseText(content) === "") {
        return { content: spec.title + " 没有返回可读正文：" + url };
      }
      const lines = ["Source: " + url];
      if (title !== "") {
        lines.push("Title: " + title);
      }
      lines.push("Type: markdown, " + content.length + " chars");
      lines.push("");
      lines.push(content.slice(0, spec.maxChars || 32000));
      if (ctx.log && typeof ctx.log.info === "function") {
        ctx.log.info(spec.name + " -> " + url + " " + content.length + " chars");
      }
      return { content: lines.join("\n") };
    },
  });
}
