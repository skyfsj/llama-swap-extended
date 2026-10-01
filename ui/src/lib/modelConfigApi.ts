import { errorMessageFromPayload } from "./apiError";

export interface ModelConfigDeleteErrorPayload {
  error?: string | { message?: string };
  model_files?: string[];
  registered?: string[];
  in_use?: string[];
}

export class ModelConfigRequestError extends Error {
  readonly status: number;
  readonly payload?: ModelConfigDeleteErrorPayload;

  constructor(message: string, status: number, payload?: ModelConfigDeleteErrorPayload) {
    super(message);
    this.name = "ModelConfigRequestError";
    this.status = status;
    this.payload = payload;
  }
}

export async function deleteModelConfig(
  modelId: string,
  etag: string,
  deleteModelFile: boolean,
): Promise<Record<string, unknown>> {
  const response = await fetch(`/api/config/models/${encodeURIComponent(modelId)}`, {
    method: "DELETE",
    headers: { "Content-Type": "application/json", "If-Match": etag },
    body: JSON.stringify({ delete_model_file: deleteModelFile }),
  });
  const payload = (await response.json().catch(() => ({}))) as Record<string, unknown> & ModelConfigDeleteErrorPayload;
  if (!response.ok) {
    throw new ModelConfigRequestError(
      errorMessageFromPayload(payload, `Unable to delete model configuration (HTTP ${response.status})`),
      response.status,
      payload,
    );
  }
  return payload;
}
