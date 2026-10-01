<script lang="ts">
  import {
    Activity,
    CopyPlus,
    Eraser,
    FileText,
    Gauge,
    GripVertical,
    Play,
    Plus,
    RotateCcw,
    Square,
    Trash2,
    Users,
  } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import * as ToggleGroup from "$lib/components/ui/toggle-group/index.js";
  import { playgroundModels, profileModels, selectorModels } from "../../stores/api";
  import { persistentStore } from "../../stores/persistent";
  import { streamChatCompletion, type StreamUsage } from "../../lib/chatApi";
  import { t, translate } from "../../lib/i18n";
  import {
    createBenchmarkPlans,
    delayWithAbort,
    estimateTokens,
    runWithConcurrency,
    summarizeBenchmarkRuns,
    DEFAULT_CONTEXT_TOKENS,
    DEFAULT_PREFILL_REQUESTS,
    type BenchmarkMode,
    type BenchmarkPhase,
    type BenchmarkPlan,
    type BenchmarkQueueEntry,
    type BenchmarkRunView,
  } from "../../lib/playgroundBenchmark";
  import BenchmarkRunLedger from "./BenchmarkRunLedger.svelte";
  import BenchmarkTimeline from "./BenchmarkTimeline.svelte";
  import ModelSelector from "./ModelSelector.svelte";

  type RunState = BenchmarkRunView & { usage?: StreamUsage };
  type TestEntry = BenchmarkQueueEntry;

  const LOAD_MARKER = "━━━━━";
  const DEFAULT_PROMPT = "Write a few sentences about the history of computing.";
  const DEFAULT_MAX_TOKENS = 256;

  const modeStore = persistentStore<BenchmarkMode>("playground-benchmark-mode", "concurrency");
  const promptStore = persistentStore<string>("concurrency-prompt", DEFAULT_PROMPT);
  const maxTokensStore = persistentStore<number>("concurrency-max-tokens", DEFAULT_MAX_TOKENS);
  const parallelismStore = persistentStore<number>("benchmark-parallelism", 0);
  const contextTokensStore = persistentStore<number>("benchmark-context-tokens", DEFAULT_CONTEXT_TOKENS);
  const prefillRequestsStore = persistentStore<number>("benchmark-prefill-requests", DEFAULT_PREFILL_REQUESTS);
  const prefillTokensStore = persistentStore<number>("benchmark-prefill-tokens", DEFAULT_CONTEXT_TOKENS);
  const probeDelayStore = persistentStore<number>("benchmark-probe-delay", 100);
  const testListStore = persistentStore<TestEntry[]>("concurrency-test-list", []);
  const timelineCollapsedStore = persistentStore<boolean>("concurrency-timeline-collapsed", false);

  let runs = $state<Record<string, RunState>>({});
  let activePlans = $state<BenchmarkPlan[]>([]);
  let isRunning = $state(false);
  let abortController: AbortController | null = null;
  let selectedModel = $state("");
  let dragIndex = $state<number | null>(null);
  let dragOverIndex = $state<number | null>(null);

  let previewPlans = $derived.by(() => createBenchmarkPlans({
    mode: $modeStore,
    queue: $testListStore,
    prompt: $promptStore,
    maxTokens: $maxTokensStore,
    contextTokens: $modeStore === "async-prefill" ? $prefillTokensStore : $contextTokensStore,
    prefillRequests: $prefillRequestsStore,
  }));
  let visiblePlans = $derived(activePlans.length > 0 ? activePlans : previewPlans);
  let hasModels = $derived($playgroundModels.length + $profileModels.length + $selectorModels.length > 0);
  let canRun = $derived(!isRunning && $testListStore.length > 0 && (
    $modeStore !== "concurrency" || $promptStore.trim() !== ""
  ));

  function samplesFor(plans: BenchmarkPlan[]) {
    return plans.map((plan) => {
      const run = runs[plan.id];
      return {
        status: run?.status ?? "waiting" as const,
        elapsedMs: run?.elapsedMs ?? 0,
        firstTokenMs: run?.firstTokenMs ?? null,
        promptTokens: run?.promptTokens ?? plan.estimatedPromptTokens,
        outputTokens: run?.outputTokens ?? 0,
      };
    });
  }

  let summary = $derived.by(() => summarizeBenchmarkRuns(samplesFor(visiblePlans)));
  let probeSummary = $derived.by(() => summarizeBenchmarkRuns(samplesFor(
    visiblePlans.filter((plan) => plan.kind === "probe"),
  )));
  let terminalCount = $derived(summary.completed + summary.failed + summary.cancelled);
  let progressPercent = $derived(summary.total > 0 ? Math.min(100, (terminalCount / summary.total) * 100) : 0);
  let plannedParallelism = $derived(parallelLimit(visiblePlans.length));
  let runStatusKey = $derived.by(() => {
    if (isRunning) return "playground.concurrency.runStatusRunning";
    if (activePlans.length === 0) {
      return visiblePlans.length > 0
        ? "playground.concurrency.runStatusReady"
        : "playground.concurrency.runStatusEmpty";
    }
    if (summary.failed + summary.cancelled > 0) return "playground.concurrency.runStatusAttention";
    return "playground.concurrency.runStatusComplete";
  });

  function clearResults(): void {
    activePlans = [];
    runs = {};
  }

  function newId(): string {
    if (typeof crypto !== "undefined" && "randomUUID" in crypto) return crypto.randomUUID();
    return `${Date.now()}-${Math.random().toString(36).slice(2)}`;
  }

  function addModel(modelId = selectedModel): void {
    if (isRunning || !modelId) return;
    clearResults();
    testListStore.update((list) => [...list, { id: newId(), model: modelId }]);
  }

  function duplicateEntry(id: string): void {
    if (isRunning) return;
    testListStore.update((list) => {
      const index = list.findIndex((entry) => entry.id === id);
      if (index < 0) return list;
      const next = [...list];
      next.splice(index + 1, 0, { id: newId(), model: list[index].model });
      return next;
    });
    clearResults();
  }

  function removeEntry(id: string): void {
    if (isRunning) return;
    testListStore.update((list) => list.filter((entry) => entry.id !== id));
    clearResults();
  }

  function clearQueue(): void {
    if (isRunning) return;
    testListStore.set([]);
    clearResults();
  }

  function onDragStart(index: number, event: DragEvent): void {
    if (isRunning) return;
    dragIndex = index;
    if (event.dataTransfer) {
      event.dataTransfer.effectAllowed = "move";
      event.dataTransfer.setData("text/plain", String(index));
    }
  }

  function onDragOver(index: number, event: DragEvent): void {
    if (isRunning || dragIndex === null) return;
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = "move";
    dragOverIndex = index;
  }

  function onDrop(index: number, event: DragEvent): void {
    if (isRunning || dragIndex === null) return;
    event.preventDefault();
    const from = dragIndex;
    dragIndex = null;
    dragOverIndex = null;
    if (from === index) return;
    testListStore.update((list) => {
      const next = [...list];
      const [moved] = next.splice(from, 1);
      next.splice(index, 0, moved);
      return next;
    });
    clearResults();
  }

  function onDragEnd(): void {
    dragIndex = null;
    dragOverIndex = null;
  }

  function changeMode(value: string | undefined): void {
    if (!value || isRunning || value === $modeStore) return;
    modeStore.set(value as BenchmarkMode);
    clearResults();
  }

  function emptyRun(plan?: Pick<BenchmarkPlan, "kind" | "estimatedPromptTokens">): RunState {
    return {
      status: "waiting",
      kind: plan?.kind ?? "request",
      loadingText: "",
      reasoningContent: "",
      content: "",
      loadingDone: false,
      waitingMs: 0,
      loadingMs: 0,
      reasoningMs: 0,
      contentMs: 0,
      phase: "waiting",
      elapsedMs: 0,
      firstTokenMs: null,
      promptTokens: plan?.estimatedPromptTokens ?? 0,
      outputTokens: 0,
    };
  }

  function ingestReasoning(
    prev: RunState,
    chunk: string,
  ): { loadingText: string; reasoningContent: string; loadingDone: boolean; nowPhase: BenchmarkPhase } {
    if (prev.loadingDone) {
      return {
        loadingText: prev.loadingText,
        reasoningContent: prev.reasoningContent + chunk,
        loadingDone: true,
        nowPhase: "reasoning",
      };
    }

    const combined = prev.loadingText + chunk;
    if (combined.length < LOAD_MARKER.length) {
      if (LOAD_MARKER.startsWith(combined)) {
        return { loadingText: combined, reasoningContent: prev.reasoningContent, loadingDone: false, nowPhase: "loading" };
      }
      return {
        loadingText: "",
        reasoningContent: prev.reasoningContent + combined,
        loadingDone: true,
        nowPhase: "reasoning",
      };
    }

    if (!combined.startsWith(LOAD_MARKER)) {
      return {
        loadingText: "",
        reasoningContent: prev.reasoningContent + combined,
        loadingDone: true,
        nowPhase: "reasoning",
      };
    }

    const closingIndex = combined.indexOf(LOAD_MARKER, LOAD_MARKER.length);
    if (closingIndex < 0) {
      return { loadingText: combined, reasoningContent: prev.reasoningContent, loadingDone: false, nowPhase: "loading" };
    }
    const newlineIndex = combined.indexOf("\n", closingIndex);
    const sliceEnd = newlineIndex >= 0 ? newlineIndex + 1 : combined.length;
    const loadingText = combined.substring(0, sliceEnd);
    const remainder = combined.substring(sliceEnd).replace(/^[ \t]*\n?/, "");
    return {
      loadingText,
      reasoningContent: prev.reasoningContent + remainder,
      loadingDone: true,
      nowPhase: remainder ? "reasoning" : "waiting",
    };
  }

  async function runOne(plan: BenchmarkPlan, signal: AbortSignal): Promise<void> {
    const start = performance.now();
    let phaseStart = start;
    runs[plan.id] = { ...emptyRun(plan), status: "streaming" };

    const accrue = (prev: RunState, now: number) => {
      const delta = now - phaseStart;
      const durations = {
        waitingMs: prev.waitingMs,
        loadingMs: prev.loadingMs,
        reasoningMs: prev.reasoningMs,
        contentMs: prev.contentMs,
      };
      if (prev.phase === "waiting") durations.waitingMs += delta;
      else if (prev.phase === "loading") durations.loadingMs += delta;
      else if (prev.phase === "reasoning") durations.reasoningMs += delta;
      else durations.contentMs += delta;
      return durations;
    };

    const ticker = window.setInterval(() => {
      const prev = runs[plan.id];
      if (!prev || prev.status !== "streaming") return;
      const now = performance.now();
      const durations = accrue(prev, now);
      phaseStart = now;
      runs[plan.id] = { ...prev, ...durations, elapsedMs: now - start };
    }, 50);

    try {
      const stream = streamChatCompletion(plan.model, [{ role: "user", content: plan.prompt }], signal, {
        endpoint: "v1/chat/completions",
        max_tokens: plan.maxTokens,
        include_usage: true,
      });
      for await (const chunk of stream) {
        const prev = runs[plan.id];
        if (!prev) break;
        const now = performance.now();
        const durations = accrue(prev, now);
        phaseStart = now;

        if (chunk.done) {
          runs[plan.id] = {
            ...prev,
            ...durations,
            usage: chunk.usage ?? prev.usage,
            promptTokens: chunk.usage?.prompt_tokens ?? prev.promptTokens,
            outputTokens: chunk.usage?.completion_tokens ?? prev.outputTokens,
            elapsedMs: now - start,
          };
          break;
        }

        let nextPhase = prev.phase;
        let loadingText = prev.loadingText;
        let reasoningContent = prev.reasoningContent;
        let loadingDone = prev.loadingDone;
        let receivedModelToken = false;

        if (chunk.reasoning_content) {
          const parsed = ingestReasoning(prev, chunk.reasoning_content);
          loadingText = parsed.loadingText;
          reasoningContent = parsed.reasoningContent;
          loadingDone = parsed.loadingDone;
          nextPhase = parsed.nowPhase;
          receivedModelToken = reasoningContent.length > prev.reasoningContent.length;
        }
        if (chunk.content) {
          nextPhase = "content";
          receivedModelToken = true;
        }

        const usage = chunk.usage ?? prev.usage;
        const content = prev.content + (chunk.content ?? "");
        runs[plan.id] = {
          ...prev,
          ...durations,
          loadingText,
          reasoningContent,
          content,
          loadingDone,
          phase: nextPhase,
          firstTokenMs: prev.firstTokenMs ?? (receivedModelToken ? now - start : null),
          promptTokens: usage?.prompt_tokens ?? prev.promptTokens,
          outputTokens: usage?.completion_tokens ?? estimateTokens(reasoningContent + content),
          usage,
          elapsedMs: now - start,
        };
      }

      const prev = runs[plan.id];
      if (prev) {
        const now = performance.now();
        const durations = accrue(prev, now);
        runs[plan.id] = { ...prev, ...durations, status: "done", elapsedMs: now - start };
      }
    } catch (error) {
      const prev = runs[plan.id] ?? emptyRun(plan);
      const now = performance.now();
      const durations = accrue(prev, now);
      const aborted = error instanceof Error && error.name === "AbortError";
      runs[plan.id] = {
        ...prev,
        ...durations,
        status: aborted ? "cancelled" : "error",
        elapsedMs: now - start,
        error: aborted ? t("status.request.cancelled") : error instanceof Error ? error.message : String(error),
      };
    } finally {
      window.clearInterval(ticker);
    }
  }

  function makePlans(): BenchmarkPlan[] {
    return createBenchmarkPlans({
      mode: $modeStore,
      queue: $testListStore,
      prompt: $promptStore,
      maxTokens: $maxTokensStore,
      contextTokens: $modeStore === "async-prefill" ? $prefillTokensStore : $contextTokensStore,
      prefillRequests: $prefillRequestsStore,
    });
  }

  function parallelLimit(count: number): number {
    const requested = Number($parallelismStore);
    if (!Number.isFinite(requested) || requested <= 0) return count;
    return Math.max(1, Math.min(count, Math.trunc(requested)));
  }

  function markCancelled(plan: BenchmarkPlan): void {
    runs[plan.id] = {
      ...emptyRun(plan),
      status: "cancelled",
      error: t("status.request.cancelled"),
    };
  }

  async function run(): Promise<void> {
    if (!canRun) return;
    const plans = makePlans();
    if (plans.length === 0) return;

    activePlans = plans;
    runs = Object.fromEntries(plans.map((plan) => [plan.id, emptyRun(plan)]));
    isRunning = true;
    abortController = new AbortController();
    const signal = abortController.signal;

    try {
      if ($modeStore !== "async-prefill") {
        await runWithConcurrency(plans, parallelLimit(plans.length), (plan) => runOne(plan, signal));
        return;
      }

      const prefillPlans = plans.filter((plan) => plan.kind === "prefill");
      const probePlans = plans.filter((plan) => plan.kind === "probe");
      const prefillWork = runWithConcurrency(
        prefillPlans,
        parallelLimit(prefillPlans.length),
        (plan) => runOne(plan, signal),
      );
      let delayError: unknown = null;
      let probeWork: Promise<void> = Promise.resolve();
      try {
        const delay = Number.isFinite(Number($probeDelayStore))
          ? Math.max(0, Math.trunc(Number($probeDelayStore)))
          : 0;
        await delayWithAbort(delay, signal);
        probeWork = runWithConcurrency(probePlans, probePlans.length, (plan) => runOne(plan, signal));
      } catch (error) {
        if (error instanceof Error && error.name === "AbortError") {
          for (const plan of probePlans) markCancelled(plan);
        } else {
          delayError = error;
        }
      }
      await Promise.allSettled([prefillWork, probeWork]);
      if (delayError) throw delayError;
    } finally {
      isRunning = false;
      abortController = null;
    }
  }

  function stop(): void {
    abortController?.abort();
  }

  function resetDefaults(): void {
    promptStore.set(DEFAULT_PROMPT);
    maxTokensStore.set(DEFAULT_MAX_TOKENS);
    contextTokensStore.set(DEFAULT_CONTEXT_TOKENS);
    prefillTokensStore.set(DEFAULT_CONTEXT_TOKENS);
    prefillRequestsStore.set(DEFAULT_PREFILL_REQUESTS);
    parallelismStore.set(0);
    probeDelayStore.set(100);
    clearResults();
  }

  function formatTokens(tokens: number): string {
    return Math.max(0, Math.round(tokens)).toLocaleString();
  }

  function formatElapsed(ms: number | null): string {
    if (ms === null) return "—";
    if (ms < 1_000) return `${Math.round(ms)}ms`;
    return `${(ms / 1_000).toFixed(2)}s`;
  }
</script>

<div class="h-full min-h-0 overflow-y-auto lg:grid lg:grid-cols-[21rem_minmax(0,1fr)] lg:overflow-hidden">
  <aside class="flex flex-col border-b border-border lg:min-h-0 lg:overflow-y-auto lg:border-r lg:border-b-0" aria-label={$translate("playground.concurrency.testPlan")}>
    <section class="order-1 border-b border-border p-3">
      <div class="mb-2 flex items-center justify-between">
        <h2 class="text-xs font-semibold">{$translate("playground.concurrency.scenario")}</h2>
        <span class="text-muted-foreground text-[10px] tabular-nums">
          {$translate("playground.concurrency.requestCount", { count: previewPlans.length })}
        </span>
      </div>
      <ToggleGroup.Root
        type="single"
        variant="outline"
        size="sm"
        value={$modeStore}
        onValueChange={changeMode}
        class="grid w-full grid-cols-3"
        aria-label={$translate("playground.concurrency.testType")}
      >
        <ToggleGroup.Item
          value="concurrency"
          class="min-w-0 gap-1 px-1 text-[10px]"
          disabled={isRunning}
          title={$translate("playground.concurrency.modeDescription.concurrency")}
        >
          <Users class="size-3" /><span class="truncate">{$translate("playground.concurrency.modeConcurrency")}</span>
        </ToggleGroup.Item>
        <ToggleGroup.Item
          value="long-context"
          class="min-w-0 gap-1 px-1 text-[10px]"
          disabled={isRunning}
          title={$translate("playground.concurrency.modeDescription.long-context")}
        >
          <FileText class="size-3" /><span class="truncate">{$translate("playground.concurrency.modeLongContext")}</span>
        </ToggleGroup.Item>
        <ToggleGroup.Item
          value="async-prefill"
          class="min-w-0 gap-1 px-1 text-[10px]"
          disabled={isRunning}
          title={$translate("playground.concurrency.modeDescription.async-prefill")}
        >
          <Activity class="size-3" /><span class="truncate">{$translate("playground.concurrency.modeAsyncPrefill")}</span>
        </ToggleGroup.Item>
      </ToggleGroup.Root>
    </section>

    <section class="order-3 p-3">
      <div class="mb-2 flex items-center gap-2">
        <h2 class="text-xs font-semibold">{$translate("playground.concurrency.settings")}</h2>
        <Button
          variant="ghost"
          size="icon-xs"
          class="ml-auto"
          onclick={resetDefaults}
          disabled={isRunning}
          aria-label={$translate("playground.concurrency.resetDefaults")}
          title={$translate("playground.concurrency.resetDefaults")}
        >
          <RotateCcw />
        </Button>
      </div>

      <label for="benchmark-prompt" class="text-muted-foreground text-[10px] font-medium uppercase">
        {$translate($modeStore === "concurrency" ? "playground.concurrency.prompt" : "playground.concurrency.query")}
      </label>
      <Textarea
        id="benchmark-prompt"
        class="mt-1 resize-none text-xs leading-4"
        rows={3}
        bind:value={$promptStore}
        oninput={clearResults}
        disabled={isRunning}
      ></Textarea>

      <div class="mt-3 grid grid-cols-2 gap-2">
        {#if $modeStore === "long-context"}
          <div class="col-span-2">
            <label for="benchmark-context-tokens" class="text-muted-foreground text-[10px] font-medium">
              {$translate("playground.concurrency.contextTokens")}
            </label>
            <Input
              id="benchmark-context-tokens"
              type="number"
              min="128"
              max="1000000"
              step="128"
              class="mt-1 h-8 text-xs tabular-nums"
              bind:value={$contextTokensStore}
              onchange={clearResults}
              disabled={isRunning}
            />
          </div>
        {:else if $modeStore === "async-prefill"}
          <div class="col-span-2">
            <label for="benchmark-prefill-tokens" class="text-muted-foreground text-[10px] font-medium">
              {$translate("playground.concurrency.prefillTokens")}
            </label>
            <Input
              id="benchmark-prefill-tokens"
              type="number"
              min="128"
              max="1000000"
              step="128"
              class="mt-1 h-8 text-xs tabular-nums"
              bind:value={$prefillTokensStore}
              onchange={clearResults}
              disabled={isRunning}
            />
          </div>
          <div>
            <label for="benchmark-prefill-requests" class="text-muted-foreground text-[10px] font-medium">
              {$translate("playground.concurrency.prefillRequests")}
            </label>
            <Input
              id="benchmark-prefill-requests"
              type="number"
              min="1"
              max="32"
              class="mt-1 h-8 text-xs tabular-nums"
              bind:value={$prefillRequestsStore}
              onchange={clearResults}
              disabled={isRunning}
            />
          </div>
          <div>
            <label for="benchmark-probe-delay" class="text-muted-foreground text-[10px] font-medium">
              {$translate("playground.concurrency.probeDelay")}
            </label>
            <Input
              id="benchmark-probe-delay"
              type="number"
              min="0"
              max="10000"
              step="10"
              class="mt-1 h-8 text-xs tabular-nums"
              bind:value={$probeDelayStore}
              onchange={clearResults}
              disabled={isRunning}
            />
          </div>
        {/if}

        <div>
          <label for="benchmark-parallelism" class="text-muted-foreground text-[10px] font-medium">
            {$translate("playground.concurrency.parallelism")}
          </label>
          <Input
            id="benchmark-parallelism"
            type="number"
            min="0"
            max="128"
            class="mt-1 h-8 text-xs tabular-nums"
            bind:value={$parallelismStore}
            onchange={clearResults}
            disabled={isRunning}
          />
        </div>
        <div>
          <label for="benchmark-max-tokens" class="text-muted-foreground text-[10px] font-medium">
            {$translate("playground.concurrency.maxTokens")}
          </label>
          <Input
            id="benchmark-max-tokens"
            type="number"
            min="1"
            max="65536"
            class="mt-1 h-8 text-xs tabular-nums"
            bind:value={$maxTokensStore}
            onchange={clearResults}
            disabled={isRunning}
          />
        </div>
      </div>
    </section>

    <section class="order-2 border-b border-border p-3">
      <div class="mb-2 flex items-center gap-2">
        <h2 class="text-xs font-semibold">{$translate("playground.concurrency.queue")}</h2>
        <span class="bg-muted text-muted-foreground px-1.5 py-0.5 text-[10px] tabular-nums">{$testListStore.length}</span>
        <Button
          variant="ghost"
          size="icon-xs"
          class="ml-auto"
          onclick={clearQueue}
          disabled={isRunning || $testListStore.length === 0}
          aria-label={$translate("playground.concurrency.clearQueue")}
          title={$translate("playground.concurrency.clearQueue")}
        >
          <Trash2 />
        </Button>
      </div>

      {#if hasModels}
        <div class="flex gap-1.5">
          <ModelSelector
            bind:value={selectedModel}
            placeholder={$translate("playground.concurrency.chooseModel")}
            disabled={isRunning}
            category="chat"
          />
          <Button
            size="icon"
            onclick={() => addModel()}
            disabled={isRunning || !selectedModel}
            aria-label={$translate("playground.concurrency.addSelected")}
            title={$translate("playground.concurrency.addSelected")}
          >
            <Plus />
          </Button>
        </div>
      {:else}
        <div class="text-muted-foreground border-y border-border py-4 text-center text-xs">
          {$translate("playground.concurrency.noModels")}
        </div>
      {/if}

      {#if $testListStore.length === 0}
        <div class="text-muted-foreground mt-2 border-y border-dashed border-border py-4 text-center text-xs">
          {$translate("playground.concurrency.queueEmpty")}
        </div>
      {:else}
        <div class="mt-2 max-h-44 overflow-y-auto border-y border-border" role="list">
          {#each $testListStore as entry, index (entry.id)}
            <div
              class="flex items-center gap-1 border-b border-border px-1 py-1 last:border-b-0 {dragOverIndex === index && dragIndex !== index ? 'bg-primary/10' : ''} {dragIndex === index ? 'opacity-50' : ''}"
              draggable={!isRunning}
              ondragstart={(event) => onDragStart(index, event)}
              ondragover={(event) => onDragOver(index, event)}
              ondrop={(event) => onDrop(index, event)}
              ondragend={onDragEnd}
              role="listitem"
            >
              <GripVertical class="text-muted-foreground size-3.5 shrink-0 cursor-grab" aria-hidden="true" />
              <span class="text-muted-foreground w-4 shrink-0 text-right text-[10px] tabular-nums">{index + 1}</span>
              <span class="min-w-0 flex-1 truncate text-xs" title={entry.model}>{entry.model}</span>
              <Button
                variant="ghost"
                size="icon-xs"
                onclick={() => duplicateEntry(entry.id)}
                disabled={isRunning}
                aria-label={$translate("playground.concurrency.duplicate", { model: entry.model })}
                title={$translate("playground.concurrency.duplicate", { model: entry.model })}
              >
                <CopyPlus />
              </Button>
              <Button
                variant="ghost"
                size="icon-xs"
                class="text-muted-foreground hover:text-destructive"
                onclick={() => removeEntry(entry.id)}
                disabled={isRunning}
                aria-label={$translate("playground.concurrency.remove", { model: entry.model })}
                title={$translate("playground.concurrency.remove", { model: entry.model })}
              >
                <Trash2 />
              </Button>
            </div>
          {/each}
        </div>
      {/if}
    </section>
  </aside>

  <section class="flex min-h-0 flex-col" aria-label={$translate("playground.concurrency.results")}>
    <header class="flex shrink-0 flex-wrap items-center gap-2 border-b border-border px-3 py-2.5">
      <div class="min-w-0 flex-1">
        <div class="flex items-center gap-2">
          <Gauge class="text-primary size-4 shrink-0" />
          <h2 class="truncate text-sm font-semibold">
            {$translate(`playground.concurrency.mode${$modeStore === "concurrency" ? "Concurrency" : $modeStore === "long-context" ? "LongContext" : "AsyncPrefill"}`)}
          </h2>
          <span class="text-muted-foreground flex shrink-0 items-center gap-1.5 text-[10px]" aria-live="polite">
            <span class="size-1.5 rounded-full {isRunning ? 'bg-warning animate-pulse' : activePlans.length > 0 && summary.failed + summary.cancelled === 0 ? 'bg-success' : activePlans.length > 0 ? 'bg-destructive' : 'bg-border'}"></span>
            {$translate(runStatusKey)}
          </span>
        </div>
        <div class="text-muted-foreground mt-0.5 flex flex-wrap gap-x-3 text-[10px] tabular-nums">
          <span>{$translate("playground.concurrency.requestCount", { count: visiblePlans.length })}</span>
          <span>{$translate("playground.concurrency.parallelCount", { count: plannedParallelism })}</span>
        </div>
      </div>

      {#if activePlans.length > 0}
        <Button
          variant="ghost"
          size="icon-sm"
          onclick={clearResults}
          disabled={isRunning}
          aria-label={$translate("playground.concurrency.clearResults")}
          title={$translate("playground.concurrency.clearResults")}
        >
          <Eraser />
        </Button>
      {/if}
      {#if isRunning}
        <Button variant="destructive" onclick={stop}>
          <Square data-icon="inline-start" />{$translate("playground.concurrency.stop")}
        </Button>
      {:else}
        <Button
          onclick={run}
          disabled={!canRun}
          title={$testListStore.length === 0 ? $translate("playground.concurrency.addModelsHint") : $translate("playground.concurrency.runHint")}
        >
          <Play data-icon="inline-start" />{$translate("playground.concurrency.go")}
        </Button>
      {/if}
    </header>

    <div class="grid shrink-0 grid-cols-2 gap-px border-b border-border bg-border sm:grid-cols-3 xl:grid-cols-6">
      <div class="bg-background px-3 py-2">
        <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.completed")}</div>
        <div class="mt-0.5 text-sm font-semibold tabular-nums">{summary.completed}/{summary.total}</div>
      </div>
      <div class="bg-background px-3 py-2">
        <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.promptLoad")}</div>
        <div class="mt-0.5 text-sm font-semibold tabular-nums">{formatTokens(summary.totalPromptTokens)}</div>
      </div>
      <div class="bg-background px-3 py-2">
        <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.p50FirstToken")}</div>
        <div class="mt-0.5 text-sm font-semibold tabular-nums">{formatElapsed(summary.p50FirstTokenMs)}</div>
      </div>
      <div class="bg-background px-3 py-2">
        <div class="text-muted-foreground text-[9px] uppercase">
          {$translate($modeStore === "async-prefill" ? "playground.concurrency.probeFirstToken" : "playground.concurrency.p95FirstToken")}
        </div>
        <div class="mt-0.5 text-sm font-semibold tabular-nums">
          {formatElapsed($modeStore === "async-prefill" ? probeSummary.p50FirstTokenMs : summary.p95FirstTokenMs)}
        </div>
      </div>
      <div class="bg-background px-3 py-2">
        <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.outputRate")}</div>
        <div class="mt-0.5 text-sm font-semibold tabular-nums">
          {activePlans.length > 0 ? `${summary.outputTokensPerSecond.toFixed(1)} t/s` : "—"}
        </div>
      </div>
      <div class="bg-background px-3 py-2">
        <div class="text-muted-foreground text-[9px] uppercase">{$translate("playground.concurrency.wallTime")}</div>
        <div class="mt-0.5 text-sm font-semibold tabular-nums">{activePlans.length > 0 ? formatElapsed(summary.wallTimeMs) : "—"}</div>
      </div>
      <div class="col-span-full h-0.5 bg-muted">
        <div class="bg-primary h-full transition-[width] duration-200" style:width={`${progressPercent}%`}></div>
      </div>
    </div>

    <div class="min-h-0 flex-1 overflow-y-auto">
      {#if visiblePlans.length === 0}
        <div class="text-muted-foreground flex min-h-56 flex-col items-center justify-center gap-2 px-6 text-center">
          <Gauge class="size-6 opacity-50" />
          <p class="text-xs">{$translate("playground.concurrency.noPlan")}</p>
        </div>
      {:else}
        {#if activePlans.length > 0}
          <BenchmarkTimeline plans={visiblePlans} runs={runs} bind:collapsed={$timelineCollapsedStore} />
        {/if}
        <BenchmarkRunLedger plans={visiblePlans} runs={runs} />
      {/if}
    </div>
  </section>
</div>
