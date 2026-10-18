// Tavily Search API。
//
//   POST https://api.tavily.com/search
//   Content-Type: application/json
//   请求体：{api_key, query, max_results, search_depth, include_answer}
//           search_depth: "basic" | "advanced"
//
//   响应：{query, answer?, results: [{title, url, content, score,
//           raw_content?}], response_time}
//
// Tavily 按设计把密钥放进 JSON body（api_key 字段），因此凭据位置声明为 body。
// 密钥只在设置里填写，绝不写进本文件。
//
// 工具名带 tavily 后缀：多个搜索器可同时启用。

import { clampCount, defineExtension, searchTool } from "@llama-swap/extension";

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "Tavily API key, sent as the api_key body field. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to request (max_results). Tavily accepts up to 20.",
    default: 5,
    min: 1,
    max: 20
  },
  searchDepth: {
    type: "select",
    label: "Search depth",
    hint: "'advanced' costs more credits but improves relevance.",
    options: ["basic", "advanced"],
    default: "basic"
  },
  includeAnswer: {
    type: "boolean",
    label: "Include answer",
    hint: "Ask Tavily to summarize an answer alongside the results.",
    default: true
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
      name: "web_search_tavily",
      title: "Tavily",
      description:
        "Search the web with Tavily and return an optional short answer plus numbered results with title, URL, score and content.",
      maxQueryLength: 400,
      credential: { setting: "apiKey", label: "API Key", body: "api_key" },
      request({ query, config }) {
        return {
          url: "https://api.tavily.com/search",
          method: "POST",
          body: {
            query,
            max_results: clampCount(config.maxResults, 5, 1, 20),
            search_depth: config.searchDepth === "advanced" ? "advanced" : "basic",
            include_answer: config.includeAnswer !== false
          }
        };
      },
      // answer 是服务端给出的现成答案，放在结果之前展示。
      preface(payload) {
        return payload.answer ? "Answer: " + payload.answer : undefined;
      },
      parse(payload) {
        const items = Array.isArray(payload.results) ? payload.results : [];
        return items.map((item) => ({
          title: item.title,
          url: item.url,
          score: typeof item.score === "number" ? item.score.toFixed(3) : undefined,
          content: item.content
        }));
      }
    })
  ]
});
