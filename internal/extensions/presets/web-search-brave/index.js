// Brave Search API（web 端点）。
//
//   GET https://api.search.brave.com/v1/web/search
//       ?q=...&count=...&country=...&safesearch=...
//   请求头：Accept: application/json
//           X-Subscription-Token: <token>   ← 订阅令牌，放进设置，别写进代码
//
//   响应：{query: {...}, web: {results: [{title, url, description, age}]}}
//   错误以 4xx/5xx 返回 JSON，SDK 会连状态码一起整形成可读信息。
//
// 工具名带 brave 后缀，便于和其它搜索器同时启用。

import { clampCount, defineExtension, searchTool } from "@llama-swap/extension";

export const settings = {
  apiKey: {
    type: "string",
    label: "Subscription Token",
    hint: "Brave Search API token (X-Subscription-Token header). Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to return. Brave accepts 1-20.",
    default: 10,
    min: 1,
    max: 20
  },
  country: {
    type: "select",
    label: "Country",
    hint: "Two-letter country code that biases results.",
    options: ["US", "GB", "DE", "FR", "JP", "IN", "BR", "AU", "CA", "CN"],
    default: "CN"
  },
  safeSearch: {
    type: "select",
    label: "Safe search",
    options: ["moderate", "strict", "off"],
    default: "moderate"
  },
  requestTimeoutMs: {
    type: "number",
    label: "HTTP timeout (ms)",
    hint: "Per-request timeout for the search call.",
    default: 15000,
    min: 1000,
    max: 20000
  }
};

export default defineExtension({
  tools: [
    searchTool({
      name: "web_search_brave",
      title: "Brave",
      description:
        "Search the web with Brave Search and return numbered results with title, URL, age and description.",
      maxQueryLength: 400,
      credential: { setting: "apiKey", label: "Subscription Token", header: "X-Subscription-Token" },
      request({ query, config }) {
        // Brave 的查询参数叫 q，条数 count，安全过滤 safesearch。
        const params = [
          "q=" + encodeURIComponent(query),
          "count=" + clampCount(config.maxResults, 10, 1, 20),
          "safesearch=" + encodeURIComponent(config.safeSearch || "moderate")
        ];
        if (config.country) {
          params.push("country=" + encodeURIComponent(config.country));
        }
        return {
          url: "https://api.search.brave.com/v1/web/search?" + params.join("&"),
          headers: { Accept: "application/json" }
        };
      },
      parse(payload) {
        const web = payload.web;
        const items = web && Array.isArray(web.results) ? web.results : [];
        return items.map((item) => ({
          title: item.title,
          url: item.url,
          date: item.age,
          snippet: item.description
        }));
      }
    })
  ]
});
