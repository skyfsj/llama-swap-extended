<script lang="ts">
  import { Download, LoaderCircle, RefreshCw } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { translate } from "$lib/i18n";
  import type { RuntimeVersionCandidate } from "$lib/types";

  interface Props {
    open?: boolean;
    runtimeName: string;
    candidates: RuntimeVersionCandidate[];
    selectedVersion: string;
    loading?: boolean;
    busy?: boolean;
    error?: string;
    onRefresh: () => void;
    onStage: (candidate: RuntimeVersionCandidate) => void;
  }

  let { open = $bindable(false), runtimeName, candidates, selectedVersion = $bindable(""), loading = false, busy = false, error = "", onRefresh, onStage }: Props = $props();
  let selected = $derived(candidates.find((candidate) => candidate.version === selectedVersion) ?? null);
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="max-h-[calc(100dvh-2rem)] max-w-xl overflow-y-auto">
    <Dialog.Header>
      <Dialog.Title>{$translate("controlPlane.runtimeCenter.versionDialog.title")}</Dialog.Title>
      <Dialog.Description>{$translate("controlPlane.runtimeCenter.versionDialog.description", { name: runtimeName })}</Dialog.Description>
    </Dialog.Header>
    <div class="grid gap-3">
      <div class="flex items-center justify-between gap-2"><label class="grid min-w-0 flex-1 gap-1 text-sm" for="runtime-version-select"><span class="font-medium">{$translate("controlPlane.runtimeCenter.versionDialog.select")}</span>{#if loading && candidates.length === 0}<span class="flex h-9 items-center gap-2 rounded-lg border px-3 text-xs text-muted-foreground"><LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{$translate("controlPlane.runtimeCenter.versionDialog.loading")}</span>{:else}<select disabled={busy || loading} id="runtime-version-select" class="h-9 min-w-0 w-full rounded-lg border border-input bg-background px-3 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" bind:value={selectedVersion}><option value="">{$translate("controlPlane.runtimeCenter.versionDialog.placeholder")}</option>{#each candidates as candidate (candidate.version)}<option value={candidate.version}>{candidate.label && candidate.label !== candidate.version ? `${candidate.label} · ` : ""}{candidate.version}{candidate.recommended ? ` · ${$translate("controlPlane.runtimeCenter.versionDialog.recommended")}` : ""}{candidate.installed ? ` · ${$translate("controlPlane.runtimeCenter.versionDialog.installed")}` : ""}</option>{/each}</select>{/if}</label><Button variant="ghost" size="sm" disabled={loading || busy} onclick={onRefresh}><RefreshCw data-icon="inline-start" class={loading ? "animate-spin" : ""} />{$translate("common.refresh")}</Button></div>
      {#if candidates.length === 0 && !loading}<p class="rounded-md border border-dashed p-3 text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.versionDialog.empty")}</p>{/if}
      {#if selected?.commit}<p class="rounded-md border bg-muted/20 p-2 text-xs text-muted-foreground">{$translate("controlPlane.runtimeCommit")}: <code class="break-all">{selected.commit}</code></p>{/if}
      {#if selected?.digest}<p class="rounded-md border bg-muted/20 p-2 text-xs text-muted-foreground">{$translate("controlPlane.runtimeDigest")}: <code class="break-all">{selected.digest}</code></p>{/if}
      {#if error}<p class="text-xs text-destructive" role="alert">{error}</p>{/if}
    </div>
    <Dialog.Footer><Button variant="outline" onclick={() => (open = false)}>{$translate("common.cancel")}</Button><Button disabled={busy || loading || !!error || !selected} onclick={() => selected && onStage(selected)}>{#if busy}<LoaderCircle class="animate-spin" aria-hidden="true" />{:else}<Download data-icon="inline-start" />{/if}{$translate("controlPlane.runtimeCenter.actions.stage")}</Button></Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
