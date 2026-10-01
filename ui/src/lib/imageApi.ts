import type { ImageGenerationRequest, ImageGenerationResponse } from "./types";
import { playgroundSessionHeaders } from "./playgroundSession";
import { t } from "./i18n";

export async function generateImage(
  model: string,
  prompt: string,
  size: string,
  signal?: AbortSignal
): Promise<ImageGenerationResponse> {
  const request: ImageGenerationRequest = {
    model,
    prompt,
    n: 1,
    size,
  };

  const response = await fetch("/v1/images/generations", {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      ...playgroundSessionHeaders,
    },
    body: JSON.stringify(request),
    signal,
  });

  if (!response.ok) {
    const errorText = await response.text();
    throw new Error(t("errors.imageApi", { status: response.status, detail: errorText }));
  }

  return response.json();
}
