<script lang="ts">
  import { Loader2 } from "@lucide/svelte";
  import { translate } from "../lib/i18n";
</script>

<div class="bg-background flex h-full items-center justify-center" role="status" aria-label={$translate("common.loading")}>
  <Loader2 class="text-muted-foreground size-6 animate-spin" aria-hidden="true" />
</div>
