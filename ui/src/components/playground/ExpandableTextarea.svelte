<script lang="ts">
  import { untrack, type Snippet } from "svelte";
  import { Maximize2, X } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import { translate } from "../../lib/i18n";

  interface Props {
    value: string;
    ref?: HTMLTextAreaElement | null;
    placeholder?: string;
    rows?: number;
    disabled?: boolean;
    onkeydown?: (event: KeyboardEvent) => void;
    /** Strip the frame so the textarea sits inside a surrounding composer surface. */
    bare?: boolean;
    /**
     * Fill the available height instead of growing with content: the textarea
     * scrolls internally and the toolbar row is pinned to the bottom.
     */
    fill?: boolean;
    /** Controls rendered in a row below the textarea; owns the expand action. */
    toolbar?: Snippet<[{ expand: () => void }]>;
  }

  let {
    value = $bindable(),
    ref = $bindable(null),
    placeholder = "",
    rows = 3,
    disabled = false,
    onkeydown,
    bare = false,
    fill = false,
    toolbar,
  }: Props = $props();
  let isExpanded = $state(false);
  let expandedValue = $state("");
  let expandedTextarea: HTMLTextAreaElement | undefined = $state();

  function openExpanded() {
    expandedValue = value;
    isExpanded = true;
  }

  function closeExpanded() {
    isExpanded = false;
  }

  function saveExpanded() {
    value = expandedValue;
    isExpanded = false;
  }

  function handleKeyDown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      closeExpanded();
    }
  }

  // Focus the textarea when expanded view opens
  $effect(() => {
    if (isExpanded && expandedTextarea) {
      expandedTextarea.focus();
      const len = untrack(() => expandedValue.length);
      expandedTextarea.setSelectionRange(len, len);
    }
  });
</script>

<div class="group relative flex min-h-0 flex-1 flex-col" class:gap-1.5={toolbar !== undefined}>
  <Textarea
    class={`resize-none${toolbar === undefined ? " pr-10" : ""}${bare ? " pg-bare-textarea" : ""}${fill ? " min-h-0 flex-1 field-sizing-fixed" : ""}`}
    bind:ref
    {placeholder}
    {rows}
    bind:value
    {onkeydown}
    {disabled}
  />
  {#if toolbar}
    <div class="flex shrink-0 items-center gap-1.5" class:mt-auto={fill}>
      {@render toolbar({ expand: openExpanded })}
    </div>
  {:else}
    <Button
      variant="outline"
      size="icon-sm"
      class="absolute right-2 top-2 opacity-60 transition-opacity group-hover:opacity-100 md:opacity-0"
      onclick={openExpanded}
      title={$translate("playground.expandable.expandToEdit")}
      type="button"
      {disabled}
    >
      <Maximize2 />
    </Button>
  {/if}
</div>

{#if isExpanded}
  <div class="fixed inset-0 z-50 flex items-center justify-center bg-black/50 p-4">
    <div class="pg-float flex h-[80vh] w-full max-w-4xl flex-col">
      <!-- Header -->
      <div class="flex items-center justify-between border-b pg-divide px-4 py-3">
        <h3 class="pb-0 font-medium">{$translate("playground.expandable.editText")}</h3>
        <Button variant="ghost" size="icon-sm" class="pg-tool" onclick={closeExpanded} title={$translate("common.close")} type="button">
          <X />
        </Button>
      </div>

      <!-- Textarea -->
      <div class="flex-1 p-4">
        <Textarea
          bind:ref={expandedTextarea}
          class="h-full resize-none"
          {placeholder}
          bind:value={expandedValue}
          onkeydown={handleKeyDown}
        />
      </div>

      <!-- Footer -->
      <div class="flex justify-end gap-2 border-t pg-divide p-4">
        <Button variant="outline" class="pg-control" onclick={closeExpanded} type="button">{$translate("common.cancel")}</Button>
        <Button class="pg-action" onclick={saveExpanded} type="button">{$translate("common.done")}</Button>
      </div>
    </div>
  </div>
{/if}
