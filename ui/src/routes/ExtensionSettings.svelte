<script lang="ts">
  import { onMount } from "svelte";
  import { push } from "svelte-spa-router";
  import { translate } from "../lib/i18n";
  import { toast } from "../lib/toast";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import ConfirmDialog from "../components/ConfirmDialog.svelte";
  import ExtensionConfiguration from "../components/extensions/ExtensionConfiguration.svelte";
  import {
    getExtension,
    saveExtension,
    settingDiagnosticsFrom,
    type ExtensionDefinition,
  } from "../lib/extensionsApi";
  import {
    ArrowLeft,
    Save,
    RotateCcw,
    FileCode2,
    Sliders,
    AlertCircle,
    Copy,
    Check,
  } from "@lucide/svelte";

  interface Props {
    params?: { id?: string } | null;
  }

  let { params = {} }: Props = $props();

  let loading = $state(true);
  let saving = $state(false);
  let error = $state("");
  let settingsErrors = $state<Record<string, string>>({});
  let draft = $state<ExtensionDefinition | null>(null);
  let baseline = $state("");
  let copiedId = $state(false);
  let activeTab = $state("basic");

  // Navigation Guard
  let leaveOpen = $state(false);
  let pendingHref = $state("#/extensions");
  let allowNavigation = false;

  const dirty = $derived(
    draft !== null && JSON.stringify(draft.manifest) !== baseline,
  );

  function snapshot(manifest: ExtensionDefinition["manifest"]): string {
    return JSON.stringify(manifest);
  }

  async function load(id?: string) {
    if (!id) {
      error = "Extension ID is missing";
      loading = false;
      return;
    }
    loading = true;
    error = "";
    settingsErrors = {};
    try {
      const fetched = await getExtension(id);
      draft = fetched;
      baseline = snapshot(fetched.manifest);
    } catch (cause) {
      error = String(cause);
    } finally {
      loading = false;
    }
  }

  async function save() {
    if (!draft || saving) return;
    saving = true;
    error = "";
    settingsErrors = {};
    try {
      const saved = await saveExtension(draft);
      draft = saved;
      baseline = snapshot(saved.manifest);
      toast.success($translate("extensions.saved"));
    } catch (cause) {
      error = String(cause);
      settingsErrors = settingDiagnosticsFrom(cause);
    } finally {
      saving = false;
    }
  }

  function revert() {
    if (!draft || !baseline) return;
    try {
      draft.manifest = JSON.parse(baseline);
      settingsErrors = {};
      error = "";
      toast.info($translate("extensions.settings.revert"));
    } catch (cause) {
      error = String(cause);
    }
  }

  function handleBack() {
    if (dirty) {
      pendingHref = "#/extensions";
      leaveOpen = true;
    } else {
      void push("/extensions");
    }
  }

  function goToEditor() {
    if (!draft?.manifest.id) return;
    const href = `#/extensions/${encodeURIComponent(draft.manifest.id)}/editor`;
    if (dirty) {
      pendingHref = href;
      leaveOpen = true;
    } else {
      void push(`/extensions/${encodeURIComponent(draft.manifest.id)}/editor`);
    }
  }

  async function copyId() {
    if (!draft?.manifest.id) return;
    try {
      await navigator.clipboard.writeText(draft.manifest.id);
      copiedId = true;
      setTimeout(() => (copiedId = false), 2000);
    } catch {
      // ignore
    }
  }

  onMount(() => {
    let activeHash = window.location.hash;
    const guardHash = (event: Event) => {
      if (!dirty || allowNavigation || window.location.hash === activeHash) {
        activeHash = window.location.hash;
        return;
      }
      pendingHref = window.location.hash;
      event.stopImmediatePropagation();
      window.history.pushState(window.history.state, "", activeHash);
      leaveOpen = true;
    };
    window.addEventListener("hashchange", guardHash, true);

    const beforeUnload = (event: BeforeUnloadEvent) => {
      if (dirty) {
        event.preventDefault();
        event.returnValue = "";
      }
    };
    window.addEventListener("beforeunload", beforeUnload);

    return () => {
      window.removeEventListener("hashchange", guardHash, true);
      window.removeEventListener("beforeunload", beforeUnload);
    };
  });

  $effect(() => {
    const id = params?.id;
    if (id) void load(id);
  });
</script>

<svelte:window
  onkeydown={(event) => {
    if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") {
      event.preventDefault();
      if (dirty && !saving) void save();
    }
  }}
/>

<svelte:head>
  <title>
    {draft ? `${draft.manifest.name || draft.manifest.id} · ${$translate("extensions.settings.title")}` : $translate("extensions.settings.title")} · llama-swap
  </title>
</svelte:head>

<div class="flex h-full min-h-0 flex-col overflow-hidden rounded-lg border border-border bg-card">
  <!-- Header Bar -->
  <header class="flex shrink-0 flex-wrap items-center justify-between gap-3 border-b border-border bg-card/70 px-4 py-3 sm:px-6">
    <div class="flex items-center gap-3 min-w-0">
      <Button variant="ghost" size="icon" onclick={handleBack} aria-label={$translate("extensions.editor.back")}>
        <ArrowLeft class="size-4" />
      </Button>

      <div class="flex size-9 shrink-0 items-center justify-center rounded-lg bg-primary/10 text-primary">
        <Sliders class="size-5" />
      </div>

      <div class="min-w-0">
        <div class="flex items-center gap-2">
          <h1 class="truncate !pb-0 text-lg font-semibold leading-tight">
            {draft?.manifest.name || draft?.manifest.id || $translate("extensions.settings.title")}
          </h1>
          {#if draft}
            <Badge variant="outline" class="font-mono text-[11px] gap-1">
              <span>{draft.manifest.id}</span>
              <button
                type="button"
                class="hover:text-foreground text-muted-foreground ml-0.5"
                onclick={copyId}
                title={$translate("extensions.workspace.copyId")}
              >
                {#if copiedId}
                  <Check class="size-3 text-success" />
                {:else}
                  <Copy class="size-3" />
                {/if}
              </button>
            </Badge>

            <span
              class={`inline-flex items-center gap-1 rounded-full px-2 py-0.5 text-xs font-medium ${
                draft.status === "ready"
                  ? "bg-success/15 text-success"
                  : ["error", "invalid"].includes(draft.status)
                    ? "bg-destructive/15 text-destructive"
                    : "bg-muted text-muted-foreground"
              }`}
            >
              <span
                class={`size-1.5 rounded-full ${
                  draft.status === "ready"
                    ? "bg-success"
                    : ["error", "invalid"].includes(draft.status)
                      ? "bg-destructive"
                      : "bg-muted-foreground"
                }`}
              ></span>
              {$translate(draft.status === "ready" ? "extensions.workspace.enabled" : `extensions.status.${draft.status}`)}
            </span>
          {/if}
        </div>
        <p class="truncate text-xs text-muted-foreground mt-0.5">
          {$translate("extensions.settings.subtitle")}
        </p>
      </div>
    </div>

    <!-- Actions -->
    <div class="flex flex-wrap items-center gap-2">
      {#if draft}
        <div class="mr-2 flex items-center gap-2 border-r border-border pr-3">
          <Switch.Root
            bind:checked={draft.manifest.enabled}
            disabled={saving}
            aria-label={$translate("extensions.enabled")}
          />
          <span class="text-xs text-muted-foreground hidden sm:inline">
            {$translate(draft.manifest.enabled ? "extensions.workspace.enabled" : "extensions.workspace.disabled")}
          </span>
        </div>
      {/if}

      {#if dirty}
        <span class="rounded-full bg-warning/15 px-2.5 py-1 text-xs font-medium text-warning">
          {$translate("extensions.unsaved")}
        </span>
        <Button variant="ghost" size="sm" onclick={revert} disabled={saving}>
          <RotateCcw class="size-3.5 mr-1" />
          {$translate("extensions.settings.revert")}
        </Button>
      {/if}

      <Button variant="outline" size="sm" onclick={goToEditor}>
        <FileCode2 class="size-3.5 mr-1" />
        {$translate("extensions.settings.openInEditor")}
      </Button>

      <Button
        disabled={!dirty || saving || !draft}
        size="sm"
        onclick={save}
      >
        <Save class="size-3.5 mr-1" />
        {$translate("extensions.save")}
      </Button>
    </div>
  </header>

  <!-- Alerts -->
  {#if error}
    <div role="alert" class="flex items-center gap-2 border-b border-destructive/40 bg-destructive/10 px-4 py-2.5 text-xs text-destructive">
      <AlertCircle class="size-4 shrink-0" />
      <span class="flex-1 break-words">{error}</span>
    </div>
  {/if}

  <!-- Body Content -->
  <main class="min-h-0 flex-1 overflow-y-auto">
    {#if loading}
      <div class="flex h-64 items-center justify-center text-sm text-muted-foreground">
        {$translate("common.loading")}
      </div>
    {:else if draft}
      <div class="mx-auto w-full max-w-[1600px] px-4 py-3 sm:px-6 sm:py-4">
        <ExtensionConfiguration
          bind:draft
          bind:activeTab
          errors={settingsErrors}
          layout="tabs"
        />
      </div>
    {:else}
      <div class="flex h-64 flex-col items-center justify-center gap-3 text-center p-6">
        <AlertCircle class="size-8 text-muted-foreground" />
        <p class="text-sm font-medium">{$translate("extensions.workspace.loadFailed")}</p>
        <Button variant="outline" onclick={handleBack}>
          {$translate("extensions.editor.back")}
        </Button>
      </div>
    {/if}
  </main>
</div>

<ConfirmDialog
  bind:open={leaveOpen}
  title={$translate("extensions.unsaved")}
  message={$translate("extensions.workspace.leave")}
  confirmLabel={$translate("extensions.workspace.discard")}
  onConfirm={() => {
    leaveOpen = false;
    allowNavigation = true;
    window.location.hash = pendingHref;
  }}
/>
