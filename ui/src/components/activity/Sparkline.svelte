<script lang="ts">
  interface Props {
    /** Values in draw order; null leaves a gap in the line. */
    values: (number | null)[];
    /** Accessible summary, since the shape carries no labels. */
    label: string;
    colorClass?: string;
    height?: number;
  }

  let { values, label, colorClass = "text-blue-500", height = 28 }: Props = $props();

  const width = 100;
  const padding = 2;

  let path = $derived.by(() => {
    const numbers = values.filter((value): value is number => value !== null && Number.isFinite(value));
    if (numbers.length < 2) return "";
    const min = Math.min(...numbers);
    const max = Math.max(...numbers);
    const range = max > min ? max - min : 1;
    const stepX = (width - padding * 2) / (numbers.length - 1);
    return numbers
      .map((value, index) => {
        const x = padding + index * stepX;
        const y = height - padding - ((value - min) / range) * (height - padding * 2);
        return `${index === 0 ? "M" : "L"}${x.toFixed(2)},${y.toFixed(2)}`;
      })
      .join(" ");
  });
</script>

{#if path !== ""}
  <svg
    viewBox="0 0 {width} {height}"
    class="h-7 w-full"
    preserveAspectRatio="none"
    role="img"
    aria-label={label}
  >
    <path d={path} fill="none" stroke="currentColor" stroke-width="1.5" class={colorClass} vector-effect="non-scaling-stroke" />
  </svg>
{:else}
  <div class="text-muted-foreground flex h-7 items-center text-xs">—</div>
{/if}
