// Tavily Search API.
//
//   POST https://api.tavily.com/search
//   Content-Type: application/json
//   Body: {api_key, query, max_results, search_depth, include_answer}
//         search_depth: "basic" | "advanced"
//
// Response: {query, answer?, results: [{title, url, content, score,
//            raw_content?}], response_time}.
//
// Tavily sends the API key inside the JSON body by design. The key belongs in
// the API Key setting below (declared secret, no default); never hard-code it
// in this file.

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
  }
};

function clampCount(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 5;
  if (count > 20) return 20;
  return count;
}

function formatResults(payload, query) {
  const lines = ["Tavily results for: " + query, ""];
  if (payload.answer) {
    lines.push("Answer: " + payload.answer);
    lines.push("");
  }
  const results = Array.isArray(payload.results) ? payload.results : [];
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    lines.push((index + 1) + ". " + (item.title || "(untitled)"));
    if (item.url) lines.push("   " + item.url);
    if (typeof item.score === "number") lines.push("   score " + item.score.toFixed(3));
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
      description: "Search the web with Tavily and return an optional short answer plus numbered results with title, URL, score and content.",
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

    const body = {
      api_key: apiKey,
      query: query,
      max_results: clampCount(ctx.config.maxResults),
      search_depth: ctx.config.searchDepth === "advanced" ? "advanced" : "basic",
      include_answer: ctx.config.includeAnswer === true
    };

    const response = await ctx.http.fetch("https://api.tavily.com/search", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify(body)
    });
    if (!response.ok) {
      throw new Error("Tavily search failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error("Tavily search returned a response that is not JSON");
    }

    const results = Array.isArray(payload.results) ? payload.results : [];
    if (results.length === 0 && !payload.answer) {
      return { content: "No Tavily results found for: " + query };
    }
    return { content: formatResults(payload, query) };
  }
};
