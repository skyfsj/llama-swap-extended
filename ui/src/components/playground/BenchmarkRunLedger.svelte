<script lang="ts">
  import { ChevronDown, ChevronRight } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../../lib/i18n";
  import type { BenchmarkPlan, BenchmarkRunStatus, BenchmarkRunView } from "../../lib/playgroundBenchmark";

  interface Props {
    plans: BenchmarkPlan[];
    runs: Record<string, BenchmarkRunView>;
  }

  let { plans, runs }: Props = $props();
  let expanded = $state<Set<string>>(new Set());

  function toggle(planId: string): void {
    const next = new Set(expanded);
    if (next.has(planId)) next.delete(planId);
    else next.add(planId);
    expanded = next;
  }

  function formatElapsed(ms: number | null): string {
    if (ms === null) return "—";
    if (ms < 1_000) return `${Math.round(ms)}ms`;
    return `${(ms / 1_000).toFixed(2)}s`;
  }

  function formatTokens(tokens: number): string {
    return Math.max(0, Math.round(tokens)).toLocaleString();
  }

  function outputRate(run: BenchmarkRunView | undefined): string {
    if (!run || run.contentMs <= 0 || run.outputTokens <= 0) return "—";
    return `${(run.outputTokens / (run.contentMs / 1_000)).toFixed(1)} t/s`;
  }

  function statusDotClass(status: BenchmarkRunStatus): string {
    if (status === "done") return "bg-success";
    if (status === "streaming") return "bg-warning";
    if (status === "error") return "bg-destructive";
    if (status === "cancelled") return "bg-muted-foreground";
    return "bg-border";
  }

  function hasDetails(run: BenchmarkRunView | undefined): boolean {
    return Boolean(run?.loadingText || run?.reasoningContent || run?.content || run?.error);
  }
</script>

<section class="min-h-0" aria-labelledby="benchmark-results-title">
  <div class="flex items-center border-b border-border px-3 py-2">
    <h3 id="benchmark-results-title" class="text-xs font-semibold">
      {$translate("playground.concurrency.results")}
    </h3>
    <span class="text-muted-foreground ml-auto text-[11px] tabular-nums">
      {$translate("playground.concurrency.requestCount", { count: plans.length })}
    </span>
  </div>

  <div role="list" aria-label={$translate("playground.concurrency.results")}>
    {#each plans as plan, index (plan.id)}
      {@const run = runs[plan.id]}
      {@const status = run?.status ?? "waiting"}
      {@const detailsAvailable = hasDetails(run)}
      <article class="border-b border-border last:border-b-0" role="listitem">
        <div class="px-3 py-2.5">
          <div class="flex min-w-0 items-start gap-2">
            <span class="text-muted-foreground mt-0.5 w-5 shrink-0 text-right text-[11px] tabular-nums">{index + 1}</span>
            <div class="min-w-0 flex-1">
              <div class="flex min-w-0 flex-wrap items-center gap-x-2 gap-y-1">
                <span class="min-w-0 truncate text-sm font-medium" title={plan.model}>{plan.model}</span>
                <span class="border-border text-muted-foreground border px-1.5 py-0.5 text-[9px] leading-none uppercase">
                  {$translate(`playground.concurrency.${plan.kind}`)}
                </span>
              </div>
              <div class="mt-2 grid grid-cols-2 gap-x-3 gap-y-2 sm:grid-cols-5">
                <div class="min-w-0">
                  <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.status")}</div>
                  <div class="mt-0.5 flex items-center gap-1.5 text-[11px]">
                    <span class="size-1.5 shrink-0 rounded-full {statusDotClass(status)}"></span>
                    <span class="truncate">{$translate(`status.request.${status}`)}</span>
                  </div>
                </div>
                <div>
                  <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.promptTokens")}</div>
                  <div class="mt-0.5 text-[11px] tabular-nums">{formatTokens(run?.promptTokens ?? plan.estimatedPromptTokens)}</div>
                </div>
                <div>
                  <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.firstToken")}</div>
                  <div class="mt-0.5 text-[11px] tabular-nums">{formatElapsed(run?.firstTokenMs ?? null)}</div>
                </div>
                <div>
                  <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.duration")}</div>
                  <div class="mt-0.5 text-[11px] tabular-nums">{run ? formatElapsed(run.elapsedMs) : "—"}</div>
                </div>
                <div>
                  <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.outputRate")}</div>
                  <div class="mt-0.5 text-[11px] tabular-nums">{outputRate(run)}</div>
                </div>
              </div>
            </div>
            <Button
              variant="ghost"
              size="icon-xs"
              class="mt-0.5 shrink-0"
              onclick={() => toggle(plan.id)}
              disabled={!detailsAvailable}
              aria-label={$translate(expanded.has(plan.id) ? "playground.concurrency.hideOutput" : "playground.concurrency.showOutput", { model: plan.model })}
              title={$translate(expanded.has(plan.id) ? "playground.concurrency.hideOutput" : "playground.concurrency.showOutput", { model: plan.model })}
            >
              {#if expanded.has(plan.id)}
                <ChevronDown />
              {:else}
                <ChevronRight />
              {/if}
            </Button>
          </div>
        </div>

        {#if expanded.has(plan.id) && run}
          <div class="bg-muted/30 max-h-72 overflow-auto border-t border-border px-3 py-2 font-mono text-[11px] leading-5">
            {#if run.loadingText}
              <section class="mb-3">
                <h4 class="text-muted-foreground mb-1 font-sans text-[9px] font-semibold uppercase">
                  {$translate("playground.concurrency.loadingOutput")}
                </h4>
                <pre class="whitespace-pre-wrap break-words font-inherit">{run.loadingText.trim()}</pre>
              </section>
            {/if}
            {#if run.reasoningContent}
              <section class="mb-3">
                <h4 class="text-muted-foreground mb-1 font-sans text-[9px] font-semibold uppercase">
                  {$translate("playground.concurrency.reasoningOutput")}
                </h4>
                <pre class="text-chart-3 whitespace-pre-wrap break-words font-inherit">{run.reasoningContent}</pre>
              </section>
            {/if}
            {#if run.content}
              <section>
                <h4 class="text-muted-foreground mb-1 font-sans text-[9px] font-semibold uppercase">
                  {$translate("playground.concurrency.responseOutput")}
                </h4>
                <pre class="whitespace-pre-wrap break-words font-inherit">{run.content}</pre>
              </section>
            {/if}
            {#if run.error}
              <div class="text-destructive">{$translate("playground.concurrency.error", { message: run.error })}</div>
            {/if}
          </div>
        {/if}
      </article>
    {/each}
  </div>
</section>
