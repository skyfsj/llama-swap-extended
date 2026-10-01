<script lang="ts">
  import { onMount } from "svelte";
  import { Plus, RefreshCw, Search, Server, Terminal, X } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { filterRuntimes, runtimeHasUpdate, runtimeNeedsAttention, type RuntimeFilter } from "$lib/runtimeCenter";
  import * as Card from "$lib/components/ui/card/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { translate } from "$lib/i18n";
  import { ansiToHtml } from "$lib/ansi";
  import { isDarkMode } from "../stores/theme";
  import { fetchLMCacheDashboard, fetchLMCacheStatus, postLMCacheAction } from "$lib/lmcacheApi";
  import { fetchLMCacheServerLog, fetchRuntimeCatalog, fetchRuntimeDetail, fetchRuntimeLog, fetchRuntimeReadiness, fetchRuntimes, postRuntimeAction, deleteRuntimeVersion } from "$lib/runtimeApi";
  import { fetchSettings, type SettingsSnapshot } from "$lib/settingsApi";
  import type { BackendProgressEvent, LMCacheDashboardResponse, LMCacheStatus, RuntimeCatalogResponse, RuntimeDetail, RuntimeStatus, RuntimeVersionCandidate } from "$lib/types";
  import { backendProgress } from "../stores/api";
  import ConfirmDialog from "../components/ConfirmDialog.svelte";
  import RuntimeCard from "../components/runtime/RuntimeCard.svelte";
  import RuntimeDetailSheet from "../components/runtime/RuntimeDetailSheet.svelte";
  import RuntimeLMCacheCard from "../components/runtime/RuntimeLMCacheCard.svelte";
  import LMCacheSettingsDialog from "../components/runtime/LMCacheSettingsDialog.svelte";
  import RuntimeVersionDialog from "../components/runtime/RuntimeVersionDialog.svelte";
  import RuntimeWizard from "../components/runtime/RuntimeWizard.svelte";

  let runtimes = $state<RuntimeStatus[]>([]);
  let details = $state<Record<string, RuntimeDetail>>({});
  let detailErrors = $state<Record<string, string>>({});
  let settingsSnapshot = $state<SettingsSnapshot | null>(null);
  let loading = $state(true);
  let refreshing = $state(false);
  let error = $state("");
  let busy = $state<Record<string, string>>({});
  let listError = $state("");
  let settingsError = $state("");
  let listLoaded = $state(false);
  let search = $state("");
  let filter = $state<RuntimeFilter>("all");
  let detailLoading = $state<Record<string, boolean>>({});
  const detailRequests: Record<string, number> = {};
  let disposed = false;
  let stageRequestSerial = 0;
  let requestSerial = 0;

  let selectedName = $state("");
  let detailOpen = $state(false);
  let stageOpen = $state(false);
  let stageRuntimeName = $state("");
  let stageCandidates = $state<RuntimeVersionCandidate[]>([]);
  let stageVersion = $state("");
  let stageLoading = $state(false);
  let stageError = $state("");
  type LogSource = { kind: "runtime"; name: string } | { kind: "lmcache" };
  let logOpen = $state(false);
  let logSource = $state<LogSource | null>(null);
  let logPollSerial = 0;
  let logPolledOutput = $state("");
  let logOperationId = $state("");
  let logTruncated = $state(false);
  let logLoading = $state(false);
  let logPanelError = $state("");
  let logPanelEl = $state<HTMLDivElement | null>(null);
  let logStickToBottom = $state(true);

  function handleLogPanelScroll(): void {
    const el = logPanelEl;
    if (!el) return;
    logStickToBottom = el.scrollHeight - el.scrollTop - el.clientHeight < 32;
  }

  // Keep the newest output visible while the panel polls, unless the reader
  // scrolled up to inspect history.
  $effect(() => {
    void logPolledOutput;
    const el = logPanelEl;
    if (el && logStickToBottom) el.scrollTop = el.scrollHeight;
  });
  let wizardOpen = $state(false);
  let wizardSeed = $state(0);
  let wizardMode = $state<"create" | "clone" | "edit">("create");
  let wizardName = $state("");
  let wizardConfig = $state<Record<string, unknown> | undefined>(undefined);
  let lmcache = $state<LMCacheStatus | null>(null);
  let lmcacheDashboard = $state<LMCacheDashboardResponse | null>(null);
  let lmcacheDashboardLoading = $state(false);
  let lmcacheDashboardError = $state("");
  let lmcacheError = $state("");
  let lmcacheBusy = $state(false);
  let lmcacheSettingsOpen = $state(false);
  let lmcacheDashboardRequest = 0;
  let loadPromise: Promise<void> | null = null;

  let confirmOpen = $state(false);
  let confirmTitle = $state("");
  let confirmMessage = $state("");
  let confirmLabel = $state("");
  let confirmRun = $state<(() => Promise<void>) | null>(null);

  let userRuntimes = $derived(runtimes.filter((runtime) => runtime.name !== "lmcache"));

  let visibleRuntimes = $derived(filterRuntimes(userRuntimes, search, filter));
  let attentionCount = $derived(userRuntimes.filter(runtimeNeedsAttention).length);
  let updateCount = $derived(userRuntimes.filter(runtimeHasUpdate).length);

  $effect(() => { if (!stageOpen) stageRequestSerial++; });

  let selectedRuntime = $derived(runtimes.find((runtime) => runtime.name === selectedName) ?? null);
  let selectedDetail = $derived(details[selectedName]);
  let logRuntimeProgress = $derived(
    logSource?.kind === "runtime" ? $backendProgress[logSource.name] as BackendProgressEvent | undefined : undefined,
  );

  function asRecord(value: unknown): Record<string, any> {
    return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, any> : {};
  }

  function message(cause: unknown): string {
    return cause instanceof Error ? cause.message : String(cause);
  }

  function runtimeConfig(name: string): Record<string, unknown> | undefined {
    return asRecord(asRecord(settingsSnapshot?.config).runtimes)[name] as Record<string, unknown> | undefined;
  }

  function runtimePolicy(name: string): string {
    if (!settingsSnapshot) return "";
    return String(asRecord(runtimeConfig(name)?.update).policy ?? "automatic");
  }

  function askConfirm(title: string, confirm: string, label: string, run: () => Promise<void>): void {
    confirmTitle = title;
    confirmMessage = confirm;
    confirmLabel = label;
    confirmRun = run;
    confirmOpen = true;
  }

  function runConfirm(): void {
    const action = confirmRun;
    confirmOpen = false;
    confirmRun = null;
    if (action) void action();
  }

  async function loadSettings(): Promise<void> {
    try {
      const snapshot = await fetchSettings();
      if (disposed) return;
      settingsSnapshot = snapshot;
      settingsError = "";
    } catch (cause) {
      if (disposed) return;
      settingsSnapshot = null;
      settingsError = message(cause);
    }
  }

  async function loadLMCache(): Promise<void> {
    const dashboardRequest = ++lmcacheDashboardRequest;
    try {
      const next = await fetchLMCacheStatus();
      if (disposed || dashboardRequest !== lmcacheDashboardRequest) return;
      lmcache = next;
      lmcacheError = "";
      if (next.server?.running && next.server.state === "RUNNING" && next.server.healthy) {
        await loadLMCacheDashboard(dashboardRequest);
      } else {
        lmcacheDashboard = null;
        lmcacheDashboardError = "";
        lmcacheDashboardLoading = false;
      }
    } catch (cause) {
      if (disposed || dashboardRequest !== lmcacheDashboardRequest) return;
      lmcacheError = message(cause);
      lmcacheDashboard = null;
      lmcacheDashboardError = "";
      lmcacheDashboardLoading = false;
    }
  }

  async function loadLMCacheDashboard(dashboardRequest: number): Promise<void> {
    lmcacheDashboardLoading = true;
    lmcacheDashboardError = "";
    try {
      const next = await fetchLMCacheDashboard();
      if (disposed || dashboardRequest !== lmcacheDashboardRequest) return;
      lmcacheDashboard = next;
    } catch (cause) {
      if (disposed || dashboardRequest !== lmcacheDashboardRequest) return;
      lmcacheDashboard = null;
      lmcacheDashboardError = message(cause);
    } finally {
      if (dashboardRequest === lmcacheDashboardRequest) lmcacheDashboardLoading = false;
    }
  }

  async function loadDetail(name: string): Promise<void> {
    const serial = (detailRequests[name] ?? 0) + 1;
    detailRequests[name] = serial;
    detailLoading[name] = true;
    detailErrors[name] = "";
    try {
      const detail = await fetchRuntimeDetail(name);
      if (!disposed && serial === detailRequests[name]) details[name] = detail;
    } catch (cause) {
      if (!disposed && serial === detailRequests[name]) detailErrors[name] = message(cause);
    } finally {
      if (!disposed && serial === detailRequests[name]) detailLoading[name] = false;
    }
  }

  async function refresh(manual = false): Promise<void> {
    const serial = ++requestSerial;
    if (manual) refreshing = true;
    if (!listLoaded) loading = true;
    try {
      const results = await Promise.allSettled([fetchRuntimes(), loadSettings(), loadLMCache()]);
      const runtimeResult = results[0];
      if (disposed || serial !== requestSerial) return;
      if (runtimeResult.status === "rejected") throw runtimeResult.reason;
      listLoaded = true;
      listError = "";
      runtimes = Array.isArray(runtimeResult.value.data) ? runtimeResult.value.data : [];
      if (detailOpen && selectedName && runtimes.some((runtime) => runtime.name === selectedName)) await loadDetail(selectedName);
    } catch (cause) {
      if (!disposed && serial === requestSerial) listError = message(cause);
    } finally {
      if (!disposed && serial === requestSerial) {
        loading = false;
        if (manual) refreshing = false;
      }
    }
  }

  function load(manual = false): Promise<void> {
    if (disposed) return Promise.resolve();
    if (loadPromise) return loadPromise;
    loadPromise = refresh(manual).finally(() => {
      loadPromise = null;
    });
    return loadPromise;
  }

  async function loadAfterAction(): Promise<void> {
    // An in-flight poll may predate the mutation. Fetch again after it settles.
    if (loadPromise) await loadPromise;
    await load();
  }

  function openDetails(name: string): void {
    selectedName = name;
    detailOpen = true;
    void loadDetail(name);
  }

  async function openStage(name: string): Promise<void> {
    if (busy[name]) return;
    const serial = ++stageRequestSerial;
    stageRuntimeName = name;
    stageCandidates = [];
    stageVersion = "";
    stageError = "";
    stageLoading = true;
    stageOpen = true;
    try {
      const payload: RuntimeCatalogResponse = await fetchRuntimeCatalog(name);
      if (disposed || serial !== stageRequestSerial || !stageOpen || name !== stageRuntimeName) return;
      stageCandidates = Array.isArray(payload.data) ? payload.data : [];
      const runtime = runtimes.find((item) => item.name === name);
      if (stageCandidates.length === 0 && runtime?.available) stageCandidates = [{ version: runtime.available, recommended: true }];
      stageVersion = stageCandidates.find((candidate) => candidate.recommended && !candidate.installed)?.version
        ?? stageCandidates.find((candidate) => !candidate.installed)?.version
        ?? stageCandidates[0]?.version ?? "";
    } catch (cause) {
      if (!disposed && serial === stageRequestSerial && stageOpen) stageError = message(cause);
    } finally {
      if (!disposed && serial === stageRequestSerial) stageLoading = false;
    }
  }

  async function stageSelected(candidate: RuntimeVersionCandidate): Promise<void> {
    const name = stageRuntimeName;
    if (!name || stageLoading || !stageOpen || busy[name] || !stageCandidates.includes(candidate)) return;
    const serial = stageRequestSerial;
    busy[name] = "stage";
    stageError = "";
    try {
      await postRuntimeAction(name, "stage", { version: candidate.version, ref: candidate.ref, commit: candidate.commit, digest: candidate.digest });
      if (serial === stageRequestSerial) stageOpen = false;
      await loadAfterAction();
    } catch (cause) {
      if (serial === stageRequestSerial && stageOpen) stageError = message(cause);
      else error = message(cause);
    } finally {
      delete busy[name];
    }
  }

  async function operation(name: string, path: string, body?: unknown): Promise<void> {
    if (busy[name]) return;
    busy[name] = path;
    error = "";
    try {
      await postRuntimeAction(name, path, body);
      await loadAfterAction();
    } catch (cause) {
      error = message(cause);
    } finally {
      delete busy[name];
    }
  }

  async function activate(name: string, version: string): Promise<void> {
    if (!version || busy[name]) return;
    busy[name] = "readiness";
    error = "";
    try {
      let check = await fetchRuntimeReadiness(name, version);
      if (disposed) return;
      let blocked = check.checks.find((item) => item.level === "block");
      if (blocked?.code === "candidate-not-staged") {
        // The target version is already installed but not staged; stage it
        // (idempotent on the server) and re-check before asking to activate.
        busy[name] = "stage";
        await postRuntimeAction(name, "stage", { version });
        if (disposed) return;
        check = await fetchRuntimeReadiness(name, version);
        if (disposed) return;
        blocked = check.checks.find((item) => item.level === "block");
      }
      if (blocked) {
        error = `${blocked.title}: ${blocked.detail}`;
        return;
      }
      askConfirm($translate("controlPlane.runtimeCenter.actions.activate"), $translate("controlPlane.runtimeActivateConfirm", { name, version }), $translate("controlPlane.runtimeCenter.actions.activate"), () => operation(name, `activate/${encodeURIComponent(version)}`));
    } catch (cause) {
      error = $translate("controlPlane.runtimeCenter.readiness.loadError", { message: message(cause) });
    } finally {
      delete busy[name];
    }
  }

  function rollback(name: string, version: string): void {
    askConfirm($translate("controlPlane.runtimeCenter.actions.rollback"), $translate("controlPlane.runtimeRollbackConfirm", { name, version }), $translate("controlPlane.runtimeCenter.actions.rollback"), () => operation(name, "rollback"));
  }

  function pin(name: string, version: string): void {
    askConfirm($translate("controlPlane.runtimeCenter.actions.pin"), $translate("controlPlane.runtimePinConfirm", { name, version }), $translate("controlPlane.runtimeCenter.actions.pin"), () => operation(name, `pin/${encodeURIComponent(version)}`));
  }

  function deleteVersion(name: string, version: string): void {
    askConfirm($translate("controlPlane.runtimeCenter.actions.deleteVersion"), $translate("controlPlane.runtimeDeleteConfirm", { name, version }), $translate("controlPlane.runtimeCenter.actions.deleteVersion"), async () => {
      if (busy[name]) return;
      busy[name] = `delete:${version}`;
      error = "";
      try {
        await deleteRuntimeVersion(name, version);
        await loadAfterAction();
      } catch (cause) {
        error = message(cause);
      } finally {
        delete busy[name];
      }
    });
  }

  function openLogs(name: string): void {
    logSource = { kind: "runtime", name };
    logOpen = true;
  }

  function openLMCacheLogs(): void {
    logSource = { kind: "lmcache" };
    logOpen = true;
  }

  // The log panels poll their source instead of relying only on the SSE
  // stream: a panel opened mid-build (or after it finished, or after a
  // reconnect) still needs the output that streamed before it existed.
  $effect(() => {
    if (!logOpen || !logSource) return;
    const source = logSource;
    const serial = ++logPollSerial;
    const load = async (): Promise<void> => {
      logLoading = true;
      try {
        if (source.kind === "runtime") {
          const data = await fetchRuntimeLog(source.name);
          if (serial !== logPollSerial) return;
          logOperationId = data.operationId ?? "";
          logPolledOutput = data.output ?? "";
          logTruncated = false;
        } else {
          const data = await fetchLMCacheServerLog();
          if (serial !== logPollSerial) return;
          logOperationId = "";
          logPolledOutput = data.output ?? "";
          logTruncated = data.truncated;
        }
        logPanelError = "";
      } catch (cause) {
        if (serial === logPollSerial) logPanelError = message(cause);
      } finally {
        if (serial === logPollSerial) logLoading = false;
      }
    };
    void load();
    const timer = setInterval(() => void load(), 1500);
    return () => {
      logPollSerial++;
      clearInterval(timer);
    };
  });

  function openWizard(nextMode: "create" | "clone" | "edit", name = "", config?: Record<string, unknown>): void {
    wizardMode = nextMode;
    wizardName = name;
    wizardConfig = config;
    wizardSeed += 1;
    wizardOpen = true;
  }

  function editRuntime(name: string): void {
    const config = runtimeConfig(name);
    if (config) openWizard("edit", name, config);
    else error = $translate("controlPlane.runtimeCenter.wizard.settingsUnavailable");
  }

  async function commitLMCacheAction(path: string, body?: unknown): Promise<boolean> {
    if (lmcacheBusy) return false;
    lmcacheBusy = true;
    lmcacheError = "";
    lmcacheDashboardRequest++;
    lmcacheDashboard = null;
    lmcacheDashboardError = "";
    lmcacheDashboardLoading = false;
    try {
      await postLMCacheAction(path, body);
      await loadAfterAction();
      return true;
    } catch (cause) {
      lmcacheError = message(cause);
      return false;
    } finally {
      lmcacheBusy = false;
    }
  }

  async function commitLMCacheRuntimeAction(path: string): Promise<void> {
    lmcacheBusy = true;
    lmcacheError = "";
    try {
      await postRuntimeAction("lmcache", path);
      await loadAfterAction();
    } catch (cause) {
      lmcacheError = message(cause);
    } finally {
      lmcacheBusy = false;
    }
  }

  onMount(() => {
    void load();
    const timer = window.setInterval(() => void load(), 10000);
    return () => {
      window.clearInterval(timer);
      disposed = true;
      requestSerial++;
      stageRequestSerial++;
      lmcacheDashboardRequest++;
    };
  });
</script>

<section class="space-y-4" aria-labelledby="runtimes-title">
  {#if error}<div class="flex items-start justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive" role="alert"><p class="min-w-0 break-words [overflow-wrap:anywhere]">{$translate("controlPlane.error", { message: error })}</p><Button variant="ghost" size="icon-xs" aria-label={$translate("common.close")} onclick={() => (error = "")}><X aria-hidden="true" /></Button></div>
  {/if}
  {#if settingsError}<p class="break-words rounded-lg border bg-muted/20 p-3 text-xs text-muted-foreground" role="status">{$translate("controlPlane.runtimeCenter.wizard.settingsUnavailable")} {settingsError}</p>
  {/if}

  <Card.Root class="shrink-0 gap-0 overflow-hidden py-0" aria-labelledby="runtimes-title">
    <Card.Header class="shrink-0 gap-3 border-b px-4 py-3">
      <div class="flex w-full items-center justify-between gap-3">
        <div class="flex min-w-0 items-center gap-2">
          <Server class="size-5 shrink-0" aria-hidden="true" />
          <h1 id="runtimes-title" class="text-lg font-semibold">{$translate("navigation.runtimes")}</h1>
          <span class="text-muted-foreground text-sm tabular-nums">{listLoaded ? userRuntimes.length : "—"}</span>
        </div>
        <div class="flex shrink-0 items-center gap-2">
          <Button variant="outline" size="sm" onclick={() => void load(true)} disabled={refreshing}><RefreshCw aria-hidden="true" class={refreshing ? "animate-spin" : ""} />{$translate("common.refresh")}</Button>
          <Button size="sm" disabled={!settingsSnapshot?.writable} onclick={() => openWizard("create")}><Plus aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.create")}</Button>
        </div>
      </div>
      <div class="flex w-full flex-wrap items-center gap-3">
        <div class="flex flex-wrap items-center gap-1" role="group" aria-label={$translate("controlPlane.runtimeCenter.list.filter")}>
          {#each ["all", "attention", "updates"] as item}
            {@const count = item === "all" ? userRuntimes.length : item === "attention" ? attentionCount : updateCount}
            <Button variant={filter === item ? "secondary" : "ghost"} size="sm" aria-pressed={filter === item} onclick={() => (filter = item as RuntimeFilter)}>{$translate(`controlPlane.runtimeCenter.list.${item}`)}<span class="ml-1 tabular-nums text-muted-foreground">{listLoaded ? count : "—"}</span></Button>
          {/each}
        </div>
        <label class="relative ml-auto w-full sm:w-72">
          <Search class="pointer-events-none absolute top-1/2 left-2.5 size-4 -translate-y-1/2 text-muted-foreground" aria-hidden="true" />
          <Input type="search" class="pl-9" aria-label={$translate("controlPlane.runtimeCenter.list.search")} placeholder={$translate("controlPlane.runtimeCenter.list.search")} bind:value={search} />
        </label>
      </div>
    </Card.Header>
  </Card.Root>

  <Card.Root class="shrink-0 gap-0 overflow-hidden py-0" aria-label={$translate("controlPlane.runtimeCenter.list.environments")}>
    <Card.Content class="p-0">
      {#if listError}<div class="m-4 flex flex-wrap items-center justify-between gap-3 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive" role="alert"><p class="min-w-0 break-words">{$translate("controlPlane.runtimeCenter.list.loadError", { message: listError })}</p><Button variant="outline" size="sm" disabled={refreshing} onclick={() => void load(true)}>{$translate("common.refresh")}</Button></div>
      {/if}
      {#if loading}
        <div class="grid gap-3 p-4" role="status"><span class="sr-only">{$translate("controlPlane.loading")}</span>{#each [1, 2, 3] as row (row)}<div class="h-14 animate-pulse rounded-lg bg-muted/40" aria-hidden="true"></div>{/each}</div>
      {:else if listLoaded && userRuntimes.length === 0}
        <div class="px-4 py-12 text-center" role="status"><Server class="mx-auto size-8 text-muted-foreground" aria-hidden="true" /><h3 class="mt-3 text-sm font-semibold">{$translate("controlPlane.runtimeCenter.empty.title")}</h3><p class="mx-auto mt-1 max-w-lg text-sm text-muted-foreground">{$translate("controlPlane.runtimeCenter.empty.description")}</p><Button class="mt-4" variant="outline" disabled={!settingsSnapshot?.writable} onclick={() => openWizard("create")}><Plus aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.create")}</Button></div>
      {:else if listLoaded && visibleRuntimes.length === 0}
        <div class="px-4 py-10 text-center" role="status"><Search class="mx-auto size-7 text-muted-foreground" aria-hidden="true" /><h3 class="mt-3 text-sm font-semibold">{$translate("controlPlane.runtimeCenter.list.noResults")}</h3><p class="mt-1 text-sm text-muted-foreground">{$translate("controlPlane.runtimeCenter.list.noResultsHint")}</p><Button class="mt-3" variant="outline" size="sm" onclick={() => { search = ""; filter = "all"; }}>{$translate("controlPlane.runtimeCenter.list.clearFilters")}</Button></div>
      {:else if listLoaded}
        <div class="overflow-x-auto">
          <table class="w-full min-w-[860px] text-left text-sm">
            <caption class="sr-only">{$translate("controlPlane.runtimeCenter.list.environments")}</caption>
            <thead class="bg-muted/30 text-muted-foreground border-b text-xs">
              <tr>
                <th class="px-4 py-2.5 font-medium">{$translate("controlPlane.runtimeCenter.list.runtime")}</th>
                <th class="px-4 py-2.5 font-medium">{$translate("controlPlane.runtimeCenter.facts.current")}</th>
                <th class="px-4 py-2.5 font-medium">{$translate("controlPlane.runtimeCenter.facts.candidate")}</th>
                <th class="px-4 py-2.5 font-medium">{$translate("controlPlane.runtimeCenter.facts.policy")}</th>
                <th class="px-4 py-2.5 text-right font-medium">{$translate("controlPlane.runtimeCenter.list.actions")}</th>
              </tr>
            </thead>
            <tbody class="divide-y">
              {#each visibleRuntimes as runtime (runtime.name)}
                <RuntimeCard {runtime} progress={$backendProgress[runtime.name]} policy={runtimePolicy(runtime.name)} busy={Boolean(busy[runtime.name])} editable={Boolean(settingsSnapshot?.writable)} onDetails={() => openDetails(runtime.name)} onCheck={() => void operation(runtime.name, "check")} onStage={() => void openStage(runtime.name)} onActivate={() => void activate(runtime.name, runtime.staged ?? "")} onRollback={() => rollback(runtime.name, runtime.previous ?? "")} onLogs={() => openLogs(runtime.name)} onEdit={() => editRuntime(runtime.name)} />
              {/each}
            </tbody>
          </table>
        </div>
      {/if}
    </Card.Content>
  </Card.Root>

  <RuntimeLMCacheCard status={lmcache} dashboard={lmcacheDashboard} dashboardLoading={lmcacheDashboardLoading} dashboardError={lmcacheDashboardError} progress={$backendProgress["lmcache"]} error={lmcacheError} busy={lmcacheBusy} onEnable={() => void commitLMCacheAction("enable")} onStart={() => void commitLMCacheAction("server/start")} onStop={() => askConfirm($translate("controlPlane.runtimeCenter.cache.stopTitle"), $translate("controlPlane.runtimeCenter.cache.stopConfirm"), $translate("controlPlane.lmcacheServerStop"), async () => { await commitLMCacheAction("server/stop"); })} onSettings={() => (lmcacheSettingsOpen = true)} onLogs={() => openLMCacheLogs()} onRefresh={() => void load(true)} />
  <LMCacheSettingsDialog bind:open={lmcacheSettingsOpen} status={lmcache} dashboard={lmcacheDashboard} busy={lmcacheBusy} error={lmcacheError} onRefresh={loadAfterAction} onAction={commitLMCacheAction} onRuntimeAction={commitLMCacheRuntimeAction} />
</section>



<RuntimeDetailSheet
  bind:open={detailOpen}
  runtime={selectedRuntime}
  detail={selectedDetail}
  loading={detailLoading[selectedName] ?? false}
  error={detailErrors[selectedName] || ""}
  actionError={error}
  onRetry={() => selectedName && void loadDetail(selectedName)}
  policy={selectedRuntime ? runtimePolicy(selectedRuntime.name) : "automatic"}
  busy={Boolean(selectedRuntime && busy[selectedRuntime.name])}
  onStage={() => selectedRuntime && void openStage(selectedRuntime.name)}
  onActivate={(version) => selectedRuntime && void activate(selectedRuntime.name, version)}
  onRollback={() => selectedRuntime && rollback(selectedRuntime.name, selectedRuntime.previous ?? "")}
  onPin={(version) => selectedRuntime && pin(selectedRuntime.name, version)}
  onUnpin={() => selectedRuntime && void operation(selectedRuntime.name, "unpin")}
  onDelete={(version) => selectedRuntime && deleteVersion(selectedRuntime.name, version)}
/>

<RuntimeVersionDialog
  bind:open={stageOpen}
  runtimeName={stageRuntimeName}
  candidates={stageCandidates}
  bind:selectedVersion={stageVersion}
  loading={stageLoading}
  busy={busy[stageRuntimeName] === "stage"}
  error={stageError}
  onRefresh={() => void openStage(stageRuntimeName)}
  onStage={(candidate) => void stageSelected(candidate)}
/>

<RuntimeWizard
  bind:open={wizardOpen}
  seed={wizardSeed}
  runtimeNames={userRuntimes.map((runtime) => runtime.name)}
  settingsSnapshot={settingsSnapshot}
  initialName={wizardName}
  initialConfig={wizardConfig}
  mode={wizardMode}
  onCommitted={() => { void loadAfterAction(); }}
/>

<Dialog.Root bind:open={logOpen}>
    <Dialog.Content showCloseButton={false} class="max-h-[calc(100dvh-2rem)] max-w-3xl sm:max-w-3xl overflow-y-auto">
    <Dialog.Header><Dialog.Title class="flex items-center gap-2"><Terminal class="size-4" aria-hidden="true" />{logSource?.kind === "lmcache" ? $translate("controlPlane.runtimeCenter.logs.lmcacheTitle") : `${$translate("controlPlane.runtimeCenter.logs.title")}: ${logSource?.kind === "runtime" ? logSource.name : ""}`}</Dialog.Title><Dialog.Description>{$translate("controlPlane.runtimeCenter.logs.description")}</Dialog.Description></Dialog.Header>
    {#if logSource?.kind === "runtime" && logOperationId}<p class="text-muted-foreground font-mono text-xs">{logOperationId}</p>{/if}
    {#if logSource?.kind === "runtime" && logRuntimeProgress}<div class="flex items-center justify-between rounded-md border bg-muted/20 px-3 py-2 text-xs" aria-live="polite"><span>{logRuntimeProgress.phase}</span><span>{Math.round(Math.max(0, Math.min(1, logRuntimeProgress.progress)) * 100)}%</span></div>{/if}
    {#if logSource?.kind === "lmcache" && logTruncated}<p class="text-muted-foreground text-xs" role="note">{$translate("controlPlane.runtimeCenter.logs.truncated")}</p>{/if}
    {#if logPanelError}<p class="text-destructive text-xs" role="alert">{logPanelError}</p>{/if}
    {#if logPolledOutput}<div class="max-h-[55vh] overflow-auto rounded-lg border bg-muted/20 p-3" bind:this={logPanelEl} onscroll={handleLogPanelScroll}><pre class="whitespace-pre-wrap break-words font-mono text-xs leading-5">{@html ansiToHtml(logPolledOutput, $isDarkMode)}</pre></div>{:else if logLoading}<p class="rounded-lg border border-dashed p-3 text-xs text-muted-foreground" role="status">{$translate("controlPlane.loading")}</p>{:else}<p class="rounded-lg border border-dashed p-3 text-xs text-muted-foreground" role="status">{$translate("controlPlane.runtimeCenter.logs.empty")}</p>{/if}
    <Dialog.Footer showCloseButton />
  </Dialog.Content>
</Dialog.Root>

<ConfirmDialog bind:open={confirmOpen} title={confirmTitle} message={confirmMessage} confirmLabel={confirmLabel} onConfirm={runConfirm} />
