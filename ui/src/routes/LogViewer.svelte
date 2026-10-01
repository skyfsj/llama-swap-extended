<script lang="ts">
  import { proxyLogs, upstreamLogs, models } from "../stores/api";
  import { persistentStore } from "../stores/persistent";
  import IncidentLogPanel from "../components/IncidentLogPanel.svelte";
  import LogPanel from "../components/LogPanel.svelte";
  import { Tabs, TabsList, TabsTrigger, TabsContent } from "$lib/components/ui/tabs/index.js";
  import { translate } from "../lib/i18n";
  import { streamModelLog } from "../stores/modelLogs";
  import { fetchLMCacheServerLog, fetchRuntimeLog, fetchRuntimes, type LMCacheServerLogResponse, type RuntimeLogResponse } from "$lib/runtimeApi";

  type ViewMode = "proxy" | "upstream" | "model" | "runtime" | "lmcache" | "incidents";
  type LogSource = { kind: "runtime"; name: string } | { kind: "lmcache" };

  const viewModeStore = persistentStore<ViewMode>("logviewer-view-mode", "proxy");

  let selectedModel = $state("");
  let selectedRuntime = $state("");
  let runtimeNames = $state<string[]>([]);
  let sourceLog = $state("");
  let sourceLogError = $state("");
  let sourcePollSerial = 0;

  let modelNames = $derived($models.map((model) => model.id).sort((a, b) => a.localeCompare(b, undefined, { numeric: true })));
  let view = $derived($viewModeStore);

  // Per-model inference logs stream over the same long-lived endpoint the
  // model detail page uses; the store reconnects on its own.
  let modelLogData = $state("");
  $effect(() => {
    if (view !== "model" || !selectedModel) return;
    const model = selectedModel;
    const stream = streamModelLog(model);
    const unsub = stream.subscribe((value) => (modelLogData = value));
    return () => {
      unsub();
      modelLogData = "";
    };
  });

  // Runtime and LMCache tails poll their snapshot endpoints: a viewer that
  // opens later still gets the output that streamed before it existed.
  $effect(() => {
    const source = view === "runtime" && selectedRuntime ? ({ kind: "runtime", name: selectedRuntime } as LogSource)
      : view === "lmcache" ? ({ kind: "lmcache" } as LogSource)
      : null;
    if (!source) return;
    const serial = ++sourcePollSerial;
    const load = async (): Promise<void> => {
      try {
        let data: RuntimeLogResponse | LMCacheServerLogResponse;
        if (source.kind === "runtime") data = await fetchRuntimeLog(source.name);
        else data = await fetchLMCacheServerLog();
        if (serial !== sourcePollSerial) return;
        sourceLog = data.output ?? "";
        sourceLogError = "";
      } catch (cause) {
        if (serial === sourcePollSerial) sourceLogError = cause instanceof Error ? cause.message : String(cause);
    }
    };
    void load();
    const timer = setInterval(() => void load(), 1500);
    return () => {
      sourcePollSerial++;
      clearInterval(timer);
      sourceLog = "";
      sourceLogError = "";
    };
  });

  $effect(() => {
    if (view !== "runtime" || runtimeNames.length > 0) return;
    void fetchRuntimes().then((response) => {
      runtimeNames = (response.data ?? []).map((status) => status.name).filter((name) => name !== "lmcache").sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
      if (!selectedRuntime && runtimeNames.length > 0) selectedRuntime = runtimeNames[0];
    }).catch(() => undefined);
  });

  const selectClass = "border-input bg-background h-9 w-72 rounded-md border px-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30";
</script>

<div class="flex flex-col h-full w-full gap-2">
  <Tabs
    value={$viewModeStore}
    onValueChange={(v) => v && viewModeStore.set(v as ViewMode)}
    class="flex flex-1 w-full flex-col gap-2 overflow-hidden"
  >
    <TabsList variant="line">
      <TabsTrigger value="proxy">{$translate("logs.proxy")}</TabsTrigger>
      <TabsTrigger value="upstream">{$translate("logs.upstream")}</TabsTrigger>
      <TabsTrigger value="model">{$translate("logs.modelTab")}</TabsTrigger>
      <TabsTrigger value="runtime">{$translate("logs.runtimeTab")}</TabsTrigger>
      <TabsTrigger value="lmcache">{$translate("logs.lmcacheTab")}</TabsTrigger>
      <TabsTrigger value="incidents">{$translate("logs.incidentTab")}</TabsTrigger>
    </TabsList>

    <div class="flex-1 w-full overflow-hidden flex flex-col gap-2">
      <TabsContent value="proxy" class="h-full">
        <LogPanel id="proxy" title={$translate("logs.proxyTitle")} logData={$proxyLogs} />
      </TabsContent>

      <TabsContent value="upstream" class="h-full">
        <LogPanel id="upstream" title={$translate("logs.upstreamTitle")} logData={$upstreamLogs} />
      </TabsContent>

      <TabsContent value="model" class="h-full flex flex-col gap-2 min-h-0">
        <select class={selectClass} bind:value={selectedModel}>
          <option value="" disabled>{$translate("logs.selectModel")}</option>
          {#each modelNames as name (name)}<option value={name}>{name}</option>{/each}
        </select>
        <div class="flex-1 min-h-0">
          {#if selectedModel}
            <LogPanel id="model-log" title={`${$translate("logs.modelTitle")}: ${selectedModel}`} logData={modelLogData} />
          {:else}
            <p class="rounded-lg border border-dashed p-3 text-xs text-muted-foreground" role="status">{$translate("logs.selectModel")}</p>
          {/if}
        </div>
      </TabsContent>

      <TabsContent value="runtime" class="h-full flex flex-col gap-2 min-h-0">
        <select class={selectClass} bind:value={selectedRuntime}>
          <option value="" disabled>{$translate("logs.selectRuntime")}</option>
          {#each runtimeNames as name (name)}<option value={name}>{name}</option>{/each}
        </select>
        <div class="flex-1 min-h-0">
          {#if selectedRuntime}
            {#if sourceLogError}<p class="text-destructive text-xs" role="alert">{sourceLogError}</p>{/if}
            <LogPanel id="runtime-log" title={`${$translate("logs.runtimeTitle")}: ${selectedRuntime}`} logData={sourceLog} />
          {:else}
            <p class="rounded-lg border border-dashed p-3 text-xs text-muted-foreground" role="status">{$translate("logs.selectRuntime")}</p>
          {/if}
        </div>
      </TabsContent>

      <TabsContent value="lmcache" class="h-full min-h-0">
        {#if sourceLogError}<p class="text-destructive text-xs" role="alert">{sourceLogError}</p>{/if}
        <LogPanel id="lmcache-log" title={$translate("logs.lmcacheTitle")} logData={sourceLog} />
      </TabsContent>

      <TabsContent value="incidents" class="h-full min-h-0">
        <IncidentLogPanel />
      </TabsContent>
    </div>
  </Tabs>
</div>
