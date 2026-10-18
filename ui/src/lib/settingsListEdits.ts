import type { SettingsChange } from "./settingsApi";

/**
 * Model and change operations for a sensitive sequence (apiKeys).
 *
 * The configuration accepts entries as plain secrets or as structured objects,
 * and the server hands the form a redacted copy: every secret arrives as the
 * "[REDACTED]" sentinel. Two consequences drive this module:
 *
 * 1. Rows the operator did not touch must never be written back. A masked row
 *    carries no identity, so re-sending the whole list with sentinels makes the
 *    server restore secrets by position — deleting the first of two rows would
 *    put the first secret back. The editor therefore emits one index-scoped
 *    operation per edit and leaves every other entry alone.
 * 2. The loader rejects empty entries ("empty api key found in apiKeys"), so a
 *    new row only reaches the draft once it has a value and an emptied row keeps
 *    the stored value instead of writing "".
 *
 * A redacted row renders its mask as a placeholder rather than as the field
 * value, so typing cannot append to the mask.
 */

export const REDACTED = "[REDACTED]";
export const REDACTED_MASK = "••••••••••••";

const SECRET_FIELDS = ["key", "value", "secret"] as const;

export type SecretRow =
  | { kind: "scalar"; index: number; redacted: boolean; secret: string; entry: string }
  | {
      kind: "entry";
      index: number;
      label: string;
      field: string;
      redacted: boolean;
      secret: string;
      entry: Record<string, unknown>;
    };

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

/**
 * Projects the raw config value into render rows. A structured entry keeps its
 * object so an edit to its secret cannot drop the name, scopes or models that
 * the rest of the entry carries.
 */
export function secretRows(value: unknown): SecretRow[] {
  if (!Array.isArray(value)) return [];
  return value.map((item, index) => {
    if (isRecord(item)) {
      const field = SECRET_FIELDS.find((candidate) => typeof item[candidate] === "string") ?? "key";
      const secret = typeof item[field] === "string" ? (item[field] as string) : "";
      const label = [item.name, item.id].find((candidate) => typeof candidate === "string" && candidate !== "") as
        | string
        | undefined;
      return {
        kind: "entry" as const,
        index,
        label: label ?? "",
        field,
        redacted: secret === REDACTED,
        secret,
        entry: item,
      };
    }
    const secret = typeof item === "string" ? item : item === undefined || item === null ? "" : String(item);
    return { kind: "scalar" as const, index, redacted: secret === REDACTED, secret, entry: secret };
  });
}

export function listAddOp(path: string, value: string): SettingsChange {
  return { op: "add", path: `${path}/-`, value };
}

export function listRemoveOp(path: string, index: number): SettingsChange {
  return { op: "remove", path: `${path}/${index}` };
}

export function listReplaceOp(path: string, index: number, value: unknown): SettingsChange {
  return { op: "replace", path: `${path}/${index}`, value };
}

/** Applies a row edit to the draft array without touching other entries. */
export function replaceRow(value: unknown, index: number, next: unknown): unknown[] {
  const items = Array.isArray(value) ? [...value] : [];
  items[index] = next;
  return items;
}

export function removeRow(value: unknown, index: number): unknown[] {
  return (Array.isArray(value) ? value : []).filter((_, itemIndex) => itemIndex !== index);
}

export function appendRow(value: unknown, next: string): unknown[] {
  return [...(Array.isArray(value) ? value : []), next];
}

/**
 * Merges the operations recorded by list editors with the changes derived from
 * the draft diff. The diff replaces a changed top-level key wholesale, which is
 * exactly what a redacted list cannot survive, so a path with recorded
 * operations contributes those instead. Operation order is the order the
 * operator worked in: each index refers to the list as it stood at that moment.
 */
export function mergeDraftChanges(
  generated: SettingsChange[],
  recorded: Record<string, SettingsChange[]>,
): SettingsChange[] {
  const recordedPaths = new Set(Object.keys(recorded).filter((path) => (recorded[path]?.length ?? 0) > 0));
  const derived = generated.filter((change) => !recordedPaths.has(change.path));
  const recordedChanges = Object.keys(recorded)
    .filter((path) => recordedPaths.has(path))
    .flatMap((path) => recorded[path]);
  return [...derived, ...recordedChanges];
}
