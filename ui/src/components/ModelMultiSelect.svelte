<script lang="ts">
  import { Check, ChevronDown } from "@lucide/svelte";
  import * as DropdownMenu from "$lib/components/ui/dropdown-menu/index.js";

  /** One model with the alias names that resolve to it. */
  export interface ModelOptionGroup {
    model: string;
    aliases: string[];
  }

  interface Props {
    value: string[];
    options?: string[];
    /**
     * Model groups: each model followed by its aliases, which are routable
     * names for the model above them rather than separate models. Takes
     * precedence over `options` when both are given.
     */
    groups?: ModelOptionGroup[];
    disabled?: boolean;
    ariaLabel: string;
    allLabel: string;
    selectedLabel: string;
    emptyLabel: string;
    /**
     * Makes aliases read-only children of their model: selecting the model
     * selects every name it answers to, and no name can be selected alone.
     * Defaults to true for callers that pass `groups`.
     */
    aliasSelectionReadOnly?: boolean;
    // onValueChange lets a controlled caller react to a selection without owning
    // the state. Callers that use bind:value can omit it.
    onValueChange?: (next: string[]) => void;
  }

  let {
    value = $bindable(),
    options = [],
    groups,
    disabled = false,
    ariaLabel,
    allLabel,
    selectedLabel,
    emptyLabel,
    onValueChange,
    aliasSelectionReadOnly = true,
  }: Props = $props();
  let open = $state(false);
  // Grouped options stay in model order; the flat fallback keeps the previous
  // alphabetical single list.
  let grouped = $derived(
    (groups ?? []).map((group) => ({
      model: group.model,
      aliases: (group.aliases ?? []).filter((alias) => alias && alias !== group.model),
    }))
  );
  let menuOptions = $derived(
    grouped.length > 0
      ? [...new Set([...grouped.flatMap((group) => [group.model, ...group.aliases]), ...value])].filter(
          (option, index, all) => all.indexOf(option) === index
        )
      : [...new Set([...options, ...value])].sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))
  );
  let summary = $derived(value.length === 0 ? allLabel : selectedLabel);

  function clearSelection(): void {
    value = [];
    open = false;
    onValueChange?.(value);
  }

  // Aliases belong to their model: selecting the model selects every routing
  // name it answers to, and clearing any one name clears the whole group. A
  // key restricted to a model must accept it under each of its names, so an
  // allowlist that could name a model while rejecting its aliases would be a
  // trap.
  // A model is selected once, by one name. The server resolves every spelling
  // of that model (its client-facing name, each alias, the provider-qualified
  // form) when it authorizes a request, so storing a copy per spelling would
  // only make the operator register the same model twice. A set that already
  // carries an alias spelling is normalized to the model's own name so the
  // stored list stays one entry per model.
  $effect(() => {
    if (!aliasSelectionReadOnly || grouped.length === 0) return;
    const selected = new Set(value);
    let changed = false;
    for (const group of grouped) {
      const names = [group.model, ...group.aliases];
      const picked = names.filter((name) => selected.has(name));
      if (picked.length === 0) continue;
      if (picked.length === 1 && picked[0] === group.model) continue;
      for (const alias of group.aliases) selected.delete(alias);
      selected.add(group.model);
      changed = true;
    }
    if (!changed) return;
    value = menuOptions.filter((item) => selected.has(item));
    onValueChange?.(value);
  });

  function groupFor(option: string): ModelOptionGroup | undefined {
    return grouped.find((group) => group.model === option || group.aliases.includes(option));
  }

  function toggleSelection(option: string, checked: boolean): void {
    const group = groupFor(option);
    const selected = new Set(value);
    if (group) {
      // Selecting the model selects the model: its aliases are shown as
      // selected because the same model answers to them.
      for (const alias of group.aliases) selected.delete(alias);
      if (checked) selected.add(group.model);
      else selected.delete(group.model);
    } else if (checked) {
      selected.add(option);
    } else {
      selected.delete(option);
    }
    value = menuOptions.filter((item) => selected.has(item));
    onValueChange?.(value);
  }
</script>

<DropdownMenu.Root bind:open>
  <DropdownMenu.Trigger
    class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 inline-flex h-9 w-full min-w-0 items-center justify-between gap-2 rounded-lg border px-3 text-left text-sm outline-none focus-visible:ring-3 disabled:pointer-events-none disabled:opacity-50"
    {disabled}
    aria-label={ariaLabel}
    title={summary}
  >
    <span class="min-w-0 truncate">{summary}</span>
    <ChevronDown class="text-muted-foreground size-4 shrink-0" aria-hidden="true" />
  </DropdownMenu.Trigger>
  <DropdownMenu.Content align="start" class="max-h-[60vh] min-w-[min(20rem,calc(100vw-2rem))] max-w-[calc(100vw-2rem)]">
    <DropdownMenu.Label class="text-muted-foreground px-2 py-1.5 text-xs font-medium">{ariaLabel}</DropdownMenu.Label>
    <DropdownMenu.Item onclick={clearSelection}>
      <Check class={value.length === 0 ? "size-4" : "size-4 opacity-0"} aria-hidden="true" />
      <span class="truncate">{allLabel}</span>
    </DropdownMenu.Item>
    {#if menuOptions.length === 0}
      <p class="text-muted-foreground px-2 py-2 text-xs" role="status">{emptyLabel}</p>
    {:else}
      <DropdownMenu.Separator />
      {#if grouped.length > 0}
        {#each grouped as group (group.model)}
          <DropdownMenu.CheckboxItem
            checked={value.includes(group.model) || group.aliases.some((alias) => value.includes(alias))}
            onCheckedChange={(checked) => toggleSelection(group.model, !!checked)}
            closeOnSelect={false}
          >
            <span class="min-w-0 truncate">{group.model}</span>
          </DropdownMenu.CheckboxItem>
          {#each group.aliases as alias (alias)}
            {#if aliasSelectionReadOnly}
              <!-- The alias is part of its model: it follows the model's own
                   checkbox and is listed only so the operator knows the key
                   also answers to that name. A plain row keeps it out of the
                   menu's keyboard navigation. -->
              <div class="text-muted-foreground flex items-center gap-1.5 py-0.5 pl-6 text-xs">
                <span class="flex size-3.5 shrink-0 items-center justify-center">
                  {#if value.includes(group.model) || group.aliases.some((alias) => value.includes(alias))}
                    <Check class="size-3" aria-hidden="true" />
                  {/if}
                </span>
                <span class="min-w-0 truncate">↳ {alias}</span>
              </div>
            {:else}
              <DropdownMenu.CheckboxItem
                checked={value.includes(alias)}
                onCheckedChange={(checked) => toggleSelection(alias, !!checked)}
                closeOnSelect={false}
                class="text-muted-foreground pl-6 text-xs"
              >
                <span class="min-w-0 truncate">↳ {alias}</span>
              </DropdownMenu.CheckboxItem>
            {/if}
          {/each}
        {/each}
      {:else}
        {#each menuOptions as option (option)}
          <DropdownMenu.CheckboxItem
            checked={value.includes(option)}
            onCheckedChange={(checked) => toggleSelection(option, !!checked)}
            closeOnSelect={false}
          >
            <span class="min-w-0 truncate">{option}</span>
          </DropdownMenu.CheckboxItem>
        {/each}
      {/if}
    {/if}
  </DropdownMenu.Content>
</DropdownMenu.Root>
