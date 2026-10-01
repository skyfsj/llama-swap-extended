import { errorMessageFromPayload } from "./apiError";
import type { ModelDownload, ModelFile, ModelFilesResponse } from "./types";

export interface ModelFileProject {
  key: string;
  name: string;
  presentation: "directory" | "file";
  revision?: string;
  sourceId: string;
  sourceName: string;
  sourceType: string;
  totalSize: number;
  files: ModelFile[];
  download?: ModelDownload;
}

export function groupModelFiles(
  files: ModelFile[],
  sourceNames: Record<string, string>,
  downloads: ModelDownload[] = [],
): ModelFileProject[] {
  const groups = new Map<string, ModelFileProject>();
  for (const file of files) {
    const name = modelFileProjectName(file, sourceNames[file.source_id] ?? file.source_id);
    const presentation = modelFileProjectPresentation(file);
    const projectPath = modelFileProjectPath(file);
    const revision = file.revision?.trim() || undefined;
    const key = [file.source_id, presentation, projectPath || name, revision ?? "", presentation === "file" ? file.id : ""].join("\u0000");
    let group = groups.get(key);
    if (!group) {
      group = {
        key,
        name,
        presentation,
        revision,
        sourceId: file.source_id,
        sourceName: sourceNames[file.source_id] ?? file.source_id,
        sourceType: file.source_type,
        totalSize: 0,
        files: [],
      };
      groups.set(key, group);
    }
    group.files.push(file);
    group.totalSize += Math.max(0, file.size || 0);
  }

  for (const download of downloads) {
    if (download.status !== "queued" && download.status !== "downloading") continue;
    const matching = [...groups.values()].find((group) =>
      group.sourceId === download.source_id && group.name === download.repo_id,
    );
    if (matching) {
      if (!matching.download || matching.download.updated_at < download.updated_at) matching.download = download;
      continue;
    }
    const sourceName = sourceNames[download.source_id]
      ?? (download.provider === "modelscope" ? "ModelScope" : "Hugging Face");
    const key = [download.source_id, download.repo_id, download.revision, "download"].join("\u0000");
    groups.set(key, {
      key,
      name: download.repo_id,
      presentation: "directory",
      revision: download.revision || undefined,
      sourceId: download.source_id,
      sourceName,
      sourceType: download.provider === "modelscope" ? "modelscope_cache" : "hf_cache",
      totalSize: 0,
      files: [],
      download,
    });
  }

  return [...groups.values()]
    .map((group) => ({
      ...group,
      files: [...group.files].sort((left, right) => modelFileDisplayPath(left).localeCompare(modelFileDisplayPath(right))),
    }))
    .sort((left, right) => {
      const presentationOrder = left.presentation === right.presentation ? 0 : left.presentation === "directory" ? -1 : 1;
      return presentationOrder
        || left.name.localeCompare(right.name)
        || (left.revision ?? "").localeCompare(right.revision ?? "")
        || left.sourceName.localeCompare(right.sourceName);
    });
}

export function modelFileDisplayPath(file: ModelFile): string {
  const relative = file.relative_path.replaceAll("\\", "/");
  if (file.repository && file.revision) {
    const marker = `/snapshots/${file.revision}/`;
    const path = `/${relative}`;
    const markerIndex = path.indexOf(marker);
    if (markerIndex >= 0) return path.slice(markerIndex + marker.length);
  }
  const segments = relative.split("/").filter(Boolean);
  if (file.source_type === "directory" && segments.length > 1) return segments.slice(1).join("/");
  return relative || file.name;
}

function modelFileProjectName(file: ModelFile, sourceName: string): string {
  if (file.repository?.trim()) return file.repository.trim();
  const projectPath = modelFileProjectPath(file);
  if (projectPath) return projectPath.split("/").at(-1) ?? sourceName;
  if (modelFileProjectPresentation(file) === "file") return modelFileName(file);
  return sourceName;
}

function modelFileProjectPath(file: ModelFile): string {
  if (file.source_type !== "directory" || file.repository?.trim()) return "";
  const directories = file.relative_path.replaceAll("\\", "/").split("/").filter(Boolean).slice(0, -1);
  const genericDirectoryNames = new Set(["blobs", "checkpoints", "files", "model", "models", "snapshot", "snapshots", "transformer", "transformers", "weights"]);
  let index = directories.length - 1;
  while (index >= 0 && genericDirectoryNames.has(directories[index].toLowerCase())) index--;
  return index >= 0 ? directories.slice(0, index + 1).join("/") : "";
}

function modelFileProjectPresentation(file: ModelFile): "directory" | "file" {
  if (file.source_type === "file") return "file";
  if (file.source_type !== "directory" || file.repository?.trim()) return "directory";
  return file.relative_path.replaceAll("\\", "/").split("/").filter(Boolean).length <= 1
    ? "file"
    : "directory";
}

function modelFileName(file: ModelFile): string {
  return file.name.replace(/\.(?:gguf|safetensors|bin|onnx|pt|pth|ckpt)$/i, "") || file.name;
}

export interface ModelFileDeleteConflict {
  error?: string;
  registered?: string[];
  in_use?: string[];
}

export class ModelFileRequestError extends Error {
  readonly status: number;
  readonly conflict?: ModelFileDeleteConflict;

  constructor(message: string, status: number, conflict?: ModelFileDeleteConflict) {
    super(message);
    this.name = "ModelFileRequestError";
    this.status = status;
    this.conflict = conflict;
  }
}

async function readError(response: Response, fallback: string): Promise<ModelFileRequestError> {
  const payload = (await response.json().catch(() => ({}))) as Record<string, unknown>;
  // Backend error envelopes carry `error` as either a string or a
  // {message,...} object; passing the object straight to Error would render
  // as "[object Object]". Route both shapes through the shared extractor.
  const message = errorMessageFromPayload(payload, `${fallback} (HTTP ${response.status})`);
  return new ModelFileRequestError(message, response.status, payload as ModelFileDeleteConflict);
}

export async function getModelFiles(options: { source?: string; query?: string } = {}): Promise<ModelFilesResponse> {
  const params = new URLSearchParams({ limit: "1000" });
  if (options.source && options.source !== "all") params.set("source", options.source);
  if (options.query?.trim()) params.set("query", options.query.trim());
  const response = await fetch(`/api/model-files?${params.toString()}`);
  if (!response.ok) throw await readError(response, "Unable to load model files");
  return (await response.json()) as ModelFilesResponse;
}

export async function deleteModelFile(file: Pick<ModelFile, "id" | "source_id" | "path">): Promise<void> {
  const response = await fetch(`/api/model-files/${encodeURIComponent(file.id)}`, {
    method: "DELETE",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ source_id: file.source_id, path: file.path, confirm: true }),
  });
  if (!response.ok) throw await readError(response, "Unable to delete model file");
}
