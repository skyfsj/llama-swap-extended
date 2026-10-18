<script lang="ts">
  import { tick, untrack } from "svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Tabs from "$lib/components/ui/tabs/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import {
    Activity,
    Clock,
    Database,
    DollarSign,
    FileCode2,
    Layers,
    MessageSquare,
    Network,
    Zap,
    X,
    Cpu,
    CheckCircle2,
    AlertCircle,
    XCircle,
  } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  import {
    auditTranscript,
    collapseDataUrls,
    decodeAuditBytes,
    fetchAuditConversationBody,
    formatAuditBody,
    formatBytes,
    type AuditBodyView,
    type AuditBytes,
    type AuditEvent,
  } from "../../lib/audit";
  import { parseSpeedTimeline, formatMs } from "../../lib/speedTimeline";
  import type { AuditConversation } from "../../lib/types";
  import AuditTranscript from "./AuditTranscript.svelte";
  import PhaseShareBar from "./PhaseShareBar.svelte";
  import SpeedCurve from "./SpeedCurve.svelte";
  import AuditCopyButton from "./AuditCopyButton.svelte";

  interface Props {
    conversation: AuditConversation | null;
    open: boolean;
    loading?: boolean;
    error?: string;
    onclose: () => void;
  }

  let { conversation, open, loading = false, error = "", onclose }: Props = $props();

  // Active view tabs
  let activeTab = $state<string>("transcript");
  let payloadViewMode = $state<"split" | "request" | "response">("split");
  let payloadFormat = $state<"pretty" | "raw">("pretty");

  function bodyText(value: AuditBytes): string {
    return collapseDataUrls(decodeAuditBytes(value));
  }

  function bodyPretty(value: AuditBytes): string {
    const raw = decodeAuditBytes(value);
    if (!raw.trim()) return "";
    try {
      const parsed = JSON.parse(raw);
      return collapseDataUrls(JSON.stringify(parsed, null, 2));
    } catch {
      return collapseDataUrls(raw);
    }
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
    return item.activityId && item.activityId > 0 ? String(item.activityId) : item.id.slice(0, 8);
  }

  function percent(value?: number): string {
    return `${((value ?? 0) * 100).toFixed(1)}%`;
  }

  function statusVariant(status: number): { label: string; badgeClass: string; icon: typeof CheckCircle2 } {
    if (status >= 200 && status < 300) {
      return {
        label: `${status} OK`,
        badgeClass: "bg-emerald-500/15 text-emerald-700 dark:text-emerald-300 border-emerald-500/30",
        icon: CheckCircle2,
      };
    }
    if (status >= 400 && status < 500) {
      return {
        label: `${status} Error`,
        badgeClass: "bg-amber-500/15 text-amber-700 dark:text-amber-300 border-amber-500/30",
        icon: AlertCircle,
      };
    }
    return {
      label: `${status} Fail`,
      badgeClass: "bg-destructive/15 text-destructive border-destructive/30",
      icon: XCircle,
    };
  }

  // The detail response carries only body references, so the dialog opens on the
  // metadata and streams each transcript body afterwards.
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
    // The reset writes streamedBodies/bodiesError; reading them in the same
    // effect would retrigger it on every run. The reads below are taken from a
    // snapshot so the effect depends only on the conversation being opened.
    untrack(() => {
      streamedBodies = {};
      bodiesError = "";
    });
    if (!open || !item) {
      bodiesLoading = false;
      return;
    }
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
        if (cancelled) return;
        untrack(() => { streamedBodies = { ...streamedBodies, request: bytes }; });
      }));
    }
    if (wantResponse) {
      jobs.push(fetchAuditConversationBody(id, "response").then((bytes) => {
        if (cancelled) return;
        untrack(() => { streamedBodies = { ...streamedBodies, response: bytes }; });
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
    activeTab = "transcript";
    void tick().then(() => transcriptScroller?.scrollTo({ top: 0 }));
  });
</script>

<Dialog.Root
  {open}
  onOpenChange={(value) => {
    if (!value) onclose();
  }}
>
  <Dialog.Content
    showCloseButton={false}
    class="flex h-[88vh] max-h-[92vh] w-[96vw] sm:max-w-[96vw] md:max-w-[94vw] lg:max-w-[1400px] xl:max-w-[1560px] flex-col gap-0 p-0 overflow-hidden shadow-2xl border border-border bg-background"
  >
    {#if loading}
      <Dialog.Header class="border-b px-5 py-4">
        <Dialog.Title>{$translate("controlPlane.auditTitle")}</Dialog.Title>
        <Dialog.Description>{$translate("common.loading")}</Dialog.Description>
      </Dialog.Header>
      <div class="flex flex-1 items-center justify-center p-8 text-sm text-muted-foreground">
        {$translate("common.loading")}
      </div>
    {:else if error}
      <Dialog.Header class="border-b px-5 py-4">
        <Dialog.Title>{$translate("controlPlane.auditTitle")}</Dialog.Title>
        <Dialog.Description>{$translate("controlPlane.error", { message: error })}</Dialog.Description>
      </Dialog.Header>
      <div class="flex flex-1 items-center justify-center p-8 text-sm text-destructive">
        {$translate("controlPlane.error", { message: error })}
      </div>
    {:else if conversation}
      {@const statusInfo = statusVariant(conversation.responseStatus)}
      <!-- Header -->
      <Dialog.Header class="border-b bg-card/60 px-5 py-3 backdrop-blur-xs">
        <div class="flex items-center justify-between gap-4">
          <div class="min-w-0 flex-1 space-y-1">
            <!-- Line 1: Title, Status, Model, ID -->
            <div class="flex flex-wrap items-center gap-2">
              <span class="text-base font-bold tracking-tight text-foreground">
                {$translate("controlPlane.auditTitle")}
              </span>

              <!-- Status badge -->
              <span class="inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-xs font-semibold {statusInfo.badgeClass}">
                <statusInfo.icon class="size-3.5" aria-hidden="true" />
                <span>{statusInfo.label}</span>
              </span>

              <!-- Model badge -->
              <Badge variant="secondary" class="inline-flex items-center gap-1 font-mono text-xs font-medium">
                <Cpu class="size-3 text-primary" aria-hidden="true" />
                <span class="max-w-56 truncate">{conversation.model}</span>
              </Badge>

              <!-- ID badge with copy -->
              <div class="inline-flex items-center gap-1 rounded-md border bg-muted/40 px-1.5 py-0.5 text-xs text-muted-foreground font-mono">
                <span>#{displayID(conversation)}</span>
                <AuditCopyButton text={displayID(conversation)} title={$translate("controlPlane.auditCopyId")} />
              </div>
            </div>

            <!-- Line 2: Path, timestamp, Key, Session -->
            <div class="flex flex-wrap items-center gap-x-2.5 gap-y-1 text-xs text-muted-foreground">
              <span class="inline-flex items-center gap-1 font-mono text-foreground/80 font-medium">
                <span>{conversation.reqPath}</span>
                <AuditCopyButton text={conversation.reqPath} title={$translate("controlPlane.auditCopyPath")} />
              </span>
              <span>·</span>
              <span class="inline-flex items-center gap-1">
                <Clock class="size-3" aria-hidden="true" />
                <span>{formatDate(conversation.timestamp)}</span>
              </span>
              {#if conversation.keyId}
                <span>·</span>
                <span class="font-mono">Key: {conversation.keyId}</span>
              {/if}
              {#if conversation.sessionId}
                <span>·</span>
                <span class="inline-flex items-center gap-1 font-mono">
                  <span>Session: {conversation.sessionId.slice(0, 16)}</span>
                  <AuditCopyButton text={conversation.sessionId} />
                </span>
              {/if}
            </div>
          </div>

          <!-- Single Close Button -->
          <Button
            variant="ghost"
            size="icon"
            class="size-8 shrink-0 rounded-full text-muted-foreground hover:bg-muted hover:text-foreground"
            onclick={onclose}
            aria-label={$translate("common.close")}
          >
            <X class="size-4" aria-hidden="true" />
          </Button>
        </div>
      </Dialog.Header>

      <!-- Compact Metrics Ribbon (Clean, space-efficient, no awkward truncation) -->
      <div class="flex flex-wrap items-center gap-2 border-b bg-muted/20 px-5 py-2 text-xs">
        <!-- Duration & TTFT -->
        <div class="flex items-center gap-1.5 rounded-md border bg-background/90 px-2.5 py-1 shadow-2xs">
          <Clock class="size-3.5 text-muted-foreground" aria-hidden="true" />
          <span class="text-muted-foreground">{$translate("controlPlane.auditDuration")}:</span>
          <span class="font-mono font-semibold text-foreground">{formatMs(conversation.durationMs ?? 0)}</span>
          {#if conversation.firstTokenMs !== undefined && conversation.firstTokenMs >= 0}
            <span class="text-muted-foreground font-mono">({$translate("controlPlane.auditTtft")}: {formatMs(conversation.firstTokenMs)})</span>
          {/if}
        </div>

        <!-- Tokens -->
        <div class="flex items-center gap-1.5 rounded-md border bg-background/90 px-2.5 py-1 shadow-2xs">
          <Zap class="size-3.5 text-amber-500" aria-hidden="true" />
          <span class="text-muted-foreground">{$translate("controlPlane.auditTokens")}:</span>
          <span class="font-mono font-semibold text-foreground">{totalTokens(conversation)}</span>
          <span class="text-muted-foreground font-mono">({conversation.inputTokens}↑ · {conversation.outputTokens}↓{#if conversation.reasoningTokens} · {conversation.reasoningTokens}💡{/if})</span>
        </div>

        <!-- Cache Hit -->
        <div class="flex items-center gap-1.5 rounded-md border bg-background/90 px-2.5 py-1 shadow-2xs">
          <Database class="size-3.5 text-sky-500" aria-hidden="true" />
          <span class="text-muted-foreground">{$translate("controlPlane.cacheHitRatio")}:</span>
          <span class="font-mono font-semibold text-foreground">{percent(conversation.cacheHitRatio)}</span>
          {#if conversation.cachedTokens}
            <span class="text-muted-foreground font-mono">({conversation.cachedTokens})</span>
          {/if}
        </div>

        <!-- Estimated Cost -->
        <div class="flex items-center gap-1.5 rounded-md border bg-background/90 px-2.5 py-1 shadow-2xs">
          <DollarSign class="size-3.5 text-emerald-500" aria-hidden="true" />
          <span class="text-muted-foreground">{$translate("controlPlane.auditCost")}:</span>
          <span class="font-mono font-semibold text-foreground">${conversation.estimatedCost.toFixed(6)}</span>
        </div>

        <!-- Session status if multi-turn -->
        {#if conversation.session && conversation.session.requestCount > 1}
          <div class="flex items-center gap-1.5 rounded-md border bg-background/90 px-2.5 py-1 shadow-2xs">
            <Layers class="size-3.5 text-primary" aria-hidden="true" />
            <span class="text-muted-foreground">{$translate("controlPlane.auditSession")}:</span>
            <span class="font-mono font-semibold text-foreground">Req #{conversation.session.requestCount}</span>
          </div>
        {/if}
      </div>

      <!-- Main Tabs Structure -->
      <Tabs.Root bind:value={activeTab} class="flex min-h-0 flex-1 flex-col">
        <!-- Tabs Header -->
        <div class="border-b bg-card/40 px-5">
          <Tabs.List class="h-10 w-auto gap-2 bg-transparent p-0">
            <Tabs.Trigger
              value="transcript"
              class="relative h-10 rounded-none border-b-2 border-transparent px-3 text-xs font-medium text-muted-foreground transition-none data-[state=active]:border-primary data-[state=active]:text-foreground data-[state=active]:shadow-none"
            >
              <MessageSquare class="size-3.5 mr-1.5" />
              <span>{$translate("controlPlane.auditTabTranscript")}</span>
              {#if transcript.length > 0}
                <Badge variant="secondary" class="ml-1.5 px-1.5 py-0 text-xs font-normal">
                  {transcript.length}
                </Badge>
              {/if}
            </Tabs.Trigger>

            <Tabs.Trigger
              value="payload"
              class="relative h-10 rounded-none border-b-2 border-transparent px-3 text-xs font-medium text-muted-foreground transition-none data-[state=active]:border-primary data-[state=active]:text-foreground data-[state=active]:shadow-none"
            >
              <FileCode2 class="size-3.5 mr-1.5" />
              <span>{$translate("controlPlane.auditTabPayload")}</span>
            </Tabs.Trigger>

            <Tabs.Trigger
              value="performance"
              class="relative h-10 rounded-none border-b-2 border-transparent px-3 text-xs font-medium text-muted-foreground transition-none data-[state=active]:border-primary data-[state=active]:text-foreground data-[state=active]:shadow-none"
            >
              <Activity class="size-3.5 mr-1.5" />
              <span>{$translate("controlPlane.auditTabPerformance")}</span>
              {#if speedPoints.length > 0}
                <Badge variant="secondary" class="ml-1.5 px-1.5 py-0 text-xs font-normal">
                  TPS
                </Badge>
              {/if}
            </Tabs.Trigger>

            <Tabs.Trigger
              value="headers"
              class="relative h-10 rounded-none border-b-2 border-transparent px-3 text-xs font-medium text-muted-foreground transition-none data-[state=active]:border-primary data-[state=active]:text-foreground data-[state=active]:shadow-none"
            >
              <Network class="size-3.5 mr-1.5" />
              <span>{$translate("controlPlane.auditTabHeaders")}</span>
              {#if responseView?.events.length}
                <Badge variant="secondary" class="ml-1.5 px-1.5 py-0 text-xs font-normal">
                  {responseView.events.length}
                </Badge>
              {/if}
            </Tabs.Trigger>
          </Tabs.List>
        </div>

        <!-- TAB 1: TRANSCRIPT -->
        <Tabs.Content value="transcript" class="m-0 min-h-0 flex-1 overflow-y-auto p-4 sm:p-5">
          <div bind:this={transcriptScroller} class="w-full space-y-4">
            {#if bodiesLoading}
              <div class="rounded-lg border border-dashed p-8 text-center text-sm text-muted-foreground">
                <div class="inline-block animate-spin mr-2">⌛</div>
                <span>{$translate("common.loading")}</span>
              </div>
            {:else if bodiesError}
              <div class="rounded-lg border border-destructive/30 bg-destructive/5 p-4 text-sm text-destructive">
                {$translate("controlPlane.error", { message: bodiesError })}
              </div>
            {:else if transcript.length > 0}
              <AuditTranscript items={transcript} finishReason={responseView?.finishReason} />
            {:else}
              <div class="rounded-lg border border-dashed p-8 text-center text-sm text-muted-foreground">
                {$translate("controlPlane.auditTranscriptUnavailable")}
              </div>
            {/if}
          </div>
        </Tabs.Content>

        <!-- TAB 2: PAYLOAD -->
        <Tabs.Content value="payload" class="m-0 min-h-0 flex-1 flex flex-col p-4 sm:p-5 gap-3">
          <!-- Payload view controls -->
          <div class="flex flex-wrap items-center justify-between gap-2.5 rounded-lg border bg-muted/20 p-2 text-xs">
            <div class="flex items-center gap-1">
              <span class="text-muted-foreground font-medium mr-1.5">View:</span>
              <button
                type="button"
                class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors {payloadViewMode === 'split' ? 'bg-primary text-primary-foreground shadow-2xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
                onclick={() => (payloadViewMode = "split")}
              >
                {$translate("controlPlane.auditViewSplit")}
              </button>
              <button
                type="button"
                class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors {payloadViewMode === 'request' ? 'bg-primary text-primary-foreground shadow-2xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
                onclick={() => (payloadViewMode = "request")}
              >
                {$translate("controlPlane.auditViewRequest")}
              </button>
              <button
                type="button"
                class="rounded-md px-2.5 py-1 text-xs font-medium transition-colors {payloadViewMode === 'response' ? 'bg-primary text-primary-foreground shadow-2xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
                onclick={() => (payloadViewMode = "response")}
              >
                {$translate("controlPlane.auditViewResponse")}
              </button>
            </div>

            <div class="flex items-center gap-2">
              <div class="flex items-center gap-1">
                <button
                  type="button"
                  class="rounded-md px-2 py-0.5 text-xs font-medium transition-colors {payloadFormat === 'pretty' ? 'bg-background text-foreground shadow-2xs border' : 'text-muted-foreground hover:text-foreground'}"
                  onclick={() => (payloadFormat = "pretty")}
                >
                  {$translate("controlPlane.auditFormatPretty")}
                </button>
                <button
                  type="button"
                  class="rounded-md px-2 py-0.5 text-xs font-medium transition-colors {payloadFormat === 'raw' ? 'bg-background text-foreground shadow-2xs border' : 'text-muted-foreground hover:text-foreground'}"
                  onclick={() => (payloadFormat = "raw")}
                >
                  {$translate("controlPlane.auditFormatRaw")}
                </button>
              </div>
            </div>
          </div>

          <!-- Payload Display Areas -->
          <div class="grid min-h-0 flex-1 gap-4 {payloadViewMode === 'split' ? 'lg:grid-cols-2' : 'grid-cols-1'}">
            <!-- Request Payload -->
            {#if payloadViewMode === 'split' || payloadViewMode === 'request'}
              {@const reqStr = payloadFormat === 'pretty' ? bodyPretty(requestBody) : bodyText(requestBody)}
              <div class="flex min-h-0 flex-col rounded-lg border bg-card shadow-2xs overflow-hidden">
                <div class="flex items-center justify-between border-b bg-muted/40 px-3.5 py-2 text-xs">
                  <div class="flex items-center gap-2 font-medium text-foreground">
                    <span class="rounded bg-primary/10 px-1.5 py-0.5 font-mono text-xs font-semibold text-primary">POST</span>
                    <span>{$translate("controlPlane.auditRequest")}</span>
                    {#if conversation.requestBodyBytes}
                      <span class="text-muted-foreground font-normal">({formatBytes(conversation.requestBodyBytes)})</span>
                    {/if}
                  </div>
                  <AuditCopyButton text={reqStr} label={$translate("controlPlane.auditCopy")} />
                </div>
                <div class="min-h-0 flex-1 overflow-auto bg-muted/10 p-3 font-mono text-xs leading-5">
                  <pre class="whitespace-pre-wrap break-words">{reqStr || $translate("capture.empty")}</pre>
                </div>
              </div>
            {/if}

            <!-- Response Payload -->
            {#if payloadViewMode === 'split' || payloadViewMode === 'response'}
              {@const respStr = payloadFormat === 'pretty' ? bodyPretty(responseBody) : bodyText(responseBody)}
              <div class="flex min-h-0 flex-col rounded-lg border bg-card shadow-2xs overflow-hidden">
                <div class="flex items-center justify-between border-b bg-muted/40 px-3.5 py-2 text-xs">
                  <div class="flex items-center gap-2 font-medium text-foreground">
                    <span class="rounded px-1.5 py-0.5 font-mono text-xs font-semibold {statusInfo.badgeClass}">{conversation.responseStatus}</span>
                    <span>{$translate("controlPlane.auditResponse")}</span>
                    {#if conversation.responseBodyBytes}
                      <span class="text-muted-foreground font-normal">({formatBytes(conversation.responseBodyBytes)})</span>
                    {/if}
                  </div>
                  <AuditCopyButton text={respStr} label={$translate("controlPlane.auditCopy")} />
                </div>
                <div class="min-h-0 flex-1 overflow-auto bg-muted/10 p-3 font-mono text-xs leading-5">
                  <pre class="whitespace-pre-wrap break-words">{respStr || $translate("capture.empty")}</pre>
                </div>
              </div>
            {/if}
          </div>
        </Tabs.Content>

        <!-- TAB 3: PERFORMANCE -->
        <Tabs.Content value="performance" class="m-0 min-h-0 flex-1 overflow-y-auto p-4 sm:p-5">
          <div class="w-full space-y-6">
            <!-- Phase Timing Section -->
            <section class="rounded-xl border bg-card p-4 shadow-2xs space-y-3" aria-labelledby="timing-title">
              <div class="flex items-center justify-between border-b pb-2.5">
                <h3 id="timing-title" class="text-sm font-semibold flex items-center gap-2 text-foreground">
                  <Clock class="size-4 text-primary" aria-hidden="true" />
                  <span>{$translate("controlPlane.speedSection")}</span>
                </h3>
                <span class="text-xs text-muted-foreground font-mono">
                  {$translate("controlPlane.phaseTotal")}: {formatMs(conversation.durationMs ?? 0)}
                </span>
              </div>
              <PhaseShareBar
                firstTokenMs={conversation.firstTokenMs}
                decodeMs={conversation.decodeMs}
                durationMs={conversation.durationMs}
              />
            </section>

            <!-- TPS Curve Section -->
            <section class="rounded-xl border bg-card p-4 shadow-2xs space-y-3" aria-labelledby="speed-title">
              <div class="flex items-center justify-between border-b pb-2.5">
                <h3 id="speed-title" class="text-sm font-semibold flex items-center gap-2 text-foreground">
                  <Activity class="size-4 text-primary" aria-hidden="true" />
                  <span>Token Generation Speed (TPS)</span>
                </h3>
              </div>
              <SpeedCurve points={speedPoints} />
            </section>

            <!-- Session Cumulative Context (if available) -->
            {#if conversation.session && conversation.session.requestCount > 1}
              <section class="rounded-xl border bg-card p-4 shadow-2xs space-y-3" aria-labelledby="session-title">
                <div class="flex items-center justify-between border-b pb-2.5">
                  <h3 id="session-title" class="text-sm font-semibold flex items-center gap-2 text-foreground">
                    <Layers class="size-4 text-primary" aria-hidden="true" />
                    <span>{$translate("controlPlane.auditSessionAccumulated")}</span>
                  </h3>
                  <span class="font-mono text-xs text-muted-foreground">Session ID: {conversation.sessionId}</span>
                </div>
                <div class="grid gap-3 sm:grid-cols-2 lg:grid-cols-4 text-xs">
                  <div class="rounded-lg border bg-muted/20 p-3">
                    <div class="text-muted-foreground">{$translate("controlPlane.auditSessionRequests")}</div>
                    <div class="mt-1 font-mono text-base font-semibold text-foreground">{conversation.session.requestCount}</div>
                  </div>
                  <div class="rounded-lg border bg-muted/20 p-3">
                    <div class="text-muted-foreground">{$translate("controlPlane.auditSessionTokens")}</div>
                    <div class="mt-1 font-mono text-base font-semibold text-foreground">
                      {conversation.session.inputTokens} → {conversation.session.outputTokens}
                    </div>
                  </div>
                  <div class="rounded-lg border bg-muted/20 p-3">
                    <div class="text-muted-foreground">{$translate("controlPlane.auditSessionStored")}</div>
                    <div class="mt-1 font-mono text-base font-semibold text-foreground">
                      {formatBytes(conversation.session.storedBytes)}
                    </div>
                  </div>
                  <div class="rounded-lg border bg-muted/20 p-3">
                    <div class="text-muted-foreground">{$translate("controlPlane.auditSessionShared")}</div>
                    <div class="mt-1 font-mono text-base font-semibold text-foreground">
                      {formatBytes(conversation.session.sharedBlockBytes)}
                    </div>
                  </div>
                </div>
              </section>
            {/if}
          </div>
        </Tabs.Content>

        <!-- TAB 4: HEADERS & EVENTS -->
        <Tabs.Content value="headers" class="m-0 min-h-0 flex-1 overflow-y-auto p-4 sm:p-5">
          <div class="w-full space-y-6">
            <!-- Headers Section -->
            <div class="grid gap-4 md:grid-cols-2">
              <!-- Request Headers -->
              <div class="rounded-xl border bg-card shadow-2xs overflow-hidden flex flex-col">
                <div class="flex items-center justify-between border-b bg-muted/30 px-3.5 py-2.5 text-xs">
                  <span class="font-medium text-foreground">{$translate("controlPlane.auditRequestHeaders")}</span>
                  {#if conversation.requestHeaders}
                    <AuditCopyButton text={headersView(conversation.requestHeaders)} label={$translate("controlPlane.auditCopy")} />
                  {/if}
                </div>
                <div class="min-h-36 max-h-72 flex-1 overflow-auto bg-muted/10 p-3 font-mono text-xs">
                  {#if conversation.requestHeaders}
                    <pre class="whitespace-pre-wrap break-words">{headersView(conversation.requestHeaders)}</pre>
                  {:else}
                    <p class="text-muted-foreground italic">{$translate("controlPlane.auditNoHeaders")}</p>
                  {/if}
                </div>
              </div>

              <!-- Response Headers -->
              <div class="rounded-xl border bg-card shadow-2xs overflow-hidden flex flex-col">
                <div class="flex items-center justify-between border-b bg-muted/30 px-3.5 py-2.5 text-xs">
                  <span class="font-medium text-foreground">{$translate("controlPlane.auditResponseHeaders")}</span>
                  {#if conversation.responseHeaders}
                    <AuditCopyButton text={headersView(conversation.responseHeaders)} label={$translate("controlPlane.auditCopy")} />
                  {/if}
                </div>
                <div class="min-h-36 max-h-72 flex-1 overflow-auto bg-muted/10 p-3 font-mono text-xs">
                  {#if conversation.responseHeaders}
                    <pre class="whitespace-pre-wrap break-words">{headersView(conversation.responseHeaders)}</pre>
                  {:else}
                    <p class="text-muted-foreground italic">{$translate("controlPlane.auditNoHeaders")}</p>
                  {/if}
                </div>
              </div>
            </div>

            <!-- SSE Stream Events Section -->
            {#if responseView?.events && responseView.events.length > 0}
              <section class="rounded-xl border bg-card p-4 shadow-2xs space-y-3" aria-labelledby="events-summary-title">
                <div class="flex items-center justify-between border-b pb-2.5">
                  <h3 id="events-summary-title" class="text-sm font-semibold flex items-center gap-2 text-foreground">
                    <Activity class="size-4 text-primary" aria-hidden="true" />
                    <span>{$translate("controlPlane.auditEvents")}</span>
                  </h3>
                  <span class="text-xs text-muted-foreground font-mono">
                    {$translate("controlPlane.auditEventCount", { count: responseView.events.length })}
                  </span>
                </div>

                <!-- Event Groups Summary -->
                <div class="grid gap-2 sm:grid-cols-2">
                  {#each summarizeEvents(responseView.events) as event (event.type)}
                    <div class="rounded-lg border bg-muted/20 p-2.5 text-xs flex items-center justify-between">
                      <div class="flex items-center gap-2">
                        <Badge variant="outline" class="font-mono text-xs py-0">
                          {event.type}
                        </Badge>
                        <span class="text-muted-foreground font-medium">
                          {$translate("controlPlane.auditEventCount", { count: event.count })}
                        </span>
                      </div>
                      <div class="text-muted-foreground text-xs font-mono">
                        {#if event.characters > 0}
                          {$translate("controlPlane.auditEventCharacters", { count: event.characters })}
                        {/if}
                        {#if event.finishReason}
                          · {event.finishReason}
                        {/if}
                      </div>
                    </div>
                  {/each}
                </div>
              </section>
            {/if}
          </div>
        </Tabs.Content>
      </Tabs.Root>

      <!-- Footer -->
      <Dialog.Footer class="mx-0 mb-0 shrink-0 border-t bg-card/60 px-5 py-2.5 flex items-center justify-between">
        <div class="flex items-center gap-3 text-xs text-muted-foreground">
          <span>{$translate("controlPlane.auditTokens")}: <strong class="text-foreground font-mono">{totalTokens(conversation)}</strong></span>
          <span>·</span>
          <span>{$translate("controlPlane.cacheHitRatio")}: <strong class="text-foreground font-mono">{percent(conversation.cacheHitRatio)}</strong></span>
        </div>
        <Button variant="outline" size="sm" onclick={onclose}>
          {$translate("common.close")}
        </Button>
      </Dialog.Footer>
    {/if}
  </Dialog.Content>
</Dialog.Root>
