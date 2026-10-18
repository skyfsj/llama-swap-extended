# Extensions

Extensions add request and response hooks and function tools to Chat Completions, Responses, and Anthropic Messages endpoints.

```yaml
extensions:
  enabled: true
  directory: /app/data/extensions
  maxToolRounds: 4
```

Create one directory per extension under `extensions.directory`. Each directory contains `manifest.yaml` and `index.js`; `package.json` and `package-lock.json` are optional together. Copy an example from [`examples`](examples) to get started. Changes made through the Extensions UI are validated before activation. File changes are detected automatically; an invalid new version leaves the last valid version active.

Configure an API key with the `config-admin` scope before using the Extensions management API or UI from a deployment that has keys. In a keyless deployment every management route is open, this one included, so the Extensions API follows the same posture as `/api/config` and model control rather than being stricter.

## Files

`index.js` is the entrypoint and the only file that is required. Add as many modules as the extension needs below it and import them by relative path:

```
extensions/web-search/
├── manifest.yaml
├── index.js
├── lib/wikipedia.js
├── package.json          (optional, requires package-lock.json)
└── package-lock.json
```

```js
import { search } from "./lib/wikipedia.js";
```

Relative imports work with and without npm dependencies. Managed extensions are `.js`, `.mjs`, `.cjs`, `.json`, `.md`, `.txt`, `.yaml` and `.yml`; anything else on disk is invisible to the editor and is removed by the next save. One extension may hold up to 200 files, 256 KiB per file and 4 MiB in total. Dotfiles and `node_modules` are never written by the API.

Saving an extension writes exactly the file list it was given, so a file that is no longer submitted is deleted. `manifest.yaml` is edited through the manifest panel, not as a file.

## Presets

The Extensions page has a **Presets** button that opens a store of packaged
extensions, grouped by category:

- **Web search** — Doubao, Bing, Google via SerpApi, GLM, Brave, Tavily, Exa,
  Parallel, DeepSeek, and OpenAI's built-in `web_search` tool.
- **Documents** — `web-fetch` (read a URL as clean Markdown), `web-render` (load
  a page in a real browser, for a client-rendered app where a plain fetch sees an
  empty shell), and PDF text extraction.
- **Knowledge and memory** — a RAG connector and an OpenViking memory manager.
- **Media** — `image-tools`: crop, resize, rotate and analyse an image. The codec
  runs in the daemon (see `ctx.images` below), so nothing is uploaded.
- **Utilities** — `math-tools` (`calculator`, which is exact for integers, and
  `symbolic_math`, which simplifies, expands, factors, differentiates, integrates
  and solves), `unit-convert`, and `datetime-tools` (`get_datetime`,
  `timezone_convert`).

The catalog is embedded in the binary, so it works with no network access, and
installing one writes a normal extension directory that is yours to edit or
delete — it is a starting point, not a dependency.

Each card lists the tools it installs, the network hosts it will contact, and
the settings it declares. The extension ID box under a card is the name the
extension is installed under, so two search backends can coexist as
`web-search-bing` and `web-search-brave`. Installing a preset that declares a
required credential seeds it empty: fill it in under **Settings** before calling
the tool, which reports a missing credential as an error rather than sending a
request that can only fail.

### Search and fetch backends

Each search provider installs a tool named after itself, so several can be
enabled at once. The model sees which engine answered, and one failing provider
does not take the others down with it.

| Preset | Tool | What it does |
| --- | --- | --- |
| `web-search-bing` | `web_search_bing` | Microsoft Bing Web Search v7 |
| `web-search-brave` | `web_search_brave` | Brave Search |
| `web-search-doubao` | `web_search_doubao` | Volcengine (Doubao) networked search |
| `web-search-exa` | `web_search_exa` | Exa neural search; highlights or page text |
| `web-search-glm` | `web_search_glm` | Zhipu GLM web search |
| `web-search-google` | `web_search_google` | Google through SerpApi |
| `web-search-parallel` | `web_search_parallel` | Parallel Search; accepts an objective |
| `web-search-tavily` | `web_search_tavily` | Tavily; optional answer plus results |
| `deepseek-tools` | `deepseek_search` | DeepSeek search (`site:`, `lang:`, `after:` …) |
| `deepseek-tools` | `deepseek_fetch` | DeepSeek fetch: a URL becomes title + Markdown |
| `deepseek-tools` | `deepseek_balance` | DeepSeek account balance (free to call) |
| `web-fetch` | `web_fetch` | In-extension fetch: downloads and parses HTML itself |

`web-fetch` needs no key and works offline, but only reads static pages. The
provider fetchers cost per successful request and return server-rendered
Markdown. Every route to a page is its own tool, so a prompt can try one and
fall back to another.

Presets that reach the network name their host in the manifest. The runtime
permits only those hosts and refuses loopback, private and reserved addresses, so
a self-hosted target has to be reachable on a public host and has to be added
under **Permissions → Allowed network hosts** before it can be called.

## Settings

A script declares the settings a user should be able to change, and the Extensions UI renders a form for them. Values are stored in `manifest.config` and delivered to the script as `ctx.config`:

```js
export const settings = {
  apiKey: { type: "string", label: "API Key", secret: true, required: true },
  locale: { type: "select", label: "Language", options: ["zh", "en"], default: "zh" },
  maxResults: { type: "number", label: "Results", default: 5, min: 1, max: 20 },
  verbose: { type: "boolean", label: "Verbose", default: false },
  notes: { type: "textarea", label: "Notes" },
  headers: { type: "map", label: "Extra headers" },
  channels: { type: "list", label: "Channels" },
  tuning: { type: "json", label: "Tuning" }
};

export default {
  async onRequest(ctx, request) {
    const key = ctx.config.apiKey; // "k" once set, otherwise missing
  }
};
```

Each field takes `type`, `label`, and optionally `hint`, `default`, `options` (select), `required`, `secret`, `min`, `max` and `section`. A typo in a property name fails the build with the offending key named, so a malformed declaration never reaches a running extension. `secret: true` renders a masked input; the value itself is stored in `manifest.yaml` like the rest of the configuration.

Keys the user never touched fall back to the declared `default` at runtime, so `manifest.config` only records explicit choices and the script always sees a usable value. The form appears after the extension compiles, which is why a brand new draft shows the raw `manifest.config` JSON until it is saved once. Keys the script does not declare are passed through untouched. `required` means the key has to be present in the configuration; an empty value is caught by the script itself when it runs.

`ctx.config` is always an object, so reading a key on an extension that declares
no settings returns `undefined` rather than throwing.

## Manifest

```yaml
id: web-search
name: Web Search
enabled: true
priority: 100
match:
  models: ["qwen*"]
  excludeModels: ["*guard*"]
  profiles: [coding]
  providers: [vllm]
  endpoints: [chat.completions, responses, anthropic.messages]
permissions:
  networkHosts: [en.wikipedia.org]
  readRoots: [/app/data/documents]
  writeRoots: [/app/data/output]
timeout: 10s
maxCpuMillis: 2000
maxMemoryMiB: 256
continueOnError: false
toolConflict: skip
interceptClientTools: false
```

An empty match list matches every value. Model patterns apply to the resolved model ID after Profile and Selector routing. Glob patterns use `*` and `?`; exclusions win. Request hooks run by ascending priority and ID. Response hooks run in reverse order. `toolConflict` accepts `skip`, `override`, or `error`; `skip` preserves client tools.

## JavaScript API

`index.js` exports an object with optional `tools`, `onRequest`, `onBeforeForward`, `onToolCall`, `onToolResult`, `onResponse`, `onStreamEvent`, and `onError`. Hooks receive a context and the current request, response, tool call, result, or event. Returning `null` or `undefined` leaves the input unchanged. See [`extension.d.ts`](extension.d.ts) for the complete API.
Tools use a provider-independent function schema and declare `execution: "client"` or `execution: "server"`. Client tools are returned to the API caller. Server tools call `onToolCall`, insert the result into the conversation, and continue generation, up to `maxToolRounds`. Tool definitions and results are rendered for each endpoint; Anthropic Messages uses `input_schema`, `tool_use`, and `tool_result`.

Set `interceptClientTools: true` to inspect client-owned calls in `onToolCall`. Return `{ handled: true, content: ... }` to execute a call on the server; return `undefined` to leave it with the client. A name conflict skipped by `toolConflict: skip` remains client-owned.

A round that returns both a server call and a caller-owned call cannot be handed over as-is: the caller would answer a call whose server siblings never ran. Each surface solves that in the one way its callers can accept.

Anthropic Messages and Responses report the work. On Messages the caller receives each server call as a `server_tool_use` block immediately followed by its `mcp_tool_result`, next to its own `tool_use` calls; on Responses each server call becomes an `mcp_call` item carrying its output. The caller replays the whole turn on the next request, so the daemon keeps no record of the round at all - this is Anthropic's own contract, verified against Claude Code 2.1.278: it accepts, echoes and does not answer that pair (`mcp_tool_use` fails its stream accumulator, which takes input deltas only for `tool_use`/`server_tool_use`), and a caller that strips unknown blocks merely loses the server work from its transcript without breaking the conversation. The consequence is the vendor one: those results stay in the caller's context and are re-sent every turn.

Chat Completions and Gemini have no part for "the server ran this, do not", so there the round stays hidden and is completed from a record instead: the caller gets only its own calls, the extension's own calls are written down unexecuted and run when the caller comes back with the conversation (`extensions.maxContinuationMiB`, default 64 MiB, bounds that one-round record, so it does not grow with the session). Those surfaces therefore require the caller to replay the assistant turn: a request carrying only the tool results is answered with a 400 naming that requirement (the status Anthropic's own Messages API uses for a continuation whose history is incomplete). A well-formed request that conflicts with state the daemon already holds - the record is in use by another in-flight request, or the model/profile/endpoint changed under it - stays a 409. Past the record limit the caller's calls of the round are reported to the model as failed tool results rather than failing the turn. Gemini's function parts carry no call ids, so its record is keyed by tool name - scoped to the caller's identity, and checked against the arguments of the caller's own calls and against the text of the conversation that led to the round, so two conversations that happen to call the same tool cannot be folded into each other. (A replay that carries no earlier text at all - the model turn and the results only - leaves nothing to compare, so those two checks are all that stand between it and a same-named round of another conversation.)

`ctx.http.fetch()` reaches any public host unless `networkHosts` narrows it, and `ctx.files.read()` and `ctx.files.write()` are limited to their configured directories; file access is denied when no matching permission is configured. Script code has no Node `fs`, `process`, shell, or environment access. Pure JavaScript npm packages with a lock file can be installed during extension validation; installation scripts and native modules are unsupported. An npm executable is required to save an extension that declares dependencies.

`ctx.http.fetch(url, options)` resolves to `{status, ok, url, headers, text(), json()}`, where `url` is the address after redirects and `headers` carries lowercased response header names. `options.timeoutMs` (1000-60000, default 8000) overrides the client timeout for slower targets; the hook's own budget in the manifest still caps the whole call, so raise `timeout` alongside it. Redirects are followed up to five times, every hop is re-checked against the host list and the public-address rule, and `Authorization` and `Cookie` are dropped when a redirect leaves the original host. The fetch follows the daemon's `network.proxy` policy: a configured proxy, `direct`, and `noProxy` all apply to extension requests without the script doing anything.

`ctx.kv` stores JSON values per extension. `permissions.storage` is `ephemeral` by default (in-memory, sliding TTL), `persistent` for a durable store that survives restarts, and `none` to disable storage. Keys are limited to 128 characters, values to `extensions.storageMaxValueMiB` per entry (default 256 MiB), and the store to `extensions.storageMaxKeys` per extension (default 2000). The `scope` option is `global` (default), `session`, or `key`; session and key scopes isolate callers from each other. `ctx.usage()` reports the caller's 24h aggregate requests and tokens so a script can enforce its own quotas.

`ctx.images.apply(request)` runs one image operation in the daemon and is how the `image-tools` preset is built. The pixel work lives on the host because a byte loop in the sandbox VM manages about six million array element touches a second, while a decode plus resample is dozens of passes over the pixels: through this call a 1.4 MP image is analysed in about 115 ms and cropped in about 166 ms. The standard library also reads what a hand-written decoder should not attempt (progressive and CMYK JPEG, interlaced PNG, every bit depth). The request is `{operation: "info" | "analyze" | "crop" | "resize", image, ...}` where `image` is a bare or data-URL base64 payload, and the operation's own fields are optional: `crop: {x, y, width, height}`, `resize: {width, height, filter}`, `rotate`, `flip`, `autoOrient`, `colors`, `samples`, `format`, `quality`. It resolves to the image's metadata, the encoded result (`image` as base64, `mediaType`, `bytes`) for the operations that produce one, and an `analysis` object for `analyze`. The ceilings are enforced by the daemon, not taken from the request: 32 MiB of decoded input, 25 million pixels in and out, and 5 MiB of encoded result. Those are the daemon's own limits, and a tool call cannot reach the first one: the whole call is one 8 MiB protocol line shared with the extension's bundle, so an image argument tops out near 5.9 MiB of decoded data. A call past the line is answered as a tool result naming the limit and the way out, not by killing the worker.

An image is base64 text rather than bytes because the worker protocol is JSON lines: `ctx.http.fetch` and `ctx.files.read` both hand back a UTF-8 string, and bytes above 0x7F do not survive that, which is also why the tools refuse a URL or a path with an explanation instead of fetching one. The protocol line that carries a hook is 8 MiB, and it carries the extension's own bundle too, so the room a payload has depends on how large the installed extension is (roughly 8 MiB for a small one, about 7.8 MiB for `math-tools`). The limit applies to **one hook call's input**, not to the conversation: `onToolCall` receives just the tool name and its arguments — the message history stays in the daemon — so a long conversation is never the reason a tool call fails. The hooks that do see the whole request body are the request-shaped ones (`onRequest`, `onBeforeForward`); when one of those cannot fit, the request **continues without that hook** and the reason is logged, rather than failing. A tool call whose arguments do not fit is answered as a tool result naming the limit and the way out, so the model can shrink the payload instead of losing the turn.

`ctx.forward(model, request)` runs one non-streaming model call as the caller: the caller's key allowlist applies, and the extension does not run inside the forwarded request, so a hook cannot recurse. Nested forwards are bounded by `extensions.maxForwardDepth` (default 2). `ctx.models` lists the models the caller may reach. These host capabilities, like the rest of the host-call channel (kv, usage), are disabled in the single-request test mode.

The UI's **Test extension** action runs hooks on a simulated request without starting a model. Network and file calls are disabled in this test mode. The active request path uses a separate worker process for each hook and enforces the configured wall-clock, CPU-time, and memory limits. CPU and RSS are sampled across platforms; the sampling interval permits a brief overshoot. Extensions should be administered as trusted code.

Setting `label` and `hint` accept a plain string or a per-locale object keyed by the console languages `en`, `zh-CN`, and `zh-TW`, e.g. `label: { en: "Log level", "zh-CN": "日志级别" }`. The settings center renders the console's language; the canonical value prefers `en`.

## Editor

The code editor completes the extension API (`ctx`, the hook names, `tools`, `http`, `files`, `log`, and the setting keys the script itself declares) and checks every file in the tree as it is typed. The check is syntax and import resolution only — it reports the file and position of a syntax error and of a relative import that points at nothing, but it does not type-check, so it will not catch a misspelled property. npm dependencies are resolved when the extension is saved, not during a check.

## Management API

`GET`/`POST /api/extensions` and `GET`/`PUT`/`DELETE /api/extensions/{id}` exchange a definition whose `files` map carries the whole source tree; `PUT` and `DELETE` require `If-Match` with the returned `ETag`. `POST /api/extensions/{id}/test` runs the hooks on a simulated request, `POST /api/extensions/{id}/duplicate` copies one, and `POST /api/extensions/reload` rescans the directory. `POST /api/extensions/check` accepts `{files}` or `{path, source}` and returns `{diagnostics: [{path, line, column, length, severity, message}]}` without saving anything. A definition that violates its declared settings answers `422` with a `diagnostics` array whose `path` names the rejected setting.

`GET /api/extensions/presets` lists the store, `GET /api/extensions/presets/{id}` returns one with its files, and `POST /api/extensions/presets/{id}/install` installs it under the given id (its own when the body omits one) and answers with the saved definition.

`networkHosts` may be left empty, which is the default and grants every **public**
host: an extension whose targets are chosen at run time (a web-fetch tool is
handed its URL by the model) cannot name its hosts in advance. Entries narrow
the grant to a hostname or a leading subdomain wildcard such as `*.pinecone.io`,
and `*` spells the unrestricted default out. Whatever the list says, every
request is refused when it targets the daemon's own host, a loopback address, or
a private or reserved network, and the same check runs on each redirect hop.
File permissions keep the opposite default: no `readRoots` or `writeRoots`
means no file access at all.

