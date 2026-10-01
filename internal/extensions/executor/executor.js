// Node extension executor. Runs under the system Node LTS with a hardened
// runtime: no require/import of Node builtins survives into the bundle, code
// generation is disabled, and the process talks the same line protocol as the
// goja worker (stdin request → {"type":"log"} → {"type":"call"} → response).
// The bundle is built by `go run ./internal/extensions/sdk/executor-build.go`
// and committed so `go build` alone still works.

import { createInterface } from "node:readline";

// ---- protocol plumbing (mirrors internal/extensions/worker.go) ----

let nextCallID = 0;
const pendingCalls = new Map();

function writeLine(obj) {
  process.stdout.write(JSON.stringify(obj) + "\n");
}

/** writeResponse emits the terminal response with output as raw JSON. */
function writeResponse(outputRaw, error) {
  process.stdout.write('{"type":"response","output":' + (outputRaw === undefined ? 'null' : outputRaw) + (error ? ',"error":' + JSON.stringify(error) : '') + '}\n');
}

/** hostCall mirrors the goja bridge: emits a call line, returns a promise.
 * args must be an object; writeLine stringifies the whole line once. */
function hostCall(fn, args, extensionID) {
  const id = ++nextCallID;
  writeLine({ type: "call", id, fn, args, extension: extensionID });
  return new Promise((resolve, reject) => {
    pendingCalls.set(id, { resolve, reject });
  });
}

// ---- context surface (parity with goja makeContext) ----

function buildContext(request, manifest, extensionID, config, callLog) {
  const log = (level) => (message) => {
    const text = typeof message === "string" ? message : JSON.stringify(message);
    callLog(level, text);
    return undefined;
  };
  return {
    requestId: request.requestId ?? "",
    requestedModel: request.requestedModel ?? "",
    resolvedModel: request.resolvedModel ?? "",
    profile: request.profile ?? "",
    provider: request.provider ?? "",
    endpoint: request.endpoint ?? "",
    stream: request.stream ?? false,
    locale: request.locale ?? "zh-CN",
    extension: extensionID,
    config,
    abortSignal: { aborted: false },
    log: { debug: log("debug"), info: log("info"), warn: log("warn"), error: log("error") },
    console: {
      debug: log("debug"), log: log("info"), info: log("info"), warn: log("warn"), error: log("error"),
    },
    models: request.models ?? [],
    session: request.session ?? undefined,
    kv: {
      get: (key, options) => hostCall("kv.get", { key, ...options }, extensionID),
      set: (key, value, options) => hostCall("kv.set", { key, value, ...options }, extensionID),
      delete: (key, options) => hostCall("kv.delete", { key, ...options }, extensionID),
      keys: (prefix, options) => hostCall("kv.keys", { prefix, ...options }, extensionID),
    },
    forward: (model, request2) => hostCall("models.forward", { model, request: request2 }, extensionID),
    usage: () => hostCall("session.usage", {}, extensionID),
    http: {
      fetch: (url, options) => hostCall("http.fetch", { url, options: options ?? {} }, extensionID).then((result) => {
        if (result && typeof result === "object" && "status" in result) {
          return { ...result, text: () => result.body ?? "", json: () => JSON.parse(result.body ?? "null") };
        }
        return result;
      }),
    },
    files: {
      read: (path) => hostCall("files.read", { path }, extensionID),
      write: (path, content) => hostCall("files.write", { path, content }, extensionID),
    },
  };
}

// ---- main loop ----

const state = { extension: null, extensionID: "" };

async function runHook(line) {
  const { bundle, manifest, hook, context, input, config } = line;
  state.extensionID = manifest.id;
  // The bundle is an ESM module (it may use `export default`); wrap it in a
  // module-friendly async wrapper and capture the namespace. The SDK alias is
  // already resolved inside the bundle by esbuild on the Go side.
  const module = await extractModule(bundle);
  const object = module?.default ?? {};
  state.extension = object;
  const callLog = (level, message) => {
    writeLine({ type: "log", level, message, hook });
  };
  const ctx = buildContext(context, manifest, manifest.id, config ?? {}, callLog);
  // Bare console.* must reach the same log stream (parity with the goja
  // worker, which installs a per-hook global console).
  globalThis.console = { debug: callLog.bind(null, "debug"), log: callLog.bind(null, "info"), info: callLog.bind(null, "info"), warn: callLog.bind(null, "warn"), error: callLog.bind(null, "error") };

  let input_value = input;
  if (hook === "onToolCall" && !object.onToolCall) {
    const tools = Array.isArray(object.tools) ? object.tools : [];
    const bound = tools.find((tool) => tool?.__llamaSwapTool === true && tool.name === input?.name);
    if (bound && typeof bound.handler === "function") {
      let result;
      try {
        result = await bound.handler(input?.arguments ?? {}, ctx);
      } catch (error) {
        writeResponse(undefined, String(error?.message ?? error));
        return;
      }
      writeResponse(JSON.stringify(result ?? input));
      return;
    }
  }

  const fn = object[hook];
  if (typeof fn !== "function") {
    writeResponse(JSON.stringify(input));
    return;
  }
  let value;
  try {
    value = await fn.call(object, ctx, input_value);
  } catch (error) {
    writeResponse(undefined, String(error?.message ?? error));
    return;
  }
  if (value === undefined || value === null) {
    writeResponse(JSON.stringify(input));
    return;
  }
  writeResponse(JSON.stringify(value));
}

/** extractModule evaluates a bundle string and returns the extension module.
 * Two bundle shapes exist: ESM (export default) and the goja-style IIFE that
 * assigns a global. Handle both. */
async function extractModule(bundle) {
  if (/^\s*export\s+default/m.test(bundle)) {
    const importBase64 = Buffer.from(bundle, "utf8").toString("base64");
    return import("data:text/javascript;base64," + importBase64);
  }
  // The IIFE assigns __llamaSwapExtension via var at the global scope of the
  // eval context; capture it by appending a read of the global.
  const globalName = "__llamaSwapExtension";
  const evaluate = new Function(`${bundle}\n;return typeof ${globalName} === "object" ? ${globalName} : globalThis[${JSON.stringify(globalName)}];`);
  const namespace = evaluate();
  return { default: namespace?.default ?? namespace };
}

function handleResultLine(lineObj) {
  const pending = pendingCalls.get(lineObj.id);
  if (!pending) return;
  pendingCalls.delete(lineObj.id);
  if (lineObj.ok) pending.resolve(lineObj.value);
  else pending.reject(new Error(lineObj.error || "host call failed"));
}

async function main() {
  const rl = createInterface({ input: process.stdin, crlfDelay: Infinity });
  let request = null;
  rl.on("line", (raw) => {
    let parsed;
    try {
      parsed = JSON.parse(raw);
    } catch {
      return;
    }
    if (parsed.type === "result") {
      handleResultLine(parsed);
      return;
    }
    if (parsed.type === "response") return; // echo guard
    request = parsed; // the workerRequest line
    runHook(request)
      .catch((error) => writeLine({ type: "response", error: String(error) }));
  });
}

main().catch((error) => {
  writeResponse(undefined, String(error));
  process.exit(1);
});
