<script lang="ts">
  import { Plus, Trash2 } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";

  interface Props {
    value: unknown;
    onChange: (value: Record<string, unknown>) => void;
    numeric?: boolean;
  }

  let { value, onChange, numeric = false }: Props = $props();
  let entries = $derived(Object.entries(
    value && typeof value === "object" && !Array.isArray(value)
      ? value as Record<string, unknown>
      : {},
  ));

  function update(index: number, key: string, raw: string): void {
    const out = [...entries];
    const old = out[index]?.[1];
    const next = numeric || typeof old === "number" ? Number(raw) : raw;
    out[index] = [key, numeric && !Number.isFinite(next) ? 0 : next];
    onChange(Object.fromEntries(out.filter(([name]) => name.trim())));
  }

  function add(): void {
    onChange({ ...Object.fromEntries(entries), "": numeric ? 0 : "" });
  }

  function remove(index: number): void {
    onChange(Object.fromEntries(entries.filter((_, itemIndex) => itemIndex !== index)));
  }
</script>

<div class="settings-map-editor">
  {#each entries as [key, item], index (index)}
    <div class="settings-map-row">
      <Input class="w-2/5" aria-label={$translate("settingsCenter.field.key")} value={key} oninput={(event) => update(index, event.currentTarget.value, String(item ?? ""))} />
      <Input class="flex-1" aria-label={$translate("settingsCenter.field.value")} value={String(item ?? "")} oninput={(event) => update(index, key, event.currentTarget.value)} />
      <Button class="settings-inline-action" type="button" variant="ghost" size="icon" aria-label={$translate("settingsCenter.field.removeItem")} onclick={() => remove(index)}><Trash2 /></Button>
    </div>
  {/each}
  <Button class="w-fit" type="button" variant="outline" size="sm" onclick={add}><Plus />{$translate("settingsCenter.field.addItem")}</Button>
</div>
