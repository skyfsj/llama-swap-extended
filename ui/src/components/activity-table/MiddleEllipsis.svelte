<script lang="ts">
  interface Props {
    value: string;
    tailLength?: number;
    className?: string;
  }

  let { value, tailLength = 10, className = "" }: Props = $props();

  let displayValue = $derived(value || "-");
  let head = $derived(
    displayValue.length > tailLength ? displayValue.slice(0, -tailLength) : displayValue
  );
  let tail = $derived(
    displayValue.length > tailLength ? displayValue.slice(-tailLength) : ""
  );
</script>

<!--
  whitespace-pre, not nowrap: each half is its own flex item, and a flex item
  drops the whitespace at the edge of its line — "opencode-go / Responses" split
  into "opencode-go /" and " Responses" rendered as "opencode-go /Responses".
  Pre preserves the value exactly while the head still truncates with an ellipsis.
-->
<span class="flex min-w-0 items-baseline {className}" title={displayValue} aria-label={displayValue}>
  <span class="min-w-0 overflow-hidden text-ellipsis whitespace-pre">{head}</span>
  {#if tail}
    <span class="shrink-0 whitespace-pre">{tail}</span>
  {/if}
</span>
