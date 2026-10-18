<script lang="ts">
  import { onMount } from "svelte";
  import {
    LoaderCircle,
    Plus,
    Trash2,
    Download,
    FlaskConical,
    KeyRound,
    ChevronDown,
    ChevronRight,
    RefreshCw,
    Sparkles,
    Power,
  } from "@lucide/svelte";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Switch } from "$lib/components/ui/switch/index.js";
  import ConfirmDialog from "../ConfirmDialog.svelte";
  import { translate } from "../../lib/i18n";
  import { errorMessageFromPayload } from "../../lib/apiError";
  import { toast } from "../../lib/toast";
  import {
    applyProviderTemplate,
    cooldownPolicyOf,
    fetchConfigSnapshot,
    fetchProviderQuotas,
    distributeModels,
    fetchProvidersStatus,
    listProviderTemplates,
    normalizeModelList,
    normalizeProvider,
    probeProvider,
    refreshProviderQuotas,
    saveProvidersSection,
    setProviderDisabled,
    normalizeProvidersSection,
    type ConfigSnapshot,
    type CredentialQuota,
    type ProviderAdapterUI,
    type ProviderTemplateSummary,
    type ProviderConfigUI,
    type ProviderModelEntry,
    type ProvidersStatusResponse,
    normalizeProviderModels,
  } from "../../lib/providersApi";

  interface Props {
    onExternalChange?: () => void | Promise<void>;
  }
  let { onExternalChange = () => {} }: Props = $props();

  let providers = $state<Record<string, ProviderConfigUI>>({});
  let snapshot = $state<ConfigSnapshot | null>(null);
  let status = $state<ProvidersStatusResponse | null>(null);
  let loading = $state(true);
  let saving = $state(false);
  let error = $state("");
  let loadRequestID = 0;
  let statusTimer: ReturnType<typeof setInterval> | null = null;

  // Editor dialog state. editingID === null means "creating a new provider".
  let editorOpen = $state(false);
  let editingID = $state<string | null>(null);
  let draftID = $state("");
  let draft = $state<ProviderConfigUI>(emptyProvider());
  let probeBusy = $state(false);
  let probeError = $state("");

  let pendingDelete = $state<string | null>(null);
  // 正在切换启用状态的供应商 id。逐行记录而不是一个全局标志：一次点击只应
  // 让那一行的按钮转圈，其它行仍可操作。
  let togglingProvider = $state<string | null>(null);
  let pendingDeleteKey = $state<number | null>(null);

  // Provider templates: the picker dialog and its apply state.
  let templates = $state<ProviderTemplateSummary[]>([]);
  let templatePickerOpen = $state(false);
  let templateBusy = $state<string | null>(null);
  let templateError = $state("");

  // Subscription quota: cached snapshots keyed by credential id, plus which
  // provider rows are expanded and which refreshes are in flight.
  let quotas = $state<Record<string, CredentialQuota>>({});
  let quotaExpanded = $state<Record<string, boolean>>({});
  let quotaBusy = $state<Record<string, boolean>>({});

  function emptyAdapter(): ProviderAdapterUI {
    return { name: "", type: "openai", endpoint: "", models: [] };
  }

  function emptyProvider(): ProviderConfigUI {
    return {
      type: "openai",
      endpoint: "",
      models: [],
      adapters: [emptyAdapter()],
      sticky: true,
      exposeUpstreamName: true,
      bareNamePolicy: "local-first",
      maxRetries: 2,
      disabled: false,
      apiKeys: [{ value: "" }],
    };
  }

  function addAdapter(): void {
    draft.adapters = [...draft.adapters, emptyAdapter()];
  }

  function removeAdapter(index: number): void {
    draft.adapters = draft.adapters.filter((_, i) => i !== index);
  }

  function updateAdapter(index: number, patch: Partial<ProviderAdapterUI>): void {
    const next = [...draft.adapters];
    next[index] = { ...next[index], ...patch };
    draft.adapters = next;
  }

  function addAdapterModel(index: number): void {
    updateAdapter(index, {
      models: [...draft.adapters[index].models, { local: "", remote: "" }],
    });
  }


  function removeAdapterModel(adapterIndex: number, modelIndex: number): void {
    updateAdapter(adapterIndex, {
      models: draft.adapters[adapterIndex].models.filter((_, i) => i !== modelIndex),
    });
  }

  /** Client-facing names of one entry: primary name first, then aliases. */
  function modelNamesText(model: ProviderModelEntry): string {
    return [model.local, ...(model.aliases ?? [])].filter((name) => name !== "").join(", ");
  }

  /** Applies a comma-separated name list: first is primary, the rest aliases. */
  function onAdapterModelNames(adapterIndex: number, modelIndex: number, value: string): void {
    const names: string[] = [];
    for (const raw of value.split(",")) {
      const name = raw.trim();
      if (name !== "" && !names.includes(name)) names.push(name);
    }
    const models = [...draft.adapters[adapterIndex].models];
    const entry = { ...models[modelIndex] };
    entry.local = names[0] ?? "";
    entry.aliases = names.length > 1 ? names.slice(1) : undefined;
    // A new entry without an upstream name defaults to the primary name.
    if (entry.remote === "" || entry.remote === undefined) entry.remote = entry.local;
    models[modelIndex] = entry;
    updateAdapter(adapterIndex, { models });
  }

  function onAdapterModelRemote(adapterIndex: number, modelIndex: number, value: string): void {
    const models = [...draft.adapters[adapterIndex].models];
    models[modelIndex] = { ...models[modelIndex], remote: value };
    updateAdapter(adapterIndex, { models });
  }

  function onAdapterModelProxy(adapterIndex: number, modelIndex: number, value: string): void {
    const models = [...draft.adapters[adapterIndex].models];
    const proxy = value.trim();
    models[modelIndex] = { ...models[modelIndex], proxy: proxy === "" ? undefined : proxy };
    updateAdapter(adapterIndex, { models });
  }

  const modelKinds = ["", "vision", "image", "speech", "transcription", "translation"] as const;
  type ModelKind = (typeof modelKinds)[number];

  function modelKindOf(model: ProviderModelEntry): ModelKind {
    const caps = model.capabilities;
    if (!caps) return "";
    if (caps.translation) return "translation";
    const inList = caps.in ?? [];
    const outList = caps.out ?? [];
    if (outList.includes("image")) return "image";
    if (outList.includes("audio")) return "speech";
    if (inList.includes("audio")) return "transcription";
    if (inList.includes("image")) return "vision";
    return "";
  }

  function capabilitiesForKind(kind: ModelKind): ProviderModelEntry["capabilities"] {
    switch (kind) {
      case "vision":
        return { in: ["text", "image"] };
      case "image":
        return { in: ["text"], out: ["image"] };
      case "speech":
        return { in: ["text"], out: ["audio"] };
      case "transcription":
        return { in: ["audio"], out: ["text"] };
      case "translation":
        return { translation: true };
      default:
        return undefined;
    }
  }

  function onAdapterModelKind(adapterIndex: number, modelIndex: number, kind: ModelKind): void {
    const models = [...draft.adapters[adapterIndex].models];
    const entry = { ...models[modelIndex] };
    const capabilities = capabilitiesForKind(kind);
    if (capabilities) entry.capabilities = capabilities;
    else delete entry.capabilities;
    models[modelIndex] = entry;
    updateAdapter(adapterIndex, { models });
  }

  /** Path rewrites are edited as "from=to, from2=to2". */
  function parsePathMap(value: string): Record<string, string> | undefined {
    const out: Record<string, string> = {};
    for (const pair of value.split(",")) {
      const [from, to] = pair.split("=").map((part) => part.trim());
      if (from && to) out[from] = to;
    }
    return Object.keys(out).length > 0 ? out : undefined;
  }

  function pathMapText(adapter: ProviderAdapterUI): string {
    return Object.entries(adapter.pathMap ?? {})
      .map(([from, to]) => `${from}=${to}`)
      .join(", ");
  }

  function updateQuota(patch: NonNullable<ProviderConfigUI["quota"]>): void {
    draft.quota = { ...(draft.quota ?? {}), ...patch };
  }

  function updateQuotaEnforce(patch: NonNullable<NonNullable<ProviderConfigUI["quota"]>["enforce"]>): void {
    const quota = draft.quota ?? {};
    draft.quota = { ...quota, enforce: { ...(quota.enforce ?? {}), ...patch } };
  }

  function parsePatterns(value: string): string[] | undefined {
    const patterns = value
      .split(",")
      .map((part) => part.trim())
      .filter((part) => part !== "");
    return patterns.length > 0 ? patterns : undefined;
  }

  async function load(): Promise<void> {
    const id = ++loadRequestID;
    loading = true;
    error = "";
    try {
      const snap = await fetchConfigSnapshot();
      if (id !== loadRequestID) return;
      snapshot = snap;
      providers = normalizeProvidersSection(snap.config);
    } catch (cause) {
      if (id === loadRequestID) {
        error = cause instanceof Error ? cause.message : String(cause);
      }
    } finally {
      if (id === loadRequestID) loading = false;
    }
  }

  async function refreshStatus(): Promise<void> {
    try {
      status = await fetchProvidersStatus();
    } catch {
      status = null;
    }
  }

  async function refreshQuotas(): Promise<void> {
    try {
      const snapshots = await fetchProviderQuotas();
      const next: Record<string, CredentialQuota> = {};
      for (const snapshot of snapshots) next[snapshot.keyID] = snapshot;
      quotas = next;
    } catch {
      // Quota is auxiliary: keep the last snapshots on failure.
    }
  }

  /** Refreshes upstream quota for one provider (all keys or a single key). */
  async function refreshQuota(providerID: string, keyID?: string): Promise<void> {
    const busyKey = keyID ?? providerID;
    quotaBusy = { ...quotaBusy, [busyKey]: true };
    try {
      const snapshots = await refreshProviderQuotas(providerID, keyID);
      const next = { ...quotas };
      for (const snapshot of snapshots) next[snapshot.keyID] = snapshot;
      quotas = next;
    } catch (cause) {
      toast.error(errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause)));
    } finally {
      const rest = { ...quotaBusy };
      delete rest[busyKey];
      quotaBusy = rest;
    }
  }

  /** Credential ids of one provider, deduplicated across its routes. */
  function providerKeyIDs(providerID: string): string[] {
    const route = status?.routes.find((entry) => entry.providerID === providerID);
    if (!route) return [];
    const ids: string[] = [];
    for (const key of route.keys) {
      if (!ids.includes(key.id)) ids.push(key.id);
    }
    return ids;
  }

  function quotaBusyFor(keyID: string): boolean {
    return quotaBusy[keyID] === true;
  }

  function quotaWindows(keyID: string): CredentialQuota["windows"] {
    return quotas[keyID]?.windows ?? [];
  }

  function windowLabel(window: { id: string; label: string }): string {
    const key = `providers.quota.windows.${window.id}`;
    const translated = $translate(key);
    return translated === key || translated === "" ? window.label : translated;
  }

  function formatReset(value?: string): string {
    if (!value) return "";
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return value;
    return parsed.toLocaleString();
  }

  function formatFetched(value?: string): string {
    if (!value) return $translate("providers.quota.never");
    const parsed = new Date(value);
    if (Number.isNaN(parsed.getTime())) return value;
    return parsed.toLocaleTimeString();
  }

  // 余额式额度用标量行表达（是否有余额、金额、币种）。布尔值直接渲染成
  // "true/false" 对运维没有意义，这里换成是/否，与其它面板的措辞一致。
  function formatExtraValue(value: unknown): string {
    if (typeof value === "boolean") {
      return value ? $translate("common.yes") : $translate("common.no");
    }
    if (value === null || value === undefined) return "";
    return String(value);
  }

  onMount(() => {
    void load();
    void refreshStatus();
    void refreshQuotas();
    statusTimer = setInterval(() => void refreshStatus(), 5000);
    return () => {
      loadRequestID++;
      if (statusTimer !== null) clearInterval(statusTimer);
      statusTimer = null;
    };
  });

  async function openTemplatePicker(): Promise<void> {
    templateError = "";
    templatePickerOpen = true;
    if (templates.length > 0) return;
    try {
      templates = await listProviderTemplates();
    } catch (cause) {
      templateError = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    }
  }

  /** Creates an editor draft from a template (models already pulled). */
  async function applyTemplate(template: ProviderTemplateSummary): Promise<void> {
    templateBusy = template.id;
    templateError = "";
    try {
      const result = await applyProviderTemplate(template.id);
      const draftProvider = normalizeProvider({
        // A template provider carries no top-level endpoint: its adapters do.
        ...(result.provider as Record<string, unknown>),
        apiKeys:
          Array.isArray((result.provider as Record<string, unknown>).apiKeys) &&
          ((result.provider as Record<string, unknown>).apiKeys as unknown[]).length > 0
            ? ((result.provider as Record<string, unknown>).apiKeys as unknown[])
            : [{ value: "" }],
      });
      if (!draftProvider) {
        templateError = $translate("providers.templates.invalid");
        return;
      }
      if (!draftProvider.apiKeys.some((key) => key.value || key.valueEnv)) {
        draftProvider.apiKeys = [{ value: "" }];
      }
      editingID = null;
      draftID = template.id;
      draft = draftProvider;
      probeError = "";
      templatePickerOpen = false;
      editorOpen = true;
      for (const warning of result.warnings ?? []) {
        toast.warning(warning);
      }
      if (result.pulled > 0) {
        toast.success($translate("providers.templates.applied", { count: result.pulled }));
      }
    } catch (cause) {
      templateError = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      templateBusy = null;
    }
  }

  /** Re-pulls the model list for a template-created provider and redistributes it. */
  async function repullTemplate(providerID: string): Promise<void> {
    const cfg = providers[providerID];
    if (!cfg?.template) return;
    quotaBusy = { ...quotaBusy, [providerID]: true };
    try {
      const result = await applyProviderTemplate(cfg.template, { providerID });
      // Replace only the model lists; everything else stays as the user
      // configured it (keys, adapters, quota, session headers).
      const draftAdapters = result.provider.adapters as Array<Record<string, unknown>> | undefined;
      if (Array.isArray(draftAdapters)) {
        const next = { ...providers };
        const entry = JSON.parse(JSON.stringify(cfg)) as ProviderConfigUI;
        entry.adapters = entry.adapters.map((adapter, index) => ({
          ...adapter,
          models:
            index < draftAdapters.length
              ? normalizeModelList(draftAdapters[index].models)
              : adapter.models,
        }));
        next[providerID] = entry;
        providers = next;
        toast.success($translate("providers.templates.repulled", { count: result.pulled }));
        for (const warning of result.warnings ?? []) {
          toast.warning(warning);
        }
      }
    } catch (cause) {
      toast.error(errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause)));
    } finally {
      const rest = { ...quotaBusy };
      delete rest[providerID];
      quotaBusy = rest;
    }
  }

  function openCreate(): void {
    editingID = null;
    draftID = "";
    draft = emptyProvider();
    probeError = "";
    editorOpen = true;
  }

  function openEdit(providerID: string): void {
    const existing = providers[providerID];
    if (!existing) return;
    editingID = providerID;
    draftID = providerID;
    // Deep-copy so edits stay local until saved; secret values arrive as
    // [REDACTED] sentinels and are restored server-side when untouched.
    draft = JSON.parse(JSON.stringify(existing)) as ProviderConfigUI;
    probeError = "";
    editorOpen = true;
  }

  async function save(): Promise<void> {
    if (!snapshot || saving) return;
    const trimmedID = draftID.trim();
    if (trimmedID === "") {
      probeError = $translate("providers.errors.nameRequired");
      return;
    }
    if (draft.adapters.length === 0 || draft.adapters.some((a) => a.endpoint.trim() === "")) {
      probeError = $translate("providers.errors.endpointRequired");
      return;
    }
    if (draft.apiKeys.length === 0) {
      probeError = $translate("providers.errors.keysRequired");
      return;
    }
    // Manual model rows are normalized before both the check and the save: an
    // untouched row (or one where only the upstream name was typed) would
    // otherwise reach the server as an empty local name and reject the whole
    // patch.
    const prepared = {
      ...draft,
      adapters: draft.adapters.map((adapter) => ({
        ...adapter,
        models: normalizeProviderModels(adapter.models),
      })),
    };
    if (prepared.adapters.every((a) => a.models.length === 0)) {
      probeError = $translate("providers.errors.modelsRequired");
      return;
    }

    const next = { ...providers };
    if (editingID !== null && editingID !== trimmedID) delete next[editingID];
    next[trimmedID] = $state.snapshot(prepared);

    saving = true;
    try {
      const saved = await saveProvidersSection(snapshot.etag, next);
      snapshot = saved;
      providers = normalizeProvidersSection(saved.config);
      editorOpen = false;
      toast.success($translate("providers.toast.saved", { id: trimmedID }));
      void onExternalChange();
      void refreshStatus();
    } catch (cause) {
      probeError = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      saving = false;
    }
  }

  async function confirmDelete(): Promise<void> {
    if (!snapshot || pendingDelete === null) return;
    const target = pendingDelete;
    pendingDelete = null;
    const next = { ...providers };
    delete next[target];
    saving = true;
    try {
      const saved = await saveProvidersSection(snapshot.etag, next);
      snapshot = saved;
      providers = normalizeProvidersSection(saved.config);
      toast.success($translate("providers.toast.deleted", { id: target }));
      void onExternalChange();
      void refreshStatus();
    } catch (cause) {
      error = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      saving = false;
    }
  }

  /**
   * 快捷启用/停用。只发一个 disabled 成员的最小 patch，因此不会触碰密钥字段，
   * 也就没有"脱敏密钥按位置还原"的风险；配置保存后立即热重载，无需进编辑弹窗。
   */
  async function toggleProvider(id: string): Promise<void> {
    if (!snapshot || togglingProvider !== null) return;
    const current = providers[id];
    if (!current) return;
    togglingProvider = id;
    error = "";
    try {
      // 原始快照里是否真的有 disabled 键，决定启用时用 remove 还是什么都不做。
      const rawProviders = (snapshot.config as Record<string, unknown> | null)?.["providers"];
      const rawEntry =
        rawProviders && typeof rawProviders === "object"
          ? (rawProviders as Record<string, unknown>)[id]
          : undefined;
      const keyPresent = Boolean(rawEntry && typeof rawEntry === "object" && "disabled" in (rawEntry as object));
      const saved = await setProviderDisabled(snapshot.etag, id, !current.disabled, keyPresent);
      snapshot = saved;
      providers = normalizeProvidersSection(saved.config);
      toast.success(
        $translate(current.disabled ? "providers.toast.enabled" : "providers.toast.disabled", { id }),
      );
      void onExternalChange();
      void refreshStatus();
    } catch (cause) {
      error = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      togglingProvider = null;
    }
  }

  function addKey(): void {
    draft.apiKeys = [...draft.apiKeys, { value: "" }];
  }

  function removeKey(index: number): void {
    pendingDeleteKey = index;
  }

  function confirmDeleteKey(): void {
    if (pendingDeleteKey === null) return;
    draft.apiKeys = draft.apiKeys.filter((_, i) => i !== pendingDeleteKey);
    pendingDeleteKey = null;
  }

  function draftKey(): string | undefined {
    return (
      draft.apiKeys.find((k) => k.value && k.value !== "[REDACTED]")?.value ??
      draft.apiKeys.find((k) => k.valueEnv)?.valueEnv
    );
  }

  /** Pulls the upstream model list for one adapter (no save needed). */
  async function pullModels(adapterIndex: number): Promise<void> {
    const adapter = draft.adapters[adapterIndex];
    if (!adapter || adapter.endpoint.trim() === "") {
      probeError = $translate("providers.errors.endpointRequired");
      return;
    }
    probeBusy = true;
    probeError = "";
    try {
      const result = await probeProvider({
        type: adapter.type,
        endpoint: adapter.endpoint,
        proxy: draft.proxy,
        apiKey: draftKey(),
      });
      if (result.models.length === 0) {
        toast.warning($translate("providers.toast.noModels"));
        return;
      }
      // A model already served by another adapter of this provider stays there:
      // adding it here too would make its client-facing name ambiguous.
      const servedElsewhere = new Set<string>();
      draft.adapters.forEach((other, index) => {
        if (index === adapterIndex) return;
        for (const model of other.models) {
          servedElsewhere.add(model.local);
          for (const alias of model.aliases ?? []) servedElsewhere.add(alias);
        }
      });
      // Merge pulled models with existing entries: keep mappings, add new.
      const existing = new Map(adapter.models.map((m) => [m.remote, m]));
      let added = 0;
      let skipped = 0;
      for (const remote of result.models) {
        if (servedElsewhere.has(remote)) {
          skipped += 1;
          continue;
        }
        if (!existing.has(remote)) {
          existing.set(remote, { local: remote, remote });
          added += 1;
        }
      }
      updateAdapter(adapterIndex, {
        models: [...existing.values()].sort((a, b) => a.local.localeCompare(b.local)),
      });
      if (added > 0) {
        toast.success($translate("providers.toast.pulled", { count: added }));
      }
      if (skipped > 0) {
        toast.warning($translate("providers.toast.pullSkipped", { count: skipped }));
      }
    } catch (cause) {
      probeError = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      probeBusy = false;
    }
  }

  /**
   * Pulls the upstream model list once and distributes it across the adapters:
   * existing assignments stay put, new models follow the adapter modelMatch
   * rules (default adapter as the catch-all). No manual bookkeeping.
   */
  async function pullAndDistribute(): Promise<void> {
    const sourceIndex = draft.adapters.findIndex((adapter) => adapter.default === true);
    const source = draft.adapters[sourceIndex >= 0 ? sourceIndex : 0];
    if (!source || source.endpoint.trim() === "") {
      probeError = $translate("providers.errors.endpointRequired");
      return;
    }
    probeBusy = true;
    probeError = "";
    try {
      const result = await probeProvider({
        type: source.type,
        endpoint: source.endpoint,
        proxy: draft.proxy,
        apiKey: draftKey(),
      });
      if (result.models.length === 0) {
        toast.warning($translate("providers.toast.noModels"));
        return;
      }
      const distributed = distributeModels(draft.adapters, result.models);
      draft.adapters = distributed.adapters;
      if (distributed.added > 0) {
        toast.success($translate("providers.toast.distributed", { count: distributed.added }));
      }
      if (distributed.skipped > 0) {
        toast.warning($translate("providers.toast.pullSkipped", { count: distributed.skipped }));
      }
    } catch (cause) {
      probeError = errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause));
    } finally {
      probeBusy = false;
    }
  }

  /** Connectivity test for one adapter with the draft's first credential. */
  async function testDraft(adapterIndex: number): Promise<void> {
    const adapter = draft.adapters[adapterIndex];
    if (!adapter) return;
    probeBusy = true;
    probeError = "";
    try {
      const result = await probeProvider({
        type: adapter.type,
        endpoint: adapter.endpoint,
        proxy: draft.proxy,
        apiKey: draftKey() ?? "",
      });
      toast.success($translate("providers.toast.testOk", { count: result.models.length }));
    } catch (cause) {
      toast.error(errorMessageFromPayload(cause, cause instanceof Error ? cause.message : String(cause)));
    } finally {
      probeBusy = false;
    }
  }

  /** Runtime summary for one provider from the live status feed. */
  function keySummary(providerID: string): { ok: number; total: number; cooling: number } {
    const route = status?.routes.find((r) => r.providerID === providerID);
    if (!route) return { ok: 0, total: 0, cooling: 0 };
    const now = Date.now();
    let ok = 0;
    let cooling = 0;
    for (const key of route.keys) {
      const coolingNow =
        (key.authCooldownUntil && new Date(key.authCooldownUntil).getTime() > now) ||
        (key.rateLimitCooldownUntil && new Date(key.rateLimitCooldownUntil).getTime() > now) ||
        (key.transientCooldownUntil && new Date(key.transientCooldownUntil).getTime() > now);
      if (key.disabled || key.unresolvedEnv) continue;
      if (coolingNow) {
        cooling++;
      } else {
        ok++;
      }
    }
    return { ok, total: route.keys.length, cooling };
  }

  const providerIDs = $derived(Object.keys(providers).sort((a, b) => a.localeCompare(b)));
  const knownQuotaPresets = $derived(status?.quotaPresets ?? []);
</script>

<div class="flex flex-col gap-4">
  <div class="flex items-center justify-between">
    <div>
      <h2 class="text-lg font-semibold">{$translate("providers.title")}</h2>
      <p class="text-sm text-muted-foreground">{$translate("providers.subtitle")}</p>
    </div>
    <div class="flex items-center gap-2">
      <Button variant="outline" onclick={() => void openTemplatePicker()}>
        <Sparkles aria-hidden="true" />
        {$translate("providers.actions.addFromTemplate")}
      </Button>
      <Button onclick={openCreate}>
        <Plus aria-hidden="true" />
        {$translate("providers.actions.add")}
      </Button>
    </div>
  </div>

  {#if error}
    <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
      {error}
    </div>
  {/if}

  {#if loading}
    <div class="flex items-center gap-2 py-8 text-sm text-muted-foreground" role="status" aria-live="polite">
      <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
      {$translate("controlPlane.loading")}
    </div>
  {:else if providerIDs.length === 0}
    <div class="rounded-md border border-dashed p-8 text-center text-sm text-muted-foreground">
      {$translate("providers.empty")}
    </div>
  {:else}
    <div class="flex flex-col gap-2">
      {#each providerIDs as id (id)}
        {@const summary = keySummary(id)}
        {@const cfg = providers[id]}
        <div class="flex items-center gap-4 rounded-lg border p-4">
          <div class="min-w-0 flex-1">
            <div class="flex items-center gap-2">
              <button
                class="text-sm font-semibold hover:underline"
                onclick={() => openEdit(id)}
              >{id}</button>
              <span class="rounded bg-muted px-1.5 py-0.5 text-xs uppercase">
                {cfg.adapters.length > 1
                  ? `${cfg.adapters.length} × ${$translate("providers.fields.adapters")}`
                  : cfg.adapters[0]?.type ?? cfg.type}
              </span>
              {#if cfg.template}
                <span class="rounded bg-muted px-1.5 py-0.5 text-xs">{cfg.template}</span>
              {/if}
              {#if cfg.disabled}
                <span class="rounded bg-destructive/20 px-1.5 py-0.5 text-xs text-destructive">
                  {$translate("providers.status.disabled")}
                </span>
              {/if}
            </div>
            <p class="truncate text-xs text-muted-foreground">
              {cfg.adapters.map((adapter) => adapter.endpoint).join(" · ")}
            </p>
          </div>
          <div class="flex items-center gap-3 text-xs text-muted-foreground">
            <span class="inline-flex items-center gap-1" title={$translate("providers.fields.apiKeys")}>
              <KeyRound class="size-3.5" aria-hidden="true" />
              {#if summary.total > 0}
                {summary.ok}/{summary.total}{#if summary.cooling > 0}&nbsp;({summary.cooling} {$translate("providers.status.coolingShort")}){/if}
              {:else}
                {cfg.apiKeys.length}
              {/if}
            </span>
            <span>
              {$translate("providers.status.models")}:
              {cfg.adapters.reduce((total, adapter) => total + adapter.models.length, 0)}
            </span>
          </div>
          <div class="flex items-center gap-1">
            {#if cfg.template}
              <Button
                variant="ghost"
                size="sm"
                onclick={() => void repullTemplate(id)}
                disabled={quotaBusyFor(id)}
                title={$translate("providers.templates.repull")}
              >
                {#if quotaBusyFor(id)}
                  <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
                {:else}
                  <Download class="size-3.5" aria-hidden="true" />
                {/if}
                {$translate("providers.templates.repull")}
              </Button>
            {/if}
            {#if cfg.quota}
              <Button
                variant="ghost"
                size="sm"
                onclick={() => (quotaExpanded = { ...quotaExpanded, [id]: !quotaExpanded[id] })}
                aria-expanded={quotaExpanded[id] === true}
              >
                {#if quotaExpanded[id]}
                  <ChevronDown class="size-4" aria-hidden="true" />
                {:else}
                  <ChevronRight class="size-4" aria-hidden="true" />
                {/if}
                {$translate("providers.quota.title")}
              </Button>
            {/if}
            <Button
              variant="ghost"
              size="sm"
              onclick={() => void toggleProvider(id)}
              disabled={togglingProvider !== null}
              title={cfg.disabled ? $translate("providers.actions.enable") : $translate("providers.actions.disable")}
              aria-label={cfg.disabled ? $translate("providers.actions.enable") : $translate("providers.actions.disable")}
            >
              {#if togglingProvider === id}
                <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
              {:else}
                <Power
                  class={cfg.disabled ? "size-4 text-muted-foreground" : "size-4 text-emerald-600"}
                  aria-hidden="true"
                />
              {/if}
            </Button>
            <Button variant="ghost" size="sm" onclick={() => openEdit(id)}>
              {$translate("common.edit")}
            </Button>
            <Button variant="ghost" size="sm" onclick={() => (pendingDelete = id)} aria-label={$translate("common.delete")}>
              <Trash2 class="size-4 text-destructive" aria-hidden="true" />
            </Button>
          </div>
        </div>
        {#if cfg.quota && quotaExpanded[id]}
          <div class="ml-4 border-l pl-4">
            <div class="mb-2 flex items-center justify-between gap-3">
              <span class="min-w-0 flex-1 text-xs text-muted-foreground">
                {$translate("providers.quota.subtitle")}
              </span>
              <Button
                variant="outline"
                size="sm"
                class="shrink-0"
                onclick={() => void refreshQuota(id)}
                disabled={quotaBusyFor(id)}
              >
                {#if quotaBusyFor(id)}
                  <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
                {:else}
                  <RefreshCw class="size-3.5" aria-hidden="true" />
                {/if}
                {$translate("providers.quota.refreshAll")}
              </Button>
            </div>
            {#if providerKeyIDs(id).length === 0}
              <p class="text-xs text-muted-foreground">{$translate("providers.quota.noCredentials")}</p>
            {:else}
              <div class="grid grid-cols-1 gap-3 md:grid-cols-2 xl:grid-cols-3">
                {#each providerKeyIDs(id) as keyID (keyID)}
                  {@const snapshot = quotas[keyID]}
                  <div class="rounded-lg border p-3">
                    <div class="flex items-center justify-between gap-2">
                      <span class="truncate text-xs font-medium" title={keyID}>{keyID}</span>
                      {#if snapshot?.enforced}
                        <span class="rounded bg-destructive/15 px-1.5 py-0.5 text-xs text-destructive">
                          {$translate("providers.quota.enforced")}
                        </span>
                      {/if}
                    </div>
                    {#if snapshot?.error}
                      <p class="mt-1 text-xs text-destructive">{snapshot.error}</p>
                    {/if}
                    <div class="mt-2 flex flex-col gap-2">
                      {#each quotaWindows(keyID) as window (window.id)}
                        <div class="flex flex-col gap-1">
                          <div class="flex items-baseline justify-between text-xs">
                            <span>{windowLabel(window)}</span>
                            <span class="text-muted-foreground">
                              {#if window.status === "missing"}
                                {$translate("providers.quota.missing")}
                              {:else}
                                {$translate("providers.quota.remaining", { percent: window.remainingPercent })}
                              {/if}
                            </span>
                          </div>
                          <div class="h-1.5 overflow-hidden rounded bg-muted">
                            <div
                              class="h-full rounded {window.percent >= 100 ? 'bg-destructive' : window.percent >= 80 ? 'bg-amber-500' : 'bg-emerald-500'}"
                              style={`width: ${Math.min(100, Math.max(0, window.percent))}%`}
                            ></div>
                          </div>
                          {#if window.status || window.resetsAt}
                            <div class="flex items-baseline justify-between text-xs text-muted-foreground">
                              <span>{window.status ?? ""}</span>
                              <span>
                                {#if window.resetsAt}
                                  {$translate("providers.quota.resets")} {formatReset(window.resetsAt)}
                                {/if}
                              </span>
                            </div>
                          {/if}
                        </div>
                      {/each}
                      {#if quotaWindows(keyID)?.length === 0 && (snapshot?.extras?.length ?? 0) === 0 && !snapshot?.error && !snapshot?.fetchedAt}
                        <!-- 只有 extras 的映射（余额类）没有窗口，但已经取到数据时
                             不能再报"尚未刷新"：那行提示只说这份快照里什么都没有。 -->
                        <p class="text-xs text-muted-foreground">{$translate("providers.quota.never")}</p>
                      {/if}
                      {#each snapshot?.extras ?? [] as extra (extra.id)}
                        <div class="flex items-baseline justify-between text-xs">
                          <span>{extra.label}</span>
                          <span class="text-muted-foreground">{formatExtraValue(extra.value)}</span>
                        </div>
                      {/each}
                    </div>
                    <div class="mt-2 flex items-center justify-between">
                      <span class="text-xs text-muted-foreground">
                        {$translate("providers.quota.updated")} {formatFetched(snapshot?.fetchedAt)}
                      </span>
                      <Button
                        variant="ghost"
                        size="sm"
                        onclick={() => void refreshQuota(id, keyID)}
                        disabled={quotaBusyFor(keyID)}
                        aria-label={$translate("providers.quota.refresh")}
                      >
                        {#if quotaBusyFor(keyID)}
                          <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
                        {:else}
                          <RefreshCw class="size-3.5" aria-hidden="true" />
                        {/if}
                        {$translate("providers.quota.refresh")}
                      </Button>
                    </div>
                  </div>
                {/each}
              </div>
            {/if}
          </div>
        {/if}
      {/each}
    </div>
  {/if}
</div>

<Dialog.Root bind:open={templatePickerOpen}>
  <Dialog.Content class="max-h-[calc(100dvh-1.5rem)] max-w-2xl overflow-y-auto sm:max-w-2xl">
    <Dialog.Header>
      <Dialog.Title>{$translate("providers.templates.title")}</Dialog.Title>
    </Dialog.Header>
    <p class="text-sm text-muted-foreground">{$translate("providers.templates.subtitle")}</p>
    {#if templateError}
      <div class="mt-3 rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">
        {templateError}
      </div>
    {/if}
    <div class="mt-3 flex flex-col gap-2">
      {#each templates as template (template.id)}
        <div class="flex items-start justify-between gap-3 rounded-lg border p-3">
          <div class="min-w-0">
            <div class="flex items-center gap-2">
              <span class="text-sm font-semibold">{template.name}</span>
              {#each template.adapters as adapter (adapter.name ?? adapter.type)}
                <span class="rounded bg-muted px-1.5 py-0.5 text-xs uppercase">{adapter.type}</span>
              {/each}
            </div>
            <p class="mt-1 text-xs text-muted-foreground">{template.description}</p>
          </div>
          <Button
            size="sm"
            onclick={() => void applyTemplate(template)}
            disabled={templateBusy !== null}
          >
            {#if templateBusy === template.id}
              <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
            {/if}
            {$translate("providers.templates.use")}
          </Button>
        </div>
      {/each}
      {#if templates.length === 0 && templateError === ""}
        <div class="flex items-center gap-2 py-4 text-sm text-muted-foreground" role="status">
          <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
          {$translate("controlPlane.loading")}
        </div>
      {/if}
    </div>
    <Dialog.Footer>
      <Button variant="outline" onclick={() => (templatePickerOpen = false)}>
        {$translate("common.cancel")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

<Dialog.Root bind:open={editorOpen}>
  <Dialog.Content class="max-h-[calc(100dvh-1.5rem)] max-w-3xl overflow-y-auto sm:max-w-3xl">
      <Dialog.Header>
        <Dialog.Title>
          {editingID === null ? $translate("providers.editor.createTitle") : $translate("providers.editor.editTitle", { id: editingID })}
        </Dialog.Title>
      </Dialog.Header>

      <div class="flex flex-col gap-4">
        <div class="grid grid-cols-1 gap-4 sm:grid-cols-2">
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.name")}</span>
            <Input bind:value={draftID} disabled={editingID !== null} placeholder="deepseek" />
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.modelPrefix")}</span>
            <Input
              value={draft.modelPrefix ?? ""}
              placeholder={$translate("providers.fields.modelPrefixPlaceholder")}
              onchange={(e) => (draft.modelPrefix = e.currentTarget.value.trim() || undefined)}
            />
            <span class="text-xs text-muted-foreground">{$translate("providers.fields.modelPrefixHint")}</span>
          </label>
        </div>

        <div class="rounded-lg border p-3">
          <!-- min-w-0/flex-1 + shrink-0: the hint wraps inside its own column
               instead of running under the buttons. -->
          <div class="mb-2 flex items-center justify-between gap-3">
            <div class="min-w-0 flex-1">
              <h3 class="text-sm font-semibold">{$translate("providers.fields.adapters")}</h3>
              <p class="text-xs text-muted-foreground">{$translate("providers.fields.adaptersHint")}</p>
            </div>
            <div class="flex shrink-0 gap-2">
              <Button variant="outline" size="sm" onclick={() => void pullAndDistribute()} disabled={probeBusy}>
                {#if probeBusy}
                  <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
                {:else}
                  <Download class="size-3.5" aria-hidden="true" />
                {/if}
                {$translate("providers.actions.pullDistribute")}
              </Button>
              <Button variant="outline" size="sm" onclick={addAdapter}>
                <Plus class="size-3.5" aria-hidden="true" />
                {$translate("providers.actions.addAdapter")}
              </Button>
            </div>
          </div>
          <div class="flex flex-col gap-3">
            {#each draft.adapters as adapter, ai (ai)}
              <div class="rounded-lg border bg-muted/20 p-3">
                <div class="grid grid-cols-1 items-end gap-2 sm:grid-cols-[1fr_1fr_2fr_auto]">
                  <label class="flex flex-col gap-1">
                    <span class="text-xs font-medium text-muted-foreground">{$translate("providers.fields.adapterName")}</span>
                    <Input
                      value={adapter.name ?? ""}
                      placeholder={$translate("providers.fields.adapterNamePlaceholder")}
                      onchange={(e) => updateAdapter(ai, { name: e.currentTarget.value.trim() })}
                    />
                  </label>
                  <label class="flex flex-col gap-1">
                    <span class="text-xs font-medium text-muted-foreground">{$translate("providers.fields.type")}</span>
                    <select
                      class="h-9 rounded-md border bg-transparent px-3 text-sm"
                      value={adapter.type}
                      onchange={(e) =>
                        updateAdapter(ai, { type: e.currentTarget.value as ProviderAdapterUI["type"] })}
                    >
                      <option value="openai">{$translate("providers.fields.typeOpenAI")}</option>
                      <option value="openai-responses">{$translate("providers.fields.typeOpenAIResponses")}</option>
                      <option value="anthropic">{$translate("providers.fields.typeAnthropic")}</option>
                      <option value="gemini">{$translate("providers.fields.typeGemini")}</option>
                      <option value="gemini-interactions"
                        >{$translate("providers.fields.typeGeminiInteractions")}</option
                      >
                    </select>
                  </label>
                  <label class="flex flex-col gap-1">
                    <span class="text-xs font-medium text-muted-foreground">{$translate("providers.fields.endpoint")}</span>
                    <Input
                      value={adapter.endpoint}
                      placeholder={adapter.type === "gemini" ||
                      adapter.type === "gemini-interactions"
                        ? "https://generativelanguage.googleapis.com"
                        : "https://api.deepseek.com"}
                      onchange={(e) => updateAdapter(ai, { endpoint: e.currentTarget.value.trim() })}
                    />
                  </label>
                  <Button
                    variant="ghost"
                    size="sm"
                    onclick={() => removeAdapter(ai)}
                    disabled={draft.adapters.length <= 1}
                    aria-label={$translate("common.delete")}
                  >
                    <Trash2 class="size-4 text-destructive" aria-hidden="true" />
                  </Button>
                </div>

                <div class="mt-3 flex items-center justify-between">
                  <span class="text-xs font-medium text-muted-foreground">
                    {$translate("providers.fields.models")} ({adapter.models.length})
                  </span>
                  <div class="flex gap-2">
                    <Button variant="outline" size="sm" onclick={() => void testDraft(ai)} disabled={probeBusy}>
                      <FlaskConical class="size-3.5" aria-hidden="true" />
                      {$translate("providers.actions.test")}
                    </Button>
                    <Button variant="outline" size="sm" onclick={() => void pullModels(ai)} disabled={probeBusy}>
                      {#if probeBusy}
                        <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
                      {:else}
                        <Download class="size-3.5" aria-hidden="true" />
                      {/if}
                      {$translate("providers.actions.pull")}
                    </Button>
                    <Button variant="outline" size="sm" onclick={() => addAdapterModel(ai)}>
                      <Plus class="size-3.5" aria-hidden="true" />
                      {$translate("providers.actions.addModel")}
                    </Button>
                  </div>
                </div>

                <div class="mt-2 flex flex-col">
                  {#if adapter.models.length > 0}
                    <div
                      class="grid grid-cols-1 gap-2 border-b pb-1 text-xs font-medium text-muted-foreground sm:grid-cols-[1.1fr_1.4fr_1fr_1fr_auto]"
                    >
                      <span>{$translate("providers.fields.modelRemote")}</span>
                      <span>{$translate("providers.fields.modelAliases")}</span>
                      <span>{$translate("providers.fields.modelKind")}</span>
                      <span>{$translate("providers.fields.modelProxy")}</span>
                      <span class="w-8"></span>
                    </div>
                  {/if}
                  {#each adapter.models as model, mi (mi)}
                    <div class="grid grid-cols-1 items-center gap-2 py-1 sm:grid-cols-[1.1fr_1.4fr_1fr_1fr_auto]">
                      <Input
                        placeholder={$translate("providers.fields.modelRemote")}
                        value={model.remote}
                        onchange={(e) => onAdapterModelRemote(ai, mi, e.currentTarget.value)}
                      />
                      <Input
                        placeholder={$translate("providers.fields.modelAliasesPlaceholder")}
                        value={modelNamesText(model)}
                        onchange={(e) => onAdapterModelNames(ai, mi, e.currentTarget.value)}
                      />
                      <select
                        class="border-input bg-background focus-visible:border-ring focus-visible:ring-ring/50 h-9 w-full rounded-lg border px-2 text-sm outline-none focus-visible:ring-3"
                        aria-label={$translate("providers.fields.modelKind")}
                        value={modelKindOf(model)}
                        onchange={(e) => onAdapterModelKind(ai, mi, e.currentTarget.value as ModelKind)}
                      >
                        {#each modelKinds as kind (kind)}
                          <option value={kind}>
                            {$translate(kind === "" ? "providers.modelKind.chat" : `providers.modelKind.${kind}`)}
                          </option>
                        {/each}
                      </select>
                      <Input
                        placeholder={$translate("providers.fields.modelProxy")}
                        value={model.proxy ?? ""}
                        onchange={(e) => onAdapterModelProxy(ai, mi, e.currentTarget.value)}
                      />
                      <Button
                        variant="ghost"
                        size="sm"
                        onclick={() => removeAdapterModel(ai, mi)}
                        aria-label={$translate("common.delete")}
                      >
                        <Trash2 class="size-4 text-destructive" aria-hidden="true" />
                      </Button>
                    </div>
                  {/each}
                  {#if adapter.models.length === 0}
                    <p class="text-xs text-muted-foreground">{$translate("providers.editor.noModels")}</p>
                  {/if}
                </div>

                <details class="mt-2">
                  <summary class="cursor-pointer text-xs text-muted-foreground">
                    {$translate("providers.fields.adapterAdvanced")}
                  </summary>
                  <div class="mt-2 flex flex-col gap-2">
                    <label class="flex flex-col gap-1">
                      <span class="text-xs text-muted-foreground">{$translate("providers.fields.adapterProxy")}</span>
                      <Input
                        value={adapter.proxy ?? ""}
                        placeholder="socks5://127.0.0.1:1080"
                        onchange={(e) => updateAdapter(ai, { proxy: e.currentTarget.value.trim() || undefined })}
                      />
                      <span class="text-xs text-muted-foreground">{$translate("providers.fields.proxyHint")}</span>
                    </label>
                    <label class="flex flex-col gap-1">
                      <span class="text-xs text-muted-foreground">{$translate("providers.fields.pathMap")}</span>
                      <Input
                        value={pathMapText(adapter)}
                        placeholder="/v1beta/models/=/v1/models/"
                        onchange={(e) => updateAdapter(ai, { pathMap: parsePathMap(e.currentTarget.value) })}
                      />
                      <span class="text-xs text-muted-foreground">{$translate("providers.fields.pathMapHint")}</span>
                    </label>
                    <label class="flex flex-col gap-1">
                      <span class="text-xs text-muted-foreground">{$translate("providers.fields.modelMatch")}</span>
                      <Input
                        value={(adapter.modelMatch ?? []).join(", ")}
                        placeholder="grok-*, gpt-*"
                        onchange={(e) => updateAdapter(ai, { modelMatch: parsePatterns(e.currentTarget.value) })}
                      />
                      <span class="text-xs text-muted-foreground">{$translate("providers.fields.modelMatchHint")}</span>
                    </label>
                    <label class="flex items-center gap-2 text-xs">
                      <Switch
                        checked={adapter.default === true}
                        onCheckedChange={(checked) => updateAdapter(ai, { default: checked })}
                      />
                      {$translate("providers.fields.adapterDefault")}
                    </label>
                  </div>
                </details>
              </div>
            {/each}
          </div>
        </div>

        <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.proxy")}</span>
            <Input bind:value={draft.proxy} placeholder="http://..." />
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.priority")}</span>
            <Input type="number" min="0" bind:value={draft.priority} />
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.weight")}</span>
            <Input type="number" min="0" bind:value={draft.weight} />
          </label>
        </div>

        <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.bareNamePolicy")}</span>
            <select class="h-9 rounded-md border bg-transparent px-3 text-sm" bind:value={draft.bareNamePolicy}>
              <option value="local-first">{$translate("providers.fields.localFirst")}</option>
              <option value="provider-first">{$translate("providers.fields.providerFirst")}</option>
            </select>
          </label>
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.maxRetries")}</span>
            <Input type="number" min="0" bind:value={draft.maxRetries} />
          </label>
        </div>

        <div class="grid grid-cols-1 gap-4 sm:grid-cols-3">
          <label class="flex flex-col gap-1.5">
            <span class="text-sm font-medium">{$translate("providers.fields.cooldownPolicy")}</span>
            <select
              class="h-9 rounded-md border bg-transparent px-3 text-sm"
              value={cooldownPolicyOf(draft.cooldown)}
              onchange={(e) => {
                const policy = e.currentTarget.value as "inherit" | "enabled" | "disabled";
                draft.cooldown =
                  policy === "inherit"
                    ? undefined
                    : typeof draft.cooldown === "object"
                      ? { ...draft.cooldown, policy }
                      : policy;
              }}
            >
              <option value="inherit">{$translate("providers.fields.cooldownInherit")}</option>
              <option value="enabled">{$translate("providers.fields.cooldownEnabled")}</option>
              <option value="disabled">{$translate("providers.fields.cooldownDisabled")}</option>
            </select>
            <span class="text-xs text-muted-foreground">{$translate("providers.fields.cooldownHint")}</span>
          </label>
          <!-- flex-wrap + nowrap: three switches share one narrow column, and
               without it each label wraps to one character per line. -->
          <div class="flex flex-wrap items-end gap-x-6 gap-y-2 pb-1.5">
            <label class="flex items-center gap-2 text-sm whitespace-nowrap">
              <Switch bind:checked={draft.sticky} />
              {$translate("providers.fields.sticky")}
            </label>
            <label class="flex items-center gap-2 text-sm whitespace-nowrap" title={$translate("providers.fields.exposeUpstreamNameHint")}>
              <Switch bind:checked={draft.exposeUpstreamName} />
              {$translate("providers.fields.exposeUpstreamName")}
            </label>
            <label class="flex items-center gap-2 text-sm whitespace-nowrap">
              <Switch bind:checked={draft.disabled} />
              {$translate("providers.fields.disabled")}
            </label>
          </div>
        </div>

        <div class="rounded-lg border p-3">
          <h3 class="text-sm font-semibold">{$translate("providers.quota.configTitle")}</h3>
          <p class="mb-2 text-xs text-muted-foreground">{$translate("providers.quota.configHint")}</p>
          <div class="grid grid-cols-1 items-end gap-3 sm:grid-cols-3">
            <label class="flex flex-col gap-1.5">
              <span class="text-sm font-medium">{$translate("providers.quota.preset")}</span>
              <Input
                list="providers-quota-presets"
                value={draft.quota?.preset ?? ""}
                placeholder="opencode-go"
                onchange={(e) => updateQuota({ preset: e.currentTarget.value.trim() || undefined })}
              />
            </label>
            <label class="flex flex-col gap-1.5">
              <span class="text-sm font-medium">{$translate("providers.quota.refreshInterval")}</span>
              <Input
                value={draft.quota?.refreshInterval ?? ""}
                placeholder={$translate("providers.quota.refreshIntervalPlaceholder")}
                onchange={(e) => updateQuota({ refreshInterval: e.currentTarget.value.trim() || undefined })}
              />
            </label>
            <div class="flex items-center gap-4 pb-1.5">
              <label class="flex items-center gap-2 text-sm">
                <Switch
                  checked={draft.quota?.enforce?.enabled === true}
                  onCheckedChange={(checked) => updateQuotaEnforce({ enabled: checked })}
                />
                {$translate("providers.quota.enforce")}
              </label>
              {#if draft.quota?.enforce?.enabled}
                <label class="flex items-center gap-2 text-sm">
                  <span>{$translate("providers.quota.threshold")}</span>
                  <Input
                    type="number"
                    min="1"
                    max="100"
                    class="w-20"
                    value={draft.quota?.enforce?.threshold ?? 100}
                    onchange={(e) =>
                      updateQuotaEnforce({ threshold: e.currentTarget.valueAsNumber || 100 })}
                  />
                </label>
              {/if}
            </div>
          </div>
          {#if knownQuotaPresets.length > 0}
            <datalist id="providers-quota-presets">
              {#each knownQuotaPresets as presetID (presetID)}
                <option value={presetID}></option>
              {/each}
            </datalist>
          {/if}
        </div>

        <label class="flex flex-col gap-1.5">
          <span class="text-sm font-medium">{$translate("providers.fields.sessionHeaders")}</span>
          <Input
            placeholder="X-Session-Id, X-Litellm-Session-Id"
            value={(draft.sessionHeaders ?? []).join(", ")}
            onchange={(e) => {
              draft.sessionHeaders = e.currentTarget.value
                .split(",")
                .map((value) => value.trim())
                .filter((value) => value !== "");
            }}
          />
          <span class="text-xs text-muted-foreground">
            {$translate("providers.fields.sessionHeadersHint")}
          </span>
        </label>

        <div class="rounded-lg border p-3">
          <div class="mb-2 flex items-center justify-between gap-3">
            <h3 class="min-w-0 flex-1 text-sm font-semibold">{$translate("providers.fields.apiKeys")}</h3>
            <Button variant="outline" size="sm" class="shrink-0" onclick={addKey}>
              <Plus class="size-3.5" aria-hidden="true" />
              {$translate("providers.actions.addKey")}
            </Button>
          </div>
          <div class="flex flex-col gap-2">
            {#each draft.apiKeys as key, i}
              <div class="grid grid-cols-1 items-center gap-2 sm:grid-cols-[2fr_1fr_1fr_1fr_auto]">
                <Input
                  type="password"
                  placeholder={$translate("providers.fields.keyValueOrEnv")}
                  value={key.value !== undefined && key.value !== ""
                    ? key.value
                    : key.valueEnv
                      ? `env:${key.valueEnv}`
                      : ""}
                  onchange={(e) => {
                    const raw = e.currentTarget.value;
                    const next = [...draft.apiKeys];
                    if (raw.startsWith("env:")) {
                      next[i] = { ...key, value: undefined, valueEnv: raw.slice(4) };
                    } else {
                      next[i] = { ...key, value: raw, valueEnv: undefined };
                    }
                    draft.apiKeys = next;
                  }}
                />
                <Input
                  type="number"
                  min="0"
                  placeholder={$translate("providers.fields.weight")}
                  value={key.weight ?? ""}
                  onchange={(e) => {
                    const next = [...draft.apiKeys];
                    next[i] = { ...key, weight: e.currentTarget.valueAsNumber || 0 };
                    draft.apiKeys = next;
                  }}
                />
                <Input
                  type="number"
                  min="0"
                  placeholder={$translate("providers.fields.priority")}
                  value={key.priority ?? ""}
                  onchange={(e) => {
                    const next = [...draft.apiKeys];
                    next[i] = { ...key, priority: e.currentTarget.valueAsNumber || 0 };
                    draft.apiKeys = next;
                  }}
                />
                <Input
                  type="number"
                  min="0"
                  placeholder={$translate("providers.fields.maxConcurrent")}
                  value={key.maxConcurrent ?? ""}
                  onchange={(e) => {
                    const next = [...draft.apiKeys];
                    next[i] = { ...key, maxConcurrent: e.currentTarget.valueAsNumber || 0 };
                    draft.apiKeys = next;
                  }}
                />
                <Button variant="ghost" size="sm" onclick={() => removeKey(i)} aria-label={$translate("common.delete")}>
                  <Trash2 class="size-4 text-destructive" aria-hidden="true" />
                </Button>
              </div>
            {/each}
          </div>
        </div>

        {#if probeError}
          <div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm break-words whitespace-pre-wrap max-h-40 overflow-y-auto" role="alert">
            {probeError}
          </div>
        {/if}
      </div>

      <Dialog.Footer>
        <Button variant="outline" onclick={() => (editorOpen = false)}>
          {$translate("common.cancel")}
        </Button>
        <Button onclick={() => void save()} disabled={saving}>
          {#if saving}
            <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
          {/if}
          {$translate("common.save")}
        </Button>
      </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>

{#if pendingDelete !== null}
  <ConfirmDialog
    open={true}
    title={$translate("providers.delete.title")}
    message={$translate("providers.delete.message", { id: pendingDelete })}
    confirmLabel={$translate("common.delete")}
    onConfirm={() => void confirmDelete()}
  />
{/if}
{#if pendingDeleteKey !== null}
  <ConfirmDialog
    open={true}
    title={$translate("providers.deleteKey.title")}
    message={$translate("providers.deleteKey.message")}
    confirmLabel={$translate("common.delete")}
    onConfirm={confirmDeleteKey}
  />
{/if}
