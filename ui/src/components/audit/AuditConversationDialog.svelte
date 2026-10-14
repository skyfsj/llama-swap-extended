<script lang="ts">
  import { tick } from "svelte";
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
        badgeClass: "bg-emerald-500/10 text-emerald-600 dark:text-emerald-400 border-emerald-500/20",
        icon: CheckCircle2,
      };
    }
    if (status >= 400 && status < 500) {
      return {
        label: `${status} Error`,
        badgeClass: "bg-amber-500/10 text-amber-600 dark:text-amber-400 border-amber-500/20",
        icon: AlertCircle,
      };
    }
    return {
      label: `${status} Fail`,
      badgeClass: "bg-destructive/10 text-destructive border-destructive/20",
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
    streamedBodies = {};
    bodiesError = "";
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
  <Dialog.Content class="flex h-[90vh] max-h-[92vh] w-[95vw] max-w-6xl flex-col gap-0 p-0 overflow-hidden shadow-2xl border-border">
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
      <!-- Enhanced Header -->
      <Dialog.Header class="border-b bg-card/60 px-5 py-3.5 backdrop-blur-xs">
        <div class="flex items-center justify-between gap-4">
          <div class="min-w-0 flex-1">
            <div class="flex flex-wrap items-center gap-2">
              <span class="text-base font-bold tracking-tight text-foreground">
                {$translate("controlPlane.auditTitle")}
              </span>

              <!-- Model badge -->
              <Badge variant="secondary" class="inline-flex items-center gap-1 font-mono text-xs font-medium">
                <Cpu class="size-3 text-primary" aria-hidden="true" />
                <span class="max-w-48 truncate">{conversation.model}</span>
              </Badge>

              <!-- Status badge -->
              <span class="inline-flex items-center gap-1 rounded-md border px-2 py-0.5 font-mono text-xs font-semibold {statusInfo.badgeClass}">
                <statusInfo.icon class="size-3" aria-hidden="true" />
                <span>{statusInfo.label}</span>
              </span>

              <!-- ID badge with copy -->
              <div class="inline-flex items-center gap-1 rounded-md border bg-muted/40 px-1.5 py-0.5 text-xs text-muted-foreground font-mono">
                <span>#{displayID(conversation)}</span>
                <AuditCopyButton text={displayID(conversation)} title={$translate("controlPlane.auditCopyId")} />
              </div>
            </div>

            <!-- Path and timestamp -->
            <div class="mt-1 flex flex-wrap items-center gap-x-3 gap-y-1 text-xs text-muted-foreground">
              <span class="inline-flex items-center gap-1 font-mono text-foreground/80">
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
                <span class="font-mono">{$translate("activity.filters.key")}: {conversation.keyId}</span>
              {/if}
              {#if conversation.sessionId}
                <span>·</span>
                <span class="inline-flex items-center gap-1 font-mono">
                  <span>{$translate("controlPlane.auditSession")}: {conversation.sessionId.slice(0, 16)}</span>
                  <AuditCopyButton text={conversation.sessionId} />
                </span>
              {/if}
            </div>
          </div>

          <Button
            variant="ghost"
            size="icon"
            class="size-8 shrink-0 rounded-full text-muted-foreground hover:text-foreground"
            onclick={onclose}
            aria-label={$translate("common.close")}
          >
            <X class="size-4" aria-hidden="true" />
          </Button>
        </div>
      </Dialog.Header>

      <!-- KPI Overview Bar -->
      <div class="grid grid-cols-2 gap-2 border-b bg-muted/20 px-5 py-2.5 sm:grid-cols-4 lg:grid-cols-5 text-xs">
        <!-- Duration & TTFT -->
        <div class="rounded-lg border bg-background/80 p-2.5 shadow-2xs">
          <div class="text-muted-foreground flex items-center justify-between">
            <span>{$translate("controlPlane.auditDuration")}</span>
            <Clock class="size-3.5 text-muted-foreground" />
          </div>
          <div class="mt-1 font-mono text-sm font-semibold text-foreground">
            {formatMs(conversation.durationMs ?? 0)}
          </div>
          <div class="mt-0.5 text-muted-foreground text-xs">
            {$translate("controlPlane.auditTtft")}: {conversation.firstTokenMs !== undefined && conversation.firstTokenMs >= 0 ? formatMs(conversation.firstTokenMs) : "—"}
          </div>
        </div>

        <!-- Tokens -->
        <div class="rounded-lg border bg-background/80 p-2.5 shadow-2xs">
          <div class="text-muted-foreground flex items-center justify-between">
            <span>{$translate("controlPlane.auditTokens")}</span>
            <Zap class="size-3.5 text-amber-500" />
          </div>
          <div class="mt-1 font-mono text-sm font-semibold text-foreground">
            {totalTokens(conversation)}
          </div>
          <div class="mt-0.5 text-muted-foreground text-xs truncate">
            {conversation.inputTokens} ↑ · {conversation.outputTokens} ↓
            {#if conversation.reasoningTokens}
              · {conversation.reasoningTokens} 💡
            {/if}
          </div>
        </div>

        <!-- Cache Hit -->
        <div class="rounded-lg border bg-background/80 p-2.5 shadow-2xs">
          <div class="text-muted-foreground flex items-center justify-between">
            <span>{$translate("controlPlane.cacheHitRatio")}</span>
            <Database class="size-3.5 text-sky-500" />
          </div>
          <div class="mt-1 font-mono text-sm font-semibold text-foreground">
            {percent(conversation.cacheHitRatio)}
          </div>
          <div class="mt-0.5 text-muted-foreground text-xs truncate">
            {conversation.cachedTokens ?? 0} cached
          </div>
        </div>

        <!-- Estimated Cost -->
        <div class="rounded-lg border bg-background/80 p-2.5 shadow-2xs">
          <div class="text-muted-foreground flex items-center justify-between">
            <span>{$translate("controlPlane.auditCost")}</span>
            <DollarSign class="size-3.5 text-emerald-500" />
          </div>
          <div class="mt-1 font-mono text-sm font-semibold text-foreground">
            ${conversation.estimatedCost.toFixed(6)}
          </div>
          <div class="mt-0.5 text-muted-foreground text-xs">
            {conversation.complete ? "Complete" : "Inflight"}
          </div>
        </div>

        <!-- Session Status (if applicable) -->
        <div class="col-span-2 sm:col-span-4 lg:col-span-1 rounded-lg border bg-background/80 p-2.5 shadow-2xs">
          <div class="text-muted-foreground flex items-center justify-between">
            <span>{$translate("controlPlane.auditSession")}</span>
            <Layers class="size-3.5 text-primary" />
          </div>
          {#if conversation.session && conversation.session.requestCount > 1}
            <div class="mt-1 font-mono text-sm font-semibold text-foreground">
              Req #{conversation.session.requestCount}
            </div>
            <div class="mt-0.5 text-muted-foreground text-xs truncate">
              {conversation.session.inputTokens} → {conversation.session.outputTokens} tok
            </div>
          {:else}
            <div class="mt-1 font-mono text-sm font-medium text-muted-foreground truncate">
              {conversation.sessionId ? conversation.sessionId.slice(0, 10) : "1 turn"}
            </div>
            <div class="mt-0.5 text-muted-foreground text-xs">
              Single request
            </div>
          {/if}
        </div>
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
          <div bind:this={transcriptScroller} class="max-w-4xl mx-auto space-y-4">
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
              {@const reqStr = payloadFormat === 'pretty' ? bodyPretty(conversation.requestBody) : bodyText(conversation.requestBody)}
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
              {@const respStr = payloadFormat === 'pretty' ? bodyPretty(conversation.responseBody) : bodyText(conversation.responseBody)}
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
          <div class="max-w-4xl mx-auto space-y-6">
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
          <div class="max-w-4xl mx-auto space-y-6">
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
      <Dialog.Footer class="mx-0 mb-0 shrink-0 border-t bg-card/60 px-5 py-3 flex items-center justify-between">
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
