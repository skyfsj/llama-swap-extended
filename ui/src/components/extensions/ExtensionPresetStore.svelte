<script lang="ts">
  import { translate } from "../../lib/i18n";
  import { installPreset, type ExtensionPreset } from "../../lib/extensionsApi";

  interface Props {
    presets: ExtensionPreset[];
    onInstalled: (id: string) => void;
  }

  let { presets, onInstalled }: Props = $props();

  let busyId = $state("");
  let error = $state("");
  let targets = $state<Record<string, string>>({});

  const groups = $derived.by(() => {
    const byCategory = new Map<string, ExtensionPreset[]>();
    for (const preset of presets) {
      const bucket = byCategory.get(preset.category) ?? [];
      bucket.push(preset);
      byCategory.set(preset.category, bucket);
    }
    return [...byCategory.entries()];
  });

  function targetFor(preset: ExtensionPreset): string {
    const custom = (targets[preset.id] ?? "").trim();
    return custom || preset.id;
  }

  async function install(preset: ExtensionPreset): Promise<void> {
    busyId = preset.id;
    error = "";
    try {
      await installPreset(preset.id, targetFor(preset));
      onInstalled(targetFor(preset));
    } catch (cause) {
      error = String(cause);
    } finally {
      busyId = "";
    }
  }

  function settingSummary(preset: ExtensionPreset): string {
    const fields = preset.settings ?? [];
    if (fields.length === 0) return "";
    return fields
      .map((field) => field.label + (field.secret ? " (" + $translate("extensions.presets.credential") + ")" : ""))
      .join(" · ");
  }
</script>

<div class="space-y-4">
  <div class="flex items-start justify-between gap-3">
    <div>
      <p class="text-lg font-semibold">{$translate("extensions.presets.title")}</p>
      <p class="mt-1 text-sm text-muted-foreground">{$translate("extensions.presets.subtitle")}</p>
    </div>
  </div>
  {#if error}<p role="alert" class="rounded-md border border-destructive/40 bg-destructive/10 p-2 text-xs text-destructive">{error}</p>{/if}
  {#if presets.length === 0}
    <p class="rounded-md border border-dashed border-border p-4 text-sm text-muted-foreground">{$translate("extensions.presets.empty")}</p>
  {/if}
  {#each groups as [category, items] (category)}
    <section class="space-y-2">
      <p class="text-xs font-semibold uppercase tracking-wide text-muted-foreground">
        {$translate(`extensions.presets.category.${category}`)}
      </p>
      <div class="grid gap-2 md:grid-cols-2">
        {#each items as preset (preset.id)}
          <article class="flex flex-col gap-2 rounded-md border border-border p-3">
            <header class="flex items-start justify-between gap-2">
              <div class="min-w-0">
                <p class="truncate text-sm font-medium">{preset.name}</p>
                <p class="mt-0.5 line-clamp-3 text-xs text-muted-foreground">{preset.description}</p>
              </div>
              {#if preset.installed}
                <span class="shrink-0 rounded-full bg-success/15 px-2 py-0.5 text-[10px] text-success">{$translate("extensions.presets.installed")}</span>
              {/if}
            </header>
            {#if (preset.tools ?? []).length > 0}
              <p class="flex flex-wrap gap-1 text-[10px] text-muted-foreground">
                {#each preset.tools as tool}
                  <span class="rounded bg-muted px-1.5 py-0.5 font-mono">{tool}</span>
                {/each}
              </p>
            {/if}
            {#if (preset.hosts ?? []).length > 0}
              <p class="truncate font-mono text-[10px] text-muted-foreground" title={(preset.hosts ?? []).join(", ")}>
                {(preset.hosts ?? []).join(", ")}
              </p>
            {/if}
            {#if settingSummary(preset)}
              <p class="text-[11px] text-muted-foreground">{settingSummary(preset)}</p>
            {/if}
            <div class="mt-auto flex items-center gap-2 pt-1">
              <input
                class="min-w-0 flex-1 rounded-md border border-border bg-background px-2 py-1 font-mono text-xs"
                value={targets[preset.id] ?? preset.id}
                oninput={(event) => targets = { ...targets, [preset.id]: event.currentTarget.value }}
                aria-label={$translate("extensions.presets.targetId")}
                disabled={busyId === preset.id}
              />
              <button
                class="shrink-0 rounded-md bg-primary px-2.5 py-1 text-xs text-primary-foreground disabled:opacity-50"
                disabled={busyId !== ""}
                onclick={() => void install(preset)}
              >{busyId === preset.id ? $translate("extensions.presets.installing") : $translate("extensions.presets.install")}</button>
            </div>
          </article>
        {/each}
      </div>
    </section>
  {/each}
</div>
