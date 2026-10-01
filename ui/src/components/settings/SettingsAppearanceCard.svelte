<script lang="ts">
  import { Settings2 } from "@lucide/svelte";
  import * as Label from "$lib/components/ui/label/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import SettingsSectionCard from "./SettingsSectionCard.svelte";
  import { locale, localeNames, setLocale, supportedLocales, translate, type Locale } from "../../lib/i18n";
  import { showCapabilityTags } from "../../stores/modelDisplay";
  import { themeName, themeMode, themes, type ThemeMode } from "../../stores/theme";

  const modes: { value: ThemeMode; labelKey: string }[] = [
    { value: "light", labelKey: "settings.light" },
    { value: "dark", labelKey: "settings.dark" },
    { value: "system", labelKey: "settings.system" },
  ];

  let themeLabel = $derived($translate(`settings.themeNames.${$themeName}`));
  let modeLabel = $derived($translate(`settings.${$themeMode}`));
</script>

<SettingsSectionCard
  id="appearance"
  icon={Settings2}
  title={$translate("settings.appearance")}
  description={$translate("settingsCenter.appearanceDescription")}
>
  <div class="settings-fields-grid">
    <div class="settings-local-field">
      <Label.Root for="settings-theme">{$translate("settings.theme")}</Label.Root>
      <Select.Root type="single" value={$themeName} onValueChange={(value) => value && themeName.set(value as typeof $themeName)}>
        <Select.Trigger id="settings-theme" class="w-full">{themeLabel}</Select.Trigger>
        <Select.Content>
          {#each themes as theme (theme.value)}
            <Select.Item value={theme.value}>{$translate(`settings.themeNames.${theme.value}`)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="settings-local-field">
      <Label.Root for="settings-mode">{$translate("settings.mode")}</Label.Root>
      <Select.Root type="single" value={$themeMode} onValueChange={(value) => value && themeMode.set(value as ThemeMode)}>
        <Select.Trigger id="settings-mode" class="w-full">{modeLabel}</Select.Trigger>
        <Select.Content>
          {#each modes as option (option.value)}
            <Select.Item value={option.value}>{$translate(option.labelKey)}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="settings-local-field">
      <Label.Root for="settings-language">{$translate("settings.language")}</Label.Root>
      <Select.Root type="single" value={$locale} onValueChange={(value) => value && setLocale(value as Locale)}>
        <Select.Trigger id="settings-language" class="w-full">{localeNames[$locale]}</Select.Trigger>
        <Select.Content>
          {#each supportedLocales as value (value)}
            <Select.Item value={value}>{localeNames[value]}</Select.Item>
          {/each}
        </Select.Content>
      </Select.Root>
    </div>
    <div class="settings-local-switch">
      <div>
        <Label.Root for="settings-capabilities">{$translate("settings.showCapabilityTags")}</Label.Root>
        <p>{$translate("settings.capabilityDescription")}</p>
      </div>
      <Switch.Root id="settings-capabilities" checked={$showCapabilityTags} onCheckedChange={(value) => showCapabilityTags.set(value)} />
    </div>
  </div>
</SettingsSectionCard>

<style>
  .settings-fields-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.9rem 1.05rem;
  }

  .settings-local-field {
    display: grid;
    gap: 0.45rem;
  }

  .settings-local-switch {
    grid-column: 1 / -1;
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 1rem;
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.55rem;
    padding: 0.75rem 0.85rem;
    background: color-mix(in oklab, var(--background) 45%, transparent);
  }

  .settings-local-switch p {
    max-inline-size: 46rem;
    margin: 0.25rem 0 0;
    color: var(--muted-foreground);
    font-size: 0.73rem;
    line-height: 1.45;
  }

  @media (max-width: 1100px) {
    .settings-fields-grid {
      grid-template-columns: repeat(2, minmax(0, 1fr));
    }
  }

  @media (max-width: 640px) {
    .settings-fields-grid {
      grid-template-columns: minmax(0, 1fr);
    }
  }
</style>
