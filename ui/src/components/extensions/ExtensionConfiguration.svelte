<script lang="ts">
  import { translate } from "../../lib/i18n";
  import { push } from "svelte-spa-router";
  import type { ExtensionDefinition } from "../../lib/extensionsApi";
  import ExtensionValueEditor from "./ExtensionValueEditor.svelte";
  import ExtensionSettingsForm from "./ExtensionSettingsForm.svelte";
  import ModelMultiSelect from "../ModelMultiSelect.svelte";
  import { models } from "../../stores/api";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import { Label } from "$lib/components/ui/label/index.js";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import * as Tabs from "$lib/components/ui/tabs/index.js";
  import {
    Info,
    Sliders,
    Layers,
    Shield,
    HardDrive,
    Network,
    Cpu,
    Clock,
    Copy,
    Check,
    Braces,
    FormInput,
    AlertCircle,
    CheckCircle2
  } from "@lucide/svelte";

  interface Props {
    draft: ExtensionDefinition;
    errors?: Record<string, string>;
    layout?: "tabs" | "sidebar";
    activeTab?: string;
    compact?: boolean;
  }

  let {
    draft = $bindable(),
    errors = {},
    layout = "tabs",
    activeTab = $bindable("basic"),
    compact = false,
  }: Props = $props();

  let copiedId = $state(false);
  let rawJsonMode = $state(false);
  let rawJsonDraft = $state("");
  let rawJsonError = $state("");
  let rawJsonValid = $state(false);

  // Parse comma or newline separated strings
  const parseList = (value: string) =>
    value
      .split(/[\n,]+/)
      .map((entry) => entry.trim())
      .filter(Boolean);

  const formatList = (arr: string[] | undefined | null) => (arr ?? []).join(", ");
  const formatLines = (arr: string[] | undefined | null) => (arr ?? []).join("\n");

  // Common endpoints helper
  const standardEndpoints = [
    { id: "/v1/chat/completions", label: "Chat (/v1/chat/completions)" },
    { id: "/v1/completions", label: "Completions (/v1/completions)" },
    { id: "/v1/embeddings", label: "Embeddings (/v1/embeddings)" },
    { id: "/v1/images/generations", label: "Images (/v1/images/generations)" },
    { id: "/v1/audio/transcriptions", label: "Audio (/v1/audio/transcriptions)" },
  ];

  function toggleEndpoint(ep: string) {
    const list = draft.manifest.match.endpoints ?? [];
    if (list.includes(ep)) {
      draft.manifest.match.endpoints = list.filter((item) => item !== ep);
    } else {
      draft.manifest.match.endpoints = [...list, ep];
    }
  }

  async function copyId() {
    if (!draft?.manifest.id) return;
    try {
      await navigator.clipboard.writeText(draft.manifest.id);
      copiedId = true;
      setTimeout(() => (copiedId = false), 2000);
    } catch {
      // fallback
    }
  }

  function initRawJson() {
    rawJsonDraft = JSON.stringify(draft.manifest.config ?? {}, null, 2);
    rawJsonError = "";
    rawJsonValid = false;
  }

  function handleRawJsonInput(val: string) {
    rawJsonDraft = val;
    try {
      const parsed = JSON.parse(val);
      if (parsed && typeof parsed === "object" && !Array.isArray(parsed)) {
        draft.manifest.config = parsed as Record<string, unknown>;
        rawJsonError = "";
        rawJsonValid = true;
      } else {
        rawJsonError = $translate("extensions.invalidConfig");
        rawJsonValid = false;
      }
    } catch (e) {
      rawJsonError = String(e);
      rawJsonValid = false;
    }
  }

  function formatRawJson() {
    try {
      const parsed = JSON.parse(rawJsonDraft);
      rawJsonDraft = JSON.stringify(parsed, null, 2);
      rawJsonError = "";
      rawJsonValid = true;
    } catch (e) {
      rawJsonError = String(e);
      rawJsonValid = false;
    }
  }

  function setTimeoutPreset(val: string) {
    draft.manifest.timeout = val;
  }
</script>

<div class="extension-config w-full" class:compact={layout === "sidebar" || compact}>
  {#if layout === "sidebar"}
    <!-- Compact sidebar mode for the code editor's inspector panel -->
    <div class="space-y-4 p-3 text-xs">
      <!-- Section 1: Basic -->
      <div class="rounded-lg border border-border/70 bg-card/60 p-3 shadow-xs">
        <div class="mb-2.5 flex items-center justify-between border-b border-border/50 pb-2">
          <span class="font-semibold text-foreground">{$translate("extensions.workspace.basic")}</span>
          <div class="flex items-center gap-1.5">
            <Switch.Root
              bind:checked={draft.manifest.enabled}
              aria-label={$translate("extensions.enabled")}
            />
            <span class="text-[11px] text-muted-foreground">
              {$translate(draft.manifest.enabled ? "extensions.workspace.enabled" : "extensions.workspace.disabled")}
            </span>
          </div>
        </div>

        <div class="space-y-2.5">
          <div class="space-y-1">
            <Label class="text-[11px] text-muted-foreground">{$translate("extensions.name")}</Label>
            <Input bind:value={draft.manifest.name} class="h-8 text-xs" />
          </div>

          <div class="space-y-1">
            <Label class="text-[11px] text-muted-foreground">{$translate("extensions.id")}</Label>
            <Input
              bind:value={draft.manifest.id}
              disabled={Boolean(draft.etag)}
              class="h-8 font-mono text-xs"
              placeholder="my-extension"
            />
          </div>

          <div class="space-y-1">
            <Label class="text-[11px] text-muted-foreground">{$translate("extensions.description")}</Label>
            <Textarea rows={2} bind:value={draft.manifest.description} class="resize-none text-xs" />
          </div>
        </div>
      </div>

      <!-- Section 2: Scope & Models -->
      <div class="rounded-lg border border-border/70 bg-card/60 p-3 shadow-xs">
        <div class="mb-2.5 flex items-center justify-between border-b border-border/50 pb-2">
          <span class="font-semibold text-foreground">{$translate("extensions.workspace.models")}</span>
          <Badge variant="outline" class="text-[10px]">
            {(draft.manifest.match.models ?? []).length === 0
              ? $translate("extensions.workspace.allModels")
              : `${(draft.manifest.match.models ?? []).length} models`}
          </Badge>
        </div>

        <div class="space-y-2.5">
          <ModelMultiSelect
            value={draft.manifest.match.models ?? []}
            options={$models.map((m) => m.id)}
            ariaLabel={$translate("extensions.models")}
            allLabel={$translate("extensions.workspace.allModels")}
            selectedLabel={(draft.manifest.match.models ?? []).join(", ")}
            emptyLabel={$translate("extensions.dev.noModels")}
            onValueChange={(val) => (draft.manifest.match.models = val)}
          />

          <div class="space-y-1">
            <Label class="text-[11px] text-muted-foreground">{$translate("extensions.dev.modelPatterns")}</Label>
            <Input
              value={(draft.manifest.match.models ?? []).join(", ")}
              oninput={(e) => (draft.manifest.match.models = parseList(e.currentTarget.value))}
              placeholder="*"
              class="h-8 text-xs"
            />
          </div>

          <div class="space-y-1">
            <Label class="text-[11px] text-muted-foreground">{$translate("extensions.endpoints")}</Label>
            <Input
              value={formatList(draft.manifest.match.endpoints)}
              oninput={(e) => (draft.manifest.match.endpoints = parseList(e.currentTarget.value))}
              placeholder="/v1/chat/completions"
              class="h-8 font-mono text-[11px]"
            />
          </div>
        </div>
      </div>

      <!-- Section 3: Runtime Settings -->
      <div class="rounded-lg border border-border/70 bg-card/60 p-3 shadow-xs">
        <div class="mb-2.5 flex items-center justify-between border-b border-border/50 pb-2">
          <span class="font-semibold text-foreground">{$translate("extensions.workspace.runtime")}</span>
          <Badge variant="secondary" class="text-[10px]">
            P{draft.manifest.priority ?? 100} · {draft.manifest.timeout || "10s"}
          </Badge>
        </div>

        {#if draft.settings?.length}
          <div class="mb-3 space-y-3">
            <ExtensionSettingsForm
              settings={draft.settings}
              config={draft.manifest.config ?? {}}
              {errors}
              onChange={(key, val) => (draft.manifest.config = { ...draft.manifest.config, [key]: val })}
            />
          </div>
        {:else}
          <div class="mb-3">
            <ExtensionValueEditor
              root
              value={draft.manifest.config ?? {}}
              onChange={(val) => (draft.manifest.config = (val ?? {}) as Record<string, unknown>)}
            />
          </div>
        {/if}

        <div class="space-y-2 border-t border-border/50 pt-2.5">
          <div class="grid grid-cols-2 gap-2">
            <div class="space-y-1">
              <Label class="text-[10px] text-muted-foreground">{$translate("extensions.priority")}</Label>
              <Input type="number" bind:value={draft.manifest.priority} class="h-7 text-xs" />
            </div>
            <div class="space-y-1">
              <Label class="text-[10px] text-muted-foreground">{$translate("extensions.timeout")}</Label>
              <Input bind:value={draft.manifest.timeout} placeholder="10s" class="h-7 text-xs" />
            </div>
          </div>

          <div class="flex items-center justify-between pt-1">
            <Label class="cursor-pointer text-[11px]">{$translate("extensions.continueOnError")}</Label>
            <Switch.Root bind:checked={draft.manifest.continueOnError} />
          </div>
        </div>
      </div>
    </div>
  {:else}
    <!-- Full-featured Tabs Mode: Used in ExtensionSettings route and Quick Settings modal -->
    <Tabs.Root bind:value={activeTab} class="w-full">
      <div class="border-b border-border bg-muted/20 px-4 pt-2">
        <Tabs.List class="h-10 w-full justify-start gap-2 bg-transparent p-0">
          <Tabs.Trigger
            value="basic"
            class="flex items-center gap-1.5 rounded-none border-b-2 border-transparent px-3 py-2 text-xs font-medium text-muted-foreground data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-foreground data-[state=active]:shadow-none"
          >
            <Info class="size-3.5" />
            {$translate("extensions.settings.tabs.basic")}
          </Tabs.Trigger>

          <Tabs.Trigger
            value="matching"
            class="flex items-center gap-1.5 rounded-none border-b-2 border-transparent px-3 py-2 text-xs font-medium text-muted-foreground data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-foreground data-[state=active]:shadow-none"
          >
            <Layers class="size-3.5" />
            {$translate("extensions.settings.tabs.matching")}
          </Tabs.Trigger>

          <Tabs.Trigger
            value="config"
            class="flex items-center gap-1.5 rounded-none border-b-2 border-transparent px-3 py-2 text-xs font-medium text-muted-foreground data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-foreground data-[state=active]:shadow-none"
          >
            <Sliders class="size-3.5" />
            {$translate("extensions.settings.tabs.config")}
            {#if draft.settings?.length}
              <Badge variant="secondary" class="ml-1 h-4 px-1 text-[10px]">
                {draft.settings.length}
              </Badge>
            {/if}
          </Tabs.Trigger>

          <Tabs.Trigger
            value="limits"
            class="flex items-center gap-1.5 rounded-none border-b-2 border-transparent px-3 py-2 text-xs font-medium text-muted-foreground data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-foreground data-[state=active]:shadow-none"
          >
            <Cpu class="size-3.5" />
            {$translate("extensions.settings.tabs.limits")}
          </Tabs.Trigger>

          <Tabs.Trigger
            value="permissions"
            class="flex items-center gap-1.5 rounded-none border-b-2 border-transparent px-3 py-2 text-xs font-medium text-muted-foreground data-[state=active]:border-primary data-[state=active]:bg-transparent data-[state=active]:text-foreground data-[state=active]:shadow-none"
          >
            <Shield class="size-3.5" />
            {$translate("extensions.settings.tabs.permissions")}
          </Tabs.Trigger>
        </Tabs.List>
      </div>

      <div class="p-4 sm:p-6">
        <!-- 1. BASIC TAB -->
        <Tabs.Content value="basic" class="mt-0 space-y-6">
          <div class="grid gap-6 md:grid-cols-2">
            <div class="space-y-4">
              <div class="space-y-1.5">
                <Label for="ext-name">{$translate("extensions.name")}</Label>
                <Input
                  id="ext-name"
                  bind:value={draft.manifest.name}
                  placeholder="e.g. Prompt Optimizer"
                  class="bg-background"
                />
                <p class="text-xs text-muted-foreground">用户友好的展示名称，方便在列表中识别。</p>
              </div>

              <div class="space-y-1.5">
                <Label for="ext-id">{$translate("extensions.id")}</Label>
                <div class="flex items-center gap-2">
                  <Input
                    id="ext-id"
                    bind:value={draft.manifest.id}
                    disabled={Boolean(draft.etag)}
                    placeholder="prompt-optimizer"
                    class="font-mono text-sm bg-background"
                  />
                  {#if draft.etag}
                    <Button variant="outline" size="icon" onclick={copyId} title={$translate("extensions.workspace.copyId")}>
                      {#if copiedId}
                        <Check class="size-4 text-success" />
                      {:else}
                        <Copy class="size-4 text-muted-foreground" />
                      {/if}
                    </Button>
                  {/if}
                </div>
                <p class="text-xs text-muted-foreground">
                  {draft.etag ? "已创建拓展的标识不可变更。" : "唯一标识符，建议仅包含小写字母、数字和中划线。"}
                </p>
              </div>

              <div class="space-y-1.5">
                <Label for="ext-desc">{$translate("extensions.description")}</Label>
                <Textarea
                  id="ext-desc"
                  rows={3}
                  bind:value={draft.manifest.description}
                  placeholder="简述该拓展的功能与用途…"
                  class="bg-background"
                />
              </div>
            </div>

            <div class="space-y-4">
              <div class="rounded-xl border border-border bg-muted/20 p-4">
                <div class="flex items-center justify-between">
                  <div>
                    <p class="font-medium text-sm text-foreground">{$translate("extensions.enabled")}</p>
                    <p class="text-xs text-muted-foreground">开启后，符合匹配规则的请求将经由此拓展处理。</p>
                  </div>
                  <Switch.Root bind:checked={draft.manifest.enabled} />
                </div>
              </div>

              <div class="rounded-xl border border-border bg-card p-4 space-y-3">
                <div class="space-y-1.5">
                  <div class="flex items-center justify-between">
                    <Label for="ext-priority">{$translate("extensions.priority")}</Label>
                    <Badge variant="outline" class="font-mono text-xs">P{draft.manifest.priority ?? 100}</Badge>
                  </div>
                  <Input
                    id="ext-priority"
                    type="number"
                    bind:value={draft.manifest.priority}
                    class="bg-background"
                  />
                  <p class="text-xs text-muted-foreground">
                    执行优先级数值（默认 100）。数值越小优先级越高，更早执行钩子。
                  </p>
                </div>
              </div>

              {#if draft.etag}
                <div class="rounded-xl border border-border/80 bg-muted/30 p-3.5 text-xs text-muted-foreground space-y-1">
                  <div class="flex items-center justify-between">
                    <span>状态:</span>
                    <span class="font-medium text-foreground capitalize">{draft.status}</span>
                  </div>
                  <div class="flex items-center justify-between">
                    <span>版本 ETag:</span>
                    <span class="font-mono text-[11px] truncate max-w-44">{draft.etag}</span>
                  </div>
                </div>
              {/if}
            </div>
          </div>
        </Tabs.Content>

        <!-- 2. MATCHING TAB -->
        <Tabs.Content value="matching" class="mt-0 space-y-6">
          <div class="space-y-5">
            <div class="space-y-2">
              <div class="flex items-center justify-between">
                <div>
                  <Label class="text-sm font-semibold">{$translate("extensions.workspace.models")}</Label>
                  <p class="text-xs text-muted-foreground">选择适用此拓展的目标模型，未选定具体模型时默认适用全部模型。</p>
                </div>
                {#if (draft.manifest.match.models ?? []).length === 0}
                  <Badge variant="outline" class="border-success/30 bg-success/10 text-success text-xs">
                    {$translate("extensions.workspace.allModels")}
                  </Badge>
                {/if}
              </div>

              <ModelMultiSelect
                value={draft.manifest.match.models ?? []}
                options={$models.map((model) => model.id)}
                ariaLabel={$translate("extensions.models")}
                allLabel={$translate("extensions.workspace.allModels")}
                selectedLabel={(draft.manifest.match.models ?? []).join(", ")}
                emptyLabel={$translate("extensions.dev.noModels")}
                onValueChange={(value) => (draft.manifest.match.models = value)}
              />
            </div>

            <div class="rounded-xl border border-border bg-card p-4 space-y-4">
              <div class="space-y-1.5">
                <Label for="ext-model-patterns">{$translate("extensions.dev.modelPatterns")}</Label>
                <Input
                  id="ext-model-patterns"
                  value={formatList(draft.manifest.match.models)}
                  oninput={(e) => (draft.manifest.match.models = parseList(e.currentTarget.value))}
                  placeholder="* 或 Qwen/*, gpt-4o"
                  class="bg-background"
                />
                <p class="text-xs text-muted-foreground">{$translate("extensions.matchHint")}</p>
              </div>

              <div class="space-y-1.5">
                <Label for="ext-exclude-models">{$translate("extensions.excludeModels")}</Label>
                <Input
                  id="ext-exclude-models"
                  value={formatList(draft.manifest.match.excludeModels)}
                  oninput={(e) => (draft.manifest.match.excludeModels = parseList(e.currentTarget.value))}
                  placeholder="claude-3-haiku, test-*"
                  class="bg-background"
                />
                <p class="text-xs text-muted-foreground">排除的模型列表，排除规则优先于匹配规则生效。</p>
              </div>
            </div>

            <div class="rounded-xl border border-border bg-card p-4 space-y-3">
              <div>
                <Label class="text-sm font-semibold">{$translate("extensions.endpoints")}</Label>
                <p class="text-xs text-muted-foreground">限定拦截调用的 API 端点。留空或未指定时匹配所有端点。</p>
              </div>

              <div class="flex flex-wrap gap-2 pt-1">
                {#each standardEndpoints as ep}
                  {@const active = (draft.manifest.match.endpoints ?? []).includes(ep.id)}
                  <button
                    type="button"
                    class={`inline-flex items-center gap-1.5 rounded-lg border px-3 py-1.5 text-xs transition-colors ${
                      active
                        ? "border-primary/50 bg-primary/10 text-primary font-medium"
                        : "border-border bg-muted/30 text-muted-foreground hover:bg-muted"
                    }`}
                    onclick={() => toggleEndpoint(ep.id)}
                  >
                    <span>{ep.label}</span>
                    {#if active}
                      <CheckCircle2 class="size-3.5" />
                    {/if}
                  </button>
                {/each}
              </div>

              <div class="space-y-1 pt-2">
                <Label for="ext-custom-endpoints" class="text-xs text-muted-foreground">
                  {$translate("extensions.workspace.customEndpoints")}
                </Label>
                <Input
                  id="ext-custom-endpoints"
                  value={formatList(draft.manifest.match.endpoints)}
                  oninput={(e) => (draft.manifest.match.endpoints = parseList(e.currentTarget.value))}
                  placeholder="/v1/chat/completions, /v1/embeddings"
                  class="font-mono text-xs bg-background"
                />
              </div>
            </div>

            <div class="grid gap-4 sm:grid-cols-2">
              <div class="space-y-1.5">
                <Label for="ext-profiles">{$translate("extensions.profiles")}</Label>
                <Input
                  id="ext-profiles"
                  value={formatList(draft.manifest.match.profiles)}
                  oninput={(e) => (draft.manifest.match.profiles = parseList(e.currentTarget.value))}
                  placeholder="default, production"
                  class="bg-background"
                />
                <p class="text-xs text-muted-foreground">仅在匹配对应激活配置档时触发。</p>
              </div>

              <div class="space-y-1.5">
                <Label for="ext-providers">{$translate("extensions.providers")}</Label>
                <Input
                  id="ext-providers"
                  value={formatList(draft.manifest.match.providers)}
                  oninput={(e) => (draft.manifest.match.providers = parseList(e.currentTarget.value))}
                  placeholder="openai, anthropic"
                  class="bg-background"
                />
                <p class="text-xs text-muted-foreground">仅在匹配对应后端服务提供方时触发。</p>
              </div>
            </div>
          </div>
        </Tabs.Content>

        <!-- 3. CONFIG TAB -->
        <Tabs.Content value="config" class="mt-0 space-y-6">
          <div class="flex flex-wrap items-center justify-between gap-3 border-b border-border pb-3">
            <div>
              <h3 class="font-semibold text-sm">{$translate("extensions.workspace.runtime")}</h3>
              <p class="text-xs text-muted-foreground">
                {draft.settings?.length
                  ? "此拓展声明了可视配置表单项，支持直观调整或切换至原始 JSON 视图。"
                  : "此拓展未声明表单配置项，可通过结构化或原始 JSON 配置。"}
              </p>
            </div>

            <div class="flex items-center gap-2">
              <Button
                variant={rawJsonMode ? "default" : "outline"}
                size="sm"
                onclick={() => {
                  rawJsonMode = !rawJsonMode;
                  if (rawJsonMode) initRawJson();
                }}
              >
                {#if rawJsonMode}
                  <FormInput class="size-3.5 mr-1" />
                  {$translate("extensions.settings.form")}
                {:else}
                  <Braces class="size-3.5 mr-1" />
                  {$translate("extensions.settings.rawJson")}
                {/if}
              </Button>
            </div>
          </div>

          {#if rawJsonMode}
            <!-- Raw JSON Editor View -->
            <div class="space-y-3">
              <div class="flex items-center justify-between">
                <div class="flex items-center gap-2 text-xs">
                  {#if rawJsonValid}
                    <span class="flex items-center gap-1 text-success">
                      <CheckCircle2 class="size-3.5" />
                      {$translate("extensions.settings.validJson")}
                    </span>
                  {:else if rawJsonError}
                    <span class="flex items-center gap-1 text-destructive">
                      <AlertCircle class="size-3.5" />
                      {rawJsonError}
                    </span>
                  {/if}
                </div>
                <Button variant="outline" size="sm" onclick={formatRawJson}>
                  {$translate("extensions.settings.formatJson")}
                </Button>
              </div>

              <Textarea
                rows={12}
                value={rawJsonDraft}
                oninput={(e) => handleRawJsonInput(e.currentTarget.value)}
                class="font-mono text-xs leading-relaxed bg-background"
                placeholder={'{\n  "key": "value"\n}'}
              />
            </div>
          {:else}
            <!-- Structured Form or Value Editor -->
            {#if draft.settings?.length}
              <div class="rounded-xl border border-border bg-card p-5">
                <ExtensionSettingsForm
                  settings={draft.settings}
                  config={draft.manifest.config ?? {}}
                  {errors}
                  onChange={(key, val) => (draft.manifest.config = { ...draft.manifest.config, [key]: val })}
                />
              </div>
            {:else}
              <div class="rounded-xl border border-border bg-card p-5 space-y-3">
                <p class="text-xs text-muted-foreground">
                  {$translate("extensions.settings.none")}
                </p>
                <ExtensionValueEditor
                  root
                  value={draft.manifest.config ?? {}}
                  onChange={(val) => (draft.manifest.config = (val ?? {}) as Record<string, unknown>)}
                />
              </div>
            {/if}
          {/if}
        </Tabs.Content>

        <!-- 4. LIMITS TAB -->
        <Tabs.Content value="limits" class="mt-0 space-y-6">
          <div class="grid gap-6 md:grid-cols-2">
            <!-- Left: Resource Quotas -->
            <div class="rounded-xl border border-border bg-card p-5 space-y-4">
              <div class="flex items-center gap-2 border-b border-border pb-3">
                <Clock class="size-4 text-primary" />
                <h3 class="font-semibold text-sm">超时与执行配额</h3>
              </div>

              <div class="space-y-2">
                <div class="flex items-center justify-between">
                  <Label for="ext-timeout">{$translate("extensions.timeout")}</Label>
                  <div class="flex items-center gap-1">
                    {#each ["5s", "10s", "30s", "60s"] as t}
                      <button
                        type="button"
                        class={`rounded px-1.5 py-0.5 text-[11px] transition-colors ${
                          draft.manifest.timeout === t
                            ? "bg-primary text-primary-foreground font-medium"
                            : "bg-muted text-muted-foreground hover:bg-muted/80"
                        }`}
                        onclick={() => setTimeoutPreset(t)}
                      >
                        {t}
                      </button>
                    {/each}
                  </div>
                </div>
                <Input
                  id="ext-timeout"
                  bind:value={draft.manifest.timeout}
                  placeholder="10s"
                  class="font-mono text-xs bg-background"
                />
                <p class="text-xs text-muted-foreground">钩子执行的最长超时时间，如 10s 或 500ms。</p>
              </div>

              <div class="grid grid-cols-2 gap-3 pt-2">
                <div class="space-y-1.5">
                  <Label for="ext-cpu">{$translate("extensions.cpu")}</Label>
                  <Input
                    id="ext-cpu"
                    type="number"
                    min="100"
                    bind:value={draft.manifest.maxCpuMillis}
                    class="bg-background"
                  />
                  <p class="text-[11px] text-muted-foreground">单次执行最大 CPU 时间（毫秒）</p>
                </div>

                <div class="space-y-1.5">
                  <Label for="ext-mem">{$translate("extensions.memory")}</Label>
                  <Input
                    id="ext-mem"
                    type="number"
                    min="16"
                    bind:value={draft.manifest.maxMemoryMiB}
                    class="bg-background"
                  />
                  <p class="text-[11px] text-muted-foreground">沙箱最大内存配额（MiB）</p>
                </div>
              </div>
            </div>

            <!-- Right: Policy & Error Handling -->
            <div class="rounded-xl border border-border bg-card p-5 space-y-4">
              <div class="flex items-center gap-2 border-b border-border pb-3">
                <Sliders class="size-4 text-primary" />
                <h3 class="font-semibold text-sm">冲突策略与异常处理</h3>
              </div>

              <div class="space-y-1.5">
                <Label for="ext-conflict">{$translate("extensions.conflict")}</Label>
                <select
                  id="ext-conflict"
                  bind:value={draft.manifest.toolConflict}
                  class="w-full rounded-md border border-border bg-background p-2 text-xs"
                >
                  <option value="skip">skip（保留既有工具，跳过重名注入）</option>
                  <option value="override">override（优先覆盖，替换已有工具）</option>
                  <option value="error">error（冲突时中断报错）</option>
                </select>
                <p class="text-xs text-muted-foreground">当拓展注入的工具名称与请求中既有工具重名时的处理方式。</p>
              </div>

              <div class="space-y-3 pt-3 border-t border-border/60">
                <div class="flex items-center justify-between">
                  <div>
                    <Label class="text-xs font-medium cursor-pointer">{$translate("extensions.continueOnError")}</Label>
                    <p class="text-[11px] text-muted-foreground">拓展钩子抛错时忽略并放行原始请求，避免阻断调用。</p>
                  </div>
                  <Switch.Root bind:checked={draft.manifest.continueOnError} />
                </div>

                <div class="flex items-center justify-between pt-2">
                  <div>
                    <Label class="text-xs font-medium cursor-pointer">{$translate("extensions.interceptClientTools")}</Label>
                    <p class="text-[11px] text-muted-foreground">拦截并代理客户端携带的工具定义与调用。</p>
                  </div>
                  <Switch.Root bind:checked={draft.manifest.interceptClientTools} />
                </div>
              </div>
            </div>
          </div>
        </Tabs.Content>

        <!-- 5. PERMISSIONS TAB -->
        <Tabs.Content value="permissions" class="mt-0 space-y-6">
          <div class="grid gap-6 md:grid-cols-2">
            <!-- Left: Network & Storage -->
            <div class="space-y-5">
              <div class="rounded-xl border border-border bg-card p-5 space-y-3">
                <div class="flex items-center gap-2 border-b border-border pb-2.5">
                  <Network class="size-4 text-primary" />
                  <Label class="font-semibold text-sm">{$translate("extensions.networkHosts")}</Label>
                </div>
                <Textarea
                  rows={4}
                  value={formatLines(draft.manifest.permissions.networkHosts)}
                  oninput={(e) => (draft.manifest.permissions.networkHosts = parseList(e.currentTarget.value))}
                  placeholder="api.weather.com&#10;api.github.com"
                  class="font-mono text-xs bg-background"
                />
                <p class="text-xs text-muted-foreground">
                  {$translate("extensions.networkHostsHint")}
                </p>
                <div class="flex items-start gap-2 rounded-lg border border-border/60 bg-muted/40 p-3">
                  <Info class="size-3.5 shrink-0 text-muted-foreground mt-0.5" />
                  <div class="min-w-0 flex-1 space-y-2">
                    <p class="text-xs text-muted-foreground">
                      {$translate("extensions.proxyHintBody")}
                    </p>
                    <Button variant="outline" size="sm" onclick={() => push("/settings")}>
                      {$translate("extensions.proxyHintOpen")}
                    </Button>
                  </div>
                </div>
              </div>

              <div class="rounded-xl border border-border bg-card p-5 space-y-3">
                <div class="flex items-center gap-2 border-b border-border pb-2.5">
                  <HardDrive class="size-4 text-primary" />
                  <Label class="font-semibold text-sm">{$translate("extensions.workspace.storageMode")}</Label>
                </div>
                <select
                  class="w-full rounded-md border border-border bg-background p-2 text-xs"
                  value={draft.manifest.permissions.storage ?? ""}
                  onchange={(e) => {
                    const val = e.currentTarget.value;
                    if (val) draft.manifest.permissions.storage = val;
                    else delete draft.manifest.permissions.storage;
                  }}
                >
                  <option value="">{$translate("extensions.workspace.storageEphemeral")}</option>
                  <option value="persistent">{$translate("extensions.workspace.storagePersistent")}</option>
                  <option value="none">{$translate("extensions.workspace.storageNone")}</option>
                </select>
                <p class="text-xs text-muted-foreground">
                  控制拓展调用 <code>ctx.kv</code> 的存储策略：临时内存隔离、数据库持久化或禁止存取。
                </p>
              </div>
            </div>

            <!-- Right: Filesystem Roots -->
            <div class="rounded-xl border border-border bg-card p-5 space-y-4">
              <div class="flex items-center gap-2 border-b border-border pb-2.5">
                <Shield class="size-4 text-primary" />
                <h3 class="font-semibold text-sm">文件系统沙箱访问控制</h3>
              </div>

              <div class="space-y-1.5">
                <Label>{$translate("extensions.readRoots")}</Label>
                <Textarea
                  rows={3}
                  value={formatLines(draft.manifest.permissions.readRoots)}
                  oninput={(e) => (draft.manifest.permissions.readRoots = parseList(e.currentTarget.value))}
                  placeholder="/var/data/readonly&#10;/opt/models/catalog"
                  class="font-mono text-xs bg-background"
                />
                <p class="text-xs text-muted-foreground">允许拓展只读访问的主机目录绝对路径（每行一个）。</p>
              </div>

              <div class="space-y-1.5 pt-2">
                <Label>{$translate("extensions.writeRoots")}</Label>
                <Textarea
                  rows={3}
                  value={formatLines(draft.manifest.permissions.writeRoots)}
                  oninput={(e) => (draft.manifest.permissions.writeRoots = parseList(e.currentTarget.value))}
                  placeholder="/tmp/extension-cache"
                  class="font-mono text-xs bg-background"
                />
                <p class="text-xs text-muted-foreground">允许拓展写入访问的主机目录绝对路径（每行一个）。</p>
              </div>
            </div>
          </div>
        </Tabs.Content>
      </div>
    </Tabs.Root>
  {/if}
</div>
