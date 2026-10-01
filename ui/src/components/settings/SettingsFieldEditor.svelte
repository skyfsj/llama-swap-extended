<script lang="ts">
  import * as Label from "$lib/components/ui/label/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import SettingsListEditor from "./SettingsListEditor.svelte";
  import SettingsMapEditor from "./SettingsMapEditor.svelte";
  import ModelMultiSelect from "../ModelMultiSelect.svelte";
  import type { SettingsField, SettingsOption } from "../../lib/settingsApi";
  import { translate } from "../../lib/i18n";

  interface Props {
    field: SettingsField;
    value: unknown;
    options?: SettingsOption[];
    loading?: boolean;
    error?: string;
    onChange: (value: unknown) => void;
  }

  let { field, value, options = [], loading = false, error = "", onChange }: Props = $props();
  // A key the file does not define renders the catalogue default so the user
  // sees the effective value. The default stays out of the draft until the
  // user actually types, so saving an untouched field never materializes it.
  let hasStoredValue = $derived(value !== undefined && value !== null);
  let showsDefault = $derived(!hasStoredValue && field.default !== undefined && field.default !== null && field.default !== "");
  let textValue = $derived(
    hasStoredValue
      ? (typeof value === "number" && Number.isNaN(value) ? "" : String(value))
      : (typeof field.default === "number" || typeof field.default === "string" ? String(field.default) : "")
  );
  let wide = $derived(field.component === "list" || field.component === "map");
  // List values arrive as unknown (they come from parsed YAML); the editor only
  // ever hands the multi-select the string entries it can actually hold.
  let listValue = $derived(Array.isArray(value) ? (value as string[]) : []);
  // A provider-backed list gets its choices from the server, so it renders as a
  // multi-select instead of free-text rows where a typo is silently dropped by
  // the loader.
  let providerList = $derived(field.component === "list" && !!field.provider);

  // Sensitive values reach the editor as "[REDACTED]" placeholders. While the
  // field is untouched, show a fixed-length mask so a value is visible without
  // being read. Typing replaces the draft value; the server keeps the old
  // secret when the sentinel is submitted unchanged and writes new values.
  const REDACTED_MASK = "••••••••••••";
  let sensitiveTouched = $state(false);
  let isStructuredValue = $derived(value !== null && typeof value === "object");
  let sensitiveDisplay = $derived(
    !sensitiveTouched && typeof value === "string" && value.includes("[REDACTED]")
      ? REDACTED_MASK
      : value === undefined || value === null
        ? ""
        : String(value)
  );

  function updateText(raw: string): void {
    // Clearing a numeric or duration field drops the key from the draft so
    // the file falls back to the server default instead of storing "".
    if (field.component === "number") {
      if (!raw.trim()) { onChange(undefined); return; }
      onChange(Number(raw));
      return;
    }
    if (field.component === "duration") {
      onChange(raw.trim() || undefined);
      return;
    }
    onChange(raw);
  }

  function updateSensitive(raw: string): void {
    sensitiveTouched = true;
    onChange(raw);
  }
</script>

<div class="settings-field" class:settings-field--wide={wide}>
  <Label.Root for={`settings-${field.path}`}>
    {field.label}
    {#if showsDefault}<span class="text-muted-foreground ml-1 text-xs">{$translate("settingsCenter.field.default")}</span>{/if}
    {#if field.required}<span class="text-destructive ml-1">*</span>{/if}
  </Label.Root>

  {#if field.sensitive && !isStructuredValue}
    <Input
      id={`settings-${field.path}`}
      type="password"
      value={sensitiveDisplay}
      oninput={(event) => updateSensitive(event.currentTarget.value)}
      aria-invalid={error ? "true" : "false"}
    />
  {:else if field.sensitive}
    <div class="settings-sensitive-value text-muted-foreground flex items-center border px-3 text-sm" role="status">
      {value ? $translate("settingsCenter.field.configured") : $translate("settingsCenter.field.notConfigured")}
    </div>
  {:else if field.component === "switch"}
    {@const switchOn = value === true || (value === undefined && field.default === true)}
    <div class="settings-field__switch">
      <Switch.Root id={`settings-${field.path}`} checked={switchOn} onCheckedChange={onChange} />
      <span class="settings-field__switch-status">{switchOn ? $translate("settingsCenter.field.enabled") : $translate("settingsCenter.field.disabled")}</span>
    </div>
  {:else if field.provider || field.enum?.length}
    {@const selectValue = String(value ?? (typeof field.default === "string" && field.default ? field.default : undefined) ?? field.enum?.[0] ?? "")}
    <Select.Root type="single" value={selectValue} onValueChange={(next) => next && onChange(next)}>
      <Select.Trigger id={`settings-${field.path}`} class="w-full">
        <span class="truncate">{loading ? $translate("common.loading") : selectValue}</span>
      </Select.Trigger>
      <Select.Content>
        {#each (field.provider ? options : (field.enum ?? []).map((item) => ({ value: item, label: item }))) as option (option.value)}
          <Select.Item value={option.value}>{option.label}</Select.Item>
        {/each}
      </Select.Content>
    </Select.Root>
  {:else if providerList}
    <ModelMultiSelect
      value={listValue}
      options={options.map((option) => option.value)}
      ariaLabel={field.label}
      allLabel={$translate("settingsCenter.list.all")}
      selectedLabel={$translate("settingsCenter.list.selected", { count: listValue.length })}
      emptyLabel={$translate("settingsCenter.list.empty")}
      onValueChange={onChange}
    />
  {:else if field.component === "list"}
    <SettingsListEditor value={value} onChange={onChange} />
  {:else if field.component === "map"}
    <SettingsMapEditor value={value} onChange={(next) => onChange(next)} numeric={field.path.includes("priority")} />
  {:else if field.component === "textarea" || field.component === "json"}
    <textarea
      id={`settings-${field.path}`}
      class={field.component === "json"
        ? "min-h-24 w-full rounded-md border bg-background p-2 font-mono text-sm"
        : "min-h-20 w-full rounded-md border bg-background p-2 text-sm"}
      value={textValue}
      oninput={(event) => onChange(event.currentTarget.value)}
      aria-invalid={error ? "true" : "false"}
    ></textarea>
  {:else}
    <Input
      id={`settings-${field.path}`}
      type={field.component === "number" ? "number" : "text"}
      value={textValue}
      oninput={(event) => updateText(event.currentTarget.value)}
      aria-invalid={error ? "true" : "false"}
    />
  {/if}

  {#if field.hint}<p class="settings-field__hint">{field.hint}</p>{/if}
  {#if error}<p class="text-destructive text-xs" role="alert">{error}</p>{/if}
</div>
