<script lang="ts">
  import { hasListedModels } from "../../stores/api";
  import { persistentStore } from "../../stores/persistent";
  import { createPlaygroundInterface } from "../../lib/playgroundInterface";
  import { generateImage } from "../../lib/imageApi";
  import { generateSdImage, fetchSdLoras } from "../../lib/sdApi";
  import { playgroundStores } from "../../stores/playgroundActivity";
  import ModelSelector from "./ModelSelector.svelte";
  import ExpandableTextarea from "./ExpandableTextarea.svelte";
  import EmptyState from "../EmptyState.svelte";
  import type { ImageApiMode, SdApiLora, SdApiLoraRef } from "../../lib/types";
  import { isSubmitEnter } from "../../lib/ime";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import { Download, X, Sparkles, SlidersHorizontal, Trash2, ZoomIn } from "@lucide/svelte";
  import { t, translate } from "../../lib/i18n";

  const iface = createPlaygroundInterface("playground-image-model", playgroundStores.imageGenerating);
  const selectedModelStore = iface.selectedModel;
  const busyStore = iface.busy;
  const error = iface.error;
  const selectedSizeStore = persistentStore<string>("playground-image-size", "1024x1024");
  const apiModeStore = persistentStore<ImageApiMode>("playground-image-api-mode", "openai");

  // SDAPI persistent settings
  const sdNegativePromptStore = persistentStore<string>("playground-sdapi-negative-prompt", "");
  const sdStepsStore = persistentStore<number>("playground-sdapi-steps", 20);
  const sdCfgScaleStore = persistentStore<number>("playground-sdapi-cfg-scale", 7);
  const sdSeedStore = persistentStore<number>("playground-sdapi-seed", -1);
  const sdSamplerStore = persistentStore<string>("playground-sdapi-sampler", "");
  const sdSchedulerStore = persistentStore<string>("playground-sdapi-scheduler", "");
  const sdBatchSizeStore = persistentStore<number>("playground-sdapi-batch-size", 1);

  let prompt = $state("");
  let isGenerating = $derived($busyStore);
  let generatedImages = $state<string[]>([]);
  let showFullscreen = $state(false);
  let fullscreenIndex = $state(0);
  let showSettings = $state(false);

  // SDAPI lora state
  let availableLoras = $state<SdApiLora[]>([]);
  let selectedLoras = $state<SdApiLoraRef[]>([]);
  let isLoadingLoras = $state(false);
  let lorasLoaded = $state(false);
  let loraError = $state<string | null>(null);

  let isSdapi = $derived($apiModeStore === "sdapi");

  async function loadLoras() {
    if (!$selectedModelStore || isLoadingLoras) return;
    isLoadingLoras = true;
    loraError = null;
    try {
      const loras = await fetchSdLoras($selectedModelStore);
      availableLoras = loras;
      lorasLoaded = true;
    } catch (err) {
      availableLoras = [];
      loraError = err instanceof Error ? err.message : t("errors.failedLoadLoras");
      lorasLoaded = false;
    } finally {
      isLoadingLoras = false;
    }
  }

  function removeLora(path: string) {
    selectedLoras = selectedLoras.filter((l) => l.path !== path);
  }

  function updateLoraMultiplier(path: string, multiplier: number) {
    selectedLoras = selectedLoras.map((l) =>
      l.path === path ? { ...l, multiplier } : l
    );
  }

  function getLoraName(path: string): string {
    return availableLoras.find((l) => l.path === path)?.name ?? path;
  }

  async function generate() {
    const trimmedPrompt = prompt.trim();
    if (!trimmedPrompt || !$selectedModelStore || isGenerating) return;

    await iface.run(async (signal) => {
      if (isSdapi) {
        const [w, h] = $selectedSizeStore.split("x").map(Number);
        const request = {
          model: $selectedModelStore,
          prompt: trimmedPrompt,
          negative_prompt: $sdNegativePromptStore || undefined,
          width: w,
          height: h,
          steps: $sdStepsStore,
          cfg_scale: $sdCfgScaleStore,
          seed: $sdSeedStore,
          batch_size: $sdBatchSizeStore,
          sampler_name: $sdSamplerStore || undefined,
          scheduler: $sdSchedulerStore || undefined,
          lora: selectedLoras.length > 0 ? selectedLoras : undefined,
        };

        const response = await generateSdImage(request, signal);
        if (response.images && response.images.length > 0) {
          generatedImages = response.images.map(
            (img) => `data:image/png;base64,${img}`
          );
        }
      } else {
        const response = await generateImage(
          $selectedModelStore,
          trimmedPrompt,
          $selectedSizeStore,
          signal
        );

        if (response.data && response.data.length > 0) {
          const imageData = response.data[0];
          if (imageData.b64_json) {
            generatedImages = [`data:image/png;base64,${imageData.b64_json}`];
          } else if (imageData.url) {
            generatedImages = [imageData.url];
          }
        }
      }
    });
  }

  function cancelGeneration() {
    iface.cancel();
  }

  function clearImage() {
    generatedImages = [];
    $error = null;
    prompt = "";
  }

  function downloadImage(index: number = 0) {
    const img = generatedImages[index];
    if (!img) return;

    const link = document.createElement("a");
    link.href = img;
    link.download = `generated-image-${Date.now()}-${index}.png`;
    document.body.appendChild(link);
    link.click();
    document.body.removeChild(link);
  }

  function openFullscreen(index: number = 0) {
    fullscreenIndex = index;
    showFullscreen = true;
  }

  function closeFullscreen(event?: MouseEvent) {
    if (event && event.target !== event.currentTarget) {
      return;
    }
    showFullscreen = false;
  }

  const samplePrompts = [
    "A photorealistic portrait of an astronaut on Mars, cinematic lighting, 8k resolution",
    "Cyberpunk city street at night in rain, glowing neon signs, reflections on wet asphalt",
    "Cute fluffy red panda sitting on a bamboo branch, studio lighting, highly detailed",
    "Watercolor landscape of rolling green hills and distant misty mountains at sunrise",
  ];

  let elapsedSeconds = $state(0);
  let timerInterval: ReturnType<typeof setInterval> | null = null;

  $effect(() => {
    if (isGenerating) {
      elapsedSeconds = 0;
      timerInterval = setInterval(() => {
        elapsedSeconds += 1;
      }, 1000);
    } else {
      if (timerInterval) {
        clearInterval(timerInterval);
        timerInterval = null;
      }
    }
    return () => {
      if (timerInterval) clearInterval(timerInterval);
    };
  });

  function handleKeyDown(event: KeyboardEvent) {
    if (isSubmitEnter(event)) {
      event.preventDefault();
      generate();
    }
  }
</script>

<div class="flex flex-col h-full">
  <!-- Model selector and mode toggle -->
  <div class="shrink-0 flex flex-wrap items-center gap-2 mb-3">
    <ModelSelector
      bind:value={$selectedModelStore}
      placeholder={$translate("playground.image.modelPlaceholder")}
      disabled={isGenerating}
      category="image"
      groupLabelKey="playground.modelSelector.groupImage"
      others
    />

    <Select.Root
      type="single"
      value={$apiModeStore}
      onValueChange={(v) => v && apiModeStore.set(v as ImageApiMode)}
      disabled={isGenerating}
    >
      <Select.Trigger class="pg-trigger h-8 w-28 text-xs font-mono">{$apiModeStore}</Select.Trigger>
      <Select.Content>
        <Select.Item value="openai">OpenAI</Select.Item>
        <Select.Item value="sdapi">SDAPI</Select.Item>
      </Select.Content>
    </Select.Root>

    <Select.Root
      type="single"
      value={$selectedSizeStore}
      onValueChange={(v) => v && selectedSizeStore.set(v)}
      disabled={isGenerating}
    >
      <Select.Trigger class="pg-trigger h-8 w-44 text-xs font-mono">
        <span class="truncate">{$selectedSizeStore}</span>
      </Select.Trigger>
      <Select.Content>
        <Select.Group>
          <Select.Label>{$translate("playground.image.square")} (1:1)</Select.Label>
          <Select.Item value="512x512">512x512 (1:1)</Select.Item>
          <Select.Item value="1024x1024">1024x1024 (1:1)</Select.Item>
        </Select.Group>
        <Select.Separator />
        <Select.Group>
          <Select.Label>{$translate("playground.image.landscape")}</Select.Label>
          <Select.Item value="1280x720">1280x720 (16:9)</Select.Item>
          <Select.Item value="1024x768">1024x768 (4:3)</Select.Item>
          <Select.Item value="1792x1024">1792x1024 (SDXL)</Select.Item>
        </Select.Group>
        <Select.Separator />
        <Select.Group>
          <Select.Label>{$translate("playground.image.portrait")}</Select.Label>
          <Select.Item value="720x1280">720x1280 (9:16)</Select.Item>
          <Select.Item value="768x1024">768x1024 (3:4)</Select.Item>
          <Select.Item value="1024x1792">1024x1792 (SDXL)</Select.Item>
        </Select.Group>
      </Select.Content>
    </Select.Root>

    {#if isSdapi}
      <Button
        variant="outline"
        size="sm"
        class="pg-control gap-1.5 h-8 text-xs ml-auto"
        onclick={() => (showSettings = !showSettings)}
      >
        <SlidersHorizontal class="size-3.5" />
        <span>{$translate(showSettings ? "playground.image.hideSettings" : "playground.image.settings")}</span>
      </Button>
    {/if}
  </div>

  <!-- SDAPI Settings Panel -->
  {#if isSdapi && showSettings}
    <div class="pg-panel shrink-0 mb-3 p-3.5 border border-border/60 shadow-xs">
      <div class="grid grid-cols-2 sm:grid-cols-4 gap-3 mb-3">
        <label class="flex flex-col gap-1">
          <span class="text-xs text-muted-foreground font-medium">{$translate("playground.image.steps")}</span>
          <Input
            type="number"
            class="h-8 text-xs"
            bind:value={$sdStepsStore}
            min="1"
            max="150"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-xs text-muted-foreground font-medium">{$translate("playground.image.cfgScale")}</span>
          <Input
            type="number"
            class="h-8 text-xs"
            bind:value={$sdCfgScaleStore}
            min="1"
            max="30"
            step="0.5"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-xs text-muted-foreground font-medium">{$translate("playground.image.seed")}</span>
          <Input
            type="number"
            class="h-8 text-xs"
            bind:value={$sdSeedStore}
            min="-1"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-xs text-muted-foreground font-medium">{$translate("playground.image.batchSize")}</span>
          <Input
            type="number"
            class="h-8 text-xs"
            bind:value={$sdBatchSizeStore}
            min="1"
            max="8"
          />
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-xs text-muted-foreground font-medium">{$translate("playground.image.sampler")}</span>
          <Select.Root
            type="single"
            value={$sdSamplerStore}
            onValueChange={(v) => sdSamplerStore.set(v ?? "")}
          >
            <Select.Trigger class="pg-trigger h-8 text-xs">{$sdSamplerStore || $translate("playground.image.default")}</Select.Trigger>
            <Select.Content>
              <Select.Item value="">{$translate("playground.image.default")}</Select.Item>
              <Select.Item value="euler_a">euler_a</Select.Item>
              <Select.Item value="euler">euler</Select.Item>
              <Select.Item value="heun">heun</Select.Item>
              <Select.Item value="dpm2">dpm2</Select.Item>
              <Select.Item value="dpmpp2s_a">dpmpp2s_a</Select.Item>
              <Select.Item value="dpmpp2m">dpmpp2m</Select.Item>
              <Select.Item value="dpmpp2mv2">dpmpp2mv2</Select.Item>
              <Select.Item value="ipndm">ipndm</Select.Item>
              <Select.Item value="ipndm_v">ipndm_v</Select.Item>
              <Select.Item value="lcm">lcm</Select.Item>
              <Select.Item value="ddim_trailing">ddim_trailing</Select.Item>
              <Select.Item value="tcd">tcd</Select.Item>
            </Select.Content>
          </Select.Root>
        </label>
        <label class="flex flex-col gap-1">
          <span class="text-xs text-muted-foreground font-medium">{$translate("playground.image.scheduler")}</span>
          <Select.Root
            type="single"
            value={$sdSchedulerStore}
            onValueChange={(v) => sdSchedulerStore.set(v ?? "")}
          >
            <Select.Trigger class="pg-trigger h-8 text-xs">{$sdSchedulerStore || $translate("playground.image.autoForModel")}</Select.Trigger>
            <Select.Content>
              <Select.Item value="">{$translate("playground.image.autoForModel")}</Select.Item>
              <Select.Item value="discrete">discrete</Select.Item>
              <Select.Item value="karras">karras</Select.Item>
              <Select.Item value="exponential">exponential</Select.Item>
              <Select.Item value="ays">ays</Select.Item>
              <Select.Item value="gits">gits</Select.Item>
            </Select.Content>
          </Select.Root>
        </label>
      </div>

      <label class="flex flex-col gap-1 mb-3">
        <span class="text-xs text-muted-foreground font-medium">{$translate("playground.image.negativePrompt")}</span>
        <Textarea
          bind:value={$sdNegativePromptStore}
          rows={2}
          class="text-xs resize-none"
          placeholder={$translate("playground.image.negativePromptPlaceholder")}
        ></Textarea>
      </label>

      <!-- LoRA Selection -->
      <div>
        <span class="text-xs text-muted-foreground font-medium block mb-1">{$translate("playground.image.loras")}</span>
        <div class="flex items-center gap-2 mb-2">
          <Button
            variant="outline"
            class="pg-control text-xs h-8"
            onclick={loadLoras}
            disabled={!$selectedModelStore || isLoadingLoras}
          >
            {isLoadingLoras ? $translate("playground.image.loading") : lorasLoaded ? $translate("playground.image.reloadLoras") : $translate("playground.image.loadLoras")}
          </Button>
          {#if lorasLoaded && availableLoras.length > 0}
            <Select.Root
              type="single"
              value=""
              onValueChange={(v) => {
                if (v) {
                  const lora = availableLoras.find((l) => l.path === v);
                  if (lora && !selectedLoras.some((s) => s.path === v)) {
                    selectedLoras = [...selectedLoras, { path: lora.path, multiplier: 1.0 }];
                  }
                }
              }}
            >
              <Select.Trigger class="pg-trigger h-8 flex-1 text-xs">{$translate("playground.image.addLora")}</Select.Trigger>
              <Select.Content>
                {#each availableLoras.filter((l) => !selectedLoras.some((s) => s.path === l.path)) as lora (lora.path)}
                  <Select.Item value={lora.path}>{lora.name}</Select.Item>
                {/each}
              </Select.Content>
            </Select.Root>
          {/if}
        </div>
        {#if loraError}
          <p class="text-xs text-destructive mb-1">{loraError}</p>
        {/if}
        {#if lorasLoaded && availableLoras.length === 0}
          <p class="text-xs text-muted-foreground">{$translate("playground.image.noLoras")}</p>
        {/if}
        {#if selectedLoras.length > 0}
          <div class="flex flex-col gap-1.5 pt-1">
            {#each selectedLoras as lora}
              <div class="flex items-center gap-2 text-xs p-1.5 rounded-md border border-border/60 bg-muted/20">
                <span class="flex-1 truncate font-medium">{getLoraName(lora.path)}</span>
                <span class="text-[11px] text-muted-foreground">×</span>
                <Input
                  type="number"
                  class="h-7 w-20 text-xs tabular-nums"
                  value={lora.multiplier}
                  oninput={(e) => updateLoraMultiplier(lora.path, parseFloat((e.target as HTMLInputElement).value) || 1)}
                  min="0"
                  max="2"
                  step="0.1"
                />
                <Button
                  variant="ghost"
                  size="icon-xs"
                  class="size-7 text-muted-foreground hover:text-destructive"
                  onclick={() => removeLora(lora.path)}
                  aria-label={$translate("playground.image.removeLora")}
                >
                  <X class="size-3" />
                </Button>
              </div>
            {/each}
          </div>
        {/if}
      </div>
    </div>
  {/if}

  <!-- Empty state for no models configured -->
  {#if !$hasListedModels}
    <EmptyState message={$translate("playground.image.noModels")} />
  {:else}
    <!-- Image display area -->
    <div class="pg-panel flex-1 overflow-auto mb-3 flex items-center justify-center border border-border/50 relative bg-muted/10 p-2">
      {#if isGenerating}
        <div class="flex flex-col items-center justify-center gap-3 text-center text-muted-foreground p-6">
          <div class="relative flex size-12 items-center justify-center">
            <div class="absolute inline-flex size-full animate-spin rounded-full border-2 border-primary border-t-transparent"></div>
            <Sparkles class="size-5 text-primary" />
          </div>
          <div>
            <p class="font-medium text-foreground text-sm">{$translate("playground.image.generating")}</p>
            <p class="text-xs text-muted-foreground mt-0.5">{elapsedSeconds}s</p>
          </div>
        </div>
      {:else if $error}
        <div class="text-center text-destructive p-4 max-w-md rounded-xl bg-destructive/10 border border-destructive/20">
          <p class="font-medium text-sm">{$translate("common.error")}</p>
          <p class="text-xs mt-1 leading-relaxed">{$error}</p>
        </div>
      {:else if generatedImages.length > 1}
        <div class="grid grid-cols-2 gap-3 p-2 w-full h-full overflow-auto">
          {#each generatedImages as img, i}
            <div class="group relative flex items-center justify-center rounded-xl overflow-hidden border border-border/60 bg-card shadow-xs">
              <button
                class="p-0 border-0 bg-transparent cursor-pointer w-full h-full flex items-center justify-center"
                onclick={() => openFullscreen(i)}
                aria-label={$translate("playground.image.viewFullscreen")}
              >
                <img
                  src={img}
                  alt={$translate("playground.image.alt", { index: i + 1 })}
                  class="max-w-full max-h-full object-contain transition-transform duration-200 group-hover:scale-[1.01]"
                />
              </button>
              <div class="absolute bottom-2 right-2 flex items-center gap-1.5 opacity-90 transition-opacity">
                <Button
                  variant="secondary"
                  size="icon-xs"
                  class="size-8 rounded-lg bg-black/70 hover:bg-black/90 text-white shadow-sm"
                  onclick={() => openFullscreen(i)}
                  title={$translate("playground.image.viewFullscreen")}
                >
                  <ZoomIn class="size-3.5" />
                </Button>
                <Button
                  variant="secondary"
                  size="icon-xs"
                  class="size-8 rounded-lg bg-black/70 hover:bg-black/90 text-white shadow-sm"
                  onclick={(e) => { e.stopPropagation(); downloadImage(i); }}
                  aria-label={$translate("playground.image.download")}
                  title={$translate("playground.image.download")}
                >
                  <Download class="size-3.5" />
                </Button>
              </div>
            </div>
          {/each}
        </div>
      {:else if generatedImages.length === 1}
        <div class="group relative max-w-full max-h-full flex items-center justify-center rounded-xl overflow-hidden">
          <button
            class="p-0 border-0 bg-transparent cursor-pointer flex items-center justify-center"
            onclick={() => openFullscreen(0)}
            aria-label={$translate("playground.image.viewFullscreen")}
          >
            <img
              src={generatedImages[0]}
              alt={$translate("playground.image.alt", { index: 1 })}
              class="max-w-full max-h-full object-contain rounded-lg shadow-sm transition-opacity hover:opacity-95"
            />
          </button>
          <div class="absolute bottom-3 right-3 flex items-center gap-1.5 opacity-0 group-hover:opacity-100 transition-opacity">
            <Button
              variant="secondary"
              size="icon-sm"
              class="rounded-lg bg-black/70 hover:bg-black/90 text-white shadow-sm"
              onclick={() => openFullscreen(0)}
              title={$translate("playground.image.viewFullscreen")}
            >
              <ZoomIn class="size-4" />
            </Button>
            <Button
              variant="secondary"
              size="icon-sm"
              class="rounded-lg bg-black/70 hover:bg-black/90 text-white shadow-sm"
              onclick={(e) => { e.stopPropagation(); downloadImage(0); }}
              aria-label={$translate("playground.image.download")}
              title={$translate("playground.image.download")}
            >
              <Download class="size-4" />
            </Button>
          </div>
        </div>
      {:else}
        <div class="flex h-full flex-col items-center justify-center gap-3 text-center px-4 py-8 max-w-lg mx-auto">
          <div class="pg-inset flex size-12 items-center justify-center rounded-2xl border border-primary/10 bg-primary/5">
            <Sparkles class="size-6 text-primary" />
          </div>
          <div class="space-y-1">
            <p class="text-sm font-semibold">{$translate("playground.image.empty")}</p>
            <p class="pg-hint text-xs">{$translate("playground.image.promptPlaceholder")}</p>
          </div>
          <div class="flex flex-wrap justify-center gap-1.5 pt-2">
            {#each samplePrompts as sample}
              <button
                type="button"
                class="text-xs text-left line-clamp-1 max-w-[260px] rounded-lg border border-border/60 bg-card/80 px-2.5 py-1.5 text-muted-foreground hover:text-foreground hover:border-primary/40 hover:bg-muted/40 transition-colors cursor-pointer"
                onclick={() => (prompt = sample)}
                title={sample}
              >
                {sample}
              </button>
            {/each}
          </div>
        </div>
      {/if}
    </div>

    <!-- Prompt input area -->
    <div class="shrink-0 flex flex-col md:flex-row gap-2">
      <ExpandableTextarea
        bind:value={prompt}
        placeholder={$translate("playground.image.promptPlaceholder")}
        rows={2}
        onkeydown={handleKeyDown}
        disabled={isGenerating || !$selectedModelStore}
      />
      <div class="flex flex-row md:flex-col gap-2 shrink-0">
        {#if isGenerating}
          <Button variant="outline" class="pg-control pg-control--danger flex-1 md:flex-none gap-1.5" onclick={cancelGeneration}>
            <X class="size-3.5" />
            {$translate("common.cancel")}
          </Button>
        {:else}
          <Button
            class="pg-action flex-1 md:flex-none gap-1.5"
            onclick={generate}
            disabled={!prompt.trim() || !$selectedModelStore}
          >
            <Sparkles class="size-3.5" />
            {$translate("playground.image.generate")}
          </Button>
          <Button
            variant="outline"
            class="pg-control flex-1 md:flex-none gap-1.5"
            onclick={clearImage}
            disabled={generatedImages.length === 0 && !$error && !prompt.trim()}
          >
            <Trash2 class="size-3.5" />
            {$translate("common.clear")}
          </Button>
        {/if}
      </div>
    </div>
  {/if}
</div>

<!-- Fullscreen dialog -->
{#if showFullscreen && generatedImages[fullscreenIndex]}
  <div
    class="fixed inset-0 bg-black/90 z-50 flex items-center justify-center p-4"
    onclick={(e) => closeFullscreen(e)}
    onkeydown={(e) => e.key === 'Escape' && closeFullscreen()}
    role="dialog"
    aria-modal="true"
    tabindex="-1"
  >
    <Button
      variant="secondary"
      size="icon"
      class="absolute top-4 right-4 bg-black/60 hover:bg-black/80 text-white"
      onclick={() => closeFullscreen()}
      aria-label={$translate("playground.image.closeFullscreen")}
    >
      <X class="size-6" />
    </Button>
    <img
      src={generatedImages[fullscreenIndex]}
      alt={$translate("playground.image.alt", { index: fullscreenIndex + 1 })}
      class="max-w-full max-h-full object-contain pointer-events-none"
    />
  </div>
{/if}
