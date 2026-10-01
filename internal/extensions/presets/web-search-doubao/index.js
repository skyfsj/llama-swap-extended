// Volcengine Networked Search (联网搜索) web API.
//
// The official doc page (docs.volcengine.com/docs/Networkedsearch/...) is a
// JavaScript-rendered application that could not be fetched as text, so the
// contract below was verified against the open-source Volcengine clients that
// ship this exact API:
//   - volcengine/mcp-server -> server/mcp_server_askecho_search_infinity
//   - bytedance/agentkit-samples -> skills/byted-web-search
//
// Endpoint (API-key auth):
//   POST https://open.feedcoopapi.com/search_api/web_search
//   Authorization: Bearer <api-key>
//   Content-Type: application/json
//   Body: {Query, SearchType:"web", Count, TimeRange?, Filter?, QueryControl?, ContentFormats?}
// Response envelope:
//   {Result: {ResultCount, TimeCost, WebResults: [{SortId, Title, SiteName,
//    Url, Summary, Snippet, AuthInfoDes, Content, PublishTime, LogoUrl}]}}
//
// The console also issues Volcengine AK/SK pairs that sign requests to
// https://mercury.volcengineapi.com/?Action=WebSearch&Version=2025-01-01
// (service volc_torchlight_api, HMAC-SHA256). That path is NOT implemented
// here: the extension sandbox has no crypto primitives.
//
// Put your key in the API Key setting (declared secret below, no default);
// never hard-code it in this file.

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "API key from the Volcengine networked-search (联网搜索) console. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to return. The API accepts 1-50 for web search.",
    default: 5,
    min: 1,
    max: 50
  },
  timeRange: {
    type: "select",
    label: "Time range",
    hint: "Restrict results by recency. 'any' leaves the parameter out.",
    options: ["any", "OneDay", "OneWeek", "OneMonth", "OneYear"],
    default: "any"
  },
  contentFormat: {
    type: "select",
    label: "Content format",
    hint: "Format used for returned page content, when content is requested.",
    options: ["any", "text", "markdown"],
    default: "any"
  }
};

// Known API error codes with actionable hints.
const ERROR_HINTS = {
  "10400": "parameter error; check the query and parameters",
  "10402": "unsupported search type; only 'web' and 'image' exist",
  "10403": "key or permission error; confirm the API key comes from the networked-search console",
  "10406": "free quota exhausted; check the account balance",
  "10407": "no usable free strategy on this account",
  "10500": "service internal error; retry later",
  "700429": "rate limited on the free link; slow down and retry",
  "100013": "sub-account is not granted TorchlightApiFullAccess"
};

function clampCount(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 5;
  if (count > 50) return 50;
  return count;
}

// Pull the result list out of the response, tolerating the shape variants the
// service and its proxies use.
function extractResults(payload) {
  if (!payload || typeof payload !== "object") return [];
  const candidates = [];
  if (payload.Result && typeof payload.Result === "object") {
    if (Array.isArray(payload.Result.WebResults)) candidates.push(payload.Result.WebResults);
    if (Array.isArray(payload.Result.Results)) candidates.push(payload.Result.Results);
  }
  if (Array.isArray(payload.WebResults)) candidates.push(payload.WebResults);
  if (Array.isArray(payload.results)) candidates.push(payload.results);
  if (Array.isArray(payload.search_result)) candidates.push(payload.search_result);
  for (let index = 0; index < candidates.length; index += 1) {
    if (candidates[index].length > 0) return candidates[index];
  }
  return [];
}

function formatResults(results, query) {
  const lines = ["Web results for: " + query, ""];
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    const title = item.Title || item.title || "(untitled)";
    lines.push((index + 1) + ". " + title);
    const meta = [];
    if (item.SiteName || item.media) meta.push(item.SiteName || item.media);
    if (item.AuthInfoDes) meta.push(item.AuthInfoDes);
    if (item.PublishTime || item.publish_date) meta.push(String(item.PublishTime || item.publish_date));
    if (meta.length > 0) lines.push("   " + meta.join(" | "));
    const url = item.Url || item.link || item.url;
    if (url) lines.push("   " + url);
    const summary = item.Summary || item.Snippet || item.content || "";
    if (summary) lines.push("   " + String(summary).slice(0, 600));
    lines.push("");
  }
  return lines.join("\n").slice(0, 8000);
}

function apiErrorMessage(payload) {
  if (!payload || typeof payload !== "object") return "";
  if (payload.error) {
    const error = payload.error;
    if (typeof error === "string") return error;
    const code = error.code !== undefined ? String(error.code) : "";
    const message = error.message || error.Message || "";
    const hint = ERROR_HINTS[code] ? " (" + ERROR_HINTS[code] + ")" : "";
    return code ? code + " " + message + hint : message + hint;
  }
  if (typeof payload.ResponseMetadata === "object" && payload.ResponseMetadata.Error) {
    return payload.ResponseMetadata.Error.Message || JSON.stringify(payload.ResponseMetadata.Error);
  }
  return "";
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "web_search",
      description: "Search the live web through the Volcengine (Doubao) networked search API. Returns numbered results with title, source, URL and summary.",
      parameters: {
        type: "object",
        properties: {
          query: { type: "string", description: "Search query, 1-100 characters." }
        },
        required: ["query"]
      }
    }
  }],

  async onToolCall(ctx, call) {
    if (call.name !== "web_search") return undefined;
    const query = String(call.arguments.query || "").trim();
    if (query.length === 0) {
      throw new Error("web_search: query must not be empty");
    }
    if (query.length > 100) {
      throw new Error("web_search: query must be at most 100 characters (got " + query.length + ")");
    }
    const apiKey = String(ctx.config.apiKey || "").trim();
    if (apiKey.length === 0) {
      throw new Error("web_search: set the API Key in the extension settings");
    }

    const body = {
      Query: query,
      SearchType: "web",
      Count: clampCount(ctx.config.maxResults)
    };
    if (ctx.config.timeRange && ctx.config.timeRange !== "any") {
      body.TimeRange = ctx.config.timeRange;
    }
    if (ctx.config.contentFormat && ctx.config.contentFormat !== "any") {
      body.ContentFormats = ctx.config.contentFormat;
    }

    const response = await ctx.http.fetch("https://open.feedcoopapi.com/search_api/web_search", {
      method: "POST",
      headers: {
        "Authorization": "Bearer " + apiKey,
        "Content-Type": "application/json"
      },
      body: JSON.stringify(body)
    });

    if (!response.ok) {
      throw new Error("Volcengine networked search failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error("Volcengine networked search returned a response that is not JSON");
    }
    const error = apiErrorMessage(payload);
    if (error) {
      throw new Error("Volcengine networked search error: " + error);
    }

    const results = extractResults(payload);
    if (results.length === 0) {
      return { content: "No web results found for: " + query };
    }
    return { content: formatResults(results, query) };
  }
};
