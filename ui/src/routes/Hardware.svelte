<script lang="ts">
  import { onMount } from "svelte";
  import { getHardware } from "../stores/api";
  import type { HardwareAccelerator, HardwareSnapshot } from "../lib/types";
  import { formatCapacity } from "../lib/format";
  import { copyText } from "../lib/clipboard";
  import { Check, Copy } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Tabs, TabsContent, TabsList, TabsTrigger } from "$lib/components/ui/tabs/index.js";
  import { locale, translate } from "../lib/i18n";

  let hardware = $state<HardwareSnapshot | null>(null);
  let loading = $state(true);
  let error = $state("");
  let copied = $state(false);

  onMount(async () => {
    try {
      hardware = await getHardware();
    } catch (cause) {
      error = cause instanceof Error ? cause.message : "";
    } finally {
      loading = false;
    }
  });

  function shown(value: string | number | null | undefined): string {
    return value === null || value === undefined || value === "" ? $translate("hardware.notDetected") : String(value);
  }

  function titleCase(value: string): string {
    return value.replaceAll("_", " ").replace(/\b\w/g, (letter) => letter.toUpperCase());
  }

  function osLabel(snapshot: HardwareSnapshot): string {
    return [snapshot.operating_system.name ?? titleCase(snapshot.operating_system.family), snapshot.operating_system.version]
      .filter(Boolean)
      .join(" ");
  }

  function acceleratorTitle(accelerator: HardwareAccelerator): string {
    return accelerator.model ?? `${titleCase(accelerator.kind)} ${accelerator.index + 1}`;
  }

  function environmentLabel(snapshot: HardwareSnapshot): string {
    return `${titleCase(snapshot.environment.kind)}${snapshot.environment.name ? ` (${snapshot.environment.name})` : ""}${snapshot.environment.version ? ` ${snapshot.environment.version}` : ""}`;
  }

  function driverLabel(accelerator: HardwareAccelerator): string {
    return accelerator.driver
      ? [accelerator.driver.name, accelerator.driver.version].filter(Boolean).join(" ") || $translate("hardware.notDetected")
      : $translate("hardware.notDetected");
  }

  function acceleratorSummary(accelerator: HardwareAccelerator): string[] {
    return [
      `${$translate("hardware.accelerators")} ${accelerator.index + 1}: ${acceleratorTitle(accelerator)}`,
      `  ${$translate("hardware.type")}: ${titleCase(accelerator.kind)}`,
      `  ${$translate("hardware.vendor")}: ${shown(accelerator.vendor)}`,
      `  ${$translate("hardware.architecture")}: ${shown(accelerator.architecture)}`,
      `  ${$translate("hardware.memory")}: ${accelerator.memory.capacity_bytes ? formatCapacity(accelerator.memory.capacity_bytes, $translate("hardware.notDetected")) : $translate("hardware.notDetected")} (${titleCase(accelerator.memory.kind)})`,
      `  ${$translate("hardware.driver")}: ${driverLabel(accelerator)}`,
      `  ${$translate("hardware.powerLimit")}: ${accelerator.power_limit_watts === null ? $translate("hardware.notDetected") : `${accelerator.power_limit_watts} W`}`,
    ];
  }

  function hardwareSummary(snapshot: HardwareSnapshot): string {
    const acceleratorSection = snapshot.accelerators.length === 0
      ? []
      : [
          "",
          `${$translate("hardware.accelerators")} (${snapshot.accelerators.length})`,
          ...snapshot.accelerators.flatMap((accelerator, index) => [
            ...(index > 0 ? [""] : []),
            ...acceleratorSummary(accelerator),
          ]),
        ];

    return [
      $translate("hardware.textSummary"),
      "",
      $translate("hardware.system"),
      `  ${$translate("hardware.operatingSystem")}: ${osLabel(snapshot)}`,
      `  ${$translate("hardware.kernel")}: ${shown(snapshot.operating_system.kernel)}`,
      `  ${$translate("hardware.architecture")}: ${snapshot.architecture.name}`,
      `  ${$translate("hardware.environment")}: ${environmentLabel(snapshot)}`,
      `  ${$translate("hardware.systemMemory")}: ${formatCapacity(snapshot.memory.capacity_bytes, $translate("hardware.notDetected"))}`,
      "",
      $translate("hardware.cpu"),
      `  ${$translate("hardware.model")}: ${shown(snapshot.cpu.model)}`,
      `  ${$translate("hardware.vendor")}: ${shown(snapshot.cpu.vendor)}`,
      `  ${$translate("hardware.sockets")}: ${shown(snapshot.cpu.socket_count)}`,
      `  ${$translate("hardware.physicalCores")}: ${shown(snapshot.cpu.physical_core_count)}`,
      `  ${$translate("hardware.logicalThreads")}: ${shown(snapshot.cpu.logical_thread_count)}`,
      ...acceleratorSection,
    ].join("\n");
  }

  let summary = $derived.by(() => {
    $locale;
    return hardware ? hardwareSummary(hardware) : "";
  });

  async function copySummary() {
    if (await copyText(summary)) {
      copied = true;
      window.setTimeout(() => (copied = false), 2000);
    }
  }
</script>

<div class="p-2">
  <div class="mt-4 mb-4">
    <h3 class="text-lg font-semibold">{$translate("hardware.title")}</h3>
    <p class="text-sm text-muted-foreground">
      {$translate("hardware.experimentalBefore")} <a
        class="underline hover:text-foreground"
        href="https://github.com/mostlygeek/llama-swap/issues/977">{$translate("hardware.issue")}</a
      >{$translate("hardware.experimentalAfter")}
    </p>
  </div>

  {#if loading}
    <div class="rounded-lg border p-6 text-sm text-muted-foreground">{$translate("hardware.loading")}</div>
  {:else if error || !hardware}
    <div class="rounded-lg border border-destructive/50 p-6">
      <h4 class="font-semibold">{$translate("hardware.unavailable")}</h4>
      <p class="mt-1 text-sm text-muted-foreground">{error || $translate("hardware.noSnapshot")}</p>
    </div>
  {:else}
    <Tabs value="overview">
      <TabsList variant="line">
        <TabsTrigger value="overview">{$translate("hardware.overview")}</TabsTrigger>
        <TabsTrigger value="summary">{$translate("hardware.text")}</TabsTrigger>
      </TabsList>

      <TabsContent value="overview" class="mt-4">
        <div class="grid gap-4 lg:grid-cols-2">
          <section class="rounded-lg border p-4">
            <h4 class="mb-3 text-sm font-semibold text-muted-foreground">{$translate("hardware.system")}</h4>
            <dl class="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-2 text-sm">
              <dt class="text-muted-foreground">{$translate("hardware.operatingSystem")}</dt><dd>{osLabel(hardware)}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.kernel")}</dt><dd>{shown(hardware.operating_system.kernel)}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.architecture")}</dt><dd>{hardware.architecture.name}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.environment")}</dt><dd>{environmentLabel(hardware)}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.systemMemory")}</dt><dd>{formatCapacity(hardware.memory.capacity_bytes, $translate("hardware.notDetected"))}</dd>
            </dl>
          </section>

          <section class="rounded-lg border p-4">
            <h4 class="mb-3 text-sm font-semibold text-muted-foreground">{$translate("hardware.cpu")}</h4>
            <dl class="grid grid-cols-[minmax(8rem,auto)_1fr] gap-x-4 gap-y-2 text-sm">
              <dt class="text-muted-foreground">{$translate("hardware.model")}</dt><dd>{shown(hardware.cpu.model)}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.vendor")}</dt><dd>{shown(hardware.cpu.vendor)}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.sockets")}</dt><dd>{shown(hardware.cpu.socket_count)}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.physicalCores")}</dt><dd>{shown(hardware.cpu.physical_core_count)}</dd>
              <dt class="text-muted-foreground">{$translate("hardware.logicalThreads")}</dt><dd>{shown(hardware.cpu.logical_thread_count)}</dd>
            </dl>
          </section>
        </div>

        {#if hardware.accelerators.length > 0}
        <section class="mt-4 rounded-lg border p-4">
          <div class="mb-3 flex items-baseline justify-between gap-4">
            <h4 class="text-sm font-semibold text-muted-foreground">{$translate("hardware.accelerators")}</h4>
            <span class="text-xs text-muted-foreground">{$translate("hardware.detected", { count: hardware.accelerators.length })}</span>
          </div>
          {#if hardware.accelerators.length === 0}
            <p class="text-sm text-muted-foreground">{$translate("hardware.noAccelerators")}</p>
          {:else}
            <div class="grid gap-3 lg:grid-cols-2 2xl:grid-cols-3">
              {#each hardware.accelerators as accelerator (accelerator.index)}
                <article class="rounded-md border bg-muted/20 p-3">
                  <div class="mb-3">
                    <h5 class="font-medium">{acceleratorTitle(accelerator)}</h5>
                    <p class="text-xs text-muted-foreground">{shown(accelerator.vendor)} · {titleCase(accelerator.kind)}</p>
                  </div>
                  <dl class="grid grid-cols-[auto_1fr] gap-x-3 gap-y-1.5 text-sm">
                    <dt class="text-muted-foreground">{$translate("hardware.architecture")}</dt><dd>{shown(accelerator.architecture)}</dd>
                    <dt class="text-muted-foreground">{$translate("hardware.memory")}</dt>
                    <dd>{accelerator.memory.capacity_bytes ? formatCapacity(accelerator.memory.capacity_bytes, $translate("hardware.notDetected")) : $translate("hardware.notDetected")} ({titleCase(accelerator.memory.kind)})</dd>
                    <dt class="text-muted-foreground">{$translate("hardware.driver")}</dt><dd>{driverLabel(accelerator)}</dd>
                    <dt class="text-muted-foreground">{$translate("hardware.powerLimit")}</dt>
                    <dd>{accelerator.power_limit_watts === null ? $translate("hardware.notDetected") : `${accelerator.power_limit_watts} W`}</dd>
                  </dl>
                </article>
              {/each}
            </div>
          {/if}
        </section>
        {/if}
      </TabsContent>

      <TabsContent value="summary" class="mt-4">
        <section class="rounded-lg border p-4">
          <div class="mb-3 flex items-center justify-between gap-4">
            <p class="text-sm text-muted-foreground">{$translate("hardware.summaryDescription")}</p>
            <Button variant="outline" size="sm" onclick={copySummary} title={$translate("hardware.copySummary")}>
              {#if copied}
                <Check /> {$translate("common.copied")}
              {:else}
                <Copy /> {$translate("common.copy")}
              {/if}
            </Button>
          </div>
          <textarea
            class="min-h-112 w-full resize-y rounded-md border bg-muted/20 p-3 font-mono text-sm leading-5"
            aria-label={$translate("hardware.textSummary")}
            readonly
            value={summary}
          ></textarea>
        </section>
      </TabsContent>
    </Tabs>
  {/if}
</div>
