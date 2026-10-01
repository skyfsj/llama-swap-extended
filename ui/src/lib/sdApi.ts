import type { SdApiTxt2ImgRequest, SdApiResponse, SdApiLora } from "./types";
import { playgroundSessionHeaders } from "./playgroundSession";
import { t } from "./i18n";

export async function generateSdImage(
  request: SdApiTxt2ImgRequest,
  signal?: AbortSignal
): Promise<SdApiResponse> {
  const response = await fetch("/sdapi/v1/txt2img", {
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
    throw new Error(t("errors.sdApi", { status: response.status, detail: errorText }));
  }

  return response.json();
}

export async function fetchSdLoras(
  model: string,
  signal?: AbortSignal
): Promise<SdApiLora[]> {
  const response = await fetch(
    `/sdapi/v1/loras?model=${encodeURIComponent(model)}`,
    { headers: playgroundSessionHeaders, signal }
  );

  if (!response.ok) {
    const errorText = await response.text();
    throw new Error(t("errors.sdApiLoras", { status: response.status, detail: errorText }));
  }

  return response.json();
}
