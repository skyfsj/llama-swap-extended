/**
 * Ambient typings for the `?worker` imports used by
 * components/extensions/ExtensionMonacoEditor.svelte.
 *
 * Vite's own `vite/client` types cover this, but they are not part of this
 * project's tsconfig, and `declare module` blocks in a regular (module) source
 * file are treated as augmentations that must resolve to an existing module.
 * Ambient module declarations therefore live here, in a file without any
 * import or export of its own. Keep the specifiers in sync with the worker
 * imports in that component.
 */

declare module "monaco-editor/editor/editor.worker.js?worker" {
  const WorkerFactory: new () => Worker;
  export default WorkerFactory;
}

declare module "monaco-editor/languages/features/json/json.worker.js?worker" {
  const WorkerFactory: new () => Worker;
  export default WorkerFactory;
}

declare module "monaco-editor/languages/features/typescript/ts.worker.js?worker" {
  const WorkerFactory: new () => Worker;
  export default WorkerFactory;
}
