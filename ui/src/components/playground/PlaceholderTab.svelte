<script lang="ts">
  import { translate } from "../../lib/i18n";

  interface Props {
    featureName: string;
  }

  let { featureName }: Props = $props();
</script>

<div class="flex items-center justify-center h-full">
  <div class="text-muted-foreground text-center">
    <p class="text-lg">{featureName}</p>
    <p class="text-sm mt-2">{$translate("playground.toBeImplemented")}</p>
  </div>
</div>
