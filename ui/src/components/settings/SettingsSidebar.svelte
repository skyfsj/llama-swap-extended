<script lang="ts">
  import {
    Activity,
    Boxes,
    FileText,
    Gauge,
    GitBranch,
    House,
    KeyRound,
    ScrollText,
    Sigma,
    SlidersHorizontal,
    Users,
    Waypoints,
  } from "@lucide/svelte";
  import type { Component } from "svelte";
  import { SETTINGS_NAVIGATION_GROUPS, type SettingsNavigationItem } from "../../lib/settingsNavigation";
  import { translate } from "../../lib/i18n";

  interface Props {
    items: SettingsNavigationItem[];
    activeID: string;
    onSelect: (id: string) => void;
  }

  let { items, activeID, onSelect }: Props = $props();

  const itemIcons: Record<string, Component> = {
    general: House,
    observability: Activity,
    modelFiles: FileText,
    models: Boxes,
    performance: Gauge,
    routing: GitBranch,
    selectors: Users,
    upstreamPeers: Waypoints,
    security: KeyRound,
    audit: ScrollText,
    macros: Sigma,
    advanced: SlidersHorizontal,
  };

  const groupIcons: Record<string, Component> = {
    basic: House,
    models: Boxes,
    scheduling: GitBranch,
    security: KeyRound,
    advanced: SlidersHorizontal,
  };

  function itemIcon(id: string): Component {
    return itemIcons[id] ?? SlidersHorizontal;
  }

  function groupIcon(group: string): Component {
    return groupIcons[group] ?? SlidersHorizontal;
  }

  // Items arrive already ordered by category (the navigation source array);
  // bucket them per category for the labelled groups.
  const grouped = $derived(
    SETTINGS_NAVIGATION_GROUPS
      .map((group) => ({ group, items: items.filter((item) => item.group === group) }))
      .filter((entry) => entry.items.length > 0),
  );
</script>

<aside class="settings-sidebar" aria-label={$translate("settingsCenter.sidebar.navigation")}>
  <nav class="settings-sidebar__nav" aria-label={$translate("settingsCenter.sidebar.sections")}>
    {#each grouped as entry (entry.group)}
      {@const GroupIcon = groupIcon(entry.group)}
      <div class="settings-sidebar__group">
        <div class="settings-sidebar__group-label">
          <GroupIcon aria-hidden="true" />
          <span>{$translate(`settingsCenter.categories.${entry.group}`)}</span>
        </div>
        {#each entry.items as item (item.id)}
          {@const Icon = itemIcon(item.id)}
          <button
            type="button"
            class:settings-sidebar__link--active={activeID === item.id}
            class="settings-sidebar__link"
            aria-current={activeID === item.id ? "page" : undefined}
            title={item.description}
            onclick={() => onSelect(item.id)}
          >
            <Icon aria-hidden="true" />
            <span>{item.label}</span>
          </button>
        {/each}
      </div>
    {/each}
  </nav>
</aside>

<style>
  /* Sub-navigation only: no logo, no product name, no brand icons. The app
     header lives above this in the shell; the settings page header above the
     body already carries the "设置" title. */
  .settings-sidebar {
    display: flex;
    flex-direction: column;
    inline-size: 13.5rem;
    flex: 0 0 13.5rem;
    min-inline-size: 0;
    box-sizing: border-box;
    overflow-y: auto;
    border-inline-end: 1px solid color-mix(in oklab, var(--border) 78%, transparent);
    background: color-mix(in oklab, var(--background) 98%, black 2%);
  }

  .settings-sidebar__nav {
    display: flex;
    flex-direction: column;
    gap: 1.1rem;
    padding: 0.9rem 0.55rem 1.5rem;
  }

  .settings-sidebar__group {
    display: grid;
    gap: 0.15rem;
  }

  .settings-sidebar__group-label {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    padding: 0.15rem 0.6rem 0.4rem;
    color: var(--muted-foreground);
    font-size: 0.68rem;
    font-weight: 700;
    letter-spacing: 0.06em;
    text-transform: uppercase;
  }

  .settings-sidebar__group-label :global(svg) {
    inline-size: 0.85rem;
    block-size: 0.85rem;
    flex: 0 0 auto;
  }

  .settings-sidebar__link {
    display: flex;
    align-items: center;
    gap: 0.55rem;
    inline-size: 100%;
    min-block-size: 2.15rem;
    box-sizing: border-box;
    border: 1px solid transparent;
    border-radius: 0.5rem;
    background: transparent;
    color: color-mix(in oklab, var(--foreground) 82%, var(--muted-foreground));
    padding-inline: 0.6rem;
    text-align: start;
    font: inherit;
    font-size: 0.84rem;
    font-weight: 500;
    cursor: pointer;
    transition: border-color 140ms ease, background-color 140ms ease, color 140ms ease;
  }

  .settings-sidebar__link:hover {
    border-color: color-mix(in oklab, var(--primary) 26%, transparent);
    background: color-mix(in oklab, var(--primary) 7%, transparent);
    color: var(--foreground);
  }

  .settings-sidebar__link:focus-visible {
    outline: 2px solid var(--ring);
    outline-offset: 1px;
  }

  .settings-sidebar__link--active {
    border-color: color-mix(in oklab, var(--primary) 42%, transparent);
    background: color-mix(in oklab, var(--primary) 12%, transparent);
    color: var(--primary);
    font-weight: 600;
  }

  .settings-sidebar__link :global(svg) {
    inline-size: 1rem;
    block-size: 1rem;
    flex: 0 0 auto;
  }

  .settings-sidebar__link span {
    overflow: hidden;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  /* Medium widths: collapse to a compact horizontal strip so the settings
     content keeps the width. Mobile: same strip, scrollable, no drawer needed
     since the primary sidebar is already a drawer at this width. */
  @media (max-width: 960px) {
    .settings-sidebar {
      inline-size: 100%;
      flex: 0 0 auto;
      max-block-size: 30vh;
      border-inline-end: 0;
      border-block-end: 1px solid color-mix(in oklab, var(--border) 78%, transparent);
    }

    .settings-sidebar__nav {
      flex-direction: row;
      flex-wrap: wrap;
      gap: 0.75rem 1.25rem;
      padding: 0.6rem 0.75rem;
    }

    .settings-sidebar__group {
      display: flex;
      flex-wrap: wrap;
      align-items: center;
      gap: 0.25rem;
    }

    .settings-sidebar__group-label {
      inline-size: 100%;
      padding: 0 0.15rem 0.25rem;
    }

    .settings-sidebar__link {
      inline-size: auto;
      min-block-size: 2rem;
      padding-inline: 0.55rem;
    }

    .settings-sidebar__link span {
      white-space: nowrap;
    }
  }
</style>
