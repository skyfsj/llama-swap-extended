<script lang="ts">
  import { hasListedModels } from "../../stores/api";
  import { persistentStore } from "../../stores/persistent";
  import { streamChatCompletion, type Endpoint } from "../../lib/chatApi";
  import { playgroundStores } from "../../stores/playgroundActivity";
  import type { ChatMessage, ContentPart } from "../../lib/types";
  import { isSubmitEnter } from "../../lib/ime";
  import ChatMessageComponent from "./ChatMessage.svelte";
  import ModelSelector from "./ModelSelector.svelte";
  import ExpandableTextarea from "./ExpandableTextarea.svelte";
  import EmptyState from "../EmptyState.svelte";
  import { Settings, Paperclip, Send, Maximize2, MessagesSquare } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { Textarea } from "$lib/components/ui/textarea/index.js";
  import { Label } from "$lib/components/ui/label/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { X } from "@lucide/svelte";
  import { t, translate } from "../../lib/i18n";

  const selectedModelStore = persistentStore<string>("playground-selected-model", "");
  const systemPromptStore = persistentStore<string>("playground-system-prompt", "");
  const temperatureStore = persistentStore<number>("playground-temperature", 0.7);
  const endpointStore = persistentStore<Endpoint>("playground-endpoint", "v1/chat/completions");
  const maxTokensStore = persistentStore<number>("playground-max-tokens", 4096);

  function loadMessages(): ChatMessage[] {
    try {
      const saved = localStorage.getItem("playground-messages");
      return saved ? JSON.parse(saved) : [];
    } catch {
      return [];
    }
  }

  let messages = $state<ChatMessage[]>(loadMessages());
  let userInput = $state("");
  let isStreaming = $state(false);
  let isReasoning = $state(false);
  let reasoningStartTime = $state<number>(0);
  let abortController = $state<AbortController | null>(null);
  let messagesContainer: HTMLDivElement | undefined = $state();
  let inputRef: HTMLTextAreaElement | null = $state(null);
  let showSettings = $state(false);
  let attachedImages = $state<string[]>([]);
  let fileInput = $state<HTMLInputElement | null>(null);
  let imageError = $state<string | null>(null);

  let userScrolledUp = $state(false);

  $effect(() => {
    playgroundStores.chatStreaming.set(isStreaming);
  });

  let wasStreaming = $state(false);
  $effect(() => {
    if (wasStreaming && !isStreaming) {
      inputRef?.focus();
    }
    wasStreaming = isStreaming;
  });

  function handleMessagesScroll() {
    if (!messagesContainer) return;
    const { scrollTop, scrollHeight, clientHeight } = messagesContainer;
    // Consider "at bottom" if within 40px of the bottom
    userScrolledUp = scrollHeight - scrollTop - clientHeight > 40;
  }

  // Auto-scroll when messages change — skip if user scrolled up
  $effect(() => {
    if (messages.length > 0 && messagesContainer && !userScrolledUp) {
      messagesContainer.scrollTo({
        top: messagesContainer.scrollHeight,
        behavior: isStreaming ? "instant" : "smooth",
      });
    }
  });

  // Persist messages to localStorage (throttled to once per 2s)
  let lastSaveTime = 0;
  $effect(() => {
    const json = JSON.stringify(messages);
    const elapsed = Date.now() - lastSaveTime;
    const save = () => {
      try { localStorage.setItem("playground-messages", json); } catch {}
      lastSaveTime = Date.now();
    };
    if (elapsed >= 2000) {
      save();
      return;
    }
    const timer = setTimeout(save, 2000 - elapsed);
    return () => clearTimeout(timer);
  });

  async function sendMessage() {
    const trimmedInput = userInput.trim();
    if ((!trimmedInput && attachedImages.length === 0) || !$selectedModelStore || isStreaming) return;

    userScrolledUp = false;

    // Build message content (multimodal if images attached)
    let content: string | ContentPart[];
    if (attachedImages.length > 0) {
      const parts: ContentPart[] = [];
      if (trimmedInput) {
        parts.push({ type: "text", text: trimmedInput });
      }
      for (const url of attachedImages) {
        parts.push({ type: "image_url", image_url: { url } });
      }
      content = parts;
    } else {
      content = trimmedInput;
    }

    // Add user message
    messages = [...messages, { role: "user", content }];
    userInput = "";
    attachedImages = [];
    imageError = null;

    // Generate response from the new user message
    await regenerateFromIndex(messages.length - 1);
  }

  function cancelStreaming() {
    abortController?.abort();
  }

  function newChat() {
    if (isStreaming) {
      cancelStreaming();
    }
    messages = [];
    isReasoning = false;
    reasoningStartTime = 0;
  }

  async function regenerateFromIndex(idx: number) {
    // Remove all messages after the edited user message
    messages = messages.slice(0, idx + 1);

    // Add empty assistant message for the new response
    messages = [...messages, { role: "assistant", content: "" }];

    isStreaming = true;
    isReasoning = false;
    reasoningStartTime = 0;
    abortController = new AbortController();

    try {
      // Build messages array with optional system prompt
      const apiMessages: ChatMessage[] = [];
      if ($systemPromptStore.trim()) {
        apiMessages.push({ role: "system", content: $systemPromptStore.trim() });
      }
      apiMessages.push(...messages.slice(0, -1)); // Add all messages except the empty assistant one

      const stream = streamChatCompletion(
        $selectedModelStore,
        apiMessages,
        abortController.signal,
        {
          temperature: $temperatureStore,
          endpoint: $endpointStore,
          max_tokens: $maxTokensStore,
          // OpenAI-compatible streaming backends (including vLLM) only append
          // final token usage when this is requested. Without it, the activity
          // log has no authoritative prompt/completion counts for Playground
          // conversations.
          include_usage: $endpointStore === "v1/chat/completions",
        }
      );

      for await (const chunk of stream) {
        if (chunk.done) break;

        // Handle reasoning content
        if (chunk.reasoning_content) {
          // Start timing on first reasoning content
          if (!isReasoning) {
            isReasoning = true;
            reasoningStartTime = Date.now();
          }

          // Update the last message with reasoning content
          messages = messages.map((msg, i) =>
            i === messages.length - 1
              ? { ...msg, reasoning_content: (msg.reasoning_content || "") + chunk.reasoning_content }
              : msg
          );
        }

        // Handle regular content - end reasoning phase when we get content
        if (chunk.content) {
          if (isReasoning) {
            // Calculate reasoning time
            const reasoningTimeMs = Date.now() - reasoningStartTime;
            isReasoning = false;

            // Update message with reasoning time
            messages = messages.map((msg, i) =>
              i === messages.length - 1
                ? { ...msg, reasoningTimeMs }
                : msg
            );
          }

          // Update the last message (assistant) with new content
          messages = messages.map((msg, i) =>
            i === messages.length - 1
              ? { ...msg, content: msg.content + chunk.content }
              : msg
          );
        }
      }
    } catch (error) {
      if (error instanceof Error && error.name === "AbortError") {
        // User cancelled, keep partial response
        // If we were still reasoning, record the time
        if (isReasoning && reasoningStartTime > 0) {
          const reasoningTimeMs = Date.now() - reasoningStartTime;
          messages = messages.map((msg, i) =>
            i === messages.length - 1
              ? { ...msg, reasoningTimeMs }
              : msg
          );
        }
      } else {
        // Show error in the assistant message
        const errorMessage = error instanceof Error ? error.message : t("errors.anErrorOccurred");
        messages = messages.map((msg, i) =>
          i === messages.length - 1
              ? { ...msg, content: msg.content + `\n\n**${t("common.error")}:** ${errorMessage}` }
            : msg
        );
      }
    } finally {
      isStreaming = false;
      isReasoning = false;
      abortController = null;
    }
  }

  async function editMessage(idx: number, newContent: string) {
    if (isStreaming || !$selectedModelStore) return;

    // Update the user message at the specified index
    messages = messages.map((msg, i) =>
      i === idx ? { ...msg, content: newContent } : msg
    );

    // Trigger a new chat request with the updated messages
    await regenerateFromIndex(idx);
  }

  function handleKeyDown(event: KeyboardEvent) {
    if (isSubmitEnter(event)) {
      event.preventDefault();
      sendMessage();
    }
  }

  const ACCEPTED_IMAGE_FORMATS = ["image/jpeg", "image/png", "image/gif", "image/webp"];
  const MAX_IMAGE_SIZE = 20 * 1024 * 1024; // 20MB
  const MAX_IMAGES_PER_MESSAGE = 5;

  function validateImageFile(file: File): string | null {
    if (!ACCEPTED_IMAGE_FORMATS.includes(file.type)) {
      return t("errors.invalidImageType", { type: file.type });
    }
    if (file.size > MAX_IMAGE_SIZE) {
      return t("errors.fileTooLarge", { size: (file.size / 1024 / 1024).toFixed(1) });
    }
    return null;
  }

  function fileToDataUrl(file: File): Promise<string> {
    return new Promise((resolve, reject) => {
      const reader = new FileReader();
      reader.onload = () => resolve(reader.result as string);
      reader.onerror = () => reject(new Error(t("errors.failedReadFile")));
      reader.readAsDataURL(file);
    });
  }

  async function processImageFiles(files: File[]): Promise<void> {
    imageError = null;

    if (attachedImages.length + files.length > MAX_IMAGES_PER_MESSAGE) {
      imageError = t("errors.maximumImages", { count: MAX_IMAGES_PER_MESSAGE });
      return;
    }

    for (const file of files) {
      const error = validateImageFile(file);
      if (error) {
        imageError = error;
        return;
      }
    }

    try {
      const dataUrls = await Promise.all(files.map(fileToDataUrl));
      attachedImages = [...attachedImages, ...dataUrls];
    } catch (error) {
      imageError = error instanceof Error ? error.message : t("errors.failedProcessImages");
    }
  }

  function handleImageSelect(event: Event) {
    const input = event.target as HTMLInputElement;
    if (input.files && input.files.length > 0) {
      processImageFiles(Array.from(input.files));
    }
    // Reset the input so the same file can be selected again
    input.value = "";
  }

  function removeImage(idx: number) {
    attachedImages = attachedImages.filter((_, i) => i !== idx);
    imageError = null;
  }
</script>

<div class="flex flex-col h-full">
  <!-- Model selector and controls -->
  <div class="shrink-0 flex flex-wrap items-center gap-2 mb-3">
    <ModelSelector bind:value={$selectedModelStore} disabled={isStreaming} category="chat" />
    <div class="flex items-center gap-1.5 ml-auto">
      <Button variant="ghost" size="icon" class="pg-tool" onclick={() => (showSettings = true)} title={$translate("common.settings")}>
        <Settings />
      </Button>
      <Button variant="outline" class="pg-control" onclick={newChat} disabled={messages.length === 0 && !isStreaming}>
        {$translate("playground.chat.newChat")}
      </Button>
    </div>
  </div>

  <!-- Settings dialog -->
  <Dialog.Root bind:open={showSettings}>
    <Dialog.Content class="pg-float max-w-xl">
      <Dialog.Header>
        <Dialog.Title>{$translate("playground.chat.settings")}</Dialog.Title>
      </Dialog.Header>

      <div class="space-y-4">
        <div>
          <Label class="mb-1" for="endpoint">{$translate("playground.chat.endpoint")}</Label>
          <Select.Root
            type="single"
            value={$endpointStore}
            onValueChange={(v) => v && endpointStore.set(v as Endpoint)}
          >
            <Select.Trigger class="pg-trigger w-full">/{$endpointStore}</Select.Trigger>
            <Select.Content>
              <Select.Item value="v1/chat/completions">/v1/chat/completions</Select.Item>
              <Select.Item value="v1/messages">/v1/messages</Select.Item>
              <Select.Item value="v1/responses">/v1/responses</Select.Item>
            </Select.Content>
          </Select.Root>
        </div>
        <div>
          <Label class="mb-1" for="system-prompt">{$translate("playground.chat.systemPrompt")}</Label>
          <Textarea
            id="system-prompt"
            class="resize-none"
            placeholder={$translate("playground.chat.systemPromptPlaceholder")}
            rows={3}
            bind:value={$systemPromptStore}
            disabled={isStreaming}
          />
        </div>
        <div>
          <Label class="mb-1" for="temperature">
            {$translate("playground.chat.temperature")}: {$temperatureStore.toFixed(2)}
          </Label>
          <input
            id="temperature"
            type="range"
            min="0"
            max="2"
            step="0.05"
            class="accent-primary w-full"
            bind:value={$temperatureStore}
            disabled={isStreaming}
          />
          <div class="text-muted-foreground mt-1 flex justify-between text-xs">
            <span>{$translate("playground.chat.precise")}</span>
            <span>{$translate("playground.chat.creative")}</span>
          </div>
        </div>
        <div>
          <Label class="mb-1" for="max-tokens">{$translate("playground.chat.maxTokens")}</Label>
          <Input id="max-tokens" type="number" min="1" bind:value={$maxTokensStore} disabled={isStreaming} />
          <p class="text-muted-foreground mt-1 text-xs">{$translate("playground.chat.requiredForMessages")}</p>
        </div>
      </div>

      <Dialog.Footer>
        <Button variant="outline" class="pg-control" onclick={() => (showSettings = false)}>{$translate("common.done")}</Button>
      </Dialog.Footer>
    </Dialog.Content>
  </Dialog.Root>

  <!-- Empty state for no models configured -->
  {#if !$hasListedModels}
    <EmptyState message={$translate("playground.chat.noModels")} />
  {:else}
    <!-- Messages area -->
    <div
      class="mb-3 flex-1 overflow-y-auto pg-scroll px-1"
      bind:this={messagesContainer}
      onscroll={handleMessagesScroll}
    >
      {#if messages.length === 0}
        <div class="flex h-full flex-col items-center justify-center gap-3 text-center">
          <div class="pg-inset flex size-11 items-center justify-center" style="border-radius: 99px">
            <MessagesSquare class="size-5" style="color: var(--pg-ink-3)" />
          </div>
          <p class="pg-hint max-w-xs leading-relaxed">{$translate("playground.chat.emptyConversation")}</p>
        </div>
      {:else}
        {#each messages as message, idx (idx)}
          <ChatMessageComponent
            role={message.role}
            content={message.content}
            reasoning_content={message.reasoning_content}
            reasoningTimeMs={message.reasoningTimeMs}
            isStreaming={isStreaming && idx === messages.length - 1 && message.role === "assistant"}
            isReasoning={isReasoning && idx === messages.length - 1 && message.role === "assistant"}
            onEdit={message.role === "user" ? (newContent) => editMessage(idx, newContent) : undefined}
            onRegenerate={message.role === "assistant" && idx > 0 && messages[idx - 1].role === "user"
              ? () => regenerateFromIndex(idx - 1)
              : undefined}
          />
        {/each}
      {/if}
    </div>

    <!-- Input area -->
    <div class="shrink-0">
      <!-- Image preview strip -->
      {#if attachedImages.length > 0}
        <div class="mb-2 flex flex-wrap gap-2">
          {#each attachedImages as imageUrl, idx (idx)}
            <div class="group relative">
              <img
                src={imageUrl}
                alt={$translate("playground.chat.attachedImage", { index: idx + 1 })}
                class="size-16 rounded-[var(--pg-r-control)] border border-black/10 object-cover dark:border-white/10"
              />
              <Button
                variant="destructive"
                size="icon-xs"
                class="absolute -right-1.5 -top-1.5 size-5 rounded-full opacity-0 transition-opacity group-hover:opacity-100"
                onclick={() => removeImage(idx)}
                title={$translate("playground.chat.removeImage")}
              >
                <X class="size-3" />
              </Button>
            </div>
          {/each}
        </div>
      {/if}

      <!-- Error message -->
      {#if imageError}
        <div class="pg-inset-soft text-destructive mb-2 px-3 py-2 text-[13px]">
          {imageError}
        </div>
      {/if}

      <div class="pg-composer flex flex-col gap-1 p-1.5">
        <!-- Hidden file input -->
        <input
          type="file"
          accept=".jpg,.jpeg,.png,.gif,.webp"
          multiple
          class="hidden"
          bind:this={fileInput}
          onchange={handleImageSelect}
        />

        <ExpandableTextarea
          bare
          bind:ref={inputRef}
          bind:value={userInput}
          placeholder={$translate("playground.chat.inputPlaceholder")}
          rows={2}
          onkeydown={handleKeyDown}
          disabled={isStreaming || !$selectedModelStore}
        >
          {#snippet toolbar({ expand })}
            <Button
              variant="outline"
              size="icon-sm"
              class="pg-tool"
              onclick={() => fileInput?.click()}
              disabled={isStreaming || !$selectedModelStore}
              title={$translate("playground.chat.attachImage")}
            >
              <Paperclip />
            </Button>
            <Button
              variant="ghost"
              size="icon-sm"
              class="pg-tool"
              onclick={expand}
              disabled={isStreaming || !$selectedModelStore}
              title={$translate("playground.expandable.expandToEdit")}
            >
              <Maximize2 />
            </Button>
            {#if $selectedModelStore}
              <span class="pg-chip ml-1 hidden max-w-[14rem] truncate sm:inline-flex" title={$selectedModelStore}>
                {$selectedModelStore}
              </span>
            {/if}
            <div class="ml-auto flex items-center gap-1.5">
              {#if isStreaming}
                <Button variant="outline" class="pg-control pg-control--danger" onclick={cancelStreaming}>
                  <X />
                  {$translate("common.cancel")}
                </Button>
              {:else}
                <Button
                  class="pg-action"
                  onclick={sendMessage}
                  disabled={(!userInput.trim() && attachedImages.length === 0) || !$selectedModelStore}
                  title={$translate("playground.chat.send")}
                >
                  <Send />
                  {$translate("playground.chat.send")}
                </Button>
              {/if}
            </div>
          {/snippet}
        </ExpandableTextarea>
      </div>
    </div>
  {/if}
</div>
