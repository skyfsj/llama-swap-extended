<script lang="ts">
  import { streamModelLog } from "../../stores/modelLogs";
  import LogPanel from "../LogPanel.svelte";
  import { translate } from "../../lib/i18n";

  interface Props {
    modelId: string;
  }

  let { modelId }: Props = $props();

  let logData = $state("");
  $effect(() => {
    const id = modelId;
    if (!id) {
      logData = "";
      return;
    }
    const store = streamModelLog(id);
    const unsub = store.subscribe((v) => (logData = v));
    return () => unsub();
  });
</script>

<div class="h-full">
  <LogPanel id={`model-${modelId}`} title={$translate("modelDetail.modelLogs")} {logData} />
</div>
