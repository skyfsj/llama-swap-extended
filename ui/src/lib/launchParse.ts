import { serializeLaunchArgument, tokenizeArgLine, type ModelLaunchDraft } from "./modelConfig";

// ParsedLaunch is the structured subset of an engine launch command that the
// model configuration owns. It mirrors the server's launchspec.Parse
// contract: everything the structured fields do not manage stays in
// extraArgs verbatim, and every list is a real (possibly empty) array, so
// callers never dereference a null.
export interface ParsedLaunch {
  model: string;
  servedModelName: string;
  contextPerRequest: number;
  maxConcurrency: number;
  gpuMemoryUtilization: number | null;
  tensorParallelSize: number;
  cudaVisibleDevices: string[];
  extraArgs: string[];
  warnings: string[];
  // pendingFields lists structured fields whose flag is present in the text
  // but carries no usable value: the operator is editing that line, or the
  // config line is genuinely broken. The field keeps its current value so the
  // line is neither dropped nor rewritten under the cursor; only a line that
  // disappears entirely clears the field.
  pendingFields: string[];
}

// reservedArgs reports which structured field owns a canonical flag for a
// runtime kind, so the same flag never lands in both the launch block and
// the extra arguments.
function reservedArgs(kind: string): Record<string, string> {
  if (kind === "vllm") {
    return {
      model: "--model",
      "model-path": "--model",
      "served-model-name": "--served-model-name",
      "max-model-len": "--max-model-len",
      "max-num-seqs": "--max-num-seqs",
      "gpu-memory-utilization": "--gpu-memory-utilization",
      "tensor-parallel-size": "--tensor-parallel-size",
    };
  }
  return {
    model: "--model",
    alias: "--alias",
    "ctx-size": "--ctx-size",
    parallel: "--parallel",
    "kv-unified-per-slot": "--kv-unified-per-slot",
    device: "--device",
  };
}

// ownedFieldForFlag names the structured field a canonical flag feeds, so a
// pending (valueless or unparseable) line can be reported per field instead
// of per flag spelling.
function ownedFieldForFlag(flag: string): string {
  switch (flag) {
    case "--served-model-name":
    case "--alias":
      return "servedModelName";
    case "--max-model-len":
    case "--ctx-size":
    case "--kv-unified-per-slot":
      return "contextPerRequest";
    case "--max-num-seqs":
    case "--parallel":
      return "maxConcurrency";
    case "--gpu-memory-utilization":
      return "gpuMemoryUtilization";
    case "--tensor-parallel-size":
      return "tensorParallelSize";
    case "--model":
      return "model";
    default:
      return flag;
  }
}

// isFlagToken reports whether an argv token opens a flag rather than carries
// a value. It decides when a flag may consume the token after it: the model
// editor keeps one flag per line, so a reserved flag written without a value
// must never swallow the flag on the following line.
function isFlagToken(token: string): boolean {
  return token.startsWith("-") && token.length > 1;
}

// canonicalFlag maps short aliases to their long forms so the rest of the
// parser never repeats alias checks.
function canonicalFlag(name: string): string {
  if (name.startsWith("-") && !name.startsWith("--")) {
    const aliases: Record<string, string> = {
      "-m": "model",
      "-a": "alias",
      "-c": "ctx-size",
      "-np": "parallel",
      "-tp": "tensor-parallel-size",
      "-hp": "host",
    };
    const mapped = aliases[name];
    return mapped ?? name.slice(1);
  }
  return name.startsWith("--") ? name.slice(2) : name;
}

// validEnvName accepts the identifier shape a shell assignment takes before
// the executable.
function validEnvName(name: string): boolean {
  return /^[A-Za-z0-9_]+$/.test(name);
}

// stripInvocation removes the launcher prefix: a python -m module form, a
// vllm/serve, or a llama-server path. The executable only decides how the
// command head is dropped; the managed runtime owns the real entrypoint at
// start.
function stripInvocation(kind: string, tokens: string[]): string[] {
  let rest = tokens;
  while (rest.length > 0) {
    const bare = rest[0].replace(/^\.\//, "").replace(/^\//, "").toLowerCase();
    if (bare === "python" || bare === "python3" || bare.endsWith("python")) {
      rest = rest.slice(1);
      if (rest.length > 0 && (rest[0] === "-m" || rest[0] === "--module")) {
        rest = rest.slice(1);
        if (rest.length > 0) rest = rest.slice(1);
      }
      continue;
    }
    if (bare === "vllm" || bare === "llama-server" || bare === "llama.cpp/llama-server") {
      rest = rest.slice(1);
      continue;
    }
    if (bare === "serve" && kind === "vllm") {
      rest = rest.slice(1);
      continue;
    }
    break;
  }
  return rest;
}

// parseIntStrict is the Atoi shape: an optional sign and digits only.
function parseIntStrict(value: string): number | null {
  if (!/^[+-]?\d+$/.test(value)) return null;
  return Number(value);
}

function splitDeviceList(value: string): string[] {
  return value.split(",").map((part) => part.trim()).filter((part) => part !== "");
}

interface MutableSpec extends ParsedLaunch {
  kvUnifiedPerSlot: number;
}

// applyFlag moves one owned flag's value into its structured field, keeping
// the first occurrence and warning about values the engine would reject. A
// value the field cannot accept leaves the flag in pendingFields so the
// editor keeps its line instead of dropping it under the cursor.
function applyFlag(spec: MutableSpec, owner: string, value: string): void {
  const pending = () => spec.pendingFields.push(ownedFieldForFlag(owner));
  switch (owner) {
    case "--model":
      if (spec.model === "") spec.model = value;
      return;
    case "--served-model-name":
    case "--alias":
      if (spec.servedModelName === "") spec.servedModelName = value;
      return;
    case "--max-model-len":
    case "--kv-unified-per-slot":
    case "--ctx-size": {
      const parsed = parseIntStrict(value);
      if (parsed === null || parsed <= 0) {
        spec.warnings.push(`${owner} 的值 ${JSON.stringify(value)} 不是有效的 token 数`);
        pending();
        return;
      }
      if (owner === "--kv-unified-per-slot") {
        spec.kvUnifiedPerSlot = parsed;
        spec.contextPerRequest = parsed;
        return;
      }
      if (spec.contextPerRequest === 0) spec.contextPerRequest = parsed;
      return;
    }
    case "--max-num-seqs":
    case "--parallel": {
      const parsed = parseIntStrict(value);
      if (parsed === null || parsed <= 0) {
        spec.warnings.push(`${owner} 的值 ${JSON.stringify(value)} 不是有效的并发数`);
        pending();
        return;
      }
      if (spec.maxConcurrency === 0) spec.maxConcurrency = parsed;
      return;
    }
    case "--gpu-memory-utilization": {
      const parsed = Number.parseFloat(value);
      if (!Number.isFinite(parsed) || parsed <= 0 || parsed > 1) {
        spec.warnings.push(`--gpu-memory-utilization 的值 ${JSON.stringify(value)} 不在 (0,1] 内`);
        pending();
        return;
      }
      spec.gpuMemoryUtilization = parsed;
      return;
    }
    case "--tensor-parallel-size": {
      const parsed = parseIntStrict(value);
      if (parsed === null || parsed <= 0) {
        spec.warnings.push(`--tensor-parallel-size 的值 ${JSON.stringify(value)} 无效`);
        pending();
        return;
      }
      spec.tensorParallelSize = parsed;
      return;
    }
    case "--device":
      if (spec.cudaVisibleDevices.length === 0) spec.cudaVisibleDevices = splitDeviceList(value);
      return;
  }
}

// parseLaunchCommand turns a pasted full command (or a bare argument list)
// into the structured launch fields plus the remaining unmanaged arguments.
// Parsing is purely lexical — the text is never executed — and line-aware:
// tokenizeArgLine runs per line, so a stray unbalanced quote (the doubled
// '' people copy out of terminals) can only absorb the rest of its own line
// and is dropped, never swallow the following lines the way a whole-input
// split would. Unknown flags are never dropped; flags the structured launch
// block owns are moved into their fields and the editor keeps the rest.
export function parseLaunchCommand(kind: string, command: string): ParsedLaunch {
  const spec: MutableSpec = {
    model: "",
    servedModelName: "",
    contextPerRequest: 0,
    maxConcurrency: 0,
    gpuMemoryUtilization: null,
    tensorParallelSize: 0,
    cudaVisibleDevices: [],
    extraArgs: [],
    warnings: [],
    pendingFields: [],
    kvUnifiedPerSlot: 0,
  };

  // Continuations are joined first so a backslash-wrapped shell command
  // tokenizes as the one line the shell would see.
  const joined = command.replaceAll("\\\n", " ");
  const tokens: string[] = [];
  for (const line of joined.split("\n")) tokens.push(...tokenizeArgLine(line));

  let position = 0;
  while (position < tokens.length) {
    const token = tokens[position];
    const equals = token.indexOf("=");
    if (equals <= 0) break;
    const name = token.slice(0, equals);
    if (!validEnvName(name)) break;
    // CUDA_VISIBLE_DEVICES becomes the GPU selection; other assignments have
    // no structured field and are dropped, same as the server parser.
    if (name === "CUDA_VISIBLE_DEVICES") spec.cudaVisibleDevices = splitDeviceList(token.slice(equals + 1));
    position += 1;
  }

  const rest = stripInvocation(kind, tokens.slice(position));
  const reserved = reservedArgs(kind);
  const extras: string[] = [];
  for (let index = 0; index < rest.length; index += 1) {
    const token = rest[index];
    let name = token;
    let value = "";
    let inline = false;
    const equals = token.indexOf("=");
    if (equals > 0) {
      name = token.slice(0, equals);
      value = token.slice(equals + 1);
      inline = true;
    }
    const canonical = canonicalFlag(name);
    const owner = reserved[canonical];
    if (!owner) {
      // Host and port are owned by llama-swap: the managed engine listens
      // where the model's proxy points, so operator duplicates are dropped.
      if (canonical === "host" || canonical === "port") {
        if (!inline && !isFlagToken(rest[index + 1] ?? "")) index += 1;
        continue;
      }
      extras.push(token);
      continue;
    }
    if (!inline) {
      const next = rest[index + 1] ?? "";
      if (isFlagToken(next) || next === "") {
        // A value on the following line is only taken when that line starts
        // with the value; the next flag (or the end of the text) means the
        // operator cleared this one and is typing a new value.
        spec.pendingFields.push(ownedFieldForFlag(owner));
        if (next === "") spec.warnings.push(`${token} 缺少值，已保留为普通参数`);
        extras.push(token);
        continue;
      }
      value = rest[index + 1];
      index += 1;
    }
    applyFlag(spec, owner, value);
  }

  // For vllm the first non-flag extra is the positional model target.
  if (kind === "vllm" && spec.model === "" && extras.length > 0 && !extras[0].startsWith("-")) {
    spec.model = extras[0];
    extras.splice(0, 1);
  }
  spec.extraArgs = extras;

  // A bare llama.cpp ctx-size is the total context, shared across the
  // parallel slots; the per-request field stores the per-slot share.
  if (kind === "llamacpp" && spec.contextPerRequest > 0 && spec.maxConcurrency > 1 && spec.kvUnifiedPerSlot === 0) {
    if (spec.contextPerRequest % spec.maxConcurrency !== 0) {
      spec.warnings.push(`ctx-size ${spec.contextPerRequest} 无法平均分配给 ${spec.maxConcurrency} 个并发槽位，已按原值保留，请确认`);
    } else {
      spec.contextPerRequest /= spec.maxConcurrency;
    }
  }
  return spec;
}

// stripOwnedLaunchArguments removes the flags the structured launch block
// owns from a command-line text: the reserved engine flags plus the
// llama-swap-owned bind host and port. The server's config validator rejects
// a backend.launch.model whose backend.args still repeat them ("backend.args
// --model is managed by backend.launch"), so once the launch block carries
// the model target the stored args keep only the surviving extras. A line
// that loses nothing is kept verbatim; a line that loses tokens is rebuilt
// from its survivors with the editor's quoting so re-tokenizing yields the
// same values.
export function stripOwnedLaunchArguments(kind: string, value: string): string {
  const reserved = reservedArgs(kind);
  const kept: string[] = [];
  for (const line of value.split("\n")) {
    const tokens = tokenizeArgLine(line);
    if (tokens.length === 0) continue;
    const survivors: string[] = [];
    for (let index = 0; index < tokens.length; index += 1) {
      const token = tokens[index];
      const equals = token.indexOf("=");
      const name = equals > 0 ? token.slice(0, equals) : token;
      const canonical = canonicalFlag(name);
      if (canonical === "host" || canonical === "port" || reserved[canonical]) {
        // A separate value token belongs to the dropped flag; the inline
        // "--flag=value" form is fully contained in the token itself. The
        // following flag is not a value, so it survives with its own line.
        if (equals <= 0 && !isFlagToken(tokens[index + 1] ?? "")) index += 1;
        continue;
      }
      survivors.push(token);
    }
    if (survivors.length === 0) continue;
    kept.push(survivors.length === tokens.length ? line.trim() : survivors.map(serializeLaunchArgument).join(" "));
  }
  return kept.join("\n");
}

// OwnedNumericField lists the structured launch fields that hold a plain
// count. The GPU selection and the served name are not read back from the
// args text: the card picker owns the former and the derived model name owns
// the latter.
type OwnedNumericField = "contextPerRequest" | "maxConcurrency" | "tensorParallelSize";

// adoptLaunchFields reads the operator's editor text back into the structured
// launch fields: the model target (operator content the args text may carry,
// from a --model line or a leading positional) plus the numeric flags the
// structured fields own. A flag whose line is mid-edit — present in the text
// without a usable value — keeps its field, so retyping a value in place
// cannot clear it; only a line that is gone entirely clears it. Everything
// else in the draft, the GPU selection included, is preserved.
export function adoptLaunchFields(kind: string, value: string, launch: ModelLaunchDraft): ModelLaunchDraft {
  if (kind !== "vllm" && kind !== "llamacpp") return launch;
  let parsed: ParsedLaunch;
  try {
    parsed = parseLaunchCommand(kind, value);
  } catch {
    return launch;
  }
  const pending = new Set(parsed.pendingFields);
  const next: ModelLaunchDraft = { ...launch, gpus: [...launch.gpus] };
  if (parsed.model !== "") next.model = parsed.model;
  const adopt = (field: OwnedNumericField, parsedValue: number): void => {
    if (parsedValue > 0) {
      next[field] = String(parsedValue);
      return;
    }
    if (pending.has(field)) return;
    next[field] = "";
  };
  adopt("contextPerRequest", parsed.contextPerRequest);
  adopt("maxConcurrency", parsed.maxConcurrency);
  adopt("tensorParallelSize", parsed.tensorParallelSize);
  const utilization = parsed.gpuMemoryUtilization;
  if (utilization !== null && utilization > 0) {
    next.gpuMemoryUtilization = String(Number((utilization * 100).toFixed(6)));
  } else if (!pending.has("gpuMemoryUtilization")) {
    next.gpuMemoryUtilization = "";
  }
  return next;
}
