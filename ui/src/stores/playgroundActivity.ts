import { writable, derived } from "svelte/store";

const chatStreaming = writable(false);
const imageGenerating = writable(false);
const translationGenerating = writable(false);

export const playgroundActivity = derived(
  [chatStreaming, imageGenerating, translationGenerating],
  ([$chat, $image, $translation]) => $chat || $image || $translation
);

export const playgroundStores = {
  chatStreaming,
  imageGenerating,
  translationGenerating,
};
