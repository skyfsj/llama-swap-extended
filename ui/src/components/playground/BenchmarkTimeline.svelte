<script lang="ts">
  import { ChevronDown, ChevronRight } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  import type {
    BenchmarkPhase,
    BenchmarkPlan,
    BenchmarkRunView,
  } from "../../lib/playgroundBenchmark";

  interface Props {
    plans: BenchmarkPlan[];
    runs: Record<string, BenchmarkRunView>;
    collapsed?: boolean;
  }

  let { plans, runs, collapsed = $bindable(false) }: Props = $props();

  let timelineMaxMs = $derived(Math.max(100, ...plans.map((plan) => runs[plan.id]?.elapsedMs ?? 0)));

  function niceStepMs(maxMs: number): number {
    if (maxMs <= 500) return 100;
    if (maxMs <= 2_000) return 500;
    if (maxMs <= 5_000) return 1_000;
    if (maxMs <= 20_000) return 5_000;
    if (maxMs <= 60_000) return 10_000;
    return 30_000;
  }

  let timelineTicks = $derived.by(() => {
    const step = niceStepMs(timelineMaxMs);
    const ticks: number[] = [];
    for (let tick = 0; tick <= timelineMaxMs; tick += step) ticks.push(tick);
    return ticks;
  });

  const phaseColors: Record<BenchmarkPhase, string> = {
    waiting: "var(--muted-foreground)",
    loading: "var(--info)",
    reasoning: "var(--chart-3)",
    content: "var(--warning)",
  };

  function phaseColor(run: BenchmarkRunView, phase: BenchmarkPhase): string {
    if ((run.status === "error" || run.status === "cancelled") && run.phase === phase) {
      return "var(--destructive)";
    }
    if (phase === "content" && run.status === "done") return "var(--success)";
    return phaseColors[phase];
  }

  function formatElapsed(ms: number): string {
    if (ms < 1_000) return `${Math.round(ms)}ms`;
    return `${(ms / 1_000).toFixed(2)}s`;
  }

  function formatTick(ms: number): string {
    if (ms < 1_000) return `${ms}`;
    return `${(ms / 1_000).toFixed(ms % 1_000 === 0 ? 0 : 1)}s`;
  }

  function phaseWidth(run: BenchmarkRunView, phase: BenchmarkPhase): number {
    const duration = phase === "waiting"
      ? run.waitingMs
      : phase === "loading"
        ? run.loadingMs
        : phase === "reasoning"
          ? run.reasoningMs
          : run.contentMs;
    return (duration / timelineMaxMs) * 100;
  }

  function phaseOffset(run: BenchmarkRunView, phase: BenchmarkPhase): number {
    if (phase === "waiting") return 0;
    if (phase === "loading") return phaseWidth(run, "waiting");
    if (phase === "reasoning") return phaseWidth(run, "waiting") + phaseWidth(run, "loading");
    return phaseWidth(run, "waiting") + phaseWidth(run, "loading") + phaseWidth(run, "reasoning");
  }

  function phaseDuration(run: BenchmarkRunView, phase: BenchmarkPhase): number {
    if (phase === "waiting") return run.waitingMs;
    if (phase === "loading") return run.loadingMs;
    if (phase === "reasoning") return run.reasoningMs;
    return run.contentMs;
  }
</script>

<section class="border-b border-border" aria-labelledby="benchmark-timeline-title">
  <button
    type="button"
    class="hover:bg-muted/50 flex w-full items-center gap-2 px-3 py-2 text-left transition-colors"
    onclick={() => (collapsed = !collapsed)}
    aria-expanded={!collapsed}
  >
    {#if collapsed}
      <ChevronRight class="text-muted-foreground size-4" />
    {:else}
      <ChevronDown class="text-muted-foreground size-4" />
    {/if}
    <span id="benchmark-timeline-title" class="text-xs font-semibold">
      {$translate("playground.concurrency.timeline")}
    </span>
    <span class="text-muted-foreground ml-auto text-[11px] tabular-nums">
      {$translate(plans.length === 1 ? "playground.concurrency.maxRequest" : "playground.concurrency.maxRequests", {
        elapsed: formatElapsed(timelineMaxMs),
        count: plans.length,
      })}
    </span>
  </button>

  {#if !collapsed}
    <div class="border-t border-border px-3 py-2.5">
      <div class="mb-2 flex flex-wrap gap-x-3 gap-y-1 text-[10px] text-muted-foreground" aria-hidden="true">
        <span class="inline-flex items-center gap-1"><i class="size-2 bg-muted-foreground"></i>{$translate("status.request.waiting")}</span>
        <span class="inline-flex items-center gap-1"><i class="size-2 bg-info"></i>{$translate("status.request.loading")}</span>
        <span class="inline-flex items-center gap-1"><i class="size-2 bg-chart-3"></i>{$translate("status.request.reasoning")}</span>
        <span class="inline-flex items-center gap-1"><i class="size-2 bg-warning"></i>{$translate("status.request.streaming")}</span>
        <span class="inline-flex items-center gap-1"><i class="size-2 bg-success"></i>{$translate("status.request.done")}</span>
        <span class="inline-flex items-center gap-1"><i class="size-2 bg-destructive"></i>{$translate("status.request.error")}</span>
      </div>

      <div class="overflow-x-auto pb-1">
        <div class="min-w-[34rem]">
          <div class="flex" aria-hidden="true">
            <div class="w-36 shrink-0 sm:w-44"></div>
            <div class="relative h-4 flex-1 border-b border-border">
              {#each timelineTicks as tick (tick)}
                <div class="absolute inset-y-0 border-l border-border" style:left={`${(tick / timelineMaxMs) * 100}%`}>
                  <span class="text-muted-foreground absolute top-0 left-1 text-[9px] tabular-nums">{formatTick(tick)}</span>
                </div>
              {/each}
            </div>
            <div class="w-14 shrink-0"></div>
          </div>

          <div class="mt-1 space-y-1">
            {#each plans as plan, index (plan.id)}
              {@const run = runs[plan.id]}
              <div class="flex h-5 items-center text-[11px]">
                <div class="flex w-36 shrink-0 items-center gap-1.5 pr-2 sm:w-44">
                  <span class="text-muted-foreground w-4 text-right tabular-nums">{index + 1}</span>
                  <span class="min-w-0 flex-1 truncate font-medium" title={plan.model}>{plan.model}</span>
                  {#if plan.kind !== "request"}
                    <span class="text-muted-foreground shrink-0 text-[9px]">
                      {$translate(`playground.concurrency.${plan.kind}`)}
                    </span>
                  {/if}
                </div>
                <div class="bg-muted/35 relative h-3 flex-1 overflow-hidden">
                  {#each timelineTicks as tick (tick)}
                    <div
                      class="absolute inset-y-0 border-l border-border/70"
                      style:left={`${(tick / timelineMaxMs) * 100}%`}
                      aria-hidden="true"
                    ></div>
                  {/each}
                  {#if run}
                    {#each ["waiting", "loading", "reasoning", "content"] as phase (phase)}
                      {@const typedPhase = phase as BenchmarkPhase}
                      {@const duration = phaseDuration(run, typedPhase)}
                      {#if duration > 0}
                        <span
                          class="absolute inset-y-0 transition-[left,width] duration-150"
                          style:left={`${phaseOffset(run, typedPhase)}%`}
                          style:width={`${phaseWidth(run, typedPhase)}%`}
                          style:background-color={phaseColor(run, typedPhase)}
                          title={`${$translate(`status.request.${typedPhase === "content" ? "streaming" : typedPhase}`)} ${formatElapsed(duration)}`}
                        ></span>
                      {/if}
                    {/each}
                  {/if}
                </div>
                <span class="text-muted-foreground w-14 shrink-0 text-right tabular-nums">
                  {run ? formatElapsed(run.elapsedMs) : "—"}
                </span>
              </div>
            {/each}
          </div>
        </div>
      </div>
    </div>
  {/if}
</section>
