// Parallel Search API。
//
// 合同依据 Parallel 官方文档
// （https://docs.parallel.ai/search/search-quickstart 与
//  https://docs.parallel.ai/search/advanced-search-settings）：
//
//   POST https://api.parallel.ai/v1/search
//   x-api-key: <key>
//   Content-Type: application/json
//   请求体：
//     {search_queries: ["..."],          // 唯一必填字段
//      objective?: "...",                // 自然语言意图，能改善选源
//      mode?: "turbo"|"fast"|"advanced", // 默认 advanced（约 3 秒）
//      advanced_settings: {max_results,
//                          excerpt_settings: {max_chars_per_result},
//                          location}}
//
//   响应：
//     {search_id,
//      results: [{url, title, publish_date, excerpts: ["...", "..."]}],
//      warnings?: ["..."]}
//
// 两个容易踩的点：/v1/search 是 GA 路径（旧版 /v1beta/search 只留给存量集成，
// 官方明确说不要互相替换）；advanced 是默认模式且约 3 秒，聊天回合应当用 fast；
// 公开模式的 max_results 上限为 20，超出只会得到一条 warning。
//
// 工具名带 parallel 后缀：多个搜索器可同时启用。

const DEFAULT_RESULTS = 5;
const MIN_RESULTS = 1;
// 公开模式上限 20，要更多只会换来一条 warning。
const MAX_RESULTS = 20;
const DEFAULT_CHARS = 2000;
const MIN_CHARS = 200;
const MAX_CHARS = 20000;

import { clampCount, collapseText, defineExtension, searchTool } from "@llama-swap/extension";

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "Parallel API key from https://platform.parallel.ai, sent as the x-api-key header. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  mode: {
    type: "select",
    label: "Search mode",
    hint: "turbo ~200ms, fast ~700ms, advanced ~3s with the best quality. fast is the right default for a chat turn.",
    options: ["turbo", "fast", "advanced"],
    default: "fast",
    section: "Search"
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "Upper bound on returned results (advanced_settings.max_results). The public modes cap it at 20; the API may return fewer.",
    default: DEFAULT_RESULTS,
    min: MIN_RESULTS,
    max: MAX_RESULTS,
    section: "Search"
  },
  maxCharsPerResult: {
    type: "number",
    label: "Max excerpt characters",
    hint: "Excerpt size per result (advanced_settings.excerpt_settings.max_chars_per_result). Smaller excerpts keep the answer inside the context window.",
    default: DEFAULT_CHARS,
    min: MIN_CHARS,
    max: MAX_CHARS,
    section: "Content"
  },
  location: {
    type: "string",
    label: "Location",
    hint: "Optional ISO 3166-1 alpha-2 country code for geo-targeted results, e.g. us, gb, de, jp. Leave empty to let Parallel decide.",
    section: "Content"
  },
  requestTimeoutMs: {
    type: "number",
    label: "HTTP timeout (ms)",
    hint: "Per-request timeout. advanced mode takes ~3s, so leave it generous.",
    default: 25000,
    min: 1000,
    max: 60000
  }
};

export default defineExtension({
  tools: [
    searchTool({
      name: "web_search_parallel",
      title: "Parallel",
      description:
        "Search the web with Parallel and return numbered results with title, URL, publish date and query-relevant excerpts. It selects sources for an objective, so pass one when the question has a purpose.",
      maxQueryLength: 2048,
      credential: { setting: "apiKey", label: "API Key", header: "x-api-key" },
      // objective 是 Parallel 特有的可选参数，交给模型按需填写。
      parameters: {
        type: "object",
        properties: {
          query: { type: "string", description: "Search query." },
          objective: {
            type: "string",
            description:
              "Optional: what the answer is for, e.g. 'decide whether to migrate the build to uv'. Parallel uses it to pick better sources."
          }
        },
        required: ["query"]
      },
      request({ query, config, arguments: args }) {
        const advanced = {
          max_results: clampCount(config.maxResults, DEFAULT_RESULTS, MIN_RESULTS, MAX_RESULTS),
          excerpt_settings: {
            // 摘录预算必须嵌在 excerpt_settings 里：
            // 放在顶层会被忽略，这正是官方文档提醒的迁移陷阱。
            max_chars_per_result: clampCount(config.maxCharsPerResult, DEFAULT_CHARS, MIN_CHARS, MAX_CHARS)
          }
        };
        const location = collapseText(config.location);
        if (location !== "") {
          advanced.location = location.toLowerCase();
        }
        const body = {
          search_queries: [query],
          mode: String(config.mode || "fast"),
          advanced_settings: advanced
        };
        const objective = collapseText(args.objective);
        if (objective !== "") {
          body.objective = objective;
        }
        return { url: "https://api.parallel.ai/v1/search", method: "POST", body };
      },
      // warnings 会说明服务端做了哪些裁剪（例如 max_results 被降到 20）。
      preface(payload) {
        const warnings = Array.isArray(payload.warnings) ? payload.warnings : [];
        if (warnings.length === 0) {
          return undefined;
        }
        return warnings.map((text) => "Note from the API: " + collapseText(text));
      },
      parse(payload) {
        const items = Array.isArray(payload.results) ? payload.results : [];
        return items.map((item) => ({
          title: item.title,
          url: item.url,
          date: item.publish_date ? String(item.publish_date) : "",
          highlights: item.excerpts
        }));
      },
      statusAdvice: {
        422: "请求被判为参数错误，这通常是扩展本身的问题而不是查询的问题。"
      }
    })
  ]
});
