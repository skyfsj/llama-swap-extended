<script lang="ts">
  import { tick } from "svelte";
  import { persistentStore } from "../stores/persistent";
  import { isDarkMode } from "../stores/theme";
  import { ansiToHtml } from "../lib/ansi";
  import { Type, WrapText, Search, SearchX, CircleX } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import * as Card from "$lib/components/ui/card/index.js";
  import { translate } from "../lib/i18n";

  interface Props {
    id: string;
    title: string;
    logData: string;
  }

  let { id, title, logData }: Props = $props();

  let filterRegex = $state("");

  // Create persistent stores for this panel (id is intentionally captured at init time)
  // svelte-ignore state_referenced_locally
  const fontSizeStore = persistentStore<"xxs" | "xs" | "small" | "normal">(`logPanel-${id}-fontSize`, "normal");
  // svelte-ignore state_referenced_locally
  const wrapTextStore = persistentStore<boolean>(`logPanel-${id}-wrapText`, false);
  // svelte-ignore state_referenced_locally
  const showFilterStore = persistentStore<boolean>(`logPanel-${id}-showFilter`, false);

  let textWrapClass = $derived($wrapTextStore ? "whitespace-pre-wrap" : "whitespace-pre");

  function toggleFontSize(): void {
    fontSizeStore.update((prev) => {
      switch (prev) {
        case "xxs": return "xs";
        case "xs": return "small";
        case "small": return "normal";
        case "normal": return "xxs";
      }
    });
  }

  function toggleWrapText(): void {
    wrapTextStore.update((prev) => !prev);
  }

  function toggleFilter(): void {
    if ($showFilterStore) {
      showFilterStore.set(false);
      filterRegex = "";
    } else {
      showFilterStore.set(true);
    }
  }

  let fontSizeClass = $derived.by(() => {
    switch ($fontSizeStore) {
      case "xxs": return "text-[0.5rem]";
      case "xs": return "text-[0.75rem]";
      case "small": return "text-[0.875rem]";
      case "normal": return "text-base";
    }
  });

  let filteredLogs = $derived.by(() => {
    if (!filterRegex) return logData;
    try {
      const regex = new RegExp(filterRegex, "i");
      return logData.split("\n").filter((line) => regex.test(line)).join("\n");
    } catch {
      return logData;
    }
  });

  let renderedLogs = $derived(ansiToHtml(filteredLogs, $isDarkMode));

  let preElement: HTMLPreElement;
  let userScrolledUp = $state(false);

  function handleScroll() {
    if (!preElement) return;
    const { scrollTop, scrollHeight, clientHeight } = preElement;
    userScrolledUp = scrollHeight - scrollTop - clientHeight > 40;
  }

  function scrollToBottom(): void {
    if (preElement) preElement.scrollTop = preElement.scrollHeight;
  }

  // Keep the tail visible after Svelte has committed new log text. The
  // previous synchronous assignment could run before the <pre> scrollHeight
  // was updated, which left the panel one or more lines above the bottom.
  $effect(() => {
    // Read these values so font/wrap changes also re-anchor the tail.
    const logs = filteredLogs;
    const fontSize = $fontSizeStore;
    const wrapText = $wrapTextStore;
    void logs;
    void fontSize;
    void wrapText;

    if (!preElement || userScrolledUp) return;

    let cancelled = false;
    void tick().then(() => {
      if (!cancelled && !userScrolledUp) scrollToBottom();
    });

    // Reflow caused by font loading or wrapping can change the scroll height
    // without changing the log string itself.
    const observer = typeof ResizeObserver === "undefined" ? undefined : new ResizeObserver(() => {
      if (!userScrolledUp) scrollToBottom();
    });
    observer?.observe(preElement);

    return () => {
      cancelled = true;
      observer?.disconnect();
    };
  });
</script>

<Card.Root class="bg-muted/30 h-full w-full gap-0 overflow-hidden py-0">
  <Card.Header class="border-b px-4 py-2">
    <Card.Title class="text-sm font-semibold">{title}</Card.Title>
    <Card.Action>
      <div class="flex items-center gap-1">
        <Button variant="ghost" size="icon-sm" onclick={toggleFontSize} title={$translate("logs.changeFontSize")}>
          <Type />
        </Button>
        <Button variant="ghost" size="icon-sm" onclick={toggleWrapText} title={$translate("logs.toggleWrap")}>
          <WrapText class={$wrapTextStore ? "text-primary" : ""} />
        </Button>
        <Button variant="ghost" size="icon-sm" onclick={toggleFilter} title={$translate("logs.toggleFilter")}>
          {#if $showFilterStore}<SearchX />{:else}<Search />{/if}
        </Button>
      </div>
    </Card.Action>
    {#if $showFilterStore}
      <div class="flex w-full items-center gap-2 pt-2">
        <Input type="text" class="h-8" placeholder={$translate("logs.filterPlaceholder")} bind:value={filterRegex} />
        <Button variant="ghost" size="icon-sm" onclick={() => (filterRegex = "")} aria-label={$translate("logs.clearFilter")}>
          <CircleX />
        </Button>
      </div>
    {/if}
  </Card.Header>
  <Card.Content class="bg-background min-h-0 flex-1 p-0 font-mono text-sm">
    <pre bind:this={preElement} onscroll={handleScroll} class="{textWrapClass} {fontSizeClass} h-full overflow-auto p-4">{@html renderedLogs}</pre>
  </Card.Content>
</Card.Root>
