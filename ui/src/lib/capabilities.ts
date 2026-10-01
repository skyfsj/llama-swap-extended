import type { Model } from "./types";

// Canonical capability key -> locale message key. The JSON catalogs own all
// human-readable labels; this map only describes the domain-to-locale link.
export const capabilityMessageKeys: Record<string, string> = {
  vision: "capabilities.vision",
  audio_transcriptions: "capabilities.audioTranscriptions",
  audio_speech: "capabilities.audioSpeech",
  image_generation: "capabilities.imageGeneration",
  image_to_image: "capabilities.imageToImage",
  function_calling: "capabilities.functionCalling",
  reranker: "capabilities.reranker",
  context: "capabilities.context",
};

export interface CapabilityBadge {
  key: string;
  label?: string;
}

// Formats a token count as a compact context-window badge. Uses decimal units
// (128000 -> "128K", 1000000 -> "1M") to match the conventional "128K context"
// wording used by llama.cpp and OpenAI-compatible listings.
export function formatContextLength(tokens: number): string {
  if (tokens <= 0) return "";
  if (tokens >= 1_000_000) {
    const m = tokens / 1_000_000;
    return Number.isInteger(m) ? `${m}M` : `${m.toFixed(1)}M`;
  }
  if (tokens >= 1_000) {
    const k = tokens / 1_000;
    return Number.isInteger(k) ? `${k}K` : `${Math.round(k)}K`;
  }
  return String(tokens);
}

// Muted pastel background/text classes per capability badge key, so each is
// visually distinct on the Models list. Low-opacity backgrounds keep the look
// soft; the 700/300 text shades keep contrast in light and dark mode.
export const capabilityBadgeClass: Record<string, string> = {
  vision: "bg-violet-500/15 text-violet-700 dark:text-violet-300",
  audio_transcriptions: "bg-amber-500/15 text-amber-700 dark:text-amber-300",
  audio_speech: "bg-rose-500/15 text-rose-700 dark:text-rose-300",
  image_generation: "bg-fuchsia-500/15 text-fuchsia-700 dark:text-fuchsia-300",
  image_to_image: "bg-orange-500/15 text-orange-700 dark:text-orange-300",
  function_calling: "bg-emerald-500/15 text-emerald-700 dark:text-emerald-300",
  reranker: "bg-indigo-500/15 text-indigo-700 dark:text-indigo-300",
  context: "bg-sky-500/15 text-sky-700 dark:text-sky-300",
};

// Returns the capability badges for a Models list row: every reported
// capability in canonical order (mirroring the Details tab tag set), with the
// context window last so it sits on the right of the group.
export function listCapabilityBadges(
  model: Pick<Model, "capabilities" | "context_length">,
): CapabilityBadge[] {
  const badges: CapabilityBadge[] = [];

  const caps = model.capabilities ?? {};
  for (const key of Object.keys(capabilityMessageKeys).filter((key) => key !== "context")) {
    if (caps[key as keyof Model["capabilities"]]) {
      badges.push({ key });
    }
  }

  const ctx = model.context_length ?? 0;
  if (ctx > 0) {
    badges.push({ key: "context", label: formatContextLength(ctx) });
  }

  return badges;
}
