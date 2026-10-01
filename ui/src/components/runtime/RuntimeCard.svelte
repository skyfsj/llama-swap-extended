<script lang="ts">
  import { CircleAlert, CircleCheck, Download, Ellipsis, LoaderCircle, Pencil, Play, RotateCcw, Terminal } from "@lucide/svelte";
  import { Button, buttonVariants } from "$lib/components/ui/button/index.js";
  import * as DropdownMenu from "$lib/components/ui/dropdown-menu/index.js";
  import { formatFileSize } from "$lib/format";
  import { translate } from "$lib/i18n";
  import { runtimeEnvironmentMissing } from "$lib/runtimeCenter";
  import type { BackendProgressEvent, RuntimeStatus } from "$lib/types";
  import Tag from "../Tag.svelte";

  interface Props {
    runtime: RuntimeStatus;
    progress?: BackendProgressEvent;
    policy?: string;
    busy?: boolean;
    editable?: boolean;
    onDetails: () => void;
    onCheck: () => void;
    onStage: () => void;
    onActivate: () => void;
    onRollback: () => void;
    onLogs: () => void;
    onEdit: () => void;
  }

  let { runtime, progress, policy, busy = false, editable = true, onDetails, onCheck, onStage, onActivate, onRollback, onLogs, onEdit }: Props = $props();
  let environmentMissing = $derived(runtimeEnvironmentMissing(runtime));
  let staged = $derived(Boolean(runtime.staged && runtime.staged !== runtime.current));
  let available = $derived(Boolean(runtime.available && runtime.available !== runtime.current));
  let percentage = $derived(Math.round(Math.max(0, Math.min(1, progress?.progress ?? 0)) * 100));
  let dotClass = $derived(runtime.state === "DEGRADED" ? "bg-destructive" : runtime.state === "ACTIVE" ? "bg-success" : "bg-muted-foreground/40");

  function valueLabel(group: string, value: string | undefined): string {
    if (!value) return "—";
    const key = `controlPlane.runtimeCenter.${group}.${value.toLowerCase()}`;
    const localized = $translate(key);
    return localized === key ? value : localized;
  }

  function errorLabel(value: string): string {
    return /runtime has no \.venv/i.test(value) ? $translate("controlPlane.runtimeCenter.runtimeErrors.noVenv") : value;
  }
</script>

<tr class="transition-colors hover:bg-muted/30" aria-busy={busy}>
  <td class="max-w-72 px-4 py-3 align-top">
    <div class="flex min-w-0 items-center gap-2">
      <span class={`size-2 shrink-0 rounded-full ${dotClass}`} aria-hidden="true"></span>
      <button class="min-w-0 truncate rounded-sm text-left text-sm font-medium underline-offset-4 hover:underline focus-visible:outline-2 focus-visible:outline-ring" onclick={onDetails} aria-label={$translate("controlPlane.runtimeCenter.actions.detailsFor", { name: runtime.name })}>{runtime.name}</button>
    </div>
    <p class="text-muted-foreground mt-1 text-xs">{valueLabel("kinds", runtime.kind)} · {valueLabel("modes", runtime.mode || "native")} · {valueLabel("sourceTypes", runtime.source)}</p>
    {#if runtime.lastError}
      <p class="mt-1 flex min-w-0 items-center gap-1.5 text-xs text-destructive" role="status" title={errorLabel(runtime.lastError)}>
        <CircleAlert class="size-3 shrink-0" aria-hidden="true" />
        <span class="min-w-0 truncate">{errorLabel(runtime.lastError)}</span>
      </p>
    {/if}
  </td>

  <td class="px-4 py-3 align-top">
    {#if environmentMissing}
      <span class="text-muted-foreground">{$translate("controlPlane.runtimeCenter.facts.notInstalled")}</span>
    {:else}
      <code class="break-all font-mono">{runtime.current || "—"}</code>
    {/if}
  </td>

  <td class="px-4 py-3 align-top">
    {#if staged}
      <code class="break-all font-mono">{runtime.staged}</code>
      <Tag class="mt-1.5 px-1.5 text-[0.625rem] uppercase">{$translate("controlPlane.runtimeCenter.versions.staged")}</Tag>
    {:else if available}
      <code class="break-all font-mono">{runtime.available}</code>
      <Tag class="mt-1.5 bg-warning/10 px-1.5 text-[0.625rem] uppercase text-warning">{$translate("controlPlane.runtimeCenter.list.updates")}</Tag>
    {:else}
      <span class="text-muted-foreground">—</span>
    {/if}
  </td>

  <td class="px-4 py-3 align-top">
    <div>{valueLabel("policy", policy)}</div>
    {#if runtime.pinned}<p class="mt-1 text-xs"><Tag class="px-1.5 text-[0.625rem] uppercase">{$translate("controlPlane.runtimeCenter.policy.pinned")}</Tag> <code class="font-mono">{runtime.pinned}</code></p>{/if}
  </td>

  <td class="px-4 py-3 align-top">
    <div class="flex items-center justify-end gap-1.5 whitespace-nowrap">
      {#if staged}
        <Button size="sm" disabled={busy} onclick={onActivate}>{#if busy}<LoaderCircle class="animate-spin" aria-hidden="true" />{:else}<Play aria-hidden="true" />{/if}{$translate("controlPlane.runtimeCenter.actions.activate")}</Button>
      {:else if available}
        <Button variant="outline" size="sm" disabled={busy} onclick={onStage}><Download aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.stage")}</Button>
      {/if}
      <Button variant="outline" size="sm" onclick={onDetails}>{$translate("controlPlane.runtimeCenter.actions.details")}</Button>
      <DropdownMenu.Root>
        <DropdownMenu.Trigger class={buttonVariants({ variant: "ghost", size: "icon-sm" })} aria-label={$translate("controlPlane.runtimeCenter.actions.moreFor", { name: runtime.name })} title={$translate("controlPlane.runtimeCenter.actions.moreFor", { name: runtime.name })}><Ellipsis class="size-4" aria-hidden="true" /></DropdownMenu.Trigger>
        <DropdownMenu.Content align="end">
          <DropdownMenu.Item disabled={busy} onclick={onCheck}>{#if busy}<LoaderCircle class="animate-spin" aria-hidden="true" />{:else}<CircleCheck aria-hidden="true" />{/if}{$translate("controlPlane.runtimeCenter.actions.checkUpdate")}</DropdownMenu.Item>
          <DropdownMenu.Item disabled={!editable || busy} onclick={onEdit}><Pencil aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.edit")}</DropdownMenu.Item>
          <DropdownMenu.Item disabled={busy} onclick={onStage}><Download aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.selectVersion")}</DropdownMenu.Item>
          <DropdownMenu.Item onclick={onLogs}><Terminal aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.logs")}</DropdownMenu.Item>
          {#if runtime.previous}<DropdownMenu.Separator /><DropdownMenu.Item disabled={busy} onclick={onRollback}><RotateCcw aria-hidden="true" />{$translate("controlPlane.runtimeCenter.actions.rollback")}</DropdownMenu.Item>{/if}
        </DropdownMenu.Content>
      </DropdownMenu.Root>
    </div>
  </td>
</tr>

{#if progress}
  <tr class="bg-muted/10">
    <td class="px-4 py-2.5" colSpan={5}>
      <div class="grid gap-1.5" aria-live="polite">
        <div class="flex justify-between gap-2 text-xs text-muted-foreground"><span class="min-w-0 break-words">{progress.message || progress.phase}</span><span class="shrink-0 tabular-nums">{percentage}%</span></div>
        <div class="h-1.5 overflow-hidden rounded-full bg-muted" role="progressbar" aria-label={$translate("controlPlane.progress")} aria-valuenow={percentage} aria-valuemin={0} aria-valuemax={100}><div class="h-full bg-primary transition-[width]" style={`width: ${percentage}%`}></div></div>
        {#if (progress.total ?? 0) > 0}<p class="text-xs text-muted-foreground">{formatFileSize(Math.max(0, progress.completed ?? 0))} / {formatFileSize(progress.total ?? 0)}</p>{/if}
        {#if progress.error}<p class="break-words text-xs text-destructive" role="alert">{progress.error}</p>{/if}
      </div>
    </td>
  </tr>
{/if}
