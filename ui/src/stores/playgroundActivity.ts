import { writable, derived } from "svelte/store";

const chatStreaming = writable(false);
const imageGenerating = writable(false);
const translationGenerating = writable(false);
const concurrencyRunning = writable(false);

export const playgroundActivity = derived(
  [chatStreaming, imageGenerating, translationGenerating, concurrencyRunning],
  ([$chat, $image, $translation, $concurrency]) =>
    $chat || $image || $translation || $concurrency
);

export const playgroundStores = {
  chatStreaming,
  imageGenerating,
  translationGenerating,
  concurrencyRunning,
};

