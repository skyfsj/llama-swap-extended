var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

// internal/extensions/sdk/extension.ts
var extension_exports = {};
__export(extension_exports, {
  DEFAULT_STATUS_ADVICE: () => DEFAULT_STATUS_ADVICE,
  apiErrorMessage: () => apiErrorMessage,
  clampCount: () => clampCount,
  collapseText: () => collapseText,
  createToolRegistry: () => createToolRegistry,
  defineExtension: () => defineExtension,
  defineTool: () => defineTool,
  fetchTool: () => fetchTool,
  searchTool: () => searchTool
});
module.exports = __toCommonJS(extension_exports);
var TOOL_MARKER = "__llamaSwapTool";
function defineTool(spec) {
  var _a;
  if (!spec || typeof spec !== "object") {
    throw new Error("defineTool \u9700\u8981\u4E00\u4E2A\u5BF9\u8C61\u53C2\u6570");
  }
  if (typeof spec.name !== "string" || spec.name.trim() === "") {
    throw new Error("defineTool \u7F3A\u5C11\u5DE5\u5177\u540D name");
  }
  if (typeof spec.handler !== "function") {
    throw new Error(`defineTool(${spec.name}) \u7F3A\u5C11 handler \u51FD\u6570`);
  }
  const tool = {
    ...spec,
    execution: (_a = spec.execution) != null ? _a : "server",
    [TOOL_MARKER]: true
  };
  return Object.freeze(tool);
}
function defineExtension(definition) {
  if (!definition || typeof definition !== "object") {
    throw new Error("defineExtension \u9700\u8981\u4E00\u4E2A\u5BF9\u8C61\u53C2\u6570");
  }
  return definition;
}
function createToolRegistry() {
  const tools = [];
  const seen = {};
  return {
    add(tool) {
      if (!tool || typeof tool !== "object" || tool.__llamaSwapTool !== true) {
        throw new Error("registry.add \u9700\u8981 defineTool/searchTool/fetchTool \u7684\u8FD4\u56DE\u503C");
      }
      if (seen[tool.name]) {
        throw new Error(`\u5DE5\u5177\u540D\u91CD\u590D\uFF1A${tool.name}\uFF08\u540C\u4E00\u6269\u5C55\u5185\u5FC5\u987B\u552F\u4E00\uFF09`);
      }
      seen[tool.name] = true;
      tools.push(tool);
      return tool;
    },
    tools() {
      return tools.slice();
    },
    names() {
      return Object.keys(seen);
    }
  };
}
function collapseText(value) {
  return String(value === void 0 || value === null ? "" : value).replace(/\s+/g, " ").trim();
}
function clampCount(value, fallback, min, max) {
  const parsed = Math.floor(Number(value));
  if (!isFinite(parsed)) {
    return fallback;
  }
  if (parsed < min) {
    return min;
  }
  if (parsed > max) {
    return max;
  }
  return parsed;
}
var DEFAULT_STATUS_ADVICE = {
  400: "\u8BF7\u6C42\u683C\u5F0F\u6216\u53C2\u6570\u4E0D\u5408\u6CD5\uFF0C\u4FEE\u6B63\u53C2\u6570\u540E\u91CD\u8BD5\u3002",
  401: "API Key \u65E0\u6548\u6216\u7F3A\u5931\uFF0C\u68C0\u67E5\u6269\u5C55\u8BBE\u7F6E\u91CC\u7684\u5BC6\u94A5\u3002",
  402: "\u8D26\u6237\u4F59\u989D\u4E0D\u8DB3\uFF0C\u5145\u503C\u540E\u91CD\u8BD5\u3002",
  403: "\u8BF7\u6C42\u88AB\u62D2\u7EDD\uFF08\u53EF\u80FD\u662F\u98CE\u63A7\u6216\u6743\u9650\u9650\u5236\uFF09\uFF0C\u4E0D\u8981\u91CD\u8BD5\u3002",
  404: "\u4E0A\u6E38\u6CA1\u6709\u627E\u5230\u53EF\u7528\u5185\u5BB9\uFF0C\u6362\u4E00\u4E2A URL \u6216\u7A0D\u540E\u91CD\u8BD5\u3002",
  408: "\u4E0A\u6E38\u8BFB\u53D6\u8BF7\u6C42\u8D85\u65F6\uFF0C\u7A0D\u540E\u91CD\u8BD5\u3002",
  413: "\u8BF7\u6C42\u4F53\u8FC7\u5927\uFF0C\u51CF\u5C11\u53C2\u6570\u4F53\u79EF\u540E\u91CD\u8BD5\u3002",
  429: "\u89E6\u53D1\u9650\u6D41\uFF08QPS \u8D85\u9650\uFF09\uFF0C\u7B49\u5F85\u540E\u91CD\u8BD5\u3002",
  500: "\u4E0A\u6E38\u5185\u90E8\u9519\u8BEF\uFF0C\u7A0D\u540E\u91CD\u8BD5\u3002",
  502: "\u4E0A\u6E38\u7F51\u5173\u9519\u8BEF\uFF0C\u7A0D\u540E\u91CD\u8BD5\u3002",
  503: "\u4E0A\u6E38\u6682\u4E0D\u53EF\u7528\uFF0C\u7A0D\u540E\u91CD\u8BD5\u3002",
  504: "\u4E0A\u6E38\u4E0B\u6E38\u8D85\u65F6\uFF0C\u7A0D\u540E\u91CD\u8BD5\u3002"
};
function apiErrorMessage(status, body, options) {
  const provider = options && options.provider || "\u8BF7\u6C42";
  let message = collapseText(body).slice(0, 300);
  try {
    const parsed = JSON.parse(body);
    if (parsed && typeof parsed === "object") {
      const candidates = [
        parsed.error && typeof parsed.error === "object" ? parsed.error.message : void 0,
        typeof parsed.error === "string" ? parsed.error : void 0,
        parsed.message,
        parsed.detail,
        parsed.base_resp && parsed.base_resp.status_msg,
        parsed.ResponseMetadata && parsed.ResponseMetadata.Error && parsed.ResponseMetadata.Error.Message
      ];
      for (const candidate of candidates) {
        if (typeof candidate === "string" && collapseText(candidate) !== "") {
          message = collapseText(candidate);
          break;
        }
      }
      const code = parsed.error && typeof parsed.error === "object" ? parsed.error.code : void 0;
      if (typeof code === "string" && code !== "" && message.indexOf(code) < 0) {
        message = code + " " + message;
      }
    }
  } catch (ignored) {
  }
  const table = Object.assign({}, DEFAULT_STATUS_ADVICE, options && options.advice || {});
  let advice = "";
  if (table[status]) {
    advice = table[status];
  } else if (status >= 500) {
    advice = DEFAULT_STATUS_ADVICE[500];
  }
  let text = provider + " \u8BF7\u6C42\u5931\u8D25\uFF1AHTTP " + status;
  if (message !== "") {
    text += " - " + message;
  }
  if (advice !== "") {
    text += "\uFF08" + advice + "\uFF09";
  }
  return text;
}
function applyCredential(plan, credential, value) {
  const headers = Object.assign({}, plan.headers || {});
  if (credential.bearer) {
    headers["Authorization"] = "Bearer " + value;
  } else if (credential.header) {
    headers[credential.header] = value;
  }
  let url = plan.url;
  if (credential.query) {
    const separator = url.indexOf("?") >= 0 ? "&" : "?";
    url = url + separator + encodeURIComponent(credential.query) + "=" + encodeURIComponent(value);
  }
  let body = plan.body;
  if (credential.body && body && typeof body === "object") {
    body = Object.assign({}, body);
    body[credential.body] = value;
  }
  return { url, method: plan.method, headers, body };
}
function renderSearchResults(providerTitle, query, results, preface, maxChars) {
  const lines = [providerTitle + " results for: " + query, ""];
  for (let index = 0; index < preface.length; index += 1) {
    if (collapseText(preface[index]) !== "") {
      lines.push(preface[index]);
    }
  }
  if (preface.length > 0) {
    lines.push("");
  }
  for (let index = 0; index < results.length; index += 1) {
    const item = results[index] || {};
    lines.push(index + 1 + ". " + (collapseText(item.title) || "(untitled)"));
    if (item.url) {
      lines.push("   " + collapseText(item.url));
    }
    const meta = [];
    if (item.source) {
      meta.push(collapseText(item.source));
    }
    if (item.date) {
      meta.push(collapseText(item.date));
    }
    if (item.score !== void 0 && item.score !== null && item.score !== "") {
      meta.push("score " + collapseText(item.score));
    }
    if (item.meta && item.meta.length > 0) {
      for (let part = 0; part < item.meta.length; part += 1) {
        if (collapseText(item.meta[part]) !== "") {
          meta.push(collapseText(item.meta[part]));
        }
      }
    }
    if (meta.length > 0) {
      lines.push("   " + meta.join(" | "));
    }
    if (item.lines && item.lines.length > 0) {
      for (let part = 0; part < item.lines.length; part += 1) {
        const text = collapseText(item.lines[part]);
        if (text !== "") {
          lines.push("   " + text);
        }
      }
    } else {
      const highlights = Array.isArray(item.highlights) ? item.highlights : [];
      if (highlights.length > 0) {
        for (let part = 0; part < highlights.length; part += 1) {
          const text = collapseText(highlights[part]);
          if (text !== "") {
            lines.push("   " + text);
          }
        }
      } else {
        const bodyText = collapseText(item.content) || collapseText(item.snippet) || collapseText(item.summary);
        if (bodyText !== "") {
          lines.push("   " + bodyText);
        }
      }
    }
    lines.push("");
  }
  return lines.join("\n").slice(0, maxChars);
}
function searchTool(spec) {
  if (!spec || typeof spec !== "object") {
    throw new Error("searchTool \u9700\u8981\u4E00\u4E2A\u5BF9\u8C61\u53C2\u6570");
  }
  const queryParameter = spec.queryParameter || "query";
  const properties = { query: { type: "string", description: "\u641C\u7D22\u67E5\u8BE2\u4E32\u3002" } };
  const required = ["query"];
  if (spec.parameters && spec.parameters.properties) {
    const extra = spec.parameters.properties;
    Object.keys(extra).forEach((key) => {
      if (key !== queryParameter) {
        properties[key] = extra[key];
      }
    });
  }
  if (spec.parameters && Array.isArray(spec.parameters.required)) {
    const declared = spec.parameters.required;
    for (let index = 0; index < declared.length; index += 1) {
      if (required.indexOf(declared[index]) < 0) {
        required.push(declared[index]);
      }
    }
  }
  return defineTool({
    name: spec.name,
    description: spec.description,
    parameters: { type: "object", properties, required },
    async handler(args, ctx) {
      const query = collapseText(args[queryParameter]);
      if (query === "") {
        throw new Error(spec.name + ": query must not be empty");
      }
      if (spec.maxQueryLength && query.length > spec.maxQueryLength) {
        throw new Error(
          spec.name + ": query must be at most " + spec.maxQueryLength + " characters (got " + query.length + ")"
        );
      }
      const label = spec.credential.label || "API Key";
      const credentialValue = collapseText(ctx.config[spec.credential.setting]);
      if (credentialValue === "") {
        throw new Error(spec.name + ": set the " + label + " in the extension settings");
      }
      const defaultPlan = spec.request({ query, config: ctx.config, arguments: args });
      if (!defaultPlan || typeof defaultPlan.url !== "string" || defaultPlan.url === "") {
        throw new Error(spec.name + ": \u63D0\u4F9B\u65B9\u6CA1\u6709\u7ED9\u51FA\u8BF7\u6C42\u5730\u5740");
      }
      const plan = applyCredential(defaultPlan, spec.credential, credentialValue);
      const options = {
        method: plan.method || "GET",
        headers: Object.assign({ "Content-Type": "application/json" }, plan.headers || {})
      };
      let timeoutMs = Number(ctx.config.requestTimeoutMs);
      if (isFinite(timeoutMs) && timeoutMs > 0) {
        options.timeoutMs = timeoutMs;
      }
      if (plan.body !== void 0 && plan.body !== null) {
        options.body = typeof plan.body === "string" ? plan.body : JSON.stringify(plan.body);
      }
      if (ctx.log && typeof ctx.log.debug === "function") {
        ctx.log.debug(spec.name + " -> " + plan.url.split("?")[0] + " method=" + options.method + " query=" + query);
      }
      let response;
      try {
        response = await ctx.http.fetch(plan.url, options);
      } catch (error) {
        throw new Error(spec.title + " \u65E0\u6CD5\u8BBF\u95EE\uFF1A" + String(error === void 0 ? "" : error));
      }
      const body = response.text();
      if (!response.ok) {
        throw new Error(apiErrorMessage(response.status, body, { provider: spec.title, advice: spec.statusAdvice }));
      }
      let payload;
      try {
        payload = JSON.parse(body);
      } catch (ignored) {
        throw new Error(spec.title + " \u8FD4\u56DE\u7684\u5185\u5BB9\u4E0D\u662F JSON");
      }
      if (!payload || typeof payload !== "object") {
        throw new Error(spec.title + " \u8FD4\u56DE\u7684\u5185\u5BB9\u4E0D\u662F JSON \u5BF9\u8C61");
      }
      const results = spec.parse(payload) || [];
      const preface = [];
      if (spec.preface) {
        const extra = spec.preface(payload);
        if (typeof extra === "string") {
          preface.push(extra);
        } else if (Array.isArray(extra)) {
          for (let index = 0; index < extra.length; index += 1) {
            preface.push(String(extra[index]));
          }
        }
      }
      if (results.length === 0) {
        if (spec.emptyMessage) {
          return { content: spec.emptyMessage(query) };
        }
        const lines = ["No " + spec.title + " results found for: " + query];
        const notes = [];
        for (let index = 0; index < preface.length; index += 1) {
          if (collapseText(preface[index]) !== "") {
            notes.push(preface[index]);
          }
        }
        if (notes.length > 0) {
          lines.push("");
          for (let index = 0; index < notes.length; index += 1) {
            lines.push(notes[index]);
          }
        }
        return { content: lines.join("\n") };
      }
      if (ctx.log && typeof ctx.log.info === "function") {
        ctx.log.info(spec.name + " -> " + results.length + " \u6761\u7ED3\u679C");
      }
      return { content: renderSearchResults(spec.title, query, results, preface, spec.maxChars || 8e3) };
    }
  });
}
function fetchTool(spec) {
  if (!spec || typeof spec !== "object") {
    throw new Error("fetchTool \u9700\u8981\u4E00\u4E2A\u5BF9\u8C61\u53C2\u6570");
  }
  const maxUrlLength = spec.maxUrlLength || 8192;
  return defineTool({
    name: spec.name,
    description: spec.description,
    parameters: {
      type: "object",
      properties: { url: { type: "string", description: "\u8981\u6293\u53D6\u7684 http/https \u5730\u5740\u3002" } },
      required: ["url"],
      type: "object"
    },
    async handler(args, ctx) {
      const url = collapseText(args.url);
      if (url === "") {
        throw new Error(spec.name + ": url must not be empty");
      }
      if (url.length > maxUrlLength) {
        throw new Error(spec.name + ": url must be at most " + maxUrlLength + " characters (got " + url.length + ")");
      }
      if (!/^https?:\/\//i.test(url)) {
        throw new Error(spec.name + ": url \u5FC5\u987B\u662F http \u6216 https \u5730\u5740");
      }
      const label = spec.credential.label || "API Key";
      const credentialValue = collapseText(ctx.config[spec.credential.setting]);
      if (credentialValue === "") {
        throw new Error(spec.name + ": set the " + label + " in the extension settings");
      }
      const defaultPlan = spec.request({ url, config: ctx.config, arguments: args });
      const plan = applyCredential(defaultPlan, spec.credential, credentialValue);
      const options = {
        method: plan.method || "POST",
        headers: Object.assign({ "Content-Type": "application/json" }, plan.headers || {})
      };
      let timeoutMs = Number(ctx.config.requestTimeoutMs);
      if (isFinite(timeoutMs) && timeoutMs > 0) {
        options.timeoutMs = timeoutMs;
      }
      if (plan.body !== void 0 && plan.body !== null) {
        options.body = typeof plan.body === "string" ? plan.body : JSON.stringify(plan.body);
      }
      let response;
      try {
        response = await ctx.http.fetch(plan.url, options);
      } catch (error) {
        throw new Error(spec.title + " \u65E0\u6CD5\u8BBF\u95EE\uFF1A" + String(error === void 0 ? "" : error));
      }
      const body = response.text();
      if (!response.ok) {
        throw new Error(apiErrorMessage(response.status, body, { provider: spec.title, advice: spec.statusAdvice }));
      }
      let payload;
      try {
        payload = JSON.parse(body);
      } catch (ignored) {
        throw new Error(spec.title + " \u8FD4\u56DE\u7684\u5185\u5BB9\u4E0D\u662F JSON");
      }
      if (!payload || typeof payload !== "object") {
        throw new Error(spec.title + " \u8FD4\u56DE\u7684\u5185\u5BB9\u4E0D\u662F JSON \u5BF9\u8C61");
      }
      const parsed = spec.parse(payload) || {};
      const title = collapseText(parsed.title);
      const content = String(parsed.content === void 0 || parsed.content === null ? "" : parsed.content);
      if (collapseText(content) === "") {
        return { content: spec.title + " \u6CA1\u6709\u8FD4\u56DE\u53EF\u8BFB\u6B63\u6587\uFF1A" + url };
      }
      const lines = ["Source: " + url];
      if (title !== "") {
        lines.push("Title: " + title);
      }
      lines.push("Type: markdown, " + content.length + " chars");
      lines.push("");
      lines.push(content.slice(0, spec.maxChars || 32e3));
      if (ctx.log && typeof ctx.log.info === "function") {
        ctx.log.info(spec.name + " -> " + url + " " + content.length + " chars");
      }
      return { content: lines.join("\n") };
    }
  });
}
// Annotate the CommonJS export names for ESM import in node:
0 && (module.exports = {
  DEFAULT_STATUS_ADVICE,
  apiErrorMessage,
  clampCount,
  collapseText,
  createToolRegistry,
  defineExtension,
  defineTool,
  fetchTool,
  searchTool
});
