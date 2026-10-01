import { EXTENSION_ENTRY_POINT } from "./extensionsApi";

/**
 * Pure helpers for the extension file tree. The server owns the real path rules
 * (see internal/extensions/files.go); these mirror the ones a user can trigger
 * from the editor so a typo is caught before a save attempt.
 */

const MANAGED_EXTENSIONS = [".js", ".mjs", ".cjs", ".json", ".md", ".txt", ".yaml", ".yml"];

export type FileKind = "javascript" | "json" | "text";

export function fileKind(path: string): FileKind {
  switch (extensionOf(path)) {
    case ".json":
      return "json";
    case ".js":
    case ".mjs":
    case ".cjs":
      return "javascript";
    default:
      return "text";
  }
}

export function isEntryPoint(path: string): boolean {
  return normalizeFilePath(path) === EXTENSION_ENTRY_POINT;
}

export function extensionOf(path: string): string {
  const name = path.split("/").pop() ?? "";
  const dot = name.lastIndexOf(".");
  return dot > 0 ? name.slice(dot).toLowerCase() : "";
}

/**
 * normalizeFilePath returns the cleaned relative path, or "" when the path is
 * not usable as an extension file.
 */
export function normalizeFilePath(raw: string): string {
  // Backslashes are rejected rather than converted: the server refuses them, so
  // a path that "works" in the browser would fail on save.
  const trimmed = raw.trim();
  if (!trimmed) return "";
  if (trimmed.length > 160) return "";
  if (trimmed.includes("\\")) return "";
  const withoutPrefix = trimmed.startsWith("./") ? trimmed.slice(2) : trimmed;
  if (withoutPrefix.startsWith("/") || /^[a-zA-Z]:/.test(withoutPrefix)) return "";
  const segments = withoutPrefix.split("/");
  for (const segment of segments) {
    if (!segment || segment === "." || segment === "..") return "";
    if (segment.startsWith(".")) return "";
    if (segment === "node_modules") return "";
  }
  if (!MANAGED_EXTENSIONS.includes(extensionOf(withoutPrefix))) return "";
  return segments.join("/");
}

// normalizeDirectoryPath validates a directory name the way
// normalizeFilePath validates a file, minus the extension requirement: a
// directory may be any managed-safe name ("lib", "utils-v2", …).
export function normalizeDirectoryPath(raw: string): string {
  const trimmed = raw.trim().replace(/\/+$/, "");
  if (!trimmed) return "";
  if (trimmed.length > 160) return "";
  if (trimmed.includes("\\")) return "";
  const withoutPrefix = trimmed.startsWith("./") ? trimmed.slice(2) : trimmed;
  if (withoutPrefix.startsWith("/") || /^[a-zA-Z]:/.test(withoutPrefix)) return "";
  const segments = withoutPrefix.split("/");
  for (const segment of segments) {
    if (!segment || segment === "." || segment === "..") return "";
    if (segment.startsWith(".")) return "";
    if (segment === "node_modules") return "";
  }
  return segments.join("/");
}

/** sortFilePaths lists files in a stable, readable order: shallow first, then by name. */
export function sortFilePaths(paths: string[]): string[] {
  return [...paths].sort((left, right) => {
    const leftDepth = left.split("/").length;
    const rightDepth = right.split("/").length;
    if (leftDepth !== rightDepth) return leftDepth - rightDepth;
    return left.localeCompare(right);
  });
}

export function fileTreeLevel(path: string): number {
  return Math.max(0, path.split("/").length - 1);
}

export function fileTreeIndent(path: string): string {
  return "  ".repeat(fileTreeLevel(path));
}

/** removedFilePaths reports files present in previous but not in next. */
export function removedFilePaths(previous: string[], next: string[]): string[] {
  const remaining = new Set(next);
  return previous.filter((path) => !remaining.has(path));
}

export function fileLabel(path: string): string {
  return path.split("/").pop() ?? path;
}

/** settingSectionOf groups declared settings, defaulting to a general section. */
export function settingSectionOf(field: { section?: string }): string {
  return field.section?.trim() || "general";
}

export function settingsFieldLabel(key: string, label?: string): string {
  return label?.trim() || key;
}

export interface ExtensionTreeRow {
  path: string;
  name: string;
  depth: number;
  directory: boolean;
}

// extensionTreeRows lays out the file tree. A directory is expanded when it
// is listed in `expanded` (or when `expanded` is undefined — the legacy
// expand-by-default behavior); the current file's ancestors are expanded by
// the caller either way. `declaredDirectories` are empty directories from the
// definition: they always render as directories even without child files.
export function extensionTreeRows(paths: string[], expanded?: Record<string, boolean>, declaredDirectories: string[] = []): ExtensionTreeRow[] {
  const children = new Map<string, Map<string, ExtensionTreeRow>>();
  for (const path of paths) {
    const parts = path.split("/");
    for (let depth = 0; depth < parts.length; depth++) {
      const parent = parts.slice(0, depth).join("/");
      const current = parts.slice(0, depth + 1).join("/");
      const entries = children.get(parent) ?? new Map<string, ExtensionTreeRow>();
      entries.set(current, { path: current, name: parts[depth], depth, directory: depth < parts.length - 1 });
      children.set(parent, entries);
    }
  }
  for (const directory of declaredDirectories) {
    const parts = directory.split("/");
    for (let depth = 0; depth < parts.length; depth++) {
      const parent = parts.slice(0, depth).join("/");
      const current = parts.slice(0, depth + 1).join("/");
      const entries = children.get(parent) ?? new Map<string, ExtensionTreeRow>();
      const existing = entries.get(current);
      if (!existing) entries.set(current, { path: current, name: parts[depth], depth, directory: true });
      children.set(parent, entries);
    }
  }
  const rows: ExtensionTreeRow[] = [];
  function visit(parent: string) {
    const entries = [...(children.get(parent)?.values() ?? [])].sort((a, b) => Number(b.directory) - Number(a.directory) || a.name.localeCompare(b.name));
    for (const entry of entries) {
      rows.push(entry);
      if (entry.directory && (expanded === undefined || expanded[entry.path])) visit(entry.path);
    }
  }
  visit("");
  return rows;
}
