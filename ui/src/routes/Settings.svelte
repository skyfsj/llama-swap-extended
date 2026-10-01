<script lang="ts">
  import { onMount } from "svelte";
  import {
    AlertTriangle,
    Eye,
    LoaderCircle,
    RefreshCw,
    Save,
    Search,
  } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import SettingsFieldGroups from "../components/settings/SettingsFieldGroups.svelte";
  import SettingsAppearanceCard from "../components/settings/SettingsAppearanceCard.svelte";
  import SettingsMatrixCard from "../components/settings/SettingsMatrixCard.svelte";
  import SettingsPreviewDialog from "../components/settings/SettingsPreviewDialog.svelte";
  import SettingsSidebar from "../components/settings/SettingsSidebar.svelte";
  import SettingsSystemCard from "../components/settings/SettingsSystemCard.svelte";
  import {
    fieldsForSettingsNavigation,
    groupSettingsFields,
    settingsNavigationFor,
  } from "../lib/settingsNavigation";
  import {
    draftChanges,
    updateAtConfig,
    validateSettingsField,
    valueAtConfig,
  } from "../lib/settingsDraft";
  import { record, summarizeChanges, type ConfigChange } from "../lib/modelConfig";
  import { settingsDirty } from "../stores/settingsGuard";
  import {
    commitSettings,
    fetchSettings,
    fetchSettingsOptions,
    fetchSettingsPermission,
    fetchSettingsSchema,
    previewSettings,
    type SettingsDiagnostic,
    type SettingsDraft,
    type SettingsField,
    type SettingsOption,
    type SettingsPermission,
    type SettingsPreview,
    type SettingsSchema,
    type SettingsSnapshot,
  } from "../lib/settingsApi";
  import { translate } from "../lib/i18n";

  let schema = $state<SettingsSchema | null>(null);
  let snapshot = $state<SettingsSnapshot | null>(null);
  let draftConfig = $state<Record<string, unknown>>({});
  let activeSection = $state("");
  let preview = $state<SettingsPreview | null>(null);
  let previewOpen = $state(false);
  let permission = $state<SettingsPermission | null>(null);
  let options = $state<Record<string, SettingsOption[]>>({});
  let optionLoading = $state<Record<string, boolean>>({});
  let fieldErrors = $state<Record<string, string>>({});
  let search = $state("");
  let loading = $state(true);
  let busy = $state(false);
  let error = $state("");
  let baseline = "";

  let navigationItems = $derived(settingsNavigationFor(schema?.sections ?? {}, $translate));
  let activeNavigation = $derived(navigationItems.find((item) => item.id === activeSection));
  let rawFields = $derived(fieldsForSettingsNavigation(schema?.sections ?? {}, activeSection));
  let fields = $derived(rawFields.filter((field) => matchesSearch(field, search)));
  let fieldGroups = $derived(groupSettingsFields(activeSection, fields, $translate));
  let dirty = $derived(JSON.stringify(draftConfig) !== baseline);
  let baselineConfig = $derived(baseline ? JSON.parse(baseline) as Record<string, unknown> : {});
  let previewChanges = $derived<ConfigChange[]>(summarizeChanges(baselineConfig, draftConfig));
  let modelOptions = $derived(options.models ?? []);

  // Keep the app shell informed so it can warn before in-app navigation away
  // from Settings while this draft has unsaved changes.
  $effect(() => {
    settingsDirty.set(dirty);
    return () => settingsDirty.set(false);
  });

  // Browser-level leave (refresh / close / tab close) is guarded directly.
  $effect(() => {
    if (!dirty) return;
    const handler = (event: BeforeUnloadEvent): void => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", handler);
    return () => window.removeEventListener("beforeunload", handler);
  });

  function matchesSearch(field: SettingsField, query: string): boolean {
    const normalized = query.trim().toLowerCase();
    if (!normalized) return true;
    return `${field.label} ${field.path} ${field.hint ?? ""}`.toLowerCase().includes(normalized);
  }

  function valueAt(path: string): unknown {
    return valueAtConfig(draftConfig, path);
  }

  function updateAt(path: string, value: unknown): void {
    draftConfig = updateAtConfig(draftConfig, path, value);
    preview = null;
  }

  function update(field: SettingsField, value: unknown): void {
    updateAt(field.path, value);
    const next = { ...fieldErrors };
    const message = validateSettingsField($translate, field, value);
    if (message) next[field.path] = message;
    else delete next[field.path];
    fieldErrors = next;
    // Once every field error is resolved, drop the generic "fix the errors"
    // banner so a corrected form reads as clean again.
    if (Object.keys(next).length === 0 && error === $translate("settingsCenter.validation.fixErrors")) {
      error = "";
    }
  }

  function models(): Record<string, unknown> {
    const value = draftConfig.models;
    return value && typeof value === "object" && !Array.isArray(value)
      ? value as Record<string, unknown>
      : {};
  }

  // The matrix editor reads and writes the block the selected router actually
  // uses: the canonical routing tree, or the legacy top-level key while it is
  // still the one carrying the matrix.
  let routerUse = $derived(String(valueAt("/routing/router/use") ?? ""));
  let matrixValue = $derived(
    valueAt("/routing/router/settings/matrix") ?? valueAt("/matrix"),
  );
  let matrixPath = $derived(valueAt("/routing/router/settings/matrix") !== undefined ? "/routing/router/settings/matrix" : "/matrix");

  // Local model IDs, read straight from the draft: the vars table needs the
  // full configured set, not the provider-filtered picker options.
  let matrixModelOptions = $derived(Object.keys(models()).sort((left, right) => left.localeCompare(right, undefined, { numeric: true })));

  function updateModels(value: Record<string, unknown>): void {
    updateAt("/models", value);
  }

  function selectSection(id: string): void {
    activeSection = id;
    search = "";
    preview = null;
  }

  async function loadOptions(provider: string): Promise<void> {
    optionLoading = { ...optionLoading, [provider]: true };
    try {
      const result = await fetchSettingsOptions(provider);
      options = { ...options, [provider]: result.items ?? [] };
    } catch {
      // The form remains directly editable when an optional provider is absent.
    } finally {
      optionLoading = { ...optionLoading, [provider]: false };
    }
  }

  function allValid(): boolean {
    const next: Record<string, string> = {};
    for (const sectionFields of Object.values(schema?.sections ?? {})) {
      for (const field of sectionFields) {
        if (field.hidden) continue;
        const message = validateSettingsField($translate, field, valueAt(field.path));
        if (message) next[field.path] = message;
      }
    }
    fieldErrors = next;
    return Object.keys(next).length === 0;
  }

  // Anchors the server's pathed validation diagnostics onto the fields they
  // describe so each setting shows its own message underneath, while issues
  // without a known field path are returned for the banner. Several fields
  // can carry simultaneous errors.
  function absorbDiagnostics(cause: unknown): string {
    const diagnostics = (cause as { diagnostics?: SettingsDiagnostic[] } | null)?.diagnostics;
    if (!Array.isArray(diagnostics) || !diagnostics.length) return "";
    const known = new Set<string>();
    for (const sectionFields of Object.values(schema?.sections ?? {})) {
      for (const field of sectionFields) known.add(field.path);
    }
    const next = { ...fieldErrors };
    const unanchored: string[] = [];
    let anchored = 0;
    for (const diagnostic of diagnostics) {
      const message = diagnostic?.message?.trim();
      if (!message) continue;
      if (diagnostic.path && known.has(diagnostic.path)) {
        next[diagnostic.path] = message;
        anchored += 1;
      } else {
        unanchored.push(message);
      }
    }
    fieldErrors = next;
    if (unanchored.length) return unanchored.join("; ");
    if (anchored) return $translate("settingsCenter.validation.fixErrors");
    return "";
  }

  function draft(): SettingsDraft {
    return { mode: "structured", changes: draftChanges(baseline, draftConfig), message: "settings center" };
  }

  async function load(): Promise<void> {
    loading = true;
    error = "";
    try {
      const [nextSchema, nextSnapshot, nextPermission] = await Promise.all([
        fetchSettingsSchema(),
        fetchSettings(),
        fetchSettingsPermission().catch(() => null),
      ]);
      schema = nextSchema;
      snapshot = nextSnapshot;
      permission = nextPermission;
      draftConfig = structuredClone(nextSnapshot.config ?? {});
      baseline = JSON.stringify(nextSnapshot.config ?? {});
      const availableNavigation = settingsNavigationFor(nextSchema.sections);
      activeSection = availableNavigation.some((item) => item.id === activeSection)
        ? activeSection
        : availableNavigation[0]?.id ?? "";
      for (const provider of new Set(
        Object.values(nextSchema.sections)
          .flat()
          .map((field) => field.provider)
          .filter((provider): provider is string => Boolean(provider)),
      )) void loadOptions(provider);
      preview = null;
      fieldErrors = {};
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      loading = false;
    }
  }

  // Re-sync the draft after a model dialog (edit/create) closes: those
  // dialogs commit straight to the server, so the page's draft and etag can
  // be stale. With a clean draft this is a plain reload; with unsaved edits
  // the server's models section wins while the other sections keep the
  // local edits.
  async function syncFromServer(): Promise<void> {
    if (busy) return;
    try {
      const nextSnapshot = await fetchSettings();
      const nextModels = record(nextSnapshot.config?.models);
      const previousModels = record(snapshot?.config?.models);
      snapshot = nextSnapshot;
      if (!dirty) {
        draftConfig = structuredClone(nextSnapshot.config ?? {});
        baseline = JSON.stringify(nextSnapshot.config ?? {});
        preview = null;
        fieldErrors = {};
        return;
      }
      if (JSON.stringify(nextModels) !== JSON.stringify(previousModels)) {
        draftConfig = { ...draftConfig, models: structuredClone(nextModels) };
      }
    } catch (cause) {
      console.warn("settings: failed to sync config after model dialog", cause);
    }
  }

  async function doPreview(): Promise<void> {
    if (!snapshot || busy || !allValid()) {
      if (!allValid()) error = $translate("settingsCenter.validation.fixErrors");
      return;
    }
    busy = true;
    error = "";
    try {
      preview = await previewSettings(draft());
      previewOpen = true;
    } catch (cause) {
      error = absorbDiagnostics(cause)
        || (cause instanceof Error ? cause.message : String(cause));
    } finally {
      busy = false;
    }
  }

  async function save(): Promise<void> {
    if (!snapshot || busy || !dirty) return;
    if (!allValid()) {
      error = $translate("settingsCenter.validation.fixErrors");
      return;
    }
    busy = true;
    error = "";
    try {
      const checked = await previewSettings(draft());
      preview = checked;
      if (!checked.valid) {
        previewOpen = true;
        return;
      }
      const result = await commitSettings(snapshot, draft(), checked.previewHash);
      snapshot = result.snapshot;
      draftConfig = structuredClone(result.snapshot.config ?? {});
      baseline = JSON.stringify(result.snapshot.config ?? {});
      preview = result.preview;
      previewOpen = false;
    } catch (cause) {
      const status = (cause as { status?: number }).status;
      if (status === 412) {
        error = $translate("settingsCenter.validation.conflict");
        return;
      }
      error = absorbDiagnostics(cause)
        || (cause instanceof Error ? cause.message : String(cause));
    } finally {
      busy = false;
    }
  }

  onMount(() => {
    void load();
  });
</script>

<div class="settings-root">
  <header class="settings-header">
    <div class="settings-header__titles">
      <div class="settings-header__row">
        <h1>{$translate("settings.title")}</h1>
        {#if dirty}
          <span class="settings-dirty">{$translate("settingsCenter.state.unsaved")}</span>
        {/if}
      </div>
      <p>{$translate("settingsCenter.description")}</p>
    </div>
    <div class="settings-header__toolbar" aria-label={$translate("settingsCenter.toolbar.actions")}>
      <div class="settings-search">
        <Search aria-hidden="true" />
        <Input
          aria-label={$translate("settingsCenter.search.label")}
          placeholder={$translate("settingsCenter.search.placeholder")}
          value={search}
          oninput={(event) => (search = event.currentTarget.value)}
        />
      </div>
      <Button variant="outline" size="sm" onclick={() => void load()} disabled={loading || busy}>
        <RefreshCw class={loading ? "animate-spin" : ""} aria-hidden="true" />{$translate("common.refresh")}
      </Button>
      <Button variant="outline" size="sm" onclick={() => void doPreview()} disabled={busy || !dirty || Object.keys(fieldErrors).length > 0}>
        <Eye aria-hidden="true" />{$translate("settingsCenter.toolbar.preview")}
      </Button>
      <Button size="sm" onclick={() => void save()} disabled={!snapshot?.writable || permission?.write === false || !dirty || busy || Object.keys(fieldErrors).length > 0}>
        <Save aria-hidden="true" />{$translate("common.save")}
      </Button>
    </div>
  </header>

  <div class="settings-body">
    <SettingsSidebar items={navigationItems} activeID={activeSection} onSelect={selectSection} />

    <div class="settings-content settings-form">
      <div class="settings-canvas">
        {#if activeNavigation}
          <div class="settings-section-intro">
            <h2>{activeNavigation.label}</h2>
            <p>{activeNavigation.description}</p>
          </div>
        {/if}

        {#if error}
          <div class="settings-alert settings-alert--error" role="alert">
            <AlertTriangle aria-hidden="true" />
            <span>{snapshot ? error : `${error} — ${$translate("settingsCenter.state.loadFailed")}`}</span>
          </div>
        {/if}

        {#if loading && !snapshot}
          <div class="settings-loading" role="status">
            <LoaderCircle class="animate-spin" aria-hidden="true" />
            <span>{$translate("settingsCenter.state.loading")}</span>
          </div>
        {:else if !snapshot}
          {#if !error}
            <div class="settings-empty">{$translate("settingsCenter.state.loadFailed")}</div>
          {/if}
        {:else}
          <div class="settings-section-stack">
            {#if fieldGroups.length === 0}
              <div class="settings-empty">{search ? $translate("settingsCenter.state.noMatchingFields") : $translate("settingsCenter.state.noEditableFields")}</div>
            {:else}
              <SettingsFieldGroups
                groups={fieldGroups}
                models={models()}
                writable={snapshot.writable && permission?.write !== false}
                modelOptions={modelOptions}
                {options}
                {optionLoading}
                {fieldErrors}
                {valueAt}
                {update}
                {updateModels}
                onModelsExternalChange={() => void syncFromServer()}
              />
            {/if}

            {#if activeSection === "general"}
              <SettingsAppearanceCard />
            {/if}

            {#if activeSection === "routing" && routerUse === "matrix"}
              <!--
                The matrix is the one routing engine with no schema-driven
                fields, so it gets a purpose-built editor: vars, evict costs and
                a combination table per set. It writes back to
                routing.router.settings.matrix, and falls back to the legacy
                top-level key only while that key is still what the config uses.
              -->
              <SettingsMatrixCard
                value={matrixValue}
                modelOptions={matrixModelOptions}
                disabled={!snapshot.writable || permission?.write === false}
                onChange={(next) => { updateAt(matrixPath, next); }}
              />
            {/if}

            {#if activeSection === "advanced"}
              <SettingsSystemCard />
            {/if}
          </div>
        {/if}
      </div>
    </div>
  </div>
</div>

<SettingsPreviewDialog
  bind:open={previewOpen}
  {preview}
  changes={previewChanges}
  {busy}
  onSave={() => void save()}
/>

<style>
  .settings-root {
    display: flex;
    flex-direction: column;
    block-size: 100%;
    min-block-size: 0;
    background: var(--background);
  }

  .settings-header {
    display: flex;
    flex-wrap: wrap;
    align-items: flex-start;
    justify-content: space-between;
    gap: 0.9rem 1.25rem;
    flex: 0 0 auto;
    padding: 0.85rem 1.25rem;
    border-block-end: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    background: color-mix(in oklab, var(--background) 96%, black 4%);
  }

  .settings-header__row {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.6rem;
  }

  .settings-header h1 {
    margin: 0;
    padding: 0;
    font-size: 1.35rem;
    line-height: 1.2;
  }

  .settings-header__titles p {
    margin: 0.25rem 0 0;
    color: var(--muted-foreground);
    font-size: 0.8rem;
  }

  .settings-dirty {
    border: 1px solid color-mix(in oklab, var(--warning) 55%, transparent);
    border-radius: 999px;
    background: color-mix(in oklab, var(--warning) 12%, transparent);
    color: var(--warning);
    padding: 0.15rem 0.55rem;
    font-size: 0.7rem;
    font-weight: 700;
  }

  .settings-header__toolbar {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    justify-content: flex-end;
    gap: 0.5rem;
  }

  .settings-search {
    display: flex;
    align-items: center;
    gap: 0.45rem;
    inline-size: clamp(11rem, 22vw, 17rem);
    padding-inline: 0.6rem;
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.5rem;
    background: color-mix(in oklab, var(--background) 64%, transparent);
  }

  .settings-search :global(svg) {
    flex: 0 0 auto;
    inline-size: 0.95rem;
    color: var(--muted-foreground);
  }

  .settings-search :global([data-slot="input"]) {
    block-size: 2.1rem;
    border: 0;
    background: transparent;
    box-shadow: none;
  }

  .settings-search :global([data-slot="input"]:focus-visible) {
    box-shadow: none;
  }

  .settings-body {
    display: flex;
    flex: 1 1 auto;
    min-block-size: 0;
    overflow: hidden;
  }

  .settings-content {
    flex: 1 1 auto;
    min-inline-size: 0;
    overflow-y: auto;
  }

  .settings-canvas {
    box-sizing: border-box;
    inline-size: min(100%, 88rem);
    margin: 0 auto;
    padding: 1rem 1.25rem 2.25rem;
  }

  .settings-section-intro {
    margin-block-end: 1rem;
  }

  .settings-section-intro h2 {
    margin: 0;
    padding: 0;
    font-size: 1.15rem;
  }

  .settings-section-intro p {
    margin: 0.2rem 0 0;
    color: var(--muted-foreground);
    font-size: 0.8rem;
  }

  .settings-section-stack {
    display: grid;
    gap: 0.8rem;
  }

  .settings-alert {
    display: flex;
    align-items: flex-start;
    gap: 0.55rem;
    margin-block: 0.75rem;
    border: 1px solid color-mix(in oklab, var(--success) 38%, var(--border));
    border-radius: 0.6rem;
    background: color-mix(in oklab, var(--success) 9%, transparent);
    padding: 0.75rem 0.9rem;
    color: var(--foreground);
    font-size: 0.82rem;
  }

  .settings-alert :global(svg) {
    flex: 0 0 auto;
    inline-size: 0.95rem;
    margin-block-start: 0.12rem;
    color: var(--success);
  }

  .settings-alert--error {
    border-color: color-mix(in oklab, var(--destructive) 48%, var(--border));
    background: color-mix(in oklab, var(--destructive) 10%, transparent);
  }

  .settings-alert--error :global(svg) {
    color: var(--destructive);
  }

  .settings-loading,
  .settings-empty {
    display: flex;
    align-items: center;
    justify-content: center;
    gap: 0.6rem;
    min-block-size: 12rem;
    border: 1px dashed color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.7rem;
    color: var(--muted-foreground);
    font-size: 0.85rem;
  }

  .settings-loading :global(svg) {
    inline-size: 1.05rem;
  }

  @media (max-width: 960px) {
    .settings-body {
      flex-direction: column;
      overflow-y: auto;
    }

    .settings-content {
      overflow: visible;
    }

    .settings-header {
      padding: 0.75rem 0.9rem;
    }

    .settings-search {
      inline-size: 100%;
      order: 3;
    }

    .settings-header__toolbar {
      inline-size: 100%;
    }
  }
</style>
