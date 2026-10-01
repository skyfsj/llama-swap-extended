// Generic RAG / vector-search connector.
//
// Calls a user-configured JSON API:
//   POST {baseUrl}{queryPath}
//   Content-Type: application/json
//   Body: {query, top_k, collection?}
// and returns the JSON response to the model verbatim.
//
// The expected contract is deliberately generic: field names differ between
// services (top_k vs k, collection vs namespace). If your API expects other
// names, adjust buildBody below. The runtime only permits the hosts listed
// in the manifest's networkHosts, and it rejects loopback, private and
// reserved addresses - so the API must be reachable on a public host. Add
// your host in the manifest panel (Permissions -> networkHosts) before use.
//
// The API key, when used, is sent per authStyle and is never logged.

export const settings = {
  baseUrl: {
    type: "string",
    label: "Base URL",
    hint: "API root of the search service, e.g. https://my-rag.example.com. Required for the tool to work and must be a public host; fill it in after installing the preset.",
    required: true
  },
  queryPath: {
    type: "string",
    label: "Query path",
    hint: "Path appended to the base URL, e.g. /query or /v1/search.",
    default: "/query"
  },
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "Optional; sent per the auth style setting. Leave empty for keyless services.",
    secret: true
  },
  authStyle: {
    type: "select",
    label: "Auth style",
    hint: "How the API key is attached. Ignored when no key is set.",
    options: ["bearer", "x-api-key", "query"],
    default: "bearer"
  },
  collection: {
    type: "string",
    label: "Collection",
    hint: "Collection / index / namespace to search. Leave empty to omit the field."
  },
  topK: {
    type: "number",
    label: "Top K",
    hint: "How many matches to request (top_k).",
    default: 5,
    min: 1,
    max: 100
  }
};

function normalizeBaseUrl(value) {
  let base = String(value || "").trim();
  if (base.length === 0) {
    throw new Error("rag_query: the base URL setting is empty; configure it in the extension settings");
  }
  if (base.indexOf("http://") !== 0 && base.indexOf("https://") !== 0) {
    base = "https://" + base;
  }
  return base.replace(/\/+$/, "");
}

function buildBody(config, query) {
  const body = { query: query, top_k: clampTopK(config.topK) };
  if (config.collection && String(config.collection).trim().length > 0) {
    body.collection = String(config.collection).trim();
  }
  return body;
}

function clampTopK(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 5;
  if (count > 100) return 100;
  return count;
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "rag_query",
      description: "Query the configured RAG / vector-search HTTP API and return the JSON matches. Use it to retrieve private knowledge before answering.",
      parameters: {
        type: "object",
        properties: {
          query: { type: "string", description: "Natural-language query sent to the search API." }
        },
        required: ["query"]
      }
    }
  }],

  async onToolCall(ctx, call) {
    if (call.name !== "rag_query") return undefined;
    const query = String(call.arguments.query || "").trim();
    if (query.length === 0) {
      throw new Error("rag_query: query must not be empty");
    }

    const base = normalizeBaseUrl(ctx.config.baseUrl);
    const path = String(ctx.config.queryPath || "/query").trim();
    if (path.length === 0 || path.charAt(0) !== "/") {
      throw new Error("rag_query: the query path must start with '/'");
    }
    let url = base + path;

    const headers = { "Content-Type": "application/json" };
    const key = ctx.config.apiKey ? String(ctx.config.apiKey) : "";
    if (key) {
      const style = String(ctx.config.authStyle || "bearer");
      if (style === "x-api-key") {
        headers["X-API-Key"] = key;
      } else if (style === "query") {
        url += (url.indexOf("?") === -1 ? "?" : "&") + "api_key=" + encodeURIComponent(key);
      } else {
        headers["Authorization"] = "Bearer " + key;
      }
    }

    // Log target and result, never the key.
    ctx.log.debug("rag_query -> " + base + " " + JSON.stringify(buildBody(ctx.config, query)));

    const response = await ctx.http.fetch(url, {
      method: "POST",
      headers: headers,
      body: JSON.stringify(buildBody(ctx.config, query))
    });
    if (!response.ok) {
      throw new Error("RAG query failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error("RAG query returned a response that is not JSON");
    }
    if (payload.error || payload.detail) {
      const message = payload.error ? (payload.error.message || JSON.stringify(payload.error)) : payload.detail;
      throw new Error("RAG query error: " + message);
    }

    const rendered = JSON.stringify(payload);
    if (rendered.length > 16000) {
      return { content: rendered.slice(0, 16000) + "\n... (truncated)" };
    }
    return { content: rendered };
  }
};
