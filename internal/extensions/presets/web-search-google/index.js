// Google search via SerpApi.
//
//   GET https://serpapi.com/search.json
//       ?engine=google&api_key=...&q=...&num=...&hl=...&gl=...
//
// Response: {organic_results: [{title, link, snippet, position, date}],
//            answer_box?, search_information?, error?}.
// SerpApi sends the key as a query parameter by design; this file never logs
// the full URL so the key cannot leak into the console. Known-answer
// responses (answer_box) are reported when present.

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
    default: "en"
  },
  gl: {
    type: "string",
    label: "Country (gl)",
    hint: "Two-letter country code such as 'us' or 'cn'. Leave empty to omit.",
    default: "us"
  }
};

function clampCount(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 10;
  if (count > 100) return 100;
  return count;
}

function formatResults(payload, query) {
  const lines = ["Google results for: " + query, ""];
  const answer = payload.answer_box;
  if (answer && (answer.answer || answer.snippet)) {
    lines.push("Answer box: " + (answer.answer || answer.snippet));
    if (answer.link) lines.push("  " + answer.link);
    lines.push("");
  }
  const results = Array.isArray(payload.organic_results) ? payload.organic_results : [];
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    lines.push((index + 1) + ". " + (item.title || "(untitled)"));
    if (item.link) lines.push("   " + item.link);
    if (item.date) lines.push("   " + item.date);
    if (item.snippet) lines.push("   " + String(item.snippet));
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
      description: "Search Google through SerpApi and return numbered organic results with title, link, date and snippet.",
      parameters: {
        type: "object",
        properties: {
          query: { type: "string", description: "Search query." }
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

    const params = [
      "engine=google",
      "q=" + encodeURIComponent(query),
      "num=" + clampCount(ctx.config.maxResults),
      "api_key=" + encodeURIComponent(apiKey)
    ];
    if (ctx.config.hl) params.push("hl=" + encodeURIComponent(ctx.config.hl));
    if (ctx.config.gl) params.push("gl=" + encodeURIComponent(ctx.config.gl));
    const url = "https://serpapi.com/search.json?" + params.join("&");

    const response = await ctx.http.fetch(url);
    if (!response.ok) {
      throw new Error("SerpApi search failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error("SerpApi search returned a response that is not JSON");
    }
    if (payload.error) {
      throw new Error("SerpApi search error: " + (payload.error.message || payload.error));
    }

    const results = Array.isArray(payload.organic_results) ? payload.organic_results : [];
    if (results.length === 0 && !payload.answer_box) {
      return { content: "No Google results found for: " + query };
    }
    return { content: formatResults(payload, query) };
  }
};
