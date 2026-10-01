// Zhipu GLM (智谱) web search tool API.
//
// Contract verified from the official doc at
// https://docs.bigmodel.cn/api-reference/工具-api/网络搜索:
//
//   POST https://open.bigmodel.cn/api/paas/v4/web_search
//   Authorization: Bearer <api-key>
//   Content-Type: application/json
//   Body: {search_query, search_engine, search_intent, count?, content_size?,
//          search_recency_filter?, search_domain_filter?, request_id?, user_id?}
//   search_engine: search_std | search_pro | search_pro_sogou | search_pro_quark
//   search_recency_filter: oneDay | oneWeek | oneMonth | oneYear | noLimit
//   content_size: medium | high
//
// Response: {id, created, request_id,
//            search_intent: [{query, intent, keywords}],
//            search_result: [{title, content, link, media, icon, refer,
//                             publish_date}]}
// Errors come back as {error: {code, message}}.
//
// The API key belongs in the API Key setting below (declared secret, no
// default); never hard-code it in this file.

export const settings = {
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "API key from the Zhipu bigmodel platform. Required for the tool to work; fill it in after installing the preset.",
    secret: true,
    required: true
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many results to request (count). Zhipu accepts 1-50.",
    default: 10,
    min: 1,
    max: 50
  },
  searchEngine: {
    type: "select",
    label: "Search engine",
    hint: "Underlying search backend. search_pro is the general default.",
    options: ["search_pro", "search_std", "search_pro_sogou", "search_pro_quark"]
  },
  contentSize: {
    type: "select",
    label: "Content size",
    hint: "medium returns a summary per result; high returns more context.",
    options: ["medium", "high"],
    default: "medium"
  },
  recencyFilter: {
    type: "select",
    label: "Recency filter",
    options: ["noLimit", "oneDay", "oneWeek", "oneMonth", "oneYear"],
    default: "noLimit"
  },
  intentRecognition: {
    type: "boolean",
    label: "Intent recognition",
    hint: "Let the service rewrite the query first (search_intent=true).",
    default: false
  }
};

function clampCount(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 10;
  if (count > 50) return 50;
  return count;
}

function formatResults(payload, query) {
  const lines = ["Zhipu web search results for: " + query, ""];
  const intents = Array.isArray(payload.search_intent) ? payload.search_intent : [];
  for (let index = 0; index < intents.length; index += 1) {
    const intent = intents[index] || {};
    if (intent.keywords) lines.push("Rewritten keywords: " + intent.keywords);
  }
  if (intents.length > 0) lines.push("");
  const results = Array.isArray(payload.search_result) ? payload.search_result : [];
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    lines.push((index + 1) + ". " + (item.title || "(untitled)"));
    const meta = [];
    if (item.media) meta.push(item.media);
    if (item.publish_date) meta.push(String(item.publish_date));
    if (item.refer !== undefined) meta.push("refer " + item.refer);
    if (meta.length > 0) lines.push("   " + meta.join(" | "));
    if (item.link) lines.push("   " + item.link);
    if (item.content) lines.push("   " + String(item.content));
    lines.push("");
  }
  return lines.join("\n").slice(0, 8000);
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "web_search",
      description: "Search the web with the Zhipu GLM web search API and return numbered results with title, source, link and content.",
      parameters: {
        type: "object",
        properties: {
          query: { type: "string", description: "Search query; keep it under 70 characters." }
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
    const apiKey = String(ctx.config.apiKey || "").trim();
    if (apiKey.length === 0) {
      throw new Error("web_search: set the API Key in the extension settings");
    }

    const body = {
      search_query: query,
      search_engine: ctx.config.searchEngine || "search_pro",
      search_intent: ctx.config.intentRecognition === true,
      count: clampCount(ctx.config.maxResults),
      content_size: ctx.config.contentSize || "medium",
      search_recency_filter: ctx.config.recencyFilter || "noLimit"
    };

    const response = await ctx.http.fetch("https://open.bigmodel.cn/api/paas/v4/web_search", {
      method: "POST",
      headers: {
        "Authorization": "Bearer " + apiKey,
        "Content-Type": "application/json"
      },
      body: JSON.stringify(body)
    });
    if (!response.ok) {
      throw new Error("Zhipu web search failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error("Zhipu web search returned a response that is not JSON");
    }
    if (payload.error) {
      const error = payload.error;
      throw new Error("Zhipu web search error: " + (error.code ? error.code + " " : "") + (error.message || JSON.stringify(error)));
    }

    const results = Array.isArray(payload.search_result) ? payload.search_result : [];
    if (results.length === 0) {
      return { content: "No Zhipu search results for: " + query };
    }
    return { content: formatResults(payload, query) };
  }
};
