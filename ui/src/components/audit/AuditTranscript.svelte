<script lang="ts">
  import {
    Bot,
    Braces,
    Brain,
    Download,
    FileAudio,
    FileText,
    Image as ImageIcon,
    UserRound,
    Wrench,
    ChevronDown,
    ChevronUp,
    Code,
    Filter,
  } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  import { formatBytes, type AuditTranscriptItem } from "../../lib/audit";
  import { renderMarkdown } from "../../lib/markdown";
  import AuditCopyButton from "./AuditCopyButton.svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";

  type MessageItem = Extract<AuditTranscriptItem, { kind: "message" }>;
  type ReasoningItem = Extract<AuditTranscriptItem, { kind: "reasoning" }>;
  type ToolCallItem = Extract<AuditTranscriptItem, { kind: "tool_call" }>;
  type ToolResultItem = Extract<AuditTranscriptItem, { kind: "tool_result" }>;
  type MediaItem = Extract<AuditTranscriptItem, { kind: "image" } | { kind: "file" } | { kind: "audio" }>;
  type TranscriptBlock =
    | { kind: "message"; message: MessageItem }
    | { kind: "reasoning"; item: ReasoningItem }
    | { kind: "tool"; call: ToolCallItem | null; results: ToolResultItem[] }
    | { kind: "media"; item: MediaItem };

  interface Props {
    items: AuditTranscriptItem[];
    finishReason?: string;
  }

  let { items, finishReason = "" }: Props = $props();

  // Controls & state
  let renderMd = $state(true);
  let roleFilter = $state<"all" | "user" | "assistant" | "system" | "tool" | "reasoning">("all");
  let expandedBlocks = $state<Record<string, boolean>>({});
  let globalExpanded = $state<boolean | null>(null);

  function roleLabel(role: "system" | "user" | "assistant"): string {
    if (role === "system") return $translate("controlPlane.auditRoleSystem");
    if (role === "user") return $translate("controlPlane.auditRoleUser");
    return $translate("controlPlane.auditRoleAssistant");
  }

  function isLongText(text: string): boolean {
    return text.length > 800 || text.split("\n").length > 14;
  }

  function isBlockExpanded(key: string, defaultOpen = false): boolean {
    if (globalExpanded !== null) return globalExpanded;
    return expandedBlocks[key] ?? defaultOpen;
  }

  function toggleBlock(key: string, defaultOpen = false): void {
    const current = isBlockExpanded(key, defaultOpen);
    expandedBlocks[key] = !current;
    globalExpanded = null;
  }

  function expandAll(): void {
    globalExpanded = true;
  }

  function collapseAll(): void {
    globalExpanded = false;
  }

  function transcriptBlocks(transcript: AuditTranscriptItem[]): TranscriptBlock[] {
    const blocks: TranscriptBlock[] = [];
    const callsByID = new Map<string, Extract<TranscriptBlock, { kind: "tool" }>>();
    let latestTool: Extract<TranscriptBlock, { kind: "tool" }> | undefined;

    for (const item of transcript) {
      if (item.kind === "image" || item.kind === "file" || item.kind === "audio") {
        blocks.push({ kind: "media", item });
        continue;
      }
      if (item.kind === "reasoning") {
        blocks.push({ kind: "reasoning", item });
        continue;
      }
      if (item.kind === "message") {
        blocks.push({ kind: "message", message: item });
        latestTool = undefined;
        continue;
      }
      if (item.kind === "tool_call") {
        latestTool = { kind: "tool", call: item, results: [] };
        blocks.push(latestTool);
        if (item.id) callsByID.set(item.id, latestTool);
        continue;
      }

      const target = (item.id ? callsByID.get(item.id) : undefined) ?? latestTool;
      if (target) {
        target.results.push(item);
        continue;
      }
      latestTool = { kind: "tool", call: null, results: [item] };
      blocks.push(latestTool);
    }
    return blocks;
  }

  let allBlocks = $derived(transcriptBlocks(items));

  let filteredBlocks = $derived.by(() => {
    if (roleFilter === "all") return allBlocks;
    return allBlocks.filter((b) => {
      if (roleFilter === "user") return b.kind === "message" && b.message.role === "user";
      if (roleFilter === "assistant") return b.kind === "message" && b.message.role === "assistant";
      if (roleFilter === "system") return b.kind === "message" && b.message.role === "system";
      if (roleFilter === "tool") return b.kind === "tool";
      if (roleFilter === "reasoning") return b.kind === "reasoning";
      return true;
    });
  });

  // Safe markdown render helper
  function renderSafeMarkdown(content: string): string {
    if (!content) return "";
    try {
      return renderMarkdown(content);
    } catch {
      return content;
    }
  }
</script>

<div class="space-y-4" aria-label={$translate("controlPlane.auditTranscript")}>
  <!-- Transcript Toolbar -->
  <div class="flex flex-wrap items-center justify-between gap-2.5 rounded-lg border bg-muted/30 px-3 py-2 text-xs">
    <!-- Filter pills -->
    <div class="flex flex-wrap items-center gap-1">
      <span class="mr-1 inline-flex items-center gap-1 text-muted-foreground font-medium">
        <Filter class="size-3.5" aria-hidden="true" />
        <span>{$translate("controlPlane.auditFilterAll")}:</span>
      </span>

      <button
        type="button"
        class="rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors {roleFilter === 'all' ? 'bg-primary text-primary-foreground shadow-xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
        onclick={() => (roleFilter = "all")}
      >
        {$translate("controlPlane.auditFilterAll")} ({allBlocks.length})
      </button>

      <button
        type="button"
        class="rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors {roleFilter === 'user' ? 'bg-primary text-primary-foreground shadow-xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
        onclick={() => (roleFilter = "user")}
      >
        {$translate("controlPlane.auditRoleUser")}
      </button>

      <button
        type="button"
        class="rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors {roleFilter === 'assistant' ? 'bg-primary text-primary-foreground shadow-xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
        onclick={() => (roleFilter = "assistant")}
      >
        {$translate("controlPlane.auditRoleAssistant")}
      </button>

      <button
        type="button"
        class="rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors {roleFilter === 'tool' ? 'bg-primary text-primary-foreground shadow-xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
        onclick={() => (roleFilter = "tool")}
      >
        {$translate("controlPlane.auditToolCall")}
      </button>

      <button
        type="button"
        class="rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors {roleFilter === 'reasoning' ? 'bg-primary text-primary-foreground shadow-xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
        onclick={() => (roleFilter = "reasoning")}
      >
        {$translate("controlPlane.auditReasoning")}
      </button>

      <button
        type="button"
        class="rounded-full px-2.5 py-0.5 text-xs font-medium transition-colors {roleFilter === 'system' ? 'bg-primary text-primary-foreground shadow-xs' : 'bg-background hover:bg-muted text-muted-foreground'}"
        onclick={() => (roleFilter = "system")}
      >
        {$translate("controlPlane.auditRoleSystem")}
      </button>
    </div>

    <!-- View & Expand toggles -->
    <div class="flex items-center gap-2">
      <Button
        variant="ghost"
        size="xs"
        class="h-6 text-xs text-muted-foreground hover:text-foreground"
        onclick={() => (renderMd = !renderMd)}
      >
        <Code class="size-3.5 mr-1" aria-hidden="true" />
        <span>{renderMd ? $translate("controlPlane.auditFormatRaw") : $translate("controlPlane.auditRenderMarkdown")}</span>
      </Button>

      <div class="h-3.5 w-px bg-border" aria-hidden="true"></div>

      <Button
        variant="ghost"
        size="xs"
        class="h-6 text-xs text-muted-foreground hover:text-foreground"
        onclick={expandAll}
      >
        {$translate("controlPlane.auditExpandAll")}
      </Button>
      <Button
        variant="ghost"
        size="xs"
        class="h-6 text-xs text-muted-foreground hover:text-foreground"
        onclick={collapseAll}
      >
        {$translate("controlPlane.auditCollapseAll")}
      </Button>
    </div>
  </div>

  <!-- Messages List -->
  {#if filteredBlocks.length === 0}
    <p class="rounded-lg border border-dashed p-6 text-center text-sm text-muted-foreground">
      {$translate("controlPlane.auditTranscriptUnavailable")}
    </p>
  {:else}
    <div class="space-y-4">
      {#each filteredBlocks as block, index (`${block.kind}-${index}`)}
        {#if block.kind === "message"}
          <!-- SYSTEM MESSAGE -->
          {#if block.message.role === "system"}
            {@const key = `system-${index}`}
            {@const expanded = isBlockExpanded(key, false)}
            <div class="rounded-lg border border-border/80 bg-muted/20 transition-colors">
              <div class="flex items-center justify-between gap-2 px-3.5 py-2 text-xs">
                <button
                  type="button"
                  class="flex min-w-0 flex-1 items-center gap-2 text-left font-medium text-foreground hover:text-primary transition-colors focus-visible:outline-none"
                  onclick={() => toggleBlock(key, false)}
                >
                  <Braces class="size-4 shrink-0 text-muted-foreground" aria-hidden="true" />
                  <Badge variant="outline" class="font-normal text-xs py-0">
                    {roleLabel(block.message.role)}
                  </Badge>
                  <span class="truncate text-muted-foreground">
                    {block.message.text.length} {$translate("playground.chat.characters")}
                  </span>
                  {#if expanded}
                    <ChevronUp class="size-3.5 text-muted-foreground ml-1" />
                  {:else}
                    <ChevronDown class="size-3.5 text-muted-foreground ml-1" />
                  {/if}
                </button>
                <AuditCopyButton text={block.message.text} />
              </div>

              {#if expanded}
                <div class="border-t border-border/60 bg-background/50 px-3.5 py-3 text-xs">
                  {#if renderMd}
                    <div class="prose prose-sm dark:prose-invert max-w-none break-words">
                      {@html renderSafeMarkdown(block.message.text)}
                    </div>
                  {:else}
                    <pre class="max-h-96 overflow-auto whitespace-pre-wrap break-words font-mono leading-5">{block.message.text}</pre>
                  {/if}
                </div>
              {/if}
            </div>

          <!-- USER MESSAGE -->
          {:else if block.message.role === "user"}
            {@const key = `user-${index}`}
            {@const hasLong = isLongText(block.message.text)}
            {@const expanded = isBlockExpanded(key, !hasLong)}
            <article class="flex justify-end">
              <div class="w-full max-w-[94%] sm:max-w-[85%] rounded-2xl border border-primary/20 bg-primary/5 p-3.5 shadow-xs transition-shadow hover:shadow-sm dark:bg-primary/10">
                <!-- Header -->
                <div class="mb-2 flex items-center justify-between gap-2 border-b border-primary/10 pb-1.5 text-xs">
                  <div class="flex items-center gap-1.5 font-medium text-primary">
                    <UserRound class="size-3.5" aria-hidden="true" />
                    <span>{roleLabel(block.message.role)}</span>
                    <span class="text-xs font-normal text-muted-foreground">· {block.message.text.length} {$translate("playground.chat.characters")}</span>
                  </div>
                  <AuditCopyButton text={block.message.text} />
                </div>

                <!-- Body -->
                {#if hasLong && !expanded}
                  <div class="relative">
                    <div class="max-h-32 overflow-hidden text-sm leading-6">
                      {#if renderMd}
                        <div class="prose prose-sm dark:prose-invert max-w-none break-words">
                          {@html renderSafeMarkdown(block.message.text.slice(0, 400))}
                        </div>
                      {:else}
                        <div class="whitespace-pre-wrap break-words">{block.message.text.slice(0, 400)}...</div>
                      {/if}
                    </div>
                    <div class="absolute inset-x-0 bottom-0 flex h-14 items-end justify-center bg-gradient-to-t from-primary/10 via-primary/5 to-transparent pb-1">
                      <Button
                        variant="secondary"
                        size="xs"
                        class="h-6 text-xs shadow-xs"
                        onclick={() => toggleBlock(key, false)}
                      >
                        <ChevronDown class="size-3.5 mr-1" />
                        {$translate("controlPlane.auditShowFull")}
                      </Button>
                    </div>
                  </div>
                {:else}
                  <div class="text-sm leading-6 text-foreground">
                    {#if renderMd}
                      <div class="prose prose-sm dark:prose-invert max-w-none break-words">
                        {@html renderSafeMarkdown(block.message.text)}
                      </div>
                    {:else}
                      <div class="whitespace-pre-wrap break-words">{block.message.text}</div>
                    {/if}
                    {#if hasLong}
                      <div class="mt-2 pt-1 text-right">
                        <Button
                          variant="ghost"
                          size="xs"
                          class="h-5 text-xs text-muted-foreground hover:text-foreground"
                          onclick={() => toggleBlock(key, true)}
                        >
                          <ChevronUp class="size-3.5 mr-1" />
                          {$translate("controlPlane.auditCollapseAll")}
                        </Button>
                      </div>
                    {/if}
                  </div>
                {/if}
              </div>
            </article>

          <!-- ASSISTANT MESSAGE -->
          {:else}
            {@const key = `assistant-${index}`}
            {@const hasLong = isLongText(block.message.text)}
            {@const expanded = isBlockExpanded(key, !hasLong)}
            <article class="flex justify-start">
              <div class="w-full max-w-[94%] sm:max-w-[85%] rounded-2xl border border-border bg-card p-4 shadow-xs transition-shadow hover:shadow-sm">
                <!-- Header -->
                <div class="mb-2.5 flex items-center justify-between gap-2 border-b border-border/60 pb-2 text-xs">
                  <div class="flex items-center gap-1.5 font-medium text-foreground">
                    <div class="flex size-5 items-center justify-center rounded-md bg-primary/10 text-primary">
                      <Bot class="size-3.5" aria-hidden="true" />
                    </div>
                    <span>{roleLabel(block.message.role)}</span>
                    <span class="text-xs font-normal text-muted-foreground">· {block.message.text.length} {$translate("playground.chat.characters")}</span>
                  </div>
                  <div class="flex items-center gap-1.5">
                    {#if finishReason}
                      <Badge variant="outline" class="font-normal text-xs py-0 text-muted-foreground">
                        {finishReason}
                      </Badge>
                    {/if}
                    <AuditCopyButton text={block.message.text} />
                  </div>
                </div>

                <!-- Body -->
                {#if hasLong && !expanded}
                  <div class="relative">
                    <div class="max-h-36 overflow-hidden text-sm leading-6">
                      {#if renderMd}
                        <div class="prose prose-sm dark:prose-invert max-w-none break-words">
                          {@html renderSafeMarkdown(block.message.text.slice(0, 500))}
                        </div>
                      {:else}
                        <div class="whitespace-pre-wrap break-words">{block.message.text.slice(0, 500)}...</div>
                      {/if}
                    </div>
                    <div class="absolute inset-x-0 bottom-0 flex h-16 items-end justify-center bg-gradient-to-t from-card via-card/80 to-transparent pb-1">
                      <Button
                        variant="secondary"
                        size="xs"
                        class="h-6 text-xs shadow-xs"
                        onclick={() => toggleBlock(key, false)}
                      >
                        <ChevronDown class="size-3.5 mr-1" />
                        {$translate("controlPlane.auditShowFull")}
                      </Button>
                    </div>
                  </div>
                {:else}
                  <div class="text-sm leading-6 text-foreground">
                    {#if renderMd}
                      <div class="prose prose-sm dark:prose-invert max-w-none break-words">
                        {@html renderSafeMarkdown(block.message.text)}
                      </div>
                    {:else}
                      <div class="whitespace-pre-wrap break-words">{block.message.text}</div>
                    {/if}
                    {#if hasLong}
                      <div class="mt-2 pt-1 text-right">
                        <Button
                          variant="ghost"
                          size="xs"
                          class="h-5 text-xs text-muted-foreground hover:text-foreground"
                          onclick={() => toggleBlock(key, true)}
                        >
                          <ChevronUp class="size-3.5 mr-1" />
                          {$translate("controlPlane.auditCollapseAll")}
                        </Button>
                      </div>
                    {/if}
                  </div>
                {/if}
              </div>
            </article>
          {/if}

        <!-- REASONING / THINKING -->
        {:else if block.kind === "reasoning"}
          {@const key = `reasoning-${index}`}
          {@const expanded = isBlockExpanded(key, false)}
          <div class="mx-auto w-full max-w-[94%] sm:max-w-[85%] rounded-xl border border-amber-500/20 bg-amber-500/5 text-xs text-foreground transition-all">
            <div class="flex items-center justify-between gap-2 px-3 py-2">
              <button
                type="button"
                class="flex min-w-0 flex-1 items-center gap-2 text-left font-medium text-amber-800 dark:text-amber-200 hover:opacity-80 transition-opacity focus-visible:outline-none"
                onclick={() => toggleBlock(key, false)}
              >
                <Brain class="size-4 shrink-0 text-amber-600 dark:text-amber-400" aria-hidden="true" />
                <span>{$translate("controlPlane.auditReasoning")}</span>
                <span class="text-xs text-amber-700/80 dark:text-amber-300/80 font-normal">
                  ({block.item.text.length} {$translate("playground.chat.characters")})
                </span>
                {#if expanded}
                  <ChevronUp class="size-3.5 text-amber-600 dark:text-amber-400 ml-1" />
                {:else}
                  <ChevronDown class="size-3.5 text-amber-600 dark:text-amber-400 ml-1" />
                {/if}
              </button>
              <AuditCopyButton text={block.item.text} />
            </div>

            {#if expanded}
              <div class="border-t border-amber-500/20 bg-background/50 px-3.5 py-3 text-xs leading-5 text-muted-foreground">
                {#if renderMd}
                  <div class="prose prose-sm dark:prose-invert max-w-none break-words text-xs">
                    {@html renderSafeMarkdown(block.item.text)}
                  </div>
                {:else}
                  <pre class="max-h-80 overflow-auto whitespace-pre-wrap break-words font-mono">{block.item.text}</pre>
                {/if}
              </div>
            {/if}
          </div>

        <!-- MEDIA -->
        {:else if block.kind === "media"}
          <figure class="mx-auto w-full max-w-[94%] sm:max-w-[85%] overflow-hidden rounded-xl border bg-muted/20 text-sm shadow-xs">
            {#if block.item.kind === "image"}
              <a
                href={block.item.dataUrl}
                target="_blank"
                rel="noreferrer"
                class="block bg-[repeating-conic-gradient(#8882_0%_25%,transparent_0%_50%)] bg-[length:16px_16px] p-2 hover:opacity-95 transition-opacity"
              >
                <img
                  src={block.item.dataUrl}
                  alt={block.item.label}
                  class="mx-auto max-h-72 w-auto max-w-full rounded-md object-contain shadow-xs"
                  loading="lazy"
                />
              </a>
            {:else if block.item.kind === "audio"}
              <div class="p-3">
                <audio controls preload="metadata" src={block.item.dataUrl} class="w-full"></audio>
              </div>
            {:else}
              <div class="flex items-center gap-2.5 px-4 py-3">
                <FileText class="size-5 text-primary" aria-hidden="true" />
                <span class="min-w-0 truncate font-medium">{block.item.label}</span>
              </div>
            {/if}

            <figcaption class="flex items-center gap-2 border-t bg-muted/40 px-3.5 py-2 text-xs text-muted-foreground">
              {#if block.item.kind === "image"}<ImageIcon class="size-3.5" aria-hidden="true" />
              {:else if block.item.kind === "audio"}<FileAudio class="size-3.5" aria-hidden="true" />
              {:else}<FileText class="size-3.5" aria-hidden="true" />{/if}
              <span class="truncate">
                {block.item.kind === "image" ? $translate("controlPlane.auditMediaImage") : block.item.kind === "audio" ? $translate("controlPlane.auditMediaAudio") : $translate("controlPlane.auditMediaFile")} · {block.item.label}{#if block.item.sizeBytes > 0} · {formatBytes(block.item.sizeBytes)}{/if}
              </span>
              <a
                class="ml-auto inline-flex shrink-0 items-center gap-1 rounded-md px-2 py-0.5 text-xs font-medium text-foreground hover:bg-muted"
                href={block.item.dataUrl}
                download={block.item.label}
              >
                <Download class="size-3.5" aria-hidden="true" />
                <span>{$translate("controlPlane.auditMediaDownload")}</span>
              </a>
            </figcaption>
          </figure>

        <!-- TOOL CALL & RESULTS -->
        {:else}
          {@const key = `tool-${index}`}
          {@const expanded = isBlockExpanded(key, true)}
          <article class="mr-auto w-full max-w-[94%] sm:max-w-[85%] overflow-hidden rounded-xl border bg-muted/20 text-sm shadow-xs">
            {#if block.call}
              <div class="flex items-center justify-between gap-2 border-b bg-muted/40 px-3.5 py-2 text-xs">
                <div class="flex min-w-0 items-center gap-2">
                  <div class="flex size-5 items-center justify-center rounded-md bg-amber-500/10 text-amber-600 dark:text-amber-400">
                    <Wrench class="size-3.5" aria-hidden="true" />
                  </div>
                  <span class="font-medium text-foreground">{$translate("controlPlane.auditToolCall")}</span>
                  <code class="rounded bg-background px-2 py-0.5 font-mono text-xs font-semibold text-primary border border-border/60">
                    {block.call.name}
                  </code>
                  {#if block.call.id}
                    <span class="truncate font-mono text-xs text-muted-foreground">#{block.call.id}</span>
                  {/if}
                </div>
                <div class="flex items-center gap-1">
                  <AuditCopyButton text={JSON.stringify(block.call.fields, null, 2)} />
                  <button
                    type="button"
                    class="rounded p-1 text-muted-foreground hover:text-foreground"
                    onclick={() => toggleBlock(key, true)}
                    aria-label="Toggle tool details"
                  >
                    {#if expanded}
                      <ChevronUp class="size-3.5" />
                    {:else}
                      <ChevronDown class="size-3.5" />
                    {/if}
                  </button>
                </div>
              </div>

              {#if expanded && block.call.fields.length}
                <div class="border-b bg-background/50 p-3 text-xs">
                  <dl class="grid gap-x-3 gap-y-2 sm:grid-cols-[minmax(7rem,0.3fr)_1fr]">
                    {#each block.call.fields as field (`${field.name}-${field.value}`)}
                      <dt class="font-mono text-muted-foreground font-medium">{field.name}:</dt>
                      <dd class="min-w-0 font-mono text-xs whitespace-pre-wrap break-words bg-muted/30 rounded p-1.5 border border-border/40">
                        {field.value}
                      </dd>
                    {/each}
                  </dl>
                </div>
              {/if}
            {/if}

            {#if expanded}
              {#each block.results as result, resultIndex (`${result.id}-${resultIndex}`)}
                {@const resKey = `result-${index}-${resultIndex}`}
                {@const isResultLong = isLongText(result.text)}
                {@const resExpanded = isBlockExpanded(resKey, !isResultLong)}
                <section class={block.call ? "px-3.5 py-2.5 bg-background/70" : "px-3.5 py-2.5"}>
                  <div class="mb-1.5 flex items-center justify-between gap-2 text-xs">
                    <div class="flex items-center gap-1.5">
                      <div class="size-1.5 rounded-full bg-emerald-500"></div>
                      <span class="font-medium text-foreground">{$translate("controlPlane.auditToolResult")}</span>
                      {#if result.name || block.call?.name}
                        <code class="rounded bg-muted px-1.5 py-0.5 font-mono text-xs text-muted-foreground">
                          {result.name || block.call?.name}
                        </code>
                      {/if}
                      {#if !block.call && result.id}
                        <span class="truncate font-mono text-xs text-muted-foreground">#{result.id}</span>
                      {/if}
                    </div>
                    <AuditCopyButton text={result.text} />
                  </div>

                  {#if isResultLong && !resExpanded}
                    <div class="relative">
                      <pre class="max-h-24 overflow-hidden rounded bg-muted/40 p-2 font-mono text-xs leading-5 whitespace-pre-wrap break-words">{result.text.slice(0, 300)}...</pre>
                      <div class="absolute inset-x-0 bottom-0 flex h-12 items-end justify-center bg-gradient-to-t from-background via-background/80 to-transparent pb-1">
                        <Button
                          variant="secondary"
                          size="xs"
                          class="h-5 text-xs"
                          onclick={() => toggleBlock(resKey, false)}
                        >
                          <ChevronDown class="size-3 mr-1" />
                          {$translate("controlPlane.auditShowFull")}
                        </Button>
                      </div>
                    </div>
                  {:else}
                    <pre class="max-h-80 overflow-auto rounded bg-muted/40 p-2.5 font-mono text-xs leading-5 whitespace-pre-wrap break-words text-foreground border border-border/50">{result.text}</pre>
                    {#if isResultLong}
                      <div class="mt-1 text-right">
                        <Button
                          variant="ghost"
                          size="xs"
                          class="h-5 text-xs text-muted-foreground hover:text-foreground"
                          onclick={() => toggleBlock(resKey, true)}
                        >
                          <ChevronUp class="size-3 mr-1" />
                          {$translate("controlPlane.auditCollapseAll")}
                        </Button>
                      </div>
                    {/if}
                  {/if}
                </section>
              {/each}
            {/if}
          </article>
        {/if}
      {/each}
    </div>
  {/if}
</div>
