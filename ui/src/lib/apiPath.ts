/**
 * Resolves server-rooted paths ("/api/version") against the path prefix the
 * UI is mounted under.
 *
 * llama-swap is commonly served behind a reverse proxy that strips a prefix,
 * e.g. https://host/asubpath/llama-swap/ rewrites to the origin's "/". The
 * server still sees /api/... and /v1/..., but the browser must request
 * /asubpath/llama-swap/api/...; root-anchored URLs would resolve against the
 * origin root and escape the proxy's location block. Every same-origin fetch
 * and EventSource URL funnels through apiUrl() so the prefix is re-attached.
 *
 * The UI uses hash routing (svelte-spa-router), so the document path is the
 * static mount point ending in "/ui" (or "/ui/") and the prefix is whatever
 * precedes it. When there is no prefix, or no browser at all (unit tests, SSR),
 * the path is returned unchanged.
 */

/** Extract the mount prefix preceding the UI's own path segment. Exported
 * for unit testing; callers should use apiUrl(). */
export function uiPrefix(pathname: string): string {
  const cut = pathname.lastIndexOf("/ui");
  if (cut < 0) return "";
  const rest = pathname.slice(cut);
  if (rest === "/ui" || rest === "/ui/") return pathname.slice(0, cut);
  return "";
}

/** Prefix a rooted server path with the current mount prefix.
 *
 * Only root-anchored paths are rewritten: a relative URL, an absolute URL
 * ("https://...") or a data: URL is returned unchanged, so this is safe to
 * apply to any request target. */
export function apiUrl(path: string): string {
  if (typeof window === "undefined") return path;
  if (!path.startsWith("/")) return path;
  // "//host/path" is protocol-relative and must not become a subpath.
  if (path.startsWith("//")) return path;
  const prefix = uiPrefix(window.location.pathname);
  return prefix ? prefix + path : path;
}
