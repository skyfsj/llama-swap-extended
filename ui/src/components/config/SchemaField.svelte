<script lang="ts">
  import { Plus, Trash2 } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import { childSchema, defaultForSchema, resolveSchema, schemaFieldLabel, type SchemaNode } from "../../lib/configSchema";
  import { translate } from "../../lib/i18n";
  import SchemaField from "./SchemaField.svelte";

  interface Props {
    schema?: SchemaNode;
    value: unknown;
    label?: string;
    onChange: (value: unknown) => void;
    depth?: number;
    fieldPath?: string;
    modelOptions?: string[];
    parentVariant?: string;
  }

  let { schema = true, value, label = "", onChange, depth = 0, fieldPath = "", modelOptions = [], parentVariant = "" }: Props = $props();
  let newKey = $state("");

  let resolved = $derived(resolveSchema(schema));
  let schemaType = $derived(typeof resolved.type === "string" ? resolved.type : "");
  let allowedTypes = $derived(Array.isArray(resolved.type) ? resolved.type.filter((item): item is string => typeof item === "string" && item !== "null") : []);
  let valueType = $derived(Array.isArray(value) ? "array" : value === null ? "string" : typeof value);
  let unionType = $derived(valueType === "number" && allowedTypes.includes("integer") ? "integer" : valueType);
  let type = $derived(schema === true ? "unknown" : schemaType || (allowedTypes.length > 0 ? "union" : resolved.properties || resolved.additionalProperties ? "object" : resolved.items ? "array" : valueType));
  let objectValue = $derived(value !== null && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : {});
  let arrayValue = $derived(Array.isArray(value) ? value : []);
  let allProperties = $derived((resolved.properties ?? {}) as Record<string, SchemaNode>);
  let properties = $derived(visibleProperties(allProperties, fieldPath, parentVariant));
  let knownKeys = $derived(Object.keys(properties));
  let requiredKeys = $derived(new Set(Array.isArray(resolved.required) ? resolved.required.filter((key): key is string => typeof key === "string") : []));
  let presentKnownKeys = $derived(knownKeys.filter((key) => Object.prototype.hasOwnProperty.call(objectValue, key)));
  let availableKnownKeys = $derived(knownKeys.filter((key) => !Object.prototype.hasOwnProperty.call(objectValue, key)));
  let customKeys = $derived(Object.keys(objectValue).filter((key) => !Object.prototype.hasOwnProperty.call(allProperties, key)));
  let enums = $derived(Array.isArray(resolved.enum) ? resolved.enum : []);
  let modelValue = $derived(isModelValuePath(fieldPath));
  let modelKey = $derived(isModelKeyPath(fieldPath));
  let availableModelKeys = $derived(modelOptions.filter((option) => !Object.prototype.hasOwnProperty.call(objectValue, option)));

  function setObjectKey(key: string, next: unknown): void {
    onChange({ ...objectValue, [key]: next });
  }

  function removeObjectKey(key: string): void {
    const next = { ...objectValue };
    delete next[key];
    onChange(next);
  }

  function addKnownKey(key: string): void {
    setObjectKey(key, defaultForSchema(properties[key]));
  }

  function addCustomKey(): void {
    const key = newKey.trim();
    if (!key || Object.prototype.hasOwnProperty.call(objectValue, key)) return;
    setObjectKey(key, defaultForSchema(childSchema(schema, key)));
    newKey = "";
  }

  function updateArray(index: number, nextValue: unknown): void {
    const next = [...arrayValue];
    next[index] = nextValue;
    onChange(next);
  }

  function removeArray(index: number): void {
    onChange(arrayValue.filter((_, itemIndex) => itemIndex !== index));
  }

  function addArrayItem(): void {
    onChange([...arrayValue, defaultForSchema(resolved.items as SchemaNode | undefined)]);
  }

  function changeUnknownType(nextType: string): void {
    const defaults: Record<string, unknown> = { string: "", number: 0, integer: 0, boolean: false, object: {}, array: [] };
    onChange(defaults[nextType] ?? "");
  }

  function typeLabel(item: string): string {
    const labels: Record<string, string> = {
      string: "controlPlane.schemaText",
      number: "controlPlane.schemaNumber",
      integer: "controlPlane.schemaNumber",
      boolean: "controlPlane.schemaBoolean",
      object: "controlPlane.schemaObject",
      array: "controlPlane.schemaList",
    };
    return labels[item] ? $translate(labels[item]) : item;
  }

  function isModelValuePath(path: string): boolean {
    return path.endsWith(".preload[]")
      || path.endsWith(".members[]")
      || path.endsWith(".models[]") && !path.startsWith("peers.")
      || /\.vars\.[^.]+$/.test(path)
      || /\.gpus\.[^.]+\[\]$/.test(path);
  }

  function isModelKeyPath(path: string): boolean {
    return path.endsWith(".priority") || path.endsWith(".evict_costs");
  }

  function visibleProperties(input: Record<string, SchemaNode>, path: string, variant: string): Record<string, SchemaNode> {
    if (path.endsWith(".router.settings") && (variant === "group" || variant === "matrix" || variant === "gpus")) {
      const key = variant === "group" ? "groups" : variant;
      return input[key] === undefined ? {} : { [key]: input[key] };
    }
    if (path.endsWith(".scheduler.settings") && variant === "fifo") {
      return input.fifo === undefined ? {} : { fifo: input.fifo };
    }
    return input;
  }

  function childPath(key: string): string {
    return fieldPath ? `${fieldPath}.${key}` : key;
  }

  function childModelOptions(key: string): string[] {
    if (key !== "evict_costs") return modelOptions;
    const variables = objectValue.vars !== null && typeof objectValue.vars === "object" && !Array.isArray(objectValue.vars)
      ? Object.keys(objectValue.vars as Record<string, unknown>)
      : [];
    return [...new Set([...modelOptions, ...variables])];
  }

  function customKeyPlaceholder(): string {
    if (fieldPath.endsWith(".groups")) return $translate("controlPlane.schemaGroupName");
    if (fieldPath.endsWith(".vars")) return $translate("controlPlane.schemaVariableName");
    if (fieldPath.endsWith(".sets")) return $translate("controlPlane.schemaSetName");
    if (fieldPath.endsWith(".sources")) return $translate("controlPlane.schemaSourceName");
    if (fieldPath.endsWith(".gpus")) return $translate("controlPlane.schemaGpuCardName");
    return $translate("controlPlane.schemaFieldName");
  }

  function addCustomLabel(): string {
    if (fieldPath.endsWith(".groups")) return $translate("controlPlane.schemaAddGroup");
    if (fieldPath.endsWith(".vars")) return $translate("controlPlane.schemaAddVariable");
    if (fieldPath.endsWith(".sets")) return $translate("controlPlane.schemaAddSet");
    if (fieldPath.endsWith(".sources")) return $translate("controlPlane.schemaAddSource");
    if (fieldPath.endsWith(".gpus")) return $translate("controlPlane.schemaAddGpuCard");
    return $translate("controlPlane.schemaAddField");
  }

  function fieldLabel(key: string): string {
    return schemaFieldLabel(key, $translate);
  }

  function numericValue(raw: string): void {
    if (raw === "") { onChange(0); return; }
    const next = Number(raw);
    if (Number.isFinite(next)) onChange(next);
  }
</script>

{#if type === "object"}
  <div class="grid gap-3">
    {#if label}<div class="text-sm font-medium">{label}</div>{/if}
    {#each presentKnownKeys as key (key)}
      <div class="grid gap-2 border-b border-border/70 pb-3 last:border-b-0 last:pb-0">
        <div class="flex min-h-8 items-center gap-2">
          <span class="text-sm font-medium">{fieldLabel(key)}</span>
          {#if !requiredKeys.has(key)}<Button class="ml-auto" type="button" variant="ghost" size="icon-sm" title={$translate("controlPlane.schemaRemoveField")} onclick={() => removeObjectKey(key)}><Trash2 class="size-3.5" aria-hidden="true" /></Button>{/if}
        </div>
        <SchemaField schema={properties[key]} value={objectValue[key]} label="" depth={depth + 1} fieldPath={childPath(key)} modelOptions={childModelOptions(key)} parentVariant={typeof objectValue.use === "string" ? objectValue.use : ""} onChange={(next) => setObjectKey(key, next)} />
      </div>
    {/each}

    {#each customKeys as key (key)}
      <div class="border-border/70 grid gap-2 border-t pt-3">
        <div class="flex items-center gap-2"><code class="text-xs font-medium">{key}</code><Button class="ml-auto" type="button" variant="ghost" size="icon-sm" title={$translate("controlPlane.schemaRemoveField")} onclick={() => removeObjectKey(key)}><Trash2 class="size-3.5" aria-hidden="true" /></Button></div>
        <SchemaField schema={childSchema(schema, key)} value={objectValue[key]} depth={depth + 1} fieldPath={childPath(key)} {modelOptions} parentVariant={typeof objectValue.use === "string" ? objectValue.use : ""} onChange={(next) => setObjectKey(key, next)} />
      </div>
    {/each}

    {#if availableKnownKeys.length > 0}
      <div class="border-border/70 flex flex-wrap gap-2 border-t pt-3">
        {#each availableKnownKeys as key (key)}
          <Button type="button" variant="outline" size="sm" onclick={() => addKnownKey(key)}><Plus class="size-3.5" aria-hidden="true" />{fieldLabel(key)}</Button>
        {/each}
      </div>
    {/if}

    {#if resolved.additionalProperties !== undefined && resolved.additionalProperties !== false}
      <div class="border-border/70 flex flex-wrap items-center gap-2 border-t pt-3">
        {#if modelKey}
          <select class="border-input bg-background h-8 min-w-40 flex-1 rounded-md border pr-8 pl-2 text-sm" bind:value={newKey} aria-label={$translate("controlPlane.schemaSelectModel")}>
            <option value="">{$translate("controlPlane.schemaSelectModel")}</option>
            {#each availableModelKeys as option (option)}<option value={option}>{option}</option>{/each}
          </select>
        {:else}
          <Input class="h-8 min-w-40 flex-1" bind:value={newKey} placeholder={customKeyPlaceholder()} onkeydown={(event) => { if (event.key === "Enter") { event.preventDefault(); addCustomKey(); } }} />
        {/if}
        <Button type="button" variant="outline" size="sm" onclick={addCustomKey} disabled={!newKey.trim()}><Plus class="size-3.5" aria-hidden="true" />{addCustomLabel()}</Button>
      </div>
    {/if}
  </div>
{:else if type === "array"}
  <div class="grid gap-2">
    {#if label}<div class="text-sm font-medium">{label}</div>{/if}
    {#each arrayValue as item, index (index)}
      <div class="grid grid-cols-[minmax(0,1fr)_auto] items-start gap-2">
        <SchemaField schema={resolved.items as SchemaNode | undefined} value={item} label={isModelValuePath(`${fieldPath}[]`) ? "" : `${index + 1}`} depth={depth + 1} fieldPath={`${fieldPath}[]`} {modelOptions} onChange={(next) => updateArray(index, next)} />
        <Button type="button" variant="ghost" size="icon-sm" title={$translate("controlPlane.schemaRemoveItem")} onclick={() => removeArray(index)}><Trash2 class="size-3.5" aria-hidden="true" /></Button>
      </div>
    {/each}
    <Button class="justify-self-start" type="button" variant="outline" size="sm" onclick={addArrayItem}><Plus class="size-3.5" aria-hidden="true" />{$translate("controlPlane.schemaAddItem")}</Button>
  </div>
{:else if enums.length > 0}
  <label class="grid gap-1.5 text-sm">{#if label}<span class="font-medium">{label}</span>{/if}<select class="border-input bg-background h-9 w-full rounded-md border pr-9 pl-3 text-sm" value={String(value ?? "")} onchange={(event) => onChange(event.currentTarget.value)}>{#each enums as option (String(option))}<option value={String(option)}>{String(option)}</option>{/each}</select></label>
{:else if type === "union"}
  <div class="grid grid-cols-[7rem_minmax(0,1fr)] items-end gap-2">
    <label class="grid gap-1.5 text-xs"><span>{$translate("controlPlane.schemaType")}</span><select class="border-input bg-background h-9 rounded-md border pr-8 pl-2 text-sm" value={unionType} onchange={(event) => changeUnknownType(event.currentTarget.value)}>{#each allowedTypes as item (item)}<option value={item}>{typeLabel(item)}</option>{/each}</select></label>
    <SchemaField schema={{ type: unionType }} {value} {label} depth={depth + 1} {fieldPath} {modelOptions} {onChange} />
  </div>
{:else if type === "boolean"}
  <label class="border-border/70 flex min-h-10 items-center justify-between gap-3 rounded-md border px-3 text-sm">{#if label}<span class="font-medium">{label}</span>{/if}<Switch.Root class={!label ? "ml-auto" : ""} checked={value === true} onCheckedChange={(checked) => onChange(checked)} /></label>
{:else if type === "integer" || type === "number"}
  <label class="grid gap-1.5 text-sm">{#if label}<span class="font-medium">{label}</span>{/if}<Input type="number" value={typeof value === "number" ? value : 0} min={typeof resolved.minimum === "number" ? resolved.minimum : undefined} max={typeof resolved.maximum === "number" ? resolved.maximum : undefined} step={type === "integer" ? 1 : "any"} oninput={(event) => numericValue(event.currentTarget.value)} /></label>
{:else if type === "unknown"}
  <div class="grid grid-cols-[7rem_minmax(0,1fr)] items-end gap-2">
    <label class="grid gap-1.5 text-xs"><span>{$translate("controlPlane.schemaType")}</span><select class="border-input bg-background h-9 rounded-md border pr-8 pl-2 text-sm" value={valueType === "number" ? "number" : valueType} onchange={(event) => changeUnknownType(event.currentTarget.value)}><option value="string">{$translate("controlPlane.schemaText")}</option><option value="number">{$translate("controlPlane.schemaNumber")}</option><option value="boolean">{$translate("controlPlane.schemaBoolean")}</option><option value="object">{$translate("controlPlane.schemaObject")}</option><option value="array">{$translate("controlPlane.schemaList")}</option></select></label>
    {#if valueType === "boolean"}<label class="border-border/70 flex h-9 items-center justify-between rounded-md border px-3 text-sm"><span>{label}</span><Switch.Root checked={value === true} onCheckedChange={(checked) => onChange(checked)} /></label>
    {:else if valueType === "object" || valueType === "array"}<SchemaField schema={{ type: valueType }} {value} {label} depth={depth + 1} {fieldPath} {modelOptions} {onChange} />
    {:else}<label class="grid gap-1.5 text-sm">{#if label}<span class="font-medium">{label}</span>{/if}<Input value={typeof value === "string" || typeof value === "number" ? value : ""} type={valueType === "number" ? "number" : "text"} oninput={(event) => valueType === "number" ? numericValue(event.currentTarget.value) : onChange(event.currentTarget.value)} /></label>{/if}
  </div>
{:else if modelValue}
  <label class="grid gap-1.5 text-sm">
    {#if label}<span class="font-medium">{label}</span>{/if}
    <select class="border-input bg-background h-9 w-full rounded-md border pr-9 pl-3 text-sm" value={typeof value === "string" ? value : ""} aria-label={label || $translate("controlPlane.schemaSelectModel")} onchange={(event) => onChange(event.currentTarget.value)}>
      <option value="">{$translate("controlPlane.schemaSelectModel")}</option>
      {#if typeof value === "string" && value && !modelOptions.includes(value)}<option value={value}>{value}</option>{/if}
      {#each modelOptions as option (option)}<option value={option}>{option}</option>{/each}
    </select>
  </label>
{:else}
  <label class="grid gap-1.5 text-sm">{#if label}<span class="font-medium">{label}</span>{/if}<Input type={resolved.format === "password" ? "password" : "text"} autocomplete={resolved.format === "password" ? "new-password" : undefined} value={typeof value === "string" ? value : ""} oninput={(event) => onChange(event.currentTarget.value)} /></label>
{/if}
