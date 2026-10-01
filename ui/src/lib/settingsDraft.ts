import type { Translate } from "./i18n";
import type { SettingsChange, SettingsField } from "./settingsApi";

// Pure helpers for the settings-center draft: JSON-pointer-ish path access,
// per-field validation and draft-vs-baseline change summarization. They are
// free of Svelte reactivity so they stay unit-testable; the Settings route
// owns the draft state and wires these into its editors.

// Go duration literal: one or more "<number><unit>" parts, e.g. "2s",
// "1h30m". A unitless number is rejected on purpose: Go would read it as
// nanoseconds, which is almost never what a settings entry means.
const DURATION_RE = /^(?:\d+(?:\.\d+)?(?:ns|us|µs|ms|s|m|h))+(?:[.,]\d+)?$/;
const ENV_NAME_RE = /^[A-Za-z_][A-Za-z0-9_]*$/;

export function configPathParts(path: string): string[] {
  return path
    .replace(/^\//, "")
    .split("/")
    .filter(Boolean)
    .map((part) => part.replace(/~1/g, "/").replace(/~0/g, "~"));
}

export function valueAtConfig(config: Record<string, unknown>, path: string): unknown {
  let current: unknown = config;
  for (const part of configPathParts(path)) {
    if (!current || typeof current !== "object") return undefined;
    current = (current as Record<string, unknown>)[part];
  }
  return current;
}

// Draft configs are Svelte reactive proxies. The API only deals in JSON
// values, so serializing is both safe and avoids structuredClone trying to
// clone the proxy itself when a field is edited.
export function cloneConfig(config: Record<string, unknown>): Record<string, unknown> {
  return JSON.parse(JSON.stringify(config)) as Record<string, unknown>;
}

// Returns a NEW config with the value set at the JSON-pointer-ish path,
// creating intermediate objects. Pure: the caller assigns the result back to
// its reactive state.
export function updateAtConfig(
  config: Record<string, unknown>,
  path: string,
  value: unknown,
): Record<string, unknown> {
  const keys = configPathParts(path);
  if (!keys.length) return config;
  const next = cloneConfig(config);
  let cursor = next;
  for (let index = 0; index < keys.length - 1; index += 1) {
    const child = cursor[keys[index]];
    cursor[keys[index]] = child && typeof child === "object" && !Array.isArray(child)
      ? structuredClone(child)
      : {};
    cursor = cursor[keys[index]] as Record<string, unknown>;
  }
  cursor[keys[keys.length - 1]] = value;
  return next;
}

export function validateSettingsField(
  t: Translate,
  field: SettingsField,
  value: unknown,
): string | undefined {
  const empty = value === undefined || value === null || value === "";
  if (field.required && empty) return t("settingsCenter.validation.required");
  if (empty) return undefined;
  if (field.component === "number") {
    if (typeof value !== "number" || !Number.isFinite(value)) return t("settingsCenter.validation.number");
    if ((typeof field.min === "number" && value < field.min)
      || (typeof field.max === "number" && value > field.max)) {
      return t("settingsCenter.validation.range", { min: String(field.min ?? 0), max: String(field.max ?? "") });
    }
  }
  if (field.component === "duration" && (typeof value !== "string" || !DURATION_RE.test(value))) {
    return t("settingsCenter.validation.duration");
  }
  if (field.enum?.length && typeof value === "string" && !field.enum.includes(value)) return t("settingsCenter.validation.option");
  if (typeof value === "string" && value !== "") {
    if (field.path.endsWith("hfBaseURL") || field.path.endsWith("modelScopeBaseURL")) {
      let parsed: URL | undefined;
      try { parsed = new URL(value); } catch { parsed = undefined; }
      if (!parsed || (parsed.protocol !== "http:" && parsed.protocol !== "https:")
        || parsed.username || parsed.password || parsed.search || parsed.hash) {
        return t("settingsCenter.validation.url");
      }
    }
    if (field.path.endsWith("hfTokenEnv") || field.path.endsWith("modelScopeTokenEnv")) {
      if (!ENV_NAME_RE.test(value)) return t("settingsCenter.validation.envName");
    }
  }
  return undefined;
}

// Top-level keys of the draft that differ from the baseline, expressed as the
// RFC 6902 replace operations the structured settings draft API expects.
export function draftChanges(
  baseline: string,
  draftConfig: Record<string, unknown>,
): SettingsChange[] {
  const current = baseline ? JSON.parse(baseline) as Record<string, unknown> : {};
  return Object.entries(draftConfig)
    .filter(([key, value]) => JSON.stringify(value) !== JSON.stringify(current[key]))
    .map(([key, value]) => ({ op: "replace", path: `/${key}`, value }));
}
