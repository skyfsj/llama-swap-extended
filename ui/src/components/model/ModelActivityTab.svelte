<script lang="ts">
  import { inflightRequestEntries } from "../../stores/api";
  import RequestRecords from "../activity/RequestRecords.svelte";

  interface Props {
    modelId: string;
  }

  let { modelId }: Props = $props();

  let modelInflightRequests = $derived(
    $inflightRequestEntries.filter((request) => request.model === modelId),
  );
</script>

<RequestRecords
  {modelId}
  storagePrefix="model-detail-activity"
  showModelColumn={false}
  inflightRequests={modelInflightRequests}
/>
