import type { ModelDownload, ModelDownloadsResponse, ModelFileSource } from "./types";

export type ModelDownloadProvider = "huggingface" | "modelscope";

export interface ModelDownloadInput {
  provider: ModelDownloadProvider;
  repo_id: string;
  revision?: string;
  source_id?: string;
  include?: string[];
  exclude?: string[];
}

export interface ModelDownloadErrorPayload {
  error?: string | { message?: string };
}

export interface ModelDownloadCredentialStatus {
  configured: boolean;
  environmentConfigured: boolean;
  environmentVariable: string;
}

export interface ModelDownloadCredentials {
  etag: string;
  writable: boolean;
  huggingface: ModelDownloadCredentialStatus;
  modelscope: ModelDownloadCredentialStatus;
}

export interface ModelDownloadCredentialsPatch {
  hfToken?: string;
  modelScopeToken?: string;
}

export class ModelDownloadRequestError extends Error {
  readonly status: number;

  constructor(message: string, status: number) {
    super(message);
    this.name = "ModelDownloadRequestError";
    this.status = status;
  }
}

// The server answers with an OpenAI-compatible envelope where "error" is an
// object ({message, type, code}); some older builds still return a plain
// string. Extract the human message from either shape so the UI never renders
// a raw "[object Object]".
async function readError(response: Response, fallback: string): Promise<ModelDownloadRequestError> {
  const payload = (await response.json().catch(() => ({}))) as ModelDownloadErrorPayload;
  const raw = payload.error;
  const detail = typeof raw === "string" ? raw : raw && typeof raw === "object" ? raw.message ?? "" : "";
  return new ModelDownloadRequestError(
    detail.trim() !== "" ? detail.trim() : `${fallback} (HTTP ${response.status})`,
    response.status,
  );
}

export function downloadSourcesForProvider(sources: ModelFileSource[], provider: ModelDownloadProvider): ModelFileSource[] {
  const cacheType = provider === "modelscope" ? "modelscope_cache" : "hf_cache";
  return sources.filter((source) => source.type === "directory" || source.type === cacheType);
}

export function defaultDownloadSourceID(sources: ModelFileSource[], provider: ModelDownloadProvider): string {
  const compatible = downloadSourcesForProvider(sources, provider);
  const cacheType = provider === "modelscope" ? "modelscope_cache" : "hf_cache";
  return compatible.find((source) => source.type === cacheType)?.id
    ?? compatible.find((source) => source.type === "directory")?.id
    ?? compatible[0]?.id
    ?? "";
}

export async function getModelDownloads(): Promise<ModelDownloadsResponse> {
  const response = await fetch("/api/model-downloads?limit=100");
  if (!response.ok) throw await readError(response, "Unable to load model downloads");
  return (await response.json()) as ModelDownloadsResponse;
}

export async function enqueueModelDownload(input: ModelDownloadInput): Promise<{ task: ModelDownload; duplicate: boolean }> {
  const response = await fetch("/api/model-downloads", {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify(input),
  });
  if (!response.ok) throw await readError(response, "Unable to enqueue model download");
  // The backend answers 200 with duplicate=true when the same repository is
  // already queued; surface it so the UI does not claim a fresh enqueue.
  const payload = (await response.json()) as { task: ModelDownload; duplicate?: boolean };
  return { task: payload.task, duplicate: payload.duplicate === true };
}

export async function deleteModelDownload(id: string): Promise<void> {
  const response = await fetch(`/api/model-downloads/${encodeURIComponent(id)}`, { method: "DELETE" });
  if (!response.ok) throw await readError(response, "Unable to delete model download");
}

export async function getModelDownloadCredentials(): Promise<ModelDownloadCredentials> {
  const response = await fetch("/api/model-download-credentials");
  if (!response.ok) throw await readError(response, "Unable to load model download credentials");
  return (await response.json()) as ModelDownloadCredentials;
}

export async function updateModelDownloadCredentials(
  patch: ModelDownloadCredentialsPatch,
  etag: string,
): Promise<ModelDownloadCredentials> {
  const response = await fetch("/api/model-download-credentials", {
    method: "PATCH",
    headers: { "Content-Type": "application/json", "If-Match": etag },
    body: JSON.stringify(patch),
  });
  if (!response.ok) throw await readError(response, "Unable to save model download credentials");
  return (await response.json()) as ModelDownloadCredentials;
}

export async function cancelModelDownload(id: string): Promise<ModelDownload> {
  return mutateModelDownload(id, "cancel");
}

export async function retryModelDownload(id: string): Promise<ModelDownload> {
  return mutateModelDownload(id, "retry");
}

async function mutateModelDownload(id: string, action: "cancel" | "retry"): Promise<ModelDownload> {
  const response = await fetch(`/api/model-downloads/${encodeURIComponent(id)}/${action}`, { method: "POST" });
  if (!response.ok) throw await readError(response, `Unable to ${action} model download`);
  const payload = (await response.json()) as { task: ModelDownload };
  return payload.task;
}
