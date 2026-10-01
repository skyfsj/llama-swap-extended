<script lang="ts">
  import ExtensionValueEditor from "./ExtensionValueEditor.svelte";
  import SettingsFieldEditor from "../settings/SettingsFieldEditor.svelte";
  import { translate, locale } from "../../lib/i18n";
  import { settingSectionOf, settingsFieldLabel } from "../../lib/extensionFiles";
  import type { ExtensionSettingField } from "../../lib/extensionsApi";

  interface Props {
    settings: ExtensionSettingField[];
    config: Record<string, unknown>;
    errors?: Record<string, string>;
    onChange: (key: string, value: unknown) => void;
  }

  let { settings, config, errors = {}, onChange }: Props = $props();
  // Declared labels may be per-language objects; the console language wins.
  let currentLocale = $locale;

  const sections = $derived.by(() => {
    const grouped = new Map<string, ExtensionSettingField[]>();
    for (const field of settings) {
      const section = settingSectionOf(field);
      const bucket = grouped.get(section) ?? [];
      bucket.push(field);
      grouped.set(section, bucket);
    }
    return [...grouped.entries()];
  });

  /**
   * localized picks the console language when the declaration carries one,
   * falling back to the canonical label the server resolved.
   */
  function localized(key: string, label: string, translations?: Record<string, string>): string {
    return settingsFieldLabel(key, translations?.[currentLocale] ?? label);
  }
  /**
   * asSettingsField reshapes a declared setting into the shape the settings
   * center renderer consumes, so the extension form and the settings center
   * stay one visual language instead of growing a second form kit.
   */
  function asSettingsField(field: ExtensionSettingField) {
    return {
      path: field.key,
      label: localized(field.key, field.label, field.labelTranslations),
      hint: field.hintTranslations?.[currentLocale] ?? field.hint,
      component: field.component,
      enum: field.options,
      default: field.default,
      sensitive: field.secret,
      required: field.required,
      min: field.min,
      max: field.max,
    };
  }
</script>

<div class="settings-form space-y-5">
  {#if settings.length === 0}
    <p class="rounded-md border border-dashed border-border p-4 text-sm text-muted-foreground">
      {$translate("extensions.settings.empty")}
    </p>
  {:else}
    {#each sections as [section, fields] (section)}
      <section class="space-y-3">
        {#if section !== "general"}<p class="text-xs font-semibold text-muted-foreground">{section}</p>{/if}
        <div class="grid gap-4">
          {#each fields as field (field.key)}
            {#if field.component === "json"}
              <div class="space-y-2"><ExtensionValueEditor value={config[field.key] ?? field.default ?? {}} label={localized(field.key, field.label, field.labelTranslations)} allowedTypes={["object", "array"]} onChange={(value) => onChange(field.key, value)} />{#if field.hintTranslations?.[currentLocale] ?? field.hint}<p class="text-xs text-muted-foreground">{field.hintTranslations?.[currentLocale] ?? field.hint}</p>{/if}{#if errors[field.key]}<p role="alert" class="text-xs text-destructive">{errors[field.key]}</p>{/if}</div>
            {:else}
            <SettingsFieldEditor
              field={asSettingsField(field)}
              value={config[field.key]}
              error={errors[field.key] ?? ""}
              onChange={(value) => onChange(field.key, value)}
            />
            {/if}
          {/each}
        </div>
      </section>
    {/each}
  {/if}
</div>
