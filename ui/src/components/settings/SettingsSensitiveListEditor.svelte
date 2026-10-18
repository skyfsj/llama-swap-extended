<script lang="ts">
  import { Trash2 } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../../lib/i18n";
  import type { SettingsChange } from "../../lib/settingsApi";
  import {
    appendRow,
    listAddOp,
    listRemoveOp,
    listReplaceOp,
    REDACTED_MASK,
    removeRow,
    replaceRow,
    secretRows,
    type SecretRow,
  } from "../../lib/settingsListEdits";

  // Masked editor for a sensitive sequence (apiKeys). Every edit is reported as
  // one index-scoped operation next to the new array: rows the operator did not
  // touch keep their stored secret and are never part of the write, which the
  // wholesale replace used for other lists cannot promise once the values are
  // redacted.
  interface Props {
    path: string;
    value: unknown;
    onChange: (value: unknown[], changes: SettingsChange[]) => void;
  }

  let { path, value, onChange }: Props = $props();
  let rows = $derived(secretRows(value));
  // A new key enters the list only once it has a value, because the loader
  // rejects an empty entry.
  let pending = $state("");

  function edit(row: SecretRow, raw: string): void {
    // An entry can never be empty: an input cleared back to nothing keeps the
    // stored value, and removing a key is the row's delete button.
    if (raw === "" && row.redacted) return;
    const next = row.kind === "entry" ? { ...row.entry, [row.field]: raw } : raw;
    onChange(replaceRow(value, row.index, next), [listReplaceOp(path, row.index, next)]);
  }

  function remove(row: SecretRow): void {
    onChange(removeRow(value, row.index), [listRemoveOp(path, row.index)]);
  }

  function commitPending(): void {
    const text = pending;
    pending = "";
    if (!text.trim()) return;
    onChange(appendRow(value, text), [listAddOp(path, text)]);
  }

  function onPendingKeydown(event: KeyboardEvent): void {
    if (event.key === "Enter") {
      event.preventDefault();
      commitPending();
    }
  }
</script>

<div class="settings-list-editor">
  {#each rows as row (row.index)}
    <div class="settings-list-row">
      <div class="settings-secret-cell">
        {#if row.kind === "entry"}
          <span class="settings-secret-label">
            {row.label || $translate("settingsCenter.field.structuredEntry")}
          </span>
        {/if}
        <!-- The mask is a placeholder, never the field value: typing must not
             append to it, and an untouched row sends no change at all. -->
        <Input
          value={row.redacted ? "" : row.secret}
          placeholder={row.redacted ? REDACTED_MASK : ""}
          aria-label={row.kind === "entry"
            ? row.label || $translate("settingsCenter.field.structuredEntry")
            : $translate("settingsCenter.field.secretValue")}
          autocomplete="off"
          spellcheck="false"
          oninput={(event) => edit(row, event.currentTarget.value)}
        />
      </div>
      <Button
        class="settings-inline-action"
        type="button"
        variant="ghost"
        size="icon"
        aria-label={$translate("settingsCenter.field.removeItem")}
        onclick={() => remove(row)}
      >
        <Trash2 />
      </Button>
    </div>
  {/each}
  <div class="settings-list-row settings-list-row--new">
    <Input
      value={pending}
      placeholder={$translate("settingsCenter.field.addSecret")}
      aria-label={$translate("settingsCenter.field.addSecret")}
      autocomplete="off"
      spellcheck="false"
      oninput={(event) => (pending = event.currentTarget.value)}
      onblur={commitPending}
      onkeydown={onPendingKeydown}
    />
  </div>
  <p class="settings-field__hint">{$translate("settingsCenter.field.secretListHint")}</p>
</div>

<style>
  .settings-secret-cell {
    display: flex;
    flex: 1 1 auto;
    align-items: center;
    gap: 0.5rem;
    min-inline-size: 0;
  }

  .settings-secret-label {
    flex: 0 0 auto;
    max-inline-size: 12rem;
    overflow: hidden;
    color: var(--muted-foreground);
    font-size: 0.75rem;
    text-overflow: ellipsis;
    white-space: nowrap;
  }

  .settings-list-row--new :global([data-slot="input"]) {
    border-style: dashed;
  }
</style>
