<script lang="ts">
  import { Activity } from "@lucide/svelte";
  import SettingsSectionCard from "./SettingsSectionCard.svelte";
  import { translate } from "../../lib/i18n";
  import { versionInfo } from "../../stores/api";
  import { connectionState } from "../../stores/theme";

  function shownBuildValue(value: string | undefined): string {
    return !value || value === "unknown" ? $translate("common.unknown") : value;
  }
</script>

<SettingsSectionCard
  id="system"
  icon={Activity}
  title={$translate("settingsCenter.system.title")}
  description={$translate("settingsCenter.system.description")}
>
  <dl>
    <div><dt>{$translate("settings.version")}</dt><dd>{shownBuildValue($versionInfo?.version)}</dd></div>
    <div><dt>{$translate("settings.commitHash")}</dt><dd>{shownBuildValue($versionInfo?.commit?.substring(0, 7))}</dd></div>
    <div><dt>{$translate("settings.eventStream")}</dt><dd>{$translate(`status.${$connectionState ?? "unknown"}`)}</dd></div>
  </dl>
</SettingsSectionCard>

<style>
  dl {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.65rem;
    margin: 0;
  }

  dl > div {
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.5rem;
    padding: 0.7rem;
    background: color-mix(in oklab, var(--background) 46%, transparent);
  }

  dt {
    color: var(--muted-foreground);
    font-size: 0.7rem;
  }

  dd {
    margin: 0.3rem 0 0;
    overflow-wrap: anywhere;
    font-size: 0.82rem;
    font-weight: 600;
  }

  @media (max-width: 640px) {
    dl {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
