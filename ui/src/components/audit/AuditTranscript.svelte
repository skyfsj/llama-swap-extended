<script lang="ts">
  import { Bot, Braces, Download, FileText, Image as ImageIcon, UserRound, Wrench } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  import { formatBytes, type AuditTranscriptItem } from "../../lib/audit";

  type MessageItem = Extract<AuditTranscriptItem, { kind: "message" }>;
  type ToolCallItem = Extract<AuditTranscriptItem, { kind: "tool_call" }>;
  type ToolResultItem = Extract<AuditTranscriptItem, { kind: "tool_result" }>;
  type MediaItem = Extract<AuditTranscriptItem, { kind: "image" } | { kind: "file" }>;
  type TranscriptBlock =
    | { kind: "message"; message: MessageItem }
    | { kind: "tool"; call: ToolCallItem | null; results: ToolResultItem[] }
    | { kind: "media"; item: MediaItem };

  interface Props {
    items: AuditTranscriptItem[];
  }

  let { items }: Props = $props();

  function roleLabel(role: "system" | "user" | "assistant"): string {
    if (role === "system") return $translate("controlPlane.auditRoleSystem");
    if (role === "user") return $translate("controlPlane.auditRoleUser");
    return $translate("controlPlane.auditRoleAssistant");
  }

  function isLongText(text: string): boolean {
    return text.length > 1_200 || text.split("\n").length > 18;
  }

  function previewText(text: string): string {
    const compact = text.trim().replace(/\s+/g, " ");
    return compact.length > 320 ? `${compact.slice(0, 320).trimEnd()}…` : compact;
  }

  function transcriptBlocks(transcript: AuditTranscriptItem[]): TranscriptBlock[] {
    const blocks: TranscriptBlock[] = [];
    const callsByID = new Map<string, Extract<TranscriptBlock, { kind: "tool" }>>();
    let latestTool: Extract<TranscriptBlock, { kind: "tool" }> | undefined;

    for (const item of transcript) {
      if (item.kind === "image" || item.kind === "file") {
        blocks.push({ kind: "media", item });
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

  let blocks = $derived(transcriptBlocks(items));
</script>

<div class="space-y-3" aria-label={$translate("controlPlane.auditTranscript")}>
  {#each blocks as block, index (`${block.kind}-${index}`)}
    {#if block.kind === "message"}
      {#if block.message.role === "system"}
        <details class="overflow-hidden rounded-lg border bg-muted/30">
          <summary class="flex cursor-pointer items-start gap-2 px-3 py-2 text-sm font-medium">
            <Braces class="size-4" aria-hidden="true" />
            <div class="min-w-0">
              <div>{roleLabel(block.message.role)}</div>
              {#if isLongText(block.message.text)}
                <p class="mt-1 max-h-[3.75rem] overflow-hidden whitespace-pre-wrap break-words text-xs leading-5 font-normal text-muted-foreground">{previewText(block.message.text)}</p>
                <span class="mt-1 inline-block text-xs font-normal text-muted-foreground underline underline-offset-2">{$translate("controlPlane.auditShowFull")}</span>
              {/if}
            </div>
          </summary>
          <div class="max-h-72 overflow-auto border-t px-3 py-2 text-sm whitespace-pre-wrap break-words">{block.message.text}</div>
        </details>
      {:else}
        <article class={`flex ${block.message.role === "user" ? "justify-end" : "justify-start"}`}>
          {#if isLongText(block.message.text)}
            <details class={`max-w-[92%] overflow-hidden rounded-lg text-sm sm:max-w-[80%] ${block.message.role === "user" ? "bg-primary text-primary-foreground" : "border bg-card"}`}>
              <summary class={`flex cursor-pointer items-start gap-1.5 px-4 py-3 text-xs font-medium ${block.message.role === "user" ? "text-primary-foreground/80" : "text-muted-foreground"}`}>
                {#if block.message.role === "user"}
                  <UserRound class="size-3.5" aria-hidden="true" />
                {:else}
                  <Bot class="size-3.5" aria-hidden="true" />
                {/if}
                <div class="min-w-0">
                  <div>{$translate("controlPlane.auditLongContext", { role: roleLabel(block.message.role), count: block.message.text.length })}</div>
                  <p class={`mt-1 max-h-[3.75rem] overflow-hidden whitespace-pre-wrap break-words text-xs leading-5 font-normal ${block.message.role === "user" ? "text-primary-foreground" : "text-foreground"}`}>{previewText(block.message.text)}</p>
                  <span class="mt-1 inline-block underline underline-offset-2">{$translate("controlPlane.auditShowFull")}</span>
                </div>
              </summary>
              <div class={`max-h-72 overflow-auto border-t px-4 py-3 whitespace-pre-wrap break-words leading-6 ${block.message.role === "user" ? "border-primary-foreground/20" : ""}`}>{block.message.text}</div>
            </details>
          {:else}
            <div class={`max-w-[92%] rounded-lg px-4 py-3 text-sm sm:max-w-[80%] ${block.message.role === "user" ? "bg-primary text-primary-foreground" : "border bg-card"}`}>
              <div class={`mb-1 flex items-center gap-1.5 text-xs font-medium ${block.message.role === "user" ? "text-primary-foreground/80" : "text-muted-foreground"}`}>
                {#if block.message.role === "user"}
                  <UserRound class="size-3.5" aria-hidden="true" />
                {:else}
                  <Bot class="size-3.5" aria-hidden="true" />
                {/if}
                {roleLabel(block.message.role)}
              </div>
              <div class="whitespace-pre-wrap break-words leading-6">{block.message.text}</div>
            </div>
          {/if}
        </article>
      {/if}
    {:else if block.kind === "media"}
      <figure class="mx-auto w-full max-w-[92%] overflow-hidden rounded-lg border bg-muted/30 text-sm sm:max-w-[80%]">
        {#if block.item.kind === "image"}
          <a href={block.item.dataUrl} target="_blank" rel="noreferrer" class="block bg-[repeating-conic-gradient(#8882_0%_25%,transparent_0%_50%)] bg-[length:16px_16px] p-1">
            <img src={block.item.dataUrl} alt={block.item.label} class="mx-auto max-h-64 w-auto max-w-full rounded object-contain" loading="lazy" />
          </a>
        {:else}
          <div class="flex items-center gap-2 px-3 py-2">
            <FileText class="size-4 text-muted-foreground" aria-hidden="true" />
            <span class="min-w-0 truncate font-medium">{block.item.label}</span>
          </div>
        {/if}
        <figcaption class="flex items-center gap-2 border-t px-3 py-2 text-xs text-muted-foreground">
          {#if block.item.kind === "image"}<ImageIcon class="size-3.5" aria-hidden="true" />{:else}<FileText class="size-3.5" aria-hidden="true" />{/if}
          <span class="truncate">{block.item.kind === "image" ? $translate("controlPlane.auditMediaImage") : $translate("controlPlane.auditMediaFile")} · {block.item.label}{#if block.item.sizeBytes > 0} · {formatBytes(block.item.sizeBytes)}{/if}</span>
          <a class="ml-auto inline-flex shrink-0 items-center gap-1 underline underline-offset-2 hover:text-foreground" href={block.item.dataUrl} download={block.item.label}>
            <Download class="size-3.5" aria-hidden="true" />{$translate("controlPlane.auditMediaDownload")}
          </a>
        </figcaption>
      </figure>
    {:else}
      <article class="mr-auto w-full max-w-[92%] overflow-hidden rounded-lg border bg-muted/30 text-sm sm:max-w-[80%]">
        {#if block.call}
          <div class="flex items-center gap-2 border-b px-3 py-2">
            <Wrench class="size-4 text-muted-foreground" aria-hidden="true" />
            <span class="font-medium">{$translate("controlPlane.auditToolCall")}</span>
            <code class="rounded bg-background px-1.5 py-0.5 font-mono text-xs">{block.call.name}</code>
            {#if block.call.id}<span class="ml-auto truncate font-mono text-xs text-muted-foreground">{block.call.id}</span>{/if}
          </div>
          {#if block.call.fields.length}
            <dl class="grid gap-x-3 gap-y-2 p-3 text-xs sm:grid-cols-[minmax(7rem,0.3fr)_1fr]">
              {#each block.call.fields as field (`${field.name}-${field.value}`)}
                <dt class="font-mono text-muted-foreground">{field.name}</dt>
                <dd class="min-w-0 whitespace-pre-wrap break-words">{field.value}</dd>
              {/each}
            </dl>
          {/if}
        {/if}
        {#each block.results as result, resultIndex (`${result.id}-${resultIndex}`)}
          <section class={block.call ? "border-t px-3 py-2" : "px-3 py-2"}>
            <div class="mb-1 flex items-center gap-2 text-xs">
              <Wrench class="size-3.5 text-muted-foreground" aria-hidden="true" />
              <span class="font-medium">{$translate("controlPlane.auditToolResult")}</span>
              {#if result.name || block.call?.name}<code class="rounded bg-background px-1.5 py-0.5 font-mono text-xs">{result.name || block.call?.name}</code>{/if}
              {#if !block.call && result.id}<span class="ml-auto truncate font-mono text-xs text-muted-foreground">{result.id}</span>{/if}
            </div>
            {#if isLongText(result.text)}
              <details class="rounded border bg-background/60">
                <summary class="cursor-pointer px-3 py-2 text-xs text-muted-foreground">
                  <div>{$translate("controlPlane.auditLongContext", { role: $translate("controlPlane.auditToolResult"), count: result.text.length })}</div>
                  <p class="mt-1 max-h-[3.75rem] overflow-hidden whitespace-pre-wrap break-words leading-5 text-foreground">{previewText(result.text)}</p>
                  <span class="mt-1 inline-block underline underline-offset-2">{$translate("controlPlane.auditShowFull")}</span>
                </summary>
                <div class="max-h-72 overflow-auto border-t px-3 py-2 whitespace-pre-wrap break-words text-sm leading-6">{result.text}</div>
              </details>
            {:else}
              <div class="whitespace-pre-wrap break-words text-sm leading-6">{result.text}</div>
            {/if}
          </section>
        {/each}
      </article>
    {/if}
  {/each}
</div>
