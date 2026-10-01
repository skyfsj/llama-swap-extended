import type { SettingsSnapshot } from "./settingsApi";
import type { RuntimeReadinessCheck, RuntimeReadinessLevel, RuntimeStatus } from "./types";

export type RuntimeFilter = "all" | "attention" | "updates";

const environmentMissingPatterns = [/no \.venv/i, /build path is not a real directory/i, /active runtime manifest is missing/i];

export function runtimeEnvironmentMissing(runtime: RuntimeStatus): boolean {
  const lastError = runtime.lastError;
  if (!lastError) return false;
  return environmentMissingPatterns.some((pattern) => pattern.test(lastError));
}

export function runtimeHasUpdate(runtime: RuntimeStatus): boolean {
  return Boolean((runtime.staged && runtime.staged !== runtime.current) || (runtime.available && runtime.available !== runtime.current));
}

export function runtimeNeedsAttention(runtime: RuntimeStatus): boolean {
  return Boolean(runtime.lastError || runtime.state === "DEGRADED");
}

export function filterRuntimes(runtimes: RuntimeStatus[], query: string, filter: RuntimeFilter): RuntimeStatus[] {
  const needle = query.trim().toLowerCase();
  return runtimes.filter((runtime) => {
    const matchesQuery = !needle || [runtime.name, runtime.kind, runtime.mode, runtime.source, runtime.current, runtime.staged, runtime.available].some((value) => value?.toLowerCase().includes(needle));
    return matchesQuery && (filter === "all" || (filter === "updates" ? runtimeHasUpdate(runtime) : runtimeNeedsAttention(runtime)));
  });
}

export type RuntimeTemplateID =
  | "vllm-official"
  | "llamacpp-official"
  | "llamacpp-source"
  | "vllm-wheel"
  | "vllm-git"
  | "container"
  | "clone";

export interface RuntimeTemplate {
  id: RuntimeTemplateID;
  kind: "llamacpp" | "vllm";
  mode: "native" | "container";
  labelKey: string;
  descriptionKey: string;
  defaultName: string;
  config: Record<string, unknown>;
}

const defaultUpdate = (): Record<string, unknown> => ({
  policy: "automatic",
  channel: "stable",
  activateOnlyWhenIdle: true,
  keepVersions: 2,
  rollbackOnFailure: true,
});

export const officialRuntimeTemplateIDs: readonly RuntimeTemplateID[] = ["vllm-official", "llamacpp-official"];

export function isOfficialRuntimeTemplate(id: RuntimeTemplateID): boolean {
  return (officialRuntimeTemplateIDs as readonly string[]).includes(id);
}

export const runtimeTemplates: RuntimeTemplate[] = [
  {
    id: "vllm-official",
    kind: "vllm",
    mode: "native",
    labelKey: "controlPlane.runtimeCenter.templates.vllmOfficial.label",
    descriptionKey: "controlPlane.runtimeCenter.templates.vllmOfficial.description",
    defaultName: "vllm",
    config: {
      kind: "vllm",
      mode: "native",
      source: { type: "pypi" },
      build: { driver: "uv" },
      update: defaultUpdate(),
    },
  },
  {
    id: "llamacpp-official",
    kind: "llamacpp",
    mode: "native",
    labelKey: "controlPlane.runtimeCenter.templates.llamacppOfficial.label",
    descriptionKey: "controlPlane.runtimeCenter.templates.llamacppOfficial.description",
    defaultName: "llamacpp",
    config: {
      kind: "llamacpp",
      mode: "native",
      source: { type: "git", repository: "https://github.com/ggml-org/llama.cpp", trackRef: "main" },
      update: defaultUpdate(),
    },
  },
  {
    id: "llamacpp-source",
    kind: "llamacpp",
    mode: "native",
    labelKey: "controlPlane.runtimeCenter.templates.llamacppSource.label",
    descriptionKey: "controlPlane.runtimeCenter.templates.llamacppSource.description",
    defaultName: "llamacpp",
    config: {
      kind: "llamacpp",
      mode: "native",
      source: { type: "git", repository: "", trackRef: "main" },
      update: defaultUpdate(),
    },
  },
  {
    id: "vllm-wheel",
    kind: "vllm",
    mode: "native",
    labelKey: "controlPlane.runtimeCenter.templates.vllmWheel.label",
    descriptionKey: "controlPlane.runtimeCenter.templates.vllmWheel.description",
    defaultName: "vllm",
    config: {
      kind: "vllm",
      mode: "native",
      source: { type: "wheel", path: "" },
      build: { driver: "uv" },
      update: defaultUpdate(),
    },
  },
  {
    id: "vllm-git",
    kind: "vllm",
    mode: "native",
    labelKey: "controlPlane.runtimeCenter.templates.vllmGit.label",
    descriptionKey: "controlPlane.runtimeCenter.templates.vllmGit.description",
    defaultName: "vllm",
    config: {
      kind: "vllm",
      mode: "native",
      source: { type: "git", repository: "", trackRef: "refs/heads/main" },
      build: { driver: "uv" },
      update: defaultUpdate(),
    },
  },
  {
    id: "container",
    kind: "vllm",
    mode: "container",
    labelKey: "controlPlane.runtimeCenter.templates.container.label",
    descriptionKey: "controlPlane.runtimeCenter.templates.container.description",
    defaultName: "vllm",
    config: {
      kind: "vllm",
      mode: "container",
      source: { type: "image", image: "", pullPolicy: "if-missing" },
      container: { engine: "docker", image: "", pullPolicy: "if-missing" },
      update: defaultUpdate(),
    },
  },
];

export function cloneRuntimeValue<T>(value: T): T {
  if (value === undefined || value === null) return value;
  return JSON.parse(JSON.stringify(value)) as T;
}

export function runtimeConfigForTemplate(id: RuntimeTemplateID, source?: Record<string, unknown>): Record<string, unknown> {
  if (id === "clone" && source) return cloneRuntimeValue(source);
  return cloneRuntimeValue(runtimeTemplates.find((template) => template.id === id)?.config ?? runtimeTemplates[0].config);
}

// runtimeWizardFirstStep reports the step the wizard opens on. An existing
// runtime has no template to choose — its definition is already there — so
// editing starts on the definition form and only offers definition and
// preview. Creating and cloning still start from the template picker.
export function runtimeWizardFirstStep(mode: "create" | "clone" | "edit"): number {
  return mode === "edit" ? 1 : 0;
}

export function runtimeTemplateFor(id: RuntimeTemplateID): RuntimeTemplate | undefined {
  return runtimeTemplates.find((template) => template.id === id);
}

// Suggested runtime name when picking a template. Clone mode keeps the
// caller-provided name, so it returns an empty string.
export function runtimeTemplateNameSuggestion(id: RuntimeTemplateID): string {
  if (id === "clone") return "";
  return runtimeTemplates.find((template) => template.id === id)?.defaultName ?? "vllm";
}

export function runtimeWizardValidation(name: string, runtimeConfig: Record<string, unknown>): string[] {
  const errors: string[] = [];
  const normalizedName = name.trim();
  if (!normalizedName) errors.push("name-required");
  else if (!/^[A-Za-z0-9][A-Za-z0-9._-]*$/.test(normalizedName)) errors.push("name-invalid");

  const kind = String(runtimeConfig.kind ?? "").trim().toLowerCase();
  const mode = String(runtimeConfig.mode ?? "native").trim().toLowerCase();
  const source = asRecord(runtimeConfig.source);
  const sourceType = String(source.type ?? "").trim().toLowerCase();
  if (kind !== "vllm" && kind !== "llamacpp") errors.push("kind-invalid");
  if (mode !== "native" && mode !== "container") errors.push("mode-invalid");
  if (!sourceType) errors.push("source-type-required");
  if (sourceType === "wheel" && !String(source.path ?? source.url ?? "").trim()) errors.push("wheel-path-required");
  if (["git", "tag", "commit"].includes(sourceType) && !String(source.repository ?? source.url ?? "").trim()) errors.push("repository-required");
  if (["release", "channel", "url"].includes(sourceType) && !String(source.url ?? "").trim()) errors.push("source-url-required");
  if (sourceType === "local" && !String(source.path ?? source.url ?? "").trim()) errors.push("local-source-required");
  if (mode === "container" && !String(source.image ?? asRecord(runtimeConfig.container).image ?? "").trim()) errors.push("image-required");
  return errors;
}

export interface RuntimeBindingModel {
  type?: string;
  runtime?: string;
  runtimeVersion?: string;
  args?: unknown[];
  launch?: unknown;
  container?: { image?: string };
  cmd?: string;
  lmcache?: unknown;
}

export function runtimeModelCompatibility(runtimeConfig: Record<string, unknown>, model: RuntimeBindingModel): { compatible: boolean; reason: string } {
  const runtimeKind = String(runtimeConfig.kind ?? "").trim().toLowerCase();
  const backendKind = String(model.type ?? "").trim().toLowerCase();
  if (!runtimeKind || runtimeKind !== backendKind) {
    return { compatible: false, reason: `backend type ${model.type ?? ""} must match runtime kind ${runtimeConfig.kind ?? ""}` };
  }
  if (runtimeKind !== "vllm" && runtimeKind !== "llamacpp") {
    return { compatible: false, reason: `runtime kind ${runtimeConfig.kind ?? ""} is not supported` };
  }
  const source = asRecord(runtimeConfig.source);
  const container = asRecord(runtimeConfig.container);
  const mode = String(runtimeConfig.mode ?? "native").trim().toLowerCase();
  if (String(model.cmd ?? "").trim()) {
    return { compatible: false, reason: "legacy cmd conflicts with managed runtime binding" };
  }
  if (mode === "container") {
    if (!String(model.container?.image ?? source.image ?? container.image ?? "").trim()) {
      return { compatible: false, reason: "container runtime needs a runtime or backend image" };
    }
    if (model.lmcache) return { compatible: false, reason: "LMCache requires a native vLLM runtime" };
    return { compatible: true, reason: "container backend is compatible" };
  }
  if (String(model.container?.image ?? "").trim()) {
    return { compatible: false, reason: "native runtime cannot use a backend container image" };
  }
  if (!Array.isArray(model.args) && !model.launch) {
    return { compatible: false, reason: "native runtime needs backend args or a launch block" };
  }
  return { compatible: true, reason: "native backend is compatible" };
}

export function readinessLevel(checks: RuntimeReadinessCheck[]): RuntimeReadinessLevel {
  if (checks.some((check) => check.level === "block")) return "block";
  if (checks.some((check) => check.level === "warning")) return "warning";
  return "pass";
}

export function runtimeNextAction(status: {
  configured?: boolean;
  current?: string;
  available?: string;
  staged?: string;
  lastError?: string;
}): "configure" | "inspect" | "stage" | "activate" | "check" {
  if (!status.configured) return "configure";
  if (status.lastError) return "inspect";
  if (status.staged && status.staged !== status.current) return "activate";
  if (status.available && status.available !== status.current) return "stage";
  return "check";
}

/**
 * Find the nearest source owner for a JSON pointer. Settings ownership may
 * expose a nested pointer or only its top-level parent depending on the
 * source loader, so walking upward keeps the UI conservative without
 * hardcoding source filenames.
 */
export function sourceOwnerForPath(ownership: Record<string, string> | undefined, path: string): string | undefined {
  if (!ownership) return undefined;
  let current = path;
  while (current) {
    if (ownership[current]) return ownership[current];
    const slash = current.lastIndexOf("/");
    if (slash <= 0) break;
    current = current.slice(0, slash);
  }
  return ownership["/"];
}

export function sourceBoundaryForPaths(snapshot: Pick<SettingsSnapshot, "ownership" | "sources"> | null, paths: string[]): { blocked: boolean; owners: string[]; reason: string } {
  if (!snapshot) return { blocked: true, owners: [], reason: "settings snapshot is unavailable" };
  const owners = [...new Set(paths.map((path) => sourceOwnerForPath(snapshot.ownership, path)).filter((owner): owner is string => Boolean(owner)))];
  const writable = new Set((snapshot.sources ?? []).filter((source) => source.writable).map((source) => source.path));
  const unresolved = paths.filter((path) => !sourceOwnerForPath(snapshot.ownership, path));
  if (owners.length > 1) {
    return { blocked: true, owners, reason: "runtime definition and model binding belong to different configuration sources" };
  }
  if (owners.some((owner) => !writable.has(owner))) {
    return { blocked: true, owners, reason: "one or more configuration paths belong to a read-only source" };
  }
  if (unresolved.length > 0 && owners.length > 0) {
    return { blocked: true, owners, reason: "some configuration paths have no safe writable source" };
  }
  return { blocked: false, owners, reason: "one configuration transaction can cover the selected paths" };
}

function asRecord(value: unknown): Record<string, any> {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, any> : {};
}
