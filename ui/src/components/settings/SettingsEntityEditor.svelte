<script lang="ts">
  import { Plus, Trash2 } from "@lucide/svelte";
  import * as Label from "$lib/components/ui/label/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";
  import type { SettingsOption } from "../../lib/settingsApi";
  import SettingsMapEditor from "./SettingsMapEditor.svelte";

  interface Props {
    editor: string;
    value: unknown;
    modelOptions?: SettingsOption[];
    onChange: (value: unknown) => void;
  }

  let { editor, value, modelOptions = [], onChange }: Props = $props();
  let selected = $state("");
  let entities = $derived(value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {});
  let keys = $derived(Object.keys(entities).sort());
  let active = $derived(selected && entities[selected] && typeof entities[selected] === "object" ? entities[selected] as Record<string, unknown> : {});

  function choose(next: string): void {
    selected = next;
  }

  function setActive(key: string, next: unknown): void {
    onChange({ ...entities, [key]: next });
  }

  function set(name: string, next: unknown): void {
    if (selected) setActive(selected, { ...active, [name]: next });
  }

  function nested(name: string): Record<string, unknown> {
    const item = active[name];
    return item && typeof item === "object" && !Array.isArray(item) ? item as Record<string, unknown> : {};
  }

  function setNested(name: string, next: Record<string, unknown>): void {
    set(name, { ...nested(name), ...next });
  }

  function list(name: string): unknown[] {
    return Array.isArray(active[name]) ? active[name] as unknown[] : [];
  }

  function updateList(name: string, index: number, next: string): void {
    const items = list(name);
    items[index] = next;
    set(name, items);
  }

  function addList(name: string): void {
    set(name, [...list(name), ""]);
  }

  function removeList(name: string, index: number): void {
    set(name, list(name).filter((_, itemIndex) => itemIndex !== index));
  }

  function titleKey(): string {
    if (editor === "runtime") return "settingsCenter.entity.runtime";
    if (editor === "peer") return "settingsCenter.entity.peer";
    if (editor === "profile") return "settingsCenter.entity.profile";
    return "settingsCenter.entity.selector";
  }
</script>

<div class="settings-entity-editor sm:col-span-2 rounded-md border p-3">
  <div class="flex flex-wrap items-center gap-2">
    <h3 class="text-sm font-medium">{$translate(titleKey())}</h3>
    <Select.Root type="single" value={selected} onValueChange={choose}>
      <Select.Trigger class="ml-auto w-full max-w-xs"><span class="truncate">{selected || $translate("settingsCenter.entity.selectEntry")}</span></Select.Trigger>
      <Select.Content>{#each keys as key}<Select.Item value={key}>{key}</Select.Item>{/each}</Select.Content>
    </Select.Root>
  </div>

  {#if !selected}
    <p class="text-muted-foreground mt-3 text-sm">{keys.length ? $translate("settingsCenter.entity.chooseEntry") : $translate("settingsCenter.entity.noEntries")}</p>
  {:else if editor === "runtime"}
    <div class="mt-4 grid gap-3 sm:grid-cols-2">
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("settingsCenter.entity.type")}</span><Input value={String(active.kind ?? "")} oninput={(event) => set("kind", event.currentTarget.value)} /></label>
      <label class="grid gap-1.5 text-sm">
        <span class="font-medium">{$translate("settingsCenter.entity.mode")}</span>
        <Select.Root type="single" value={String(active.mode ?? "native")} onValueChange={(next) => next && set("mode", next)}><Select.Trigger class="w-full"><span>{String(active.mode ?? "native")}</span></Select.Trigger><Select.Content>{#each ["native", "container"] as item}<Select.Item value={item}>{item}</Select.Item>{/each}</Select.Content></Select.Root>
      </label>
      <label class="grid gap-1.5 text-sm">
        <span class="font-medium">{$translate("settingsCenter.entity.sourceType")}</span>
        <Select.Root type="single" value={String(nested("source").type ?? "bundled")} onValueChange={(next) => next && setNested("source", { type: next })}><Select.Trigger class="w-full"><span>{String(nested("source").type ?? "bundled")}</span></Select.Trigger><Select.Content>{#each ["bundled", "release", "channel", "pypi", "wheel", "git", "local", "image"] as item}<Select.Item value={item}>{item}</Select.Item>{/each}</Select.Content></Select.Root>
      </label>
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("settingsCenter.entity.repository")}</span><Input value={String(nested("source").repository ?? "")} oninput={(event) => setNested("source", { repository: event.currentTarget.value })} /></label>
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("settingsCenter.entity.reference")}</span><Input value={String(nested("source").ref ?? "")} oninput={(event) => setNested("source", { ref: event.currentTarget.value })} /></label>
      <label class="grid gap-1.5 text-sm">
        <span class="font-medium">{$translate("settingsCenter.entity.updatePolicy")}</span>
        <Select.Root type="single" value={String(nested("update").policy ?? "manual")} onValueChange={(next) => next && setNested("update", { policy: next })}><Select.Trigger class="w-full"><span>{String(nested("update").policy ?? "manual")}</span></Select.Trigger><Select.Content>{#each ["disabled", "manual", "automatic", "pinned"] as item}<Select.Item value={item}>{item}</Select.Item>{/each}</Select.Content></Select.Root>
      </label>
    </div>
  {:else if editor === "peer"}
    <div class="mt-4 grid gap-3">
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("controlPlane.modelProxy")}</span><Input value={String(active.proxy ?? "")} oninput={(event) => set("proxy", event.currentTarget.value)} /></label>
      <div class="grid gap-2 text-sm">
        <Label.Root>{$translate("models.title")}</Label.Root>
        {#each list("models") as item, index (index)}
          <div class="flex items-center gap-2"><Input value={String(item ?? "")} oninput={(event) => updateList("models", index, event.currentTarget.value)} /><Button type="button" variant="ghost" size="icon" aria-label={$translate("settingsCenter.field.removeItem")} onclick={() => removeList("models", index)}><Trash2 class="size-4" /></Button></div>
        {/each}
        <Button class="mt-1 w-fit" type="button" variant="outline" size="sm" onclick={() => addList("models")}><Plus class="size-3.5" />{$translate("settingsCenter.entity.addModel")}</Button>
      </div>
      <div class="text-muted-foreground text-xs">{$translate("settingsCenter.entity.apiKey")}: {active.apiKey ? $translate("settingsCenter.field.configured") : $translate("settingsCenter.field.notConfigured")}</div>
    </div>
  {:else if editor === "profile"}
    <div class="mt-4 grid gap-3">
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("controlPlane.modelDescription")}</span><Input value={String(active.description ?? "")} oninput={(event) => set("description", event.currentTarget.value)} /></label>
      <div class="grid gap-1.5 text-sm"><Label.Root>{$translate("settingsCenter.entity.modelMappings")}</Label.Root><SettingsMapEditor value={active.pins} onChange={(next) => set("pins", next)} /></div>
    </div>
  {:else}
    <div class="mt-4 grid gap-3 sm:grid-cols-2">
      <label class="grid gap-1.5 text-sm"><span class="font-medium">{$translate("controlPlane.modelName")}</span><Input value={String(active.name ?? "")} oninput={(event) => set("name", event.currentTarget.value)} /></label>
      <label class="grid gap-1.5 text-sm">
        <span class="font-medium">{$translate("settingsCenter.entity.strategy")}</span>
        <Select.Root type="single" value={String(active.strategy ?? "pin")} onValueChange={(next) => next && set("strategy", next)}><Select.Trigger class="w-full"><span>{String(active.strategy ?? "pin")}</span></Select.Trigger><Select.Content>{#each ["warm", "pin", "spillover", "failover"] as item}<Select.Item value={item}>{item}</Select.Item>{/each}</Select.Content></Select.Root>
      </label>
      <div class="grid gap-2 text-sm sm:col-span-2">
        <Label.Root>{$translate("settingsCenter.entity.targetModels")}</Label.Root>
        {#each list("targets") as item, index (index)}
          <div class="flex items-center gap-2"><Select.Root type="single" value={String(item ?? "")} onValueChange={(next) => next && updateList("targets", index, next)}><Select.Trigger class="w-full"><span>{String(item ?? $translate("settingsCenter.entity.selectModel"))}</span></Select.Trigger><Select.Content>{#each modelOptions as option}<Select.Item value={option.value}>{option.label}</Select.Item>{/each}</Select.Content></Select.Root><Button type="button" variant="ghost" size="icon" aria-label={$translate("settingsCenter.field.removeItem")} onclick={() => removeList("targets", index)}><Trash2 class="size-4" /></Button></div>
        {/each}
        <Button class="mt-1 w-fit" type="button" variant="outline" size="sm" onclick={() => addList("targets")}><Plus class="size-3.5" />{$translate("settingsCenter.entity.addTarget")}</Button>
      </div>
      <div class="flex items-center gap-2"><Switch.Root id="selector-unlisted" checked={active.unlisted === true} onCheckedChange={(next) => set("unlisted", next)} /><Label.Root for="selector-unlisted">{$translate("controlPlane.modelUnlisted")}</Label.Root></div>
    </div>
  {/if}
</div>
