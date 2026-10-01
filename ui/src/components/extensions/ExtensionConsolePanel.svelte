<script lang="ts">
  import { onMount, onDestroy } from "svelte";
  import { Trash2, Pause, Play } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";
  import { fetchExtensionLogHistory, streamExtensionLogs } from "../../lib/extensionsApi";

  interface Props {
    /** extension id whose logs to stream */
    id: string;
    /** compact styling for the editor dock */
    compact?: boolean;
  }

  let { id, compact = false }: Props = $props();

  // Chrome-console style panel: newest at the bottom, level colors, search
  // filter, pause scrolling, clear view, and a persisted tail from the server.
  interface LogRow {
    level: string;
    message: string;
    ts: string;
  }

  let rows = $state<LogRow[]>([]);
  let search = $state("");
  let paused = $state(false);
  let levelFilter = $state<"all" | "info" | "warn" | "error" | "debug">("all");
  let loading = $state(true);
  let scrollBox: HTMLDivElement | undefined;
  let controller: AbortController | null = null;
  const MAX_ROWS = 5000;

  let buffer = "";
  let pending: LogRow[] = [];

  // The stream writes "…\n[ts] [LEVEL] msg\n…"; rows split on newlines and
  // level-tagged lines become structured rows.
  function pushChunk(chunk: string): void {
    buffer += chunk;
    const parts = buffer.split("\n");
    buffer = parts.pop() ?? "";
    for (const line of parts) {
      const trimmed = line.trim();
      if (!trimmed || trimmed.startsWith(":")) continue; // keepalive
      const match = /^\[([^\]]+)\] \[([A-Z]+)\] (.*)$/.exec(trimmed);
      if (match) {
        pending.push({ ts: match[1], level: match[2].toLowerCase(), message: match[3] });
      } else {
        // Continuation of a multi-line record (e.g. exception stack).
        const last = pending.at(-1);
        if (last) last.message += "\n" + trimmed;
        else pending.push({ ts: "", level: "info", message: trimmed });
      }
    }
    flushPending();
  }

  function flushPending(): void {
    if (pending.length === 0) return;
    rows = [...rows, ...pending].slice(-MAX_ROWS);
    pending = [];
    if (!paused && scrollBox) scrollBox.scrollTop = scrollBox.scrollHeight;
  }

  const filtered = $derived(
    rows.filter((row) => {
      if (levelFilter !== "all" && row.level !== levelFilter) return false;
      if (search.trim() && !row.message.toLowerCase().includes(search.trim().toLowerCase())) return false;
      return true;
    }),
  );

  const levelColor: Record<string, string> = {
    info: "text-foreground/80",
    debug: "text-muted-foreground",
    warn: "text-warning",
    error: "text-destructive",
  };

  onMount(() => {
    controller = new AbortController();
    (async () => {
      const history = await fetchExtensionLogHistory(id);
      if (history) pushChunk(history);
      loading = false;
      try {
        for await (const chunk of streamExtensionLogs(id, { signal: controller?.signal })) {
          pushChunk(chunk);
        }
      } catch { /* client disconnect or server restart: view keeps the tail */ }
    })();
  });

  onDestroy(() => controller?.abort());

  function clearView(): void {
    rows = [];
    pending = [];
  }
</script>

<div class="flex h-full min-h-0 flex-col">
  <div class="flex flex-wrap items-center gap-1.5 border-b border-border p-1.5">
    <Input class="h-7 w-44 text-xs" bind:value={search} placeholder={$translate("extensions.logs.search")} aria-label={$translate("extensions.logs.search")} />
    <div class="flex gap-0.5">
      {#each ["all", "debug", "info", "warn", "error"] as level}
        <button class={`rounded px-2 py-0.5 text-xs ${levelFilter === level ? "bg-primary/15 font-semibold text-primary" : "text-muted-foreground hover:bg-muted"}`} aria-pressed={levelFilter === level} onclick={() => levelFilter = level as typeof levelFilter}>
          {level === "all" ? $translate("extensions.logs.allLevels") : level}
        </button>
      {/each}
    </div>
    <div class="ml-auto flex gap-1">
      <Button variant="ghost" size="sm" aria-pressed={paused} onclick={() => paused = !paused} title={$translate("extensions.logs.pause")}>
        {#if paused}<Play class="size-3.5" />{:else}<Pause class="size-3.5" />{/if}
      </Button>
      <Button variant="ghost" size="sm" aria-label={$translate("extensions.logs.clearView")} onclick={clearView}><Trash2 class="size-3.5" /></Button>
    </div>
  </div>
  <div bind:this={scrollBox} class="console-scroll min-h-0 flex-1 overflow-y-auto bg-background font-mono text-xs leading-relaxed" class:compact>
    {#if loading && rows.length === 0}
      <p class="p-4 text-muted-foreground">{$translate("common.loading")}</p>
    {:else if filtered.length === 0}
      <p class="p-4 text-muted-foreground">{$translate("extensions.logs.empty")}</p>
    {:else}
      {#each filtered as row, index (index)}
        <div class="flex gap-2 border-b border-border/40 px-2 py-0.5 hover:bg-muted/40">
          <span class="shrink-0 text-muted-foreground/60">{row.ts.slice(11) || "--:--:--"}</span>
          <span class={`w-10 shrink-0 uppercase ${levelColor[row.level] ?? "text-muted-foreground"}`}>{row.level}</span>
          <span class="min-w-0 flex-1 whitespace-pre-wrap break-all">{row.message}</span>
        </div>
      {/each}
    {/if}
  </div>
</div>
<style>
  .console-scroll.compact { font-size: 11px; }
</style>
