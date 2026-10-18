// URL resolution for links found in a page.
//
// The sandbox has no URL class, so the parts are split with a regular
// expression and the path is normalized by hand. This is what turns
// href="/docs/intro" into something a model can actually cite.

// SPLIT separates a URL into scheme, authority, path, query and fragment.
const SPLIT = /^(?:([a-zA-Z][a-zA-Z0-9+.-]*):)?(?:\/\/([^/?#]*))?([^?#]*)(\?[^#]*)?(#[\s\S]*)?$/;

// ABSOLUTE is the scheme test: "https://…", "mailto:…", "data:…".
const ABSOLUTE = /^[a-zA-Z][a-zA-Z0-9+.-]*:/;

export function isAbsoluteUrl(value) {
  return ABSOLUTE.test(value);
}

// resolveUrl resolves a reference against the page it was found on. An
// unresolvable reference returns "" so the caller drops the link instead of
// printing a fragment of a URL.
export function resolveUrl(base, reference) {
  const raw = String(reference === undefined || reference === null ? "" : reference).replace(/[\r\n\t]/g, "").trim();
  if (raw === "") {
    return "";
  }
  const baseParts = SPLIT.exec(String(base || "")) || [];
  const scheme = baseParts[1] || "https";
  const authority = baseParts[2] || "";
  const basePath = baseParts[3] || "/";
  if (raw.charAt(0) === "#") {
    return String(base || "") + raw;
  }
  if (raw.indexOf("//") === 0) {
    // Protocol-relative: inherit the page's scheme.
    return scheme + ":" + raw;
  }
  if (ABSOLUTE.test(raw)) {
    return raw;
  }
  if (raw.charAt(0) === "?") {
    return scheme + "://" + authority + basePath + raw;
  }
  const slash = basePath.lastIndexOf("/");
  const candidate = raw.charAt(0) === "/" ? raw : (slash >= 0 ? basePath.slice(0, slash + 1) : "/") + raw;
  const parts = SPLIT.exec(candidate) || [];
  const path = normalizePath(parts[3] === undefined || parts[3] === "" ? candidate : parts[3]);
  const suffix = (parts[4] || "") + (parts[5] || "");
  return scheme + "://" + authority + path + suffix;
}

// normalizePath removes "." and ".." segments, keeping the leading slash and a
// trailing slash when the reference asked for one.
export function normalizePath(path) {
  const value = String(path || "");
  const trailing = value.length > 1 && value.charAt(value.length - 1) === "/";
  const queryMatch = /\?[\s\S]*$/.exec(value);
  const query = queryMatch ? queryMatch[0] : "";
  const bare = query ? value.slice(0, value.length - query.length) : value;
  const hashMatch = /#[\s\S]*$/.exec(bare);
  const hash = hashMatch ? hashMatch[0] : "";
  const cleanPath = hash ? bare.slice(0, bare.length - hash.length) : bare;
  const leading = cleanPath.charAt(0) === "/";
  const segments = cleanPath.split("/");
  const out = [];
  for (let index = 0; index < segments.length; index += 1) {
    const segment = segments[index];
    if (segment === "" || segment === ".") {
      continue;
    }
    if (segment === "..") {
      out.pop();
      continue;
    }
    out.push(segment);
  }
  let result = out.join("/");
  if (leading) {
    result = "/" + result;
  }
  if (trailing && result.charAt(result.length - 1) !== "/") {
    result += "/";
  }
  return result + hash + query;
}

// originOf returns "scheme://authority" for logging, which deliberately drops
// the path and query: a URL in a log line can carry a token.
export function originOf(url) {
  const parts = SPLIT.exec(String(url || ""));
  if (!parts) {
    return "";
  }
  return parts[2] ? (parts[1] || "https") + "://" + parts[2] : "";
}

// withScheme gives a bare "example.com/page" the https scheme, which is what a
// user typing a URL into a chat means.
export function withScheme(value) {
  const trimmed = String(value || "").trim();
  if (trimmed === "") {
    return "";
  }
  if (trimmed.indexOf("//") === 0) {
    return "https:" + trimmed;
  }
  if (ABSOLUTE.test(trimmed)) {
    return trimmed;
  }
  return "https://" + trimmed;
}
