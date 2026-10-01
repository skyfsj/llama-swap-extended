<script lang="ts">
  import { ArrowLeft, BookOpen, Check, Copy, Search } from "@lucide/svelte";
  import { push } from "svelte-spa-router";
  import { Button } from "$lib/components/ui/button/index.js";
  import { extensionDocSections, quickStart } from "$lib/extensionDocs";
  import { renderMarkdown } from "$lib/markdown";

  let query = $state("");
  let selected = $state("start");
  let copied = $state(false);
  let copyError = $state("");
  let contentPane: HTMLElement;
  const sections = extensionDocSections.map((section) => ({ ...section, html: renderMarkdown(section.content) }));
  const filtered = $derived(sections.filter((section) => `${section.title} ${section.description} ${section.content}`.toLowerCase().includes(query.trim().toLowerCase())));
  const active = $derived(filtered.find((section) => section.id === selected) ?? filtered[0]);

  function select(id: string) {
    selected = id;
    contentPane?.scrollTo({ top: 0 });
  }
  async function copyExample() {
    try {
      await navigator.clipboard.writeText(quickStart);
      copied = true;
      copyError = "";
      setTimeout(() => copied = false, 2000);
    } catch {
      copyError = "复制失败，请选中代码后复制。";
    }
  }
</script>

<svelte:head><title>拓展开发文档 · llama-swap</title></svelte:head>

<div class="flex h-full min-h-0 flex-col bg-background">
  <header class="flex shrink-0 flex-wrap items-center gap-3 border-b px-5 py-4">
    <Button variant="ghost" size="icon" onclick={() => push("/extensions")} aria-label="返回拓展管理器"><ArrowLeft class="size-4" /></Button>
    <BookOpen class="size-5 text-primary" />
    <div class="min-w-0 flex-1"><h1 class="!pb-0 text-lg font-semibold">拓展开发文档</h1><p class="text-xs text-muted-foreground">快速上手、生命周期与 API 参考</p></div>
    <Button variant="outline" size="sm" onclick={() => push("/extensions/new")}>新建拓展</Button>
  </header>
  <div class="docs-layout grid min-h-0 flex-1">
    <aside class="min-h-0 overflow-y-auto border-r bg-muted/20 p-4">
      <label class="mb-4 flex items-center gap-2 rounded-md border bg-background px-3 py-2">
        <Search class="size-4 shrink-0 text-muted-foreground" />
        <input aria-label="搜索开发文档" placeholder="搜索文档或 API…" bind:value={query} class="w-full min-w-0 bg-transparent text-sm outline-none" />
      </label>
      <nav aria-label="文档目录" class="flex flex-col gap-1">
        {#each filtered as section}
          <button onclick={() => select(section.id)} aria-current={active?.id === section.id ? "page" : undefined} class={`rounded-md px-3 py-2.5 text-left text-sm transition-colors ${active?.id === section.id ? "bg-primary/10 font-medium text-primary" : "text-muted-foreground hover:bg-muted hover:text-foreground"}`}>{section.title}</button>
        {/each}
      </nav>
      {#if !filtered.length}<p class="px-3 py-4 text-sm text-muted-foreground">未找到匹配内容</p>{/if}
    </aside>
    <main bind:this={contentPane} class="min-w-0 overflow-y-auto px-5 py-6 sm:px-8 lg:px-12">
      {#if active}
        <article class="mx-auto max-w-4xl">
          <div class="mb-7 flex flex-wrap items-start justify-between gap-3 border-b pb-5">
            <div><h2 class="!pb-0 text-2xl font-semibold tracking-tight">{active.title}</h2><p class="mt-2 text-sm text-muted-foreground">{active.description}</p></div>
            {#if active.id === "start"}<Button variant="outline" size="sm" onclick={copyExample}>{#if copied}<Check class="size-4" />已复制{:else}<Copy class="size-4" />复制示例{/if}</Button>{/if}
          </div>
          {#if copyError}<p role="status" class="mb-3 text-sm text-destructive">{copyError}</p>{/if}
          <div class="documentation text-sm leading-7">{@html active.html}</div>
        </article>
      {:else}
        <div class="py-20 text-center"><Search class="mx-auto mb-4 size-8 text-muted-foreground" /><h2 class="font-medium">没有找到相关文档</h2><p class="mt-2 text-sm text-muted-foreground">试试钩子名称、配置字段或 API 方法。</p><Button variant="ghost" class="mt-4" onclick={() => query = ""}>清除搜索</Button></div>
      {/if}
    </main>
  </div>
</div>

<style>
  .docs-layout { grid-template-columns: 14rem minmax(0, 1fr); }
  .documentation :global(p) { margin: 0 0 1rem; }
  .documentation :global(h3) { margin: 1.75rem 0 .75rem; font-size: 1rem; font-weight: 600; }
  .documentation :global(ol), .documentation :global(ul) { margin: 0 0 1.25rem; padding-left: 1.5rem; list-style: revert; }
  .documentation :global(li) { margin-bottom: .5rem; }
  .documentation :global(pre) { overflow-x: auto; padding: 1rem 1.25rem; margin: 1.25rem 0; border: 1px solid var(--border); border-radius: .5rem; background: var(--muted); line-height: 1.65; font-size: .8125rem; }
  .documentation :global(code) { font-family: var(--font-mono); }
  .documentation :global(table) { display: block; max-width: 100%; overflow-x: auto; border-collapse: collapse; margin: 1.25rem 0; font-size: .8125rem; }
  .documentation :global(th), .documentation :global(td) { padding: .7rem .85rem; border: 1px solid var(--border); text-align: left; vertical-align: top; }
  .documentation :global(th) { background: var(--muted); font-weight: 600; white-space: nowrap; }
  .documentation :global(td:first-child) { min-width: 10rem; font-family: var(--font-mono); }
  @media (max-width: 767px) {
    .docs-layout { grid-template-columns: minmax(0, 1fr); grid-template-rows: auto minmax(0, 1fr); }
    aside { border-right: 0; border-bottom: 1px solid var(--border); padding: .75rem 1rem; }
    aside label { margin-bottom: .5rem; }
    nav { flex-direction: row; overflow-x: auto; }
    nav button { flex-shrink: 0; white-space: nowrap; }
  }
</style>
