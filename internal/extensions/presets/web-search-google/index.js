// Google 搜索（经 SerpApi）。
//
//   GET https://serpapi.com/search.json
//       ?engine=google&api_key=...&q=...&num=...&hl=...&gl=...
//
//   响应：{organic_results: [{title, link, snippet, position, date}],
//           answer_box?, search_information?, error?}
//
// SerpApi 按设计把密钥放进查询参数（api_key），因此凭据位置声明为 query；
// SDK 只打印去掉查询串后的地址，密钥不会进日志。
// 已知答案（answer_box）存在时一并返回。
//
// 工具名带 google 后缀：多个搜索器可同时启用。

import { clampCount, defineExtension, searchTool } from "@llama-swap/extension";

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "SerpApi API key, passed as the api_key query parameter. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many organic results to request (num). SerpApi accepts 1-100.",
    default: 10,
    min: 1,
    max: 100
  },
  hl: {
    type: "string",
    label: "Language (hl)",
    hint: "UI language code such as 'en' or 'zh'. Leave empty to omit.",
    default: "zh-cn"
  },
  gl: {
    type: "string",
    label: "Country (gl)",
    hint: "Two-letter country code such as 'us' or 'cn'. Leave empty to omit.",
    default: "cn"
  },
  requestTimeoutMs: {
    type: "number",
    label: "HTTP timeout (ms)",
    hint: "Per-request timeout for the search call.",
    default: 20000,
    min: 1000,
    max: 30000
  }
};

export default defineExtension({
  tools: [
    searchTool({
      name: "web_search_google",
      title: "Google",
      description:
        "Search Google through SerpApi and return numbered organic results with title, link, date and snippet.",
      maxQueryLength: 400,
      credential: { setting: "apiKey", label: "API Key", query: "api_key" },
      request({ query, config }) {
        // SerpApi 的查询参数叫 q（工具参数仍叫 query）。
        const params = [
          "engine=google",
          "q=" + encodeURIComponent(query),
          "num=" + clampCount(config.maxResults, 10, 1, 100)
        ];
        if (config.hl) {
          params.push("hl=" + encodeURIComponent(config.hl));
        }
        if (config.gl) {
          params.push("gl=" + encodeURIComponent(config.gl));
        }
        return { url: "https://serpapi.com/search.json?" + params.join("&") };
      },
      preface(payload) {
        // 已知答案（answer_box）是 Google 直接给出的结论，比链接更有用。
        const answer = payload.answer_box;
        if (answer && (answer.answer || answer.snippet)) {
          const text = "Answer box: " + (answer.answer || answer.snippet);
          return answer.link ? [text, "  " + answer.link] : text;
        }
        return undefined;
      },
      parse(payload) {
        // SerpApi 用 HTTP 200 携带业务错误，必须显式识别。
        if (payload.error) {
          const message =
            payload.error && typeof payload.error === "object"
              ? payload.error.message || JSON.stringify(payload.error)
              : String(payload.error);
          throw new Error("SerpApi 返回错误：" + message);
        }
        const items = Array.isArray(payload.organic_results) ? payload.organic_results : [];
        return items.map((item) => ({
          title: item.title,
          url: item.link,
          date: item.date,
          snippet: item.snippet
        }));
      }
    })
  ]
});
