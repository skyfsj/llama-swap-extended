// Bing Web Search v7（微软）。
//
//   GET https://api.bing.microsoft.com/v7.0/search
//       ?q=...&count=...&mkt=...&safeSearch=...&responseFilter=Webpages
//   请求头：Ocp-Apim-Subscription-Key: <key>
//
//   响应：{webPages: {value: [{name, url, snippet, displayUrl, ...}]}}
//   无结果时服务端直接省略 webPages，所以要防御式取值。
//
// 工具名带 bing 后缀：多个搜索器可同时启用（重名会被工具冲突策略跳过）。
// 密钥只在设置里填写，绝不写进本文件。

import { clampCount, defineExtension, searchTool } from "@llama-swap/extension";

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "Bing Web Search v7 subscription key (Ocp-Apim-Subscription-Key). Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to return. Bing accepts 1-50.",
    default: 10,
    min: 1,
    max: 50
  },
  market: {
    type: "select",
    label: "Market",
    hint: "Market code that biases results toward a language and region.",
    options: ["en-US", "zh-CN", "ja-JP", "en-GB", "de-DE", "fr-FR", "es-ES", "pt-BR", "ru-RU"],
    default: "zh-CN"
  },
  safeSearch: {
    type: "select",
    label: "Safe search",
    options: ["Moderate", "Strict", "Off"],
    default: "Moderate"
  },
  requestTimeoutMs: {
    type: "number",
    label: "HTTP timeout (ms)",
    hint: "Per-request timeout for the search call. The extension's own timeout still caps the whole hook.",
    default: 15000,
    min: 1000,
    max: 20000
  }
};

export default defineExtension({
  tools: [
    searchTool({
      name: "web_search_bing",
      title: "Bing",
      description:
        "Search the web with Microsoft Bing and return numbered results with title, URL and snippet. Use it for current facts, news and sources.",
      maxQueryLength: 400,
      credential: { setting: "apiKey", label: "API Key", header: "Ocp-Apim-Subscription-Key" },
      request({ query, config }) {
        // Bing 的查询参数名是 q（工具参数仍叫 query），条数是 count。
        const params = [
          "q=" + encodeURIComponent(query),
          "count=" + clampCount(config.maxResults, 10, 1, 50),
          "responseFilter=Webpages",
          "safeSearch=" + encodeURIComponent(config.safeSearch || "Moderate")
        ];
        if (config.market) {
          params.push("mkt=" + encodeURIComponent(config.market));
        }
        return { url: "https://api.bing.microsoft.com/v7.0/search?" + params.join("&") };
      },
      parse(payload) {
        const page = payload.webPages;
        const items = page && Array.isArray(page.value) ? page.value : [];
        return items.map((item) => ({
          title: item.name,
          url: item.url,
          snippet: item.snippet,
          source: item.displayUrl
        }));
      }
    })
  ]
});
