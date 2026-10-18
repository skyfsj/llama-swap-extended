// Web Fetch - read a URL the way a browser does.
//
// The tool takes a URL the model was given (by a user, or by a search result)
// and returns the page as clean Markdown: title, final URL after redirects, and
// the article with navigation, cookie banners and comment threads removed.
//
// Browser simulation here means the request shape and the extraction:
//
//   - a real Chrome User-Agent, a document Accept header, Accept-Language, and
//     the Sec-Fetch-* headers a navigation sends;
//   - redirects are followed, and the final URL is reported so relative links
//     resolve and the model cites the address it actually read;
//   - the body is read as a document: entities decoded, relative links made
//     absolute, boilerplate stripped, main content selected.
//
// It is header-level impersonation, not TLS-level: Go's TLS stack has a
// fingerprint that sites protected by a bot-management service (Cloudflare's
// full check, PerimeterX) can still tell apart, which is exactly what
// curl-impersonate and curl_cffi's browser profiles exist to fix. Doing that
// needs a Go uTLS transport, not a script, so a page behind such a service
// answers 403 here.
//
// Proxy: this extension makes no proxy decision of its own. `ctx.http.fetch`
// follows the daemon's `network.proxy` / `noProxy` policy, so an egress proxy
// configured once covers this tool without anything here being aware of it.
//
// The declared permission is `networkHosts: ["*"]`, which is the whole public
// internet and *only* the public internet: the runtime refuses loopback,
// private, link-local and reserved addresses on the first request and again on
// every redirect hop, so a URL that resolves to the local network is rejected
// before a socket is opened.
//
// This is a fetch tool for content the caller asked for - it is not a crawler
// and it does not consult robots.txt. Pointing it at a site accepts that site's
// terms.

import { documentInfo, extractContent, toMarkdown } from "./lib/html.js";
import { originOf, withScheme } from "./lib/url.js";

const FORMAT_MARKDOWN = "markdown";
const FORMAT_TEXT = "text";
const FORMAT_HTML = "html";

// Sent verbatim, so the request is indistinguishable from a browser navigation
// at the header level. A site that wants a different agent (an API that asks
// callers to identify themselves) can be served by changing the setting.
const BROWSER_UA = "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36";
const BROWSER_LANGUAGE = "en-US,en;q=0.9";
const BROWSER_ACCEPT = "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,image/apng,*/*;q=0.8";

// Accept-Encoding is deliberately absent. Go's transport adds "gzip" itself and
// transparently decompresses the response; setting the header here would
// disable that and hand the converter compressed bytes, because the sandbox
// has no inflate.
const NAVIGATION_HEADERS = {
  "Sec-Fetch-Dest": "document",
  "Sec-Fetch-Mode": "navigate",
  "Sec-Fetch-Site": "none",
  "Sec-Fetch-User": "?1",
  "Upgrade-Insecure-Requests": "1",
};

// The runtime refuses a response body over 10 MiB and cap the whole call; these
// are this tool's own, tighter, limits.
const MAX_PARSED = 2 * 1024 * 1024;
const MAX_PASTED = 1024 * 1024;
const DEFAULT_CHARS = 20000;
const MIN_CHARS = 1000;
const MAX_CHARS = 100000;
const DEFAULT_TIMEOUT_SECONDS = 20;
const MIN_TIMEOUT_SECONDS = 3;
const MAX_TIMEOUT_SECONDS = 45;

const UTF8_CHARSETS = /^(?:utf-?8|us-ascii|ascii|unicode-1-1-utf-8)$/;

export const settings = {
  userAgent: {
    type: "string",
    label: "User agent",
    hint: "Sent as the User-Agent header. Leaving the browser default is what keeps ordinary sites from answering 403.",
    default: BROWSER_UA,
    section: "Request",
  },
  language: {
    type: "string",
    label: "Accept-Language",
    hint: "Which translation of a page the server should return, e.g. zh-CN,zh;q=0.9,en;q=0.8.",
    default: BROWSER_LANGUAGE,
    section: "Request",
  },
  timeoutSeconds: {
    type: "number",
    label: "Request timeout (seconds)",
    hint: "How long one page fetch may take, redirects included. The manifest's own timeout must be larger, or the hook is killed first.",
    default: DEFAULT_TIMEOUT_SECONDS,
    min: MIN_TIMEOUT_SECONDS,
    max: MAX_TIMEOUT_SECONDS,
    section: "Request",
  },
  format: {
    type: "select",
    label: "Default format",
    hint: "markdown keeps headings, lists and links; text drops the markup. A tool call may ask for something else.",
    options: [FORMAT_MARKDOWN, FORMAT_TEXT, FORMAT_HTML],
    default: FORMAT_MARKDOWN,
    section: "Content",
  },
  contentMode: {
    type: "select",
    label: "Content selection",
    hint: "article finds the main body and drops navigation, sidebars and comments; full keeps everything after scripts and styles are removed.",
    options: ["article", "full"],
    default: "article",
    section: "Content",
  },
  maxChars: {
    type: "number",
    label: "Max characters",
    hint: "How much text one fetch returns. A tool call may ask for less, never for more.",
    default: DEFAULT_CHARS,
    min: MIN_CHARS,
    max: MAX_CHARS,
    section: "Content",
  },
};

function number(value, fallback) {
  const parsed = Number(value);
  return isFinite(parsed) ? parsed : fallback;
}

function clamp(value, low, high) {
  const parsed = Math.floor(number(value, low));
  if (parsed < low) {
    return low;
  }
  return parsed > high ? high : parsed;
}

function pickFormat(value, fallback) {
  const wanted = String(value || fallback || FORMAT_MARKDOWN).trim().toLowerCase();
  if (wanted === FORMAT_TEXT || wanted === FORMAT_HTML) {
    return wanted;
  }
  return FORMAT_MARKDOWN;
}

function normalizeTarget(value) {
  const raw = withScheme(String(value || "").trim());
  if (raw === "") {
    throw new Error("web_fetch: url is required");
  }
  const scheme = /^([a-zA-Z][a-zA-Z0-9+.-]*):/.exec(raw);
  const name = scheme ? scheme[1].toLowerCase() : "";
  if (name !== "http" && name !== "https") {
    throw new Error("web_fetch: only http and https URLs can be fetched (got " + (name || "no scheme") + ")");
  }
  if (/^[a-zA-Z][a-zA-Z0-9+.-]*:\/\/[^/?#]*@/.test(raw)) {
    throw new Error("web_fetch: remove the credentials from the URL; this tool does not send them");
  }
  return raw;
}

// readableAddress is what goes into a log line: the query string is dropped
// because a URL being fetched can carry a token.
function readableAddress(url) {
  const cut = url.split("#")[0].split("?")[0];
  return cut === "" ? originOf(url) : cut;
}

// sourceLine names where the text came from. Content handed to the tool
// directly has no URL, and saying so is better than printing an empty address.
function sourceLine(finalUrl) {
  return finalUrl === "" ? "Source: content supplied by the caller" : "Source: " + finalUrl;
}

function size(bytes) {
  if (bytes < 1024) {
    return bytes + " bytes";
  }
  if (bytes < 1024 * 1024) {
    return Math.round(bytes / 1024) + " KiB";
  }
  return (Math.round(bytes / (1024 * 1024) * 10) / 10) + " MiB";
}

function charsetOf(contentType) {
  const match = /charset\s*=\s*["']?([a-zA-Z0-9_:.-]+)/i.exec(String(contentType || ""));
  return match ? match[1].toLowerCase() : "";
}

// looksBinary spots a response that is not text: a NUL byte, a run of control
// characters, or a body that is mostly U+FFFD, which is what the runtime
// produces for bytes that are not UTF-8 (a GBK page, an image).
function looksBinary(body) {
  const sample = body.slice(0, 4096);
  if (sample.length === 0) {
    return false;
  }
  let control = 0;
  let broken = 0;
  for (let index = 0; index < sample.length; index += 1) {
    const code = sample.charCodeAt(index);
    if (code === 0) {
      return true;
    }
    if (code < 9 || (code > 13 && code < 32)) {
      control += 1;
    } else if (code === 0xfffd) {
      broken += 1;
    }
  }
  return (control + broken) / sample.length > 0.05;
}

// classify decides how the body should be read. The declared content type wins;
// sniffing only fills in for a server that declared nothing useful.
function classify(body, contentType) {
  const type = String(contentType || "").split(";")[0].trim().toLowerCase();
  const head = body.slice(0, 1024).trim().toLowerCase();
  if (type === "application/pdf" || body.slice(0, 5) === "%PDF-") {
    return "pdf";
  }
  if (type.indexOf("json") >= 0) {
    return "json";
  }
  if (type.indexOf("html") >= 0 || type.indexOf("xhtml") >= 0) {
    return "html";
  }
  if (type.indexOf("xml") >= 0) {
    return head.indexOf("<html") >= 0 ? "html" : "text";
  }
  if (looksBinary(body)) {
    return "binary";
  }
  if (head.indexOf("<!doctype html") === 0 || head.indexOf("<html") >= 0) {
    return "html";
  }
  if (head.charAt(0) === "{" || head.charAt(0) === "[") {
    try {
      JSON.parse(body);
      return "json";
    } catch (ignored) {
      return "text";
    }
  }
  return "text";
}

function prettyJSON(body) {
  let value;
  try {
    value = JSON.parse(body);
  } catch (ignored) {
    return body;
  }
  try {
    const rendered = JSON.stringify(value, null, 2);
    return rendered === undefined ? body : rendered;
  } catch (ignored) {
    return body;
  }
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
  return cut + "\n\n[... " + (content.length - cut.length) + " characters omitted; this page is longer than the " + limit + " character limit ...]";
}

// fetchFailure turns a runtime rejection into something the model can act on.
// The messages come from the extension sandbox and name the reason precisely;
// the restatement here adds what the caller should do about it.
function fetchFailure(url, error) {
  const reason = String(error === undefined || error === null ? "" : error);
  const host = originOf(url);
  if (reason.indexOf("not a public address") >= 0 || reason.indexOf("could not resolve") >= 0) {
    return "web_fetch: " + host + " does not resolve to a public address, so it cannot be fetched. Only internet sites are reachable; local, private and link-local addresses are blocked.";
  }
  if (reason.indexOf("network host is not permitted") >= 0 || reason.indexOf("redirect host is not permitted") >= 0) {
    return "web_fetch: the extension's network permissions do not allow " + host + ". Add it to permissions.networkHosts in the extension manifest.";
  }
  if (reason.indexOf("exceeds 10 MiB") >= 0) {
    return "web_fetch: " + host + " returned more than 10 MiB, which the sandbox does not allow. Nothing was read.";
  }
  if (reason.indexOf("too many redirects") >= 0) {
    return "web_fetch: " + host + " redirects in a loop; the fetch was stopped after five hops.";
  }
  if (reason.toLowerCase().indexOf("timeout") >= 0 || reason.toLowerCase().indexOf("deadline") >= 0) {
    return "web_fetch: the request to " + host + " timed out. The page may be slow, or an egress proxy may not be answering; retry, or raise the timeout setting.";
  }
  return "web_fetch: cannot read " + host + " (" + reason + ")";
}

// statusFailure describes an HTTP failure. It is returned as the tool result
// rather than thrown: a site answering 404 is a fact about the page, not a
// malfunction of the tool.
function statusFailure(url, status, contentType) {
  const head = [
    "Source: " + url,
    "Status: HTTP " + status,
  ];
  let advice = "The server refused the request.";
  if (status === 401 || status === 403) {
    advice = "The site refused the request. It may require a login, block automated clients, or serve a bot challenge (a service like Cloudflare or PerimeterX can tell a scripted client apart by its TLS fingerprint). A different user agent will not help.";
  } else if (status === 404 || status === 410) {
    advice = "The page does not exist at this address. Check the URL, or search for the content instead.";
  } else if (status === 429) {
    advice = "The site is rate limiting this client. Wait before fetching more pages from it.";
  } else if (status >= 500) {
    advice = "The site reported a server error. Retrying later may work.";
  } else if (status >= 300 && status < 400) {
    advice = "The site redirected without landing anywhere useful; the URL may be wrong.";
  }
  if (String(contentType || "").indexOf("html") >= 0) {
    head.push("Note: the response body was an HTML error page and was not converted.");
  }
  head.push("");
  head.push(advice);
  return head.join("\n");
}

function typeLine(kind, contentType, bytes) {
  const declared = String(contentType || "").split(";")[0].trim();
  return (declared === "" ? kind : declared) + ", " + size(bytes);
}

export default {
  tools: [{
    type: "function",
    execution: "server",
    function: {
      name: "web_fetch",
      description: "Fetch a web page and return it as readable Markdown: the title, the final URL after redirects, and the main content with navigation, cookie banners and comments removed. Use it when the user gives a URL, or to read a search result in full. Long pages are truncated. Local and private addresses cannot be fetched, and pages behind a login usually answer 401 or 403. Prefer web_search for facts across many sources.",
      parameters: {
        type: "object",
        properties: {
          url: {
            type: "string",
            description: "Page to read, for example https://example.com/docs or example.com/docs (https is assumed).",
          },
          format: {
            type: "string",
            enum: [FORMAT_MARKDOWN, FORMAT_TEXT, FORMAT_HTML],
            description: "markdown (default) keeps headings, lists and links; text drops the markup; html returns the cleaned HTML fragment instead of converting it.",
          },
          maxChars: {
            type: "number",
            description: "Maximum characters of content to return (1000-100000). Raise it when a first read reports that text was omitted.",
          },
          content: {
            type: "string",
            description: "Raw HTML to convert instead of fetching a URL. Rarely needed; use url unless you already hold the page source.",
          },
        },
        required: ["url"],
      },
    },
  }],

  async onToolCall(ctx, call) {
    if (call.name !== "web_fetch") {
      return undefined;
    }
    const args = call.arguments || {};
    const format = pickFormat(args.format, ctx.config.format);
    const mode = ctx.config.contentMode === "full" ? "full" : "article";
    const limit = clamp(args.maxChars === undefined ? ctx.config.maxChars : args.maxChars, MIN_CHARS, MAX_CHARS);
    const timeoutSeconds = clamp(ctx.config.timeoutSeconds, MIN_TIMEOUT_SECONDS, MAX_TIMEOUT_SECONDS);
    const pasted = typeof args.content === "string" ? args.content : "";

    let body = pasted;
    let status = 200;
    let contentType = "";
    let finalUrl = "";

    if (pasted !== "") {
      if (pasted.length > MAX_PASTED) {
        throw new Error("web_fetch: the supplied content is larger than " + size(MAX_PASTED) + "; fetch the URL instead");
      }
      finalUrl = String(args.url || "").trim();
    } else {
      const target = normalizeTarget(args.url);
      ctx.log.debug("web_fetch <- " + readableAddress(target));
      const headers = {
        "User-Agent": String(ctx.config.userAgent || BROWSER_UA),
        "Accept": BROWSER_ACCEPT,
        "Accept-Language": String(ctx.config.language || BROWSER_LANGUAGE),
      };
      const names = Object.keys(NAVIGATION_HEADERS);
      for (let index = 0; index < names.length; index += 1) {
        headers[names[index]] = NAVIGATION_HEADERS[names[index]];
      }
      let response;
      try {
        // The hook budget in the manifest caps this call too: fetching a page
        // is slower than calling an API, so both have to allow for it.
        response = await ctx.http.fetch(target, {
          method: "GET",
          headers: headers,
          timeoutMs: timeoutSeconds * 1000,
        });
      } catch (error) {
        throw new Error(fetchFailure(target, error));
      }
      status = number(response.status, 0);
      contentType = String((response.headers || {})["content-type"] || (response.headers || {})["Content-Type"] || "");
      finalUrl = String(response.url || target);
      if (!response.ok) {
        ctx.log.info("web_fetch -> HTTP " + status + " from " + readableAddress(finalUrl));
        return { content: statusFailure(finalUrl, status, contentType) };
      }
      body = response.text();
    }

    // Keep the parse bounded: the sandbox allows a 10 MiB body and the CPU
    // budget for one hook is not sized for a 10 MiB parse.
    let parsed = body;
    let clipped = false;
    if (parsed.length > MAX_PARSED && format !== FORMAT_TEXT) {
      parsed = parsed.slice(0, MAX_PARSED);
      clipped = true;
    }

    const kind = classify(parsed, contentType);
    if (kind === "pdf") {
      return {
        content: [
          sourceLine(finalUrl),
          "Type: " + typeLine(kind, contentType, body.length),
          "",
          "This is a PDF, which this tool does not decode. Install the pdf-extract preset and call extract_pdf_text with the same URL to read it.",
        ].join("\n"),
      };
    }
    if (kind === "binary") {
      return {
        content: [
          sourceLine(finalUrl),
          "Type: " + typeLine(kind, contentType, body.length),
          "",
          "The response is not text (a declared charset that is not UTF-8 garbles it, or it is an image, archive or other binary payload), so it was not returned.",
        ].join("\n"),
      };
    }

    const info = { title: "", charset: "", base: "", lang: "" };
    let content = "";
    if (kind === "html") {
      const document = documentInfo(parsed);
      info.title = document.title;
      info.charset = document.charset;
      info.base = document.base;
      info.lang = document.lang;
      const fragment = extractContent(parsed, mode);
      content = format === FORMAT_HTML ? fragment : toMarkdown(fragment, info.base || finalUrl, format === FORMAT_TEXT);
    } else if (kind === "json") {
      content = format === FORMAT_HTML ? parsed : prettyJSON(parsed);
    } else {
      content = parsed;
    }
    content = content.replace(/\u0000/g, "");
    if (content.trim() === "") {
      content = "(the page was fetched but no readable text was found in it)";
    }

    const lines = [sourceLine(finalUrl)];
    if (info.title !== "") {
      lines.push("Title: " + info.title);
    }
    lines.push("Type: " + typeLine(kind, contentType, body.length) + (clipped ? ", first " + size(MAX_PARSED) + " parsed" : ""));
    if (info.lang !== "") {
      lines.push("Language: " + info.lang);
    }
    const charset = charsetOf(contentType) || info.charset;
    if (charset !== "" && !UTF8_CHARSETS.test(charset)) {
      lines.push("Charset: " + charset + " - the page is not UTF-8 and the sandbox cannot transcode, so non-ASCII characters may be garbled.");
    }
    lines.push("");
    lines.push(truncate(content, limit));
    const result = lines.join("\n");
    ctx.log.info("web_fetch -> " + readableAddress(finalUrl) + " " + kind + " " + size(body.length) + " -> " + result.length + " chars");
    return { content: result };
  },
};
