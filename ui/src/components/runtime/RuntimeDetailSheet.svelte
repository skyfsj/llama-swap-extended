<script lang="ts">
  import { Download, Pin, PinOff, Play, Trash2 } from "@lucide/svelte";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Sheet from "$lib/components/ui/sheet/index.js";
  import * as Tabs from "$lib/components/ui/tabs/index.js";
  import { translate } from "$lib/i18n";
  import type { RuntimeDetail, RuntimeStatus } from "$lib/types";

  interface Props {
    open?: boolean;
    runtime: RuntimeStatus | null;
    detail?: RuntimeDetail;
    loading?: boolean;
    error?: string;
    actionError?: string;
    onRetry: () => void;
    policy?: string;
    busy?: boolean;
    onStage: () => void;
    onActivate: (version: string) => void;
    onRollback: () => void;
    onPin: (version: string) => void;
    onUnpin: () => void;
    onDelete: (version: string) => void;
  }

  let {
    open = $bindable(false),
    runtime,
    detail,
    loading = false,
    error = "",
    actionError = "",
    onRetry,
    policy = "automatic",
    busy = false,
    onStage,
    onActivate,
    onRollback,
    onPin,
    onUnpin,
    onDelete,
  }: Props = $props();

  let activeTab = $state("overview");
  let lastResetName = $state("");

  $effect(() => {
    if (open && runtime?.name && runtime.name !== lastResetName) {
      lastResetName = runtime.name;
      activeTab = "overview";
    }
    if (!open) lastResetName = "";
  });

  function versions(): Array<[string, NonNullable<RuntimeDetail["versions"]>[string]]> {
    return Object.entries(detail?.versions ?? {}).sort(([left], [right]) => right.localeCompare(left));
  }

  function stateLabel(state: string): string {
    if (state === "DEGRADED") return $translate("controlPlane.runtimeCenter.list.degraded");
    const key = `controlPlane.lmcacheUpdateState.${state}`;
    const localized = $translate(key);
    return localized === key ? state : localized;
  }

  function valueLabel(group: "kinds" | "modes", value: string | undefined, fallback = "—"): string {
    const normalized = String(value ?? "").trim().toLowerCase();
    if (!normalized) return fallback;
    const key = `controlPlane.runtimeCenter.${group}.${normalized}`;
    const localized = $translate(key);
    return localized === key ? value ?? fallback : localized;
  }

  function runtimeError(value: string): string {
    if (/runtime has no \.venv/i.test(value)) return $translate("controlPlane.runtimeCenter.runtimeErrors.noVenv");
    return value;
  }

</script>

{#if runtime}
  <Sheet.Root bind:open>
    <Sheet.Content side="right" class="w-full overflow-y-auto sm:!max-w-2xl">
      <Sheet.Header class="border-b pr-12">
        <Sheet.Title class="flex flex-wrap items-center gap-2">
          {runtime.name}
          <Badge variant={runtime.lastError ? "destructive" : "outline"}>{stateLabel(runtime.state)}</Badge>
        </Sheet.Title>
        <Sheet.Description class="sr-only">{valueLabel("kinds", runtime.kind)} · {valueLabel("modes", runtime.mode || "native")}</Sheet.Description>
      </Sheet.Header>

      <div class="grid min-w-0 gap-4 px-4 pb-8 sm:px-5">
        {#if error}<div class="grid gap-2 rounded-lg border border-destructive/30 bg-destructive/5 p-3 text-sm text-destructive" role="alert"><p class="break-words">{$translate("controlPlane.runtimeCenter.detail.loadError", { message: error })}</p><Button variant="outline" size="sm" class="justify-self-start" onclick={onRetry} disabled={loading}>{$translate("common.refresh")}</Button></div>{/if}
        {#if actionError}<p class="break-words rounded-lg bg-destructive/5 p-3 text-sm text-destructive" role="alert">{actionError}</p>{/if}
        {#if loading && !detail}<p class="text-sm text-muted-foreground" role="status">{$translate("controlPlane.loading")}</p>{/if}
        <Tabs.Root bind:value={activeTab} class="min-w-0">
          <Tabs.List variant="line" class="flex w-full flex-wrap items-stretch gap-1 border-b pb-1">
            <Tabs.Trigger value="overview" class="min-w-[3.75rem] flex-1 whitespace-normal px-1 text-xs leading-4">{$translate("controlPlane.runtimeCenter.tabs.overview")}</Tabs.Trigger>
            <Tabs.Trigger value="versions" class="min-w-[3.75rem] flex-1 whitespace-normal px-1 text-xs leading-4">{$translate("controlPlane.runtimeCenter.tabs.versions")}</Tabs.Trigger>
          </Tabs.List>

          <Tabs.Content value="overview" class="grid gap-3 pt-3">
            <section class="grid gap-3 rounded-lg border p-3" aria-labelledby="runtime-overview-facts">
              <h3 id="runtime-overview-facts" class="text-sm font-semibold">{$translate("controlPlane.runtimeCenter.detail.facts")}</h3>
              <dl class="grid gap-3 text-sm sm:grid-cols-2">
                <div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.facts.current")}</dt><dd class="mt-1 font-mono">{runtime.current || "—"}</dd></div>
                <div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.facts.candidate")}</dt><dd class="mt-1 font-mono">{runtime.staged || runtime.available || "—"}</dd></div>
                <div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.facts.policy")}</dt><dd class="mt-1">{policy ? $translate(`controlPlane.runtimeCenter.policy.${policy}`) : "—"}</dd></div>
                <div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.facts.rollback")}</dt><dd class="mt-1 font-mono">{runtime.previous || $translate("controlPlane.runtimeCenter.runtimeErrors.noPrevious")}</dd></div>
              </dl>
            </section>
            {#if runtime.lastError}<p class="rounded-md border border-destructive/35 bg-destructive/10 p-3 text-sm text-destructive" role="alert">{runtimeError(runtime.lastError)}</p>{/if}
            {#if runtime.previous}<div class="flex flex-wrap gap-2"><Button variant="outline" size="sm" disabled={busy} onclick={onRollback}>{$translate("controlPlane.runtimeCenter.actions.rollback")}</Button></div>{/if}
          </Tabs.Content>

          <Tabs.Content value="versions" class="grid gap-3 pt-4">
            <div class="flex flex-wrap items-center justify-between gap-2">
              <div><h3 class="text-sm font-semibold">{$translate("controlPlane.runtimeCenter.versions.title")}</h3><p class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.versions.description")}</p></div>
              <Button variant="outline" size="sm" disabled={busy} onclick={onStage}><Download data-icon="inline-start" />{$translate("controlPlane.runtimeCenter.actions.selectVersion")}</Button>
            </div>
            {#if loading && !detail}<p class="text-sm text-muted-foreground" role="status">{$translate("controlPlane.loading")}</p>
            {:else if error && !detail}<p class="text-sm text-muted-foreground">{$translate("controlPlane.runtimeCenter.detail.loadError", { message: error })}</p>
            {:else if versions().length === 0}
              <div class="rounded-md border border-dashed p-4 text-sm text-muted-foreground" role="status">{$translate("controlPlane.runtimeCenter.versions.empty")}</div>
            {:else}
              <ul class="grid gap-2">
                {#each versions() as [version, manifest] (version)}
                  <li class="grid gap-3 rounded-lg border p-3">
                    <div class="flex min-w-0 items-start justify-between gap-3">
                      <div class="min-w-0"><code class="break-all text-sm font-semibold">{version}</code><div class="mt-1 break-all text-xs text-muted-foreground">{manifest.source || "—"}</div></div>
                      <div class="flex shrink-0 flex-wrap justify-end gap-1">
                        {#if version === runtime.current}<Badge>{$translate("controlPlane.runtimeCenter.versions.current")}</Badge>{/if}
                        {#if version === runtime.staged}<Badge variant="outline">{$translate("controlPlane.runtimeCenter.versions.staged")}</Badge>{/if}
                        {#if version === runtime.previous}<Badge variant="outline">{$translate("controlPlane.runtimeCenter.versions.previous")}</Badge>{/if}
                        {#if version === runtime.pinned}<Badge variant="secondary">{$translate("controlPlane.runtimeCenter.versions.pinned")}</Badge>{/if}
                      </div>
                    </div>
                    <div class="flex flex-wrap gap-1.5">
                      {#if version !== runtime.current}<Button variant="outline" size="xs" disabled={busy} onclick={() => onActivate(version)}><Play data-icon="inline-start" />{$translate("controlPlane.runtimeCenter.actions.activate")}</Button>{/if}
                      {#if version === runtime.pinned}<Button variant="outline" size="xs" disabled={busy} onclick={onUnpin}><PinOff data-icon="inline-start" />{$translate("controlPlane.runtimeCenter.actions.unpin")}</Button>{:else}<Button variant="outline" size="xs" disabled={busy} onclick={() => onPin(version)}><Pin data-icon="inline-start" />{$translate("controlPlane.runtimeCenter.actions.pin")}</Button>{/if}
                      {#if version !== runtime.current && version !== runtime.previous && version !== runtime.pinned}<Button variant="destructive" size="xs" disabled={busy} onclick={() => onDelete(version)}><Trash2 data-icon="inline-start" />{$translate("controlPlane.runtimeCenter.actions.deleteVersion")}</Button>{/if}
                    </div>
                  </li>
                {/each}
              </ul>
            {/if}
          </Tabs.Content>

        </Tabs.Root>
      </div>
    </Sheet.Content>
  </Sheet.Root>
{/if}
