/**
 * @llama-swap/extension — the extension SDK.
 *
 * Import this from an extension entry file:
 *
 * ```js
 * import { defineExtension, defineTool } from "@llama-swap/extension";
 *
 * export default defineExtension({
 *   tools: [
 *     defineTool({
 *       name: "echo",
 *       description: "Return the input text",
 *       parameters: { type: "object", properties: { text: { type: "string" } }, required: ["text"] },
 *       async handler(args, ctx) {
 *         return { content: String(args.text ?? "") };
 *       },
 *     }),
 *   ],
 *   async onRequest(ctx, request) { return request; },
 * });
 * ```
 *
 * `defineTool` binds a handler by name: when the model calls `echo`, the
 * runtime executes the bound handler directly — no manual `onToolCall`
 * dispatch. The legacy `tools` array and `onToolCall` hook keep working, and
 * an explicit `onToolCall` takes precedence over a bound handler.
 */

/** Context passed to tool handlers, identical to the hook context. */
export type ExtensionContext = Record<string, any>;

export interface ToolSpec {
  /** Tool name the model calls; must be unique in the extension. */
  name: string;
  /** Description shown to the model. */
  description?: string;
  /** JSON Schema of the tool arguments. */
  parameters?: Record<string, unknown>;
  strict?: boolean;
  /** "server" (default for bound handlers) or "client". */
  execution?: "client" | "server";
  /**
   * The bound handler. Called with the parsed tool arguments and the request
   * context; returns `{ content }` (the tool result) or a plain value, which
   * is wrapped as `{ content: value }`.
   */
  handler(args: Record<string, unknown>, ctx: ExtensionContext): unknown;
}

/** A declared tool, tag-marked so the runtime recognizes SDK definitions. */
export interface DefinedTool extends ToolSpec {
  readonly __llamaSwapTool: true;
}

export interface ExtensionDefinition {
  tools?: (DefinedTool | Record<string, any>)[];
  settings?: Record<string, any>;
  [hook: string]: any;
}

const TOOL_MARKER = "__llamaSwapTool";

/**
 * defineTool declares a tool with a bound handler. The handler is stripped
 * from the definition the model sees and executed by the runtime when the
 * model calls the tool.
 */
export function defineTool(spec: ToolSpec): DefinedTool {
  if (!spec || typeof spec !== "object") {
    throw new Error("defineTool requires an object");
  }
  if (typeof spec.name !== "string" || spec.name.trim() === "") {
    throw new Error("defineTool requires a name");
  }
  if (typeof spec.handler !== "function") {
    throw new Error(`defineTool(${spec.name}) requires a handler function`);
  }
  const tool: DefinedTool = {
    ...spec,
    execution: spec.execution ?? "server",
    [TOOL_MARKER]: true,
  } as DefinedTool;
  return Object.freeze(tool);
}

/**
 * defineExtension marks the default export so the runtime can apply
 * SDK-specific normalization (tool handler stripping, map-form tools).
 */
export function defineExtension<T extends ExtensionDefinition>(definition: T): T {
  if (!definition || typeof definition !== "object") {
    throw new Error("defineExtension requires an object");
  }
  return definition;
}
