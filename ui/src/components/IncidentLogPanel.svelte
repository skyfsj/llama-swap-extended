<script lang="ts">
  import { onMount } from "svelte";
  import { ArrowLeft, Bug, FileWarning, LoaderCircle, RefreshCw } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Card from "$lib/components/ui/card/index.js";
  import { fetchIncidentLog, fetchIncidentLogs, type IncidentLogEntry } from "../lib/incidentLogs";
  import { locale, localeToIntl, translate } from "../lib/i18n";

  let items = $state<IncidentLogEntry[]>([]);
  let loading = $state(true);
  let error = $state("");
  let selected = $state<IncidentLogEntry | null>(null);
  let detail = $state("");
  let detailLoading = $state(false);
  let detailError = $state("");
  let detailRequest = 0;

  async function load(): Promise<void> {
    loading = true;
    error = "";
    try {
      const response = await fetchIncidentLogs();
      items = response.items;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      loading = false;
    }
  }

  onMount(() => {
    void load();
  });

  async function openItem(item: IncidentLogEntry): Promise<void> {
    const request = ++detailRequest;
    selected = item;
    detail = "";
    detailError = "";
    detailLoading = true;
    try {
      detail = await fetchIncidentLog(item.name);
    } catch (cause) {
      if (request === detailRequest) detailError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (request === detailRequest) detailLoading = false;
    }
  }

  function closeItem(): void {
    detailRequest += 1;
    selected = null;
    detail = "";
    detailError = "";
    detailLoading = false;
  }

  function kindLabel(kind: IncidentLogEntry["kind"]): string {
    return kind === "inference-crash"
      ? $translate("logs.incidentCrash")
      : $translate("logs.incidentRequestError");
  }

  function formatDate(value: string): string {
    const date = new Date(value);
    return Number.isNaN(date.getTime())
      ? value
      : new Intl.DateTimeFormat(localeToIntl($locale), { dateStyle: "short", timeStyle: "medium" }).format(date);
  }

  function formatBytes(value: number): string {
    if (!Number.isFinite(value) || value < 1024) return `${Math.max(0, value || 0)} B`;
    if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`;
    return `${(value / (1024 * 1024)).toFixed(1)} MB`;
  }
</script>

<Card.Root class="flex h-full min-h-0 flex-col gap-0 overflow-hidden py-0">
  {#if selected}
  <Card.Header class="shrink-0 border-b px-4 py-2">
    <Card.Title class="flex items-center gap-2 text-sm font-semibold">
      <Button variant="ghost" size="icon-sm" type="button" onclick={closeItem} aria-label={$translate("logs.incidentBack")} title={$translate("logs.incidentBack")}>
        <ArrowLeft class="size-4" aria-hidden="true" />
      </Button>
      <span>{kindLabel(selected.kind)}</span>
    </Card.Title>
    <Card.Description class="text-xs">
      {formatDate(selected.createdAt)}{selected.model ? ` · ${selected.model}` : ""}
    </Card.Description>
  </Card.Header>
  {:else}
    <div class="flex shrink-0 justify-end px-2 py-1">
      <Button variant="ghost" size="icon-sm" onclick={() => void load()} disabled={loading} aria-label={$translate("logs.incidentRefresh")} title={$translate("logs.incidentRefresh")}>
        {#if loading}<LoaderCircle class="animate-spin" aria-hidden="true" />{:else}<RefreshCw aria-hidden="true" />{/if}
      </Button>
    </div>
  {/if}

  <Card.Content class="min-h-0 flex-1 overflow-hidden p-0">
    {#if selected}
      <div class="flex h-full min-h-0 flex-col">
        <div class="text-muted-foreground flex shrink-0 items-center justify-between gap-3 border-b px-4 py-2 text-xs">
          <span class="min-w-0 truncate">{selected.name}</span>
          <span class="shrink-0 tabular-nums">{formatBytes(selected.size)}</span>
        </div>
        {#if detailLoading}
          <div class="text-muted-foreground flex flex-1 items-center justify-center gap-2 text-xs" role="status" aria-live="polite">
            <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
            {$translate("logs.incidentDetailLoading")}
          </div>
        {:else if detailError}
          <div class="text-destructive flex flex-1 items-center justify-center gap-2 px-4 text-xs" role="alert">
            <FileWarning class="size-3.5" aria-hidden="true" />
            {$translate("logs.incidentDetailLoadFailed", { message: detailError })}
          </div>
        {:else}
          <pre class="bg-muted/20 min-h-0 flex-1 overflow-auto whitespace-pre-wrap break-words p-4 font-mono text-xs leading-relaxed" aria-label={$translate("logs.incidentDetail")}>{detail}</pre>
        {/if}
      </div>
    {:else if loading && items.length === 0}
      <div class="text-muted-foreground flex h-full items-center justify-center gap-2 px-4 py-3 text-xs" role="status" aria-live="polite">
        <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
        {$translate("logs.incidentLoading")}
      </div>
    {:else if error}
      <div class="text-destructive flex h-full items-center justify-center gap-2 px-4 py-3 text-xs" role="alert">
        <FileWarning class="size-3.5" aria-hidden="true" />
        {$translate("logs.incidentLoadFailed", { message: error })}
      </div>
    {:else if items.length === 0}
      <div class="text-muted-foreground flex h-full items-center justify-center px-4 py-3 text-xs" role="status">
        {$translate("logs.incidentEmpty")}
      </div>
    {:else}
      <ul class="h-full divide-y overflow-auto">
        {#each items as item (item.name)}
          <li>
            <button
              class="hover:bg-muted/60 focus-visible:bg-muted/60 flex w-full items-center gap-3 px-4 py-3 text-left text-xs outline-none focus-visible:ring-2 focus-visible:ring-inset focus-visible:ring-ring/50"
              type="button"
              onclick={() => void openItem(item)}
              aria-label={`${kindLabel(item.kind)} ${formatDate(item.createdAt)}`}
              title={$translate("logs.incidentOpen")}
            >
              {#if item.kind === "inference-crash"}<Bug class="size-3.5 shrink-0 text-destructive" aria-hidden="true" />{:else}<FileWarning class="size-3.5 shrink-0 text-amber-500" aria-hidden="true" />{/if}
              <span class="min-w-0 flex-1 truncate font-medium">{kindLabel(item.kind)}{item.model ? ` · ${item.model}` : ""}</span>
              <span class="text-muted-foreground shrink-0 tabular-nums">{formatDate(item.createdAt)} · {formatBytes(item.size)}</span>
            </button>
          </li>
        {/each}
      </ul>
    {/if}
  </Card.Content>
</Card.Root>
