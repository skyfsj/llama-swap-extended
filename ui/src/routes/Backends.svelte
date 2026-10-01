<script lang="ts">
  import { onMount } from "svelte";
  import ConfirmDialog from "../components/ConfirmDialog.svelte";
  import { translate } from "../lib/i18n";
  import { errorMessageFromPayload } from "../lib/apiError";
  import { formatFileSize } from "../lib/format";
  import { backendProgress } from "../stores/api";
  import type { BackendStatus, ResourceStatus } from "../lib/types";

  let backends = $state<BackendStatus[]>([]);
  let resources = $state<ResourceStatus | null>(null);
  let loading = $state(true);
  let error = $state("");
  let busy = $state("");
  let pollTimer: ReturnType<typeof setInterval> | null = null;

  function capabilityEntries(value: BackendStatus): Array<[string, boolean]> {
    return Object.entries(value.capabilities ?? {}).sort(([left], [right]) => left.localeCompare(right));
  }

  function progressPercent(model: string): number {
    const value = $backendProgress[model]?.progress ?? 0;
    return Math.round(Math.max(0, Math.min(1, value)) * 100);
  }

  function progressBytes(completed = 0, total = 0): string {
    if (total > 0) return `${formatFileSize(Math.max(0, completed))} / ${formatFileSize(total)}`;
    if (total === -1 && completed > 0) return formatFileSize(Math.max(0, completed));
    return "";
  }

  function errorMessage(cause: unknown): string {
    return cause instanceof Error ? cause.message : String(cause);
  }

  async function load(): Promise<void> {
    loading = true;
    error = "";
    try {
      const response = await fetch("/api/backends");
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      backends = Array.isArray(payload?.data) ? payload.data : [];
      try {
        const resourceResponse = await fetch("/api/resources");
        const resourcePayload = await resourceResponse.json().catch(() => ({}));
        resources = resourceResponse.ok && resourcePayload ? resourcePayload as ResourceStatus : null;
      } catch {
        resources = null;
      }
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      loading = false;
    }
  }

  // Destructive cache operations confirm through the shared modal before
  // firing: `action` records the pending operation, `performAction` runs it.
  let confirmOpen = $state(false);
  let confirmTitle = $state("");
  let confirmMessage = $state("");
  let confirmLabel = $state("");
  let confirmRun = $state<(() => Promise<void>) | null>(null);

  function action(model: string, path: string, titleKey: string, confirmationKey: string): void {
    confirmTitle = $translate(titleKey);
    confirmMessage = $translate(confirmationKey);
    confirmLabel = $translate(titleKey);
    confirmRun = () => performAction(model, path);
    confirmOpen = true;
  }

  function runConfirmAction(): void {
    const run = confirmRun;
    confirmOpen = false;
    confirmRun = null;
    if (run) void run();
  }

  async function performAction(model: string, path: string): Promise<void> {
    busy = `${model}:${path}`;
    error = "";
    try {
      const response = await fetch(`/api/backends/${encodeURIComponent(model)}/${path}`, { method: "POST" });
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      await load();
    } catch (cause) {
      error = errorMessage(cause);
    } finally {
      busy = "";
    }
  }

  onMount(() => {
    void load();
    pollTimer = setInterval(() => void load(), 10000);
    return () => {
      if (pollTimer) clearInterval(pollTimer);
      pollTimer = null;
    };
  });
</script>

<section class="space-y-4" aria-labelledby="backends-title">
  <div class="flex flex-wrap items-start justify-between gap-3">
    <div>
      <h3 id="backends-title" class="text-lg font-semibold">{$translate("controlPlane.backendTitle")}</h3>
      <p class="text-muted-foreground text-sm">{$translate("controlPlane.backendDescription")}</p>
    </div>
    <button class="rounded-md border px-3 py-2 text-sm hover:bg-muted" type="button" onclick={() => void load()} disabled={loading}>
      {$translate("controlPlane.refresh")}
    </button>
  </div>

  {#if error}
    <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
      {$translate("controlPlane.error", { message: error })}
    </div>
  {/if}

  {#if loading}
    <p class="text-muted-foreground text-sm" aria-live="polite">{$translate("controlPlane.loading")}</p>
  {:else if backends.length === 0}
    <div class="rounded-lg border border-dashed p-6 text-sm text-muted-foreground">{$translate("controlPlane.empty")}</div>
  {:else}
    {#if resources?.enabled && resources.budget}
      <div class="rounded-lg border p-4 text-sm" aria-label={$translate("controlPlane.resourceBudget")}>
        <div class="flex flex-wrap justify-between gap-2"><span class="font-medium">{$translate("controlPlane.resourceBudget")}</span><span class="text-muted-foreground">{$translate("controlPlane.resourceUsage")}: {resources.usageVRAMMiB ?? 0}/{resources.budget.vramMiB} MiB VRAM · {resources.usageRAMMiB ?? 0}/{resources.budget.ramMiB} MiB RAM</span></div>
        {#if resources.evictionCandidates?.length}<p class="mt-2 text-xs text-muted-foreground">{$translate("controlPlane.resourceCandidates")}: {resources.evictionCandidates.join(", ")}</p>{/if}
      </div>
    {/if}
    <div class="grid gap-4 xl:grid-cols-2">
      {#each backends as backend (backend.model)}
        {@const progress = $backendProgress[backend.model]}
        {@const sleeping = backend.cache?.sleeping === true || backend.cacheState === "sleeping"}
        <article class="rounded-lg border p-4">
          <div class="flex flex-wrap items-start justify-between gap-3">
            <div>
              <h4 class="font-medium">{backend.model}</h4>
              <p class="text-muted-foreground text-xs">
                {backend.type || "—"}{#if backend.runtime} · {backend.runtime}{/if}{#if backend.protocol} · {backend.protocol}{/if}
              </p>
            </div>
            <span class="rounded-full bg-muted px-2 py-1 text-xs">{sleeping ? $translate("status.model.sleeping") : (backend.cacheState ?? "unknown")}</span>
          </div>

          <dl class="mt-4 grid gap-x-4 gap-y-2 text-sm sm:grid-cols-2">
            <div><dt class="text-muted-foreground">{$translate("controlPlane.backendCapabilities")}</dt><dd class="mt-1 flex flex-wrap gap-1">
              {#if capabilityEntries(backend).length === 0}<span>—</span>{:else}{#each capabilityEntries(backend) as [name, enabled] (name)}<span class={`rounded px-1.5 py-0.5 text-xs ${enabled ? "bg-success/15 text-success" : "bg-muted text-muted-foreground"}`}>{name}</span>{/each}{/if}
            </dd></div>
            <div><dt class="text-muted-foreground">{$translate("controlPlane.backendCache")}</dt><dd class="mt-1">{#if backend.cacheReport}{backend.cacheReport.cachedTokens} {$translate("controlPlane.backendCacheRead")}{#if backend.cacheReport.creationTokens > 0} · {backend.cacheReport.creationTokens} {$translate("controlPlane.backendCacheCreation")}{/if}{#if backend.cacheReport.hit} · {$translate("controlPlane.backendCacheHit")}{/if}{:else}—{/if}</dd></div>
            <div><dt class="text-muted-foreground">{$translate("controlPlane.backendDiscovery")}</dt><dd class="mt-1">{backend.discovery?.version || backend.discovery?.type || "—"}{#if backend.discovery?.error}<div class="text-xs text-destructive">{backend.discovery.error}</div>{/if}</dd></div>
            <div><dt class="text-muted-foreground">{$translate("controlPlane.backendServerInfo")}</dt><dd class="mt-1">{#if backend.discovery?.serverInfo}{Object.entries(backend.discovery.serverInfo).filter(([, value]) => value !== undefined && value !== null && value !== "").map(([key, value]) => `${key}: ${value}`).join(" · ") || "—"}{:else}—{/if}</dd></div>
            <div><dt class="text-muted-foreground">{$translate("controlPlane.backendResources")}</dt><dd class="mt-1">{#if backend.resources}{Object.entries(backend.resources).filter(([, value]) => value !== undefined && value !== null && value !== "" && (!Array.isArray(value) || value.length > 0)).map(([key, value]) => `${key}: ${Array.isArray(value) ? value.join(",") : value}`).join(" · ") || "—"}{:else}—{/if}</dd></div>
          </dl>

          {#if progress}
            <div class="mt-4" aria-live="polite">
              <div class="flex justify-between text-xs text-muted-foreground"><span>{$translate("controlPlane.progress")}: {progress.phase}</span><span>{progressPercent(backend.model)}%</span></div>
              <div class="mt-1 h-1.5 overflow-hidden rounded-full bg-muted"><div class="h-full bg-primary transition-[width]" style={`width: ${progressPercent(backend.model)}%`}></div></div>
              {#if progressBytes(progress.completed, progress.total)}<div class="mt-1 text-xs text-muted-foreground">{progressBytes(progress.completed, progress.total)}</div>{/if}
              {#if progress.message}<p class="mt-1 text-xs text-muted-foreground">{progress.message}</p>{/if}
              {#if progress.error}<p class="mt-1 text-xs text-destructive">{progress.error}</p>{/if}
            </div>
          {/if}

          <div class="mt-4 flex flex-wrap gap-2">
            <button class="rounded-md border px-2 py-1 text-xs hover:bg-muted" type="button" disabled={busy !== "" || sleeping} onclick={() => action(backend.model, "sleep", "controlPlane.backendSleep", "controlPlane.backendSleepConfirm")}>{$translate("controlPlane.backendSleep")}</button>
            <button class="rounded-md border px-2 py-1 text-xs hover:bg-muted" type="button" disabled={busy !== "" || !sleeping} onclick={() => action(backend.model, "wake", "controlPlane.backendWake", "controlPlane.backendWakeConfirm")}>{$translate("controlPlane.backendWake")}</button>
            <button class="rounded-md border border-warning/50 px-2 py-1 text-xs hover:bg-warning/10" type="button" disabled={busy !== ""} onclick={() => action(backend.model, "cache/reset", "controlPlane.backendResetCache", "controlPlane.backendResetConfirm")}>{$translate("controlPlane.backendResetCache")}</button>
          </div>
        </article>
      {/each}
    </div>
  {/if}

  <ConfirmDialog
    bind:open={confirmOpen}
    title={confirmTitle}
    message={confirmMessage}
    confirmLabel={confirmLabel}
    onConfirm={runConfirmAction}
  />
</section>
