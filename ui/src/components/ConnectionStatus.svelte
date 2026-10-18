<script lang="ts">
  import { connectionState } from "../stores/theme";
  import { translate } from "../lib/i18n";

  let eventStatusColor = $derived.by(() => {
    switch ($connectionState) {
      case "connected":
        return "bg-emerald-500";
      case "connecting":
        return "bg-amber-500";
      case "disconnected":
      default:
        return "bg-red-500";
    }
  });

  let tooltipText = $derived(
    $translate("status.eventStream", {
      status: $translate(`status.${$connectionState ?? "unknown"}`),
    }),
  );
</script>

<div class="flex items-center justify-center" title={tooltipText} aria-label={tooltipText}>
  <span class="relative flex size-2.5">
    {#if $connectionState === "connecting"}
      <span class="absolute inline-flex size-full animate-ping rounded-full bg-amber-400 opacity-75"></span>
    {:else if $connectionState === "connected"}
      <span class="absolute inline-flex size-full rounded-full bg-emerald-400/20 ring-2 ring-emerald-500/30"></span>
    {/if}
    <span class="relative inline-flex size-2.5 rounded-full {eventStatusColor} shadow-xs"></span>
  </span>
</div>
