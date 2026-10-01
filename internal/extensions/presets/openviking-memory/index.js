// OpenViking memory manager (github.com/volcengine/OpenViking).
//
// The HTTP API contract below was verified against the official docs at
// docs.openviking.ai (api/01-overview, api/05-sessions, api/06-retrieval,
// api/16-memory):
//
//   Base URL is user-supplied (docs use http://localhost:1933, but the
//   extension runtime blocks loopback/private IPs, so configure a public
//   host and add it to the manifest's networkHosts).
//   Routes live under /api/v1/.
//   Auth: Authorization: Bearer <api-key> (or X-API-Key); with no key
//   configured the server runs unauthenticated (shared domain).
//   Envelope: {"status": "ok", "result": {...}} or a non-2xx
//   {"status": "error", "error": {code, message}}.
//
// Writes: POST /api/v1/sessions, POST /api/v1/sessions/{id}/messages,
// POST /api/v1/sessions/{id}/commit (memory extraction is asynchronous and
// returns {status, task_id, archive_uri, archived}).
// Reads:  POST /api/v1/search/search with mode="context" (injection-ready
// block) or POST /api/v1/search/find (raw list mode), both answering
// {memories, resources, skills, total} inside result.
//
// ASSUMED, adapt if your deployment differs:
//   - Session-scoped writes: each remember creates its own session and
//     commits it, because the docs describe no direct memory-write endpoint.
//   - Tenancy: the server derives tenant identity from the API key, which is
//     what isolates memories per key. For extra safety on a keyless/shared
//     server the session id is namespaced with a hash of the key, so
//     sessions of different keys never share an id.
//   - search/search mode="context" is used for recall; mode="list" falls
//     back to search/find.

export const settings = {
  baseUrl: {
    type: "string",
    label: "Server URL",
    hint: "OpenViking server root, e.g. https://ov.example.com. Required for the tools to work and must be a public host; localhost is blocked by the runtime."
  },
  apiKey: {
    type: "string",
    label: "API Key",
    hint: "Optional. When empty the request is unauthenticated and lands in the server's shared domain.",
    secret: true
  },
  recallMode: {
    type: "select",
    label: "Recall mode",
    hint: "context returns an assembled block ready to inject; list returns raw matches.",
    options: ["context", "list"],
    default: "context"
  },
  maxResults: {
    type: "number",
    label: "Max results",
    hint: "How many memories to recall.",
    default: 5,
    min: 1,
    max: 50
  }
};

const MAX_OUTPUT_CHARS = 16000;

function normalizeBaseUrl(value) {
  let base = String(value || "").trim();
  if (base.length === 0) {
    throw new Error("the server URL setting is empty; configure it in the extension settings");
  }
  if (base.indexOf("http://") !== 0 && base.indexOf("https://") !== 0) {
    base = "https://" + base;
  }
  return base.replace(/\/+$/, "");
}

// Tenant namespace for sessions: a hash of the key, or the shared domain
// when no key is configured.
function keyNamespace(apiKey) {
  const key = apiKey ? String(apiKey) : "";
  if (key.length === 0) return "shared";
  let hash = 5381;
  for (let index = 0; index < key.length; index += 1) {
    hash = ((hash << 5) + hash + key.charCodeAt(index)) & 0x7fffffff;
  }
  return hash.toString(36);
}

function buildHeaders(apiKey) {
  const headers = { "Content-Type": "application/json" };
  if (apiKey && String(apiKey).length > 0) {
    headers["Authorization"] = "Bearer " + apiKey;
  }
  return headers;
}

// Unwrap the {status, result} envelope, throwing on the error shape.
function unwrap(payload, context) {
  if (payload && typeof payload === "object" && payload.status === "error") {
    const error = payload.error || {};
    const code = error.code ? error.code + " " : "";
    throw new Error(context + ": " + code + (error.message || JSON.stringify(error)));
  }
  if (!payload || typeof payload !== "object" || typeof payload.result !== "object") {
    throw new Error(context + ": unexpected response envelope");
  }
  return payload.result;
}

function requestJson(ctx, url, body, context) {
  return ctx.http.fetch(url, {
    method: "POST",
    headers: buildHeaders(ctx.config.apiKey),
    body: JSON.stringify(body)
  }).then(function (response) {
    if (!response.ok) {
      throw new Error(context + " failed with HTTP " + response.status);
    }
    const payload = response.json();
    if (!payload || typeof payload !== "object") {
      throw new Error(context + " returned a response that is not JSON");
    }
    return unwrap(payload, context);
  });
}

function formatContextMode(result) {
  if (result.rendered && String(result.rendered).trim().length > 0) {
    let rendered = String(result.rendered);
    if (rendered.length > MAX_OUTPUT_CHARS) {
      rendered = rendered.slice(0, MAX_OUTPUT_CHARS) + "\n... (truncated)";
    }
    return rendered;
  }
  const entries = Array.isArray(result.entries) ? result.entries : [];
  if (entries.length === 0) return "No memories were recalled.";
  const lines = ["Recalled memories:", ""];
  for (let index = 0; index < entries.length; index += 1) {
    const entry = entries[index] || {};
    lines.push((index + 1) + ". [" + (entry.category || "memory") + "] " + (entry.uri || "(no uri)"));
    if (typeof entry.score === "number") lines.push("   score " + entry.score.toFixed(3));
    if (entry.text) lines.push("   " + String(entry.text).slice(0, 800));
    lines.push("");
  }
  return lines.join("\n").slice(0, MAX_OUTPUT_CHARS);
}

function formatListMode(result) {
  const lines = [];
  let found = 0;
  const groups = [["memories", "memory"], ["resources", "resource"], ["skills", "skill"]];
  for (let group = 0; group < groups.length; group += 1) {
    const items = Array.isArray(result[groups[group][0]]) ? result[groups[group][0]] : [];
    if (items.length === 0) continue;
    lines.push(groups[group][1] + " matches:");
    for (let index = 0; index < items.length; index += 1) {
      const item = items[index] || {};
      lines.push("  " + (found + 1) + ". " + (item.uri || "(no uri)"));
      if (typeof item.score === "number") lines.push("     score " + item.score.toFixed(3));
      const text = item.abstract || item.overview || item.content || "";
      if (text) lines.push("     " + String(text).slice(0, 800));
      found += 1;
    }
    lines.push("");
  }
  if (found === 0) return "No memories were recalled.";
  return lines.join("\n").slice(0, MAX_OUTPUT_CHARS);
}

// remember(text): session-scoped write, then commit so the server extracts
// memories from the exchange.
function remember(ctx, text) {
  const base = normalizeBaseUrl(ctx.config.baseUrl);
  const sessionId = "lswap-" + keyNamespace(ctx.config.apiKey) + "-" + Date.now().toString(36);

  return requestJson(ctx, base + "/api/v1/sessions", { session_id: sessionId }, "session create")
    .then(function (created) {
      const id = (created && (created.session_id || created.id)) || sessionId;
      return requestJson(ctx, base + "/api/v1/sessions/" + encodeURIComponent(id) + "/messages", {
        role: "user",
        content: text
      }, "message add").then(function () {
        return requestJson(ctx, base + "/api/v1/sessions/" + encodeURIComponent(id) + "/commit", {
          keep_recent_count: 0,
          reset_context: false
        }, "session commit").then(function (committed) {
          ctx.log.debug("openviking: committed session " + id);
          const parts = ["Memory stored in session " + id + "."];
          if (committed && committed.task_id) parts.push("extraction task " + committed.task_id);
          if (committed && committed.archive_uri) parts.push("archive " + committed.archive_uri);
          return { content: parts.join(" ") };
        });
      });
    });
}

// recall(query): context-mode search by default, list mode on request.
function recall(ctx, query) {
  const base = normalizeBaseUrl(ctx.config.baseUrl);
  const limit = clampLimit(ctx.config.maxResults);
  if (String(ctx.config.recallMode || "context") === "list") {
    return requestJson(ctx, base + "/api/v1/search/find", { query: query, limit: limit }, "search find")
      .then(function (result) {
        return { content: formatListMode(result) };
      });
  }
  return requestJson(ctx, base + "/api/v1/search/search", {
    query: query,
    mode: "context",
    limit: limit
  }, "search search").then(function (result) {
    return { content: formatContextMode(result) };
  });
}

function clampLimit(value) {
  const count = Math.floor(Number(value));
  if (!isFinite(count) || count < 1) return 5;
  if (count > 50) return 50;
  return count;
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "remember",
      description: "Store a fact, preference or experience in the OpenViking memory for later recall. Writes are isolated per API key.",
      parameters: {
        type: "object",
        properties: {
          text: { type: "string", description: "What to remember, as a self-contained statement." }
        },
        required: ["text"]
      }
    }
  }, {
    type: "function",
    execution: "server",
    function: {
      name: "recall",
      description: "Recall relevant memories from the OpenViking memory for a query.",
      parameters: {
        type: "object",
        properties: {
          query: { type: "string", description: "What to look up in memory." }
        },
        required: ["query"]
      }
    }
  }],

  async onToolCall(ctx, call) {
    if (call.name === "remember") {
      const text = String(call.arguments.text || "").trim();
      if (text.length === 0) throw new Error("remember: text must not be empty");
      return remember(ctx, text);
    }
    if (call.name === "recall") {
      const query = String(call.arguments.query || "").trim();
      if (query.length === 0) throw new Error("recall: query must not be empty");
      return recall(ctx, query);
    }
    return undefined;
  }
};
