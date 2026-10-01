export const settings = {
  language: { type: "select", label: "Language", options: ["en", "zh"], default: "en" }
};

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "web_search",
      description: "Search Wikipedia for factual background",
      parameters: {
        type: "object",
        properties: { query: { type: "string" } },
        required: ["query"]
      }
    }
  }],
  async onToolCall(ctx, call) {
    if (call.name !== "web_search") return { content: "Unknown tool" };
    // A declared setting arrives as ctx.config, with its default filled in.
    const language = ctx.config.language || "en";
    const url = "https://" + language + ".wikipedia.org/w/api.php?action=opensearch&format=json&search=" + encodeURIComponent(call.arguments.query);
    const response = await ctx.http.fetch(url);
    if (!response.ok) throw new Error("Search request failed: " + response.status);
    return { content: await response.text() };
  }
};
