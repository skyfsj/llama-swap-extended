import type { SettingsField } from "./settingsApi";

/**
 * Which editor the settings field renders as. The template switches on this
 * value instead of a chain of `{:else if}` tests so a new variant cannot be
 * shadowed by an earlier branch: the provider-list variant must win over the
 * plain select even though both match on `field.provider`.
 */
export type SettingsFieldVariant =
  | "sensitive-input"
  | "sensitive-status"
  | "switch"
  | "provider-list"
  | "select"
  | "list"
  | "map"
  | "textarea"
  | "input";

export function fieldEditorVariant(field: SettingsField): SettingsFieldVariant {
  if (field.sensitive && field.component !== "map" && field.component !== "json") {
    return "sensitive-input";
  }
  if (field.sensitive) {
    return "sensitive-status";
  }
  if (field.component === "switch") {
    return "switch";
  }
  // Provider-backed lists (e.g. hooks.on_startup.preload) render a multi-select
  // of model IDs; falling through to the single-value select would submit a
  // scalar where the configuration expects a list.
  if (field.component === "list" && !!field.provider) {
    return "provider-list";
  }
  if (!!field.provider || (field.enum?.length ?? 0) > 0) {
    return "select";
  }
  if (field.component === "list") {
    return "list";
  }
  if (field.component === "map") {
    return "map";
  }
  if (field.component === "textarea" || field.component === "json") {
    return "textarea";
  }
  return "input";
}
