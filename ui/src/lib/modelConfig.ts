export type LaunchMode = "command" | "backend";

// ModelLaunchDraft mirrors the structured backend.launch block: the basic
// fields most users set, with backend.args holding only the extra engine
// arguments the UI does not manage. Number inputs normally start as strings,
// but a native type="number" input can write a number (or undefined when it
// is cleared), so serializers must accept all three values.
export type DraftNumberValue = string | number | undefined;

export interface ModelLaunchDraft {
  model: string;
  servedModelName: string;
  contextPerRequest: DraftNumberValue;
  maxConcurrency: DraftNumberValue;
  tensorParallelSize: DraftNumberValue;
  gpus: string[];
  gpuMemoryUtilization: DraftNumberValue;
}

export function emptyLaunchDraft(): ModelLaunchDraft {
  return {
    model: "", servedModelName: "", contextPerRequest: "", maxConcurrency: "",
    tensorParallelSize: "", gpus: [], gpuMemoryUtilization: "",
  };
}

// launchFromBackend reads the structured backend.launch block into the draft.
// Zero values stay empty so the inputs show their placeholders instead.
function launchFromBackend(backend: Record<string, unknown>): ModelLaunchDraft {
  const launch = record(property(backend, "launch"));
  const count = (value: unknown): string => {
    const parsed = numberText(value);
    return parsed === "0" ? "" : parsed;
  };
  const utilization = percentageText(property(launch, "gpuMemoryUtilization"));
  const configuredTensorParallel = count(property(launch, "tensorParallelSize"));
  return {
    model: text(property(launch, "model")),
    servedModelName: text(property(launch, "servedModelName")),
    contextPerRequest: count(property(launch, "contextPerRequest")),
    maxConcurrency: count(property(launch, "maxConcurrency")),
    // Older versions could leave this managed flag in backend.args while the
    // other launch fields already lived in backend.launch. Hydrate it here so
    // opening and saving that config cannot silently remove TP again.
    tensorParallelSize: configuredTensorParallel || tensorParallelSizeFromArguments(backend),
    gpus: listText(property(launch, "gpus")),
    gpuMemoryUtilization: utilization,
  };
}

function tensorParallelSizeFromArguments(backend: Record<string, unknown>): string {
  const raw = listText(propertyAny(backend, "args", "arguments"));
  const tokens = tokenizeLaunchArguments(raw.join("\n"));
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    const equals = token.indexOf("=");
    const name = equals > 0 ? token.slice(0, equals) : token;
    if (name !== "--tensor-parallel-size" && name !== "-tp") continue;
    const value = equals > 0 ? token.slice(equals + 1) : tokens[index + 1] ?? "";
    const parsed = Number(value);
    return Number.isInteger(parsed) && parsed > 0 ? String(parsed) : "";
  }
  return "";
}

// autoServedModelName derives the name a managed engine must serve: the
// upstream name llama-swap sends (useModelName) or the model id when no
// override is set. A manual served name that differs from this breaks
// routing, so a managed launch owns this value.
export function autoServedModelName(useModelName: string, modelId: string): string {
  const name = trimmedText(useModelName);
  return name !== "" ? name : trimmedText(modelId);
}

// launchToBackend serializes the structured launch draft back into the
// backend.launch block. Percentages are stored as the 0-1 fraction the
// engines expect.
export function launchToBackend(draft: ModelConfigDraft): Record<string, unknown> | undefined {
  const launch: Record<string, unknown> = {};
  setOptional(launch, "model", trimmedText(draft.launch.model));
  setOptional(launch, "servedModelName", trimmedText(draft.launch.servedModelName));
  setOptional(launch, "contextPerRequest", integer(draft.launch.contextPerRequest, "launch.contextPerRequest", 1));
  setOptional(launch, "maxConcurrency", integer(draft.launch.maxConcurrency, "launch.maxConcurrency", 1));
  setOptional(launch, "tensorParallelSize", integer(draft.launch.tensorParallelSize, "launch.tensorParallelSize", 1));
  // CUDA_VISIBLE_DEVICES in draft.env is the only persisted GPU selector.
  // draft.launch.gpus is transient card-picker state and must not become a
  // second config value that can drift from the environment.
  if (trimmedText(draft.launch.gpuMemoryUtilization) !== "") {
    const percent = Number(draft.launch.gpuMemoryUtilization);
    if (!Number.isFinite(percent) || percent <= 0 || percent > 100) {
      throw new Error("GPU 显存使用上限必须是 (0, 100] 内的百分比");
    }
    launch["gpuMemoryUtilization"] = percent / 100;
  }
  return Object.keys(launch).length > 0 ? launch : undefined;
}

export interface ModelConfigDraft {
  id: string;
  name: string;
  description: string;
  aliases: string;
  useModelName: string;
  launchMode: LaunchMode;
  initialLaunchMode: LaunchMode;
  cmd: string;
  cmdStop: string;
  proxy: string;
  checkEndpoint: string;
  env: string;
  ttl: DraftNumberValue;
  unloadTimeout: DraftNumberValue;
  concurrencyLimit: DraftNumberValue;
  unlisted: boolean;
  disabled: boolean;
  sendLoadingState: "" | "true" | "false";
  backendType: string;
  backendRuntime: string;
  backendRuntimeVersion: string;
  backendProtocol: string;
  backendAPIs: string[];
  backendDiscover: "" | "true" | "false";
  backendArguments: string;
  launch: ModelLaunchDraft;
  backendContainer: Record<string, unknown>;
  lmcacheEnabled: boolean;
  lmcacheMode: "mp" | "inProcess";
  lmcacheRole: string;
  lmcacheHost: string;
  lmcachePort: string;
  lmcacheChunkSize: string;
  pricingProvider: string;
  pricingModel: string;
  lifecycleMode: string;
  sleepLevel: DraftNumberValue;
  vramMiB: DraftNumberValue;
  ramMiB: DraftNumberValue;
  gpuAffinity: string;
  priority: DraftNumberValue;
  evictionPriority: DraftNumberValue;
  capabilitiesIn: string[];
  capabilitiesOut: string[];
  capabilitiesTools: boolean;
  capabilitiesReranker: boolean;
  capabilitiesContext: DraftNumberValue;
  filtersStripParams: string;
  filtersSetParams: Record<string, unknown>;
  filtersSetParamsByID: Record<string, unknown>;
  macros: Record<string, unknown>;
  metadata: Record<string, unknown>;
  timeoutConnect: DraftNumberValue;
  timeoutKeepAlive: DraftNumberValue;
  timeoutResponseHeader: DraftNumberValue;
  timeoutTLSHandshake: DraftNumberValue;
  timeoutExpectContinue: DraftNumberValue;
  timeoutIdleConn: DraftNumberValue;
  ignoreWebsockets: "" | "true" | "false";
}

export interface ConfigChange {
  path: string;
  before?: unknown;
  after?: unknown;
  kind: "added" | "removed" | "changed";
}

export interface RuntimeOption {
  name: string;
  kind: string;
  mode: string;
}

export interface ManagedModelFile {
  id: string;
  name: string;
  path: string;
  relative_path: string;
  source_id: string;
  source_type: string;
  repository?: string;
  revision?: string;
  format: string;
}

export interface ManagedModelTarget {
  value: string;
  label: string;
  path: string;
  sourceID: string;
  format: string;
}

const API_KEYS = [
  "chat",
  "completions",
  "responses",
  "embeddings",
  "transcriptions",
  "translations",
  "speech",
  "images",
  "realtime",
  "anthropic",
  "rerank",
  "classify",
  "score",
  "pooling",
  "generative_scoring",
] as const;

export const MODEL_API_KEYS = [...API_KEYS];
export const MODEL_MODALITIES = ["text", "audio", "image", "video"] as const;

export function record(value: unknown): Record<string, unknown> {
  return value !== null && typeof value === "object" && !Array.isArray(value)
    ? value as Record<string, unknown>
    : {};
}

export function property(value: Record<string, unknown>, name: string): unknown {
  if (name in value) return value[name];
  const key = Object.keys(value).find((candidate) => candidate.toLowerCase() === name.toLowerCase());
  return key === undefined ? undefined : value[key];
}

export function propertyAny(value: Record<string, unknown>, ...names: string[]): unknown {
  for (const name of names) {
    const found = property(value, name);
    if (found !== undefined) return found;
  }
  return undefined;
}

function text(value: unknown): string {
  return value === undefined || value === null ? "" : String(value);
}

// Native number inputs do not preserve the draft's string representation.
// Keep normalization at the boundary so every save path handles both values
// without ever calling trim() on a non-string.
export function trimmedText(value: unknown): string {
  if (typeof value === "string") return value.trim();
  if (typeof value === "number" && Number.isFinite(value)) return String(value);
  return "";
}

function numberText(value: unknown): string {
  return typeof value === "number" && Number.isFinite(value) ? String(value) : "";
}

function percentageText(value: unknown): string {
  const parsed = typeof value === "number" ? value : Number(value);
  if (!Number.isFinite(parsed)) return "";
  // The backend stores engine utilization as a 0-1 fraction while the form
  // is deliberately expressed in the operator-facing 0-100 percentage.
  // Keep accepting values above one for old snapshots that may already have
  // been serialized in percentage form.
  const percent = parsed <= 1 ? parsed * 100 : parsed;
  return String(Math.round(percent * 100) / 100);
}

function listText(value: unknown): string[] {
  return Array.isArray(value) ? value.map(text).filter(Boolean) : [];
}

function numericDeviceList(value: string): string[] {
  const trimmed = value.trim();
  if (!/^\d+(?:,\d+)*$/.test(trimmed)) return [];
  return trimmed.split(",");
}

function environmentLines(value: string): string[] {
  const rawLines = value.split("\n").map((line) => line.trim()).filter(Boolean);
  const result: string[] = [];
  for (let index = 0; index < rawLines.length; index += 1) {
    const line = rawLines[index];
    const equals = line.indexOf("=");
    if (equals > 0 && line.slice(0, equals).trim() === "CUDA_VISIBLE_DEVICES") {
      const devices = line.slice(equals + 1).split(",").map((device) => device.trim()).filter(Boolean);
      let next = index + 1;
      while (next < rawLines.length) {
        const continuation = numericDeviceList(rawLines[next]);
        if (continuation.length === 0) break;
        devices.push(...continuation);
        next += 1;
      }
      result.push(`${line.slice(0, equals)}=${devices.join(",")}`);
      index = next - 1;
      continue;
    }
    result.push(line);
  }
  return result;
}

function boolText(value: unknown): "" | "true" | "false" {
  return typeof value === "boolean" ? String(value) as "true" | "false" : "";
}

export function emptyModelDraft(id = ""): ModelConfigDraft {
  return {
    id,
    name: "",
    description: "",
    aliases: "",
    useModelName: "",
    launchMode: "backend",
    initialLaunchMode: "backend",
    cmd: "",
    cmdStop: "",
    proxy: "",
    checkEndpoint: "",
    env: "",
    ttl: "",
    unloadTimeout: "",
    concurrencyLimit: "",
    unlisted: false,
    disabled: false,
    sendLoadingState: "",
    backendType: "",
    backendRuntime: "",
    backendRuntimeVersion: "",
    backendProtocol: "",
    backendAPIs: [],
    backendDiscover: "",
    backendArguments: "",
    launch: emptyLaunchDraft(),
    backendContainer: {},
    lmcacheEnabled: false,
    lmcacheMode: "mp",
    lmcacheRole: "",
    lmcacheHost: "",
    lmcachePort: "",
    lmcacheChunkSize: "",
    pricingProvider: "",
    pricingModel: "",
    lifecycleMode: "",
    sleepLevel: "",
    vramMiB: "",
    ramMiB: "",
    gpuAffinity: "",
    priority: "",
    evictionPriority: "",
    capabilitiesIn: [],
    capabilitiesOut: [],
    capabilitiesTools: false,
    capabilitiesReranker: false,
    capabilitiesContext: "",
    filtersStripParams: "",
    filtersSetParams: {},
    filtersSetParamsByID: {},
    macros: {},
    metadata: {},
    timeoutConnect: "",
    timeoutKeepAlive: "",
    timeoutResponseHeader: "",
    timeoutTLSHandshake: "",
    timeoutExpectContinue: "",
    timeoutIdleConn: "",
    ignoreWebsockets: "",
  };
}

export function hasModelCapabilityOverrides(draft: ModelConfigDraft): boolean {
  return Boolean(
    trimmedText(draft.backendProtocol)
    || draft.backendAPIs.length > 0
    || draft.backendDiscover !== ""
    || draft.capabilitiesIn.length > 0
    || draft.capabilitiesOut.length > 0
    || draft.capabilitiesTools
    || draft.capabilitiesReranker
    || trimmedText(draft.capabilitiesContext),
  );
}

export function modelEntries(config: unknown): Record<string, unknown> {
  return record(property(record(config), "models"));
}

export function runtimeNames(config: unknown): string[] {
  return runtimeOptions(config).map((runtime) => runtime.name);
}

// runtimeMode mirrors the server's effectiveRuntimeMode: an explicit mode
// wins, an image on the source or container block means container, and
// everything else is native.
export function runtimeMode(runtime: unknown): string {
  const definition = record(runtime);
  const explicit = text(property(definition, "mode")).trim().toLowerCase();
  if (explicit) return explicit;
  const source = record(property(definition, "source"));
  if (text(property(source, "image")).trim() !== "" || text(property(record(property(definition, "container")), "image")).trim() !== "") {
    return "container";
  }
  return "native";
}

// managedLaunchKind returns the managed engine kind whose structured launch
// block the editor binds, or "" when there is none. Container runtimes pass
// backend.args through to the container verbatim and ignore the launch block,
// so their models must not see the structured launch fields, the model
// picker, or the reserved-flag ownership rules the native path enforces.
export function managedLaunchKind(kind: string, mode: string): string {
  const normalizedKind = kind.trim().toLowerCase();
  if (normalizedKind !== "vllm" && normalizedKind !== "llamacpp") return "";
  if (mode.trim().toLowerCase() === "container") return "";
  return normalizedKind;
}

export function runtimeOptions(config: unknown): RuntimeOption[] {
  const runtimes = record(property(record(config), "runtimes"));
  return Object.keys(runtimes)
    .sort((a, b) => a.localeCompare(b, undefined, { numeric: true }))
    .map((name) => {
      const definition = record(runtimes[name]);
      return {
        name,
        kind: text(property(definition, "kind")).trim().toLowerCase(),
        mode: runtimeMode(definition),
      };
    });
}

export function managedRuntimeEntrypoint(kind: string): string {
  switch (kind.trim().toLowerCase()) {
    case "vllm": return "vllm";
    case "llamacpp": return "llama-server";
    default: return "";
  }
}

const PYTHON_INTERPRETERS = new Set(["python", "python3"]);
const KNOWN_ENTRYPOINTS = new Set(["vllm", "llama-server", "python", "python3"]);
const VLLM_API_SERVER_MODULE = "vllm.entrypoints.openai.api_server";
const MODEL_SELECTOR_FLAGS = ["--model", "--model-path"];
const MODEL_SELECTOR_PREFIXES = ["--model=", "--model-path="];

// tokenizeArgLine splits one line of a launch command into argv tokens. A
// line without whitespace stays a single token; a line with unquoted
// whitespace is split on it, and single or double quotes group spaced
// content into one token. This lets a pasted single-line command behave the
// same as the one-token-per-line form.
export function tokenizeArgLine(line: string): string[] {
  const trimmed = line.trim();
  if (trimmed === "") return [];
  if (!/\s/.test(trimmed)) {
    const quoted = trimmed.match(/^(['"])([\s\S]*)\1$/);
    return [quoted ? quoted[2] : trimmed];
  }
  const tokens: string[] = [];
  let current = "";
  let quote = "";
  let started = false;
  for (let index = 0; index < trimmed.length; index += 1) {
    const char = trimmed[index];
    if (quote !== "") {
      if (quote === '"' && char === "\\" && index + 1 < trimmed.length) {
        // \" and \\ are the only escapes; any other backslash pair is
        // preserved verbatim (a Windows path must survive the round-trip).
        const next = trimmed[index + 1];
        current += next === '"' || next === "\\" ? next : char + next;
        index += 1;
      } else if (char === quote) {
        quote = "";
      } else {
        current += char;
      }
    } else if (char === '"' || char === "'") {
      quote = char;
      started = true;
    } else if (/\s/.test(char)) {
      if (started) {
        tokens.push(current);
        current = "";
        started = false;
      }
    } else {
      current += char;
      started = true;
    }
  }
  if (started || current !== "") tokens.push(current);
  return tokens;
}

// tokenizeLaunchArguments splits free-form launch text into argv tokens:
// every line contributes at least the tokens its whitespace split yields.
export function tokenizeLaunchArguments(value: string): string[] {
  const tokens: string[] = [];
  for (const line of value.split("\n")) tokens.push(...tokenizeArgLine(line));
  return tokens;
}

// serializeLaunchArgument renders one argv token back into the editor's text
// form. Quoting must round-trip losslessly through tokenizeArgLine: single-
// quoted content is literal (so pasted JSON values survive), and double quotes
// pass single quotes through verbatim. A token containing a single quote can
// never be single-quote wrapped, so it is double-quoted with \" and \\
// escapes, which the tokenizer unescapes.
export function serializeLaunchArgument(argument: string): string {
  if (argument === "" || !/[\s"']/.test(argument)) return argument;
  if (argument.includes("'")) {
    return `"${argument.replace(/\\/g, "\\\\").replace(/"/g, '\\"')}"`;
  }
  return `'${argument}'`;
}

interface NormalizedLaunch {
  kind: string;
  entrypoint: string;
  module: string;
  subcommand: string;
  rest: string[];
}

// stripPythonModulePrefix consumes a leading python/python3 interpreter and,
// when present, its -m/--module flag plus module name. Without this step the
// interpreter is lost during entrypoint normalization and the module name
// leaks into the model position.
function stripPythonModulePrefix(tokens: string[]): { rest: string[]; module: string } {
  let rest = tokens;
  if (!PYTHON_INTERPRETERS.has(rest[0] ?? "")) return { rest, module: "" };
  rest = rest.slice(1);
  if (rest[0] !== "-m" && rest[0] !== "--module") return { rest, module: "" };
  return { rest: rest.slice(2), module: rest[1] ?? "" };
}

// normalizeManagedLaunch parses the managed-runtime launch forms into their
// structural parts: the runtime entrypoint, the python -m module, the engine
// subcommand and the remaining positional/flag tokens.
function normalizeManagedLaunch(value: string, kind: string): NormalizedLaunch {
  const normalizedKind = kind.trim().toLowerCase();
  const entrypoint = managedRuntimeEntrypoint(normalizedKind);
  const stripped = stripPythonModulePrefix(tokenizeLaunchArguments(value));
  let rest = stripped.rest;
  if (entrypoint !== "" && KNOWN_ENTRYPOINTS.has(rest[0] ?? "")) rest = rest.slice(1);
  let subcommand = "";
  if (normalizedKind === "vllm") {
    if (stripped.module === VLLM_API_SERVER_MODULE) subcommand = "serve";
    if (rest[0] === "serve") {
      subcommand = "serve";
      rest = rest.slice(1);
    }
  }
  return { kind: normalizedKind, entrypoint, module: stripped.module, subcommand, rest };
}

// The bind host and port are owned by llama-swap: the managed engine must
// listen where the model's proxy points, so operator-written duplicates are
// dropped from the editor text instead of being stored.
const MANAGED_HOST_FLAGS = ["--host", "-hp"];
const MANAGED_HOST_PREFIXES = ["--host=", "-hp="];
const MANAGED_PORT_FLAGS = ["--port"];
const MANAGED_PORT_PREFIXES = ["--port="];

function isHostPortToken(token: string): boolean {
  return MANAGED_HOST_FLAGS.includes(token) || MANAGED_PORT_FLAGS.includes(token)
    || MANAGED_HOST_PREFIXES.some((prefix) => token.startsWith(prefix))
    || MANAGED_PORT_PREFIXES.some((prefix) => token.startsWith(prefix));
}

// systemOwnedMask marks the argv tokens llama-swap owns: a bare --host/--port
// flag also consumes the value that follows it, even when that value lives on
// the next editor line. The model target is operator content and stays.
function systemOwnedMask(tokens: string[]): boolean[] {
  const skip = new Array<boolean>(tokens.length).fill(false);
  for (let index = 0; index < tokens.length; index += 1) {
    if (!isHostPortToken(tokens[index])) continue;
    skip[index] = true;
    if (!tokens[index].includes("=") && index + 1 < tokens.length) {
      skip[index + 1] = true;
      index += 1;
    }
  }
  return skip;
}

// modelSelectorMask marks every operator-written model target: the pair
// flags, their "--model=" forms, and the leading positional token.
function modelSelectorMask(tokens: string[]): boolean[] {
  const skip = new Array<boolean>(tokens.length).fill(false);
  for (let index = 0; index < tokens.length; index += 1) {
    const token = tokens[index];
    if (MODEL_SELECTOR_FLAGS.includes(token)) {
      skip[index] = true;
      if (index + 1 < tokens.length) skip[index + 1] = true;
      index += 1;
      continue;
    }
    if (MODEL_SELECTOR_PREFIXES.some((prefix) => token.startsWith(prefix))) skip[index] = true;
  }
  for (let index = 0; index < tokens.length; index += 1) {
    if (skip[index]) continue;
    if (tokens[index] !== "" && !tokens[index].startsWith("-")) skip[index] = true;
    break;
  }
  return skip;
}

// reassembleLines joins the surviving tokens back into editor lines. A line
// none of whose tokens were touched is kept verbatim — what the operator
// wrote is what the editor shows and what gets stored. Only a line that lost
// some tokens to a system-owned removal is rebuilt from the survivors.
function reassembleLines(rawLines: string[], perLine: string[][], skip: boolean[]): string {
  const kept: string[] = [];
  let position = 0;
  for (let index = 0; index < rawLines.length; index += 1) {
    const tokens = perLine[index];
    if (tokens.length === 0) continue;
    const start = position;
    position += tokens.length;
    const survivors = tokens.filter((_, offset) => !skip[start + offset]);
    if (survivors.length === 0) continue;
    if (survivors.length === tokens.length) {
      kept.push(rawLines[index].trim());
      continue;
    }
    kept.push(survivors.map(serializeLaunchArgument).join(" "));
  }
  return kept.join("\n");
}

// extraLaunchArguments returns the operator-owned editor text for a managed
// runtime: the launch head (entrypoint, python module, engine subcommand) and
// the system-owned bind host and port are removed. Every other line is kept
// verbatim — "--flag value" lines, quoted JSON values and the operator's
// model target all round-trip exactly as written in the config file.
export function extraLaunchArguments(value: string, kind: string): string {
  const launch = normalizeManagedLaunch(value, kind);
  if (launch.entrypoint === "") return value;
  const rawLines = value.split("\n");
  const perLine = rawLines.map(tokenizeArgLine);
  const flat = perLine.flat();
  const headCount = flat.length - launch.rest.length;
  const skip = flat.map((_, index) => index < headCount);
  const owned = systemOwnedMask(launch.rest);
  for (let index = 0; index < owned.length; index += 1) {
    if (owned[index]) skip[headCount + index] = true;
  }
  return reassembleLines(rawLines, perLine, skip);
}

// withManagedModelTarget replaces every operator-written model target with a
// single --model line, leaving the remaining editor lines untouched.
export function withManagedModelTarget(value: string, kind: string, path: string): string {
  const normalizedKind = kind.trim().toLowerCase();
  const launch = normalizeManagedLaunch(value, normalizedKind);
  if (launch.entrypoint === "" || !path.trim()) return value;
  const text = extraLaunchArguments(value, normalizedKind);
  const rawLines = text.split("\n");
  const perLine = rawLines.map(tokenizeArgLine);
  const remaining = reassembleLines(rawLines, perLine, modelSelectorMask(perLine.flat()));
  const modelLine = `--model ${serializeLaunchArgument(path.trim())}`;
  return remaining.trim() !== "" ? `${modelLine}\n${remaining}` : modelLine;
}

// rebuildManagedArguments normalizes editor text into the stored backend args.
// Operator lines are preserved verbatim; only system-owned tokens are
// removed. When a model target is supplied — the model file picker — it
// replaces the operator's targets. The entrypoint, bind host and port are
// never stored: the launch binding injects them from the runtime and the
// model's proxy.
export function rebuildManagedArguments(value: string, kind: string, model: string): string {
  const launch = normalizeManagedLaunch(value, kind);
  if (launch.entrypoint === "") return value;
  const text = extraLaunchArguments(value, kind);
  if (model.trim() === "") return text;
  return withManagedModelTarget(text, kind, model);
}

// ownedLaunchFlagSet lists the raw flags the structured launch block owns for
// a runtime kind. The server drops operator-written duplicates of these
// flags and re-injects the structured values at launch (see
// BuildManagedLaunchArguments), so for a managed model the structured fields
// are the source of truth and the editor text must follow them.
function ownedLaunchFlagSet(kind: string): Set<string> {
  if (kind === "vllm") {
    return new Set(["--served-model-name", "--max-model-len", "--max-num-seqs", "--gpu-memory-utilization", "--tensor-parallel-size", "-tp"]);
  }
  if (kind === "llamacpp") {
    return new Set(["--alias", "--ctx-size", "--parallel"]);
  }
  return new Set<string>();
}

// Short flag aliases map onto the canonical long form the editor renders.
const OWNED_FLAG_ALIASES: Record<string, string> = { "-tp": "--tensor-parallel-size" };

// isValidOwnedFlagValue reports whether a structured field would accept a
// value written for its flag. A value it would reject marks a line the
// operator is still typing (or a broken config line), never a value to
// canonicalize.
function isValidOwnedFlagValue(flag: string, value: string): boolean {
  if (value.trim() === "") return false;
  if (flag === "--served-model-name" || flag === "--alias") return true;
  if (flag === "--gpu-memory-utilization") {
    const parsed = Number.parseFloat(value);
    return Number.isFinite(parsed) && parsed > 0 && parsed <= 1;
  }
  if (!/^[+-]?\d+$/.test(value.trim())) return false;
  return Number(value.trim()) > 0;
}

// ownedLaunchFlagValues renders the canonical value each structured field
// contributes to its owned flag. A flag missing from the result is unset and
// its editor line is dropped. The llamacpp context is per-sequence; the
// stored --ctx-size is the per-engine total (context x parallel), mirroring
// launchspec.BuildArgs.
function ownedLaunchFlagValues(kind: string, launch: ModelLaunchDraft, nvidiaGPUs: number): Record<string, string> {
  const values: Record<string, string> = {};
  const context = Number(launch.contextPerRequest);
  const concurrency = Number(launch.maxConcurrency);
  if (kind === "vllm") {
    if (trimmedText(launch.servedModelName) !== "") values["--served-model-name"] = trimmedText(launch.servedModelName);
    if (Number.isInteger(context) && context > 0) values["--max-model-len"] = String(context);
    if (Number.isInteger(concurrency) && concurrency > 0) values["--max-num-seqs"] = String(concurrency);
    const tensorParallelSize = Number(launch.tensorParallelSize);
    if (Number.isInteger(tensorParallelSize) && tensorParallelSize > 0) {
      values["--tensor-parallel-size"] = String(tensorParallelSize);
    }
    const utilization = Number(launch.gpuMemoryUtilization);
    if (trimmedText(launch.gpuMemoryUtilization) !== "" && utilization > 0 && utilization <= 100) {
      values["--gpu-memory-utilization"] = String(Number((utilization / 100).toFixed(6)));
    }
    // Auto parallel strategy: one shard per selected NVIDIA card once there
    // is more than one, matching the server's launch build. Non-NVIDIA cards
    // cannot take a vLLM shard, so they do not count.
    if (!("--tensor-parallel-size" in values) && nvidiaGPUs >= 2) {
      values["--tensor-parallel-size"] = String(nvidiaGPUs);
    }
  } else if (kind === "llamacpp") {
    if (trimmedText(launch.servedModelName) !== "") values["--alias"] = trimmedText(launch.servedModelName);
    if (Number.isInteger(context) && context > 0) {
      const parallel = Number.isInteger(concurrency) && concurrency > 1 ? concurrency : 1;
      values["--ctx-size"] = String(context * parallel);
    }
    if (Number.isInteger(concurrency) && concurrency > 1) values["--parallel"] = String(concurrency);
  }
  return values;
}

// ownedFlagValueKind groups the canonical flags by how their values compare,
// so the text renderer can tell "the operator is still typing" from "the
// field really holds another value".
function ownedFlagValueKind(flag: string): "text" | "fraction" | "count" {
  if (flag === "--served-model-name" || flag === "--alias") return "text";
  if (flag === "--gpu-memory-utilization") return "fraction";
  return "count";
}

// compareOwnedFlagValue classifies a written value against the canonical one
// the structured field renders: "pending" when the value is missing or one
// the field would reject (a line being typed), "same" when the two only
// differ in formatting, and "different" when the field holds another value.
function compareOwnedFlagValue(flag: string, canonical: string, written: string | undefined): "pending" | "same" | "different" {
  if (written === undefined || !isValidOwnedFlagValue(flag, written)) return "pending";
  const kind = ownedFlagValueKind(flag);
  if (kind === "text") return written === canonical ? "same" : "different";
  const parse = kind === "fraction" ? Number.parseFloat : (value: string) => Number(value.trim());
  const left = parse(written);
  const right = parse(canonical);
  if (!Number.isFinite(left) || !Number.isFinite(right)) return "pending";
  return left === right ? "same" : "different";
}

// dropOwnedFlagTokens removes the flag at index plus the value token that
// follows it on the same line, leaving the rest of the line intact.
function dropOwnedFlagTokens(tokens: string[], index: number): string[] {
  const survivors = [...tokens];
  survivors.splice(index, 1);
  if (index < survivors.length && !survivors[index].startsWith("-")) survivors.splice(index, 1);
  return survivors;
}

// syncLaunchOwnedArguments rewrites the launch-owned flag lines of the
// editor text to match the structured fields: an owned line is replaced in
// place only when the field really holds a different value, a missing owned
// flag is appended, and a line whose field is unset is dropped. Every other
// line survives verbatim — and so does an owned line whose value is still
// being typed, or that only differs in formatting (a trailing zero, a leading
// zero, a short alias): rewriting either would move the cursor and discard
// the keystroke that produced it. The editor stores one flag per line; a line
// that leads with an owned flag is that flag's line.
// nvidiaGPUs is the number of selected cards the server can actually hand to
// a CUDA runtime; callers without a hardware catalog pass the full selection
// count, which is the legacy behavior.
export function syncLaunchOwnedArguments(value: string, kind: string, launch: ModelLaunchDraft, nvidiaGPUs?: number): string {
  if (kind !== "vllm" && kind !== "llamacpp") return value;
  const owned = ownedLaunchFlagSet(kind);
  const values = ownedLaunchFlagValues(kind, launch, nvidiaGPUs ?? launch.gpus.length);
  const kept: string[] = [];
  const placed = new Set<string>();
  for (const line of value.split("\n")) {
    const trimmed = line.trim();
    if (trimmed === "") {
      kept.push(line);
      continue;
    }
    const tokens = tokenizeArgLine(trimmed);
    const first = tokens[0] ?? "";
    if (owned.has(first)) {
      const flag = OWNED_FLAG_ALIASES[first] ?? first;
      if (flag in values) {
        placed.add(flag);
        if (compareOwnedFlagValue(flag, values[flag], tokens[1]) !== "different") {
          kept.push(line);
          continue;
        }
        kept.push(`${flag} ${serializeLaunchArgument(values[flag])}`);
        continue;
      }
      // The structured field is unset, so the launch will not pass the flag:
      // drop it (with its same-line value) and keep the rest of the line.
      const survivors = dropOwnedFlagTokens(tokens, 0);
      if (survivors.length > 0) kept.push(survivors.map(serializeLaunchArgument).join(" "));
      continue;
    }
    // Inline form: an owned --flag=value token somewhere in the line.
    const inline = tokens.findIndex((token) => {
      const equals = token.indexOf("=");
      return equals > 0 && owned.has(token.slice(0, equals));
    });
    if (inline >= 0) {
      const token = tokens[inline];
      const rawFlag = token.slice(0, token.indexOf("="));
      const flag = OWNED_FLAG_ALIASES[rawFlag] ?? rawFlag;
      const written = token.slice(token.indexOf("=") + 1);
      if (flag in values) {
        placed.add(flag);
        if (compareOwnedFlagValue(flag, values[flag], written) !== "different") {
          kept.push(line);
          continue;
        }
        const replaced = [...tokens];
        replaced[inline] = `${flag}=${values[flag]}`;
        kept.push(replaced.map(serializeLaunchArgument).join(" "));
        continue;
      }
      const survivors = tokens.filter((_, index) => index !== inline);
      if (survivors.length > 0) kept.push(survivors.map(serializeLaunchArgument).join(" "));
      continue;
    }
    kept.push(line);
  }
  for (const [flag, argument] of Object.entries(values)) {
    if (placed.has(flag)) continue;
    kept.push(`${flag} ${serializeLaunchArgument(argument)}`);
  }
  return kept.join("\n");
}

// CudaEnvSyncState tracks the CUDA environment lines the GPU selection wrote
// into the environment textarea (the device-order pin and the visible-devices
// list) so both can be removed — and the operator's original lines restored —
// when the selection leaves a CUDA card.
export interface CudaEnvLineState {
  managed: string;
  stashed: string;
}

export interface CudaEnvSyncState {
  visible: CudaEnvLineState;
  order: CudaEnvLineState;
}

const CUDA_DEVICE_ORDER_LINE = "CUDA_DEVICE_ORDER=PCI_BUS_ID";

// syncManagedEnvLine keeps one environment line in step with the structured
// value: the line is replaced in place (stashing the operator line it
// overwrote), appended when absent, or removed with its stashed original
// restored when the desired value empties. A line the selection never wrote
// is left alone.
function syncManagedEnvLine(lines: string[], prefix: string, desired: string, lineState: CudaEnvLineState): void {
  const index = lines.findIndex((line) => line.trim().startsWith(prefix));
  const current = index >= 0 ? lines[index].trim() : "";
  if (desired === "") {
    if (index >= 0 && current === lineState.managed && lineState.managed !== "") {
      const stashed = lineState.stashed;
      lines.splice(index, 1);
      if (stashed !== "") lines.splice(index, 0, stashed);
      lineState.managed = "";
      lineState.stashed = "";
    }
    return;
  }
  if (current === desired) {
    lineState.managed = desired;
    return;
  }
  if (index >= 0) {
    if (current !== lineState.managed) lineState.stashed = current;
    lines[index] = desired;
  } else {
    while (lines.length > 0 && lines[lines.length - 1].trim() === "") lines.pop();
    lines.push(desired);
  }
  lineState.managed = desired;
}

// syncCudaEnvLines keeps the environment text in step with the selected CUDA
// cards: the server pins CUDA_DEVICE_ORDER to the PCI bus (so index-based
// selection stays stable) and replaces any operator-written
// CUDA_VISIBLE_DEVICES with the structured value at launch, so the textarea
// mirrors both instead of showing a stale copy.
export function syncCudaEnvLines(value: string, devices: string[], state: CudaEnvSyncState): string {
  const lines = value.split("\n");
  const visible = devices.length > 0 ? `CUDA_VISIBLE_DEVICES=${devices.join(",")}` : "";
  syncManagedEnvLine(lines, "CUDA_VISIBLE_DEVICES=", visible, state.visible);
  if (devices.length > 0) {
    const orderIndex = lines.findIndex((line) => line.trim().startsWith("CUDA_DEVICE_ORDER="));
    if (orderIndex >= 0) {
      syncManagedEnvLine(lines, "CUDA_DEVICE_ORDER=", CUDA_DEVICE_ORDER_LINE, state.order);
    } else {
      // Keep the pair adjacent: the order pin goes right before the
      // visible-devices line when that one exists.
      const visibleIndex = lines.findIndex((line) => line.trim().startsWith("CUDA_VISIBLE_DEVICES="));
      lines.splice(visibleIndex >= 0 ? visibleIndex : lines.length, 0, CUDA_DEVICE_ORDER_LINE);
      state.order.managed = CUDA_DEVICE_ORDER_LINE;
    }
  } else {
    syncManagedEnvLine(lines, "CUDA_DEVICE_ORDER=", "", state.order);
  }
  return lines.join("\n");
}

// cudaVisibleDevicesFromEnv is deliberately small and lexical: the editor
// needs to recover the existing card selection from a legacy environment
// block before it owns that line. CUDA accepts both numeric ordinals and GPU
// UUIDs, so preserve either form for the catalog mapper to resolve.
export function cudaVisibleDevicesFromEnv(value: string): string[] {
  const line = environmentLines(value).find((candidate) => candidate.startsWith("CUDA_VISIBLE_DEVICES="));
  if (!line) return [];
  return line.slice(line.indexOf("=") + 1).split(",").map((device) => device.trim()).filter(Boolean);
}

function pathBase(path: string): string {
  const normalized = path.replaceAll("\\", "/").replace(/\/$/, "");
  return normalized.slice(normalized.lastIndexOf("/") + 1);
}

function pathDirectory(path: string): string {
  const index = Math.max(path.lastIndexOf("/"), path.lastIndexOf("\\"));
  return index > 0 ? path.slice(0, index) : path;
}

function repositorySnapshotPath(file: ManagedModelFile): string {
  if (!file.repository || !file.revision) return "";
  const normalized = file.path.replaceAll("\\", "/");
  const marker = `/snapshots/${file.revision}`;
  const index = normalized.indexOf(marker);
  return index < 0 ? "" : file.path.slice(0, index + marker.length);
}

function managedModelPath(file: ManagedModelFile, kind: string): string {
  const normalizedKind = kind.trim().toLowerCase();
  if (normalizedKind === "llamacpp") return file.path;
  if (normalizedKind !== "vllm") return "";
  if (["gguf", "ggml"].includes(file.format.toLowerCase())) return file.path;
  return repositorySnapshotPath(file) || pathDirectory(file.path);
}

export function managedModelTargets(files: ManagedModelFile[], kind: string): ManagedModelTarget[] {
  const normalizedKind = kind.trim().toLowerCase();
  if (normalizedKind !== "llamacpp" && normalizedKind !== "vllm") return [];
  const targets = new Map<string, ManagedModelTarget>();
  for (const file of files) {
    const format = file.format.toLowerCase();
    if (normalizedKind === "llamacpp" && !["gguf", "ggml"].includes(format)) continue;
    const path = managedModelPath(file, normalizedKind);
    if (!path || targets.has(path)) continue;
    const targetName = normalizedKind === "vllm" && file.repository
      ? `${file.repository}${file.revision ? ` @ ${file.revision.slice(0, 8)}` : ""}`
      : normalizedKind === "vllm" ? pathBase(path) : file.name;
    targets.set(path, {
      value: path,
      label: `${targetName} · ${file.source_id}`,
      path,
      sourceID: file.source_id,
      format,
    });
  }
  return [...targets.values()].sort((left, right) =>
    left.label.localeCompare(right.label, undefined, { numeric: true }) || left.path.localeCompare(right.path),
  );
}

export function managedModelPathFromArguments(value: string, kind: string): string {
  const launch = normalizeManagedLaunch(value, kind);
  for (let index = 0; index < launch.rest.length; index += 1) {
    const flag = launch.rest[index];
    if (MODEL_SELECTOR_FLAGS.includes(flag)) return launch.rest[index + 1] ?? "";
    for (const prefix of MODEL_SELECTOR_PREFIXES) {
      if (flag.startsWith(prefix)) return flag.slice(prefix.length);
    }
  }
  if (launch.kind === "vllm") {
    const positional = launch.rest[0];
    if (positional !== undefined && positional !== "" && !positional.startsWith("-")) return positional;
  }
  return "";
}

export function draftFromModel(id: string, value: unknown): ModelConfigDraft {
  const model = record(value);
  const backend = record(property(model, "backend"));
  const lifecycle = record(property(backend, "lifecycle"));
  const resources = record(property(backend, "resources"));
  const pricing = record(property(backend, "pricing"));
  const capabilities = record(property(model, "capabilities"));
  const filters = record(property(model, "filters"));
  const timeouts = record(property(model, "timeouts"));
  const compat = record(property(model, "compat"));
  const lmcache = record(property(backend, "lmcache"));
  const cmd = text(property(model, "cmd"));
  const runtime = text(property(backend, "runtime"));
  const args = listText(propertyAny(backend, "args", "arguments"));
  const launchMode: LaunchMode = cmd.trim() ? "command" : "backend";
  return {
    ...emptyModelDraft(id),
    name: text(property(model, "name")),
    description: text(property(model, "description")),
    aliases: listText(property(model, "aliases")).join("\n"),
    useModelName: text(property(model, "useModelName")),
    launchMode,
    initialLaunchMode: launchMode,
    cmd,
    cmdStop: text(property(model, "cmdStop")),
    proxy: text(property(model, "proxy")),
    checkEndpoint: text(property(model, "checkEndpoint")),
    env: environmentLines(listText(property(model, "env")).join("\n")).join("\n"),
    ttl: numberText(propertyAny(model, "ttl", "unloadAfter")),
    unloadTimeout: numberText(property(model, "unloadTimeout")),
    concurrencyLimit: numberText(property(model, "concurrencyLimit")),
    unlisted: property(model, "unlisted") === true,
    disabled: property(model, "disabled") === true,
    sendLoadingState: boolText(property(model, "sendLoadingState")),
    backendType: text(property(backend, "type")),
    backendRuntime: runtime,
    backendRuntimeVersion: text(property(backend, "runtimeVersion")),
    backendProtocol: text(property(backend, "protocol")),
    backendAPIs: listText(property(backend, "apis")),
    backendDiscover: boolText(property(backend, "discover")),
    backendArguments: args.join("\n"),
    launch: launchFromBackend(backend),
    backendContainer: cloneRecord(property(backend, "container")),
    lmcacheEnabled: lmcache["enabled"] === true,
    lmcacheMode: text(lmcache["mode"]) === "inProcess" ? "inProcess" : "mp",
    lmcacheRole: text(lmcache["role"]),
    lmcacheHost: text(lmcache["host"]),
    lmcachePort: numberText(lmcache["port"]),
    lmcacheChunkSize: numberText(lmcache["chunkSize"]),
    pricingProvider: text(property(pricing, "provider")),
    pricingModel: text(property(pricing, "model")),
    lifecycleMode: text(property(lifecycle, "mode")),
    sleepLevel: numberText(property(lifecycle, "sleepLevel")),
    vramMiB: numberText(property(resources, "vramMiB")),
    ramMiB: numberText(property(resources, "ramMiB")),
    gpuAffinity: listText(property(resources, "gpuAffinity")).join("\n"),
    priority: numberText(property(resources, "priority")),
    evictionPriority: numberText(property(resources, "evictionPriority")),
    capabilitiesIn: listText(property(capabilities, "in")),
    capabilitiesOut: listText(property(capabilities, "out")),
    capabilitiesTools: property(capabilities, "tools") === true,
    capabilitiesReranker: property(capabilities, "reranker") === true,
    capabilitiesContext: numberText(property(capabilities, "context")),
    filtersStripParams: text(property(filters, "stripParams")),
    filtersSetParams: cloneRecord(property(filters, "setParams")),
    filtersSetParamsByID: cloneRecord(property(filters, "setParamsByID")),
    macros: cloneRecord(property(model, "macros")),
    metadata: cloneRecord(property(model, "metadata")),
    timeoutConnect: numberText(property(timeouts, "connect")),
    timeoutKeepAlive: numberText(property(timeouts, "keepalive")),
    timeoutResponseHeader: numberText(property(timeouts, "responseHeader")),
    timeoutTLSHandshake: numberText(property(timeouts, "tlsHandshake")),
    timeoutExpectContinue: numberText(property(timeouts, "expectContinue")),
    timeoutIdleConn: numberText(property(timeouts, "idleConn")),
    ignoreWebsockets: boolText(property(compat, "ignoreWebsockets")),
  };
}

function lines(value: string): string[] {
  return value.split(/\n|,/).map((item) => item.trim()).filter(Boolean);
}

function args(value: string): string[] {
  // Editor lines are stored verbatim so a hand-edited "flag value" config
  // round-trips byte for byte; the launch splitter shell-splits those items
  // at start. Managed drafts are normalized on load and on every edit, so a
  // pasted command head never reaches storage as one whole item.
  return value.split("\n").map((line) => line.trim()).filter(Boolean);
}

function integer(value: DraftNumberValue, field: string, minimum?: number): number | undefined {
  const normalized = trimmedText(value);
  if (!normalized) return undefined;
  const parsed = Number(normalized);
  if (!Number.isInteger(parsed) || (minimum !== undefined && parsed < minimum)) {
    throw new Error(`${field} must be an integer${minimum === undefined ? "" : ` >= ${minimum}`}`);
  }
  return parsed;
}

function setOptional(target: Record<string, unknown>, key: string, value: unknown): void {
  if (value === undefined || value === "" || (Array.isArray(value) && value.length === 0)) delete target[key];
  else target[key] = value;
}

function setOptionalBool(target: Record<string, unknown>, key: string, value: "" | "true" | "false"): void {
  if (value === "") delete target[key];
  else target[key] = value === "true";
}

function cloneRecord(value: unknown): Record<string, unknown> {
  const source = record(value);
  return { ...source };
}

export function buildModelValue(draft: ModelConfigDraft, current: unknown = {}): Record<string, unknown> {
  const model = cloneRecord(current);
  setOptional(model, "name", trimmedText(draft.name));
  setOptional(model, "description", trimmedText(draft.description));
  setOptional(model, "aliases", lines(draft.aliases));
  setOptional(model, "useModelName", trimmedText(draft.useModelName));
  setOptional(model, "proxy", trimmedText(draft.proxy));
  setOptional(model, "checkEndpoint", trimmedText(draft.checkEndpoint));
  setOptional(model, "env", environmentLines(draft.env));
  setOptional(model, "ttl", integer(draft.ttl, "ttl", -1));
  setOptional(model, "unloadTimeout", integer(draft.unloadTimeout, "unloadTimeout", 0));
  setOptional(model, "concurrencyLimit", integer(draft.concurrencyLimit, "concurrencyLimit", 0));
  setOptional(model, "unlisted", draft.unlisted ? true : undefined);
  setOptional(model, "disabled", draft.disabled ? true : undefined);
  setOptionalBool(model, "sendLoadingState", draft.sendLoadingState);

  const backend = cloneRecord(property(model, "backend"));
  setOptional(backend, "type", trimmedText(draft.backendType));
  setOptional(backend, "protocol", trimmedText(draft.backendProtocol));
  setOptional(backend, "apis", draft.backendAPIs.length > 0 ? [...draft.backendAPIs] : undefined);
  setOptionalBool(backend, "discover", draft.backendDiscover);

  if (draft.launchMode === "command") {
    if (!trimmedText(draft.cmd)) throw new Error("cmd is required for command mode");
    model.cmd = trimmedText(draft.cmd);
    setOptional(model, "cmdStop", trimmedText(draft.cmdStop));
    if (draft.initialLaunchMode === "backend") {
      delete backend.runtime;
      delete backend.runtimeVersion;
      delete backend.args;
      delete backend.container;
    }
  } else {
    const backendArgs = args(draft.backendArguments);
    const container = cloneRecord(draft.backendContainer);
    if (backendArgs.length === 0 && !trimmedText(property(container, "image"))) {
      throw new Error("backend.args or backend.container.image is required for backend mode");
    }
    delete model.cmd;
    delete model.cmdStop;
    setOptional(backend, "runtime", trimmedText(draft.backendRuntime));
    setOptional(backend, "runtimeVersion", trimmedText(draft.backendRuntimeVersion));
    setOptional(backend, "launch", launchToBackend(draft));
    setOptional(backend, "args", backendArgs);
    setOptional(backend, "container", Object.keys(container).length > 0 ? container : undefined);
  }
  // LMCache is a vLLM-backend feature: emit the block only when enabled, and
  // drop the key entirely when disabled so the round-trip stays clean. The
  // connector is installed into the model's vLLM venv at model start by the
  // server, so the block only declares the model's usage.
  if (draft.launchMode === "backend" && draft.lmcacheEnabled) {
    const lmcache: Record<string, unknown> = { enabled: true, mode: draft.lmcacheMode };
    if (trimmedText(draft.lmcacheRole)) setOptional(lmcache, "role", trimmedText(draft.lmcacheRole));
    if (draft.lmcacheMode === "mp") {
      if (trimmedText(draft.lmcacheHost)) setOptional(lmcache, "host", trimmedText(draft.lmcacheHost));
      const port = integer(draft.lmcachePort, "lmcache.port", 1);
      if (port !== undefined && port <= 65535) lmcache["port"] = port;
    } else {
      const chunkSize = integer(draft.lmcacheChunkSize, "lmcache.chunkSize", 0);
      if (chunkSize !== undefined && chunkSize <= 1048576) lmcache["chunkSize"] = chunkSize;
    }
    backend["lmcache"] = lmcache;
  } else {
    delete backend["lmcache"];
  }

  const lifecycle = cloneRecord(property(backend, "lifecycle"));
  setOptional(lifecycle, "mode", trimmedText(draft.lifecycleMode));
  setOptional(lifecycle, "sleepLevel", integer(draft.sleepLevel, "sleepLevel", 0));
  setOptional(backend, "lifecycle", Object.keys(lifecycle).length > 0 ? lifecycle : undefined);

  const resources = cloneRecord(property(backend, "resources"));
  setOptional(resources, "vramMiB", integer(draft.vramMiB, "vramMiB", 0));
  setOptional(resources, "ramMiB", integer(draft.ramMiB, "ramMiB", 0));
  setOptional(resources, "gpuAffinity", lines(draft.gpuAffinity));
  setOptional(resources, "priority", integer(draft.priority, "priority"));
  setOptional(resources, "evictionPriority", integer(draft.evictionPriority, "evictionPriority"));
  setOptional(backend, "resources", Object.keys(resources).length > 0 ? resources : undefined);

  const pricing = cloneRecord(property(backend, "pricing"));
  setOptional(pricing, "provider", trimmedText(draft.pricingProvider));
  setOptional(pricing, "model", trimmedText(draft.pricingModel));
  setOptional(backend, "pricing", Object.keys(pricing).length > 0 ? pricing : undefined);
  setOptional(model, "backend", Object.keys(backend).length > 0 ? backend : undefined);

  const capabilities = cloneRecord(property(model, "capabilities"));
  setOptional(capabilities, "in", draft.capabilitiesIn.length > 0 ? [...draft.capabilitiesIn] : undefined);
  setOptional(capabilities, "out", draft.capabilitiesOut.length > 0 ? [...draft.capabilitiesOut] : undefined);
  setOptional(capabilities, "tools", draft.capabilitiesTools ? true : undefined);
  setOptional(capabilities, "reranker", draft.capabilitiesReranker ? true : undefined);
  setOptional(capabilities, "context", integer(draft.capabilitiesContext, "capabilities.context", 0));
  setOptional(model, "capabilities", Object.keys(capabilities).length > 0 ? capabilities : undefined);

  const filters = cloneRecord(property(model, "filters"));
  setOptional(filters, "stripParams", trimmedText(draft.filtersStripParams));
  const setParams = cloneRecord(draft.filtersSetParams);
  const setParamsByID = cloneRecord(draft.filtersSetParamsByID);
  setOptional(filters, "setParams", Object.keys(setParams).length > 0 ? setParams : undefined);
  setOptional(filters, "setParamsByID", Object.keys(setParamsByID).length > 0 ? setParamsByID : undefined);
  setOptional(model, "filters", Object.keys(filters).length > 0 ? filters : undefined);

  const macros = cloneRecord(draft.macros);
  const metadata = cloneRecord(draft.metadata);
  setOptional(model, "macros", Object.keys(macros).length > 0 ? macros : undefined);
  setOptional(model, "metadata", Object.keys(metadata).length > 0 ? metadata : undefined);

  const timeouts = cloneRecord(property(model, "timeouts"));
  setOptional(timeouts, "connect", integer(draft.timeoutConnect, "timeouts.connect", 0));
  setOptional(timeouts, "keepalive", integer(draft.timeoutKeepAlive, "timeouts.keepalive", 0));
  setOptional(timeouts, "responseHeader", integer(draft.timeoutResponseHeader, "timeouts.responseHeader", 0));
  setOptional(timeouts, "tlsHandshake", integer(draft.timeoutTLSHandshake, "timeouts.tlsHandshake", 0));
  setOptional(timeouts, "expectContinue", integer(draft.timeoutExpectContinue, "timeouts.expectContinue", 0));
  setOptional(timeouts, "idleConn", integer(draft.timeoutIdleConn, "timeouts.idleConn", 0));
  setOptional(model, "timeouts", Object.keys(timeouts).length > 0 ? timeouts : undefined);

  const compat = cloneRecord(property(model, "compat"));
  setOptionalBool(compat, "ignoreWebsockets", draft.ignoreWebsockets);
  setOptional(model, "compat", Object.keys(compat).length > 0 ? compat : undefined);
  return model;
}

export function pointerToken(value: string): string {
  return value.replaceAll("~", "~0").replaceAll("/", "~1");
}

export function buildModelPatch(config: unknown, draft: ModelConfigDraft, editing = false): unknown[] {
  const id = trimmedText(draft.id);
  if (!id) throw new Error("model id is required");
  const current = editing ? modelEntries(config)[id] : undefined;
  const value = buildModelValue(draft, current);
  const models = modelEntries(config);
  if (!editing && Object.prototype.hasOwnProperty.call(models, id)) {
    throw new Error(`model ${id} already exists`);
  }
  if (Object.keys(models).length === 0 && !property(record(config), "models")) {
    return [{ op: "add", path: "/models", value: { [id]: value } }];
  }
  return [{ op: "add", path: `/models/${pointerToken(id)}`, value }];
}

function equalValue(left: unknown, right: unknown): boolean {
  try {
    return JSON.stringify(left) === JSON.stringify(right);
  } catch {
    return Object.is(left, right);
  }
}

export function summarizeChanges(before: unknown, after: unknown, prefix = ""): ConfigChange[] {
  if (equalValue(before, after)) return [];
  const beforeRecord = record(before);
  const afterRecord = record(after);
  const beforeIsObject = before !== null && typeof before === "object" && !Array.isArray(before);
  const afterIsObject = after !== null && typeof after === "object" && !Array.isArray(after);
  if ((beforeIsObject || before === undefined) && (afterIsObject || after === undefined)) {
    const keys = new Set([...Object.keys(beforeRecord), ...Object.keys(afterRecord)]);
    return [...keys].sort().flatMap((key) => summarizeChanges(
      beforeRecord[key],
      afterRecord[key],
      prefix ? `${prefix}.${key}` : key,
    ));
  }
  if (before === undefined) return [{ path: prefix, after, kind: "added" }];
  if (after === undefined) return [{ path: prefix, before, kind: "removed" }];
  return [{ path: prefix, before, after, kind: "changed" }];
}
