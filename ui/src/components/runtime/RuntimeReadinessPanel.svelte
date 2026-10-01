<script lang="ts">
  import { AlertTriangle, Check, CircleAlert, CircleX } from "@lucide/svelte";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { translate } from "$lib/i18n";
  import { readinessLevel } from "$lib/runtimeCenter";
  import type { RuntimeReadiness, RuntimeReadinessCheck, RuntimeReadinessLevel } from "$lib/types";

  interface Props {
    readiness: RuntimeReadiness;
    compact?: boolean;
    headingId?: string;
  }

  let { readiness, compact = false, headingId = "runtime-readiness-title" }: Props = $props();
  let overall = $derived(readinessLevel(readiness.checks));

  function label(level: RuntimeReadinessLevel): string {
    return $translate(`controlPlane.runtimeCenter.readiness.${level}`);
  }

  function checkTitle(check: RuntimeReadinessCheck): string {
    const key = `controlPlane.runtimeCenter.checks.${check.code}`;
    const localized = $translate(key);
    return localized === key ? check.title : localized;
  }

  function checkDetail(check: RuntimeReadinessCheck): string {
    const key = `controlPlane.runtimeCenter.checkDetails.${check.code}`;
    const version = check.detail.match(/version [\"]([^\"]+)[\"]/i)?.[1] ?? check.detail;
    const modelList = check.detail.split(":").slice(1).join(":").trim();
    const count = check.detail.match(/^(\d+)/)?.[1] ?? "0";
    const budget = check.detail.match(/(\d+)\s+MiB used against a (\d+)\s+MiB budget/i);
    const localized = $translate(key, {
      version,
      models: modelList,
      count,
      used: budget?.[1] ?? "0",
      budget: budget?.[2] ?? "0",
    });
    return localized === key ? check.detail : localized;
  }

  function checkIcon(level: RuntimeReadinessLevel) {
    if (level === "block") return CircleX;
    if (level === "warning") return AlertTriangle;
    return Check;
  }

  function checkClass(level: RuntimeReadinessLevel): string {
    if (level === "block") return "border-destructive/35 bg-destructive/10 text-destructive";
    if (level === "warning") return "border-amber-500/35 bg-amber-500/10 text-amber-700 dark:text-amber-300";
    return "border-primary/25 bg-primary/5 text-foreground";
  }

  function visibleChecks(checks: RuntimeReadinessCheck[]): RuntimeReadinessCheck[] {
    if (!compact) return checks;
    const actionable = checks.filter((check) => check.level !== "pass");
    return actionable.slice(0, 1);
  }
</script>

<section class="grid gap-3" aria-labelledby={headingId}>
  <div class="flex flex-wrap items-center justify-between gap-2">
    <div class="flex items-center gap-2">
      <CircleAlert class="size-4 text-muted-foreground" aria-hidden="true" />
      <h3 id={headingId} class="text-sm font-semibold">{$translate("controlPlane.runtimeCenter.readiness.title")}</h3>
    </div>
    <Badge variant={overall === "block" ? "destructive" : overall === "warning" ? "secondary" : "default"}>
      {label(overall)}
    </Badge>
  </div>

  {#if !compact || visibleChecks(readiness.checks).length > 0}
    <ul class="grid gap-2" aria-live="polite">
      {#each visibleChecks(readiness.checks) as check (check.code)}
        {@const Icon = checkIcon(check.level)}
        <li class={`flex items-start gap-2 rounded-md border px-3 py-2 text-xs ${checkClass(check.level)}`}>
          <Icon class="mt-0.5 size-3.5 shrink-0" aria-hidden="true" />
          <span class="min-w-0">
            <span class="font-medium">{checkTitle(check)}</span>
            <span class="mt-0.5 block text-muted-foreground">{checkDetail(check)}</span>
          </span>
        </li>
      {/each}
    </ul>
  {/if}
</section>
