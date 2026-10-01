<script lang="ts">
  import ExtensionValueEditor from "./ExtensionValueEditor.svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Plus, Trash2 } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  let { value, onChange, label = "", root = false, allowedTypes }: { value: unknown; onChange: (value: unknown) => void; label?: string; root?: boolean; allowedTypes?: string[] } = $props();
  let newKey = $state("");
  let error = $state("");
  const kind = $derived(value === null ? "null" : Array.isArray(value) ? "array" : typeof value);
  const defaults: Record<string, unknown> = { string: "", number: 0, boolean: false, object: {}, array: [], null: null };
  function setEntry(key: string, next: unknown) {
    if (Array.isArray(value)) { const copy = [...value]; copy[Number(key)] = next; onChange(copy); }
    else onChange({ ...(value as Record<string, unknown>), [key]: next });
  }
  function remove(key: string) {
    if (Array.isArray(value)) onChange(value.filter((_, i) => i !== Number(key)));
    else onChange(Object.fromEntries(Object.entries(value as Record<string, unknown>).filter(([k]) => k !== key)));
  }
  function add() {
    if (Array.isArray(value)) { onChange([...value, ""]); return; }
    const key = newKey.trim();
    if (!key) return;
    if (Object.hasOwn(value as object, key)) { error = $translate("extensions.dev.duplicateKey"); return; }
    onChange({ ...(value as Record<string, unknown>), [key]: "" }); newKey = ""; error = "";
  }
</script>
<div class="min-w-0 space-y-2">
  {#if !root}<div class="flex items-center gap-2"><span class="min-w-0 flex-1 break-all font-mono text-xs">{label}</span><select class="rounded border border-border bg-background p-1 text-xs" aria-label={`${label} · ${$translate("extensions.workspace.type")}`} value={kind} onchange={(e) => onChange(defaults[e.currentTarget.value])}>{#each (allowedTypes ?? Object.keys(defaults)) as type}<option value={type}>{$translate(`extensions.dev.types.${type}`)}</option>{/each}</select></div>{/if}
  {#if kind === "object" || kind === "array"}
    <div class="space-y-3" class:pl-3={!root} class:border-l={!root} class:border-border={!root}>
      {#each Object.entries(value as object) as [key, child] (key)}
        <div class="flex items-start gap-1"><div class="min-w-0 flex-1"><ExtensionValueEditor value={child} label={kind === "array" ? `[${key}]` : key} onChange={(next) => setEntry(key, next)} /></div><Button variant="ghost" size="icon-xs" class="text-muted-foreground hover:text-destructive" aria-label={`${$translate("extensions.delete")} ${key}`} onclick={() => remove(key)}><Trash2 class="size-3" /></Button></div>
      {/each}
      <div class="flex flex-wrap gap-2">{#if kind === "object"}<input class="min-w-0 flex-1 rounded-md border border-border bg-background p-2 text-xs" bind:value={newKey} aria-label={$translate("extensions.dev.key")} placeholder={$translate("extensions.dev.key")} onkeydown={(event) => { if (event.key === "Enter") { event.preventDefault(); add(); } }} />{/if}<Button variant="outline" size="sm" disabled={kind === "object" && !newKey.trim()} onclick={add}><Plus class="size-3" />{$translate("extensions.dev.addValue")}</Button></div>
      {#if error}<p role="alert" class="text-xs text-destructive">{error}</p>{/if}
    </div>
  {:else if kind === "boolean"}<label class="flex items-center gap-2 text-xs"><input type="checkbox" checked={Boolean(value)} onchange={(e) => onChange(e.currentTarget.checked)} />{$translate("extensions.dev.booleanValue")}</label>
  {:else if kind === "number"}<input aria-label={label} type="number" step="any" class="w-full rounded-md border border-border bg-background p-2 text-xs" value={Number(value)} onchange={(e) => { if (e.currentTarget.value !== "" && Number.isFinite(e.currentTarget.valueAsNumber)) onChange(e.currentTarget.valueAsNumber); }} />
  {:else if kind === "null"}<p class="text-xs text-muted-foreground">null</p>
  {:else}<textarea aria-label={label} rows="2" class="w-full resize-y rounded-md border border-border bg-background p-2 text-xs" value={String(value ?? "")} oninput={(e) => onChange(e.currentTarget.value)}></textarea>{/if}
</div>
