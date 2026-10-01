<script lang="ts">
  import { renderMarkdown, escapeHtml, renderStreamingMarkdown, createStreamingCache } from "../../lib/markdown";
  import type { RenderedBlock } from "../../lib/markdown";
  import { Copy, Check, Pencil, X, Save, RefreshCw, ChevronDown, ChevronRight, Brain, Code } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import { getTextContent, getImageUrls } from "../../lib/types";
  import type { ContentPart } from "../../lib/types";
  import { formatDuration } from "../../lib/format";
  import { copyText } from "../../lib/clipboard";
  import { isSubmitEnter } from "../../lib/ime";
  import { t, translate } from "../../lib/i18n";

  interface Props {
    role: "user" | "assistant" | "system";
    content: string | ContentPart[];
    reasoning_content?: string;
    reasoningTimeMs?: number;
    isStreaming?: boolean;
    isReasoning?: boolean;
    onEdit?: (newContent: string) => void;
    onRegenerate?: () => void;
  }

  let { role, content, reasoning_content = "", reasoningTimeMs = 0, isStreaming = false, isReasoning = false, onEdit, onRegenerate }: Props = $props();

  let textContent = $derived(getTextContent(content));
  let imageUrls = $derived(getImageUrls(content));
  let hasImages = $derived(imageUrls.length > 0);
  let canEdit = $derived(onEdit !== undefined && !hasImages);

  let streamingCache = createStreamingCache();
  let renderedParts = $derived.by(() => {
    if (role !== "assistant") {
      return { blocks: [{ id: -1, html: escapeHtml(textContent).replace(/\n/g, '<br>') }] as RenderedBlock[], pendingHtml: "" };
    }
    if (!isStreaming) {
      streamingCache = createStreamingCache();
      return { blocks: [{ id: -1, html: renderMarkdown(textContent) }] as RenderedBlock[], pendingHtml: "" };
    }
    return renderStreamingMarkdown(textContent, streamingCache);
  });
  let copied = $state(false);
  let showRaw = $state(false);
  let isEditing = $state(false);
  let editContent = $state("");
  let showReasoning = $state(false);
  let modalImageUrl = $state<string | null>(null);

  async function copyToClipboard() {
    if (await copyText(textContent)) {
      copied = true;
      setTimeout(() => (copied = false), 2000);
    }
  }

  function startEdit() {
    editContent = textContent;
    isEditing = true;
  }

  function cancelEdit() {
    isEditing = false;
    editContent = "";
  }

  function saveEdit() {
    if (onEdit && editContent.trim() !== textContent) {
      onEdit(editContent.trim());
    }
    isEditing = false;
    editContent = "";
  }

  function openModal(imageUrl: string) {
    modalImageUrl = imageUrl;
    document.body.style.overflow = "hidden";
  }

  function closeModal(event?: MouseEvent) {
    // Only close if clicking the background, not the image
    if (event && event.target !== event.currentTarget) {
      return;
    }
    modalImageUrl = null;
    document.body.style.overflow = "";
  }

  function handleModalKeyDown(event: KeyboardEvent) {
    if (event.key === "Escape") {
      closeModal();
    }
  }

  function handleKeyDown(event: KeyboardEvent) {
    if (isSubmitEnter(event)) {
      event.preventDefault();
      saveEdit();
    } else if (event.key === "Escape") {
      cancelEdit();
    }
  }

  const COPY_SVG = `<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><rect width="14" height="14" x="8" y="8" rx="2" ry="2"/><path d="M4 16c-1.1 0-2-.9-2-2V4c0-1.1.9-2 2-2h10c0 1.1.9 2 2 2"/></svg>`;
  const CHECK_SVG = `<svg xmlns="http://www.w3.org/2000/svg" width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M20 6 9 17l-5-5"/></svg>`;

  function codeBlockCopy(node: HTMLElement) {
    function attachButtons() {
      node.querySelectorAll<HTMLPreElement>('pre:not([data-copy-btn])').forEach(pre => {
        pre.setAttribute('data-copy-btn', 'true');
        const btn = document.createElement('button');
        btn.className = 'code-copy-btn';
        btn.title = t("playground.chat.copyCode");
        btn.innerHTML = COPY_SVG;
        btn.addEventListener('click', async () => {
          const text = pre.querySelector('code')?.textContent ?? pre.textContent ?? '';
          if (await copyText(text)) {
            btn.innerHTML = CHECK_SVG;
            btn.classList.add('copied');
            setTimeout(() => { btn.innerHTML = COPY_SVG; btn.classList.remove('copied'); }, 2000);
          }
        });
        pre.appendChild(btn);
      });
    }
    attachButtons();
    const mo = new MutationObserver(attachButtons);
    mo.observe(node, { childList: true, subtree: true });
    return { destroy: () => mo.disconnect() };
  }
</script>

<div class="flex {role === 'user' ? 'justify-end' : 'justify-start'} mb-3 last:mb-0">
  {#if role === "user"}
    <div class="pg-bubble group relative max-w-[85%] px-3.5 py-2.5">
      {#if isEditing}
        <div class="flex min-w-[300px] flex-col gap-2">
          <Textarea class="resize-none" rows={3} bind:value={editContent} onkeydown={handleKeyDown} />
          <div class="flex justify-end gap-2">
            <Button variant="ghost" size="icon-sm" class="pg-tool" onclick={cancelEdit} title={$translate("common.cancel")}>
              <X />
            </Button>
            <Button variant="ghost" size="icon-sm" class="pg-tool" onclick={saveEdit} title={$translate("common.save")}>
              <Save />
            </Button>
          </div>
        </div>
      {:else}
        {#if hasImages}
          <div class="mb-2 flex flex-wrap gap-2">
            {#each imageUrls as imageUrl, idx (idx)}
              <button
                onclick={() => openModal(imageUrl)}
                class="cursor-pointer overflow-hidden rounded-[var(--pg-r-control)] border border-black/10 transition-opacity hover:opacity-80 dark:border-white/10"
              >
                <img
                  src={imageUrl}
                  alt={$translate("playground.chat.image", { index: idx + 1 })}
                  class="max-w-[200px]"
                />
              </button>
            {/each}
          </div>
        {/if}
        <div class="whitespace-pre-wrap pr-7 text-[0.9375rem] leading-relaxed">{textContent}</div>
        {#if canEdit}
          <button
            class="absolute right-1.5 top-1.5 rounded-[var(--pg-r-chip)] p-1 text-current/60 opacity-0 transition-opacity hover:bg-black/5 hover:text-current group-hover:opacity-100 dark:hover:bg-white/10"
            onclick={startEdit}
            title={$translate("playground.chat.editMessage")}
          >
            <Pencil class="size-3.5" />
          </button>
        {/if}
      {/if}
    </div>
  {:else}
    <div class="pg-panel group relative w-full px-4 py-3">
      {#if reasoning_content || isReasoning}
        <div class="pg-inset mb-3 overflow-hidden">
          <button
            class="flex w-full items-center gap-2 px-3 py-1.5 text-[13px] text-[color:var(--pg-ink-2)] transition-colors hover:text-[color:var(--pg-ink)]"
            onclick={() => showReasoning = !showReasoning}
          >
            {#if showReasoning}
              <ChevronDown class="size-3.5" />
            {:else}
              <ChevronRight class="size-3.5" />
            {/if}
            <Brain class="size-3.5" />
            <span class="font-medium">{$translate("capture.reasoning")}</span>
            <span class="pg-chip">
              {reasoning_content.length}{$translate("playground.chat.characters")}{#if !isReasoning && reasoningTimeMs > 0} · {formatDuration(reasoningTimeMs, { precision: 1, subSecondMs: true })}{/if}
            </span>
            {#if isReasoning}
              <span class="ml-auto flex items-center gap-1.5">
                <span class="size-1.5 animate-pulse rounded-full bg-primary"></span>
                {$translate("status.request.reasoning")}...
              </span>
            {/if}
          </button>
          {#if showReasoning}
            <div class="text-[color:var(--pg-ink-3)] border-t pg-divide max-h-64 overflow-y-auto whitespace-pre-wrap px-3 py-2 font-mono text-xs leading-relaxed pg-scroll">
              {reasoning_content}{#if isReasoning}<span class="bg-current ml-0.5 inline-block h-3 w-1.5 animate-pulse align-middle"></span>{/if}
            </div>
          {/if}
        </div>
      {/if}
      {#if hasImages}
        <div class="mb-3 flex flex-wrap gap-2">
          {#each imageUrls as imageUrl, idx (idx)}
            <button
              onclick={() => openModal(imageUrl)}
              class="cursor-pointer overflow-hidden rounded-[var(--pg-r-control)] border border-black/[0.06] transition-opacity hover:opacity-80 dark:border-white/10"
            >
              <img
                src={imageUrl}
                alt={$translate("playground.chat.image", { index: idx + 1 })}
                class="max-h-64"
              />
            </button>
          {/each}
        </div>
      {/if}
      {#if showRaw}
        <div class="text-[color:var(--pg-ink-2)] whitespace-pre-wrap font-mono text-[13px] leading-relaxed">{textContent}</div>
      {:else}
        <div class="prose prose-sm dark:prose-invert max-w-none text-[0.9375rem]" use:codeBlockCopy>
          {#each renderedParts.blocks as block (block.id)}
            {@html block.html}
          {/each}
          {@html renderedParts.pendingHtml}
          {#if isStreaming && !isReasoning}
            <span class="pg-caret"></span>
          {/if}
        </div>
      {/if}
      {#if !isStreaming}
        <div class="mt-1.5 flex items-center gap-0.5">
          {#if onRegenerate}
            <Button variant="ghost" size="icon-xs" class="pg-tool" onclick={onRegenerate} title={$translate("playground.chat.regenerate")}>
              <RefreshCw />
            </Button>
          {/if}
          <Button
            variant="ghost"
            size="icon-xs"
            class="pg-tool"
            onclick={copyToClipboard}
            title={copied ? $translate("common.copied") : $translate("common.copy")}
          >
            {#if copied}
              <Check class="text-success" />
            {:else}
              <Copy />
            {/if}
          </Button>
          <Button
            variant="ghost"
            size="icon-xs"
            class={showRaw ? "pg-tool text-primary" : "pg-tool"}
            onclick={() => showRaw = !showRaw}
            title={$translate(showRaw ? "playground.chat.showRendered" : "playground.chat.showRaw")}
          >
            <Code />
          </Button>
        </div>
      {/if}
    </div>
  {/if}
</div>

<!-- Full-size image modal -->
{#if modalImageUrl}
  <div
    class="fixed inset-0 z-50 flex items-center justify-center bg-black/80 p-4"
    onclick={(e) => closeModal(e)}
    onkeydown={handleModalKeyDown}
    role="button"
    tabindex="-1"
  >
    <button
      class="absolute right-4 top-4 rounded-lg bg-white/10 p-2 text-white transition-colors hover:bg-white/20"
      onclick={() => closeModal()}
      title={$translate("common.close")}
    >
      <X class="size-6" />
    </button>
    <img
      src={modalImageUrl}
      alt=""
      class="max-w-full max-h-full rounded-md pointer-events-none"
    />
  </div>
{/if}

<style>
  .prose :global(pre) {
    position: relative;
    background-color: var(--pg-field);
    border: 1px solid var(--pg-line);
    border-radius: var(--pg-r-control);
    padding: 0.75rem;
    padding-right: 2.5rem;
    overflow-x: auto;
    margin: 0.5rem 0;
  }

  .prose :global(.code-copy-btn) {
    position: absolute;
    top: 0.375rem;
    right: 0.375rem;
    display: flex;
    align-items: center;
    justify-content: center;
    padding: 0.25rem;
    border-radius: 0.25rem;
    border: 1px solid var(--pg-line);
    background: var(--pg-surface);
    color: var(--pg-ink-2);
    cursor: pointer;
    transition: background-color 0.15s;
    line-height: 0;
  }

  .prose :global(.code-copy-btn:hover) {
    background: var(--pg-hover-2);
    color: var(--pg-ink);
  }

  .prose :global(.code-copy-btn.copied) {
    color: var(--success);
    opacity: 1;
  }

  .prose :global(code) {
    font-family: ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace;
    font-size: 0.875em;
  }

  .prose :global(pre code) {
    background: none;
    padding: 0;
  }

  .prose :global(code:not(pre code)) {
    background-color: var(--pg-field);
    padding: 0.125rem 0.25rem;
    border-radius: var(--pg-r-chip);
    border: 1px solid var(--pg-line);
  }

  .prose :global(p) {
    margin: 0.5rem 0;
  }

  .prose :global(p:first-child) {
    margin-top: 0;
  }

  .prose :global(p:last-child) {
    margin-bottom: 0;
  }

  .prose :global(ul),
  .prose :global(ol) {
    margin: 0.5rem 0;
    padding-left: 1.5rem;
  }

  .prose :global(li) {
    margin: 0.25rem 0;
  }

  .prose :global(h1),
  .prose :global(h2),
  .prose :global(h3),
  .prose :global(h4) {
    margin: 1rem 0 0.5rem 0;
    font-weight: 600;
  }

  .prose :global(h1:first-child),
  .prose :global(h2:first-child),
  .prose :global(h3:first-child),
  .prose :global(h4:first-child) {
    margin-top: 0;
  }

  .prose :global(blockquote) {
    border-left: 2px solid color-mix(in oklab, var(--primary) 50%, transparent);
    padding-left: 1rem;
    margin: 0.5rem 0;
    font-style: italic;
    color: var(--pg-ink-2);
  }

  .prose :global(a) {
    color: var(--primary);
    text-decoration: underline;
  }

  .prose :global(table) {
    width: 100%;
    border-collapse: collapse;
    margin: 0.5rem 0;
  }

  .prose :global(th),
  .prose :global(td) {
    border: 1px solid var(--pg-line);
    padding: 0.5rem;
    text-align: left;
  }

  .prose :global(th) {
    background-color: var(--pg-field);
    font-weight: 600;
  }

  /* Highlight.js theme overrides: the pre provides the surface in both modes */
  .prose :global(pre .hljs) {
    background: transparent;
  }
</style>
