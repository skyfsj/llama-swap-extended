<script lang="ts">
  import { onDestroy, onMount } from "svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Tabs from "$lib/components/ui/tabs/index.js";
  import { Play, Square, Download, Send, Trash2 } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  import { models, playgroundModels, fetchPlaygroundModels } from "../../stores/api";
  import { testExtension, type ExtensionTestResult } from "../../lib/extensionsApi";
  import { buildExtensionRequest, chooseExtensionEndpoint, changedRequestKeys, summarizeRuns, type RequestEndpoint, type RunSample } from "../../lib/extensionWorkbench";
  import { runDebugTurn, type DebugArtifact, type DebugChatStep } from "../../lib/extensionDebugChat";
  import ExtensionValueEditor from "./ExtensionValueEditor.svelte";

  let { id, mode, onRunning, compact = false, preferredEndpoints = [], draftFiles = null }: { id: string; mode: "debug" | "benchmark"; onRunning: (running: boolean) => void; compact?: boolean; preferredEndpoints?: string[]; draftFiles?: Record<string, string> | null } = $props();
  let model = $state("");
  let customModel = $state(false);
  let customModelId = $state("");
  let loadingModels = $state(false);
  let endpoint = $state<RequestEndpoint>("chat.completions");
  let endpointChanged = $state(false);
  const modelOptions = $derived([...new Map([...$playgroundModels, ...$models].map((item) => [item.id, item])).values()]);
  const requestModel = $derived(customModel ? customModelId : model);
  $effect(() => {
    if (!customModel && !running && !chatRunning && !modelOptions.some((item) => item.id === model && !item.disabled && !item.maintenance)) {
      model = modelOptions.find((item) => !item.disabled && !item.maintenance)?.id ?? "";
    }
  });
  $effect(() => {
    if (!endpointChanged && customRequest === null && !running) endpoint = chooseExtensionEndpoint(preferredEndpoints);
  });
  async function loadModels() {
    loadingModels = true;
    try { await fetchPlaygroundModels(); } finally { loadingModels = false; }
  }
  onMount(() => { void loadModels(); });
  let prompt = $state("Hello");
  let system = $state("");
  let temperature = $state(0.7);
  let maxTokens = $state(256);
  let profile = $state("");
  let provider = $state("");
  let stream = $state(false);
  let customRequest = $state<Record<string, unknown> | null>(null);
  const customDrafts = new Map<RequestEndpoint, Record<string, unknown>>();
  let runs = $state(10);
  let running = $state(false);
  let stopped = $state(false);
  let result = $state<ExtensionTestResult | null>(null);
  let samples = $state<RunSample[]>([]);
  let error = $state("");
  let startedAt = $state("");
  let timingAvailable = $state(true);
  let resultTab = $state("changes");
  let controller: AbortController | null = null;
  let lastRequest: Record<string, unknown> = {};
  let lastRun: { extension: string; endpoint: RequestEndpoint; context: { profile: string; provider: string; stream: boolean }; mode: "debug" | "benchmark"; requestedRuns: number } | null = null;
  const request = $derived(customRequest ?? buildExtensionRequest(endpoint, { model: requestModel, prompt, system, temperature, maxTokens }));
  const summary = $derived(summarizeRuns(samples));
  const changes = $derived(result ? changedRequestKeys(result.requestBefore, result.requestAfter) : []);
  const valid = $derived(typeof request.model === "string" && request.model.trim().length > 0 && (customRequest !== null || (Number.isFinite(temperature) && Number.isInteger(maxTokens) && maxTokens > 0)) && (mode !== "benchmark" || (Number.isInteger(runs) && runs >= 1 && runs <= 100)));
  const pretty = (value: unknown) => JSON.stringify(value, null, 2) ?? "—";
  const duration = (value: number | null) => value === null ? "—" : `${value.toFixed(1)} ms`;
  function toggleRequest() {
    if (customRequest !== null) {
      customDrafts.set(endpoint, customRequest);
      customRequest = null;
    } else {
      customRequest = customDrafts.get(endpoint) ?? JSON.parse(JSON.stringify(request));
    }
  }
  function stop() { stopped = true; controller?.abort(); }
  async function run() {
    if (running || !valid) return;
    const count = mode === "benchmark" ? runs : 1;
    lastRequest = JSON.parse(JSON.stringify(request));
    const options = { profile, provider, stream };
    const selectedEndpoint = endpoint;
    const selectedId = id;
    lastRun = { extension: selectedId, endpoint: selectedEndpoint, context: options, mode, requestedRuns: count };
    running = true; onRunning(true); stopped = false; samples = []; result = null; timingAvailable = true; error = ""; startedAt = new Date().toISOString();
    controller = new AbortController();
    try {
      for (let index = 0; index < count && !stopped; index++) {
        const start = performance.now();
        try {
          const response = await testExtension(selectedId, lastRequest, selectedEndpoint, { ...options, signal: controller.signal, files: draftFiles ?? undefined });
          if (stopped) break;
          result = response;
          const measuredDuration = typeof response.durationMs === "number" && Number.isFinite(response.durationMs) && response.durationMs >= 0 ? response.durationMs : null;
          if (response.matched && !response.error && measuredDuration === null) timingAvailable = false;
          samples = [...samples, { duration: measuredDuration, roundTrip: performance.now() - start, outcome: response.error ? "error" : response.matched ? "success" : "unmatched", error: response.error }];
          if (response.error) error = response.error;
          if (!response.matched) break;
        } catch (cause) {
          if (stopped) break;
          error = String(cause);
          samples = [...samples, { duration: null, roundTrip: performance.now() - start, outcome: "error", error }];
          break;
        }
      }
    } finally { running = false; controller = null; onRunning(false); }
  }
  function download() {
    const blob = new Blob([pretty({ startedAt, ...lastRun, stopped, request: lastRequest, samples, summary, result })], { type: "application/json" });
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a"); anchor.href = url; anchor.download = `${lastRun?.extension ?? id}-test.json`; anchor.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  }
  onDestroy(() => { stopped = true; controller?.abort(); chatController?.abort(); });

  // ===== 多轮调试对话 =====
  // The chat view posts the whole conversation to the debug chat endpoint each
  // turn; the pipeline runs only this extension and captures its activity in a
  // side-channel artifact that attaches to the assistant entry when done.
  let debugView = $state<"single" | "chat">("single");
  interface ChatEntry {
    role: "user" | "assistant";
    text: string;
    reasoning?: string;
    streaming?: boolean;
    artifact?: DebugArtifact | null;
    notice?: string;
  }
  let chat = $state<ChatEntry[]>([]);
  let chatInput = $state("");
  let chatRunning = $state(false);
  let chatError = $state("");
  let chatController: AbortController | null = null;
  let chatScroll = $state<HTMLDivElement | undefined>(undefined);
  const chatHistory = $derived(
    chat
      .filter((entry) => entry.role === "user" || entry.text !== "")
      .map((entry) => ({ role: entry.role, content: entry.text })),
  );
  const chatValid = $derived(typeof requestModel === "string" && requestModel.trim().length > 0);
  $effect(() => {
    if (chatScroll) chatScroll.scrollTop = chatScroll.scrollHeight;
  });
  function clearChat() {
    if (chatRunning) return;
    chat = [];
    chatError = "";
  }
  async function sendChat() {
    const text = chatInput.trim();
    if (chatRunning || !chatValid || !text) return;
    chatInput = "";
    chatError = "";
    chat = [...chat, { role: "user", text }];
    const history = [...chatHistory];
    const base: Record<string, unknown> = { model: requestModel, messages: system.trim() ? [{ role: "system", content: system.trim() }, ...history] : history, stream: true };
    if (customRequest === null) {
      if (Number.isFinite(temperature)) base.temperature = temperature;
      if (Number.isInteger(maxTokens) && maxTokens > 0) base.max_tokens = maxTokens;
    }
    chat = [...chat, { role: "assistant", text: "", reasoning: "", streaming: true, artifact: null, notice: "" }];
    const entryIndex = chat.length - 1;
    chatRunning = true; onRunning(true);
    chatController = new AbortController();
    try {
      const turn = await runDebugTurn({
        id,
        request: base,
        files: draftFiles ?? undefined,
        signal: chatController.signal,
        onDelta: (delta) => { chat[entryIndex].text += delta; },
        onReasoning: (delta) => { chat[entryIndex].reasoning += delta; },
      });
      chat[entryIndex].artifact = turn.artifact;
      chat[entryIndex].notice = turn.clientToolCalls.length ? $translate("extensions.dev.chatToolNotice") : "";
      chat[entryIndex].streaming = false;
      if (!chat[entryIndex].text && !turn.clientToolCalls.length) chat[entryIndex].text = $translate("extensions.dev.chatEmptyReply");
    } catch (cause) {
      chat[entryIndex].streaming = false;
      if (!chat[entryIndex].text) chat = chat.filter((_, index) => index !== entryIndex);
      chatError = String(cause);
    } finally {
      chatRunning = false; chatController = null; onRunning(false);
    }
  }
  function chatKeydown(event: KeyboardEvent) {
    if (event.key === "Enter" && !event.shiftKey && !event.isComposing) {
      event.preventDefault();
      void sendChat();
    }
  }
  function stepLabel(step: DebugChatStep): string {
    if (step.kind === "tools") return $translate("extensions.dev.chatInjected");
    if (step.kind === "hostcall") return $translate("extensions.dev.chatHostCall");
    if (step.hook === "onToolCall" || step.hook === "onToolResult") return $translate("extensions.dev.chatToolCall");
    return step.hook ?? "";
  }
</script>
{#snippet modelField()}
  <label class="min-w-40 flex-1">{$translate("extensions.testModel")}
    {#if customModel}<input bind:value={customModelId} disabled={customRequest !== null} placeholder={$translate("extensions.testModelPlaceholder")} />
    {:else}<select bind:value={model} disabled={customRequest !== null || loadingModels}>
      {#if !model}<option value="" disabled>{$translate(loadingModels ? "extensions.dev.loadingModels" : "extensions.dev.noModels")}</option>{/if}
      {#each modelOptions as item}<option value={item.id} disabled={item.disabled || item.maintenance}>{item.name && item.name !== item.id ? `${item.name} · ${item.id}` : item.id}</option>{/each}
    </select>{/if}
  </label>
{/snippet}
{#snippet modelLinks()}
  <div class="flex gap-3"><button type="button" class="text-xs text-primary hover:underline disabled:opacity-50" disabled={customRequest !== null} onclick={() => customModel = !customModel}>{$translate(customModel ? "extensions.dev.listedModels" : "extensions.dev.customModel")}</button><button type="button" class="text-xs text-muted-foreground hover:underline disabled:opacity-50" disabled={loadingModels} onclick={loadModels}>{$translate("extensions.dev.refreshModels")}</button></div>
{/snippet}
{#snippet modelInput()}
  <div class="flex-1 space-y-1.5">{@render modelField()}{@render modelLinks()}</div>
{/snippet}
{#snippet endpointInput()}
  <label>{$translate("extensions.dev.simulatedEndpoint")}<select bind:value={endpoint} onchange={() => endpointChanged = true} disabled={customRequest !== null}><option value="chat.completions">Chat Completions</option><option value="responses">Responses</option><option value="anthropic.messages">Anthropic Messages</option></select><span class="text-xs leading-relaxed text-muted-foreground">{$translate("extensions.dev.simulatedEndpointHint")}</span></label>
{/snippet}
<div class="workbench min-h-0 flex-1 overflow-y-auto p-4" class:compact>
  {#if mode === "debug"}
    <div class="mb-3 flex min-w-0 flex-wrap items-center gap-2">
      <div class="flex shrink-0 overflow-hidden rounded-md border border-border text-xs" role="group" aria-label={$translate("extensions.dev.debugView")}>
        <button type="button" class={`px-2.5 py-1 hover:bg-muted ${debugView === "single" ? "bg-primary/10 font-semibold text-primary" : ""}`} aria-pressed={debugView === "single"} onclick={() => debugView = "single"}>{$translate("extensions.dev.debugViewSingle")}</button>
        <button type="button" class={`px-2.5 py-1 hover:bg-muted ${debugView === "chat" ? "bg-primary/10 font-semibold text-primary" : ""}`} aria-pressed={debugView === "chat"} onclick={() => debugView = "chat"}>{$translate("extensions.dev.debugViewChat")}</button>
      </div>
      {#if debugView === "chat"}<span class="min-w-0 flex-1 truncate text-xs text-muted-foreground" title={$translate("extensions.dev.chatHint")}>{$translate("extensions.dev.chatHint")}</span>{/if}
    </div>
  {/if}
  {#if mode === "debug" && debugView === "chat"}
    <div class="flex min-h-[280px] flex-col" class:h-full={!compact}>
      <div class="mb-2 flex flex-wrap items-end gap-2">
        {@render modelField()}
        <Button variant="ghost" size="xs" class="align-with-control" disabled={chatRunning || !chat.length} onclick={clearChat}><Trash2 class="size-3.5" />{$translate("extensions.dev.chatClear")}</Button>
      </div>
      <div bind:this={chatScroll} class="chat-scroll min-h-0 flex-1 space-y-3 overflow-y-auto pr-1">
        {#if chat.length === 0}<div class="flex h-full min-h-32 items-center justify-center rounded-lg border border-dashed border-border px-6 py-10 text-center text-sm text-muted-foreground">{$translate("extensions.dev.chatEmpty")}</div>{/if}
        {#each chat as entry, index (index)}
          {#if entry.role === "user"}
            <div class="flex justify-end"><div class="max-w-[85%] whitespace-pre-wrap break-words rounded-lg bg-primary/10 px-3 py-2 text-sm">{entry.text}</div></div>
          {:else}
            <div class="max-w-[94%] space-y-1.5">
              {#if entry.reasoning}<details class="rounded-md border border-border bg-muted/30 px-3 py-1.5 text-xs text-muted-foreground"><summary class="cursor-pointer">{$translate("extensions.dev.chatReasoning")}</summary><p class="mt-1 whitespace-pre-wrap break-words leading-relaxed">{entry.reasoning}</p></details>{/if}
              <div class="whitespace-pre-wrap break-words rounded-lg border border-border bg-background px-3 py-2 text-sm">{entry.text}{#if entry.streaming}<span class="chat-caret" aria-hidden="true"></span>{/if}</div>
              {#if entry.notice}<p class="text-xs text-warning">{entry.notice}</p>{/if}
              {#if entry.artifact && entry.artifact.steps.length}
                <details class="rounded-md border border-border bg-muted/20 px-3 py-1.5 text-xs">
                  <summary class="cursor-pointer text-muted-foreground">{$translate("extensions.dev.chatArtifact")} · {entry.artifact.steps.length}{#if entry.artifact.truncated} +{/if}</summary>
                  <ul class="mt-2 space-y-1.5">
                    {#each entry.artifact.steps as step (step)}
                      <li class="space-y-1">
                        <p class="flex flex-wrap items-center gap-2"><span class="font-medium">{stepLabel(step)}</span>{#if step.kind === "hook" && typeof step.durationMs === "number"}<span class="text-muted-foreground">{duration(step.durationMs)}</span>{/if}{#if step.injected?.length}<span class="font-mono text-muted-foreground">{step.injected.join(", ")}</span>{/if}{#if step.error}<span class="text-destructive">{step.error}</span>{/if}</p>
                        {#if step.changed?.length}
                          <div class="space-y-1">{#each step.changed as change (change.key)}<div class="rounded border border-border bg-background p-2"><p class="font-mono text-[11px] text-muted-foreground">{change.key}</p><div class="mt-1 grid gap-1 md:grid-cols-2"><pre class="overflow-auto whitespace-pre-wrap break-all text-[11px] text-muted-foreground">{pretty(change.before)}</pre><pre class="overflow-auto whitespace-pre-wrap break-all text-[11px]">{pretty(change.after)}</pre></div></div>{/each}</div>
                        {/if}
                        {#if step.input !== undefined}<pre class="overflow-auto whitespace-pre-wrap break-all rounded border border-border bg-background p-2 text-[11px]">{pretty(step.input)}</pre>{/if}
                        {#if step.output !== undefined}<pre class="overflow-auto whitespace-pre-wrap break-all rounded border border-border bg-background p-2 text-[11px]">{pretty(step.output)}</pre>{/if}
                        {#if step.logs?.length}<pre class="overflow-auto whitespace-pre-wrap break-all rounded bg-background p-2 font-mono text-[11px] text-muted-foreground">{step.logs.join("\n")}</pre>{/if}
                      </li>
                    {/each}
                  </ul>
                </details>
              {/if}
            </div>
          {/if}
        {/each}
      </div>
      {#if chatError}<p role="alert" class="mt-2 break-words rounded-md bg-destructive/10 p-2 text-xs text-destructive">{chatError}</p>{/if}
      <div class="mt-2 flex shrink-0 items-end gap-2 border-t border-border pt-2">
        <textarea class="chat-input min-h-9 flex-1 resize-y" rows="2" bind:value={chatInput} onkeydown={chatKeydown} disabled={chatRunning} placeholder={$translate("extensions.dev.chatPlaceholder")} aria-label={$translate("extensions.dev.chatPlaceholder")}></textarea>
        {#if chatRunning}<Button variant="destructive" size="sm" onclick={() => chatController?.abort()}><Square class="size-3.5" />{$translate("extensions.dev.stop")}</Button>
        {:else}<Button size="sm" disabled={!chatValid || !chatInput.trim()} onclick={() => void sendChat()}><Send class="size-3.5" />{$translate("extensions.dev.chatSend")}</Button>{/if}
      </div>
    </div>
  {:else}
  <div class:hidden={compact} class="mb-5 flex flex-wrap items-start justify-between gap-3"><div><h2 class="font-semibold">{$translate(`extensions.dev.${mode}`)}</h2><p class="mt-1 max-w-2xl text-xs leading-relaxed text-muted-foreground">{$translate("extensions.dev.scope")}</p></div><div class="flex gap-2">{#if samples.length}<Button variant="outline" size="sm" onclick={download}><Download class="size-4" />{$translate("extensions.dev.export")}</Button>{/if}{#if running}<Button variant="destructive" onclick={stop}><Square class="size-4" />{$translate("extensions.dev.stop")}</Button>{:else}<Button disabled={!valid} onclick={run}><Play class="size-4" />{$translate(mode === "benchmark" ? "extensions.dev.startBenchmark" : "extensions.runTest")}</Button>{/if}</div></div>
  {#if compact}
    <fieldset disabled={running} class="compact-request mb-3">
      <div class="flex flex-wrap items-end gap-3">
        {@render modelField()}
        {#if mode === "debug"}<label class="min-w-28 flex-1">{$translate("extensions.dev.prompt")}<input bind:value={prompt} disabled={customRequest !== null} /></label>{:else}<label class="w-28">{$translate("extensions.dev.runs")}<input type="number" min="1" max="100" bind:value={runs} /></label>{/if}
        <Button class="align-with-control" disabled={!valid} onclick={run}><Play class="size-4" />{$translate(mode === "benchmark" ? "extensions.dev.startBenchmark" : "extensions.runTest")}</Button>
      </div>
      <div class="pt-1.5">{@render modelLinks()}</div>
    </fieldset>
    <div class="mb-3 flex items-center gap-2 text-xs text-muted-foreground"><span>{$translate("extensions.dev.extensionOnly")}</span>{#if running}<Button variant="destructive" size="xs" onclick={stop}><Square class="size-3" />{$translate("extensions.dev.stop")}</Button>{/if}{#if samples.length}<Button variant="ghost" size="xs" class="ml-auto" onclick={download}><Download class="size-3" />{$translate("extensions.dev.export")}</Button>{/if}</div>
    <details class="mb-3"><summary class="cursor-pointer text-xs text-muted-foreground">{$translate("extensions.dev.requestOptions")}</summary><fieldset disabled={running} class="space-y-3 py-3">{@render endpointInput()}<div class="grid gap-3 sm:grid-cols-2"><label>{$translate("extensions.dev.system")}<textarea rows="2" bind:value={system} disabled={customRequest !== null}></textarea></label><label>{$translate("extensions.dev.prompt")}<textarea rows="2" bind:value={prompt} disabled={customRequest !== null}></textarea></label><label>{$translate("extensions.dev.temperature")}<input type="number" min="0" max="2" step="0.1" bind:value={temperature} disabled={customRequest !== null} /></label><label>{$translate("extensions.dev.maxTokens")}<input type="number" min="1" bind:value={maxTokens} disabled={customRequest !== null} /></label><label>{$translate("extensions.profiles")}<input bind:value={profile} /></label><label>{$translate("extensions.providers")}<input bind:value={provider} /></label></div><label class="!flex items-center gap-2"><input type="checkbox" bind:checked={stream} />{$translate("extensions.dev.stream")}</label><Button variant="outline" size="sm" onclick={toggleRequest}>{$translate(customRequest === null ? "extensions.dev.customRequest" : "extensions.dev.simpleRequest")}</Button>{#if customRequest !== null}<ExtensionValueEditor root value={customRequest} onChange={(value) => customRequest = value as Record<string, unknown>} />{/if}</fieldset></details>
  {/if}
  <div class="workbench-columns grid min-w-0 gap-5 xl:grid-cols-[minmax(260px,1fr)_minmax(0,1.5fr)]">
    <fieldset disabled={running} class:hidden={compact} class="min-w-0 space-y-4 rounded-lg border border-border p-4">
      <h3 class="text-sm font-medium">{$translate("extensions.testRequest")}</h3>
      {@render modelInput()}
      {#if customRequest === null}
        <label>{$translate("extensions.dev.prompt")}<textarea rows="4" bind:value={prompt}></textarea></label>
        <label>{$translate("extensions.dev.system")}<textarea rows="2" bind:value={system}></textarea></label>
        <div class="grid grid-cols-2 gap-3"><label>{$translate("extensions.dev.temperature")}<input type="number" min="0" max="2" step="0.1" bind:value={temperature} /></label><label>{$translate("extensions.dev.maxTokens")}<input type="number" min="1" bind:value={maxTokens} /></label></div>
      {:else}<ExtensionValueEditor root value={customRequest} onChange={(next) => customRequest = next as Record<string, unknown>} />{/if}
      <Button variant="outline" size="sm" onclick={toggleRequest}>{$translate(customRequest === null ? "extensions.dev.customRequest" : "extensions.dev.simpleRequest")}</Button>
      <details class="rounded-md border border-border p-3"><summary class="cursor-pointer text-xs font-medium">{$translate("extensions.dev.context")}</summary><div class="mt-3 space-y-3">{@render endpointInput()}<label>{$translate("extensions.profiles")}<input bind:value={profile} /></label><label>{$translate("extensions.providers")}<input bind:value={provider} /></label><label class="!flex items-center gap-2"><input type="checkbox" bind:checked={stream} />{$translate("extensions.dev.stream")}</label></div></details>
      {#if mode === "benchmark"}<label>{$translate("extensions.dev.runs")}<input type="number" min="1" max="100" bind:value={runs} /></label><p class="text-xs text-muted-foreground">{$translate("extensions.dev.benchmarkHint")}</p>{/if}
    </fieldset>
    <section class="min-w-0 space-y-4" aria-live="polite">
      {#if startedAt && !compact}<p class="text-xs text-muted-foreground">{$translate("extensions.dev.lastRun")}: {new Date(startedAt).toLocaleTimeString()}</p>{/if}
      <div class="run-summary grid grid-cols-2 gap-3 sm:grid-cols-4" class:hidden={compact && mode === "debug" && !result}>{#each [["completed", String(summary.total)], ["successRate", summary.total ? `${(summary.success / summary.total * 100).toFixed(0)}%` : "—"], ["mean", timingAvailable ? duration(summary.mean) : "—"], ["p95", timingAvailable ? duration(summary.p95) : "—"]] as [key, value]}<div class="rounded-lg border border-border p-3"><p class="text-xs text-muted-foreground">{$translate(`extensions.dev.${key}`)}</p><p class="mt-2 font-mono text-lg font-semibold">{value}</p></div>{/each}</div>
      {#if running}<p role="status" class="text-sm text-primary">{$translate("extensions.dev.running", { count: samples.length, total: mode === "benchmark" ? runs : 1 })}</p>{/if}
      {#if stopped}<p class="text-sm text-muted-foreground">{$translate("extensions.dev.stopped")}</p>{/if}
      {#if error}<p role="alert" class="break-words rounded-md bg-destructive/10 p-3 text-sm text-destructive">{error}</p>{/if}
      {#if !timingAvailable}<p class="text-xs text-warning">{$translate("extensions.dev.timingUnavailable")}</p>{/if}
      {#if result && !result.matched}<p class="rounded-md bg-warning/10 p-3 text-sm text-warning">{$translate("extensions.dev.unmatched")}</p>{/if}
      {#if result}
        <Tabs.Root bind:value={resultTab}><Tabs.List class="flex h-auto flex-wrap justify-start"><Tabs.Trigger value="changes">{$translate("extensions.dev.changes")} ({changes.length})</Tabs.Trigger><Tabs.Trigger value="logs">{$translate("extensions.console")} ({result.logs?.length ?? 0})</Tabs.Trigger><Tabs.Trigger value="tools">{$translate("extensions.dev.tools")} ({result.injectedTools?.length ?? 0})</Tabs.Trigger><Tabs.Trigger value="raw">{$translate("extensions.dev.raw")}</Tabs.Trigger></Tabs.List>
          <Tabs.Content value="changes" class="space-y-3">{#if !changes.length}<p class="rounded-md border border-dashed border-border p-6 text-center text-sm text-muted-foreground">{$translate("extensions.dev.noChanges")}</p>{/if}{#each changes as key}<div class="overflow-hidden rounded-md border border-border"><h4 class="bg-muted/40 px-3 py-2 font-mono text-xs">{key}</h4><div class="grid gap-px bg-border md:grid-cols-2"><div class="min-w-0 bg-background p-3"><p class="mb-2 text-xs text-muted-foreground">{$translate("extensions.dev.before")}</p><pre class="overflow-auto whitespace-pre-wrap break-all text-xs">{pretty(result.requestBefore[key])}</pre></div><div class="min-w-0 bg-background p-3"><p class="mb-2 text-xs text-muted-foreground">{$translate("extensions.dev.after")}</p><pre class="overflow-auto whitespace-pre-wrap break-all text-xs">{pretty(result.requestAfter[key])}</pre></div></div></div>{/each}</Tabs.Content>
          <Tabs.Content value="logs"><pre class="overflow-auto whitespace-pre-wrap rounded-md bg-muted p-4 text-xs">{result.logs?.join("\n") || $translate("extensions.dev.noLogs")}</pre></Tabs.Content>
          <Tabs.Content value="tools"><pre class="overflow-auto whitespace-pre-wrap rounded-md bg-muted p-4 text-xs">{pretty(result.injectedTools ?? [])}</pre></Tabs.Content>
          <Tabs.Content value="raw"><pre class="max-h-[500px] overflow-auto rounded-md bg-muted p-4 text-xs">{pretty(result)}</pre></Tabs.Content>
        </Tabs.Root>
      {:else if !running}<div class="rounded-lg border border-dashed border-border px-6 py-12 text-center text-sm text-muted-foreground">{$translate("extensions.dev.ready")}</div>{/if}
      {#if mode === "benchmark" && samples.length}<div class="rounded-lg border border-border"><div class="flex flex-wrap gap-4 border-b border-border p-3 text-xs"><span>P50 {duration(timingAvailable ? summary.p50 : null)}</span><span>Min {duration(timingAvailable ? summary.min : null)}</span><span>Max {duration(timingAvailable ? summary.max : null)}</span><span>{$translate("extensions.dev.success")}: {summary.success}</span><span>{$translate("extensions.dev.errors")}: {summary.errors}</span></div><div class="max-h-64 overflow-y-auto"><table class="w-full text-left text-xs"><thead class="sticky top-0 bg-muted"><tr><th class="p-2">#</th><th>{$translate("extensions.workspace.status")}</th><th>{$translate("extensions.dev.duration")}</th><th>{$translate("extensions.dev.roundTrip")}</th></tr></thead><tbody>{#each samples as sample, index}<tr class="border-t border-border"><td class="p-2">{index + 1}</td><td>{$translate(`extensions.dev.${sample.outcome}`)}</td><td class="font-mono">{duration(timingAvailable && sample.outcome === "success" ? sample.duration : null)}</td><td class="font-mono">{duration(sample.roundTrip ?? null)}</td></tr>{/each}</tbody></table></div></div>{/if}
    </section>
  </div>
  {/if}
</div>
<style>
  .compact { padding: .75rem 1rem; }
  .compact .workbench-columns { display: block; }
  .compact .run-summary { display: flex; flex-wrap: wrap; gap: 1rem; }
  .compact .run-summary.hidden { display: none; }
  .compact .run-summary > div { display: flex; gap: .5rem; align-items: baseline; border: none; padding: 0; }
  .compact .run-summary > div > p { margin: 0; font-size: .75rem; }
  .workbench label { display: grid; gap: .5rem; font-size: .75rem; }
  .workbench input:not([type="checkbox"]), .workbench select, .workbench textarea { width: 100%; min-width: 0; border: 1px solid var(--border); border-radius: .375rem; padding: .5rem; background: var(--background); font-size: .8125rem; }
  /* Buttons sit next to labels whose text row makes them look sunken; shift
     them up by the label text row (0.75rem font ≈ 1.125rem line) + gap. */
  .workbench .align-with-control { margin-bottom: calc(1.125rem + .5rem); }
  .chat-input { border: 1px solid var(--border); border-radius: .375rem; padding: .5rem; background: var(--background); font-size: .8125rem; }
  .chat-caret { display: inline-block; width: .5em; height: 1em; margin-left: .15em; vertical-align: text-bottom; background: var(--primary); animation: chat-caret 1s steps(2) infinite; }
  @keyframes chat-caret { 50% { opacity: 0; } }
</style>
