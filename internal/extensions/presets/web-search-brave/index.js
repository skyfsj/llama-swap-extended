// Brave Search API (web endpoint).
//
//   GET https://api.search.brave.com/v1/web/search
//       ?q=...&count=...&country=...&safesearch=...
//   Headers: Accept: application/json
//            X-Subscription-Token: <your-token>
//
// Response: {query: {...}, web: {results: [{title, url, description, age,
//            profile?...}]}}. Errors return 4xx/5xx with a JSON body holding
// error information; they are surfaced with the status code.
//
// The subscription token belongs in the API Key setting below (declared
// secret, no default); never hard-code it in this file.

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
    options: ["US", "GB", "DE", "FR", "JP", "IN", "BR", "AU", "CA"],
    default: "US"
  },
  safeSearch: {
    type: "select",
    label: "Safe search",
    options: ["moderate", "strict", "off"]
  }
};

function clampCount(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 10;
  if (count > 20) return 20;
  return count;
}

function formatResults(results, query) {
  const lines = ["Brave results for: " + query, ""];
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    lines.push((index + 1) + ". " + (item.title || "(untitled)"));
    if (item.url) lines.push("   " + item.url);
    if (item.age) lines.push("   " + item.age);
    if (item.description) lines.push("   " + String(item.description));
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
      description: "Search the web with Brave Search and return numbered results with title, URL, age and description.",
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
      "q=" + encodeURIComponent(query),
      "count=" + clampCount(ctx.config.maxResults),
      "safesearch=" + encodeURIComponent(ctx.config.safeSearch || "moderate")
    ];
    if (ctx.config.country) {
      params.push("country=" + encodeURIComponent(ctx.config.country));
    }
    const url = "https://api.search.brave.com/v1/web/search?" + params.join("&");

    const response = await ctx.http.fetch(url, {
      headers: {
        "Accept": "application/json",
        "X-Subscription-Token": apiKey
      }
    });
    if (!response.ok) {
      throw new Error("Brave search failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error("Brave search returned a response that is not JSON");
    }

    const web = payload.web;
    const results = web && Array.isArray(web.results) ? web.results : [];
    if (results.length === 0) {
      return { content: "No Brave results found for: " + query };
    }
    return { content: formatResults(results, query) };
  }
};
