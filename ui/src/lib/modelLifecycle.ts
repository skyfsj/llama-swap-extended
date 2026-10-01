// Pure, Svelte-free helpers that translate between the internal numeric
// lifecycle/config values and the modes a human reasons about. The config uses
// sentinel numbers (ttl -1 = inherit global, 0 = never unload, N>0 = N seconds)
// that users should not have to memorise; the UI owns this mapping.

export type TtlMode = "inherit" | "never" | "custom";
export type UnloadMode = "global" | "custom";

export interface TtlState {
  mode: TtlMode;
  seconds: string;
}

export interface UnloadState {
  mode: UnloadMode;
  seconds: string;
}

export const TTL_INHERIT = -1;
export const TTL_NEVER = 0;

function asFiniteNumber(value: unknown): number | undefined {
  if (typeof value === "string" && value.trim() === "") return undefined;
  const number = Number(value);
  return Number.isFinite(number) ? number : undefined;
}

/** Read a raw `ttl` value (or absence) into a UI mode + seconds. */
export function readTtl(value: unknown): TtlState {
  const number = asFiniteNumber(value);
  if (number === undefined || number === TTL_INHERIT) return { mode: "inherit", seconds: "" };
  if (number === TTL_NEVER) return { mode: "never", seconds: "" };
  return { mode: "custom", seconds: String(Math.round(number)) };
}

/** Translate a UI mode + seconds back into the raw `ttl` number. */
export function writeTtl(state: TtlState): number {
  if (state.mode === "inherit") return TTL_INHERIT;
  if (state.mode === "never") return TTL_NEVER;
  const seconds = Number(state.seconds);
  return Number.isInteger(seconds) && seconds > 0 ? seconds : TTL_NEVER;
}

/** Read a raw `unloadTimeout` number (or absence) into a UI mode + seconds. */
export function readUnload(value: unknown): UnloadState {
  const number = asFiniteNumber(value);
  if (number === undefined || number <= 0) return { mode: "global", seconds: "" };
  return { mode: "custom", seconds: String(Math.round(number)) };
}

/** Translate a UI mode + seconds back into the raw `unloadTimeout` number. */
export function writeUnload(state: UnloadState): number | undefined {
  if (state.mode === "global") return 0;
  const seconds = Number(state.seconds);
  return Number.isInteger(seconds) && seconds >= 0 ? seconds : undefined;
}

// A built-in macro available to every model's cmd / cmdStop / proxy. `PORT` is
// only legal in cmd/proxy, `PID` only in cmdStop, and `MODEL_ID` anywhere.
export interface MacroRef {
  token: string;
  label: string;
  scope: "cmd" | "cmdStop" | "any";
}

export const BUILTIN_MACROS: MacroRef[] = [
  { token: "${PORT}", label: "Port", scope: "cmd" },
  { token: "${MODEL_ID}", label: "Model ID", scope: "any" },
  { token: "${PID}", label: "Process ID", scope: "cmdStop" },
];

/** Common environment macros, offered for convenience (values from the host env). */
export const ENV_MACRO_SUGGESTIONS = ["HF_TOKEN", "HOME", "CUDA_VISIBLE_DEVICES", "MODELSCOPE_API_TOKEN"];

export interface MacroOption {
  token: string;
  label: string;
  source: "builtin" | "env" | "global" | "model";
}

/**
 * Build the macro insert list for the command editor: built-ins first, then
 * user-defined global macros, then model macros (a model macro of the same
 * name overrides the global one), plus common env suggestions. Deduped by the
 * bare name so a global and model macro of the same name appear once.
 */
export function macroOptions(globalMacros: Record<string, unknown>, modelMacros: Record<string, unknown>): MacroOption[] {
  const seen = new Set<string>();
  const out: MacroOption[] = [];
  const push = (option: MacroOption): void => {
    const bare = option.token.replace(/^\$\{/, "").replace(/\}$/, "");
    if (seen.has(bare)) return;
    seen.add(bare);
    out.push(option);
  };
  for (const macro of BUILTIN_MACROS) push({ token: macro.token, label: macro.label, source: "builtin" });
  for (const name of Object.keys(globalMacros).sort()) push({ token: `${name}`, label: `${name} · global`, source: "global" });
  for (const name of Object.keys(modelMacros).sort()) push({ token: `${name}`, label: `${name} · model`, source: "model" });
  for (const name of ENV_MACRO_SUGGESTIONS) push({ token: `env.${name}`, label: `env.${name} · env`, source: "env" });
  return out;
}

export interface EnvEntry {
  key: string;
  value: string;
}

/**
 * Parse a `NAME=VALUE` multi-line env block into ordered pairs. Lines without a
 * `=` (or an empty name) and blank lines are ignored. Order is preserved so the
 * editor can display and re-serialize the block without reordering it. The
 * shape matches the settings KV editor's row type.
 */
export function parseEnvLines(block: string): EnvEntry[] {
  const out: EnvEntry[] = [];
  for (const raw of block.split(/\r?\n/)) {
    const line = raw.trim();
    if (!line) continue;
    const eq = line.indexOf("=");
    if (eq <= 0) continue;
    out.push({ key: line.slice(0, eq).trim(), value: line.slice(eq + 1).trim() });
  }
  return out;
}

/** Serialize ordered env pairs back into the `NAME=VALUE` block. */
export function serializeEnvLines(entries: EnvEntry[]): string {
  return entries.filter((entry) => entry.key).map((entry) => `${entry.key}=${entry.value}`).join("\n");
}
