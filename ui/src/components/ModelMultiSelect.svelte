<script lang="ts">
  import { Check, ChevronDown } from "@lucide/svelte";
  import * as DropdownMenu from "$lib/components/ui/dropdown-menu/index.js";

  interface Props {
    value: string[];
    options?: string[];
    disabled?: boolean;
    ariaLabel: string;
    allLabel: string;
    selectedLabel: string;
    emptyLabel: string;
    // onValueChange lets a controlled caller react to a selection without owning
    // the state. Callers that use bind:value can omit it.
    onValueChange?: (next: string[]) => void;
  }

  let {
    value = $bindable(),
    options = [],
    disabled = false,
    ariaLabel,
    allLabel,
    selectedLabel,
    emptyLabel,
    onValueChange,
  }: Props = $props();
  let open = $state(false);
  let menuOptions = $derived([...new Set([...options, ...value])].sort((a, b) => a.localeCompare(b, undefined, { numeric: true })));
  let summary = $derived(value.length === 0 ? allLabel : selectedLabel);

  function clearSelection(): void {
    value = [];
    open = false;
    onValueChange?.(value);
  }

  function toggleSelection(option: string, checked: boolean): void {
    const selected = new Set(value);
    if (checked) selected.add(option);
    else selected.delete(option);
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
  </DropdownMenu.Content>
</DropdownMenu.Root>
