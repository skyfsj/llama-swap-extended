<script lang="ts">
  import { ArrowLeft, ArrowRight, Check, LoaderCircle, ShieldAlert } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { translate } from "$lib/i18n";
  import {
    cloneRuntimeValue,
    isOfficialRuntimeTemplate,
    runtimeConfigForTemplate,
    runtimeTemplateNameSuggestion,
    runtimeTemplates,
    runtimeWizardFirstStep,
    runtimeWizardValidation,
    sourceBoundaryForPaths,
    type RuntimeTemplateID,
  } from "$lib/runtimeCenter";
  import { commitSettings, previewSettings, type ApiError, type SettingsDiagnostic, type SettingsDraft, type SettingsPreview, type SettingsSnapshot } from "$lib/settingsApi";

  interface Props {
    open?: boolean;
    seed: number;
    runtimeNames: string[];
    settingsSnapshot: SettingsSnapshot | null;
    initialName?: string;
    initialConfig?: Record<string, unknown>;
    mode?: "create" | "clone" | "edit";
    onCommitted: () => void;
  }

  let {
    open = $bindable(false),
    seed,
    runtimeNames,
    settingsSnapshot,
    initialName = "",
    initialConfig,
    mode = "create",
    onCommitted,
  }: Props = $props();

  let step = $state(0);
  let templateID = $state<RuntimeTemplateID>("vllm-official");
  let name = $state("");
  let nameAutoFilled = $state(false);
  let runtimeConfig = $state<Record<string, unknown>>({});
  let expertJSON = $state("");
  let expertError = $state("");
  let error = $state("");
  let loading = $state(false);
  let preview = $state<SettingsPreview | null>(null);
  let appliedSeed = -1;

  const stepKeys = ["template", "definition", "preview"] as const;

  // Editing an existing runtime has no template to pick: its definition is
  // already there, so the wizard starts on the definition form and only ever
  // offers the definition and preview steps.
  const firstStep = $derived(runtimeWizardFirstStep(mode));
  const visibleStepKeys = $derived(mode === "edit" ? stepKeys.slice(1) : stepKeys);

  function asRecord(value: unknown): Record<string, any> {
    return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, any> : {};
  }

  function reset(): void {
    templateID = initialConfig ? "clone" : "vllm-official";
    runtimeConfig = runtimeConfigForTemplate(templateID, initialConfig);
    // In create mode the template fills in a suggested name so the preset
    // needs no other input; edit/clone keep their own names.
    const suggested = mode === "create" && !initialName ? runtimeTemplateNameSuggestion(templateID) : "";
    name = initialName || suggested;
    nameAutoFilled = suggested !== "";
    expertJSON = JSON.stringify(runtimeConfig, null, 2);
    expertError = "";
    error = "";
    preview = null;
    step = firstStep;
  }

  $effect(() => {
    if (open && seed !== appliedSeed) {
      appliedSeed = seed;
      reset();
    }
    if (!open) appliedSeed = -1;
  });

  function updateConfig(next: Record<string, unknown>): void {
    runtimeConfig = next;
    expertJSON = JSON.stringify(runtimeConfig, null, 2);
    preview = null;
  }

  function chooseTemplate(next: RuntimeTemplateID): void {
    templateID = next;
    updateConfig(runtimeConfigForTemplate(next, next === "clone" ? initialConfig : undefined));
    if (mode === "create" && !initialName && (!name.trim() || nameAutoFilled)) {
      const suggested = runtimeTemplateNameSuggestion(next);
      if (suggested) {
        name = suggested;
        nameAutoFilled = true;
      }
    }
  }

  function source(): Record<string, any> {
    return asRecord(runtimeConfig.source);
  }

  function sourceValue(key: string): string {
    return String(source()[key] ?? "");
  }

  function localizedDiagnostic(diagnostic: SettingsDiagnostic): string {
    const raw = diagnostic.message || "";
    if (raw.includes("source.repository") || diagnostic.path?.includes("/source/repository")) {
      return $translate("controlPlane.runtimeCenter.wizard.diagnostics.repository");
    }
    if (raw.includes("source.url") || diagnostic.path?.includes("/source/url")) {
      return $translate("controlPlane.runtimeCenter.wizard.diagnostics.url");
    }
    if (raw.includes("source.ref") || diagnostic.path?.includes("/source/ref")) {
      return $translate("controlPlane.runtimeCenter.wizard.diagnostics.ref");
    }
    return raw;
  }

  function localizedBoundaryReason(reason: string): string {
    if (reason.includes("different configuration sources")) return $translate("controlPlane.runtimeCenter.wizard.boundaryReasons.differentSources");
    if (reason.includes("read-only source")) return $translate("controlPlane.runtimeCenter.wizard.boundaryReasons.readOnly");
    if (reason.includes("no safe writable source")) return $translate("controlPlane.runtimeCenter.wizard.boundaryReasons.unresolved");
    if (reason.includes("unavailable")) return $translate("controlPlane.runtimeCenter.wizard.boundaryReasons.unavailable");
    return reason;
  }

  function localizedError(cause: unknown): string {
    const diagnostics = (cause as ApiError)?.diagnostics;
    if (diagnostics?.length) return diagnostics.map(localizedDiagnostic).join("; ");
    return localizedDiagnostic({ message: cause instanceof Error ? cause.message : String(cause) });
  }

  function setSourceValue(key: string, value: string): void {
    updateConfig({ ...runtimeConfig, source: { ...source(), [key]: value } });
  }

  function setTopValue(key: string, value: unknown): void {
    updateConfig({ ...runtimeConfig, [key]: value });
  }

  function updatePolicyValue(key: string, value: unknown): void {
    updateConfig({ ...runtimeConfig, update: { ...asRecord(runtimeConfig.update), [key]: value } });
  }

  function policyValue(key: string): unknown {
    return asRecord(runtimeConfig.update)[key];
  }

  function validationErrors(): string[] {
    const errors = runtimeWizardValidation(name, runtimeConfig);
    if (mode !== "edit" && runtimeNames.includes(name.trim())) errors.push("name-duplicate");
    if (settingsSnapshot && !settingsSnapshot.writable) errors.push("settings-readonly");
    if (!settingsSnapshot) errors.push("settings-unavailable");
    return errors;
  }

  function boundary(): ReturnType<typeof sourceBoundaryForPaths> {
    return sourceBoundaryForPaths(settingsSnapshot, ["/runtimes"]);
  }

  function buildDraft(): SettingsDraft {
    if (!settingsSnapshot) throw new Error("settings snapshot is unavailable");
    const nextConfig = cloneRuntimeValue(settingsSnapshot.config);
    const runtimes = asRecord(nextConfig.runtimes);
    runtimes[name.trim()] = cloneRuntimeValue(runtimeConfig);
    const changes: NonNullable<SettingsDraft["changes"]> = [{ op: "replace", path: "/runtimes", value: runtimes }];
    return { mode: "structured", changes, message: "runtime control center" };
  }

  function syncExpert(): void {
    try {
      const parsed = JSON.parse(expertJSON) as unknown;
      if (!parsed || typeof parsed !== "object" || Array.isArray(parsed)) throw new Error($translate("controlPlane.runtimeCenter.wizard.diagnostics.objectRequired"));
      runtimeConfig = parsed as Record<string, unknown>;
      expertError = "";
      preview = null;
    } catch (cause) {
      expertError = cause instanceof Error ? cause.message : String(cause);
    }
  }

  async function runPreview(): Promise<void> {
    error = "";
    const errors = validationErrors();
    if (errors.length > 0) {
      error = $translate(`controlPlane.runtimeCenter.wizard.validation.${errors[0]}`);
      return;
    }
    const sourceBoundary = boundary();
    if (sourceBoundary.blocked) {
      error = `${$translate("controlPlane.runtimeCenter.wizard.sourceBoundary")}: ${localizedBoundaryReason(sourceBoundary.reason)}`;
      return;
    }
    loading = true;
    try {
      preview = await previewSettings(buildDraft());
      if (!preview.valid) {
        error = preview.diagnostics?.map(localizedDiagnostic).join("; ") || $translate("controlPlane.runtimeCenter.wizard.previewInvalid");
      } else {
        step = 2;
      }
    } catch (cause) {
      error = localizedError(cause);
    } finally {
      loading = false;
    }
  }

  async function commit(): Promise<void> {
    if (!settingsSnapshot || !preview?.valid) return;
    loading = true;
    error = "";
    try {
      await commitSettings(settingsSnapshot, buildDraft(), preview.previewHash);
      open = false;
      onCommitted();
    } catch (cause) {
      error = localizedError(cause);
    } finally {
      loading = false;
    }
  }

  function next(): void {
    error = "";
    // The definition step is the only one that leads anywhere but the
    // preview: from the template step it is "continue", and from the
    // definition step it runs the preview.
    if (step === 0) { step = 1; return; }
    if (step === 1) {
      const errors = validationErrors();
      if (errors.length > 0) { error = $translate(`controlPlane.runtimeCenter.wizard.validation.${errors[0]}`); return; }
      void runPreview();
      return;
    }
    void commit();
  }

  function previous(): void {
    if (step > firstStep) step -= 1;
  }

  function templateLabel(id: RuntimeTemplateID): string {
    return $translate(runtimeTemplates.find((template) => template.id === id)?.labelKey ?? "controlPlane.runtimeCenter.templates.clone.label");
  }
</script>

<Dialog.Root bind:open>
  <Dialog.Content class="max-h-[calc(100dvh-1.5rem)] max-w-3xl overflow-y-auto sm:max-w-3xl">
    <Dialog.Header>
      <Dialog.Title>{$translate(mode === "clone" ? "controlPlane.runtimeCenter.wizard.cloneTitle" : mode === "edit" ? "controlPlane.runtimeCenter.wizard.editTitle" : "controlPlane.runtimeCenter.wizard.createTitle")}</Dialog.Title>
      <Dialog.Description>{$translate("controlPlane.runtimeCenter.wizard.description")}</Dialog.Description>
    </Dialog.Header>

    <div class="grid gap-4">
      <ol class="grid grid-cols-3 gap-1" aria-label={$translate("controlPlane.runtimeCenter.wizard.stepsLabel")}>
        {#each visibleStepKeys as key, index (key)}<li class={`rounded-md border px-2 py-2 text-center text-xs ${step === index + firstStep ? "border-primary bg-primary/10 font-medium" : index + firstStep < step ? "border-primary/30 text-primary" : "text-muted-foreground"}`} aria-current={step === index + firstStep ? "step" : undefined}>{index + 1}. {$translate(`controlPlane.runtimeCenter.wizard.steps.${key}`)}</li>{/each}
      </ol>

      {#if step === 0}
        <fieldset class="grid gap-2"><legend class="text-sm font-semibold">{$translate("controlPlane.runtimeCenter.wizard.presetsLegend")}</legend><div class="grid gap-2 sm:grid-cols-2">
          {#each runtimeTemplates as template (template.id)}
            {#if isOfficialRuntimeTemplate(template.id)}
              <label class={`grid cursor-pointer gap-1 rounded-lg border p-3 transition-colors ${templateID === template.id ? "border-primary bg-primary/5" : "hover:bg-muted/40"}`}><input class="sr-only" type="radio" name="runtime-template" value={template.id} checked={templateID === template.id} onchange={() => chooseTemplate(template.id)} /><span class="text-sm font-medium">{$translate(template.labelKey)}</span><span class="text-xs text-muted-foreground">{$translate(template.descriptionKey)}</span></label>
            {/if}
          {/each}
        </div></fieldset>
        <fieldset class="grid gap-2"><legend class="text-sm font-semibold">{$translate("controlPlane.runtimeCenter.wizard.customLegend")}</legend><div class="grid gap-2 sm:grid-cols-2">
          {#each runtimeTemplates as template (template.id)}
            {#if !isOfficialRuntimeTemplate(template.id)}
              <label class={`grid cursor-pointer gap-1 rounded-lg border p-3 transition-colors ${templateID === template.id ? "border-primary bg-primary/5" : "hover:bg-muted/40"}`}><input class="sr-only" type="radio" name="runtime-template" value={template.id} checked={templateID === template.id} onchange={() => chooseTemplate(template.id)} /><span class="text-sm font-medium">{$translate(template.labelKey)}</span><span class="text-xs text-muted-foreground">{$translate(template.descriptionKey)}</span></label>
            {/if}
          {/each}
          {#if initialConfig}<label class={`grid cursor-pointer gap-1 rounded-lg border p-3 transition-colors ${templateID === "clone" ? "border-primary bg-primary/5" : "hover:bg-muted/40"}`}><input class="sr-only" type="radio" name="runtime-template" value="clone" checked={templateID === "clone"} onchange={() => chooseTemplate("clone")} /><span class="text-sm font-medium">{templateLabel("clone")}</span><span class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.templates.clone.description")}</span></label>{/if}
        </div></fieldset>
      {:else if step === 1}
        <div class="grid gap-4">
          <div class="grid gap-2 sm:grid-cols-2">
            <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.name")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50 disabled:cursor-not-allowed disabled:bg-muted/50" bind:value={name} oninput={() => (nameAutoFilled = false)} placeholder={mode === "create" ? runtimeTemplateNameSuggestion(templateID) : "vllm-prod"} autocomplete="off" readonly={mode === "edit"} disabled={mode === "edit"} />{#if mode === "edit"}<span class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.nameLockedHint")}</span>{/if}</label>
            <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.kind")}</span><select class="h-9 rounded-lg border border-input bg-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={String(runtimeConfig.kind ?? "")} onchange={(event) => setTopValue("kind", event.currentTarget.value)}><option value="llamacpp">llama.cpp</option><option value="vllm">vLLM</option></select></label>
            <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.mode")}</span><select class="h-9 rounded-lg border border-input bg-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={String(runtimeConfig.mode ?? "native")} onchange={(event) => setTopValue("mode", event.currentTarget.value)}><option value="native">{$translate("controlPlane.runtimeCenter.wizard.native")}</option><option value="container">{$translate("controlPlane.runtimeCenter.wizard.container")}</option></select></label>
            <label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.sourceType")}</span><select class="h-9 rounded-lg border border-input bg-background px-3 text-sm outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={sourceValue("type")} onchange={(event) => setSourceValue("type", event.currentTarget.value)}>{#if sourceValue("type") === "bundled"}<option value="bundled">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.bundled")}</option>{/if}<option value="wheel">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.wheel")}</option><option value="git">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.git")}</option><option value="release">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.release")}</option><option value="channel">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.channel")}</option><option value="pypi">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.pypi")}</option><option value="tag">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.tag")}</option><option value="commit">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.commit")}</option><option value="local">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.local")}</option><option value="image">{$translate("controlPlane.runtimeCenter.wizard.sourceTypes.image")}</option></select></label>
          </div>
          {#if sourceValue("type") === "wheel"}<label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.wheelPath")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={sourceValue("path")} oninput={(event) => setSourceValue("path", event.currentTarget.value)} placeholder="/runtime-sources/vllm.whl" /></label>{/if}
          {#if ["git", "tag", "commit"].includes(sourceValue("type"))}<div class="grid gap-2 sm:grid-cols-2"><label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.repository")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={sourceValue("repository")} oninput={(event) => setSourceValue("repository", event.currentTarget.value)} placeholder="https://github.com/org/project.git" /></label><label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.trackRef")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={sourceValue("trackRef")} oninput={(event) => setSourceValue("trackRef", event.currentTarget.value)} /></label></div>{/if}
          {#if ["release", "channel"].includes(sourceValue("type"))}<label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.sourceURL")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={sourceValue("url")} oninput={(event) => setSourceValue("url", event.currentTarget.value)} placeholder="https://example.com/llama.cpp.tar.gz" /></label>{/if}
          {#if sourceValue("type") === "local"}<label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.sourcePath")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={sourceValue("path")} oninput={(event) => setSourceValue("path", event.currentTarget.value)} placeholder="/opt/runtime-sources/llama.cpp" /></label>{/if}
          {#if String(runtimeConfig.mode ?? "native") === "container" || sourceValue("type") === "image"}<div class="grid gap-2 sm:grid-cols-2"><label class="grid gap-1 text-sm sm:col-span-2"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.image")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 font-mono text-xs outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" value={sourceValue("image")} oninput={(event) => setSourceValue("image", event.currentTarget.value)} placeholder="registry.example/vllm:stable" /></label></div>{/if}
          <fieldset class="grid gap-2 rounded-lg border p-3"><legend class="px-1 text-sm font-semibold">{$translate("controlPlane.runtimeCenter.wizard.policyLegend")}</legend><div class="grid gap-2 sm:grid-cols-2"><label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.policy")}</span><select class="h-9 rounded-lg border border-input bg-background px-3 text-sm" value={String(policyValue("policy") ?? "automatic")} onchange={(event) => updatePolicyValue("policy", event.currentTarget.value)}><option value="automatic">{$translate("controlPlane.runtimeCenter.policy.automatic")}</option><option value="manual">{$translate("controlPlane.runtimeCenter.policy.manual")}</option><option value="disabled">{$translate("controlPlane.runtimeCenter.policy.disabled")}</option><option value="pinned">{$translate("controlPlane.runtimeCenter.policy.pinned")}</option></select></label><label class="flex items-center gap-2 pt-5 text-sm"><input type="checkbox" checked={policyValue("activateOnlyWhenIdle") !== false} onchange={(event) => updatePolicyValue("activateOnlyWhenIdle", event.currentTarget.checked)} />{$translate("controlPlane.runtimeCenter.wizard.idleGate")}</label></div><div class="grid gap-2 sm:grid-cols-2"><label class="grid gap-1 text-sm"><span class="font-medium">{$translate("controlPlane.runtimeCenter.wizard.keepVersions")}</span><input class="h-9 rounded-lg border border-input bg-background px-3 text-sm" type="number" min="0" max="20" value={Number(policyValue("keepVersions") ?? 2)} oninput={(event) => updatePolicyValue("keepVersions", Number(event.currentTarget.value))} /></label><label class="flex items-center gap-2 pt-5 text-sm"><input type="checkbox" checked={policyValue("rollbackOnFailure") !== false} onchange={(event) => updatePolicyValue("rollbackOnFailure", event.currentTarget.checked)} />{$translate("controlPlane.runtimeCenter.wizard.rollback")}</label></div></fieldset>
          {#if mode === "edit"}<details class="rounded-lg border px-3 py-2"><summary class="cursor-pointer text-sm font-medium">{$translate("controlPlane.runtimeCenter.wizard.expertSummary")}</summary><div class="mt-2 grid gap-2"><p class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.expertHint")}</p><textarea class="min-h-48 w-full rounded-lg border border-input bg-background p-3 font-mono text-xs leading-5 outline-none focus-visible:border-ring focus-visible:ring-3 focus-visible:ring-ring/50" bind:value={expertJSON} oninput={syncExpert} aria-label={$translate("controlPlane.runtimeCenter.expert.ariaLabel")}></textarea>{#if expertError}<p class="text-xs text-destructive" role="alert">{expertError}</p>{/if}</div></details>{/if}
        </div>
      {:else}
        <div class="grid gap-3">
          <div class="rounded-lg border bg-muted/20 p-3"><div class="flex items-start gap-2"><Check class="mt-0.5 size-4 text-primary" aria-hidden="true" /><div><h3 class="text-sm font-semibold">{$translate("controlPlane.runtimeCenter.wizard.previewTitle")}</h3><p class="mt-1 text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.previewHint")}</p></div></div></div>
          <dl class="grid gap-3 rounded-lg border p-3 text-sm sm:grid-cols-2"><div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.name")}</dt><dd class="mt-1 font-medium">{name}</dd></div><div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.template")}</dt><dd class="mt-1">{mode === "edit" ? $translate("controlPlane.runtimeCenter.wizard.existingTemplate") : templateLabel(templateID)}</dd></div><div><dt class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.policy")}</dt><dd class="mt-1">{$translate(`controlPlane.runtimeCenter.policy.${String(policyValue("policy") ?? "automatic")}`)}</dd></div></dl>
          {#if boundary().blocked}<div class="flex items-start gap-2 rounded-md border border-destructive/35 bg-destructive/10 p-3 text-sm text-destructive" role="alert"><ShieldAlert class="mt-0.5 size-4 shrink-0" aria-hidden="true" /><span>{$translate("controlPlane.runtimeCenter.wizard.sourceBoundary")}: {localizedBoundaryReason(boundary().reason)}<span class="mt-1 block text-xs">{boundary().owners.join(" · ")}</span></span></div>{:else}<p class="rounded-md border border-primary/25 bg-primary/5 p-3 text-xs">{$translate("controlPlane.runtimeCenter.wizard.atomicHint")}</p>{/if}
          {#if preview}<div class="grid gap-2 rounded-lg border p-3"><h3 class="text-sm font-semibold">{$translate("controlPlane.runtimeCenter.wizard.previewChanges")}</h3><p class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.previewSources", { sources: preview.sourceTargets?.join(" · ") || "—" })}</p><p class="text-xs text-muted-foreground">{$translate("controlPlane.runtimeCenter.wizard.previewDiff", { count: preview.diff?.length ?? 0 })}</p>{#if preview.diagnostics?.length}<ul class="grid gap-1 text-xs text-amber-700 dark:text-amber-300">{#each preview.diagnostics as diagnostic}<li>{localizedDiagnostic(diagnostic)}</li>{/each}</ul>{/if}</div>{/if}
        </div>
      {/if}

      {#if error}<div class="rounded-md border border-destructive/35 bg-destructive/10 p-3 text-sm text-destructive" role="alert">{error}</div>{/if}
      <Dialog.Footer class="flex-wrap">
        <Button variant="outline" type="button" onclick={() => (open = false)} disabled={loading}>{$translate("common.cancel")}</Button>
        <span class="flex-1"></span>
        {#if step > firstStep}<Button variant="ghost" type="button" onclick={previous} disabled={loading}><ArrowLeft data-icon="inline-start" />{$translate("controlPlane.runtimeCenter.wizard.back")}</Button>{/if}
        <Button type="button" onclick={next} disabled={loading || Boolean(expertError) || (step === 2 && (!preview?.valid || boundary().blocked))}>{#if loading}<LoaderCircle class="animate-spin" aria-hidden="true" />{/if}{step === 2 ? $translate("controlPlane.runtimeCenter.wizard.commit") : $translate("controlPlane.runtimeCenter.wizard.next")} {#if !loading && step < 2}<ArrowRight data-icon="inline-end" />{/if}</Button>
      </Dialog.Footer>
    </div>
  </Dialog.Content>
</Dialog.Root>
