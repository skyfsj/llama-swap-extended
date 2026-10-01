<script lang="ts">
  import { AlertTriangle, Check, LoaderCircle, Save } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Tabs from "$lib/components/ui/tabs/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "../../lib/apiError";
  import { summarizeChanges, type ConfigChange } from "../../lib/modelConfig";
  import { childSchema, topLevelSchema } from "../../lib/configSchema";
  import { buildServicePatch, draftFromServiceConfig, type ServiceConfigDraft } from "../../lib/serviceConfig";
  import SchemaField from "./SchemaField.svelte";

  interface ConfigSource { path: string; writable: boolean; managed?: boolean; }
  interface ConfigSnapshot {
    config?: Record<string, unknown>;
    yaml: string;
    etag: string;
    writable: boolean;
    restartRequired?: boolean;
    restartPaths?: string[];
    sources?: ConfigSource[];
    ownership?: Record<string, string>;
  }
  interface Validation {
    valid: boolean;
    issues?: { path: string; message: string }[];
    diff?: unknown[];
    restartRequired?: boolean;
    restartPaths?: string[];
  }
  interface Props { snapshot: ConfigSnapshot; onSnapshot: (snapshot: ConfigSnapshot) => void; }

  let { snapshot, onSnapshot }: Props = $props();
  let draft = $state<ServiceConfigDraft>(draftFromServiceConfig({}));
  let activeTab = $state("runtime");
  let saving = $state(false);
  let error = $state("");
  let validation = $state<Validation | null>(null);
  let confirmOpen = $state(false);
  let pendingPatch = $state<unknown[]>([]);
  let pendingChanges = $state<ConfigChange[]>([]);
  let pendingETag = $state("");
  let loadedETag = $state("");

  const inputClass = "border-input bg-background h-9 w-full rounded-md border px-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-60";
  const selectClass = "border-input bg-background h-9 w-full rounded-md border pr-9 pl-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-60";

  $effect(() => {
    if (snapshot.etag !== loadedETag) {
      loadedETag = snapshot.etag;
      draft = draftFromServiceConfig(snapshot.config);
    }
  });

  function ownerForPath(path: string): string {
    const ownership = snapshot.ownership ?? {};
    if (ownership[path]) return ownership[path];
    const ancestors = Object.entries(ownership)
      .filter(([candidate]) => path.startsWith(`${candidate}/`))
      .sort(([left], [right]) => right.length - left.length);
    if (ancestors[0]) return ancestors[0][1];
    const descendants = Object.entries(ownership).find(([candidate]) => candidate.startsWith(`${path}/`));
    if (descendants) return descendants[1];
    return snapshot.sources?.find((source) => source.managed && source.writable)?.path
      ?? snapshot.sources?.find((source) => source.writable)?.path
      ?? "";
  }

  function resolvePatchSource(patch: unknown[]): string {
    const sources = new Set<string>();
    for (const operation of patch) {
      const path = typeof operation === "object" && operation !== null && "path" in operation
        ? String((operation as { path: unknown }).path)
        : "";
      const owner = ownerForPath(path);
      if (owner) sources.add(owner);
    }
    if (sources.size > 1) throw new Error($translate("controlPlane.serviceMixedSources"));
    return [...sources][0] ?? "";
  }

  function changeLabel(path: string): string {
    const labels: Record<string, string> = {
      healthCheckTimeout: "controlPlane.serviceHealthTimeout",
      startPort: "controlPlane.serviceStartPort",
      globalTTL: "controlPlane.serviceGlobalTTL",
      unloadTimeout: "controlPlane.serviceUnloadTimeout",
      sendLoadingState: "controlPlane.serviceLoadingState",
      includeAliasesInList: "controlPlane.serviceAliasList",
      logLevel: "controlPlane.serviceLogLevel",
      logToStdout: "controlPlane.serviceLogStdout",
      logRequests: "controlPlane.serviceLogRequests",
      logTimeFormat: "controlPlane.serviceLogTime",
      metricsMaxInMemory: "controlPlane.serviceMetricsMemory",
      session_id: "controlPlane.serviceSessionHeaders",
    };
    if (path.includes("/store")) return $translate("controlPlane.serviceStorePath");
    if (path.includes("/performance")) return $translate("controlPlane.servicePerformance");
    const key = path.split(/[./]/).filter(Boolean).pop() ?? path;
    return labels[key] ? $translate(labels[key]) : key;
  }

  function resetPending(): void {
    pendingPatch = [];
    pendingChanges = [];
    pendingETag = "";
  }

  function prepareSave(): void {
    if (!snapshot.writable || saving) return;
    error = "";
    validation = null;
    try {
      const patch = buildServicePatch(snapshot.config, draft);
      if (patch.length === 0) {
        error = $translate("controlPlane.serviceNoChanges");
        return;
      }
      resolvePatchSource(patch);
      pendingPatch = patch;
      pendingChanges = patch.flatMap((operation) => {
        if (typeof operation !== "object" || operation === null || !("path" in operation)) return [];
        const item = operation as { path: string; value?: unknown; op?: string };
        const key = item.path.replace(/^\//, "");
        const before = snapshot.config?.[key];
        return summarizeChanges(before, item.op === "remove" ? undefined : item.value, item.path);
      });
      pendingETag = snapshot.etag;
      void validatePatch(patch);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    }
  }

  async function validatePatch(patch: unknown[]): Promise<void> {
    saving = true;
    try {
      const response = await fetch("/api/config/validate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      const payload = await response.json().catch(() => ({})) as Validation;
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      validation = payload;
      if (!payload.valid) { resetPending(); return; }
      confirmOpen = true;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
      resetPending();
    } finally {
      saving = false;
    }
  }

  function cancelSave(): void {
    if (saving) return;
    confirmOpen = false;
    validation = null;
    resetPending();
  }

  async function confirmSave(): Promise<void> {
    if (pendingPatch.length === 0 || !pendingETag || saving) return;
    saving = true;
    error = "";
    try {
      const response = await fetch("/api/config", {
        method: "PATCH",
        headers: { "Content-Type": "application/json", "If-Match": pendingETag },
        body: JSON.stringify(pendingPatch),
      });
      const payload: unknown = await response.json().catch(() => ({}));
      if (response.status === 412) throw new Error($translate("controlPlane.conflict"));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      onSnapshot(payload as ConfigSnapshot);
      confirmOpen = false;
      validation = null;
      resetPending();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
      confirmOpen = false;
      resetPending();
    } finally {
      saving = false;
    }
  }

</script>

<section aria-label={$translate("controlPlane.serviceSettingsTitle")}>
  <form class="border-border/70 rounded-md border p-4" onsubmit={(event) => { event.preventDefault(); prepareSave(); }}>
    <fieldset disabled={saving || confirmOpen || !snapshot.writable}>
      <Tabs.Root bind:value={activeTab}>
        <Tabs.List class="mb-4 grid h-auto w-full grid-cols-3 gap-1">
          <Tabs.Trigger value="runtime">{$translate("controlPlane.serviceRuntimeTab")}</Tabs.Trigger>
          <Tabs.Trigger value="logging">{$translate("controlPlane.serviceLoggingTab")}</Tabs.Trigger>
          <Tabs.Trigger value="storage">{$translate("controlPlane.serviceStorageTab")}</Tabs.Trigger>
        </Tabs.List>

        <Tabs.Content value="runtime" class="mt-0 grid gap-5">
          <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <label class="grid gap-1.5 text-sm" for="service-health-timeout"><span class="font-medium">{$translate("controlPlane.serviceHealthTimeout")}</span><Input id="service-health-timeout" class={inputClass} type="number" min="15" bind:value={draft.healthCheckTimeout} /></label>
            <label class="grid gap-1.5 text-sm" for="service-start-port"><span class="font-medium">{$translate("controlPlane.serviceStartPort")}</span><Input id="service-start-port" class={inputClass} type="number" min="1" max="65535" bind:value={draft.startPort} /></label>
            <label class="grid gap-1.5 text-sm" for="service-global-ttl"><span class="font-medium">{$translate("controlPlane.serviceGlobalTTL")}</span><Input id="service-global-ttl" class={inputClass} type="number" min="0" bind:value={draft.globalTTL} /></label>
            <label class="grid gap-1.5 text-sm" for="service-unload-timeout"><span class="font-medium">{$translate("controlPlane.serviceUnloadTimeout")}</span><Input id="service-unload-timeout" class={inputClass} type="number" min="0" bind:value={draft.unloadTimeout} /></label>
          </div>
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="grid gap-1.5 text-sm" for="service-loading-state"><span class="font-medium">{$translate("controlPlane.serviceLoadingState")}</span><select id="service-loading-state" class={selectClass} bind:value={draft.sendLoadingState}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="true">{$translate("common.yes")}</option><option value="false">{$translate("common.no")}</option></select></label>
            <label class="grid gap-1.5 text-sm" for="service-alias-list"><span class="font-medium">{$translate("controlPlane.serviceAliasList")}</span><select id="service-alias-list" class={selectClass} bind:value={draft.includeAliasesInList}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="true">{$translate("common.yes")}</option><option value="false">{$translate("common.no")}</option></select></label>
          </div>
          <div class="border-border/70 bg-muted/20 grid gap-1.5 rounded-lg border p-3">
            <div class="flex items-center justify-between gap-3">
              <span class="text-sm font-medium">{$translate("controlPlane.serviceRollbackOnFailure")}</span>
              <Switch.Root id="service-rollback-on-failure" checked={draft.rollbackOnModelStartFailure} onCheckedChange={(next) => (draft.rollbackOnModelStartFailure = next)} aria-label={$translate("controlPlane.serviceRollbackOnFailure")} />
            </div>
            <p class="text-muted-foreground text-xs">
              {$translate(draft.rollbackOnModelStartFailure
                ? "controlPlane.serviceRollbackOnFailureOnHint"
                : "controlPlane.serviceRollbackOnFailureOffHint")}
            </p>
          </div>
        </Tabs.Content>

        <Tabs.Content value="logging" class="mt-0 grid gap-5">
          <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-4">
            <label class="grid gap-1.5 text-sm" for="service-log-level"><span class="font-medium">{$translate("controlPlane.serviceLogLevel")}</span><select id="service-log-level" class={selectClass} bind:value={draft.logLevel}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="debug">debug</option><option value="info">info</option><option value="warn">warn</option><option value="error">error</option></select></label>
            <label class="grid gap-1.5 text-sm" for="service-log-stdout"><span class="font-medium">{$translate("controlPlane.serviceLogStdout")}</span><select id="service-log-stdout" class={selectClass} bind:value={draft.logToStdout}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="proxy">proxy</option><option value="upstream">upstream</option><option value="both">both</option><option value="none">none</option></select></label>
            <label class="grid gap-1.5 text-sm" for="service-log-requests"><span class="font-medium">{$translate("controlPlane.serviceLogRequests")}</span><select id="service-log-requests" class={selectClass} bind:value={draft.logRequests}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="true">{$translate("common.yes")}</option><option value="false">{$translate("common.no")}</option></select></label>
            <label class="grid gap-1.5 text-sm" for="service-log-time"><span class="font-medium">{$translate("controlPlane.serviceLogTime")}</span><select id="service-log-time" class={selectClass} bind:value={draft.logTimeFormat}><option value="__inherit__">{$translate("controlPlane.modelInherit")}</option><option value="">{$translate("controlPlane.serviceLogTimeDisabled")}</option><option value="rfc3339">rfc3339</option><option value="rfc3339nano">rfc3339nano</option><option value="unixdate">unixdate</option><option value="stampmilli">stampmilli</option></select></label>
          </div>
        </Tabs.Content>

        <Tabs.Content value="storage" class="mt-0 grid gap-5">
          <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <label class="grid gap-1.5 text-sm" for="service-store"><span class="font-medium">{$translate("controlPlane.serviceStorePath")}</span><Input id="service-store" class={`${inputClass} font-mono text-xs`} bind:value={draft.storePath} placeholder="/var/lib/llama-swap/activity.db" /></label>
            <label class="grid gap-1.5 text-sm" for="service-metrics"><span class="font-medium">{$translate("controlPlane.serviceMetricsMemory")}</span><Input id="service-metrics" class={inputClass} type="number" min="0" bind:value={draft.metricsMaxInMemory} /></label>
          </div>
          <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
            <label class="grid gap-1.5 text-sm" for="service-performance-disabled"><span class="font-medium">{$translate("controlPlane.servicePerformance")}</span><select id="service-performance-disabled" class={selectClass} bind:value={draft.performanceDisabled}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="false">{$translate("controlPlane.servicePerformanceEnabled")}</option><option value="true">{$translate("controlPlane.servicePerformanceDisabled")}</option></select></label>
            <label class="grid gap-1.5 text-sm" for="service-performance-every"><span class="font-medium">{$translate("controlPlane.servicePerformanceEvery")}</span><Input id="service-performance-every" class={inputClass} bind:value={draft.performanceEvery} placeholder="5s" /></label>
            <div class="grid gap-2">
              <div class="flex min-h-9 items-center justify-between gap-3"><span class="text-sm font-medium">{$translate("controlPlane.serviceSessionHeaders")}</span><Switch.Root checked={draft.uiSessionHeadersEnabled} onCheckedChange={(checked) => (draft.uiSessionHeadersEnabled = checked)} /></div>
              {#if draft.uiSessionHeadersEnabled}<SchemaField schema={childSchema(childSchema(topLevelSchema("ui"), "activity"), "session_id")} value={draft.uiSessionHeaders} label="" onChange={(next) => { if (Array.isArray(next) && next.every((item) => typeof item === "string")) draft.uiSessionHeaders = next as string[]; }} />{/if}
            </div>
          </div>
        </Tabs.Content>
      </Tabs.Root>
    </fieldset>

    {#if error}<div class="border-destructive/40 bg-destructive/10 mt-4 rounded-md border p-3 text-sm" role="alert">{error}</div>{/if}
    {#if validation && !validation.valid}
      <div class="border-destructive/40 bg-destructive/10 mt-4 rounded-md border p-3 text-xs" role="alert">
        {#each validation.issues ?? [] as issue}<div>{issue.path}: {issue.message}</div>{/each}
      </div>
    {/if}
    <div class="border-border/70 mt-5 flex justify-end border-t pt-4">
      <Button type="submit" disabled={saving || !snapshot.writable}>{#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{:else}<Save class="size-4" aria-hidden="true" />{/if}{$translate("controlPlane.serviceSave")}</Button>
    </div>
  </form>
</section>

<Dialog.Root bind:open={confirmOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-2xl overflow-y-auto sm:max-w-2xl" showCloseButton={!saving}>
    <Dialog.Header><Dialog.Title class="flex items-center gap-2"><Check class="text-success size-4" aria-hidden="true" />{$translate("controlPlane.configDiffTitle")}</Dialog.Title><Dialog.Description>{$translate("controlPlane.configDiffDescription")}</Dialog.Description></Dialog.Header>
    {#if validation?.restartRequired}<p class="border-warning/30 bg-warning/10 text-warning rounded-md border px-3 py-2 text-sm"><AlertTriangle class="mr-1 inline size-4" aria-hidden="true" />{$translate("controlPlane.restartRequired")}</p>{/if}
    <div class="border-border/70 divide-border/70 overflow-hidden rounded-md border">
      {#each pendingChanges as change (change.path)}
        <div class="grid gap-2 border-b p-3 last:border-b-0 sm:grid-cols-[minmax(0,0.8fr)_minmax(0,1.2fr)]"><span class="min-w-0 text-sm font-medium">{changeLabel(change.path)}</span><div class="grid min-w-0 gap-1 text-xs sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)]"><span class="text-muted-foreground truncate">{change.kind === "added" ? "" : String(change.before ?? "")}</span>{#if change.kind === "changed"}<span class="text-muted-foreground">→</span>{/if}<span class="truncate">{change.kind === "removed" ? "" : String(change.after ?? "")}</span></div></div>
      {/each}
    </div>
    <Dialog.Footer><Button variant="outline" onclick={cancelSave} disabled={saving}>{$translate("common.cancel")}</Button><Button onclick={() => void confirmSave()} disabled={saving || pendingPatch.length === 0}>{#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}{$translate("controlPlane.configDiffSave")}</Button></Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
