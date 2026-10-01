// OpenAI built-in web search.
//
// This preset makes no HTTP call of its own: it adds OpenAI's server-side
// web_search tool to the outgoing request and the provider performs the
// search. Because of that it declares no networkHosts and needs no API key.
//
// Tool shapes (as documented by OpenAI):
//   chat.completions: tools: [{ "type": "web_search" }]
//   responses:        tools: [{ "type": "web_search",
//                               "search_context_size": "low|medium|high",
//                               "user_location": {"type": "approximate",
//                                                 "country": "US", ...} }]
//
// search_context_size is honored only on the Responses API; the
// chat.completions tool entry takes no arguments, so the setting is ignored
// there. A tool that is already present in request.tools is never
// duplicated, so a client that asks for web search itself keeps its entry.

export const settings = {
  searchContextSize: {
    type: "select",
    label: "Search context size",
    hint: "Responses API only; chat.completions ignores it.",
    options: ["medium", "low", "high"],
    default: "medium"
  },
  locationCountry: {
    type: "string",
    label: "Approximate country",
    hint: "Two-letter country code (e.g. US) for an approximate user_location on the Responses API. Leave empty to omit."
  }
};

function isWebSearchTool(entry) {
  return !!entry && typeof entry === "object" && entry.type === "web_search";
}

function inject(ctx, request) {
  if (!request || typeof request !== "object") return undefined;
  if (!Array.isArray(request.tools)) request.tools = [];

  const present = request.tools.some(isWebSearchTool);
  if (present) {
    // The client already asked for web search; leave its tool entry alone.
    ctx.log.debug("openai-web-search: request already carries a web_search tool; left unchanged");
    return undefined;
  }

  if (ctx.endpoint === "chat.completions") {
    request.tools.push({ type: "web_search" });
  } else if (ctx.endpoint === "responses") {
    const tool = { type: "web_search" };
    const size = ctx.config.searchContextSize;
    if (size) tool.search_context_size = size;
    const country = String(ctx.config.locationCountry || "").trim().toUpperCase();
    if (country) {
      tool.user_location = { type: "approximate", country: country };
    }
    request.tools.push(tool);
  } else {
    return undefined;
  }
  return request;
}

export default {
  onRequest(ctx, request) {
    return inject(ctx, request);
  }
};
