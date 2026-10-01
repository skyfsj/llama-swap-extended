<script lang="ts">
  import { Plus, Trash2 } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";

  interface Props {
    value: unknown;
    onChange: (value: unknown[]) => void;
  }

  let { value, onChange }: Props = $props();
  let items = $derived(Array.isArray(value) ? value : []);

  function update(index: number, next: string): void {
    const out = [...items];
    out[index] = next;
    onChange(out);
  }

  function add(): void {
    onChange([...items, ""]);
  }

  function remove(index: number): void {
    onChange(items.filter((_, itemIndex) => itemIndex !== index));
  }
</script>

<div class="settings-list-editor">
  {#each items as item, index (index)}
    <div class="settings-list-row">
      <Input value={String(item ?? "")} oninput={(event) => update(index, event.currentTarget.value)} />
      <Button class="settings-inline-action" type="button" variant="ghost" size="icon" aria-label={$translate("settingsCenter.field.removeItem")} onclick={() => remove(index)}><Trash2 /></Button>
    </div>
  {/each}
  <Button class="w-fit" type="button" variant="outline" size="sm" onclick={add}><Plus />{$translate("settingsCenter.field.addItem")}</Button>
</div>
