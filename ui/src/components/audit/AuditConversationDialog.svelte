<script lang="ts">
  import { tick } from "svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { translate } from "../../lib/i18n";
  import { auditTranscript, collapseDataUrls, decodeAuditBytes, fetchAuditConversationBody, formatAuditBody, type AuditBodyView, type AuditBytes, type AuditEvent } from "../../lib/audit";
  import { parseSpeedTimeline } from "../../lib/speedTimeline";
  import type { AuditConversation } from "../../lib/types";
  import AuditTranscript from "./AuditTranscript.svelte";
  import PhaseShareBar from "./PhaseShareBar.svelte";
  import SpeedCurve from "./SpeedCurve.svelte";

  interface Props {
    conversation: AuditConversation | null;
    open: boolean;
    loading?: boolean;
    error?: string;
    onclose: () => void;
  }

  let { conversation, open, loading = false, error = "", onclose }: Props = $props();

  function bodyText(value: AuditBytes): string {
    // The raw view collapses multimodal payloads into sized placeholders; the
    // transcript above renders the actual image/file items.
    return collapseDataUrls(decodeAuditBytes(value));
  }

  function bodyView(value: AuditBytes): AuditBodyView {
    return formatAuditBody(value);
  }

  function formatDate(value: string): string {
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? value : parsed.toLocaleString();
  }

  function headersView(value: string | number[] | undefined): string {
    const raw = bodyText(value);
    if (!raw.trim()) return "";
    try {
      return JSON.stringify(JSON.parse(raw), null, 2);
    } catch {
      return raw;
    }
  }

  function summarizeEvents(events: AuditEvent[]): { type: string; count: number; characters: number; finishReason: string }[] {
    const groups = new Map<string, { type: string; count: number; characters: number; finishReason: string }>();
    for (const event of events) {
      const group = groups.get(event.type) ?? { type: event.type, count: 0, characters: 0, finishReason: "" };
      group.count++;
      group.characters += event.text.length;
      if (event.finishReason) group.finishReason = event.finishReason;
      groups.set(event.type, group);
    }
    return [...groups.values()];
  }

  function totalTokens(item: AuditConversation): number {
    return item.inputTokens + item.outputTokens;
  }

  function displayID(item: AuditConversation): string {
    return item.activityId && item.activityId > 0 ? String(item.activityId) : "—";
  }

  function percent(value?: number): string {
    return `${((value ?? 0) * 100).toFixed(1)}%`;
  }

  // The detail response carries only body references, so the dialog opens on the
  // metadata and streams each transcript body afterwards. A conversation with
  // attachments is otherwise tens of megabytes before anything is visible.
  let streamedBodies = $state<{ request?: Uint8Array; response?: Uint8Array }>({});
  let bodiesLoading = $state(false);
  let bodiesError = $state("");
  let requestBody = $derived<AuditBytes>(conversation?.requestBody ?? streamedBodies.request);
  let responseBody = $derived<AuditBytes>(conversation?.responseBody ?? streamedBodies.response);
  let responseView = $derived(conversation ? bodyView(responseBody) : null);
  let transcript = $derived(conversation ? auditTranscript(requestBody, responseBody) : []);
  let speedPoints = $derived(conversation ? parseSpeedTimeline(conversation.speedTimeline) : []);
  let transcriptScroller = $state<HTMLDivElement | undefined>(undefined);

  $effect(() => {
    const item = conversation;
    streamedBodies = {};
    bodiesError = "";
    if (!open || !item) {
      bodiesLoading = false;
      return;
    }
    // A store that keeps bodies inline (in-memory) has nothing to stream.
    const wantRequest = Boolean(item.requestBodyRef) && item.requestBody === undefined;
    const wantResponse = Boolean(item.responseBodyRef) && item.responseBody === undefined;
    if (!wantRequest && !wantResponse) {
      bodiesLoading = false;
      return;
    }
    const id = item.activityId && item.activityId > 0 ? String(item.activityId) : item.id;
    let cancelled = false;
    bodiesLoading = true;
    const jobs: Promise<void>[] = [];
    if (wantRequest) {
      jobs.push(fetchAuditConversationBody(id, "request").then((bytes) => {
        if (!cancelled) streamedBodies = { ...streamedBodies, request: bytes };
      }));
    }
    if (wantResponse) {
      jobs.push(fetchAuditConversationBody(id, "response").then((bytes) => {
        if (!cancelled) streamedBodies = { ...streamedBodies, response: bytes };
      }));
    }
    void Promise.all(jobs)
      .catch((failure: unknown) => {
        if (!cancelled) bodiesError = failure instanceof Error ? failure.message : String(failure);
      })
      .finally(() => {
        if (!cancelled) bodiesLoading = false;
      });
    return () => {
      cancelled = true;
    };
  });

  $effect(() => {
    if (!open || !conversation) return;
    void tick().then(() => transcriptScroller?.scrollTo({ top: 0 }));
  });
</script>

<Dialog.Root
  {open}
  onOpenChange={(value) => {
    if (!value) onclose();
  }}
>
  <Dialog.Content class="flex max-h-[90vh] w-[90%] flex-col gap-0 p-0 sm:max-w-[90%]">
    {#if loading}
      <Dialog.Header class="border-b px-4 py-3">
        <Dialog.Title>{$translate("controlPlane.auditTitle")}</Dialog.Title>
        <Dialog.Description>{$translate("common.loading")}</Dialog.Description>
      </Dialog.Header>
    {:else if error}
      <Dialog.Header class="border-b px-4 py-3">
        <Dialog.Title>{$translate("controlPlane.auditTitle")}</Dialog.Title>
        <Dialog.Description>{$translate("controlPlane.error", { message: error })}</Dialog.Description>
      </Dialog.Header>
    {:else if conversation}
      <Dialog.Header class="border-b px-4 py-3">
        <Dialog.Title class="flex min-w-0 flex-wrap items-baseline gap-x-2 gap-y-1 text-lg font-bold">
          <span>{$translate("controlPlane.auditTitle")}</span>
          <span class="truncate font-mono text-base font-normal text-muted-foreground">{conversation.model}</span>
          <span class="font-mono text-xs font-normal text-muted-foreground">#{displayID(conversation)}</span>
        </Dialog.Title>
        <Dialog.Description class="font-mono text-xs">{conversation.reqPath}</Dialog.Description>
      </Dialog.Header>

      <div bind:this={transcriptScroller} class="min-h-0 flex-1 space-y-4 overflow-y-auto p-4">
        <div class="grid gap-3 rounded-lg border bg-card p-3 text-sm sm:grid-cols-2 lg:grid-cols-5">
          <div>
            <div class="text-muted-foreground text-xs">{$translate("controlPlane.auditStatus")}</div>
            <div class="mt-1 font-medium">{conversation.responseStatus}</div>
          </div>
          <div>
            <div class="text-muted-foreground text-xs">{$translate("activity.table.columns.time")}</div>
            <div class="mt-1 font-medium">{formatDate(conversation.timestamp)}</div>
          </div>
          <div>
            <div class="text-muted-foreground text-xs">{$translate("controlPlane.auditTokens")}</div>
            <div class="mt-1 font-medium">{totalTokens(conversation)}</div>
          </div>
          <div class="min-w-0">
            <div class="text-muted-foreground text-xs">{$translate("controlPlane.auditSession")}</div>
            <div class="mt-1 truncate font-mono text-xs">{conversation.sessionId || "—"}</div>
          </div>
          <div>
            <div class="text-muted-foreground text-xs">{$translate("controlPlane.auditCost")}</div>
            <div class="mt-1 font-medium">{conversation.estimatedCost.toFixed(6)}</div>
          </div>
        </div>

        <section class="min-w-0" aria-labelledby="audit-speed">
          <h3 id="audit-speed" class="mb-2 text-sm font-semibold">{$translate("controlPlane.speedSection")}</h3>
          <div class="rounded-lg border bg-card p-3">
            <PhaseShareBar
              firstTokenMs={conversation.firstTokenMs}
              decodeMs={conversation.decodeMs}
              durationMs={conversation.durationMs}
            />
            {#if speedPoints.length > 0}
              <div class="mt-3 border-t pt-2">
                <SpeedCurve points={speedPoints} />
              </div>
            {/if}
          </div>
        </section>

        <section class="min-w-0" aria-labelledby="audit-transcript">
          <h3 id="audit-transcript" class="mb-2 text-sm font-semibold">{$translate("controlPlane.auditTranscript")}</h3>
          {#if transcript.length}
            <AuditTranscript items={transcript} />
          {:else if bodiesLoading}
            <p class="rounded-md border border-dashed p-3 text-sm text-muted-foreground">{$translate("common.loading")}</p>
          {:else if bodiesError}
            <p class="rounded-md border border-dashed p-3 text-sm text-destructive">{$translate("controlPlane.error", { message: bodiesError })}</p>
          {:else}
            <p class="rounded-md border border-dashed p-3 text-sm text-muted-foreground">{$translate("controlPlane.auditTranscriptUnavailable")}</p>
          {/if}
          {#if responseView?.finishReason}
            <div class="text-muted-foreground mt-2 text-xs">{$translate("controlPlane.auditFinishReason")}: {responseView.finishReason}</div>
          {/if}
        </section>

        {#if conversation.requestHeaders || conversation.responseHeaders}
          <div class="grid gap-3 text-xs xl:grid-cols-2">
            {#if conversation.requestHeaders}
              <details>
                <summary class="cursor-pointer text-muted-foreground">{$translate("controlPlane.auditRequestHeaders")}</summary>
                <pre class="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2">{headersView(conversation.requestHeaders)}</pre>
              </details>
            {/if}
            {#if conversation.responseHeaders}
              <details>
                <summary class="cursor-pointer text-muted-foreground">{$translate("controlPlane.auditResponseHeaders")}</summary>
                <pre class="mt-1 max-h-40 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2">{headersView(conversation.responseHeaders)}</pre>
              </details>
            {/if}
          </div>
        {/if}

        {#if responseView?.events.length}
          <section class="border-t pt-3" aria-labelledby="audit-events-detail">
            <h3 id="audit-events-detail" class="mb-2 text-sm font-semibold">{$translate("controlPlane.auditEvents")}</h3>
            <ol class="grid gap-2">
              {#each summarizeEvents(responseView.events) as event (event.type)}
                <li class="grid gap-2 rounded-md border bg-card p-2 text-xs sm:grid-cols-[auto_1fr]">
                  <span class="font-mono">{event.type}</span>
                  <span class="text-muted-foreground">
                    {$translate("controlPlane.auditEventCount", { count: event.count })}
                    {#if event.characters > 0} · {$translate("controlPlane.auditEventCharacters", { count: event.characters })}{/if}
                    {#if event.finishReason} · {$translate("controlPlane.auditFinishReason")}: {event.finishReason}{/if}
                  </span>
                </li>
              {/each}
            </ol>
          </section>
        {/if}

        <details class="border-t pt-3 text-xs">
          <summary class="cursor-pointer text-muted-foreground">{$translate("controlPlane.auditRaw")}</summary>
          <div class="mt-2 grid gap-3 xl:grid-cols-2">
            <pre class="max-h-56 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2">{bodyText(conversation.requestBody) || $translate("capture.empty")}</pre>
            <pre class="max-h-56 overflow-auto whitespace-pre-wrap break-words rounded-md bg-muted/40 p-2">{bodyText(conversation.responseBody) || $translate("capture.empty")}</pre>
          </div>
        </details>
      </div>

      <Dialog.Footer class="mx-0 mb-0 shrink-0 border-t px-4 py-3">
        {#if conversation.keyId}
          <span class="text-muted-foreground mr-auto self-center font-mono text-xs">{$translate("activity.filters.key")}: {conversation.keyId}</span>
        {/if}
        <span class="text-muted-foreground self-center text-xs">{$translate("controlPlane.auditTokens")}: {totalTokens(conversation)} · {$translate("controlPlane.cacheHitRatio")}: {percent(conversation.cacheHitRatio)}</span>
        <Button variant="outline" onclick={onclose}>{$translate("common.close")}</Button>
      </Dialog.Footer>
    {/if}
  </Dialog.Content>
</Dialog.Root>
