<script lang="ts">
  import { untrack } from "svelte";
  import { LoaderCircle, Pin, PinOff, Play, RefreshCw, RotateCcw, Save, Settings2, Square, Trash2 } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Switch } from "$lib/components/ui/switch/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { fetchLMCacheCandidates, fetchLMCacheConfig, patchLMCacheConfig, type LMCacheConfigSnapshot } from "$lib/lmcacheApi";
  import { translate } from "$lib/i18n";
  import type { LMCacheDashboardResponse, LMCacheStatus, RuntimeVersionCandidate } from "$lib/types";
  import ConfirmDialog from "../ConfirmDialog.svelte";

  interface Props {
    open?: boolean;
    status: LMCacheStatus | null;
    dashboard?: LMCacheDashboardResponse | null;
    busy?: boolean;
    error?: string;
    onRefresh: () => Promise<void>;
    onAction: (path: string, body?: unknown) => Promise<boolean>;
    onRuntimeAction: (path: string) => Promise<void>;
  }

  interface LMCacheForm {
    serverEnabled: boolean;
    autoStart: boolean;
    autoStop: boolean;
    poolMaxBytes: string;
    l3Enabled: boolean;
    l3Path: string;
    l3MaxBytes: string;
    evictionPolicy: string;
    chunkSizeAuto: boolean;
    chunkSize: number;
    policy: string;
    channel: string;
    version: string;
    checkEvery: string;
    minIdle: string;
    activateOnlyWhenIdle: boolean;
    buildWhileBusy: boolean;
    keepVersions: number;
  }

  let {
    open = $bindable(false),
    status,
    dashboard = null,
    busy = false,
    error = "",
    onRefresh,
    onAction,
    onRuntimeAction,
  }: Props = $props();

  let configSnapshot = $state<LMCacheConfigSnapshot | null>(null);
  let form = $state<LMCacheForm | null>(null);
  let candidates = $state<RuntimeVersionCandidate[]>([]);
  let selectedUpgradeVersion = $state("");
  let loading = $state(false);
  let saving = $state(false);
  let candidatesLoading = $state(false);
  let updateChecked = $state(false);
  let localError = $state("");
  let sessionID = 0;
  let requestID = 0;
  let candidateRequestID = 0;
  let confirmOpen = $state(false);
  let confirmTitle = $state("");
  let confirmMessage = $state("");
  let confirmLabel = $state("");
  let confirmRun = $state<(() => Promise<unknown>) | null>(null);

  function asRecord(value: unknown): Record<string, unknown> {
    return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {};
  }

  function stringValue(value: unknown, fallback = ""): string {
    return typeof value === "string" ? value : value === undefined || value === null ? fallback : String(value);
  }

  function numberValue(value: unknown, fallback: number): number {
    const parsed = typeof value === "number" ? value : typeof value === "string" && value.trim() ? Number(value) : Number.NaN;
    return Number.isFinite(parsed) ? parsed : fallback;
  }

  function booleanValue(value: unknown, fallback: boolean): boolean {
    return typeof value === "boolean" ? value : fallback;
  }

  function durationValue(value: unknown, fallback: string): string {
    if (typeof value === "string" && value.trim()) return value;
    if (typeof value === "number" && Number.isFinite(value) && value > 0) {
      const hours = value / (60 * 60 * 1_000_000_000);
      if (Number.isInteger(hours)) return `${hours}h`;
      const minutes = value / (60 * 1_000_000_000);
      if (Number.isInteger(minutes)) return `${minutes}m`;
      const seconds = value / 1_000_000_000;
      if (Number.isInteger(seconds)) return `${seconds}s`;
    }
    return fallback;
  }

  function moduleConfig(snapshot: LMCacheConfigSnapshot): Record<string, unknown> {
    const config = asRecord(snapshot.config);
    return asRecord(config.lmcache ?? config.LMCache);
  }

  function readForm(snapshot: LMCacheConfigSnapshot): LMCacheForm {
    const module = moduleConfig(snapshot);
    const serverConfig = asRecord(module.server);
    const l2 = asRecord(serverConfig.l2);
    const l3 = asRecord(serverConfig.l3);
    const update = asRecord(module.update);
    // The pool has a single size knob (l2.maxBytes). A config that still
    // carries the legacy l1SizeGB migrates into it on read so saving never
    // silently drops the operator's effective pool size.
    const legacyL1GB = numberValue(serverConfig.l1SizeGB, 0);
    const chunkRaw = numberValue(serverConfig.chunkSize, 0);
    return {
      serverEnabled: booleanValue(serverConfig.enabled, false),
      autoStart: booleanValue(module.autoStart, true),
      autoStop: booleanValue(module.autoStop, false),
      poolMaxBytes: stringValue(l2.maxBytes).trim() || (legacyL1GB > 0 ? `${legacyL1GB}GiB` : ""),
      l3Enabled: booleanValue(l3.enabled, false),
      l3Path: stringValue(l3.path),
      l3MaxBytes: stringValue(l3.maxBytes),
      evictionPolicy: stringValue(serverConfig.evictionPolicy, "LRU"),
      chunkSizeAuto: chunkRaw <= 0,
      chunkSize: chunkRaw > 0 ? chunkRaw : 256,
      policy: stringValue(update.policy, "disabled"),
      channel: stringValue(update.channel, "stable"),
      version: stringValue(update.version).trim() || stringValue(module.version),
      checkEvery: durationValue(update.checkEvery, "24h"),
      minIdle: durationValue(update.minIdle, "30m"),
      activateOnlyWhenIdle: booleanValue(update.activateOnlyWhenIdle, true),
      buildWhileBusy: booleanValue(update.buildWhileBusy, false),
      keepVersions: numberValue(update.keepVersions, 2),
    };
  }

  function buildConfig(): Record<string, unknown> {
    if (!form || !configSnapshot) return {};
    const existing = moduleConfig(configSnapshot);
    const existingServer = asRecord(existing.server);
    const existingL2 = asRecord(existingServer.l2);
    const existingL3 = asRecord(existingServer.l3);
    const existingUpdate = asRecord(existing.update);
    // l1SizeGB is dropped on write: the pool has exactly one size knob
    // (l2.maxBytes), so a legacy value can never silently win or conflict.
    // The pool itself cannot be disabled, so l2.enabled is always true.
    const server: Record<string, unknown> = {
      ...existingServer,
      enabled: form.serverEnabled,
      l2: { ...existingL2, enabled: true, maxBytes: form.poolMaxBytes.trim() },
      l3: { ...existingL3, enabled: form.l3Enabled, path: form.l3Path.trim(), maxBytes: form.l3MaxBytes.trim() },
      evictionPolicy: form.evictionPolicy,
      chunkSize: form.chunkSizeAuto ? 0 : numberValue(form.chunkSize, 256),
    };
    delete server.l1SizeGB;
    return {
      ...existing,
      autoStart: form.autoStart,
      autoStop: form.autoStop,
      server,
      update: {
        ...existingUpdate,
        policy: form.policy,
        channel: form.channel,
        version: form.version,
        checkEvery: form.checkEvery.trim(),
        minIdle: form.minIdle.trim(),
        activateOnlyWhenIdle: form.activateOnlyWhenIdle,
        buildWhileBusy: form.buildWhileBusy,
        keepVersions: numberValue(form.keepVersions, 2),
        rollbackOnFailure: true,
      },
    };
  }

  function message(cause: unknown): string {
    return cause instanceof Error ? cause.message : String(cause);
  }

  function currentSession(id: number): boolean {
    return open && id === sessionID;
  }

  async function loadCandidates(): Promise<boolean> {
    const session = sessionID;
    const id = ++candidateRequestID;
    candidatesLoading = true;
    try {
      const response = await fetchLMCacheCandidates();
      if (!currentSession(session) || id !== candidateRequestID) return false;
      candidates = Array.isArray(response.data) ? response.data : [];
      return true;
    } catch (cause) {
      if (currentSession(session) && id === candidateRequestID) {
        candidates = [];
        if (!localError) localError = message(cause);
      }
      return false;
    } finally {
      if (currentSession(session) && id === candidateRequestID) candidatesLoading = false;
    }
  }

  async function load(): Promise<void> {
    if (!open) return;
    const session = sessionID;
    const id = ++requestID;
    loading = true;
    localError = "";
    try {
      const snapshot = await fetchLMCacheConfig();
      if (!currentSession(session) || id !== requestID) return;
      configSnapshot = snapshot;
      form = readForm(snapshot);
    } catch (cause) {
      if (currentSession(session) && id === requestID) {
        configSnapshot = null;
        form = null;
        localError = message(cause);
      }
    } finally {
      if (currentSession(session) && id === requestID) loading = false;
    }
  }

  async function save(): Promise<void> {
    if (!open || saving || busy || !configSnapshot || !form || !configSnapshot.writable) return;
    const session = sessionID;
    saving = true;
    localError = "";
    try {
      const snapshot = await patchLMCacheConfig(configSnapshot, buildConfig());
      if (currentSession(session)) {
        configSnapshot = snapshot;
        form = readForm(snapshot);
        updateChecked = false;
      }
      await onRefresh();
    } catch (cause) {
      if (currentSession(session)) localError = message(cause);
    } finally {
      if (currentSession(session)) saving = false;
    }
  }

  async function runAction(path: string, body?: unknown): Promise<boolean> {
    if (!open) return false;
    const session = sessionID;
    localError = "";
    const succeeded = await onAction(path, body);
    if (!currentSession(session) || !succeeded) return false;
    return loadCandidates();
  }

  async function checkLifecycle(): Promise<void> {
    const session = sessionID;
    updateChecked = false;
    const succeeded = await runAction("check");
    if (currentSession(session)) updateChecked = succeeded;
  }

  function ask(titleKey: string, messageKey: string, labelKey: string, run: () => Promise<unknown>): void {
    confirmTitle = $translate(titleKey);
    confirmMessage = $translate(messageKey);
    confirmLabel = $translate(labelKey);
    confirmRun = run;
    confirmOpen = true;
  }

  function confirm(): void {
    const run = confirmRun;
    confirmOpen = false;
    confirmRun = null;
    if (open && run) void run();
  }

  function versionOptions(): string[] {
    const values = [
      ...candidates.map((candidate) => candidate.version),
      status?.update?.current,
      status?.update?.previous,
      status?.update?.staged,
      status?.update?.available,
      status?.update?.targetVersion,
      form?.version,
    ];
    return [...new Set(values.map((value) => value?.trim()).filter((value): value is string => Boolean(value)))].sort((left, right) => right.localeCompare(left, undefined, { numeric: true }));
  }

  function upgradeOptions(): string[] {
    const values = [
      ...candidates.map((candidate) => candidate.version),
      status?.update?.current,
      status?.update?.previous,
      status?.update?.staged,
      status?.update?.available,
      status?.update?.targetVersion,
      form?.version,
    ];
    return [...new Set(values.map((value) => value?.trim()).filter((value): value is string => Boolean(value)))].sort((left, right) => right.localeCompare(left, undefined, { numeric: true }));
  }

  function isConfiguredOnly(version: string): boolean {
    if (version !== form?.version || candidates.some((candidate) => candidate.version === version)) return false;
    return ![status?.update?.current, status?.update?.previous, status?.update?.staged, status?.update?.available]
      .some((known) => known === version);
  }

  function displayTimestamp(value?: string): string {
    return value && !value.startsWith("0001-01-01") ? value : $translate("controlPlane.lmcacheNever");
  }

  function serverStateLabel(): string {
    const state = status?.server?.state;
    if (!state) return "—";
    if (status?.installed && state === "NOT_INSTALLED") return $translate("controlPlane.lmcacheServerStopped");
    return $translate(`controlPlane.lmcacheState.${state}`);
  }

  // The running server reports the chunk size it actually uses through the
  // dashboard's /status projection; the chunk-size field surfaces it as the
  // recognized value instead of the operator having to guess.
  let recognizedChunkSize = $derived(dashboard?.status?.chunkSize ?? 0);

  $effect(() => {
    if (!open) return;
    sessionID++;
    // Only opening the dialog starts a load. Failed requests stay visible
    // until the user retries instead of retriggering this effect.
    untrack(() => void load());
    return () => {
      sessionID++;
      requestID++;
      candidateRequestID++;
      configSnapshot = null;
      form = null;
      candidates = [];
      selectedUpgradeVersion = "";
      updateChecked = false;
      loading = false;
      saving = false;
      candidatesLoading = false;
      localError = "";
      confirmOpen = false;
      confirmRun = null;
    };
  });
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="max-h-[calc(100dvh-1.5rem)] max-w-4xl overflow-y-auto sm:max-w-4xl">
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2"><Settings2 class="size-4" aria-hidden="true" />{$translate("controlPlane.lmcacheSettings.title")}</Dialog.Title>
    </Dialog.Header>

    {#if loading}
      <div class="flex items-center gap-2 py-10 text-sm text-muted-foreground" role="status" aria-live="polite"><LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{$translate("controlPlane.lmcacheSettings.loading")}</div>
    {:else if localError && !form}
      <div class="grid gap-3" role="alert"><p class="rounded-md border border-destructive/35 bg-destructive/10 p-3 text-sm text-destructive">{localError}</p><Button variant="outline" onclick={() => void load()}><RefreshCw class="size-4" aria-hidden="true" />{$translate("common.refresh")}</Button></div>
    {:else if form}
      <div class="grid gap-5">
        {#if error || localError}<div class="rounded-md border border-destructive/35 bg-destructive/10 p-3 text-sm text-destructive" role="alert">{error || localError}</div>{/if}

        <section class="grid gap-3 rounded-lg border p-3" aria-labelledby="lmcache-settings-service">
          <div class="flex flex-wrap items-start justify-between gap-3"><h3 id="lmcache-settings-service" class="text-sm font-semibold">{$translate("controlPlane.lmcacheSettings.serviceTitle")}</h3><div class="flex flex-wrap gap-2">{#if status?.installed}<Button variant="destructive" size="sm" disabled={busy} onclick={() => ask("controlPlane.lmcacheDisable", "controlPlane.lmcacheDisableConfirm", "controlPlane.lmcacheDisable", () => runAction("disable"))}><Trash2 data-icon="inline-start" />{$translate("controlPlane.lmcacheDisable")}</Button>{:else}<Button size="sm" disabled={busy} onclick={() => void runAction("enable")}><Play data-icon="inline-start" />{$translate("controlPlane.lmcacheEnable")}</Button>{/if}</div></div>
          <div class="grid gap-3 sm:grid-cols-3"><div><span class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheSettings.installation")}</span><p class="mt-1 text-sm font-medium">{status?.installed ? $translate("controlPlane.lmcacheInstalled") : $translate("controlPlane.lmcacheNotInstalled")}</p></div><div><span class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheSettings.serverState")}</span><p class="mt-1 text-sm font-medium">{serverStateLabel()}</p></div><div><span class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheSettings.currentVersion")}</span><p class="mt-1 font-mono text-sm">{status?.update?.current || status?.version || "—"}</p></div></div>
          <div class="flex flex-wrap gap-2">{#if status?.server?.running}<Button variant="outline" size="sm" disabled={busy} onclick={() => void runAction("server/stop")}><Square data-icon="inline-start" />{$translate("controlPlane.lmcacheServerStop")}</Button>{:else if status?.installed}<Button variant="outline" size="sm" disabled={busy} onclick={() => void runAction("server/start")}><Play data-icon="inline-start" />{$translate("controlPlane.lmcacheServerStart")}</Button>{/if}<Button variant="outline" size="sm" disabled={busy || !status?.installed} onclick={() => ask("controlPlane.lmcacheServerRestart", "controlPlane.lmcacheRestartConfirm", "controlPlane.lmcacheServerRestart", () => runAction("server/restart"))}><RotateCcw data-icon="inline-start" />{$translate("controlPlane.lmcacheServerRestart")}</Button></div>
        </section>

        <section class="grid gap-3 rounded-lg border p-3" aria-labelledby="lmcache-settings-server">
          <h3 id="lmcache-settings-server" class="text-sm font-semibold">{$translate("controlPlane.lmcacheSettings.serverTitle")}</h3>
          <div class="grid gap-3 sm:grid-cols-3">
            <label class="flex items-center gap-2 text-xs"><Switch bind:checked={form.serverEnabled} aria-label={$translate("controlPlane.lmcacheSettings.serverEnabled")} /><span>{$translate("controlPlane.lmcacheSettings.serverEnabled")}</span></label>
            <label class="flex items-center gap-2 text-xs"><Switch bind:checked={form.autoStart} aria-label={$translate("controlPlane.lmcacheSettings.autoStart")} /><span>{$translate("controlPlane.lmcacheSettings.autoStart")}</span></label>
            <label class="flex items-center gap-2 text-xs"><Switch bind:checked={form.autoStop} aria-label={$translate("controlPlane.lmcacheSettings.autoStop")} /><span>{$translate("controlPlane.lmcacheSettings.autoStop")}</span></label>
          </div>
          <div class="grid gap-3 sm:grid-cols-2">
            <label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.evictionPolicy")}</span><select bind:value={form.evictionPolicy} class="h-8 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:ring-3"><option value="LRU">LRU</option><option value="IsolatedLRU">IsolatedLRU</option><option value="noop">noop</option></select></label>
            <div class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.chunkSize")}</span><div class="flex items-center gap-2"><label class="flex shrink-0 items-center gap-2"><Switch bind:checked={form.chunkSizeAuto} aria-label={$translate("controlPlane.lmcacheSettings.chunkSizeAuto")} /><span class="whitespace-nowrap">{$translate("controlPlane.lmcacheSettings.chunkSizeAuto")}</span></label><Input bind:value={form.chunkSize} type="number" min="1" disabled={form.chunkSizeAuto} /></div></div>
          </div>
          {#if recognizedChunkSize > 0}<p class="text-xs text-muted-foreground" role="status">{$translate("controlPlane.lmcacheSettings.chunkSizeRecognized", { tokens: recognizedChunkSize })}</p>{/if}
          <div class="grid gap-3 border-t pt-3 sm:grid-cols-2">
            <div class="grid gap-3 rounded-md bg-muted/20 p-3"><div class="text-xs font-medium">{$translate("controlPlane.lmcacheSettings.l2Title")}</div><label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.poolMaxBytes")}</span><Input bind:value={form.poolMaxBytes} placeholder={$translate("controlPlane.lmcacheSettings.l2MaxBytesPlaceholder")} /></label></div>
            <div class="grid gap-3 rounded-md bg-muted/20 p-3"><div class="flex items-center gap-2 text-xs font-medium"><Switch bind:checked={form.l3Enabled} aria-label={$translate("controlPlane.lmcacheSettings.l3Enabled")} />{$translate("controlPlane.lmcacheSettings.l3Title")}</div><label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.path")}</span><Input bind:value={form.l3Path} disabled={!form.l3Enabled} /></label><label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.maxBytes")}</span><Input bind:value={form.l3MaxBytes} disabled={!form.l3Enabled} placeholder={$translate("controlPlane.lmcacheSettings.l3MaxBytesPlaceholder")} /></label></div>
          </div>
        </section>

        <section class="grid gap-3 rounded-lg border p-3" aria-labelledby="lmcache-settings-lifecycle">
          <div class="flex flex-wrap items-center justify-between gap-2"><h3 id="lmcache-settings-lifecycle" class="text-sm font-semibold">{$translate("controlPlane.lmcacheSettings.lifecycleTitle")}</h3><Button variant="outline" size="sm" disabled={candidatesLoading || busy} onclick={() => void checkLifecycle()}><RefreshCw data-icon="inline-start" class={candidatesLoading || busy ? "animate-spin" : ""} />{$translate("controlPlane.lmcacheCheck")}</Button></div>
          <div class="grid gap-3 sm:grid-cols-4"><div><span class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheCurrentVersion")}</span><p class="mt-1 font-mono text-sm">{status?.update?.current || "—"}</p></div><div><span class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheAvailableVersion")}</span><p class="mt-1 font-mono text-sm">{status?.update?.available || "—"}</p></div><div><span class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheStagedVersion")}</span><p class="mt-1 font-mono text-sm">{status?.update?.staged || "—"}</p></div><div><span class="text-xs text-muted-foreground">{$translate("controlPlane.lmcachePreviousVersion")}</span><p class="mt-1 font-mono text-sm">{status?.update?.previous || "—"}</p></div></div>
          <div class="grid gap-3 sm:grid-cols-4">
            <label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcachePolicy")}</span><select bind:value={form.policy} class="h-8 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:ring-3"><option value="disabled">{$translate("controlPlane.lmcachePolicyDisabled")}</option><option value="manual">{$translate("controlPlane.lmcacheSettings.policyManual")}</option><option value="automatic">{$translate("controlPlane.lmcacheSettings.policyAutomatic")}</option><option value="pinned">{$translate("controlPlane.lmcacheSettings.policyPinned")}</option></select></label>
            <label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheChannel")}</span><select bind:value={form.channel} class="h-8 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:ring-3"><option value="stable">stable</option><option value="prerelease">prerelease</option></select></label>
            <label class="grid gap-1 text-xs sm:col-span-2"><span>{$translate("controlPlane.lmcacheTargetVersion")}</span><select bind:value={form.version} class="h-8 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:ring-3"><option value="">{$translate("controlPlane.lmcacheSettings.followChannel")}</option>{#each versionOptions() as version (version)}<option value={version} disabled={isConfiguredOnly(version)}>{version}{isConfiguredOnly(version) ? ` · ${$translate("controlPlane.lmcacheSettings.configured")}` : ""}</option>{/each}</select></label>
          </div>
          <div class="grid gap-3 sm:grid-cols-4"><label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.checkEvery")}</span><Input bind:value={form.checkEvery} placeholder={$translate("controlPlane.lmcacheSettings.checkEveryPlaceholder")} /></label><label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.minIdle")}</span><Input bind:value={form.minIdle} placeholder={$translate("controlPlane.lmcacheSettings.minIdlePlaceholder")} /></label><label class="grid gap-1 text-xs"><span>{$translate("controlPlane.lmcacheSettings.keepVersions")}</span><Input bind:value={form.keepVersions} type="number" min="1" /></label><label class="flex items-center gap-2 self-end pb-1 text-xs"><Switch bind:checked={form.activateOnlyWhenIdle} aria-label={$translate("controlPlane.lmcacheSettings.activateOnlyWhenIdle")} /><span>{$translate("controlPlane.lmcacheSettings.activateOnlyWhenIdle")}</span></label></div>
          <label class="flex items-center gap-2 text-xs"><Switch bind:checked={form.buildWhileBusy} aria-label={$translate("controlPlane.lmcacheSettings.buildWhileBusy")} /><span>{$translate("controlPlane.lmcacheSettings.buildWhileBusy")}</span></label>
          <div class="grid gap-3 rounded-md bg-muted/20 p-3"><div class="grid gap-1 sm:grid-cols-[1fr_minmax(12rem,20rem)] sm:items-center"><div><p class="text-xs font-medium">{$translate("controlPlane.lmcacheSettings.upgradeCandidate")}</p><p class="text-xs text-muted-foreground">{$translate("controlPlane.lmcacheSettings.upgradeCandidateHint")}</p></div><select bind:value={selectedUpgradeVersion} disabled={candidatesLoading || !updateChecked} class="h-8 rounded-lg border border-input bg-transparent px-2 text-sm outline-none focus-visible:ring-3"><option value="">{$translate("controlPlane.lmcacheSettings.useConfiguredTarget")}</option>{#each upgradeOptions() as version (version)}<option value={version} disabled={isConfiguredOnly(version)}>{version}{isConfiguredOnly(version) ? ` · ${$translate("controlPlane.lmcacheSettings.configured")}` : ""}</option>{/each}</select></div><div class="flex flex-wrap gap-2"><Button disabled={busy || !status?.installed || !updateChecked} onclick={() => ask("controlPlane.lmcacheUpgrade", selectedUpgradeVersion ? "controlPlane.lmcacheUpgradeExactConfirm" : "controlPlane.lmcacheUpgradeLatestConfirm", "controlPlane.lmcacheUpgrade", () => runAction("update", selectedUpgradeVersion ? { action: "upgrade", version: selectedUpgradeVersion } : { action: "upgrade" }))}><Play data-icon="inline-start" />{$translate("controlPlane.lmcacheUpgrade")}</Button>{#if status?.update?.staged}<Button variant="outline" disabled={busy || !updateChecked} onclick={() => ask("controlPlane.lmcacheUpdateActivate", "controlPlane.lmcacheActivateConfirm", "controlPlane.lmcacheUpdateActivate", () => runAction("update", { action: "activate", version: status.update.staged }))}><Play data-icon="inline-start" />{$translate("controlPlane.lmcacheUpdateActivate", { version: status.update.staged })}</Button>{/if}{#if status?.update?.previous}<Button variant="outline" disabled={busy} onclick={() => ask("controlPlane.lmcacheUpdateRollback", "controlPlane.lmcacheRollbackConfirm", "controlPlane.lmcacheUpdateRollback", () => runAction("update", { action: "rollback" }))}><RotateCcw data-icon="inline-start" />{$translate("controlPlane.lmcacheUpdateRollback", { version: status.update.previous })}</Button>{/if}</div></div>
          <div class="flex flex-wrap items-center justify-end gap-2 border-t pt-3">{#if status?.update?.pinned}<Button variant="outline" size="sm" disabled={busy} onclick={() => ask("controlPlane.lmcacheUnpin", "controlPlane.lmcacheUnpinConfirm", "controlPlane.lmcacheUnpin", () => onRuntimeAction("unpin"))}><PinOff data-icon="inline-start" />{$translate("controlPlane.lmcacheUnpin")}</Button>{:else if status?.update?.current}<Button variant="outline" size="sm" disabled={busy} onclick={() => ask("controlPlane.lmcachePinCurrent", "controlPlane.lmcachePinCurrentConfirm", "controlPlane.lmcachePinCurrent", () => onRuntimeAction(`pin/${encodeURIComponent(status.update.current || "")}`))}><Pin data-icon="inline-start" />{$translate("controlPlane.lmcachePinCurrent", { version: status.update.current || "" })}</Button>{/if}</div>
          <dl class="grid gap-2 text-xs sm:grid-cols-3"><div><dt class="text-muted-foreground">{$translate("controlPlane.lmcacheLastCheck")}</dt><dd class="mt-1">{displayTimestamp(status?.update?.lastCheck)}</dd></div><div><dt class="text-muted-foreground">{$translate("controlPlane.lmcacheLastUpdate")}</dt><dd class="mt-1">{displayTimestamp(status?.update?.lastUpdate)}</dd></div><div><dt class="text-muted-foreground">{$translate("controlPlane.lmcacheHealthCheckedAt")}</dt><dd class="mt-1">{displayTimestamp(status?.server?.healthCheckedAt)}</dd></div></dl>
        </section>

        <Dialog.Footer><Button variant="outline" onclick={() => (open = false)}>{$translate("common.close")}</Button><Button disabled={saving || busy || !configSnapshot?.writable} onclick={() => void save()}><Save data-icon="inline-start" />{saving ? $translate("controlPlane.lmcacheSettings.saving") : $translate("common.save")}</Button></Dialog.Footer>
      </div>
    {/if}
  </Dialog.Content>
</Dialog.Root>

<ConfirmDialog bind:open={confirmOpen} title={confirmTitle} message={confirmMessage} confirmLabel={confirmLabel} onConfirm={confirm} />
