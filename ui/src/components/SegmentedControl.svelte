<script lang="ts">
  import { cn } from "$lib/utils.js";

  interface Props {
    items: readonly { readonly label: string }[];
    selected: number;
    onSelect: (index: number) => void;
    /** Optional leading label rendered before the buttons. */
    label?: string;
    class?: string;
  }

  let { items, selected, onSelect, label, class: className }: Props = $props();
</script>

<div class={cn("inline-flex items-center gap-1.5", className)}>
  {#if label}
    <span class="text-muted-foreground text-xs font-medium">{label}</span>
  {/if}
  <div
    class="inline-flex items-center rounded-lg border border-border/60 bg-muted/50 p-0.5 text-muted-foreground shadow-xs"
    role="tablist"
  >
    {#each items as item, i (i)}
      <button
        type="button"
        role="tab"
        aria-selected={selected === i}
        class={cn(
          "relative inline-flex items-center justify-center rounded-md px-2.5 py-1 text-xs font-medium transition-all select-none outline-none focus-visible:ring-2 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50 cursor-pointer",
          selected === i
            ? "bg-background text-foreground font-semibold shadow-xs"
            : "text-muted-foreground hover:bg-background/40 hover:text-foreground"
        )}
        onclick={() => onSelect(i)}
      >
        {item.label}
      </button>
    {/each}
  </div>
</div>

