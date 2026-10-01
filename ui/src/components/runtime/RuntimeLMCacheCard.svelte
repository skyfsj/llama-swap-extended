<script lang="ts">
  import { Database, LoaderCircle, Play, RefreshCw, Settings2, Square, Terminal } from "@lucide/svelte";
  import * as Card from "$lib/components/ui/card/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "$lib/i18n";
  import type { BackendProgressEvent, LMCacheDashboardResponse, LMCacheStatus } from "$lib/types";
  import Tag from "../Tag.svelte";
  import LMCacheDashboard from "./LMCacheDashboard.svelte";

  interface Props {
    status: LMCacheStatus | null;
    dashboard: LMCacheDashboardResponse | null;
    dashboardLoading?: boolean;
    dashboardError?: string;
    progress?: BackendProgressEvent;
    error?: string;
    busy?: boolean;
    onEnable: () => void;
    onStart: () => void;
    onStop: () => void;
    onSettings: () => void;
    onLogs: () => void;
    onRefresh: () => void;
  }
  let { status, dashboard, dashboardLoading = false, dashboardError = "", progress, error = "", busy = false, onEnable, onStart, onStop, onSettings, onLogs, onRefresh }: Props = $props();
  let healthy = $derived(Boolean(status?.server?.running && status.server.state === "RUNNING" && status.server.healthy));
  let transitioning = $derived(Boolean(status?.installing || ["STARTING", "STOPPING", "UPDATING"].includes(status?.server?.state ?? "")));
  let inUse = $derived(Boolean(status?.usingModels?.length));
  let percentage = $derived(Math.round(Math.max(0, Math.min(1, progress?.progress ?? 0)) * 100));
  function stateLabel(): string {
    if (!status) return $translate(error ? "common.error" : "controlPlane.loading");
    if (status.installed && status.server?.state === "NOT_INSTALLED") return $translate("controlPlane.lmcacheServerStopped");
    if (status.server?.state === "RUNNING" && !healthy) return $translate("controlPlane.runtimeCenter.cache.unhealthy");
    return $translate(`controlPlane.lmcacheState.${status.server?.state || "NOT_INSTALLED"}`);
  }
</script>

<Card.Root class="shrink-0 gap-0 overflow-hidden py-0" aria-labelledby="lmcache-title" aria-busy={busy}>
  <Card.Header class="shrink-0 gap-3 border-b px-4 py-3">
    <div class="flex w-full flex-wrap items-center gap-3">
      <div class="flex size-9 shrink-0 items-center justify-center rounded-lg border bg-muted/30"><Database class="size-4 text-muted-foreground" aria-hidden="true" /></div>
      <div class="min-w-0 flex-1">
        <div class="flex flex-wrap items-center gap-2">
          <Card.Title id="lmcache-title" class="text-sm">{$translate("controlPlane.lmcacheTitle")}</Card.Title>
          <Tag class={error || status?.server?.state === "ERROR" ? "bg-destructive/10 text-destructive" : healthy ? "bg-success/10 text-success" : ""}>{stateLabel()}</Tag>
        </div>
        <p class="mt-1 text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.cache.description")}</p>
      </div>
      <div class="flex flex-wrap items-center gap-2">
        {#if status}
          <Button variant="outline" size="sm" onclick={onLogs}><Terminal aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.logs")}</Button>
          <Button variant="outline" size="sm" disabled={busy} onclick={onSettings}><Settings2 aria-hidden="true" />{$translate("controlPlane.lmcacheSettings.open")}</Button>
          {#if !status.installed}<Button size="sm" disabled={busy || transitioning || !!error} onclick={onEnable}><Play aria-hidden="true" />{$translate("controlPlane.lmcacheEnable")}</Button>
          {:else if status.server?.running}<Button variant="outline" size="sm" disabled={busy || transitioning || inUse || !!error} title={inUse ? $translate("controlPlane.runtimeCenter.cache.inUse", { models: status.usingModels.join(", ") }) : undefined} onclick={onStop}><Square aria-hidden="true" />{$translate("controlPlane.lmcacheServerStop")}</Button>
          {:else}<Button variant="outline" size="sm" disabled={busy || transitioning || !!error} onclick={onStart}><Play aria-hidden="true" />{$translate("controlPlane.lmcacheServerStart")}</Button>{/if}
        {/if}
        {#if busy || transitioning}<LoaderCircle class="size-4 animate-spin text-muted-foreground" aria-label={$translate("controlPlane.loading")} />{/if}
      </div>
    </div>
  </Card.Header>
  <Card.Content class="p-0">
    {#if error}
      <div class="mx-4 my-4 flex flex-wrap items-center justify-between gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive" role="alert"><p class="min-w-0 break-words [overflow-wrap:anywhere]">{error}</p><Button variant="outline" size="sm" disabled={busy} onclick={onRefresh}><RefreshCw aria-hidden="true" />{$translate("common.refresh")}</Button></div>
    {:else if !status}
      <p class="px-4 py-4 text-sm text-muted-foreground" role="status">{$translate("controlPlane.loading")}</p>
    {/if}
    {#if status}
      {#if status.installed}
        <dl class="grid grid-cols-2 gap-4 bg-muted/10 px-4 py-4 text-sm sm:grid-cols-4">
          <div class="min-w-0"><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.facts.current")}</dt><dd class="mt-1.5 break-all font-mono">{status.update?.current || status.version || "—"}</dd></div>
          <div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.cache.enabledModels")}</dt><dd class="mt-1.5 tabular-nums">{status.enabledModels?.length ?? 0}</dd></div>
          <div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.cache.usingModels")}</dt><dd class="mt-1.5 tabular-nums">{status.usingModels?.length ?? 0}</dd></div>
          <div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.facts.policy")}</dt><dd class="mt-1.5">{status.update?.pinned ? $translate("controlPlane.runtimeCenter.policy.pinned") : status.update?.policy ? $translate(`controlPlane.runtimeCenter.policy.${status.update.policy}`) : "—"}</dd></div>
        </dl>
        {#if inUse}<p class="border-t px-4 py-3 text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.cache.inUse", { models: status.usingModels.join(", ") })}</p>{/if}
        {#if status.server?.lastError}<p class="mx-4 mb-4 break-words rounded-lg bg-destructive/5 p-3 text-xs text-destructive" role="alert">{status.server.lastError}</p>{/if}
        {#if healthy}<div class="border-t p-4"><LMCacheDashboard {dashboard} loading={dashboardLoading} error={dashboardError} /></div>{/if}
      {:else}<p class="px-4 py-4 text-sm text-muted-foreground">{$translate("controlPlane.lmcacheNotInstalled")}</p>{/if}
    {/if}
    {#if progress && (progress.phase !== "idle" || progress.error)}<div class="grid gap-2 border-t px-4 py-3" aria-live="polite"><div class="flex justify-between gap-3 text-xs text-muted-foreground"><span class="min-w-0 break-words">{progress.message || progress.phase}</span><span>{percentage}%</span></div><div class="h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label={$translate("controlPlane.progress")} aria-valuenow={percentage} aria-valuemin={0} aria-valuemax={100}><div class="h-full bg-primary" style={`width: ${percentage}%`}></div></div>{#if progress.error}<p class="break-words text-xs text-destructive" role="alert">{progress.error}</p>{/if}</div>{/if}
  </Card.Content>
</Card.Root>
