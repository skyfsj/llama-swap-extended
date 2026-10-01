<script lang="ts">
  import { phaseShare, formatMs } from "../../lib/speedTimeline";
  import { translate } from "../../lib/i18n";

  interface Props {
    firstTokenMs?: number;
    decodeMs?: number;
    durationMs?: number;
  }

  let { firstTokenMs, decodeMs, durationMs }: Props = $props();

  let share = $derived(phaseShare(firstTokenMs, decodeMs, durationMs));
  let pct = $derived({
    ttft: share.totalMs > 0 ? (share.ttftMs / share.totalMs) * 100 : 0,
    decode: share.totalMs > 0 ? (share.decodeMs / share.totalMs) * 100 : 0,
    other: share.totalMs > 0 ? (share.otherMs / share.totalMs) * 100 : 0,
  });
</script>

<div>
  <div class="flex h-3 w-full overflow-hidden rounded-full bg-muted" role="img"
    aria-label="phase share">
    {#if pct.ttft > 0}
      <div class="h-full bg-amber-500" style="width: {pct.ttft}%"></div>
    {/if}
    {#if pct.decode > 0}
      <div class="h-full bg-sky-600" style="width: {pct.decode}%"></div>
    {/if}
    {#if pct.other > 0}
      <div class="h-full bg-muted-foreground/30" style="width: {pct.other}%"></div>
    {/if}
  </div>
  <div class="text-muted-foreground mt-2 flex flex-wrap gap-x-4 gap-y-1 text-xs">
    <span class="flex items-center gap-1.5">
      <span class="inline-block h-2 w-2 rounded-full bg-amber-500"></span>
      {$translate("controlPlane.phaseTtft")}: {formatMs(share.ttftMs)} ({pct.ttft.toFixed(0)}%)
    </span>
    <span class="flex items-center gap-1.5">
      <span class="inline-block h-2 w-2 rounded-full bg-sky-600"></span>
      {$translate("controlPlane.phaseDecode")}: {formatMs(share.decodeMs)} ({pct.decode.toFixed(0)}%)
    </span>
    {#if pct.other > 0}
      <span class="flex items-center gap-1.5">
        <span class="inline-block h-2 w-2 rounded-full bg-muted-foreground/30"></span>
        {$translate("controlPlane.phaseOther")}: {formatMs(share.otherMs)} ({pct.other.toFixed(0)}%)
      </span>
    {/if}
    <span>{$translate("controlPlane.phaseTotal")}: {formatMs(share.totalMs)}</span>
  </div>
</div>
