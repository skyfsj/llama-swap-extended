// Pure predicates for the unsaved-leave guard. An "internal" target is a same
// SPA route (a path that starts with "/"); anything with a URL scheme (http,
// mailto, tel, ...) or an in-page hash fragment is not a route navigation and
// is never guarded.

export function isInternalRoute(href: string | null): boolean {
  if (!href) return false;
  if (/^[a-z][a-z0-9+.-]*:/i.test(href)) return false; // any URL scheme
  if (href.startsWith("#")) return false;
  return href.startsWith("/");
}

/** True when clicking `href` would keep the user on the Settings page. */
export function isSameSettingsTarget(href: string): boolean {
  const path = href.split(/[?#]/)[0];
  return path === "/settings" || path.startsWith("/settings/");
}
