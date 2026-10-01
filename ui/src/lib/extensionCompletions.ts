import type { CompletionContext, CompletionResult } from "@codemirror/autocomplete";

/**
 * A curated completion vocabulary for the extension API. The code runs in a
 * sandbox that deliberately has no Node bindings, so the set of things an author
 * can call is small and known: this is the whole surface, generated from
 * docs/extensions/extension.d.ts and the runtime sandbox in
 * internal/extensions/worker.go.
 */

interface Snippet {
  label: string;
  detail: string;
  apply: string;
}

const hookSnippets: Snippet[] = [
  { label: "tools", detail: "function tools", apply: "tools: []" },
  { label: "settings", detail: "declared settings form", apply: "settings: {\n  ${}\n}" },
  { label: "onRequest", detail: "before the request is forwarded", apply: "async onRequest(ctx, request) {\n  ${}\n}" },
  { label: "onBeforeForward", detail: "last pass before forwarding", apply: "async onBeforeForward(ctx, request) {\n  ${}\n}" },
  { label: "onToolCall", detail: "server-side tool execution", apply: "async onToolCall(ctx, call) {\n  ${}\n}" },
  { label: "onToolResult", detail: "rewrite a tool result", apply: "async onToolResult(ctx, result) {\n  return result;\n}" },
  { label: "onResponse", detail: "after the response arrives", apply: "async onResponse(ctx, response) {\n  return response;\n}" },
  { label: "onStreamEvent", detail: "per SSE event", apply: "async onStreamEvent(ctx, event) {\n  return event;\n}" },
  { label: "onError", detail: "when a hook throws", apply: "async onError(ctx, error) {\n  ${}\n}" },
];

const contextMembers: Snippet[] = [
  { label: "requestId", detail: "string", apply: "requestId" },
  { label: "requestedModel", detail: "model the client asked for", apply: "requestedModel" },
  { label: "resolvedModel", detail: "model after routing", apply: "resolvedModel" },
  { label: "profile", detail: "string", apply: "profile" },
  { label: "provider", detail: "string", apply: "provider" },
  { label: "endpoint", detail: '"chat.completions" | "responses" | "anthropic.messages"', apply: "endpoint" },
  { label: "stream", detail: "boolean", apply: "stream" },
  { label: "extension", detail: "this extension's id", apply: "extension" },
  { label: "config", detail: "declared settings values", apply: "config" },
  { label: "abortSignal", detail: "{ aborted: boolean }", apply: "abortSignal" },
  { label: "log", detail: "log.debug/info/warn/error", apply: "log" },
  { label: "http", detail: "http.fetch(url, options)", apply: "http" },
  { label: "files", detail: "files.read/files.write", apply: "files" },
  { label: "kv", detail: "kv.get/set/delete/keys", apply: "kv" },
  { label: "locale", detail: "caller's UI locale", apply: "locale" },
  { label: "models", detail: "Array<{ id, name }>", apply: "models" },
  { label: "session", detail: "{ id, keyId, anonymous }", apply: "session" },
  { label: "forward", detail: "forward(model, request) => Promise<{status, body}>", apply: "forward" },
  { label: "usage", detail: "usage() => Promise<{ requests, inputTokens, outputTokens, window }>", apply: "usage" },
];

const logMembers = [
  { label: "debug", detail: "(message: string) => void" },
  { label: "info", detail: "(message: string) => void" },
  { label: "warn", detail: "(message: string) => void" },
  { label: "error", detail: "(message: string) => void" },
];

const httpMembers = [
  { label: "fetch", detail: "fetch(url, { method, headers, body }) => Promise<Response>" },
];

const filesMembers = [
  { label: "read", detail: "read(path) => Promise<string>" },
  { label: "write", detail: "write(path, content) => Promise<boolean>" },
];

const kvMembers = [
  { label: "get", detail: "get(key, { scope }) => Promise<value | undefined>" },
  { label: "set", detail: "set(key, value, { scope, ttlSeconds }) => Promise<boolean>" },
  { label: "delete", detail: "delete(key, { scope }) => Promise<boolean>" },
  { label: "keys", detail: "keys(prefix, { scope }) => Promise<string[]>" },
];

const abortSignalMembers = [{ label: "aborted", detail: "boolean" }];

const toolFunctionMembers = [
  { label: "name", detail: "string" },
  { label: "description", detail: "string" },
  { label: "parameters", detail: "JSON Schema" },
  { label: "strict", detail: "boolean" },
];

const callMembers = [
  { label: "id", detail: "string" },
  { label: "name", detail: "string" },
  { label: "arguments", detail: "Record<string, unknown>" },
];

const keywords = [
  "export", "import", "from", "as", "default", "const", "let", "var", "function",
  "async", "await", "return", "if", "else", "for", "of", "in", "while", "try",
  "catch", "finally", "throw", "new", "typeof", "instanceof", "null", "undefined",
  "true", "false", "this", "class", "extends", "delete", "void", "yield",
];

const globals = [
  { label: "console", detail: "no output: use ctx.log" },
  { label: "JSON", detail: "JSON.parse / JSON.stringify" },
  { label: "encodeURIComponent", detail: "(value: string) => string" },
  { label: "decodeURIComponent", detail: "(value: string) => string" },
  { label: "Number", detail: "Number(value)" },
  { label: "String", detail: "String(value)" },
  { label: "Boolean", detail: "Boolean(value)" },
  { label: "Array", detail: "Array.isArray(value)" },
  { label: "Object", detail: "Object.keys / Object.entries" },
  { label: "Promise", detail: "Promise" },
  { label: "Map", detail: "Map" },
  { label: "Set", detail: "Set" },
];

/**
 * extensionCompletionSource returns the completion source for one editor. The
 * declared setting keys are passed in so `ctx.config.<key>` completes with the
 * names the script itself declared.
 */
export function extensionCompletionSource(settingKeys: string[] = []) {
  return (context: CompletionContext): CompletionResult | null => {
    const before = context.matchBefore(/[\w$][\w$.]*$/);
    if (!before && !context.explicit) return null;
    const expression = (before?.text ?? "").split(".");
    const isExplicitAtEnd = before == null && context.explicit;
    if (isExplicitAtEnd) {
      return {
        from: context.pos,
        options: topLevelOptions(settingKeys),
        validFor: /^[\w$.]*$/,
      };
    }
    const property = expression[expression.length - 1];
    const prefix = expression.slice(0, -1).join(".");
    const from = context.pos - property.length;
    if (!prefix) {
      if (/[.\[]$/.test(context.state.doc.lineAt(context.pos).text.slice(0, context.pos))) {
        return null;
      }
      return { from, options: topLevelOptions(settingKeys), validFor: /^[\w$]*$/ };
    }
    const options = memberOptions(prefix, settingKeys);
    if (!options) return null;
    return { from, options, validFor: /^[\w$]*$/ };
  };
}

function topLevelOptions(settingKeys: string[]): Completion[] {
  const options = [
    ...hookSnippets.map((snippet) => toCompletion(snippet, "function")),
    ...contextMembers.map((member) => toCompletion(member, "property")),
    ...globals.map((entry) => toCompletion(entry, "variable")),
  ];
  for (const keyword of keywords) {
    options.push({ label: keyword, detail: "keyword", type: "keyword", apply: undefined });
  }
  for (const key of settingKeys) {
    // Declared settings appear at the top level so the declaration itself
    // completes with the keys it already uses.
    options.push({ label: key, detail: "declared setting", type: "variable", apply: undefined });
  }
  return options;
}

function memberOptions(prefix: string, settingKeys: string[]) {
  const segments = prefix.split(".");
  const last = segments[segments.length - 1];
  switch (last) {
    case "config":
      if (segments.includes("ctx")) {
        return settingKeys.map((key) => ({ label: key, detail: "declared setting", type: "variable" }));
      }
      return null;
    case "log":
      return segments.includes("ctx") ? logMembers.map((member) => toCompletion(member, "function")) : null;
    case "http":
      return segments.includes("ctx") ? httpMembers.map((member) => toCompletion(member, "function")) : null;
    case "files":
      return segments.includes("ctx") ? filesMembers.map((member) => toCompletion(member, "function")) : null;
    case "kv":
      return segments.includes("ctx") ? kvMembers.map((member) => toCompletion(member, "function")) : null;
    case "abortSignal":
      return segments.includes("ctx") ? abortSignalMembers.map((member) => toCompletion(member, "property")) : null;
    case "function":
      return toolFunctionMembers.map((member) => toCompletion(member, "property"));
    case "parameters":
      return null;
    case "call":
      return callMembers.map((member) => toCompletion(member, "property"));
    default:
      return null;
  }
}

function toCompletion(entry: { label: string; detail: string; apply?: string }, type: string) {
  return { label: entry.label, detail: entry.detail, type, apply: entry.apply };
}

interface Completion {
  label: string;
  detail: string;
  type: string;
  apply: string | undefined;
}

/** packageJsonCompletions completes the keys of a package.json in the tree. */
export const packageJsonCompletions = [
  "name", "version", "description", "private", "type", "main", "module",
  "license", "author", "dependencies", "devDependencies", "optionalDependencies",
  "peerDependencies", "engines", "scripts",
].map((key) => ({ label: key, detail: "package.json", type: "property" }));
