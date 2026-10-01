<script lang="ts">
  import { AlertTriangle, Check, Save } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../../lib/i18n";
  import type { ConfigChange } from "../../lib/modelConfig";
  import type { SettingsPreview } from "../../lib/settingsApi";

  // Read-only preview of the structured draft: server diagnostics when the
  // draft is invalid, otherwise the field-level before/after table.
  interface Props {
    open: boolean;
    preview: SettingsPreview | null;
    changes: ConfigChange[];
    busy: boolean;
    onSave: () => void;
  }

  let { open = $bindable(false), preview, changes, busy, onSave }: Props = $props();

  function formatValue(value: unknown): string {
    if (value === undefined) return "—";
    if (value === null) return "null";
    if (typeof value === "string") return value === "" ? '""' : value;
    return JSON.stringify(value);
  }
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="settings-preview-dialog">
    <Dialog.Header>
      <Dialog.Title>{$translate("settingsCenter.preview.title")}</Dialog.Title>
      <Dialog.Description>{$translate("settingsCenter.preview.description")}</Dialog.Description>
    </Dialog.Header>

    {#if preview && !preview.valid}
      <div class="settings-alert settings-alert--error" role="alert">
        <AlertTriangle aria-hidden="true" />
        <div>
          <strong>{$translate("settingsCenter.preview.invalid")}</strong>
          {#if preview.diagnostics?.length}
            <ul>
              {#each preview.diagnostics as diagnostic}
                <li>{diagnostic.path ? `${diagnostic.path}: ` : ""}{diagnostic.message}</li>
              {/each}
            </ul>
          {/if}
        </div>
      </div>
    {:else if changes.length === 0}
      <div class="settings-empty settings-empty--compact">{$translate("settingsCenter.preview.empty")}</div>
    {:else}
      {#if preview?.valid}
        <div class="settings-alert" role="status">
          <Check aria-hidden="true" />
          <span>{$translate("settingsCenter.preview.valid")}</span>
        </div>
      {/if}
      <div class="settings-preview-table" role="table">
        <div class="settings-preview-row settings-preview-row--head" role="row">
          <span role="columnheader">{$translate("settingsCenter.preview.field")}</span>
          <span role="columnheader">{$translate("settingsCenter.preview.before")}</span>
          <span role="columnheader">{$translate("settingsCenter.preview.after")}</span>
        </div>
        {#each changes as change (change.path)}
          <div class="settings-preview-row" role="row" data-kind={change.kind}>
            <span class="settings-preview-path" role="cell">{change.path}</span>
            <span class="settings-preview-value" role="cell">{formatValue(change.before)}</span>
            <span class="settings-preview-value" role="cell">{formatValue(change.after)}</span>
          </div>
        {/each}
      </div>
    {/if}

    <Dialog.Footer>
      <Button variant="outline" onclick={() => (open = false)}>{$translate("common.close")}</Button>
      <Button
        onclick={onSave}
        disabled={!preview?.valid || busy || changes.length === 0}
      >
        <Save aria-hidden="true" />{$translate("settingsCenter.preview.save")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<style>
  .settings-alert {
    display: flex;
    align-items: flex-start;
    gap: 0.55rem;
    margin-block: 0.75rem;
    border: 1px solid color-mix(in oklab, var(--success) 38%, var(--border));
    border-radius: 0.6rem;
    background: color-mix(in oklab, var(--success) 9%, transparent);
    padding: 0.75rem 0.9rem;
    color: var(--foreground);
    font-size: 0.82rem;
  }

  .settings-alert :global(svg) {
    flex: 0 0 auto;
    inline-size: 0.95rem;
    margin-block-start: 0.12rem;
    color: var(--success);
  }

  .settings-alert--error {
    border-color: color-mix(in oklab, var(--destructive) 48%, var(--border));
    background: color-mix(in oklab, var(--destructive) 10%, transparent);
  }

  .settings-alert--error :global(svg) {
    color: var(--destructive);
  }

  .settings-alert strong {
    display: block;
  }

  .settings-alert ul {
    margin: 0.3rem 0 0;
    padding-inline-start: 1rem;
    color: var(--muted-foreground);
    font-size: 0.76rem;
  }

  .settings-empty {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 0.6rem;
    border: 1px dashed color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.7rem;
    color: var(--muted-foreground);
    font-size: 0.85rem;
  }

  .settings-empty--compact {
    min-block-size: 5rem;
  }

  :global(.settings-preview-dialog) {
    max-inline-size: min(56rem, calc(100vw - 2rem));
  }

  .settings-preview-table {
    display: grid;
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.55rem;
    overflow: hidden;
    max-block-size: min(26rem, 45vh);
    overflow-y: auto;
    font-size: 0.8rem;
  }

  .settings-preview-row {
    display: grid;
    grid-template-columns: minmax(9rem, 1.3fr) minmax(0, 1fr) minmax(0, 1fr);
    gap: 0.6rem;
    padding: 0.45rem 0.7rem;
    border-block-end: 1px solid color-mix(in oklab, var(--border) 60%, transparent);
    align-items: baseline;
  }

  .settings-preview-row:last-child {
    border-block-end: 0;
  }

  .settings-preview-row--head {
    position: sticky;
    inset-block-start: 0;
    background: color-mix(in oklab, var(--muted) 70%, var(--background));
    color: var(--muted-foreground);
    font-size: 0.72rem;
    font-weight: 700;
    text-transform: uppercase;
    letter-spacing: 0.04em;
  }

  .settings-preview-row[data-kind="added"] {
    background: color-mix(in oklab, var(--success) 8%, transparent);
  }

  .settings-preview-row[data-kind="removed"] {
    background: color-mix(in oklab, var(--destructive) 8%, transparent);
  }

  .settings-preview-path {
    overflow-wrap: anywhere;
    font-family: var(--mono, ui-monospace, monospace);
    font-size: 0.75rem;
    color: var(--foreground);
  }

  .settings-preview-value {
    overflow-wrap: anywhere;
    color: var(--muted-foreground);
    font-family: var(--mono, ui-monospace, monospace);
    font-size: 0.75rem;
  }

  @media (max-width: 640px) {
    .settings-preview-row {
      grid-template-columns: minmax(7rem, 1fr) minmax(0, 1fr) minmax(0, 1fr);
      font-size: 0.74rem;
    }
  }
</style>
