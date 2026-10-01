<script lang="ts">
  import { translate } from "../../lib/i18n";
  import type { ExtensionDefinition } from "../../lib/extensionsApi";
  import ExtensionValueEditor from "./ExtensionValueEditor.svelte";
  import ModelMultiSelect from "../ModelMultiSelect.svelte";
  import { models } from "../../stores/api";
  import ExtensionSettingsForm from "./ExtensionSettingsForm.svelte";
  let { draft = $bindable(), errors = {} }: { draft: ExtensionDefinition; errors?: Record<string, string> } = $props();
  const parse = (value: string) => value.split(/[\n,]+/).map((entry) => entry.trim()).filter(Boolean);
</script>
<div class="configuration space-y-5 p-4">
  <section class="space-y-3">
    <h2 class="!pb-0 text-sm font-semibold">{$translate("extensions.workspace.basic")}</h2>
    <label>{$translate("extensions.name")}<input bind:value={draft.manifest.name} /></label>

    {#if !draft.etag}<label>{$translate("extensions.id")}<input bind:value={draft.manifest.id} placeholder="my-extension" /></label>{/if}
    <details><summary>{$translate("extensions.workspace.details")}</summary><div class="space-y-3 pt-3">
    {#if draft.etag}<label>{$translate("extensions.id")}<input value={draft.manifest.id} disabled /></label>{/if}
    <label>{$translate("extensions.description")}<textarea rows="2" bind:value={draft.manifest.description}></textarea></label>
    </div></details>
  </section>
  <section class="space-y-3 border-t border-border pt-4">
    <h2 class="!pb-0 text-sm font-semibold">{$translate("extensions.workspace.models")}</h2>
    <ModelMultiSelect value={draft.manifest.match.models ?? []} options={$models.map((model) => model.id)} ariaLabel={$translate("extensions.models")} allLabel={$translate("extensions.workspace.allModels")} selectedLabel={(draft.manifest.match.models ?? []).join(", ")} emptyLabel={$translate("extensions.dev.noModels")} onValueChange={(value) => draft.manifest.match.models = value} />
    <details><summary>{$translate("extensions.workspace.matchAdvanced")}</summary><div class="space-y-3 pt-3">
    <label>{$translate("extensions.dev.modelPatterns")}<textarea rows="2" placeholder="*" value={(draft.manifest.match.models ?? []).join(", ")} oninput={(e) => draft.manifest.match.models = parse(e.currentTarget.value)}></textarea></label>
    <p class="text-xs leading-relaxed text-muted-foreground">{$translate("extensions.matchHint")}</p>

      {#each ["excludeModels", "profiles", "providers", "endpoints"] as key}
        <label>{$translate(`extensions.${key}`)}<input value={(draft.manifest.match[key as keyof typeof draft.manifest.match] ?? []).join(", ")} oninput={(e) => draft.manifest.match[key as keyof typeof draft.manifest.match] = parse(e.currentTarget.value)} /></label>
      {/each}
    </div></details>
  </section>
  <section class="space-y-3 border-t border-border pt-4">
    <h2 class="!pb-0 text-sm font-semibold">{$translate("extensions.workspace.runtime")}</h2>
    {#if draft.settings?.length}
      <ExtensionSettingsForm settings={draft.settings} config={draft.manifest.config ?? {}} {errors} onChange={(key, value) => draft.manifest.config = { ...draft.manifest.config, [key]: value }} />
    {:else}
      <ExtensionValueEditor root value={draft.manifest.config ?? {}} onChange={(value) => draft.manifest.config = value as Record<string, unknown>} />
    {/if}
    <details><summary>{$translate("extensions.workspace.limits")}</summary><div class="space-y-3 pt-3">
      <label>{$translate("extensions.priority")}<input type="number" bind:value={draft.manifest.priority} /></label>
      <label>{$translate("extensions.timeout")}<input bind:value={draft.manifest.timeout} placeholder="10s" /></label>
      <label>{$translate("extensions.cpu")}<input type="number" min="1" bind:value={draft.manifest.maxCpuMillis} /></label>
      <label>{$translate("extensions.memory")}<input type="number" min="1" bind:value={draft.manifest.maxMemoryMiB} /></label>
      <label>{$translate("extensions.conflict")}<select bind:value={draft.manifest.toolConflict}><option value="skip">skip</option><option value="override">override</option><option value="error">error</option></select></label>
      <label class="!flex items-center gap-2"><input type="checkbox" bind:checked={draft.manifest.continueOnError} />{$translate("extensions.continueOnError")}</label>
      <label class="!flex items-center gap-2"><input type="checkbox" bind:checked={draft.manifest.interceptClientTools} />{$translate("extensions.interceptClientTools")}</label>
    </div></details>
  </section>
  <details class="border-t border-border pt-4"><summary>{$translate("extensions.permissions")}</summary><div class="space-y-3 pt-3">
    {#each ["networkHosts", "readRoots", "writeRoots"] as key (key)}
      <label>{$translate(`extensions.${key}`)}<textarea rows="2" value={((draft.manifest.permissions[key as "networkHosts"] as string[] | null) ?? []).join("\n")} oninput={(e) => draft.manifest.permissions[key as "networkHosts"] = parse(e.currentTarget.value)}></textarea></label>
    {/each}
    <label>{$translate("extensions.storage")}<select value={draft.manifest.permissions.storage ?? ""} onchange={(event) => { const value = event.currentTarget.value; if (value) draft.manifest.permissions.storage = value; else delete draft.manifest.permissions.storage; }}><option value="">ephemeral</option><option value="persistent">persistent</option><option value="none">none</option></select></label>
  </div></details>
</div>
<style>
  .configuration label { display: grid; gap: .5rem; font-size: .75rem; }
  .configuration input:not([type="checkbox"]), .configuration textarea, .configuration select { width: 100%; min-width: 0; border: 1px solid var(--border); border-radius: .375rem; padding: .5rem .625rem; background: var(--background); font-size: .8125rem; }
  .configuration input:disabled { color: var(--muted-foreground); background: var(--muted); }
  .configuration summary { cursor: pointer; font-size: .8125rem; font-weight: 500; }
</style>
