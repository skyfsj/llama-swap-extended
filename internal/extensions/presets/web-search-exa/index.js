// Exa Search API。
//
// 合同依据 Exa 官方文档（https://exa.ai/docs/search/quickstart）：
//
//   POST https://api.exa.ai/search
//   x-api-key: <key>            （也接受 Authorization: Bearer <key>）
//   Content-Type: application/json
//   请求体：
//     {query, type: "auto"|"fast"|"instant"|"deep-lite"|"deep"|"deep-reasoning",
//      numResults,                       // 默认 10，上限 100
//      contents: {highlights: true}      // 推荐：返回与查询相关的摘录
//              | {text: {maxCharacters}} // 或返回清洗后的正文
//      includeDomains: ["docs.python.org"]}
//
//   响应：
//     {requestId, resolvedSearchType,
//      results: [{id, title, url, publishedDate, author?, score?,
//                 highlights: [...], text?}],
//      costDollars: {...}}
//
// 搜索无分页；instant 按每千次 4 美元计费，所以条数就是成本旋钮。
// 错误以 HTTP 状态码 + JSON body 返回，SDK 会原样转成可读错误。
//
// 工具名带 exa 后缀：多个搜索器可同时启用。

import { clampCount, collapseText, defineExtension, searchTool } from "@llama-swap/extension";

const HIGHLIGHTS = "highlights";
const TEXT = "text";

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "Exa API key from https://dashboard.exa.ai/api-keys, sent as the x-api-key header. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  searchType: {
    type: "select",
    label: "Search type",
    hint: "auto balances quality and speed (~1s). fast ~450ms, instant ~250ms (billed per request). The deep types run a research pass and return synthesized content.",
    options: ["auto", "fast", "instant", "deep-lite", "deep", "deep-reasoning"],
    default: "auto",
    section: "Search"
  },
  numResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to request (numResults). Exa returns up to 100 and bills per request, so this is the cost knob.",
    default: 5,
    min: 1,
    max: 25,
    section: "Search"
  },
  includeDomains: {
    type: "string",
    label: "Limit to domains",
    hint: "Optional comma-separated domains to search only, e.g. docs.python.org, github.com. Leave empty to search the whole index.",
    section: "Search"
  },
  contentMode: {
    type: "select",
    label: "Content per result",
    hint: "highlights returns only the excerpts relevant to the query, which is the token-efficient default; text returns each page body up to the character limit.",
    options: [HIGHLIGHTS, TEXT],
    default: HIGHLIGHTS,
    section: "Content"
  },
  maxCharacters: {
    type: "number",
    label: "Max characters per page",
    hint: "Only used by the text content mode (contents.text.maxCharacters).",
    default: 2000,
    min: 200,
    max: 20000,
    section: "Content"
  },
  requestTimeoutMs: {
    type: "number",
    label: "HTTP timeout (ms)",
    hint: "Per-request timeout. Exa search is usually fast, but the deep types can take much longer.",
    default: 20000,
    min: 1000,
    max: 60000
  }
};

export default defineExtension({
  tools: [
    searchTool({
      name: "web_search_exa",
      title: "Exa",
      description:
        "Search the web with Exa: a neural search engine that finds pages by meaning rather than keywords, returning each result with the excerpts most relevant to the query. Use it for research questions and to find current sources. Queries work best as full sentences or descriptive phrases.",
      maxQueryLength: 2048,
      credential: { setting: "apiKey", label: "API Key", header: "x-api-key" },
      request({ query, config }) {
        const mode = config.contentMode === TEXT ? TEXT : HIGHLIGHTS;
        const body = {
          query,
          type: String(config.searchType || "auto"),
          numResults: clampCount(config.numResults, 5, 1, 25),
          contents:
            mode === TEXT
              ? { text: { maxCharacters: clampCount(config.maxCharacters, 2000, 200, 20000) } }
              : { highlights: true }
        };
        const domains = domainList(config.includeDomains);
        if (domains.length > 0) {
          body.includeDomains = domains;
        }
        return { url: "https://api.exa.ai/search", method: "POST", body };
      },
      parse(payload) {
        const items = Array.isArray(payload.results) ? payload.results : [];
        return items.map((item) => ({
          title: item.title,
          url: item.url,
          date: item.publishedDate ? String(item.publishedDate).slice(0, 10) : "",
          source: item.author,
          score: typeof item.score === "number" ? item.score.toFixed(2) : undefined,
          // highlights 模式给摘录数组，text 模式给整页正文；渲染器优先用摘录。
          highlights: item.highlights,
          content: item.text
        }));
      }
    })
  ]
});

/** 逗号分隔的域名列表 → 去空、去协议、去路径的域名数组。 */
function domainList(value) {
  const raw = String(value || "").split(",");
  const out = [];
  for (let index = 0; index < raw.length; index += 1) {
    const domain = collapseText(raw[index])
      .replace(/^https?:\/\//, "")
      .replace(/\/.*$/, "");
    if (domain !== "") {
      out.push(domain);
    }
  }
  return out;
}
