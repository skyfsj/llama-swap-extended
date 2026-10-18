// Web Render - read a page the way a browser does, JavaScript and all.
//
// web_fetch reads the HTML a server sends, which is the wrong thing for a page
// that builds itself in the browser: a single-page app, a documentation site with
// client-side search, a dashboard. This tool hands the URL to a rendering service
// that runs the page in a real browser and returns the finished content.
//
// The service is configured, not hard-coded: `service` picks the well-known
// reader (r.jina.ai) or a self-hosted renderer, whose address goes in
// `endpoint`. Both speak the same simple contract - GET the target URL, take the
// rendered content back - so a self-hosted deployment is a first-class citizen
// rather than a fork of this file. A self-hosted renderer has to be reachable on
// a public host: the runtime refuses loopback, private and reserved addresses
// whatever the manifest says, which is a deliberate property of the extension
// sandbox rather than a limit of this tool.
//
// This is a fetch tool for content the caller asked for. It is not a crawler: it
// does not follow links, and it does not consult robots.txt. Pointing it at a site
// accepts that site's terms.

const SERVICE_JINA = "jina";
const SERVICE_CUSTOM = "custom";

const JINA_ENDPOINT = "https://r.jina.ai/{urlRaw}";

const FORMAT_MARKDOWN = "markdown";
const FORMAT_TEXT = "text";
const FORMAT_HTML = "html";

const DEFAULT_MAX_CHARS = 20000;
const MIN_CHARS = 1000;
const MAX_CHARS = 100000;
const DEFAULT_TIMEOUT_SECONDS = 30;
const MIN_TIMEOUT_SECONDS = 3;
const MAX_TIMEOUT_SECONDS = 120;

export const settings = {
  service: {
    type: "select",
    label: "Rendering service",
    hint: "jina uses r.jina.ai, which renders the page in a browser and returns Markdown. custom points at your own renderer using the address below, for a deployment that would rather not send URLs to a third party.",
    options: [SERVICE_JINA, SERVICE_CUSTOM],
    default: SERVICE_JINA,
    section: "Service",
  },
  endpoint: {
    type: "string",
    label: "Custom endpoint",
    hint: "Used when the service is custom. A URL template with {urlRaw} (the target appended as written: https://render.example.com/{urlRaw}) or {url} (the target percent-encoded, for a query parameter: https://render.example.com/api?target={url}&format=md).",
    default: "",
    section: "Service",
  },
  apiKey: {
    type: "password",
    label: "API key",
    hint: "Sent as Authorization: Bearer <key>. The reader works without one at a low rate; a key raises the limit. Required for some self-hosted renderers, left empty for those that need none.",
    secret: true,
    section: "Service",
  },
  timeoutSeconds: {
    type: "number",
    label: "Render timeout (seconds)",
    hint: "How long the service may spend loading the page before it gives up. Rendering is slower than a plain fetch, so this is larger than web_fetch's. The manifest's own timeout must be larger still, or the hook is killed first.",
    default: DEFAULT_TIMEOUT_SECONDS,
    min: MIN_TIMEOUT_SECONDS,
    max: MAX_TIMEOUT_SECONDS,
    section: "Request",
  },
  waitForSelector: {
    type: "string",
    label: "Wait for selector",
    hint: "A CSS selector the service should wait for before reading the page; use it for a page whose content appears after its scripts run, e.g. #search-results or article. Leave empty to take the page as soon as it settles.",
    default: "",
    section: "Request",
  },
  targetSelector: {
    type: "string",
    label: "Only this selector",
    hint: "A CSS selector to read instead of the whole page, e.g. main or .documentation. Leave empty for the whole page.",
    default: "",
    section: "Content",
  },
  maxChars: {
    type: "number",
    label: "Max characters",
    hint: "How much text one render returns. A tool call may ask for less, never for more.",
    default: DEFAULT_MAX_CHARS,
    min: MIN_CHARS,
    max: MAX_CHARS,
    section: "Content",
  },
  noCache: {
    type: "boolean",
    label: "Bypass the service cache",
    hint: "Ask the service to render the page fresh instead of answering from its cache. Slower, but the right choice for a page that changes often.",
    default: false,
    section: "Request",
  },
  includeLinks: {
    type: "boolean",
    label: "Keep links",
    hint: "Ask for a form of the content that keeps link targets, which is what lets the model cite a page it took a fact from. Turning it off gives plain prose.",
    default: true,
    section: "Content",
  },
};

function number(value, fallback) {
  const parsed = Number(value);
  return isFinite(parsed) ? parsed : fallback;
}

function integer(value, fallback, low, high) {
  const parsed = Math.floor(number(value, fallback));
  if (!isFinite(parsed)) {
    return fallback;
  }
  return Math.max(low, Math.min(high, parsed));
}

function clampText(value) {
  return String(value === undefined || value === null ? "" : value).trim();
}

// normalizeTarget accepts a bare host as well as a full URL, and refuses
// anything that is not http(s). The service is asked to fetch this address, so a
// file: URL would be a request to read the renderer's own disk.
function normalizeTarget(value) {
  let text = clampText(value);
  if (text === "") {
    throw new Error("web_render: a url is required");
  }
  // "example.com:8080/app" looks like a scheme to a naive test. A scheme is a
  // colon followed by "//" or by a non-numeric rest; a port is all digits.
  const looksLikeScheme = /^[a-zA-Z][a-zA-Z0-9+.-]*:(?:\/\/|[^0-9]|$)/.test(text) && !/^[a-zA-Z][a-zA-Z0-9+.-]*:\d+/.test(text);
  if (!looksLikeScheme) {
    text = "https://" + text;
  }
  const scheme = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(text);
  const name = scheme ? scheme[1].toLowerCase() : "";
  if (name !== "http" && name !== "https") {
    throw new Error("web_render: only http and https addresses can be rendered (got " + (name || "no scheme") + ")");
  }
  if (/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\/[^/?#]*@/.test(text)) {
    throw new Error("web_render: remove the credentials from the URL; this tool does not send them");
  }
  return text;
}

// rendererAddress builds the address the service is called at.
//
// Two placeholders, because the right escaping depends on where the target lands:
//
//   {url}     percent-encoded - for a query parameter, where a "&" or "#" in the
//             target would otherwise cut the renderer's own URL short
//   {urlRaw}  verbatim        - for a path-style renderer, which is what the
//             reader API documents ("https://r.jina.ai/https://example.com")
//
// A template may use either or both. Naming the choice is what keeps a
// self-hosted renderer from needing a fork of this file.
function rendererAddress(ctx, target) {
  const service = clampText(ctx.config.service) === SERVICE_CUSTOM ? SERVICE_CUSTOM : SERVICE_JINA;
  const template = service === SERVICE_CUSTOM ? clampText(ctx.config.endpoint) : JINA_ENDPOINT;
  if (service === SERVICE_CUSTOM && template === "") {
    throw new Error("web_render: set the custom endpoint in the extension settings, or choose the jina service");
  }
  if (template.indexOf("{url}") < 0 && template.indexOf("{urlRaw}") < 0) {
    throw new Error('web_render: the custom endpoint must contain {url} (percent-encoded) or {urlRaw} (verbatim), e.g. https://render.example.com/{urlRaw} or https://render.example.com/api?target={url}');
  }
  const scheme = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(template);
  const name = scheme ? scheme[1].toLowerCase() : "";
  if (name !== "http" && name !== "https") {
    throw new Error("web_render: the renderer endpoint must be an http or https address");
  }
  return template.split("{url}").join(encodeURIComponent(target)).split("{urlRaw}").join(target);
}

function pickFormat(value, fallback) {
  const wanted = clampText(value || fallback || FORMAT_MARKDOWN).toLowerCase();
  if (wanted === FORMAT_TEXT || wanted === FORMAT_HTML) {
    return wanted;
  }
  return FORMAT_MARKDOWN;
}

function size(bytes) {
  if (bytes < 1024) {
    return bytes + " bytes";
  }
  if (bytes < 1024 * 1024) {
    return (Math.round((bytes / 1024) * 10) / 10) + " KiB";
  }
  return (Math.round((bytes / (1024 * 1024)) * 100) / 100) + " MiB";
}

// readableAddress is what goes into a log line: the query string is dropped
// because a URL being rendered can carry a token.
function readableAddress(url) {
  const cut = String(url).split("#")[0].split("?")[0];
  return cut === "" ? String(url) : cut;
}

function truncate(content, limit) {
  if (content.length <= limit) {
    return content;
  }
  let cut = content.slice(0, limit);
  const lastBreak = cut.lastIndexOf("\n");
  if (lastBreak > limit * 0.6) {
    cut = cut.slice(0, lastBreak);
  }
  return cut + "\n\n[... " + (content.length - cut.length) + " characters omitted; the page is longer than the " + limit + " character limit ...]";
}

// readPayload pulls the rendered content out of whatever the service answered.
// The reader's JSON envelope is the common shape ({code, status, data: {title,
// url, content}}), and a plain Markdown body is what a self-hosted renderer
// usually returns, so both are accepted: a tool that only understood one of them
// would fail confusingly against the other.
function readPayload(body, contentType) {
  const trimmed = body.trim();
  const looksJSON = trimmed.charAt(0) === "{" || String(contentType || "").indexOf("json") >= 0;
  if (!looksJSON) {
    return { title: "", url: "", content: body };
  }
  let payload;
  try {
    payload = JSON.parse(trimmed);
  } catch (error) {
    // A service that declares JSON and returns Markdown is wrong, but recovering
    // from it costs nothing and a failed call costs the caller a round trip.
    return { title: "", url: "", content: body };
  }
  if (!payload || typeof payload !== "object") {
    return { title: "", url: "", content: body };
  }
  if (payload.code && payload.code >= 400) {
    const reason = payload.readableMessage || payload.message || payload.name || ("HTTP " + payload.code);
    throw new Error("web_render: the rendering service refused the request (" + reason + ")");
  }
  const data = payload.data && typeof payload.data === "object" ? payload.data : payload;
  return {
    title: clampText(data.title),
    url: clampText(data.url),
    content: typeof data.content === "string" ? data.content : typeof data.text === "string" ? data.text : "",
  };
}

// statusAdvice turns an HTTP failure into something the caller can act on. It is
// returned as the tool result rather than thrown: a 404 is a fact about the page.
function statusAdvice(status, detail) {
  let advice = "The rendering service refused the request.";
  if (status === 401 || status === 403) {
    advice = "The service refused the request. An API key may be required: set one in the extension settings. The page itself may also block automated clients.";
  } else if (status === 404) {
    advice = "The service could not find that page. Check the address, or search for the content instead.";
  } else if (status === 402) {
    advice = "The service is asking for payment: the account behind the API key has run out of credit.";
  } else if (status === 429) {
    advice = "The service is rate limiting this client. Wait before rendering more pages, or set an API key to raise the limit.";
  } else if (status === 422) {
    advice = "The service rejected the address as unrenderable. It may be a file rather than a page, or a host the service will not reach.";
  } else if (status >= 500) {
    advice = "The service reported a server error. Retrying later may work.";
  }
  if (detail !== "") {
    advice += " Service said: " + detail;
  }
  return advice;
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "web_render",
      description: "Load a web page in a real browser and return the finished content - JavaScript executed, redirects followed - as Markdown by default. Use it for a page that builds itself client-side (a single-page app, a client-rendered documentation site, a dashboard), where a plain fetch would return an empty shell; for a static page, web_fetch is faster and needs no service. The page is rendered by a configured service, so the address is sent to that service; a self-hosted renderer can be configured instead. Takes a url, an optional css selector to wait for or to read in isolation, and a character limit. Long pages are truncated. Rendering is slower than fetching, so allow for the timeout setting.",
      parameters: {
        type: "object",
        properties: {
          url: {
            type: "string",
            description: "The page to render, e.g. https://example.com/app or example.com/app (https is assumed).",
          },
          format: {
            type: "string",
            enum: [FORMAT_MARKDOWN, FORMAT_TEXT, FORMAT_HTML],
            description: "markdown (default) keeps headings, lists and links; text drops the markup; html returns rendered HTML instead of converting it.",
          },
          waitFor: {
            type: "string",
            description: "A CSS selector to wait for before the page is read, e.g. \"#results\" or \"article\". Use it when the content appears after the page's scripts run.",
          },
          selector: {
            type: "string",
            description: "Read only this CSS selector instead of the whole page, e.g. \"main\" or \".documentation\".",
          },
          maxChars: {
            type: "number",
            description: "Maximum characters of content to return (1000-100000). Raise it when a first read reports that text was omitted.",
          },
          noCache: {
            type: "boolean",
            description: "Ask the service to render the page fresh rather than answering from its cache.",
          },
          timeout: {
            type: "number",
            description: "How long the service may spend loading the page, in seconds (3-120).",
          },
        },
        required: ["url"],
      },
    },
  }],

  async onToolCall(ctx, call) {
    if (call.name !== "web_render") {
      return undefined;
    }
    const args = call.arguments || {};
    const target = normalizeTarget(args.url);
    const address = rendererAddress(ctx, target);
    const format = pickFormat(args.format, ctx.config.format);
    const limit = integer(args.maxChars === undefined ? ctx.config.maxChars : args.maxChars, DEFAULT_MAX_CHARS, MIN_CHARS, MAX_CHARS);
    const timeoutSeconds = integer(
      args.timeout === undefined ? ctx.config.timeoutSeconds : args.timeout,
      DEFAULT_TIMEOUT_SECONDS,
      MIN_TIMEOUT_SECONDS,
      MAX_TIMEOUT_SECONDS,
    );
    const waitFor = clampText(args.waitFor || ctx.config.waitForSelector);
    const selector = clampText(args.selector || ctx.config.targetSelector);
    const noCache = args.noCache === undefined ? ctx.config.noCache === true : args.noCache === true;
    const apiKey = clampText(ctx.config.apiKey);

    const headers = { Accept: "application/json" };
    if (apiKey !== "") {
      headers.Authorization = "Bearer " + apiKey;
    }
    if (format !== FORMAT_MARKDOWN) {
      headers["X-Return-Format"] = format;
    } else if (ctx.config.includeLinks === false) {
      headers["X-Return-Format"] = "text";
    }
    if (waitFor !== "") {
      headers["X-Wait-For-Selector"] = waitFor;
    }
    if (selector !== "") {
      headers["X-Target-Selector"] = selector;
    }
    if (noCache) {
      headers["X-No-Cache"] = "true";
    }
    headers["X-Timeout"] = String(timeoutSeconds);

    ctx.log.debug("web_render <- " + readableAddress(target));
    let response;
    try {
      response = await ctx.http.fetch(address, {
        method: "GET",
        headers: headers,
        // The hook budget is the outer bound; this keeps one slow page from
        // consuming it without a result.
        timeoutMs: Math.min(60000, (timeoutSeconds + 5) * 1000),
      });
    } catch (error) {
      const reason = String(error === undefined || error === null ? "" : error);
      if (reason.indexOf("not a public address") >= 0 || reason.indexOf("could not resolve") >= 0) {
        throw new Error("web_render: " + readableAddress(address) + " does not resolve to a public address, so it cannot be reached. Only internet hosts are reachable; local, private and link-local addresses are blocked, which is why a renderer on your own network has to be published on a public host.");
      }
      if (reason.indexOf("network host is not permitted") >= 0 || reason.indexOf("redirect host is not permitted") >= 0) {
        throw new Error("web_render: the extension's network permissions do not allow " + readableAddress(address) + ". Add it to permissions.networkHosts in the extension manifest.");
      }
      if (reason.toLowerCase().indexOf("timeout") >= 0 || reason.toLowerCase().indexOf("deadline") >= 0) {
        throw new Error("web_render: the rendering service did not answer within " + timeoutSeconds + " seconds. The page may be heavy or the service busy; retry, or raise the timeout setting.");
      }
      throw new Error("web_render: cannot reach the rendering service at " + readableAddress(address) + " (" + reason + ")");
    }

    const body = response.text();
    const contentType = String((response.headers || {})["content-type"] || (response.headers || {})["Content-Type"] || "");
    if (!response.ok) {
      let detail = "";
      try {
        const payload = JSON.parse(body.trim());
        if (payload && typeof payload === "object") {
          detail = clampText(payload.readableMessage || payload.message || payload.name);
        }
      } catch (ignored) {
        detail = "";
      }
      ctx.log.info("web_render -> HTTP " + response.status + " from " + readableAddress(target));
      return { content: "Source: " + target + "\nStatus: HTTP " + response.status + "\n\n" + statusAdvice(number(response.status, 0), detail) };
    }

    const payload = readPayload(body, contentType);
    const finalUrl = payload.url !== "" ? payload.url : target;
    let content = payload.content.replace(/\u0000/g, "");
    if (format === FORMAT_TEXT && /[#*\[\]]{2}/.test(content)) {
      // A service that ignored the format request still sends Markdown; the
      // lightest possible unwrapping is better than handing the caller markup
      // they did not ask for.
      content = content.replace(/\[([^\]]+)\]\(([^)]+)\)/g, "$1 ($2)").replace(/^\s{0,3}#{1,6}\s+/gm, "").replace(/\*\*([^*]+)\*\*/g, "$1").replace(/^[-*] /gm, "- ");
    }
    if (content.trim() === "") {
      return {
        content: [
          "Source: " + finalUrl,
          "Type: rendered page, " + size(body.length),
          "",
          "The service rendered the page but found no readable text in it. The page may be empty, may require a login, or may need more time: try waitFor with a selector for the content.",
        ].join("\n"),
      };
    }

    const lines = ["Source: " + finalUrl];
    if (payload.title !== "") {
      lines.push("Title: " + payload.title);
    }
    lines.push("Type: rendered page, " + size(body.length) + (waitFor !== "" ? ", waited for " + waitFor : ""));
    lines.push("Note: the content was produced by running the page's JavaScript, so anything the page loaded after its scripts ran is included; anything behind a login is not.");
    lines.push("");
    lines.push(truncate(content, limit));
    const result = lines.join("\n");
    ctx.log.info("web_render -> " + readableAddress(finalUrl) + " " + size(body.length) + " -> " + result.length + " chars");
    return { content: result };
  },
};
