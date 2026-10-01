<script lang="ts">
  import { Eye, ListFilter, Trash2 } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../../lib/i18n";

  interface Props {
    onView?: () => void;
    onFilterSession?: () => void;
    onDelete?: () => void;
  }

  let { onView, onFilterSession, onDelete }: Props = $props();
</script>

<div class="flex items-center gap-1">
  {#if onView}
    <Button variant="outline" size="xs" onclick={onView}>
      <Eye class="size-3" aria-hidden="true" />
      {$translate("common.view")}
    </Button>
  {/if}
  {#if onFilterSession}
    <Button
      variant="outline"
      size="xs"
      onclick={onFilterSession}
      title={$translate("activity.table.filterSession")}
      aria-label={$translate("activity.table.filterSession")}
    >
      <ListFilter class="size-3" aria-hidden="true" />
      {$translate("activity.table.sessionShortcut")}
    </Button>
  {/if}
  {#if onDelete}
    <Button
      variant="destructive"
      size="icon-xs"
      onclick={onDelete}
      title={$translate("activity.table.deleteRow")}
      aria-label={$translate("activity.table.deleteRow")}
    >
      <Trash2 aria-hidden="true" />
    </Button>
  {/if}
</div>
