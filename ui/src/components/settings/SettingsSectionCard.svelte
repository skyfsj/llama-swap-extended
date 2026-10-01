<script lang="ts">
  import type { Component } from "svelte";

  interface Props {
    id: string;
    icon: Component;
    title: string;
    description: string;
    wide?: boolean;
    children?: import("svelte").Snippet;
  }

  let { id, icon: Icon, title, description, wide = false, children }: Props = $props();
</script>

<section class:settings-section-card--model={wide} class="settings-section-card" aria-labelledby={`settings-card-${id}`}>
  <header class="settings-section-card__header">
    <div class="settings-section-card__icon"><Icon aria-hidden="true" /></div>
    <div>
      <h3 id={`settings-card-${id}`}>{title}</h3>
      <p>{description}</p>
    </div>
  </header>
  {@render children?.()}
</section>

<style>
  .settings-section-card {
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.7rem;
    background: color-mix(in oklab, var(--card) 94%, var(--background));
    padding: clamp(0.95rem, 1.3vw, 1.2rem);
  }

  .settings-section-card__header {
    display: flex;
    align-items: flex-start;
    gap: 0.65rem;
    margin-block-end: 0.95rem;
  }

  .settings-section-card__icon {
    display: grid;
    flex: 0 0 auto;
    place-items: center;
    inline-size: 1.8rem;
    block-size: 1.8rem;
    border: 1px solid color-mix(in oklab, var(--border) 68%, transparent);
    border-radius: 0.45rem;
    color: color-mix(in oklab, var(--foreground) 88%, var(--primary));
    background: color-mix(in oklab, var(--background) 55%, transparent);
  }

  .settings-section-card__icon :global(svg) {
    inline-size: 0.95rem;
    block-size: 0.95rem;
  }

  .settings-section-card h3 {
    margin: 0;
    padding: 0;
    font-size: 0.98rem;
    line-height: 1.35;
  }

  .settings-section-card__header p {
    margin: 0.2rem 0 0;
    color: var(--muted-foreground);
    font-size: 0.76rem;
    line-height: 1.5;
  }
</style>
