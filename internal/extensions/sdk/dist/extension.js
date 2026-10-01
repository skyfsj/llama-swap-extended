var __defProp = Object.defineProperty;
var __getOwnPropDesc = Object.getOwnPropertyDescriptor;
var __getOwnPropNames = Object.getOwnPropertyNames;
var __hasOwnProp = Object.prototype.hasOwnProperty;
var __export = (target, all) => {
  for (var name in all)
    __defProp(target, name, { get: all[name], enumerable: true });
};
var __copyProps = (to, from, except, desc) => {
  if (from && typeof from === "object" || typeof from === "function") {
    for (let key of __getOwnPropNames(from))
      if (!__hasOwnProp.call(to, key) && key !== except)
        __defProp(to, key, { get: () => from[key], enumerable: !(desc = __getOwnPropDesc(from, key)) || desc.enumerable });
  }
  return to;
};
var __toCommonJS = (mod) => __copyProps(__defProp({}, "__esModule", { value: true }), mod);

// internal/extensions/sdk/extension.ts
var extension_exports = {};
__export(extension_exports, {
  defineExtension: () => defineExtension,
  defineTool: () => defineTool
});
module.exports = __toCommonJS(extension_exports);
var TOOL_MARKER = "__llamaSwapTool";
function defineTool(spec) {
  var _a;
  if (!spec || typeof spec !== "object") {
    throw new Error("defineTool requires an object");
  }
  if (typeof spec.name !== "string" || spec.name.trim() === "") {
    throw new Error("defineTool requires a name");
  }
  if (typeof spec.handler !== "function") {
    throw new Error(`defineTool(${spec.name}) requires a handler function`);
  }
  const tool = {
    ...spec,
    execution: (_a = spec.execution) != null ? _a : "server",
    [TOOL_MARKER]: true
  };
  return Object.freeze(tool);
}
function defineExtension(definition) {
  if (!definition || typeof definition !== "object") {
    throw new Error("defineExtension requires an object");
  }
  return definition;
}
// Annotate the CommonJS export names for ESM import in node:
0 && (module.exports = {
  defineExtension,
  defineTool
});
