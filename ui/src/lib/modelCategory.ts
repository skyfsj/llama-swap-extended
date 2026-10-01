import type { Model } from "./types";

/**
 * Model categories drive which playground tool a model belongs to. A model
 * declares its capabilities in llama-swap's config (`capabilities:` block) and
 * the server renders them into /v1/models; everything without a
 * modality/feature declaration is a plain chat model.
 */
export type ModelCategory =
  | "chat"
  | "image"
  | "speech"
  | "transcription"
  | "rerank"
  | "translation";

export const modelCategories: ModelCategory[] = [
  "chat",
  "image",
  "speech",
  "transcription",
  "rerank",
  "translation",
];

export function categoryOf(model: Model): ModelCategory {
  const caps = model.capabilities;
  if (!caps) return "chat";
  if (caps.translation) return "translation";
  if (caps.reranker) return "rerank";
  if (caps.image_generation || caps.image_to_image) return "image";
  if (caps.audio_speech) return "speech";
  if (caps.audio_transcriptions) return "transcription";
  return "chat";
}

export function inCategory(model: Model, category: ModelCategory): boolean {
  return categoryOf(model) === category;
}

export function filterByCategory(models: Model[], category: ModelCategory): Model[] {
  return models.filter((m) => inCategory(m, category));
}
