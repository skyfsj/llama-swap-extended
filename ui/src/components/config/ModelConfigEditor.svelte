<script lang="ts">
  import { onMount } from "svelte";
  import { AlertTriangle, Check, ChevronRight, Filter, Link2, LoaderCircle, RefreshCw, RotateCcw, Save, Server, SlidersHorizontal, Terminal } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import * as Tabs from "$lib/components/ui/tabs/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "../../lib/apiError";
  import { getModelFiles } from "../../lib/modelFiles";
  import type { ModelFile } from "../../lib/types";
  import { childSchema, modelFieldSchema } from "../../lib/configSchema";
  import SchemaField from "./SchemaField.svelte";
  import PricingCatalogPanel from "./PricingCatalogPanel.svelte";
  import {
    autoServedModelName,
    buildModelPatch,
    cudaVisibleDevicesFromEnv,
    draftFromModel,
    emptyModelDraft,
    extraLaunchArguments,
    hasModelCapabilityOverrides,
    managedLaunchKind,
    managedRuntimeEntrypoint,
    managedModelPathFromArguments,
    managedModelTargets,
    modelEntries,
    property,
    rebuildManagedArguments,
    record,
    runtimeOptions,
    serializeLaunchArgument,
    summarizeChanges,
    syncCudaEnvLines,
    syncLaunchOwnedArguments,
    trimmedText,
    type CudaEnvSyncState,
    MODEL_API_KEYS,
    MODEL_MODALITIES,
    type ConfigChange,
    type LaunchMode,
    type ModelConfigDraft,
    type ModelLaunchDraft,
    tokenizeLaunchArguments,
  } from "../../lib/modelConfig";
  import { adoptLaunchFields, parseLaunchCommand, stripOwnedLaunchArguments, type ParsedLaunch } from "../../lib/launchParse";
  import LaunchArgsCodeEditor from "./LaunchArgsCodeEditor.svelte";

  interface ConfigSource {
    path: string;
    writable: boolean;
    managed?: boolean;
  }

  interface ConfigSnapshot {
    config?: Record<string, unknown>;
    yaml: string;
    etag: string;
    writable: boolean;
    restartRequired?: boolean;
    restartPaths?: string[];
    sources?: ConfigSource[];
    ownership?: Record<string, string>;
  }

  interface Validation {
    valid: boolean;
    issues?: { path: string; message: string }[];
    diff?: unknown[];
    restartRequired?: boolean;
    restartPaths?: string[];
  }

  interface Props {
    snapshot: ConfigSnapshot;
    onSnapshot: (snapshot: ConfigSnapshot) => void;
    modelId?: string;
    mode?: "edit" | "create";
    onSaved?: (modelId: string) => void;
    showTitle?: boolean;
    embedded?: boolean;
    showModelName?: boolean;
    onCancel?: () => void;
  }

  let {
    snapshot,
    onSnapshot,
    modelId = "",
    mode = "edit",
    onSaved = () => {},
    showTitle = true,
    embedded = false,
    showModelName = true,
    onCancel = () => {},
  }: Props = $props();

  let selectedModel = $state("");
  let draft = $state<ModelConfigDraft>(emptyModelDraft());
  // Raw user-supplied engine arguments as typed in the editor. The managed
  // launch prefix (entrypoint, model, host, port) is rebuilt from this text on
  // every change rather than shown to the operator.
  let argsText = $state("");
  let activeTab = $state("overview");
  let saving = $state(false);
  let error = $state("");
  let validation = $state<Validation | null>(null);
  let diffDialogOpen = $state(false);
  let pendingPatch = $state<unknown[] | null>(null);
  let pendingChanges = $state<ConfigChange[]>([]);
  let pendingETag = $state("");
  let pendingModelId = $state("");
  let modelFiles = $state<ModelFile[]>([]);
  let modelFilesLoading = $state(false);
  let modelFilesError = $state("");
  let preserveDraftOnSnapshot = $state(false);
  let hydrateGPUSelectionFromEnv = $state(false);

  let editing = $derived(mode === "edit");
  let modelNames = $derived(Object.keys(modelEntries(snapshot.config)).sort((a, b) =>
    a.localeCompare(b, undefined, { numeric: true }),
  ));
  let runtimes = $derived(runtimeOptions(snapshot.config));
  let selectedRuntime = $derived(runtimes.find((runtime) => runtime.name === draft.backendRuntime));
  // A managed kind comes from the selected runtime, falling back to the explicit
  // backend type so the launch prefix still applies before a runtime is chosen.
  // Container runtimes are excluded: they pass backend.args through to the
  // container verbatim and ignore the structured launch block entirely.
  let effectiveManagedKind = $derived(
    selectedRuntime
      ? managedLaunchKind(selectedRuntime.kind, selectedRuntime.mode)
      : (draft.backendType === "vllm" || draft.backendType === "llamacpp" ? draft.backendType : ""),
  );
  let runtimeEntrypoint = $derived(managedRuntimeEntrypoint(selectedRuntime?.kind ?? ""));
  let managedTargets = $derived(managedModelTargets(modelFiles, effectiveManagedKind));
  let selectedManagedModelPath = $derived(
    trimmedText(draft.launch.model) || managedModelPathFromArguments(draft.backendArguments, effectiveManagedKind),
  );
  let selectedManagedTarget = $derived(managedTargets.find((target) => target.path === selectedManagedModelPath));
  let currentModel = $derived(editing ? modelEntries(snapshot.config)[selectedModel] : undefined);
  let capabilityOverrides = $derived(hasModelCapabilityOverrides(draft));
  // Global defaults a model inherits when its own field is left empty; shown
  // as the placeholder of the corresponding input.
  let configRoot = $derived(record(snapshot.config ?? {}));
  let globalTTLValue = $derived(String(property(configRoot, "globalTTL") ?? ""));
  let globalUnloadValue = $derived(String(property(configRoot, "unloadTimeout") ?? ""));

  const inputClass = "border-input bg-background h-9 w-full rounded-md border px-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-60";
  const selectClass = "border-input bg-background h-9 w-full rounded-md border pr-9 pl-3 text-sm outline-none transition-colors focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/30 disabled:cursor-not-allowed disabled:opacity-60";

  $effect(() => {
    const fixedModel = modelId.trim();
    if (mode === "create") {
      if (draft.id !== fixedModel && fixedModel) draft.id = fixedModel;
      return;
    }
    if (fixedModel && modelNames.includes(fixedModel)) {
      if (selectedModel !== fixedModel) selectedModel = fixedModel;
      return;
    }
    if (!modelNames.includes(selectedModel)) selectedModel = modelNames[0] ?? "";
  });

  $effect(() => {
    if (preserveDraftOnSnapshot) {
      preserveDraftOnSnapshot = false;
      return;
    }
    if (mode === "create") return;
    const current = modelEntries(snapshot.config)[selectedModel];
    if (current === undefined) return;
    const next = draftFromModel(selectedModel, current);
    // Managed launch text is normalized as soon as it is loaded: the runtime
    // owns the entrypoint, model target, host and port, so operator-written
    // duplicates of those are dropped and the canonical set re-injected. A
    // container runtime passes args through verbatim and ignores the launch
    // block, so no managed normalization applies to it.
    const selectedKind = next.backendRuntime
      ? (runtimes.find((candidate) => candidate.name === next.backendRuntime)?.kind ?? "")
      : "";
    const selectedMode = next.backendRuntime
      ? (runtimes.find((candidate) => candidate.name === next.backendRuntime)?.mode ?? "")
      : "";
    const kind = selectedKind !== "" || selectedMode !== ""
      ? managedLaunchKind(selectedKind, selectedMode)
      : (next.backendType === "vllm" || next.backendType === "llamacpp" ? next.backendType : "");
    // Managed launch text is normalized as soon as it is loaded: system-owned
    // tokens (entrypoint, bind host and port) are dropped and the operator's
    // own lines are preserved verbatim, so a hand-edited config round-trips.
    if (kind !== "") next.backendArguments = rebuildManagedArguments(next.backendArguments, kind, "");
    draft = next;
    argsText = extraLaunchArguments(next.backendArguments, kind);
    hydrateGPUSelectionFromEnv = true;
    cudaEnvSyncState.visible = { managed: "", stashed: "" };
    cudaEnvSyncState.order = { managed: "", stashed: "" };
    // A legacy args-only config surfaces in the structured fields too: the Go
    // parser classifies the text once and the editor mirrors the result. The
    // file itself is only rewritten when the user saves.
    const migrationKey = `${selectedModel}:${snapshot.etag}`;
    if (kind !== "" && launchViewEmpty(next.launch) && next.backendArguments.trim() !== "" && lastLaunchMigration !== migrationKey) {
      lastLaunchMigration = migrationKey;
      migrateLaunchView(next.backendArguments, kind);
    }
  });

  let lastLaunchMigration = "";

  // The engine must serve under the name llama-swap sends upstream
  // (useModelName, or the model id when unset), so a managed launch owns
  // its served model name: the draft mirrors the derived value and the
  // field is display-only.
  $effect(() => {
    if (effectiveManagedKind === "") return;
    const derived = autoServedModelName(draft.useModelName, draft.id);
    if (draft.launch.servedModelName !== derived) draft.launch.servedModelName = derived;
  });

  // The structured launch fields are the source of truth for the engine flags
  // they own, and the GPU selection owns the CUDA environment: the server
  // drops operator duplicates of those flags, pins CUDA_DEVICE_ORDER, and
  // re-injects the structured values at launch, so the editor text follows
  // the fields live instead of showing a stale copy. Only NVIDIA cards
  // reach CUDA_VISIBLE_DEVICES — on other hardware the selection is affinity
  // metadata and no CUDA variable is written. The reverse direction (editor
  // -> fields) runs in updateBackendArguments.
  let cudaEnvSyncState: CudaEnvSyncState = { visible: { managed: "", stashed: "" }, order: { managed: "", stashed: "" } };
  $effect(() => {
    const kind = effectiveManagedKind;
    if (kind === "") return;
    // Older configurations only have CUDA_VISIBLE_DEVICES in env. Resolve it
    // once after the hardware catalog arrives so the visual card selector and
    // the real launch environment cannot disagree. A subsequent explicit
    // "clear" remains a real operator choice rather than being rehydrated.
    if (hydrateGPUSelectionFromEnv) {
      const configuredDevices = cudaVisibleDevicesFromEnv(draft.env);
      if (configuredDevices.length === 0 || draft.launch.gpus.length > 0) {
        hydrateGPUSelectionFromEnv = false;
      } else if (gpus.length === 0) {
        return;
      } else {
        const selections = mapDeviceSelections(configuredDevices);
        if (selections.every((selection) => gpus.some((gpu) => gpuSelectionID(gpu) === selection))) {
          draft.launch.gpus = selections;
        }
        hydrateGPUSelectionFromEnv = false;
        return;
      }
    }
    const devices = new Set<string>();
    let complete = true;
    for (const selection of draft.launch.gpus) {
      const gpu = gpus.find((candidate) => (candidate.uuid || `index:${candidate.index}`) === selection || String(candidate.index) === selection);
      if (!gpu) {
        // A selection cannot be resolved to a catalog entry yet (or the card
        // is gone); the effect re-runs when the catalog loads.
        complete = false;
        break;
      }
      // CUDA_DEVICE_ORDER=PCI_BUS_ID pins these host inventory ordinals to
      // nvidia-smi's PCI order. This is the vLLM-compatible representation;
      // its current platform layer does not accept GPU UUIDs here.
      if (gpu.vendor.toUpperCase() === "NVIDIA") devices.add(String(gpu.index));
    }
    const deviceList = [...devices];
    const nextArgs = syncLaunchOwnedArguments(argsText, kind, draft.launch, deviceList.length);
    if (nextArgs !== argsText) {
      argsText = nextArgs;
      draft.backendArguments = rebuildManagedArguments(nextArgs, kind, "");
    }
    if (!complete) return;
    const nextEnv = syncCudaEnvLines(draft.env, deviceList, cudaEnvSyncState);
    if (nextEnv !== draft.env) draft.env = nextEnv;
  });

  function launchViewEmpty(launch: ModelLaunchDraft): boolean {
    return trimmedText(launch.model) === "" && trimmedText(launch.servedModelName) === "" &&
      trimmedText(launch.contextPerRequest) === "" && trimmedText(launch.maxConcurrency) === "" &&
      trimmedText(launch.tensorParallelSize) === "" &&
      launch.gpus.length === 0 && trimmedText(launch.gpuMemoryUtilization) === "";
  }

  function migrateLaunchView(argumentsText: string, kind: string): void {
    try {
      const parsed = parseLaunchCommand(kind, argumentsText);
      if (parsed.warnings.length > 0) return;
      if (parsed.model !== "") draft.launch.model = parsed.model;
      if (parsed.contextPerRequest > 0) draft.launch.contextPerRequest = String(parsed.contextPerRequest);
      if (parsed.maxConcurrency > 0) draft.launch.maxConcurrency = String(parsed.maxConcurrency);
      if (parsed.tensorParallelSize > 0) draft.launch.tensorParallelSize = String(parsed.tensorParallelSize);
      if (parsed.gpuMemoryUtilization != null) draft.launch.gpuMemoryUtilization = String(parsed.gpuMemoryUtilization * 100);
      if (parsed.cudaVisibleDevices.length > 0) draft.launch.gpus = mapDeviceSelections(parsed.cudaVisibleDevices);
      const extras = parsed.extraArgs;
      if (extras.length !== tokenizeLaunchArguments(argumentsText).length) {
        argsText = renderExtraArgs(extras);
        draft.backendArguments = rebuildManagedArguments(argsText, kind, "");
      }
    } catch {
      // the migration view is best effort; the legacy text keeps working
    }
  }

  function setLaunchMode(next: LaunchMode): void {
    draft.launchMode = next;
    error = "";
    validation = null;
  }

  // Per-model runtime version pin: the installed versions of the selected
  // runtime plus the runtime-level current/pinned pointers, shown as markers
  // next to the options. The pin is a property of the runtime it was chosen
  // on, so switching the runtime clears it.
  let runtimeVersionCatalog = $state<string[]>([]);
  let runtimeVersionCatalogLoading = $state(false);
  let runtimeVersionCatalogError = $state("");
  let runtimeCurrentVersion = $state("");
  let runtimePinnedVersion = $state("");
  let lastRuntimeVersionFetch = "";

  $effect(() => {
    const runtime = trimmedText(draft.backendRuntime);
    if (lastRuntimeVersionFetch === runtime) return;
    lastRuntimeVersionFetch = runtime;
    void loadRuntimeVersionCatalog(runtime);
  });

  async function loadRuntimeVersionCatalog(runtime: string): Promise<void> {
    runtimeVersionCatalog = [];
    runtimeVersionCatalogError = "";
    runtimeCurrentVersion = "";
    runtimePinnedVersion = "";
    if (runtime === "") return;
    runtimeVersionCatalogLoading = true;
    try {
      const response = await fetch(`/api/runtimes/${encodeURIComponent(runtime)}`);
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      const versions = (payload?.versions ?? {}) as Record<string, unknown>;
      const status = (payload?.status ?? {}) as Record<string, unknown>;
      runtimeCurrentVersion = typeof status.current === "string" ? status.current : "";
      runtimePinnedVersion = typeof status.pinned === "string" ? status.pinned : "";
      runtimeVersionCatalog = Object.keys(versions).sort((a, b) => a.localeCompare(b, undefined, { numeric: true }));
    } catch (cause) {
      runtimeVersionCatalogError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      runtimeVersionCatalogLoading = false;
    }
  }

  let runtimeVersionPlaceholder = $derived.by(() => {
    if (runtimeVersionCatalogLoading) return $translate("modelFiles.loading");
    if (runtimeVersionCatalog.length === 0) return $translate("controlPlane.modelRuntimeVersionNone");
    return $translate("controlPlane.modelRuntimeVersionFollow");
  });

  // A named runtime owns its backend type: keep the draft in sync (including
  // on load) so a saved config cannot carry a type the runtime definition
  // does not match.
  $effect(() => {
    const kind = selectedRuntime?.kind ?? "";
    if (kind !== "" && draft.backendType !== kind) draft.backendType = kind;
  });

  function runtimeVersionLabel(version: string): string {
    const marker = version === runtimeCurrentVersion
      ? $translate("controlPlane.modelRuntimeVersionCurrent")
      : (version === runtimePinnedVersion ? $translate("controlPlane.modelRuntimeVersionPinned") : "");
    return marker !== "" ? `${version} (${marker})` : version;
  }

  function selectRuntime(name: string): void {
    if (draft.backendRuntime !== name) draft.backendRuntimeVersion = "";
    draft.backendRuntime = name;
    const runtime = runtimes.find((candidate) => candidate.name === name);
    // A container runtime passes backend.args through verbatim and ignores the
    // structured launch block and the LMCache injection; a missing runtime or
    // a non-managed kind has no managed launch either. Drop the state that
    // only a native managed launch can honour: the server rejects a native
    // backend.runtime with a leftover backend.container, and a LMCache block
    // without a native vLLM runtime is dead configuration that still counts
    // as "in use" for the LMCache lifecycle.
    const managed = runtime ? managedLaunchKind(runtime.kind, runtime.mode) : "";
    if (managed === "") {
      draft.lmcacheEnabled = false;
      if (Object.keys(draft.backendContainer).length > 0) draft.backendContainer = {};
    }
    if (!runtime) return;
    draft.backendType = runtime.kind;
    argsText = managed === "" ? draft.backendArguments : extraLaunchArguments(draft.backendArguments, managed);
    // LMCache only attaches to a native vLLM runtime: switching the runtime
    // away from vLLM turns the model's LMCache usage off so the draft cannot
    // carry a configuration the server would reject.
    if (managed !== "vllm") draft.lmcacheEnabled = false;
  }

  function updateBackendArguments(value: string): void {
    argsText = value;
    draft.backendArguments = rebuildManagedArguments(value, effectiveManagedKind, "");
    error = "";
    validation = null;
    // A full command pasted into the box is parsed into the structured fields
    // in place instead of being kept as opaque text.
    if (looksLikeLaunchCommand(value)) {
      parseLaunchIntoFields(value);
      return;
    }
    syncFieldsFromArguments(value);
  }

  // Editing an owned flag line in the editor (for example --max-num-seqs)
  // updates the matching structured field, so the field and the text never
  // disagree. The reverse direction (fields -> text) is the effect above: it
  // re-normalizes only a line whose field really holds another value, so a
  // value being typed (or one that merely differs in formatting) stays under
  // the cursor.
  function syncFieldsFromArguments(value: string): void {
    if (effectiveManagedKind === "") return;
    draft.launch = adoptLaunchFields(effectiveManagedKind, value, draft.launch);
  }

  function selectManagedModel(path: string): void {
    if (effectiveManagedKind === "" || !path) return;
    draft.launch.model = path;
    draft.backendArguments = rebuildManagedArguments(draft.backendArguments, effectiveManagedKind, path);
    argsText = draft.backendArguments;
  }

  async function loadModelFileCatalog(): Promise<void> {
    modelFilesLoading = true;
    modelFilesError = "";
    try {
      const catalog = await getModelFiles();
      modelFiles = catalog.data;
    } catch (cause) {
      modelFilesError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      modelFilesLoading = false;
    }
  }

  onMount(() => {
    void loadModelFileCatalog();
    void loadGPUCatalog();
    void loadLmcacheAvailability();
  });

  // Auxiliary fill for the free-text fields: ${PORT} and the model id are the
  // two tokens every command needs, and a handful of environment variables are
  // useful often enough that typing them from memory is the wrong default.
  // Clicking a chip appends the token to the field instead of replacing what
  // the operator already wrote.
  function commandMacros(modelId: string): string[] {
    const macros = ["${PORT}"];
    const id = modelId.trim();
    if (id !== "") macros.push(id);
    return macros;
  }

  function envMacros(): string[] {
    return ["CUDA_VISIBLE_DEVICES=", "NCCL_P2P_LEVEL=PHB", "VLLM_SLEEP_MODE=1"];
  }

  function insertAtCursor(token: string, apply: (value: string) => void, current: string): void {
    const separator = current.trim() === "" || current.endsWith("\n") ? "" : "\n";
    apply(`${current.trimEnd()}${separator}${token}`);
  }

  // The per-model LMCache toggle is only meaningful while the reserved
  // "lmcache" managed runtime has an active version. The settings page owns
  // install/activate; here the switch is gated on that check. A non-standalone
  // (in-process) attachment runs the library inside the model's own vLLM
  // process, so it needs no server version — only the mp mode is gated.
  let lmcacheAvailable = $state(false);
  let lmcacheChecked = $state(false);
  let lmcacheStandaloneUnavailable = $derived(lmcacheChecked && !lmcacheAvailable);

  async function loadLmcacheAvailability(): Promise<void> {
    try {
      const response = await fetch("/api/runtimes/lmcache");
      const payload = await response.json().catch(() => ({}));
      const status = (payload?.status ?? {}) as Record<string, unknown>;
      lmcacheAvailable = typeof status.current === "string" && status.current !== "";
    } catch {
      lmcacheAvailable = false;
    } finally {
      lmcacheChecked = true;
    }
  }

  // GPU inventory for the card selector; a boot-time snapshot, refreshed on
  // demand with the same cadence as the model file catalog.
  interface GPUInfo {
    index: number;
    uuid: string;
    name: string;
    vendor: string;
    memoryTotalBytes: number;
  }
  let gpus = $state<GPUInfo[]>([]);
  let gpusLoading = $state(false);
  let gpusError = $state("");
  let gpuMemoryTotalBytes = $derived(
    draft.launch.gpus.reduce((total, selection) => {
      const gpu = gpus.find((candidate) => (candidate.uuid || `index:${candidate.index}`) === selection || String(candidate.index) === selection);
      return total + (gpu?.memoryTotalBytes ?? 0);
    }, 0),
  );

  async function loadGPUCatalog(): Promise<void> {
    gpusLoading = true;
    gpusError = "";
    try {
      const response = await fetch("/api/gpus");
      const payload = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      gpus = (payload?.gpus ?? []) as GPUInfo[];
    } catch (cause) {
      gpusError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      gpusLoading = false;
    }
  }

  function gpuSelectionID(gpu: GPUInfo): string {
    // CUDA_VISIBLE_DEVICES is the persisted source of truth. Keep the picker
    // in CUDA's numeric-index namespace so it directly edits that value rather
    // than creating a second UUID-based GPU selection.
    return String(gpu.index);
  }

  function toggleGPU(selection: string, checked: boolean): void {
    draft.launch.gpus = checked
      ? [...new Set([...draft.launch.gpus, selection])]
      : draft.launch.gpus.filter((item) => item !== selection);
  }

  // A full engine command pasted into the launch-args editor is parsed in
  // place, entirely in the browser: parseLaunchCommand classifies the text
  // lexically (it is never executed), the structured fields are filled, and
  // the editor keeps only the remaining engine arguments. The backend's
  // launchspec parser stays the server-side reference for config migration;
  // this is its port so pasting needs no round-trip.
  let parseNote = $state("");

  // renderExtraArgs writes the surviving engine arguments back into the editor
  // as one "flag value" line each. Values are re-quoted so a JSON payload (or
  // any token with spaces/quotes) round-trips losslessly through the
  // tokenizer on the next save.
  function renderExtraArgs(tokens: string[]): string {
    const lines: string[] = [];
    for (let index = 0; index < tokens.length; index += 1) {
      const token = tokens[index];
      if (token.startsWith("-") && index + 1 < tokens.length && !tokens[index + 1].startsWith("-")) {
        lines.push(`${token} ${serializeLaunchArgument(tokens[index + 1])}`);
        index += 1;
        continue;
      }
      lines.push(serializeLaunchArgument(token));
    }
    return lines.join("\n");
  }

  // A first line that starts like a launch command (an engine entrypoint, a
  // python -m module, or a leading NAME=VALUE assignment) rather than a bare
  // engine flag is parsed instead of kept verbatim.
  function looksLikeLaunchCommand(value: string): boolean {
    const first = value.split("\n").map((line) => line.trim()).find((line) => line !== "");
    if (!first) return false;
    const lower = first.toLowerCase();
    if (/^(vllm|llama-server|llama)([\s.]|$)/.test(lower)) return true;
    if (/^python\d?(\s|$)/.test(lower)) return true;
    if (/^[a-z][a-z0-9_]*=/.test(first)) return true;
    return false;
  }

  function mapDeviceSelections(devices: string[]): string[] {
    return devices.map((device) => {
      const gpu = gpus.find(
        (candidate) => (candidate.uuid || `index:${candidate.index}`) === device || String(candidate.index) === device,
      );
      return gpu ? gpuSelectionID(gpu) : device;
    });
  }

  function applyParsedLaunch(parsed: ParsedLaunch): void {
    if (parsed.model) draft.launch.model = parsed.model;
    if (parsed.contextPerRequest > 0) draft.launch.contextPerRequest = String(parsed.contextPerRequest);
    if (parsed.maxConcurrency > 0) draft.launch.maxConcurrency = String(parsed.maxConcurrency);
    if (parsed.tensorParallelSize > 0) draft.launch.tensorParallelSize = String(parsed.tensorParallelSize);
    if (parsed.gpuMemoryUtilization != null) draft.launch.gpuMemoryUtilization = String(parsed.gpuMemoryUtilization * 100);
    if (parsed.cudaVisibleDevices.length > 0) draft.launch.gpus = mapDeviceSelections(parsed.cudaVisibleDevices);
    argsText = renderExtraArgs(parsed.extraArgs);
    draft.backendArguments = rebuildManagedArguments(argsText, effectiveManagedKind, "");
    parseNote = parsed.warnings.join(" ");
  }

  function parseLaunchIntoFields(value: string): void {
    if (effectiveManagedKind === "") return;
    parseNote = "";
    try {
      applyParsedLaunch(parseLaunchCommand(effectiveManagedKind, value));
    } catch (cause) {
      parseNote = cause instanceof Error ? cause.message : String(cause);
    }
  }

  function toggleAPI(value: string, checked: boolean): void {
    draft.backendAPIs = checked
      ? [...new Set([...draft.backendAPIs, value])]
      : draft.backendAPIs.filter((item) => item !== value);
  }

  function toggleModality(field: "capabilitiesIn" | "capabilitiesOut", value: string, checked: boolean): void {
    draft[field] = checked
      ? [...new Set([...draft[field], value])]
      : draft[field].filter((item) => item !== value);
  }

  function clearCapabilityOverrides(): void {
    draft.backendProtocol = "";
    draft.backendAPIs = [];
    draft.backendDiscover = "";
    draft.capabilitiesIn = [];
    draft.capabilitiesOut = [];
    draft.capabilitiesTools = false;
    draft.capabilitiesReranker = false;
    draft.capabilitiesContext = "";
  }

  function shortValue(value: unknown): string {
    if (typeof value === "string") return value;
    if (typeof value === "number" || typeof value === "boolean") return String(value);
    return JSON.stringify(value);
  }

  function resetDraft(): void {
    if (saving || diffDialogOpen) return;
    error = "";
    validation = null;
    cudaEnvSyncState.visible = { managed: "", stashed: "" };
    cudaEnvSyncState.order = { managed: "", stashed: "" };
    if (mode === "create") {
      draft = emptyModelDraft(modelId.trim());
      argsText = "";
      return;
    }
    const current = modelEntries(snapshot.config)[selectedModel];
    if (current !== undefined) {
      draft = draftFromModel(selectedModel, current);
      argsText = extraLaunchArguments(draft.backendArguments, effectiveManagedKind);
    }
  }

  function clearPending(): void {
    pendingPatch = null;
    pendingChanges = [];
    pendingETag = "";
    pendingModelId = "";
  }

  function issueLabel(path: string): string {
    const key = path.split(".").pop() ?? path;
    const labels: Record<string, string> = {
      name: "controlPlane.modelName",
      description: "controlPlane.modelDescription",
      aliases: "controlPlane.modelAliases",
      useModelName: "controlPlane.modelUseModelName",
      cmd: "controlPlane.modelCmd",
      cmdStop: "controlPlane.modelCmdStop",
      proxy: "controlPlane.modelProxy",
      checkEndpoint: "controlPlane.modelCheckEndpoint",
      env: "controlPlane.modelEnv",
      ttl: "controlPlane.modelTTL",
      unloadTimeout: "controlPlane.modelUnloadTimeout",
      concurrencyLimit: "controlPlane.modelConcurrency",
      unlisted: "controlPlane.modelUnlisted",
      sendLoadingState: "controlPlane.modelSendLoadingState",
      type: "controlPlane.modelBackendType",
      runtime: "controlPlane.modelRuntime",
      protocol: "controlPlane.modelProtocol",
      apis: "controlPlane.modelAPIs",
      discover: "controlPlane.modelDiscover",
      args: "controlPlane.modelArguments",
      mode: "controlPlane.modelLifecycle",
      sleepLevel: "controlPlane.modelSleepLevel",
      vramMiB: "controlPlane.modelVRAM",
      ramMiB: "controlPlane.modelRAM",
      gpuAffinity: "controlPlane.modelGPUAffinity",
      priority: "controlPlane.modelPriority",
      evictionPriority: "controlPlane.modelEvictionPriority",
      in: "controlPlane.modelInputModalities",
      out: "controlPlane.modelOutputModalities",
      tools: "controlPlane.modelTools",
      reranker: "controlPlane.modelReranker",
      context: "controlPlane.modelContext",
      filters: "controlPlane.modelFilters",
      stripParams: "controlPlane.modelStripParams",
      setParams: "controlPlane.modelSetParams",
      setParamsByID: "controlPlane.modelSetParamsByID",
      macros: "controlPlane.modelMacros",
      metadata: "controlPlane.modelMetadata",
      timeouts: "controlPlane.modelTimeouts",
      ignoreWebsockets: "controlPlane.modelIgnoreWebsockets",
      container: "controlPlane.modelContainer",
      lmcache: "controlPlane.modelLmcache",
      pricing: "controlPlane.modelPricing",
    };
    return labels[key] ? $translate(labels[key]) : path;
  }

  function displayValue(value: unknown): string {
    if (value === undefined) return $translate("controlPlane.configValueEmpty");
    if (typeof value === "boolean") return value ? $translate("common.yes") : $translate("common.no");
    if (Array.isArray(value)) return value.join(", ") || $translate("controlPlane.configValueEmpty");
    if (typeof value === "object" && value !== null) {
      try { return JSON.stringify(value); } catch { return String(value); }
    }
    return String(value);
  }

  function prepareSave(): void {
    if (!snapshot.writable || !snapshot.etag || saving) return;
    error = "";
    validation = null;
    const containerImage = String(draft.backendContainer["image"] ?? "").trim();
    if (draft.launchMode === "backend" && effectiveManagedKind !== "" && selectedManagedModelPath === "" && containerImage === "") {
      error = $translate("controlPlane.modelManagedModelRequired");
      return;
    }
    try {
      // Once the structured launch block carries the model target it owns the
      // reserved engine flags: the server's launch validator rejects a save
      // whose backend.args still repeat them ("backend.args --model is managed
      // by backend.launch"). Store only the surviving extras — the editor text
      // keeps rendering the canonical flag lines from the structured fields.
      if (
        draft.launchMode === "backend" &&
        (effectiveManagedKind === "vllm" || effectiveManagedKind === "llamacpp") &&
        trimmedText(draft.launch.model) !== ""
      ) {
        draft.backendArguments = stripOwnedLaunchArguments(effectiveManagedKind, draft.backendArguments);
      }
      const patch = buildModelPatch(snapshot.config, draft, editing);
      const operation = patch[0] as { path?: string; value?: unknown };
      const value = operation.path === "/models"
        ? (operation.value as Record<string, unknown> | undefined)?.[trimmedText(draft.id)]
        : operation.value;
      const previous = editing ? currentModel : undefined;
      const changes = summarizeChanges(previous, value);
      if (editing && changes.length === 0) {
        error = $translate("controlPlane.modelNoChanges");
        return;
      }
      pendingPatch = patch;
      pendingChanges = changes;
      pendingETag = snapshot.etag;
      pendingModelId = trimmedText(draft.id) || selectedModel;
      void validatePatch(patch);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    }
  }

  async function validatePatch(patch: unknown[]): Promise<void> {
    saving = true;
    try {
      const response = await fetch("/api/config/validate", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify(patch),
      });
      const payload = await response.json().catch(() => ({})) as Validation;
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      validation = payload;
      if (!payload.valid) {
        clearPending();
        return;
      }
      diffDialogOpen = true;
    } catch (cause) {
      clearPending();
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      saving = false;
    }
  }

  function cancelDiff(): void {
    if (saving) return;
    diffDialogOpen = false;
    clearPending();
    validation = null;
  }

  async function confirmSave(): Promise<void> {
    if (!pendingPatch || !pendingETag || saving) return;
    saving = true;
    error = "";
    try {
      const response = await fetch("/api/config", {
        method: "PATCH",
        headers: { "Content-Type": "application/json", "If-Match": pendingETag },
        body: JSON.stringify(pendingPatch),
      });
      const payload: unknown = await response.json().catch(() => ({}));
      if (response.status === 412) throw new Error($translate("controlPlane.conflict"));
      if (!response.ok) throw new Error(errorMessageFromPayload(payload, `HTTP ${response.status}`));
      const payloadObject = payload && typeof payload === "object" ? payload as { valid?: unknown } : null;
      if (payloadObject?.valid === false) {
        validation = payload as Validation;
        diffDialogOpen = false;
        clearPending();
        return;
      }
      onSnapshot(payload as ConfigSnapshot);
      const savedID = pendingModelId;
      diffDialogOpen = false;
      clearPending();
      validation = null;
      onSaved(savedID);
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
      diffDialogOpen = false;
      clearPending();
    } finally {
      saving = false;
    }
  }
</script>

<section
  class={embedded ? "" : "border-border/70 bg-card/30 rounded-lg border p-4"}
  aria-labelledby={showTitle ? "model-editor-title" : undefined}
  aria-label={showTitle || showModelName ? undefined : $translate("controlPlane.modelEditorTitle")}
>
  {#if showTitle}
    <div class="mb-4 flex flex-wrap items-start gap-3">
      <div class="flex min-w-0 items-center gap-2">
        <span class="flex size-5 shrink-0 items-center justify-center leading-none"><Server class="text-muted-foreground size-4" aria-hidden="true" /></span>
        <div class="min-w-0">
          <h2 id="model-editor-title" class="text-base font-semibold">
            {mode === "create" ? $translate("models.createConfigTitle") : $translate("controlPlane.modelEditorTitle")}
          </h2>
        </div>
      </div>
    </div>
  {/if}

  {#if mode === "edit" && modelNames.length === 0}
    <div class="border-border/70 bg-muted/20 flex items-center gap-2 rounded-md border border-dashed p-4 text-sm text-muted-foreground" role="status">
      <AlertTriangle class="size-4 shrink-0" aria-hidden="true" />
      {$translate("controlPlane.modelNoModels")}
    </div>
  {:else}
    <form onsubmit={(event) => { event.preventDefault(); prepareSave(); }}>
      <fieldset disabled={saving || diffDialogOpen || !snapshot.writable} aria-busy={saving}>
        {#if mode === "edit" && !modelId}
          <label class="mb-4 grid max-w-md gap-1.5 text-sm" for="model-editor-select">
            <span class="font-medium">{$translate("controlPlane.modelSelect")}</span>
            <select id="model-editor-select" class={selectClass} bind:value={selectedModel}>
              {#each modelNames as name (name)}<option value={name}>{name}</option>{/each}
            </select>
          </label>
        {/if}

        <Tabs.Root bind:value={activeTab}>
          <Tabs.List class="sr-only">
            <Tabs.Trigger value="overview">{$translate("settingsCenter.model.editTitle")}</Tabs.Trigger>
            <Tabs.Trigger value="resources">{$translate("controlPlane.modelTabResources")}</Tabs.Trigger>
            <Tabs.Trigger value="advanced">{$translate("controlPlane.modelTabAdvanced")}</Tabs.Trigger>
          </Tabs.List>

          <Tabs.Content value="overview" class="mt-0 grid gap-2.5">
            <section class="border-border/70 bg-muted/10 grid gap-2.5 rounded-lg border p-3" aria-labelledby="model-overview-identity">
              <header class="flex min-h-5 items-center gap-2 leading-5"><span class="flex size-5 shrink-0 items-center justify-center leading-none"><Server class="text-muted-foreground size-4" aria-hidden="true" /></span><h3 id="model-overview-identity" class="text-sm font-semibold leading-5">{$translate("settingsCenter.model.basicInformation")}</h3></header>
              <div class="grid gap-2.5 sm:grid-cols-[minmax(0,1.15fr)_minmax(0,1fr)_minmax(0,1fr)]">
                {#if mode === "create"}
                  <label class="grid gap-1 text-sm" for="model-overview-id"><span class="font-medium">{$translate("models.modelID")}</span><Input id="model-overview-id" class={inputClass} bind:value={draft.id} maxlength={200} autocomplete="off" required /></label>
                {:else}
                  <div class="grid gap-1 text-sm"><span class="font-medium">{$translate("models.modelID")}</span><div class="border-border/70 bg-background/50 flex h-9 items-center rounded-md border px-3"><code class="truncate text-xs">{selectedModel}</code></div></div>
                {/if}
                <label class="grid gap-1 text-sm" for="model-overview-name"><span class="font-medium">{$translate("controlPlane.modelName")}</span><Input id="model-overview-name" class={inputClass} bind:value={draft.name} maxlength={200} autocomplete="off" /></label>
                <label class="grid gap-1 text-sm" for="model-overview-description"><span class="font-medium">{$translate("controlPlane.modelDescription")}</span><Input id="model-overview-description" class={inputClass} bind:value={draft.description} maxlength={1000} autocomplete="off" /></label>
              </div>
              <div class="grid gap-1 text-sm">
                <label class="font-medium" for="model-overview-aliases">{$translate("controlPlane.modelAliases")}</label>
                <Textarea id="model-overview-aliases" class="min-h-16 resize-y font-sans text-sm" bind:value={draft.aliases} placeholder={$translate("controlPlane.modelListHint")} rows={2} />
              </div>
            </section>

            {#if effectiveManagedKind === ""}
              <section class="border-border/70 bg-muted/10 grid gap-2.5 rounded-lg border p-3" aria-labelledby="model-overview-connection">
                <header class="flex min-h-5 items-center gap-2 leading-5"><span class="flex size-5 shrink-0 items-center justify-center leading-none"><Link2 class="text-muted-foreground size-4" aria-hidden="true" /></span><h3 id="model-overview-connection" class="text-sm font-semibold leading-5">{$translate("settingsCenter.model.connectionProxy")}</h3></header>
                <div class="grid gap-2.5 sm:grid-cols-3">
                  <label class="grid gap-1 text-sm" for="model-overview-proxy"><span class="font-medium">{$translate("controlPlane.modelProxy")}</span><Input id="model-overview-proxy" class={`${inputClass} font-mono text-xs`} bind:value={draft.proxy} placeholder={'http://127.0.0.1:${PORT}'} /></label>
                  <label class="grid gap-1 text-sm" for="model-overview-endpoint"><span class="font-medium">{$translate("controlPlane.modelCheckEndpoint")}</span><Input id="model-overview-endpoint" class={`${inputClass} font-mono text-xs`} bind:value={draft.checkEndpoint} placeholder="/health" /></label>
                  <label class="grid gap-1 text-sm" for="model-overview-upstream-name"><span class="font-medium">{$translate("controlPlane.modelUseModelName")}</span><Input id="model-overview-upstream-name" class={inputClass} bind:value={draft.useModelName} /></label>
                </div>
                <label class="grid max-w-64 gap-1 text-sm" for="model-overview-protocol"><span class="font-medium">{$translate("controlPlane.modelProtocol")}</span><select id="model-overview-protocol" class={selectClass} bind:value={draft.backendProtocol}><option value="">{$translate("controlPlane.modelProtocolDefault")}</option><option value="native">{$translate("controlPlane.modelProtocolNative")}</option><option value="responsesToChat">{$translate("controlPlane.modelProtocolResponsesToChat")}</option></select></label>
              </section>
            {/if}

            <section class="border-border/70 bg-muted/10 grid gap-2.5 rounded-lg border p-3" aria-labelledby="model-overview-startup">
              <header class="flex min-h-5 items-center gap-2 leading-5"><span class="flex size-5 shrink-0 items-center justify-center leading-none"><Terminal class="text-muted-foreground size-4" aria-hidden="true" /></span><h3 id="model-overview-startup" class="text-sm font-semibold leading-5">{$translate("settingsCenter.model.startupConfiguration")}</h3></header>
              <div class="grid grid-cols-1 gap-2 sm:grid-cols-2" role="group" aria-label={$translate("controlPlane.modelLaunchMode")}>
                <button type="button" class={`border-border/70 flex min-h-14 flex-col justify-center rounded-md border px-3 text-left text-sm transition-colors hover:bg-muted/60 ${draft.launchMode === "command" ? "border-primary bg-primary/10 text-foreground" : ""}`} aria-pressed={draft.launchMode === "command"} onclick={() => setLaunchMode("command")}>
                  <strong class="font-medium">{$translate("controlPlane.modelLaunchCommand")}</strong>
                </button>
                <button type="button" class={`border-border/70 flex min-h-14 flex-col justify-center rounded-md border px-3 text-left text-sm transition-colors hover:bg-muted/60 ${draft.launchMode === "backend" ? "border-primary bg-primary/10 text-foreground" : ""}`} aria-pressed={draft.launchMode === "backend"} onclick={() => setLaunchMode("backend")}>
                  <strong class="flex flex-wrap items-center gap-1.5 font-medium">{$translate("controlPlane.modelLaunchBackend")}<span class="bg-primary/15 text-primary rounded-full px-1.5 py-0.5 text-[10px] font-medium leading-none">{$translate("controlPlane.modelLaunchBackendRecommended")}</span></strong>
                </button>
              </div>
              {#if draft.launchMode === "command"}
                <div class="grid gap-2.5 sm:grid-cols-[minmax(0,1.15fr)_minmax(0,0.85fr)]">
                  <label class="grid gap-1 text-sm" for="model-overview-cmd"><span class="font-medium">{$translate("controlPlane.modelCmd")}</span><Textarea id="model-overview-cmd" class="h-24 max-h-60 overflow-y-auto font-mono text-xs leading-5" bind:value={draft.cmd} placeholder={'llama-server --model /models/model.gguf --port ${PORT}'} required />
                    <span class="text-muted-foreground text-xs">{$translate("controlPlane.modelCmdHint")}</span>
                    <div class="flex flex-wrap items-center gap-1.5">
                      {#each commandMacros(draft.id) as macro (macro)}<button type="button" class="bg-muted hover:bg-muted/70 rounded px-1.5 py-0.5 font-mono text-[11px] transition-colors" onclick={() => insertAtCursor(macro, (value) => (draft.cmd = value), draft.cmd)}>+ {macro}</button>{/each}
                    </div>
                  </label>
                  <label class="grid gap-1 text-sm" for="model-overview-env"><span class="font-medium">{$translate("controlPlane.modelEnv")}</span><Textarea id="model-overview-env" class="h-24 max-h-60 overflow-y-auto font-mono text-xs leading-5" bind:value={draft.env} placeholder="CUDA_VISIBLE_DEVICES=0,1" />
                    <span class="text-muted-foreground text-xs">{$translate("controlPlane.modelEnvHint")}</span>
                    <div class="flex flex-wrap items-center gap-1.5">
                      {#each envMacros() as macro (macro)}<button type="button" class="bg-muted hover:bg-muted/70 rounded px-1.5 py-0.5 font-mono text-[11px] transition-colors" onclick={() => insertAtCursor(macro, (value) => (draft.env = value), draft.env)}>+ {macro}</button>{/each}
                    </div>
                  </label>
                </div>
                <label class="grid max-w-md gap-1 text-sm" for="model-overview-cmd-stop"><span class="font-medium">{$translate("controlPlane.modelCmdStop")}</span><Input id="model-overview-cmd-stop" class={`${inputClass} font-mono text-xs`} bind:value={draft.cmdStop} placeholder={$translate("controlPlane.modelCmdStopPlaceholder")} /></label>
              {:else}
                <div class="grid gap-2.5 sm:grid-cols-2">
                  <label class="grid gap-1 text-sm" for="model-overview-runtime"><span class="font-medium">{$translate("controlPlane.modelRuntime")}</span><select id="model-overview-runtime" class={selectClass} value={draft.backendRuntime} onchange={(event) => selectRuntime(event.currentTarget.value)}><option value="">{$translate("controlPlane.modelRuntimeOptional")}</option>{#if draft.backendRuntime && !runtimes.some((runtime) => runtime.name === draft.backendRuntime)}<option value={draft.backendRuntime}>{draft.backendRuntime}</option>{/if}{#each runtimes as runtime (runtime.name)}<option value={runtime.name}>{runtime.name}{runtime.kind ? ` · ${runtime.kind === "llamacpp" ? "llama.cpp" : runtime.kind}` : ""}</option>{/each}</select></label>
                  {#if draft.backendRuntime}
                    <label class="grid gap-1 text-sm" for="model-overview-runtime-version"><span class="font-medium">{$translate("controlPlane.modelRuntimeVersion")}</span><select id="model-overview-runtime-version" class={selectClass} bind:value={draft.backendRuntimeVersion} disabled={runtimeVersionCatalog.length === 0}><option value="">{runtimeVersionPlaceholder}</option>{#if draft.backendRuntimeVersion && !runtimeVersionCatalog.includes(draft.backendRuntimeVersion)}<option value={draft.backendRuntimeVersion}>{draft.backendRuntimeVersion}</option>{/if}{#each runtimeVersionCatalog as version (version)}<option value={version}>{runtimeVersionLabel(version)}</option>{/each}</select>{#if runtimeVersionCatalogError}<p class="text-destructive text-xs" role="alert">{runtimeVersionCatalogError}</p>{/if}</label>
                  {/if}
                  {#if !selectedRuntime}
                    <label class="grid gap-1 text-sm" for="model-overview-type"><span class="font-medium">{$translate("controlPlane.modelBackendType")}</span><select id="model-overview-type" class={selectClass} bind:value={draft.backendType}><option value="">{$translate("controlPlane.modelBackendAuto")}</option><option value="llamacpp">llama.cpp</option><option value="vllm">vLLM</option><option value="generic">Generic</option></select></label>
                  {/if}
                  {#if selectedRuntime && runtimeEntrypoint}
                    <div class="grid gap-1 text-sm sm:col-span-2"><span class="font-medium">{$translate("controlPlane.modelRuntimeEntrypoint")}</span><div class="border-border/70 bg-muted/30 flex h-9 items-center rounded-md border px-3"><code class="text-sm">{runtimeEntrypoint}</code></div></div>
                  {/if}
                  <div class="grid gap-1.5 text-sm sm:col-span-2">
                    <span class="font-medium">{$translate("controlPlane.modelParameters")}</span>
                    {#if effectiveManagedKind !== ""}
                      <div class="mb-1 grid gap-2.5 sm:grid-cols-2">
                        <div class="grid gap-1 text-sm">
                          <div class="flex items-center justify-between gap-2"><span class="font-medium">{$translate("controlPlane.launchModel")}</span><Button type="button" variant="ghost" size="icon-sm" onclick={() => void loadModelFileCatalog()} disabled={modelFilesLoading} aria-label={$translate("modelFiles.refresh")} title={$translate("modelFiles.refresh")}><RefreshCw class={`size-4 ${modelFilesLoading ? "animate-spin" : ""}`} aria-hidden="true" /></Button></div>
                          <select class={selectClass} value={selectedManagedModelPath} disabled={modelFilesLoading} onchange={(event) => selectManagedModel(event.currentTarget.value)}>
                            <option value="">{modelFilesLoading ? $translate("modelFiles.loading") : $translate("controlPlane.modelManagedFileSelect")}</option>
                            {#if selectedManagedModelPath && !selectedManagedTarget}<option value={selectedManagedModelPath}>{selectedManagedModelPath}</option>{/if}
                            {#each managedTargets as target (target.value)}<option value={target.value}>{target.label}</option>{/each}
                          </select>
                          {#if selectedManagedTarget}<code class="text-muted-foreground truncate text-xs" title={selectedManagedTarget.path}>{selectedManagedTarget.path}</code>{/if}
                          {#if modelFilesError}<p class="text-destructive text-xs" role="alert">{$translate("modelFiles.scanError", { message: modelFilesError })}</p>
                          {:else if !modelFilesLoading && managedTargets.length === 0}<p class="text-muted-foreground text-xs">{$translate("modelFiles.empty")}</p>{/if}
                        </div>
                        <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.launchServedModelName")}</span><Input class={inputClass} readonly bind:value={draft.launch.servedModelName} /></label>
                        <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.launchContextPerRequest")}</span><Input class={inputClass} type="number" min="1" bind:value={draft.launch.contextPerRequest} placeholder="262144" /></label>
                        <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.launchMaxConcurrency")}</span><Input class={inputClass} type="number" min="1" bind:value={draft.launch.maxConcurrency} placeholder="2" /></label>
                        {#if effectiveManagedKind === "vllm"}
                          <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.launchTensorParallelSize")}</span><Input class={inputClass} type="number" min="1" bind:value={draft.launch.tensorParallelSize} placeholder={$translate("controlPlane.launchTensorParallelSizePlaceholder")} /></label>
                          <label class="grid gap-1 text-sm sm:col-span-2"><span class="font-medium">{$translate("controlPlane.launchGPUMemoryUtilization")}</span><div class="flex max-w-56 items-center gap-1"><Input class={inputClass} type="number" min="1" max="100" bind:value={draft.launch.gpuMemoryUtilization} placeholder="90" /><span class="text-muted-foreground text-sm">%</span></div></label>
                        {/if}
                      </div>
                      <div class="mb-1 grid gap-1.5 text-sm">
                        <div class="flex items-center justify-between gap-2">
                          <span class="font-medium">{$translate("controlPlane.launchGPUs")}</span>
                          <div class="flex items-center gap-1">
                            <Button type="button" variant="ghost" size="sm" onclick={() => (draft.launch.gpus = gpus.map(gpuSelectionID))}>{$translate("controlPlane.launchGPUSelectAll")}</Button>
                            <Button type="button" variant="ghost" size="sm" onclick={() => (draft.launch.gpus = [])}>{$translate("controlPlane.launchGPUClear")}</Button>
                            <Button type="button" variant="ghost" size="icon-sm" onclick={() => void loadGPUCatalog()} aria-label={$translate("modelFiles.refresh")}><RefreshCw class={`size-4 ${gpusLoading ? "animate-spin" : ""}`} aria-hidden="true" /></Button>
                          </div>
                        </div>
                        {#if gpusError}<p class="text-destructive text-xs" role="alert">{gpusError}</p>
                        {:else if gpus.length === 0}<p class="text-muted-foreground text-xs">{$translate("controlPlane.launchGPUsEmpty")}</p>
                        {:else}
                          <div class="grid gap-1.5 sm:grid-cols-2">
                            {#each gpus as gpu (gpuSelectionID(gpu))}
                              <label class="border-border/70 hover:bg-muted/40 flex min-h-10 items-center gap-2 rounded-md border px-2.5 text-xs transition-colors">
                                <input type="checkbox" checked={draft.launch.gpus.includes(gpuSelectionID(gpu))} onchange={(event) => toggleGPU(gpuSelectionID(gpu), event.currentTarget.checked)} />
                                <span class="min-w-0 flex-1"><strong class="block font-medium">GPU {gpu.index}</strong><span class="text-muted-foreground">{gpu.name}{gpu.memoryTotalBytes > 0 ? ` · ${(gpu.memoryTotalBytes / 1024 ** 3).toFixed(0)} GB` : ""}</span></span>
                              </label>
                            {/each}
                          </div>
                          {#if gpuMemoryTotalBytes > 0}<p class="text-muted-foreground text-xs">{$translate("controlPlane.launchGPUMemoryTotal", { total: (gpuMemoryTotalBytes / 1024 ** 3).toFixed(0) })}</p>{/if}
                        {/if}
                      </div>
                    {/if}
                    <span class="font-medium">{selectedRuntime ? $translate("controlPlane.modelRuntimeArguments") : $translate("controlPlane.modelArguments")}</span>
                    <LaunchArgsCodeEditor compact ariaLabel={selectedRuntime ? $translate("controlPlane.modelRuntimeArguments") : $translate("controlPlane.modelArguments")} bind:value={argsText} onChange={(value) => updateBackendArguments(value)} placeholder={effectiveManagedKind === "llamacpp" ? "--flash-attn auto" : "--trust-remote-code"} />
                    {#if parseNote}<span class="text-destructive text-xs" role="status">{parseNote}</span>{/if}
                  </div>
                  <div class="grid gap-1 text-sm sm:col-span-2">
                    <label class="grid gap-1" for="model-overview-env-managed"><span class="font-medium">{$translate("controlPlane.modelEnv")}</span><Textarea id="model-overview-env-managed" class="h-20 max-h-40 overflow-y-auto font-mono text-xs leading-5" bind:value={draft.env} placeholder="CUDA_VISIBLE_DEVICES=0,1" /></label>
                    <div class="flex flex-wrap items-center gap-1.5">
                      {#each envMacros() as macro (macro)}<button type="button" class="bg-muted hover:bg-muted/70 rounded px-1.5 py-0.5 font-mono text-[11px] transition-colors" onclick={() => insertAtCursor(macro, (value) => (draft.env = value), draft.env)}>+ {macro}</button>{/each}
                    </div>
                  </div>
                  {#if effectiveManagedKind === "vllm"}
                    <div class="border-border/70 grid gap-2 rounded-md border p-3 sm:col-span-2">
                      <label class="flex min-h-10 items-center gap-2 text-sm"><input type="checkbox" bind:checked={draft.lmcacheEnabled} disabled={lmcacheStandaloneUnavailable && !draft.lmcacheEnabled} />{$translate("controlPlane.modelLmcache")}</label>
                      {#if lmcacheChecked && lmcacheStandaloneUnavailable && draft.lmcacheMode === "mp"}
                        <p class="text-amber-500 text-xs" role="alert">{$translate("controlPlane.modelLmcacheNotInstalled")}</p>
                      {/if}
                      {#if draft.lmcacheEnabled}
                        <label class="grid gap-1 text-sm" for="model-lmcache-mode"><span class="font-medium">{$translate("controlPlane.modelLmcacheMode")}</span><select id="model-lmcache-mode" class={selectClass} bind:value={draft.lmcacheMode}><option value="mp">{$translate("controlPlane.modelLmcacheModeMp")}</option><option value="inProcess">{$translate("controlPlane.modelLmcacheModeInProcess")}</option></select><span class="text-muted-foreground text-xs">{draft.lmcacheMode === "inProcess" ? $translate("controlPlane.modelLmcacheModeInProcessHint") : $translate("controlPlane.modelLmcacheModeMpHint")}</span></label>
                        {#if draft.lmcacheMode === "mp"}
                          <div class="grid gap-2 sm:grid-cols-2">
                            <label class="grid gap-1 text-sm" for="model-lmcache-host"><span class="font-medium">{$translate("controlPlane.modelLmcacheHost")}</span><Input id="model-lmcache-host" class={inputClass} bind:value={draft.lmcacheHost} placeholder={$translate("controlPlane.modelLmcacheHostPlaceholder")} autocomplete="off" /></label>
                            <label class="grid gap-1 text-sm" for="model-lmcache-port"><span class="font-medium">{$translate("controlPlane.modelLmcachePort")}</span><Input id="model-lmcache-port" class={inputClass} type="number" min="1" max="65535" bind:value={draft.lmcachePort} placeholder={$translate("controlPlane.modelLmcachePortPlaceholder")} /></label>
                          </div>
                        {:else}
                          <label class="grid gap-1 text-sm" for="model-lmcache-chunk"><span class="font-medium">{$translate("controlPlane.modelLmcacheChunkSize")}</span><Input id="model-lmcache-chunk" class={inputClass} type="number" min="0" bind:value={draft.lmcacheChunkSize} placeholder={$translate("controlPlane.modelLmcacheChunkSizePlaceholder")} /><span class="text-muted-foreground text-xs">{$translate("controlPlane.modelLmcacheChunkSizeHint")}</span></label>
                        {/if}
                        <label class="grid gap-1 text-sm" for="model-lmcache-role"><span class="font-medium">{$translate("controlPlane.modelLmcacheRole")}</span><select id="model-lmcache-role" class={selectClass} bind:value={draft.lmcacheRole}><option value="">{$translate("controlPlane.modelLmcacheRoleBoth")}</option><option value="kv_producer">{$translate("controlPlane.modelLmcacheRoleProducer")}</option><option value="kv_consumer">{$translate("controlPlane.modelLmcacheRoleConsumer")}</option></select></label>
                      {/if}
                    </div>
                  {/if}
                </div>
              {/if}
            </section>

            <section class="border-border/70 bg-muted/10 grid gap-2.5 rounded-lg border p-3" aria-labelledby="model-overview-lifecycle">
              <header class="flex min-h-5 items-center gap-2 leading-5"><span class="flex size-5 shrink-0 items-center justify-center leading-none"><Server class="text-muted-foreground size-4" aria-hidden="true" /></span><h3 id="model-overview-lifecycle" class="text-sm font-semibold leading-5">{$translate("settingsCenter.model.lifecycleResources")}</h3></header>
              <div class="grid gap-2.5 sm:grid-cols-5">
                <label class="grid gap-1 text-sm" for="model-overview-ttl"><span class="font-medium">{$translate("controlPlane.modelTTL")}</span><Input id="model-overview-ttl" class={inputClass} type="number" min="-1" bind:value={draft.ttl} placeholder={globalTTLValue !== "" ? $translate("controlPlane.modelInheritGlobal", { value: globalTTLValue }) : $translate("controlPlane.modelInherit")} /></label>
                <label class="grid gap-1 text-sm" for="model-overview-unload"><span class="font-medium">{$translate("controlPlane.modelUnloadTimeout")}</span><Input id="model-overview-unload" class={inputClass} type="number" min="0" bind:value={draft.unloadTimeout} placeholder={globalUnloadValue !== "" ? $translate("controlPlane.modelInheritGlobal", { value: globalUnloadValue }) : $translate("controlPlane.modelInherit")} /></label>
                <label class="grid gap-1 text-sm" for="model-overview-vram"><span class="font-medium">{$translate("controlPlane.modelVRAM")}</span><Input id="model-overview-vram" class={inputClass} type="number" min="0" bind:value={draft.vramMiB} placeholder={$translate("controlPlane.modelUnlimitedHint")} /></label>
                <label class="grid gap-1 text-sm" for="model-overview-ram"><span class="font-medium">{$translate("controlPlane.modelRAM")}</span><Input id="model-overview-ram" class={inputClass} type="number" min="0" bind:value={draft.ramMiB} placeholder={$translate("controlPlane.modelUnlimitedHint")} /></label>
                <label class="grid gap-1 text-sm" for="model-overview-concurrency"><span class="font-medium">{$translate("controlPlane.modelConcurrency")}</span><Input id="model-overview-concurrency" class={inputClass} type="number" min="0" bind:value={draft.concurrencyLimit} placeholder={$translate("controlPlane.modelUnlimitedHint")} /></label>
              </div>
            </section>

            <section class="border-border/70 bg-muted/10 grid gap-2.5 rounded-lg border p-3" aria-labelledby="model-overview-compatibility">
              <header class="flex min-h-5 items-center gap-2 leading-5"><span class="flex size-5 shrink-0 items-center justify-center leading-none"><Filter class="text-muted-foreground size-4" aria-hidden="true" /></span><h3 id="model-overview-compatibility" class="text-sm font-semibold leading-5">{$translate("settingsCenter.model.compatibilityFilters")}</h3></header>
              <div class="border-border/70 grid gap-2 rounded-md border p-3">
                <div class="flex items-center justify-between gap-3">
                  <span class="text-sm font-medium">{$translate("controlPlane.modelDisabled")}</span>
                  <Switch.Root checked={draft.disabled} onCheckedChange={(next) => (draft.disabled = next)} aria-label={$translate("controlPlane.modelDisabled")} />
                </div>
                <p class="text-muted-foreground text-xs">{$translate("controlPlane.modelDisabledHint")}</p>
              </div>
              <div class="grid gap-2.5 sm:grid-cols-[10rem_10rem_minmax(0,1fr)] sm:items-end">
                <label class="grid gap-1 text-sm" for="model-overview-websocket"><span class="font-medium">{$translate("controlPlane.modelIgnoreWebsockets")}</span><select id="model-overview-websocket" class={selectClass} bind:value={draft.ignoreWebsockets}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="true">{$translate("common.yes")}</option><option value="false">{$translate("common.no")}</option></select></label>
                <label class="grid gap-1 text-sm" for="model-overview-loading-state"><span class="font-medium">{$translate("controlPlane.modelSendLoadingState")}</span><select id="model-overview-loading-state" class={selectClass} bind:value={draft.sendLoadingState}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="true">{$translate("common.yes")}</option><option value="false">{$translate("common.no")}</option></select></label>
                <label class="grid gap-1 text-sm" for="model-overview-strip"><span class="font-medium">{$translate("controlPlane.modelStripParams")}</span><Input id="model-overview-strip" class={inputClass} bind:value={draft.filtersStripParams} placeholder="temperature, top_p" /></label>
              </div>
              <div class="grid gap-1 text-sm">
                <div class="flex items-center justify-between gap-2"><span class="font-medium">{$translate("controlPlane.modelSetParams")}</span><Button type="button" variant="ghost" size="icon-sm" aria-label={$translate("controlPlane.modelTabAdvanced")} title={$translate("controlPlane.modelTabAdvanced")} onclick={() => (activeTab = "advanced")}><SlidersHorizontal class="size-4" aria-hidden="true" /></Button></div>
                <div class="border-border/70 bg-background/50 flex min-h-9 flex-wrap items-center gap-1.5 rounded-md border px-2.5 py-1.5">
                  {#each Object.entries(draft.filtersSetParams) as [key, value] (key)}<code class="bg-muted rounded px-2 py-0.5 text-xs">{key}={shortValue(value)}</code>{:else}<span class="text-muted-foreground text-xs">—</span>{/each}
                </div>
              </div>
            </section>

            <details class="border-border/70 group rounded-lg border">
              <summary class="hover:bg-muted/30 flex cursor-pointer list-none items-center gap-2 px-3 py-2.5 text-sm font-medium [&::-webkit-details-marker]:hidden"><SlidersHorizontal class="text-muted-foreground size-4" aria-hidden="true" />{$translate("settingsCenter.model.advancedSettings")}</summary>
              <div class="border-border/70 grid gap-3 border-t p-3"><div class="flex items-center justify-between gap-3 rounded-md border px-3 py-2"><span class="text-sm font-medium">{$translate("controlPlane.modelUnlisted")}</span><Switch.Root checked={draft.unlisted} onCheckedChange={(next) => (draft.unlisted = next)} /></div><div class="flex flex-wrap gap-2"><Button type="button" variant="outline" size="sm" onclick={() => (activeTab = "advanced")}>{$translate("controlPlane.modelTabAdvanced")}</Button><Button type="button" variant="outline" size="sm" onclick={() => (activeTab = "resources")}>{$translate("controlPlane.modelTabResources")}</Button></div></div>
            </details>
          </Tabs.Content>

          <Tabs.Content value="resources" class="mt-0 grid gap-5">
            <div><Button type="button" variant="ghost" size="sm" onclick={() => (activeTab = "overview")}>{$translate("settingsCenter.model.backToOverview")}</Button></div>
            <div class="grid gap-4 md:grid-cols-2 lg:grid-cols-4">
              <label class="grid gap-1.5 text-sm" for="model-editor-ttl"><span class="font-medium">{$translate("controlPlane.modelTTL")}</span><Input id="model-editor-ttl" class={inputClass} type="number" min="-1" bind:value={draft.ttl} placeholder={globalTTLValue !== "" ? $translate("controlPlane.modelInheritGlobal", { value: globalTTLValue }) : $translate("controlPlane.modelInherit")} /></label>
              <label class="grid gap-1.5 text-sm" for="model-editor-unload-timeout"><span class="font-medium">{$translate("controlPlane.modelUnloadTimeout")}</span><Input id="model-editor-unload-timeout" class={inputClass} type="number" min="0" bind:value={draft.unloadTimeout} placeholder={globalUnloadValue !== "" ? $translate("controlPlane.modelInheritGlobal", { value: globalUnloadValue }) : $translate("controlPlane.modelInherit")} /></label>
              <label class="grid gap-1.5 text-sm" for="model-editor-lifecycle"><span class="font-medium">{$translate("controlPlane.modelLifecycle")}</span><select id="model-editor-lifecycle" class={selectClass} bind:value={draft.lifecycleMode}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="process">process</option><option value="sleep">sleep</option></select></label>
              <label class="grid gap-1.5 text-sm" for="model-editor-concurrency"><span class="font-medium">{$translate("controlPlane.modelConcurrency")}</span><Input id="model-editor-concurrency" class={inputClass} type="number" min="0" bind:value={draft.concurrencyLimit} placeholder={$translate("controlPlane.modelUnlimitedHint")} /></label>
            </div>
            <div class="grid gap-4 md:grid-cols-2">
              <label class="grid gap-1.5 text-sm" for="model-editor-sleep-level"><span class="font-medium">{$translate("controlPlane.modelSleepLevel")}</span><select id="model-editor-sleep-level" class={selectClass} bind:value={draft.sleepLevel}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="1">1</option><option value="2">2</option></select></label>
              <label class="grid gap-1.5 text-sm" for="model-editor-gpu-affinity"><span class="font-medium">{$translate("controlPlane.modelGPUAffinity")}</span><Input id="model-editor-gpu-affinity" class={inputClass} bind:value={draft.gpuAffinity} placeholder="cuda:0, cuda:1" /></label>
            </div>
            <div class="grid gap-4 md:grid-cols-4">
              <label class="grid gap-1.5 text-sm" for="model-editor-vram"><span class="font-medium">{$translate("controlPlane.modelVRAM")}</span><Input id="model-editor-vram" class={inputClass} type="number" min="0" bind:value={draft.vramMiB} placeholder={$translate("controlPlane.modelUnlimitedHint")} /></label>
              <label class="grid gap-1.5 text-sm" for="model-editor-ram"><span class="font-medium">{$translate("controlPlane.modelRAM")}</span><Input id="model-editor-ram" class={inputClass} type="number" min="0" bind:value={draft.ramMiB} placeholder={$translate("controlPlane.modelUnlimitedHint")} /></label>
              <label class="grid gap-1.5 text-sm" for="model-editor-priority"><span class="font-medium">{$translate("controlPlane.modelPriority")}</span><Input id="model-editor-priority" class={inputClass} type="number" bind:value={draft.priority} /></label>
              <label class="grid gap-1.5 text-sm" for="model-editor-eviction"><span class="font-medium">{$translate("controlPlane.modelEvictionPriority")}</span><Input id="model-editor-eviction" class={inputClass} type="number" bind:value={draft.evictionPriority} /></label>
            </div>
            <PricingCatalogPanel
              snapshot={snapshot}
              targetModel={mode === "create" ? draft.id : selectedModel}
              selectedProvider={draft.pricingProvider}
              selectedPricingModel={draft.pricingModel}
              onMappingChange={(provider, model) => { draft.pricingProvider = provider; draft.pricingModel = model; }}
              onSnapshot={(next) => { preserveDraftOnSnapshot = true; onSnapshot(next); }}
            />
          </Tabs.Content>

          <Tabs.Content value="advanced" class="mt-0 grid gap-5">
            <div><Button type="button" variant="ghost" size="sm" onclick={() => (activeTab = "overview")}>{$translate("settingsCenter.model.backToOverview")}</Button></div>
            <details class="group border-border/70 rounded-md border" open={capabilityOverrides}>
              <summary class="hover:bg-muted/30 flex min-h-11 cursor-pointer list-none items-center gap-2 px-3 text-sm font-medium transition-colors [&::-webkit-details-marker]:hidden">
                <ChevronRight class="text-muted-foreground size-4 shrink-0 transition-transform group-open:rotate-90" aria-hidden="true" />
                <span>{$translate("controlPlane.modelCapabilityOverrides")}</span>
                <span class={`ml-auto text-xs font-normal ${capabilityOverrides ? "text-warning" : "text-muted-foreground"}`}>
                  {capabilityOverrides ? $translate("controlPlane.modelCapabilityOverridden") : $translate("controlPlane.modelCapabilityAutomatic")}
                </span>
              </summary>
              <div class="border-border/70 grid gap-5 border-t p-4">
                <div class="grid gap-4 md:grid-cols-2">
                  <label class="grid gap-1.5 text-sm" for="model-editor-protocol">
                    <span class="font-medium">{$translate("controlPlane.modelProtocol")}</span>
                    <select id="model-editor-protocol" class={selectClass} bind:value={draft.backendProtocol}>
                      <option value="">{$translate("controlPlane.modelProtocolDefault")}</option>
                      <option value="native">{$translate("controlPlane.modelProtocolNative")}</option>
                      <option value="responsesToChat">{$translate("controlPlane.modelProtocolResponsesToChat")}</option>
                    </select>
                    <span class="text-muted-foreground text-xs">{$translate("controlPlane.modelProtocolHint")}</span>
                  </label>
                  <label class="grid gap-1.5 text-sm" for="model-editor-discover">
                    <span class="font-medium">{$translate("controlPlane.modelDiscover")}</span>
                    <select id="model-editor-discover" class={selectClass} bind:value={draft.backendDiscover}>
                      <option value="">{$translate("controlPlane.modelCapabilityAutomatic")}</option>
                      <option value="true">{$translate("common.yes")}</option>
                      <option value="false">{$translate("common.no")}</option>
                    </select>
                  </label>
                </div>
                <fieldset class="grid gap-2">
                  <legend class="text-sm font-medium">{$translate("controlPlane.modelAPIs")}</legend>
                  <div class="grid grid-cols-2 gap-2 sm:grid-cols-3 lg:grid-cols-5">
                    {#each MODEL_API_KEYS as api (api)}
                      <label class="border-border/70 hover:bg-muted/40 flex min-h-10 items-center gap-2 rounded-md border px-2.5 text-xs transition-colors">
                        <input type="checkbox" checked={draft.backendAPIs.includes(api)} onchange={(event) => toggleAPI(api, event.currentTarget.checked)} />
                        <span>{api}</span>
                      </label>
                    {/each}
                  </div>
                </fieldset>
                <div class="grid gap-4 md:grid-cols-2">
                  <fieldset class="grid gap-2">
                    <legend class="text-sm font-medium">{$translate("controlPlane.modelInputModalities")}</legend>
                    {#each MODEL_MODALITIES as modality (modality)}
                      <label class="flex min-h-10 items-center gap-2 text-sm"><input type="checkbox" checked={draft.capabilitiesIn.includes(modality)} onchange={(event) => toggleModality("capabilitiesIn", modality, event.currentTarget.checked)} />{modality}</label>
                    {/each}
                  </fieldset>
                  <fieldset class="grid gap-2">
                    <legend class="text-sm font-medium">{$translate("controlPlane.modelOutputModalities")}</legend>
                    {#each MODEL_MODALITIES as modality (modality)}
                      <label class="flex min-h-10 items-center gap-2 text-sm"><input type="checkbox" checked={draft.capabilitiesOut.includes(modality)} onchange={(event) => toggleModality("capabilitiesOut", modality, event.currentTarget.checked)} />{modality}</label>
                    {/each}
                  </fieldset>
                </div>
                <div class="grid gap-4 md:grid-cols-3">
                  <label class="flex min-h-10 items-center gap-2 text-sm"><input type="checkbox" bind:checked={draft.capabilitiesTools} />{$translate("controlPlane.modelTools")}</label>
                  <label class="flex min-h-10 items-center gap-2 text-sm"><input type="checkbox" bind:checked={draft.capabilitiesReranker} />{$translate("controlPlane.modelReranker")}</label>
                  <label class="grid gap-1.5 text-sm" for="model-editor-context"><span class="font-medium">{$translate("controlPlane.modelContext")}</span><Input id="model-editor-context" class={inputClass} type="number" min="0" bind:value={draft.capabilitiesContext} /></label>
                </div>
                <div class="flex justify-end">
                  <Button type="button" variant="outline" size="sm" onclick={clearCapabilityOverrides} disabled={!capabilityOverrides}>
                    <RotateCcw class="size-4" aria-hidden="true" />
                    {$translate("controlPlane.modelCapabilityReset")}
                  </Button>
                </div>
              </div>
            </details>

            <section class="grid gap-3" aria-labelledby="model-filter-title">
              <h3 id="model-filter-title" class="text-sm font-semibold">{$translate("controlPlane.modelFilters")}</h3>
              <label class="grid gap-1.5 text-sm" for="model-editor-strip-params"><span class="font-medium">{$translate("controlPlane.modelStripParams")}</span><Input id="model-editor-strip-params" class={inputClass} bind:value={draft.filtersStripParams} placeholder="temperature, top_p" /></label>
              <div class="grid gap-4 md:grid-cols-2">
                <SchemaField schema={childSchema(modelFieldSchema("filters"), "setParams")} value={draft.filtersSetParams} label={$translate("controlPlane.modelSetParams")} onChange={(next) => { if (next !== null && typeof next === "object" && !Array.isArray(next)) draft.filtersSetParams = next as Record<string, unknown>; }} />
                <SchemaField schema={childSchema(modelFieldSchema("filters"), "setParamsByID")} value={draft.filtersSetParamsByID} label={$translate("controlPlane.modelSetParamsByID")} onChange={(next) => { if (next !== null && typeof next === "object" && !Array.isArray(next)) draft.filtersSetParamsByID = next as Record<string, unknown>; }} />
              </div>
            </section>

            <section class="border-border/70 grid gap-3 border-t pt-5" aria-labelledby="model-data-title">
              <h3 id="model-data-title" class="text-sm font-semibold">{$translate("controlPlane.modelDataOverrides")}</h3>
              <div class="grid gap-4 md:grid-cols-2">
                <SchemaField schema={modelFieldSchema("macros")} value={draft.macros} label={$translate("controlPlane.modelMacros")} onChange={(next) => { if (next !== null && typeof next === "object" && !Array.isArray(next)) draft.macros = next as Record<string, unknown>; }} />
                <SchemaField schema={modelFieldSchema("metadata")} value={draft.metadata} label={$translate("controlPlane.modelMetadata")} onChange={(next) => { if (next !== null && typeof next === "object" && !Array.isArray(next)) draft.metadata = next as Record<string, unknown>; }} />
              </div>
            </section>

            <section class="border-border/70 grid gap-3 border-t pt-5" aria-labelledby="model-timeouts-title">
              <h3 id="model-timeouts-title" class="text-sm font-semibold">{$translate("controlPlane.modelTimeouts")}</h3>
              <div class="grid gap-4 sm:grid-cols-2 lg:grid-cols-3">
                <label class="grid gap-1.5 text-sm" for="model-timeout-connect"><span class="font-medium">connect</span><Input id="model-timeout-connect" class={inputClass} type="number" min="0" bind:value={draft.timeoutConnect} /></label>
                <label class="grid gap-1.5 text-sm" for="model-timeout-keepalive"><span class="font-medium">keepalive</span><Input id="model-timeout-keepalive" class={inputClass} type="number" min="0" bind:value={draft.timeoutKeepAlive} /></label>
                <label class="grid gap-1.5 text-sm" for="model-timeout-response"><span class="font-medium">responseHeader</span><Input id="model-timeout-response" class={inputClass} type="number" min="0" bind:value={draft.timeoutResponseHeader} /></label>
                <label class="grid gap-1.5 text-sm" for="model-timeout-tls"><span class="font-medium">tlsHandshake</span><Input id="model-timeout-tls" class={inputClass} type="number" min="0" bind:value={draft.timeoutTLSHandshake} /></label>
                <label class="grid gap-1.5 text-sm" for="model-timeout-expect"><span class="font-medium">expectContinue</span><Input id="model-timeout-expect" class={inputClass} type="number" min="0" bind:value={draft.timeoutExpectContinue} /></label>
                <label class="grid gap-1.5 text-sm" for="model-timeout-idle"><span class="font-medium">idleConn</span><Input id="model-timeout-idle" class={inputClass} type="number" min="0" bind:value={draft.timeoutIdleConn} /></label>
              </div>
              <label class="grid max-w-sm gap-1.5 text-sm" for="model-ignore-websockets"><span class="font-medium">{$translate("controlPlane.modelIgnoreWebsockets")}</span><select id="model-ignore-websockets" class={selectClass} bind:value={draft.ignoreWebsockets}><option value="">{$translate("controlPlane.modelInherit")}</option><option value="true">{$translate("common.yes")}</option><option value="false">{$translate("common.no")}</option></select></label>
            </section>

            {#if draft.launchMode === "backend"}
              <section class="border-border/70 grid gap-3 border-t pt-5" aria-labelledby="model-container-title">
                <h3 id="model-container-title" class="text-sm font-semibold">{$translate("controlPlane.modelContainer")}</h3>
                <SchemaField schema={childSchema(modelFieldSchema("backend"), "container")} value={draft.backendContainer} label="" onChange={(next) => { if (next !== null && typeof next === "object" && !Array.isArray(next)) draft.backendContainer = next as Record<string, unknown>; }} />
              </section>
            {/if}
          </Tabs.Content>
        </Tabs.Root>
      </fieldset>

      {#if !snapshot.writable}
        <div class="border-warning/30 bg-warning/10 text-warning mt-4 flex items-center gap-2 rounded-md border px-3 py-2 text-sm" role="status">
          <AlertTriangle class="size-4 shrink-0" aria-hidden="true" />
          {$translate("models.configNotWritable")}
        </div>
      {/if}
      {#if error}
        <div class="border-destructive/40 bg-destructive/10 mt-4 rounded-md border p-3 text-sm" role="alert">{error}</div>
      {/if}
      {#if validation && !validation.valid}
        <div class="border-destructive/40 bg-destructive/10 mt-4 rounded-md border p-3 text-xs" role="alert">
          <div class="font-medium">{$translate("common.error")}</div>
          {#if validation.issues}<ul class="mt-2 list-disc space-y-1 pl-5">{#each validation.issues as issue}<li>{issue.path}: {issue.message}</li>{/each}</ul>{/if}
        </div>
      {/if}
      <div class="border-border/70 mt-5 flex flex-wrap items-center justify-between gap-3 border-t pt-4">
        <div class="ml-auto flex flex-wrap items-center gap-2">
          <Button type="button" variant="outline" onclick={onCancel} disabled={saving}>{$translate("common.cancel")}</Button>
          <Button type="button" variant="outline" onclick={resetDraft} disabled={saving}>{$translate("settingsCenter.model.reset")}</Button>
          <Button type="submit" disabled={saving || !snapshot.writable}>
            {#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{:else}<Save class="size-4" aria-hidden="true" />{/if}
            {mode === "create" ? $translate("models.addConfig") : $translate("settingsCenter.model.saveDraft")}
          </Button>
        </div>
      </div>
    </form>
  {/if}
</section>

<Dialog.Root bind:open={diffDialogOpen}>
  <Dialog.Content class="max-h-[calc(100vh-2rem)] max-w-3xl overflow-y-auto sm:max-w-3xl" showCloseButton={!saving}>
    <Dialog.Header>
      <Dialog.Title class="flex items-center gap-2"><Check class="text-success size-4" aria-hidden="true" />{$translate("controlPlane.configDiffTitle")}</Dialog.Title>
      <Dialog.Description>{$translate("controlPlane.configDiffDescription")}</Dialog.Description>
    </Dialog.Header>
    <div class="border-border/70 bg-muted/20 flex flex-wrap items-center gap-x-4 gap-y-2 rounded-md border px-3 py-2 text-xs">
      <span class="text-muted-foreground">{$translate("controlPlane.configTarget")}</span>
      <code class="truncate">{pendingModelId}</code>
    </div>
    {#if pendingChanges.length === 0}
      <p class="text-muted-foreground rounded-md border border-dashed p-4 text-sm">{$translate("controlPlane.configNoChanges")}</p>
    {:else}
      <div class="border-border/70 divide-border/70 overflow-hidden rounded-md border">
        {#each pendingChanges as change (change.path)}
          <div class="grid gap-2 border-b p-3 last:border-b-0 sm:grid-cols-[minmax(0,1fr)_minmax(0,1.6fr)] sm:items-start">
            <div class="flex items-center gap-2 text-sm">
              <span class={`size-1.5 shrink-0 rounded-full ${change.kind === "added" ? "bg-success" : change.kind === "removed" ? "bg-destructive" : "bg-warning"}`}></span>
              <span class="font-medium">{issueLabel(change.path)}</span>
            </div>
            <div class="grid min-w-0 gap-1 text-xs sm:grid-cols-[minmax(0,1fr)_auto_minmax(0,1fr)] sm:items-center">
              {#if change.kind !== "added"}<code class="text-muted-foreground truncate" title={displayValue(change.before)}>{displayValue(change.before)}</code>{/if}
              {#if change.kind === "changed"}<span class="text-muted-foreground text-center" aria-hidden="true">→</span>{/if}
              {#if change.kind !== "removed"}<code class="truncate" title={displayValue(change.after)}>{displayValue(change.after)}</code>{/if}
            </div>
          </div>
        {/each}
      </div>
    {/if}
    {#if validation?.restartRequired}
      <p class="border-warning/30 bg-warning/10 text-warning rounded-md border px-3 py-2 text-sm" role="status"><AlertTriangle class="mr-1 inline size-4" aria-hidden="true" />{$translate("controlPlane.restartRequired")}</p>
    {/if}
    <Dialog.Footer>
      <Button variant="outline" onclick={cancelDiff} disabled={saving}>{$translate("common.cancel")}</Button>
      <Button onclick={() => void confirmSave()} disabled={saving || !pendingPatch}>
        {#if saving}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
        {$translate("controlPlane.configDiffSave")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
