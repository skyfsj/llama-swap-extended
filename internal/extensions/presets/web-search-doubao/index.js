// 火山引擎「联网搜索」Web API。
//
//   POST https://open.feedcoopapi.com/search_api/web_search
//   Authorization: Bearer <api-key>
//   Content-Type: application/json
//   请求体：{Query, SearchType: "web"|"image", Count, TimeRange?, ContentFormats?}
//     TimeRange: OneDay | OneWeek | OneMonth | OneYear
//     ContentFormats: text | markdown
//
//   响应形态在不同代理/版本间有差异，结果数组可能位于
//   Result.WebResults / Result.Results / WebResults / results / search_result，
//   业务错误可能在 error 或 ResponseMetadata.Error 里，所以解析要防御式。
//
// 工具名带 doubao 后缀：多个搜索器可同时启用。

import { clampCount, collapseText, defineExtension, searchTool } from "@llama-swap/extension";

// 服务端错误码的中文释义，便于模型与运维判断该重试还是该改配置。
const ERROR_HINTS = {
  "10400": "参数错误，检查查询与参数",
  "10402": "不支持的搜索类型，只支持 web 与 image",
  "10403": "密钥或权限错误，确认 API Key 来自联网搜索控制台",
  "10406": "免费额度已用尽，检查账户余额",
  "10407": "该账户没有可用的免策略",
  "10500": "服务内部错误，稍后重试",
  "700429": "免费链路限流，降低频率后重试",
  "100013": "子账号未被授予 TorchlightApiFullAccess"
};

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "Volcengine networked-search API key (from the 联网搜索 console), sent as a Bearer token. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to request (Count). The API accepts up to 50.",
    default: 5,
    min: 1,
    max: 50
  },
  timeRange: {
    type: "select",
    label: "Time range",
    hint: "Restrict results to a recent window (TimeRange).",
    options: ["any", "OneDay", "OneWeek", "OneMonth", "OneYear"],
    default: "any"
  },
  contentFormat: {
    type: "select",
    label: "Content format",
    hint: "Ask the service for plain text or Markdown bodies (ContentFormats).",
    options: ["any", "text", "markdown"],
    default: "any"
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

/** 从响应里取出结果数组，兼容服务端与各代理的形状差异。 */
function extractResults(payload) {
  if (!payload || typeof payload !== "object") {
    return [];
  }
  const candidates = [];
  if (payload.Result && typeof payload.Result === "object") {
    if (Array.isArray(payload.Result.WebResults)) {
      candidates.push(payload.Result.WebResults);
    }
    if (Array.isArray(payload.Result.Results)) {
      candidates.push(payload.Result.Results);
    }
  }
  if (Array.isArray(payload.WebResults)) {
    candidates.push(payload.WebResults);
  }
  if (Array.isArray(payload.results)) {
    candidates.push(payload.results);
  }
  if (Array.isArray(payload.search_result)) {
    candidates.push(payload.search_result);
  }
  for (let index = 0; index < candidates.length; index += 1) {
    if (candidates[index].length > 0) {
      return candidates[index];
    }
  }
  return [];
}

/** 业务错误可能出现在两个位置，统一抽出来。 */
function businessError(payload) {
  if (!payload || typeof payload !== "object") {
    return "";
  }
  if (payload.error) {
    const error = payload.error;
    if (typeof error === "string") {
      return error;
    }
    const code = error.code !== undefined ? String(error.code) : "";
    const message = error.message || error.Message || "";
    const hint = ERROR_HINTS[code] ? "（" + ERROR_HINTS[code] + "）" : "";
    return code ? code + " " + message + hint : message + hint;
  }
  if (payload.ResponseMetadata && payload.ResponseMetadata.Error) {
    const error = payload.ResponseMetadata.Error;
    return error.Message || JSON.stringify(error);
  }
  return "";
}

export default defineExtension({
  tools: [
    searchTool({
      name: "web_search_doubao",
      title: "Web",
      description:
        "Search the live web through the Volcengine (Doubao) networked search API. Returns numbered results with title, source, URL and summary.",
      // 该服务对查询长度有硬限制。
      maxQueryLength: 100,
      credential: { setting: "apiKey", label: "API Key", bearer: true },
      request({ query, config }) {
        const body = {
          Query: query,
          SearchType: "web",
          Count: clampCount(config.maxResults, 5, 1, 50)
        };
        if (config.timeRange && config.timeRange !== "any") {
          body.TimeRange = config.timeRange;
        }
        if (config.contentFormat && config.contentFormat !== "any") {
          body.ContentFormats = config.contentFormat;
        }
        return { url: "https://open.feedcoopapi.com/search_api/web_search", method: "POST", body };
      },
      parse(payload) {
        const error = businessError(payload);
        if (error) {
          throw new Error("Volcengine 联网搜索错误：" + error);
        }
        const items = extractResults(payload);
        return items.map((item) => ({
          // 字段名大小写在不同版本间不一致，逐个兜底。
          title: item.Title || item.title,
          url: item.Url || item.url || item.link,
          source: item.SiteName || item.media,
          date: item.PublishTime || item.publish_date ? String(item.PublishTime || item.publish_date) : "",
          meta: item.AuthInfoDes ? [collapseText(item.AuthInfoDes)] : undefined,
          summary: String(item.Summary || item.Snippet || item.content || "").slice(0, 600)
        }));
      }
    })
  ]
});
