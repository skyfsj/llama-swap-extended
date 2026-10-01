// Microsoft Bing Web Search v7.
//
//   GET https://api.bing.microsoft.com/v7.0/search
//       ?q=...&count=...&mkt=...&responseFilter=Webpages
//   Header: Ocp-Apim-Subscription-Key: <your-key>
//
// Response: {webPages: {value: [{name, url, snippet, displayUrl, ...}]}}.
// When there are no results the API omits webPages entirely, so check it
// defensively. The subscription key belongs in the API Key setting below
// (declared secret, no default); never hard-code it in this file.

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
    default: "en-US"
  },
  safeSearch: {
    type: "select",
    label: "Safe search",
    options: ["Moderate", "Strict", "Off"]
  }
};

function clampCount(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 10;
  if (count > 50) return 50;
  return count;
}

function formatResults(results, query) {
  const lines = ["Bing results for: " + query, ""];
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    lines.push((index + 1) + ". " + (item.name || "(untitled)"));
    if (item.url) lines.push("   " + item.url);
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
      description: "Search the web with Bing and return numbered results with title, URL and snippet.",
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
      "responseFilter=Webpages",
      "safeSearch=" + encodeURIComponent(ctx.config.safeSearch || "Moderate")
    ];
    if (ctx.config.market) {
      params.push("mkt=" + encodeURIComponent(ctx.config.market));
    }
    const url = "https://api.bing.microsoft.com/v7.0/search?" + params.join("&");

    const response = await ctx.http.fetch(url, {
      headers: { "Ocp-Apim-Subscription-Key": apiKey }
    });
    if (!response.ok) {
      throw new Error("Bing search failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error("Bing search returned a response that is not JSON");
    }

    const page = payload.webPages;
    const results = page && Array.isArray(page.value) ? page.value : [];
    if (results.length === 0) {
      return { content: "No Bing results found for: " + query };
    }
    return { content: formatResults(results, query) };
  }
};
