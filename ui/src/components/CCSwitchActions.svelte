<script lang="ts">
  import { untrack } from "svelte";
  import { ExternalLink, LoaderCircle } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../lib/i18n";
  import { errorMessageFromPayload } from "../lib/apiError";
  import { selectableModelNames } from "../lib/ccswitchModels";
  import { authSession } from "../stores/auth";
  import { currentRoute } from "../stores/route";
  import { fetchPlaygroundModels, playgroundModels } from "../stores/api";

  // Applications CC Switch v4 imports provider links into. The order matches
  // internal/ccswitch.Apps, which mirrors CC Switch's own parser.
  const apps = ["claude", "codex", "gemini", "grokbuild", "opencode", "openclaw", "hermes", "pi"] as const;
  type CCSwitchApp = (typeof apps)[number];

  interface APIKey {
    id: string;
    name: string;
    models: string[];
  }

  interface LinkPayload {
    link?: string;
    error?: string;
    endpoint?: string;
    apiFormat?: string;
  }

  const currentKeyValue = "__current__";
  const autoKeyValue = "__auto__";
  const defaultKeyValue = "__default__";
  const defaultAPIKey = "llama-swap";
  const mainModelValue = "__main__";

  const appLabels: Record<CCSwitchApp, string> = {
    claude: "Claude Code",
    codex: "Codex",
    gemini: "Gemini CLI",
    grokbuild: "Grok Build",
    opencode: "OpenCode",
    openclaw: "OpenClaw",
    hermes: "Hermes",
    pi: "Pi",
  };

  let dialogOpen = $state(false);
  let selectedApp = $state<CCSwitchApp>("claude");
  let providerName = $state("");
  let nameFollowsModel = true;
  let model = $state("");
  let haikuModel = $state("");
  let sonnetModel = $state("");
  let opusModel = $state("");
  let keySelection = $state(defaultKeyValue);
  let currentKeyID = $state("");
  let keys = $state<APIKey[]>([]);
  let keysLoading = $state(false);
  let generating = $state(false);
  let readyLink = $state("");
  let resolvedEndpoint = $state("");
  let resolvedFormat = $state("");
  let error = $state("");
  let generationRevision = 0;
  let generationTimer: ReturnType<typeof setTimeout> | undefined;

  /**
   * The key whose model allowlist constrains this import, or null when the
   * import is not tied to a stored key. An empty allowlist means the key may
   * use every model.
   */
  function restrictingKey(): APIKey | null {
    const selectedID = selectedKeyID();
    if (selectedID) return keys.find((entry) => entry.id === selectedID) ?? null;
    if (keySelection === currentKeyValue) return keys.find((entry) => entry.id === currentKeyID) ?? null;
    return null;
  }

  /**
   * A model group is offerable when the selected key's allowlist names the
   * model or one of its aliases: the server resolves every spelling of a group
   * through the same allowlist entry, so all of its names are usable.
   */
  let modelOptions = $derived.by(() => selectableModelNames($playgroundModels, restrictingKey()?.models ?? []));

  function defaultModel(): string {
    if ($currentRoute.startsWith("/models/")) {
      try {
        const candidate = decodeURIComponent($currentRoute.slice("/models/".length));
        if (modelOptions.includes(candidate)) return candidate;
      } catch {
        // Fall through to the first selectable model.
      }
    }
    return modelOptions[0] ?? "";
  }

  function normalizeKey(value: unknown): APIKey | null {
    if (!value || typeof value !== "object") return null;
    const record = value as Record<string, unknown>;
    if (typeof record.id !== "string" || record.id.trim() === "") return null;
    return {
      id: record.id,
      name: typeof record.name === "string" ? record.name : "",
      models: Array.isArray(record.models) ? record.models.filter((item): item is string => typeof item === "string") : [],
    };
  }

  async function loadKeys(): Promise<void> {
    keysLoading = true;
    try {
      const response = await fetch("/api/cc-switch/options", { credentials: "same-origin" });
      const payload = await response.json().catch(() => ({})) as { data?: unknown; currentKeyId?: unknown; error?: string };
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      keys = Array.isArray(payload.data)
        ? payload.data.map(normalizeKey).filter((entry): entry is APIKey => entry !== null)
        : [];
      currentKeyID = typeof payload.currentKeyId === "string" ? payload.currentKeyId : "";
    } catch (cause) {
      keys = [];
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      keysLoading = false;
    }
  }

  function openImport(): void {
    if (dialogOpen) return;
    selectedApp = "claude";
    providerName = "";
    nameFollowsModel = true;
    haikuModel = "";
    sonnetModel = "";
    opusModel = "";
    keySelection = $authSession.authenticated ? currentKeyValue : defaultKeyValue;
    currentKeyID = "";
    readyLink = "";
    resolvedEndpoint = "";
    resolvedFormat = "";
    error = "";
    keys = [];
    model = defaultModel();
    providerName = model || "llama-swap";
    dialogOpen = true;
    void fetchPlaygroundModels();
    if ($authSession.authenticated) void loadKeys();
  }

  function closeImport(): void {
    dialogOpen = false;
  }

  function clearGeneratedLink(): void {
    generationRevision++;
    if (generationTimer) clearTimeout(generationTimer);
    generationTimer = undefined;
    generating = false;
    readyLink = "";
    resolvedEndpoint = "";
    resolvedFormat = "";
    error = "";
  }

  function keyLabel(value: string): string {
    if (value === autoKeyValue) return $translate("controlPlane.ccswitchAutoKey");
    if (value === currentKeyValue) return $translate("controlPlane.ccswitchCurrentKey");
    if (value === defaultKeyValue) return $translate("controlPlane.ccswitchDefaultKey");
    const key = keys.find((entry) => entry.id === value);
    return key ? (key.name ? `${key.name} (${key.id})` : key.id) : value;
  }

  /** The stored key the import should carry, or "" when no stored key applies. */
  function selectedKeyID(): string {
    if (keySelection === currentKeyValue || keySelection === autoKeyValue || keySelection === defaultKeyValue) {
      return "";
    }
    return keySelection;
  }

  function selectedKey(): string {
    return keySelection === defaultKeyValue ? defaultAPIKey : "";
  }

  function includeKey(): boolean {
    return keySelection === currentKeyValue || keySelection === defaultKeyValue || selectedKeyID() !== "";
  }

  function changeModel(value: string | undefined): void {
    if (!value || value === mainModelValue) return;
    const previous = model;
    model = value;
    if (nameFollowsModel || providerName.trim() === "" || providerName === previous) {
      providerName = value;
      nameFollowsModel = true;
    }
  }

  function changeOptionalModel(target: "haiku" | "sonnet" | "opus", value: string | undefined): void {
    const next = !value || value === mainModelValue ? "" : value;
    if (target === "haiku") haikuModel = next;
    else if (target === "sonnet") sonnetModel = next;
    else opusModel = next;
  }

  async function requestLink(revision: number, createKey = false): Promise<string> {
    const response = await fetch("/api/cc-switch/deeplink", {
      method: "POST",
      credentials: "same-origin",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({
        app: selectedApp,
        name: providerName.trim(),
        model: model.trim(),
        haikuModel: haikuModel.trim(),
        sonnetModel: sonnetModel.trim(),
        opusModel: opusModel.trim(),
        includeKey: createKey ? false : includeKey(),
        createKey,
        keyId: createKey ? "" : selectedKeyID(),
        key: createKey ? "" : selectedKey(),
      }),
    });
    const payload = await response.json().catch(() => ({})) as LinkPayload;
    if (revision !== generationRevision || !dialogOpen) return "";
    if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
    if (typeof payload.link !== "string" || payload.link.trim() === "") {
      throw new Error($translate("controlPlane.ccswitchUnavailable"));
    }
    resolvedEndpoint = payload.endpoint ?? "";
    resolvedFormat = payload.apiFormat ?? "";
    return payload.link;
  }

  function scheduleLink(): void {
    if (generationTimer) clearTimeout(generationTimer);
    generationTimer = undefined;
    readyLink = "";
    error = "";
    const revision = ++generationRevision;
    if (!dialogOpen || providerName.trim() === "" || model.trim() === "" || keySelection === autoKeyValue) {
      generating = false;
      return;
    }
    generating = true;
    generationTimer = setTimeout(() => {
      generationTimer = undefined;
      void requestLink(revision)
        .then((link) => { if (revision === generationRevision) readyLink = link; })
        .catch((cause) => {
          if (revision !== generationRevision || !dialogOpen) return;
          error = cause instanceof Error ? cause.message : String(cause);
        })
        .finally(() => {
          if (revision === generationRevision) generating = false;
        });
    }, 180);
  }

  async function createKeyAndOpen(): Promise<void> {
    if (generating || providerName.trim() === "" || model.trim() === "") return;
    const revision = ++generationRevision;
    generating = true;
    error = "";
    try {
      const link = await requestLink(revision, true);
      if (link) window.location.assign(link);
    } catch (cause) {
      if (revision === generationRevision) error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      if (revision === generationRevision) generating = false;
    }
  }

  $effect(() => {
    dialogOpen;
    selectedApp;
    providerName;
    model;
    haikuModel;
    sonnetModel;
    opusModel;
    keySelection;
    $authSession.authenticated;
    if (dialogOpen) scheduleLink();
    else clearGeneratedLink();
  });

  // The model list is narrowed once the key options arrive. A selection that
  // the chosen key cannot use would be rejected by the server, so replace it
  // with a selectable model instead of offering a link that cannot be created.
  $effect(() => {
    if (!dialogOpen) return;
    const options = modelOptions;
    if (options.length === 0) return;
    // Only modelOptions is a dependency: the selections are read and written
    // here, so they go through untrack to keep the effect from retriggering
    // itself on its own writes.
    untrack(() => {
      if (!model.trim() || !options.includes(model)) {
        const previous = model;
        model = options[0];
        if (nameFollowsModel || providerName.trim() === "" || providerName === previous) {
          providerName = model;
          nameFollowsModel = true;
        }
      }
      if (haikuModel && !options.includes(haikuModel)) haikuModel = "";
      if (sonnetModel && !options.includes(sonnetModel)) sonnetModel = "";
      if (opusModel && !options.includes(opusModel)) opusModel = "";
    });
  });
</script>

<Button variant="outline" size="sm" onclick={openImport} aria-label={$translate("controlPlane.ccswitchImport")} title={$translate("controlPlane.ccswitchImport")}>
  <ExternalLink class="size-3.5" aria-hidden="true" />
  <span class="hidden sm:inline">{$translate("controlPlane.ccswitchImport")}</span>
</Button>

<Dialog.Root bind:open={dialogOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-xl overflow-y-auto sm:max-w-xl" showCloseButton={true}>
    <Dialog.Header>
      <Dialog.Title>{$translate("controlPlane.ccswitchImportTitle")}</Dialog.Title>
    </Dialog.Header>

    <form class="grid gap-4" onsubmit={(event) => event.preventDefault()}>
      <fieldset class="grid gap-2">
        <legend class="text-sm font-medium">{$translate("controlPlane.ccswitchApplication")}</legend>
        <div class="flex flex-wrap gap-x-5 gap-y-2">
          {#each apps as app (app)}
            <label class="flex items-center gap-2 text-sm">
              <input type="radio" name="ccswitch-app" value={app} checked={selectedApp === app} onchange={() => { selectedApp = app; }} />
              <span>{appLabels[app]}</span>
            </label>
          {/each}
        </div>
        {#if resolvedEndpoint}
          <p class="text-muted-foreground text-xs">
            {$translate("controlPlane.ccswitchResolved", { endpoint: resolvedEndpoint, format: $translate(`controlPlane.ccswitchFormat_${resolvedFormat}`) })}
          </p>
        {/if}
      </fieldset>

      <label class="grid gap-1.5 text-sm" for="ccswitch-name">
        <span class="font-medium">{$translate("controlPlane.ccswitchName")}</span>
        <input id="ccswitch-name" class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 rounded-lg border px-3 outline-none focus-visible:ring-3" value={providerName} oninput={(event) => { providerName = event.currentTarget.value; nameFollowsModel = false; }} autocomplete="off" />
      </label>

      <div class="grid gap-1.5 text-sm">
        <span id="ccswitch-main-model-label" class="font-medium">{$translate("controlPlane.ccswitchMainModel")} <span class="text-destructive">*</span></span>
        <Select.Root type="single" value={model} onValueChange={changeModel}>
          <Select.Trigger id="ccswitch-main-model" class="h-9 w-full" aria-labelledby="ccswitch-main-model-label">
            {model || $translate("controlPlane.ccswitchModelPlaceholder")}
          </Select.Trigger>
          <Select.Content>
            {#each modelOptions as option (option)}<Select.Item value={option}>{option}</Select.Item>{/each}
          </Select.Content>
        </Select.Root>
      </div>

      {#if selectedApp === "claude"}
        <div class="grid gap-3">
          {#each [
            { id: "haiku", label: "ccswitchHaikuModel", value: haikuModel },
            { id: "sonnet", label: "ccswitchSonnetModel", value: sonnetModel },
            { id: "opus", label: "ccswitchOpusModel", value: opusModel },
          ] as field (field.id)}
            <div class="grid gap-1.5 text-sm">
              <span id={`ccswitch-${field.id}-model-label`} class="font-medium">{$translate(`controlPlane.${field.label}`)}</span>
              <Select.Root type="single" value={field.value || mainModelValue} onValueChange={(value) => changeOptionalModel(field.id as "haiku" | "sonnet" | "opus", value)}>
                <Select.Trigger id={`ccswitch-${field.id}-model`} class="h-9 w-full" aria-labelledby={`ccswitch-${field.id}-model-label`}>
                  {field.value || $translate("controlPlane.ccswitchUseMainModel")}
                </Select.Trigger>
                <Select.Content>
                  <Select.Item value={mainModelValue}>{$translate("controlPlane.ccswitchUseMainModel")}</Select.Item>
                  {#each modelOptions as option (option)}<Select.Item value={option}>{option}</Select.Item>{/each}
                </Select.Content>
              </Select.Root>
            </div>
          {/each}
        </div>
      {/if}

      <div class="grid gap-1.5 text-sm">
        <label class="font-medium" for="ccswitch-key">{$translate("controlPlane.ccswitchAPIKey")}</label>
        <Select.Root type="single" value={keySelection} onValueChange={(value) => { if (value) keySelection = value; }}>
          <Select.Trigger id="ccswitch-key" class="h-9 w-full" aria-label={$translate("controlPlane.ccswitchAPIKey")}>{keyLabel(keySelection)}</Select.Trigger>
          <Select.Content>
            {#if $authSession.authenticated}
              <Select.Item value={autoKeyValue}>{$translate("controlPlane.ccswitchAutoKey")}</Select.Item>
              <Select.Item value={currentKeyValue}>{$translate("controlPlane.ccswitchCurrentKey")}</Select.Item>
            {/if}
            <Select.Item value={defaultKeyValue}>{$translate("controlPlane.ccswitchDefaultKey")}</Select.Item>
            {#each keys.filter((key) => key.id !== currentKeyID) as key (key.id)}
              <Select.Item value={key.id}>{key.name ? `${key.name} (${key.id})` : key.id}</Select.Item>
            {/each}
          </Select.Content>
        </Select.Root>
        {#if keysLoading}<span class="text-muted-foreground text-xs" role="status">{$translate("controlPlane.loading")}</span>{/if}
        {#if restrictingKey() && (restrictingKey()?.models?.length ?? 0) > 0}
          <span class="text-muted-foreground text-xs">{$translate("controlPlane.ccswitchModelScope")}</span>
        {/if}
      </div>

      {#if error}
        <div class="rounded-lg border border-destructive/40 bg-destructive/10 p-3 text-sm break-words whitespace-pre-wrap max-h-40 overflow-y-auto" role="alert">{$translate("controlPlane.error", { message: error })}</div>
      {/if}

      <Dialog.Footer>
        <Button variant="outline" type="button" onclick={closeImport}>{$translate("common.cancel")}</Button>
        {#if keySelection === autoKeyValue}
          <Button type="button" disabled={generating || providerName.trim() === "" || model.trim() === ""} onclick={() => void createKeyAndOpen()}>
            {#if generating}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{:else}<ExternalLink class="size-4" aria-hidden="true" />{/if}
            {$translate("controlPlane.ccswitchOpen")}
          </Button>
        {:else}
          <Button href={readyLink || undefined} disabled={generating || readyLink === "" || providerName.trim() === "" || model.trim() === ""} rel="noreferrer">
            {#if generating}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{:else}<ExternalLink class="size-4" aria-hidden="true" />{/if}
            {$translate("controlPlane.ccswitchOpen")}
          </Button>
        {/if}
      </Dialog.Footer>
    </form>
  </Dialog.Content>
</Dialog.Root>
