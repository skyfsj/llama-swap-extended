<script lang="ts">
  import { onMount } from "svelte";
  import { AlertTriangle, Download, KeyRound, LoaderCircle, RefreshCw, RotateCcw, Trash2, X } from "@lucide/svelte";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { formatCapacity } from "$lib/format";
  import { translate } from "$lib/i18n";
  import { toast } from "$lib/toast";
  import {
    cancelModelDownload,
    defaultDownloadSourceID,
    deleteModelDownload,
    downloadSourcesForProvider,
    enqueueModelDownload,
    getModelDownloadCredentials,
    getModelDownloads,
    retryModelDownload,
    updateModelDownloadCredentials,
  } from "$lib/modelDownloads";
  import type { ModelDownloadCredentials, ModelDownloadCredentialsPatch } from "$lib/modelDownloads";
  import type { ModelDownload, ModelFileSource } from "$lib/types";

  type DownloadProvider = "huggingface" | "modelscope";
  type PatternPreset = "custom" | "remembered" | "gguf" | "transformers";

  interface PatternMemory {
    include: string;
    exclude: string;
  }

  interface Props {
    sources?: ModelFileSource[];
    onTasksChange?: (tasks: ModelDownload[]) => void;
    onDownloadCompleted?: () => void;
  }

  const patternMemoryKey = "llama-swap:model-download-patterns:v1";
  const patternPresets: Record<Exclude<PatternPreset, "custom" | "remembered">, PatternMemory> = {
    gguf: {
      include: "*.gguf\ntokenizer.json\ntokenizer_config.json\nconfig.json",
      exclude: "*.md\noriginal/*",
    },
    transformers: {
      include: "*.safetensors\n*.bin\n*.model\ntokenizer.json\ntokenizer_config.json\nconfig.json",
      exclude: "*.md\noriginal/*",
    },
  };

  let { sources = [], onTasksChange, onDownloadCompleted }: Props = $props();
  let provider = $state<DownloadProvider>("huggingface");
  let repoID = $state("");
  let revision = $state("main");
  let destinationID = $state("");
  let include = $state("");
  let exclude = $state("");
  let patternPreset = $state<PatternPreset>("custom");
  let rememberedPatterns = $state<PatternMemory | null>(null);
  let tasks = $state<ModelDownload[]>([]);
  let tasksInitialized = false;
  let loading = $state(true);
  let submitting = $state(false);
  let busyID = $state("");
  // error is the transient queue-list load state (cleared by the next
  // successful poll); formError is a user-action failure that stays visible
  // until the next attempt or the dialog closes.
  let error = $state("");
  let formError = $state("");
  let credentialStatus = $state<ModelDownloadCredentials | null>(null);
  let downloadDialogOpen = $state(false);
  let cancelDialogOpen = $state(false);
  let cancelTarget = $state<ModelDownload | null>(null);
  let deleteDialogOpen = $state(false);
  let deleteTarget = $state<ModelDownload | null>(null);
  let deleteBusyID = $state("");
  let deleteError = $state("");
  let credentialsDialogOpen = $state(false);
  let credentials = $state<ModelDownloadCredentials | null>(null);
  let credentialsLoading = $state(false);
  let credentialsSaving = $state(false);
  let credentialsError = $state("");
  let hfToken = $state("");
  let modelScopeToken = $state("");
  let hfTokenTouched = $state(false);
  let modelScopeTokenTouched = $state(false);

  let activeTasks = $derived(tasks.filter((task) => task.status === "queued" || task.status === "retrying" || task.status === "downloading"));
  let destinationSources = $derived(downloadSourcesForProvider(sources, provider));
  let selectedDestination = $derived(destinationSources.find((source) => source.id === destinationID) ?? null);
  let providerCredential = $derived(
    credentialStatus ? (provider === "modelscope" ? credentialStatus.modelscope : credentialStatus.huggingface) : null,
  );
  // The download queue attaches the configured provider token to every
  // request; the form shows which credential source is active so a missing
  // key is visible before a gated repository fails.
  let providerCredentialActive = $derived(Boolean(providerCredential && (providerCredential.configured || providerCredential.environmentConfigured)));
  let providerCredentialLabel = $derived.by(() => {
    const status = providerCredential;
    if (!status) return $translate("modelFiles.download.apiKeyMissing");
    if (status.configured) return $translate("modelFiles.download.apiKeyConfigured");
    if (status.environmentConfigured) return $translate("modelFiles.download.apiKeyFromEnv", { name: status.environmentVariable });
    return $translate("modelFiles.download.apiKeyMissing");
  });

  $effect(() => {
    if (!destinationSources.some((source) => source.id === destinationID)) {
      destinationID = defaultDownloadSourceID(sources, provider);
    }
  });

  function providerLabel(value: string): string {
    return value === "modelscope" ? $translate("modelFiles.download.modelScope") : $translate("modelFiles.download.huggingFace");
  }

  function destinationLabel(source: ModelFileSource): string {
    if (source.configured) return source.name;
    if (source.type === "modelscope_cache") return $translate("modelFiles.modelScopeCache");
    if (source.type === "hf_cache") return $translate("modelFiles.hfCache");
    return source.name;
  }

  function taskDestinationLabel(task: ModelDownload): string {
    const source = sources.find((candidate) => candidate.id === task.source_id);
    return source ? destinationLabel(source) : task.source_id;
  }

  function changeProvider(value: string | undefined): void {
    if (value !== "huggingface" && value !== "modelscope") return;
    const previousDefault = provider === "modelscope" ? "master" : "main";
    const nextDefault = value === "modelscope" ? "master" : "main";
    if (!revision.trim() || revision === previousDefault) revision = nextDefault;
    provider = value;
  }

  function parsePatterns(value: string): string[] {
    return value.split(/[\n,]/).map((item) => item.trim()).filter(Boolean);
  }

  function readPatternMemory(): void {
    try {
      const value = JSON.parse(window.localStorage.getItem(patternMemoryKey) ?? "null") as unknown;
      if (!value || typeof value !== "object") return;
      const record = value as Record<string, unknown>;
      if (typeof record.include === "string" && typeof record.exclude === "string") {
        rememberedPatterns = { include: record.include, exclude: record.exclude };
      }
    } catch {
      rememberedPatterns = null;
    }
  }

  function rememberPatterns(): void {
    rememberedPatterns = { include, exclude };
    try {
      window.localStorage.setItem(patternMemoryKey, JSON.stringify(rememberedPatterns));
    } catch {
      // Storage can be unavailable in private or embedded browser contexts.
    }
  }

  function applyPatternPreset(value: string | undefined): void {
    if (value !== "custom" && value !== "remembered" && value !== "gguf" && value !== "transformers") return;
    patternPreset = value;
    const preset = value === "remembered"
      ? rememberedPatterns
      : value === "gguf" || value === "transformers"
        ? patternPresets[value]
        : null;
    if (preset) {
      include = preset.include;
      exclude = preset.exclude;
    }
  }

  function progress(task: ModelDownload): number {
    if (task.total_bytes > 0) return Math.min(100, Math.floor((task.downloaded_bytes / task.total_bytes) * 100));
    if (task.total_files > 0) return Math.min(100, Math.floor((task.completed_files / task.total_files) * 100));
    return 0;
  }

  function statusLabel(status: string): string {
    const key = status === "downloading" ? "downloading" : status === "retrying" ? "retrying" : status === "completed" ? "completed" : status === "failed" ? "failed" : status === "canceled" ? "canceled" : "queued";
    return $translate(`modelFiles.download.${key}`);
  }

  function retryTime(task: ModelDownload): string {
    if (!task.next_retry_at) return $translate("modelFiles.download.retrySoon");
    const value = new Date(task.next_retry_at);
    if (Number.isNaN(value.getTime())) return $translate("modelFiles.download.retrySoon");
    return new Intl.DateTimeFormat(undefined, { hour: "2-digit", minute: "2-digit", second: "2-digit" }).format(value);
  }

  // Success notes are ephemeral confirmations: they fade after a few seconds
  // and are dropped when the dialog closes so a stale note never reappears on
  // the next open.

  async function loadCredentialStatus(): Promise<void> {
    try {
      credentialStatus = await getModelDownloadCredentials();
    } catch {
      credentialStatus = null;
    }
  }

  function openDownload(): void {
    if (submitting) return;
    error = "";
    formError = "";
    downloadDialogOpen = true;
    void loadCredentialStatus();
  }

  function openCredentials(): void {
    if (credentialsLoading || credentialsSaving) return;
    credentialsError = "";
    credentialsDialogOpen = true;
    void loadCredentials();
  }

  async function loadCredentials(): Promise<void> {
    credentialsLoading = true;
    credentialsError = "";
    try {
      credentials = await getModelDownloadCredentials();
      hfToken = "";
      modelScopeToken = "";
      hfTokenTouched = false;
      modelScopeTokenTouched = false;
    } catch (cause) {
      credentialsError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      credentialsLoading = false;
    }
  }

  function closeCredentials(): void {
    if (credentialsSaving) return;
    credentialsDialogOpen = false;
    credentialsError = "";
  }

  async function saveCredentials(): Promise<void> {
    if (!credentials || credentialsSaving || !credentials.writable) return;
    const patch: ModelDownloadCredentialsPatch = {};
    if (hfTokenTouched) patch.hfToken = hfToken;
    if (modelScopeTokenTouched) patch.modelScopeToken = modelScopeToken;
    if (Object.keys(patch).length === 0) {
      closeCredentials();
      return;
    }
    credentialsSaving = true;
    credentialsError = "";
    try {
      credentials = await updateModelDownloadCredentials(patch, credentials.etag);
      credentialStatus = credentials;
      hfToken = "";
      modelScopeToken = "";
      hfTokenTouched = false;
      modelScopeTokenTouched = false;
      toast.success($translate("modelFiles.download.credentialsSaved"));
      credentialsDialogOpen = false;
    } catch (cause) {
      credentialsError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      credentialsSaving = false;
    }
  }

  async function loadTasks(): Promise<void> {
    try {
      const response = await getModelDownloads();
      const next = Array.isArray(response.data) ? response.data : [];
      if (tasksInitialized) {
        const previous = new Map(tasks.map((task) => [task.id, task.status]));
        if (next.some((task) => task.status === "completed" && previous.has(task.id) && previous.get(task.id) !== "completed")) {
          onDownloadCompleted?.();
        }
      }
      tasks = next;
      tasksInitialized = true;
      onTasksChange?.(next);
      error = "";
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      loading = false;
    }
  }

  async function submit(): Promise<void> {
    if (!repoID.trim()) {
      formError = $translate("modelFiles.download.repoRequired");
      return;
    }
    if (!destinationID) {
      formError = $translate("modelFiles.download.sourceRequired");
      return;
    }
    submitting = true;
    error = "";
    formError = "";
    try {
      const { duplicate } = await enqueueModelDownload({
        provider,
        repo_id: repoID.trim(),
        revision: revision.trim() || (provider === "modelscope" ? "master" : "main"),
        source_id: destinationID,
        include: parsePatterns(include),
        exclude: parsePatterns(exclude),
      });
      rememberPatterns();
      toast.success($translate(duplicate ? "modelFiles.download.alreadyQueued" : "modelFiles.download.queued"));
      repoID = "";
      await loadTasks();
    } catch (cause) {
      formError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      submitting = false;
    }
  }

  function requestCancel(task: ModelDownload): void {
    cancelTarget = task;
    cancelDialogOpen = true;
  }

  async function confirmCancel(): Promise<void> {
    const task = cancelTarget;
    if (!task) return;
    busyID = task.id;
    formError = "";
    try {
      await cancelModelDownload(task.id);
      await loadTasks();
    } catch (cause) {
      formError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      busyID = "";
      cancelDialogOpen = false;
      cancelTarget = null;
    }
  }

  function requestDelete(task: ModelDownload): void {
    deleteTarget = task;
    deleteError = "";
    deleteDialogOpen = true;
  }

  function closeDeleteDialog(): void {
    if (deleteBusyID !== "") return;
    deleteDialogOpen = false;
    deleteTarget = null;
    deleteError = "";
  }

  async function confirmDelete(): Promise<void> {
    const task = deleteTarget;
    if (!task || deleteBusyID !== "") return;
    deleteBusyID = task.id;
    deleteError = "";
    try {
      await deleteModelDownload(task.id);
      deleteDialogOpen = false;
      deleteTarget = null;
      toast.success($translate("modelFiles.download.deleted"));
      await loadTasks();
    } catch (cause) {
      deleteError = $translate("modelFiles.download.deleteFailed", { message: cause instanceof Error ? cause.message : String(cause) });
    } finally {
      deleteBusyID = "";
    }
  }

  async function retry(task: ModelDownload): Promise<void> {
    busyID = task.id;
    formError = "";
    try {
      await retryModelDownload(task.id);
      toast.success($translate("modelFiles.download.retried"));
      await loadTasks();
    } catch (cause) {
      formError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      busyID = "";
    }
  }

  onMount(() => {
    readPatternMemory();
    void loadTasks();
    const timer = window.setInterval(() => void loadTasks(), 1500);
    return () => {
      window.clearInterval(timer);
    };
  });
</script>

<div class="flex items-center gap-1">
  <Button size="sm" onclick={openDownload} disabled={submitting} aria-label={$translate("modelFiles.download.open")}>
    <Download />
    <span>{$translate("modelFiles.download.open")}</span>
    {#if activeTasks.length > 0}<Badge variant="secondary">{activeTasks.length}</Badge>{/if}
  </Button>
  <Button variant="outline" size="icon-sm" onclick={openCredentials} disabled={credentialsLoading || credentialsSaving} aria-label={$translate("modelFiles.download.credentialsOpen")} title={$translate("modelFiles.download.credentialsOpen")}>
    <KeyRound />
  </Button>
</div>

<Dialog.Root bind:open={downloadDialogOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-5xl overflow-y-auto sm:max-w-5xl" showCloseButton={!submitting}>
    <Dialog.Header>
      <Dialog.Title>{$translate("modelFiles.download.title")}</Dialog.Title>
    </Dialog.Header>

    <div class="grid min-h-0 gap-5 lg:grid-cols-[minmax(0,1.15fr)_minmax(340px,0.85fr)]">
      <form class="grid min-w-0 content-start gap-4" onsubmit={(event) => { event.preventDefault(); void submit(); }}>
        <fieldset class="grid gap-4" disabled={submitting}>
          <div class="grid gap-4 sm:grid-cols-2">
            <div class="grid gap-1.5 text-sm">
              <span id="model-download-provider-label" class="font-medium">{$translate("modelFiles.download.provider")}</span>
              <Select.Root type="single" value={provider} onValueChange={changeProvider}>
                <Select.Trigger class="h-9 w-full" aria-labelledby="model-download-provider-label">{providerLabel(provider)}</Select.Trigger>
                <Select.Content>
                  <Select.Item value="huggingface">{$translate("modelFiles.download.huggingFace")}</Select.Item>
                  <Select.Item value="modelscope">{$translate("modelFiles.download.modelScope")}</Select.Item>
                </Select.Content>
              </Select.Root>
              <p class={`text-xs ${providerCredentialActive ? "text-success" : "text-muted-foreground"}`}>{$translate("modelFiles.download.apiKeyLabel")}: {providerCredentialLabel}</p>
            </div>
            <label class="grid gap-1.5 text-sm">
              <span class="font-medium">{$translate("modelFiles.download.revision")}</span>
              <input class="border-input bg-background h-9 rounded-lg border px-3 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50" bind:value={revision} placeholder={provider === "modelscope" ? "master" : "main"} autocomplete="off" />
            </label>
          </div>
          <div class="grid gap-1.5 text-sm">
            <span id="model-download-destination-label" class="font-medium">{$translate("modelFiles.download.destination")}</span>
            {#if destinationSources.length > 0}
              <Select.Root type="single" value={destinationID} onValueChange={(value) => { if (value) destinationID = value; }}>
                <Select.Trigger class="h-9 w-full" aria-labelledby="model-download-destination-label">
                  {selectedDestination ? destinationLabel(selectedDestination) : $translate("modelFiles.download.chooseDestination")}
                </Select.Trigger>
                <Select.Content>
                  {#each destinationSources as source (source.id)}
                    <Select.Item value={source.id}>{destinationLabel(source)}</Select.Item>
                  {/each}
                </Select.Content>
              </Select.Root>
              {#if selectedDestination}
                <p class="text-muted-foreground truncate text-xs" title={selectedDestination.path}>
                  {$translate("modelFiles.download.destinationPath", { path: selectedDestination.path })}
                </p>
              {/if}
            {:else}
              <p class="text-muted-foreground rounded-lg border border-dashed px-3 py-2 text-xs" role="status">
                {$translate("modelFiles.download.noDestinations")}
              </p>
            {/if}
          </div>
          <label class="grid gap-1.5 text-sm">
            <span class="font-medium">{$translate("modelFiles.download.repo")}</span>
            <input class="border-input bg-background h-9 rounded-lg border px-3 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50" bind:value={repoID} placeholder="Qwen/Qwen3-8B" autocomplete="off" required />
          </label>
          <div class="grid gap-1.5 text-sm">
            <span id="model-download-pattern-preset-label" class="font-medium">{$translate("modelFiles.download.patternPreset")}</span>
            <Select.Root type="single" value={patternPreset} onValueChange={applyPatternPreset}>
              <Select.Trigger class="h-9 w-full" aria-labelledby="model-download-pattern-preset-label">
                {$translate(`modelFiles.download.patternPresets.${patternPreset}`)}
              </Select.Trigger>
              <Select.Content>
                <Select.Item value="custom">{$translate("modelFiles.download.patternPresets.custom")}</Select.Item>
                {#if rememberedPatterns}<Select.Item value="remembered">{$translate("modelFiles.download.patternPresets.remembered")}</Select.Item>{/if}
                <Select.Item value="gguf">{$translate("modelFiles.download.patternPresets.gguf")}</Select.Item>
                <Select.Item value="transformers">{$translate("modelFiles.download.patternPresets.transformers")}</Select.Item>
              </Select.Content>
            </Select.Root>
          </div>
          <div class="grid gap-4 sm:grid-cols-2">
            <label class="grid min-w-0 gap-1.5 text-sm">
              <span class="font-medium">{$translate("modelFiles.download.include")}</span>
              <textarea class="border-input bg-background min-h-32 resize-y rounded-lg border px-3 py-2 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50" value={include} oninput={(event) => { include = event.currentTarget.value; patternPreset = "custom"; }} placeholder={'*.gguf\ntokenizer.json'} autocomplete="off"></textarea>
            </label>
            <label class="grid min-w-0 gap-1.5 text-sm">
              <span class="font-medium">{$translate("modelFiles.download.exclude")}</span>
              <textarea class="border-input bg-background min-h-32 resize-y rounded-lg border px-3 py-2 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50" value={exclude} oninput={(event) => { exclude = event.currentTarget.value; patternPreset = "custom"; }} placeholder={'*.md\noriginal/*'} autocomplete="off"></textarea>
            </label>
          </div>
          <p class="text-muted-foreground text-xs">{$translate("modelFiles.download.patternHint")}</p>
        </fieldset>

        {#if formError || error}
          <div class="border-destructive/40 bg-destructive/10 text-destructive flex items-start gap-2 rounded-lg border p-3 text-sm" role="alert">
            <AlertTriangle class="mt-0.5 size-4 shrink-0" />
            <span>{formError || error}</span>
          </div>
        {/if}

        <Dialog.Footer>
          <Button variant="outline" type="button" onclick={() => { downloadDialogOpen = false; }} disabled={submitting}>{$translate("common.cancel")}</Button>
          <Button type="submit" disabled={submitting}>
            <Download />
            <span>{submitting ? $translate("modelFiles.download.submitting") : $translate("modelFiles.download.start")}</span>
          </Button>
        </Dialog.Footer>
      </form>

      <section class="min-w-0 space-y-3 border-t pt-4 lg:border-t-0 lg:border-l lg:pt-0 lg:pl-5" aria-labelledby="model-download-queue-title">
        <header class="flex h-9 items-center justify-between gap-3">
          <div class="flex h-9 items-center gap-2">
            <h3 id="model-download-queue-title" class="text-sm font-semibold leading-none">{$translate("modelFiles.download.queueTitle")}</h3>
            {#if activeTasks.length > 0}<Badge variant="secondary">{activeTasks.length}</Badge>{/if}
          </div>
          <Button variant="ghost" size="icon-sm" onclick={() => void loadTasks()} disabled={loading} title={$translate("modelFiles.download.refresh")} aria-label={$translate("modelFiles.download.refresh")}>
            <RefreshCw class={loading ? "animate-spin" : ""} />
          </Button>
        </header>

      {#if loading && tasks.length === 0}
        <div class="bg-muted/40 h-14 animate-pulse rounded-lg" aria-busy="true" aria-label={$translate("modelFiles.download.loading")}></div>
      {:else if tasks.length === 0}
        <p class="text-muted-foreground py-5 text-center text-sm" role="status">{$translate("modelFiles.download.empty")}</p>
      {:else}
        <div class="max-h-80 space-y-2 overflow-y-auto pr-1 lg:max-h-[calc(100vh-11rem)]" aria-live="polite">
          {#each tasks as task (task.id)}
            {@const taskProgress = progress(task)}
            <article class="rounded-lg border bg-card p-3">
              <div class="flex flex-wrap items-start justify-between gap-2">
                <div class="min-w-0">
                  <div class="flex flex-wrap items-center gap-2">
                    <h4 class="truncate text-sm font-medium" title={task.repo_id}>{task.repo_id}</h4>
                    <Badge variant={task.status === "failed" ? "destructive" : task.status === "completed" ? "outline" : "secondary"}>{statusLabel(task.status)}</Badge>
                  </div>
                  <p class="text-muted-foreground mt-1 truncate text-xs" title={taskDestinationLabel(task)}>{providerLabel(task.provider)} · {task.revision} · {taskDestinationLabel(task)}</p>
                </div>
                <div class="flex gap-1">
                  {#if task.status === "queued" || task.status === "retrying" || task.status === "downloading"}
                    <Button variant="ghost" size="icon-sm" title={$translate("modelFiles.download.cancel")} aria-label={$translate("modelFiles.download.cancel")} disabled={busyID !== "" || deleteBusyID !== ""} onclick={() => requestCancel(task)}><X /></Button>
                  {:else if task.status === "failed" || task.status === "canceled"}
                    <Button variant="ghost" size="icon-sm" title={$translate("modelFiles.download.retry")} aria-label={$translate("modelFiles.download.retry")} disabled={busyID !== "" || deleteBusyID !== ""} onclick={() => void retry(task)}><RotateCcw /></Button>
                    <Button variant="destructive" size="icon-sm" title={$translate("modelFiles.download.delete")} aria-label={$translate("modelFiles.download.delete")} disabled={busyID !== "" || deleteBusyID !== ""} onclick={() => requestDelete(task)}><Trash2 /></Button>
                  {:else if task.status === "completed"}
                    <Button variant="destructive" size="icon-sm" title={$translate("modelFiles.download.delete")} aria-label={$translate("modelFiles.download.delete")} disabled={busyID !== "" || deleteBusyID !== ""} onclick={() => requestDelete(task)}><Trash2 /></Button>
                  {/if}
                </div>
              </div>
              <div class="mt-3 flex items-center gap-2">
                <div class="bg-muted h-2 min-w-0 flex-1 overflow-hidden rounded-full" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={taskProgress} aria-label={`${task.repo_id} ${taskProgress}%`}>
                  <div class="bg-primary h-full transition-[width]" style={`width: ${taskProgress}%`}></div>
                </div>
                <span class="text-muted-foreground w-12 text-right text-xs tabular-nums">{taskProgress}%</span>
              </div>
              <div class="text-muted-foreground mt-2 flex flex-wrap gap-x-3 gap-y-1 text-xs">
                <span>{task.completed_files}/{task.total_files} {$translate("modelFiles.download.files")}</span>
                <span>{formatCapacity(task.downloaded_bytes, "—")} / {formatCapacity(task.total_bytes, "—")}</span>
                {#if task.status === "downloading"}<span class="truncate" title={task.current_file}>{task.current_file || $translate("modelFiles.download.preparing")}</span>{/if}
                {#if task.status === "retrying"}
                  <span>{$translate("modelFiles.download.retryAttempt", { attempt: task.attempts })}</span>
                  <span>{$translate("modelFiles.download.retryAt", { time: retryTime(task) })}</span>
                {/if}
              </div>
              {#if task.error}<p class="text-destructive mt-2 line-clamp-2 text-xs" title={task.error}>{task.error}</p>{/if}
            </article>
          {/each}
        </div>
      {/if}
      </section>
    </div>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={deleteDialogOpen}>
  <Dialog.Content class="sm:max-w-md" showCloseButton={deleteBusyID === ""}>
    <Dialog.Header>
      <Dialog.Title>{$translate("modelFiles.download.deleteTitle")}</Dialog.Title>
      {#if deleteTarget}
        <Dialog.Description>{$translate("modelFiles.download.deleteConfirm", { name: deleteTarget.repo_id })}</Dialog.Description>
      {/if}
    </Dialog.Header>
    {#if deleteTarget}
      <div class="grid gap-1 rounded-lg border bg-muted/30 p-3 text-sm">
        <span class="font-medium">{deleteTarget.repo_id}</span>
        <span class="text-muted-foreground text-xs">{deleteTarget.provider} · {deleteTarget.revision}</span>
      </div>
    {/if}
    {#if deleteError}
      <div class="border-destructive/40 bg-destructive/10 text-destructive flex items-start gap-2 rounded-lg border p-3 text-sm" role="alert">
        <AlertTriangle class="mt-0.5 size-4 shrink-0" />
        <span>{deleteError}</span>
      </div>
    {/if}
    <Dialog.Footer>
      <Button variant="outline" onclick={closeDeleteDialog} disabled={deleteBusyID !== ""}>{$translate("common.cancel")}</Button>
      <Button variant="destructive" onclick={() => void confirmDelete()} disabled={!deleteTarget || deleteBusyID !== ""}>
        {#if deleteBusyID}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
        {deleteBusyID ? $translate("common.loading") : $translate("modelFiles.download.delete")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={credentialsDialogOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-2xl overflow-y-auto sm:max-w-2xl" showCloseButton={!credentialsSaving}>
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2"><KeyRound class="size-4" aria-hidden="true" />{$translate("modelFiles.download.credentialsTitle")}</Dialog.Title>
      <Dialog.Description>{$translate("modelFiles.download.credentialsDescription")}</Dialog.Description>
    </Dialog.Header>

    {#if credentialsLoading}
      <div class="text-muted-foreground flex items-center gap-2 py-8 text-sm" role="status" aria-live="polite">
        <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
        {$translate("modelFiles.download.credentialsLoading")}
      </div>
    {:else if credentials}
      <form class="grid gap-4" onsubmit={(event) => { event.preventDefault(); void saveCredentials(); }}>
        <fieldset class="grid gap-4" disabled={credentialsSaving || !credentials.writable}>
          <section class="grid gap-2 rounded-lg border p-3" aria-labelledby="model-download-hf-credential-title">
            <div class="flex flex-wrap items-center justify-between gap-2">
              <h3 id="model-download-hf-credential-title" class="text-sm font-medium">{$translate("modelFiles.download.huggingFace")}</h3>
              <span class={`text-xs ${credentials.huggingface.configured ? "text-success" : "text-muted-foreground"}`}>
                {credentials.huggingface.configured ? $translate("modelFiles.download.credentialConfigured") : $translate("modelFiles.download.credentialNotConfigured")}
              </span>
            </div>
            <label class="grid gap-1.5 text-sm" for="model-download-hf-token">
              <span class="font-medium">{$translate("modelFiles.download.hfToken")}</span>
              <input id="model-download-hf-token" class="border-input bg-background h-9 rounded-lg border px-3 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50" type="password" value={hfToken} oninput={(event) => { hfToken = event.currentTarget.value; hfTokenTouched = true; }} autocomplete="new-password" spellcheck="false" placeholder={$translate("modelFiles.download.credentialPlaceholder")} />
            </label>
            <div class="flex flex-wrap items-center justify-between gap-2">
              <p class="text-muted-foreground text-xs">
                {#if credentials.huggingface.environmentConfigured}
                  {$translate("modelFiles.download.credentialEnvConfigured", { name: credentials.huggingface.environmentVariable })}
                {:else}
                  {$translate("modelFiles.download.credentialEnvMissing", { name: credentials.huggingface.environmentVariable })}
                {/if}
              </p>
              {#if credentials.huggingface.configured}<Button type="button" variant="ghost" size="sm" onclick={() => { hfToken = ""; hfTokenTouched = true; }}>{$translate("modelFiles.download.credentialClear")}</Button>{/if}
            </div>
          </section>

          <section class="grid gap-2 rounded-lg border p-3" aria-labelledby="model-download-modelscope-credential-title">
            <div class="flex flex-wrap items-center justify-between gap-2">
              <h3 id="model-download-modelscope-credential-title" class="text-sm font-medium">{$translate("modelFiles.download.modelScope")}</h3>
              <span class={`text-xs ${credentials.modelscope.configured ? "text-success" : "text-muted-foreground"}`}>
                {credentials.modelscope.configured ? $translate("modelFiles.download.credentialConfigured") : $translate("modelFiles.download.credentialNotConfigured")}
              </span>
            </div>
            <label class="grid gap-1.5 text-sm" for="model-download-modelscope-token">
              <span class="font-medium">{$translate("modelFiles.download.modelScopeToken")}</span>
              <input id="model-download-modelscope-token" class="border-input bg-background h-9 rounded-lg border px-3 text-sm outline-none focus-visible:ring-3 focus-visible:ring-ring/50" type="password" value={modelScopeToken} oninput={(event) => { modelScopeToken = event.currentTarget.value; modelScopeTokenTouched = true; }} autocomplete="new-password" spellcheck="false" placeholder={$translate("modelFiles.download.credentialPlaceholder")} />
            </label>
            <div class="flex flex-wrap items-center justify-between gap-2">
              <p class="text-muted-foreground text-xs">
                {#if credentials.modelscope.environmentConfigured}
                  {$translate("modelFiles.download.credentialEnvConfigured", { name: credentials.modelscope.environmentVariable })}
                {:else}
                  {$translate("modelFiles.download.credentialEnvMissing", { name: credentials.modelscope.environmentVariable })}
                {/if}
              </p>
              {#if credentials.modelscope.configured}<Button type="button" variant="ghost" size="sm" onclick={() => { modelScopeToken = ""; modelScopeTokenTouched = true; }}>{$translate("modelFiles.download.credentialClear")}</Button>{/if}
            </div>
          </section>
        </fieldset>

        {#if !credentials.writable}<p class="border-warning/30 bg-warning/10 text-warning rounded-md border px-3 py-2 text-sm">{$translate("modelFiles.download.credentialsReadOnly")}</p>{/if}
        {#if credentialsError}
          <div class="border-destructive/40 bg-destructive/10 text-destructive flex items-start gap-2 rounded-lg border p-3 text-sm" role="alert">
            <AlertTriangle class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
            <span>{credentialsError}</span>
          </div>
        {/if}

        <Dialog.Footer>
          <Button variant="outline" type="button" onclick={closeCredentials} disabled={credentialsSaving}>{$translate("common.cancel")}</Button>
          <Button type="submit" disabled={credentialsSaving || !credentials.writable}>
            {#if credentialsSaving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
            {$translate("modelFiles.download.credentialsSave")}
          </Button>
        </Dialog.Footer>
      </form>
    {:else if credentialsError}
      <div class="border-destructive/40 bg-destructive/10 text-destructive flex items-start gap-2 rounded-lg border p-3 text-sm" role="alert">
        <AlertTriangle class="mt-0.5 size-4 shrink-0" aria-hidden="true" />
        <span>{credentialsError}</span>
      </div>
    {/if}
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={cancelDialogOpen}>
  <Dialog.Content class="sm:max-w-md" showCloseButton={busyID === ""}>
    <Dialog.Header>
      <Dialog.Title>{$translate("modelFiles.download.cancelTitle")}</Dialog.Title>
      {#if cancelTarget}
        <Dialog.Description>{$translate("modelFiles.download.cancelConfirm", { name: cancelTarget.repo_id })}</Dialog.Description>
      {/if}
    </Dialog.Header>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => { cancelDialogOpen = false; cancelTarget = null; }} disabled={busyID !== ""}>{$translate("common.cancel")}</Button>
      <Button variant="destructive" onclick={() => void confirmCancel()} disabled={!cancelTarget || busyID !== ""}>{$translate("modelFiles.download.cancel")}</Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
