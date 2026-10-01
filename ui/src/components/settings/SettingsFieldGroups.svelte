<script lang="ts">
  import {
    Activity,
    Boxes,
    CircleGauge,
    FileText,
    FolderCog,
    KeyRound,
    Network,
    Route,
    ServerCog,
    Settings2,
    ShieldCheck,
    SlidersHorizontal,
  } from "@lucide/svelte";
  import type { Component } from "svelte";
  import SettingsEntityEditor from "./SettingsEntityEditor.svelte";
  import SettingsFieldEditor from "./SettingsFieldEditor.svelte";
  import SettingsModelsPage from "./model/SettingsModelsPage.svelte";
  import SettingsSectionCard from "./SettingsSectionCard.svelte";
  import type { SettingsField, SettingsOption } from "../../lib/settingsApi";

  // Renders the editable field groups of one settings section. The draft
  // state stays in the route component; this component receives accessors so
  // each editor keeps reading and writing the shared draft.
  interface Props {
    groups: { id: string; title: string; description: string; fields: SettingsField[] }[];
    models: Record<string, unknown>;
    writable: boolean;
    modelOptions: SettingsOption[];
    options: Record<string, SettingsOption[]>;
    optionLoading: Record<string, boolean>;
    fieldErrors: Record<string, string>;
    valueAt: (path: string) => unknown;
    update: (field: SettingsField, value: unknown) => void;
    updateModels: (value: Record<string, unknown>) => void;
    onModelsExternalChange: () => void;
  }

  let {
    groups,
    models,
    writable,
    modelOptions,
    options,
    optionLoading,
    fieldErrors,
    valueAt,
    update,
    updateModels,
    onModelsExternalChange,
  }: Props = $props();

  const groupIcons: Record<string, Component> = {
    service: ServerCog,
    lifecycle: Settings2,
    sessions: Activity,
    storage: FolderCog,
    logging: FileText,
    metrics: CircleGauge,
    audit: ShieldCheck,
    pricing: CircleGauge,
    modelFiles: FolderCog,
    downloads: FileText,
    providers: Network,
    security: KeyRound,
    integrations: ShieldCheck,
    performance: CircleGauge,
    profiles: Route,
    selectors: Route,
    routing: Route,
    peers: Network,
    upstream: Network,
    models: Boxes,
    advanced: SlidersHorizontal,
    other: Settings2,
  };

  function iconForGroup(id: string): Component {
    return groupIcons[id] ?? Settings2;
  }
</script>

{#each groups as group (group.id)}
  <SettingsSectionCard
    id={group.id}
    icon={iconForGroup(group.id)}
    title={group.title}
    description={group.description}
    wide={group.id === "models"}
  >
    <div class="settings-fields-grid">
      {#each group.fields as field (field.path)}
        {#if field.editor === "model"}
          <div class="settings-model-list-wrap">
            <SettingsModelsPage
              {models}
              {writable}
              onChange={updateModels}
              onExternalChange={onModelsExternalChange}
            />
          </div>
        {:else if field.editor}
          <SettingsEntityEditor editor={field.editor} value={valueAt(field.path)} {modelOptions} onChange={(value) => update(field, value)} />
        {:else}
          <SettingsFieldEditor
            {field}
            value={valueAt(field.path)}
            options={field.provider ? options[field.provider] : undefined}
            loading={field.provider ? optionLoading[field.provider] : false}
            error={fieldErrors[field.path]}
            onChange={(value) => update(field, value)}
          />
        {/if}
      {/each}
    </div>
  </SettingsSectionCard>
{/each}

<style>
  .settings-fields-grid {
    display: grid;
    grid-template-columns: repeat(3, minmax(0, 1fr));
    gap: 0.9rem 1.05rem;
  }

  .settings-model-list-wrap {
    grid-column: 1 / -1;
    min-inline-size: 0;
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
