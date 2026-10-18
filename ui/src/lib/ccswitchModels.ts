/**
 * The CC Switch import dialog offers only the models the selected API key may
 * use. The server resolves every spelling of a model group through the same
 * allowlist entry, so a key that names any spelling of a group unlocks the
 * whole group — this mirrors that rule.
 */

export interface ModelGroupLike {
  id: string;
  aliases?: string[];
}

/** A key with no allowlist is unrestricted; otherwise the group must be named. */
export function modelGroupAllowed(entry: ModelGroupLike, allowed: readonly string[]): boolean {
  if (allowed.length === 0) return true;
  if (allowed.includes(entry.id)) return true;
  return (entry.aliases ?? []).some((alias) => allowed.includes(alias));
}

/**
 * Flattens model groups into the selectable names for one key: a permitted
 * group contributes its id and every alias, so the link may target whichever
 * spelling the client expects.
 */
export function selectableModelNames(
  groups: readonly ModelGroupLike[],
  allowed: readonly string[],
): string[] {
  const values = new Set<string>();
  for (const group of groups) {
    if (!modelGroupAllowed(group, allowed)) continue;
    if (group.id) values.add(group.id);
    for (const alias of group.aliases ?? []) {
      if (alias) values.add(alias);
    }
  }
  return [...values].sort((left, right) => left.localeCompare(right, undefined, { numeric: true }));
}
