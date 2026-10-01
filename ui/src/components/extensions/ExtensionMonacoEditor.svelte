<script module lang="ts">
  // Monaco's imports and worker wiring live at module scope so they run once
  // per app. The Extensions page re-keys this component whenever the open file
  // changes, and re-running the setup per mount would rebuild the worker
  // environment and re-register global state for every tab.
  // The editor API namespace on its own does not include the editor
  // contributions (find widget, suggest widget, hover, folding, format, ...).
  // Monaco's export map resolves deep paths relative to esm/vs/, i.e. without
  // the leading "esm/vs/" segment in the specifier.
  import "monaco-editor/features/register.all.js";
  // Tokenizers for the languages an extension file can be. The language
  // services further down only register intelligence, not highlighting.
  import "monaco-editor/languages/definitions/javascript/register.js";
  // Real language intelligence: completions, hover and syntax diagnostics for
  // JS/TS, plus schema-aware diagnostics and key completion for JSON.
  import "monaco-editor/languages/features/typescript/register.js";
  import "monaco-editor/languages/features/json/register.js";

  // Vite bundles each ?worker import as its own worker chunk. Monaco asks for a
  // worker at runtime by language label, so this factory decides which chunk
  // to instantiate.
  import editorWorker from "monaco-editor/editor/editor.worker.js?worker";
  import jsonWorker from "monaco-editor/languages/features/json/json.worker.js?worker";
  import tsWorker from "monaco-editor/languages/features/typescript/ts.worker.js?worker";

  if (typeof self !== "undefined") {
    (self as { MonacoEnvironment?: { getWorker: (workerId: string, label: string) => Worker } }).MonacoEnvironment = {
      getWorker(_workerId: string, label: string): Worker {
        if (label === "json") return new jsonWorker();
        if (label === "javascript" || label === "typescript") return new tsWorker();
        return new editorWorker();
      },
    };
  }
</script>

<script lang="ts">
  import { onMount } from "svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "../../lib/i18n";
  import { fileKind } from "../../lib/extensionFiles";
  // Imported again here (the module script above cannot see instance scope):
  // ES modules are evaluated once, so this is the same shared Monaco runtime.
  // Monaco's export map resolves deep paths relative to esm/vs/, i.e. without
  // the leading "esm/vs/" segment in the specifier.
  import * as monaco from "monaco-editor/editor/editor.api.js";
  import type { Completion, CompletionContext, CompletionResult } from "@codemirror/autocomplete";
  import { extensionCompletionSource, packageJsonCompletions } from "../../lib/extensionCompletions";
  import type { ExtensionDiagnostic } from "../../lib/extensionsApi";

  interface Props {
    value: string;
    ariaLabel: string;
    path: string;
    settingKeys?: string[];
    diagnostics?: ExtensionDiagnostic[];
    revealLocation?: { line: number; column: number };
    onChange: (value: string) => void;
  }

  let { value, ariaLabel, path, settingKeys = [], diagnostics = [], revealLocation, onChange }: Props = $props();

  let host: HTMLDivElement;
  // Monaco instances live in plain lets, not $state: they are class instances
  // (which Svelte does not proxy) created once in onMount and read back from
  // the effects below, which re-run off `editorReady` instead.
  let editor: monaco.editor.IStandaloneCodeEditor | null = null;
  let model: monaco.editor.ITextModel | null = null;
  let createdModel = false;
  let syncing = false;
  let editorReady = $state(false);
  let cursorLine = $state(1);
  let cursorColumn = $state(1);

  let wrap = $state(false);
  const language = $derived(fileKind(path) === "text" ? "plaintext" : fileKind(path));
  function command(id: string) { editor?.focus(); editor?.trigger("toolbar", id, null); }
  $effect(() => {
    if (!editorReady || !editor || !revealLocation) return;
    editor.setPosition({ lineNumber: Math.max(1, revealLocation.line), column: Math.max(1, revealLocation.column) });
    editor.revealLineInCenter(revealLocation.line); editor.focus();
  });

  /** Theme name; must match Monaco's /^[a-z0-9-]+$/i theme-name check. */
  const monacoTheme = "llama-swap-editor";
  /** Marker owner for server-side lint results, kept distinct from the TS service's. */
  const lintMarkerOwner = "extension-server-lint";
  const monoFontStack = "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace";

  onMount(() => {
    applyTheme();
    // A model is global to the Monaco instance and keyed by URI. Layout changes
    // can mount a second editor for the same file before the first one has torn
    // its model down, so an existing model is adopted rather than recreated:
    // createModel throws on a duplicate URI and that error takes the whole page
    // down with it.
    const uri = modelUri(path);
    const existing = monaco.editor.getModel(uri);
    model = existing ?? monaco.editor.createModel(value, language, uri);
    // Only the mount that created the model may dispose it, or the second
    // editor looking at the same file tears the model out from under the first.
    createdModel = !existing;
    if (model.getValue() !== value) model.setValue(value);
    editor = monaco.editor.create(host, {
      model,
      theme: monacoTheme,
      ariaLabel,
      automaticLayout: true,
      minimap: { enabled: false },
      fontSize: 13,
      fontFamily: monoFontStack,
      scrollBeyondLastLine: false,
      renderLineHighlight: "all",
      tabSize: 2,
      wordWrap: "off",
      // The curated completion vocabulary below is only useful if it appears
      // while the author types. Monaco's built-in default degrades to "off"
      // when inline completions are active, so pin it on here.
      quickSuggestions: { other: "on", comments: "off", strings: "off" },
    });

    const cursor = editor.onDidChangeCursorPosition((event) => {
      cursorLine = event.position.lineNumber;
      cursorColumn = event.position.column;
    });
    const content = editor.onDidChangeModelContent(() => {
      if (syncing || !editor) return;
      onChange(editor.getValue());
    });
    const jsCompletions = monaco.languages.registerCompletionItemProvider("javascript", {
      triggerCharacters: ["."],
      provideCompletionItems: (completionModel, position, context) => ({
        suggestions: javascriptCompletions(completionModel, position, context.triggerKind === monaco.languages.CompletionTriggerKind.Invoke),
      }),
    });
    // package.json is the only JSON file in an extension tree, and it is the
    // one place the old editor offered key completions.
    const jsonProvider = language === "json"
      ? monaco.languages.registerCompletionItemProvider("json", {
        provideCompletionItems: (completionModel, position) => ({ suggestions: jsonCompletions(completionModel, position) }),
      })
      : null;

    // The app repaints by toggling class/data-theme on <html>; Monaco themes
    // are static definitions, so they are re-derived whenever that happens.
    const themeObserver = new MutationObserver(() => applyTheme());
    themeObserver.observe(document.documentElement, { attributes: true, attributeFilter: ["class", "data-theme"] });

    editorReady = true;

    return () => {
      themeObserver.disconnect();
      jsCompletions.dispose();
      jsonProvider?.dispose();
      cursor.dispose();
      content.dispose();
      // Disposing the editor does not dispose its model, and models are keyed
      // by URI. The creator disposes; an adopting editor leaves it alone.
      if (createdModel) model?.dispose();
      model = null;
      editor?.dispose();
      editor = null;
      editorReady = false;
    };
  });

  // The document is owned by Monaco. The page only pushes a value in when
  // something outside the editor replaced the text, and `syncing` keeps the
  // change event that push produces from echoing back out.
  $effect(() => {
    if (!editorReady || !editor) return;
    if (editor.getValue() === value) return;
    syncing = true;
    editor.setValue(value);
    syncing = false;
  });

  // Server lint results arrive out of band, one set for the whole tree; only
  // the entries for this file are projected onto this model.
  $effect(() => {
    if (!editorReady || !model) return;
    // Held in a const: `model` is assigned by onMount/cleanup, which resets
    // TypeScript's narrowing inside nested callbacks.
    const current = model;
    const markers = (diagnostics ?? [])
      .filter((result) => (result.path || path) === path)
      .map((result) => toMarkerData(current, result));
    monaco.editor.setModelMarkers(current, lintMarkerOwner, markers);
  });

  function modelUri(filePath: string): monaco.Uri {
    // One model per file, identified by a stable file URI.
    return monaco.Uri.parse(`file:///extensions/${filePath.split("/").map(encodeURIComponent).join("/")}`);
  }

  function toMarkerData(current: monaco.editor.ITextModel, result: ExtensionDiagnostic): monaco.editor.IMarkerData {
    const lineNumber = Math.min(Math.max(1, result.line || 1), current.getLineCount());
    const lastColumn = current.getLineMaxColumn(lineNumber);
    const startColumn = Math.min(Math.max(1, result.column || 1), lastColumn);
    const endColumn = Math.min(Math.max(startColumn, startColumn + Math.max(1, result.length || 1)), lastColumn);
    const range = new monaco.Range(lineNumber, startColumn, lineNumber, endColumn);
    return {
      ...range,
      severity: result.severity === "warning" ? monaco.MarkerSeverity.Warning : monaco.MarkerSeverity.Error,
      message: result.message,
    };
  }

  /**
   * javascriptCompletions maps the shared extension vocabulary onto Monaco
   * suggestion items. The vocabulary itself lives in lib/extensionCompletions.ts
   * behind a CodeMirror-shaped completion source, so it is driven here with a
   * synthetic context that exposes the same line text and cursor: the module
   * never looks at anything else.
   */
  function javascriptCompletions(model: monaco.editor.ITextModel, position: monaco.Position, explicit: boolean): monaco.languages.CompletionItem[] {
    const before = model.getLineContent(position.lineNumber).slice(0, position.column - 1);
    const source = extensionCompletionSource(settingKeys);

    let result: CompletionResult | null = sharedSource(source, before, explicit);
    if (!result && /(^|[^\w$])ctx\.\w*$/.test(before)) {
      // `ctx.` is the one member group the shared source cannot address: its
      // memberOptions() has no bare-ctx case, while the top-level list is the
      // context members tagged as "property" (hooks are "function", globals
      // and declared settings are "variable", keywords are "keyword").
      result = sharedSource(source, "", true);
      if (result) result = { ...result, options: result.options.filter((option) => option.type === "property") };
    }
    if (!result) return [];

    const range = replaceRange(before, position);
    return result.options.map((option, index) => toCompletionItem(option, range, index));
  }

  function jsonCompletions(model: monaco.editor.ITextModel, position: monaco.Position): monaco.languages.CompletionItem[] {
    const word = model.getWordUntilPosition(position);
    const range = new monaco.Range(position.lineNumber, word.startColumn, position.lineNumber, position.column);
    return packageJsonCompletions.map((option, index) => ({
      label: option.label,
      detail: option.detail,
      insertText: option.label,
      kind: monaco.languages.CompletionItemKind.Property,
      range,
      sortText: sortTextFor(index),
    }));
  }

  /**
   * sharedSource drives the CodeMirror completion source with a synthetic
   * document consisting of the line prefix ending at the cursor. A trailing
   * dot is probed with a placeholder word (`ctx.log.` becomes `ctx.log.a`),
   * which is what makes the shared source resolve prefix members before the
   * author has typed anything.
   */
  function sharedSource(source: (context: CompletionContext) => CompletionResult | null, before: string, explicit: boolean): CompletionResult | null {
    const line = before.endsWith(".") ? `${before}a` : before;
    const context = {
      pos: line.length,
      explicit,
      matchBefore(expr: RegExp) {
        const anchored = new RegExp(`${expr.source.replace(/\$$/, "")}$`);
        const match = anchored.exec(line);
        if (!match) return null;
        return { from: line.length - match[0].length, to: line.length, text: match[0] };
      },
      state: { doc: { lineAt: () => ({ text: line }) } },
    } as unknown as CompletionContext;
    return source(context);
  }

  /**
   * replaceRange spans the text a suggestion replaces. It starts at the last
   * dot-separated segment, not at the whole expression: Monaco scores every
   * candidate against the text the range covers, so a range over `ctx.config`
   * would make the filter word "ctx.config" and hide every candidate.
   */
  function replaceRange(before: string, position: monaco.Position): monaco.Range {
    const word = /[\w$][\w$.]*$/.exec(before);
    if (!word) return new monaco.Range(position.lineNumber, position.column, position.lineNumber, position.column);
    const lastDot = word[0].lastIndexOf(".");
    const segment = lastDot === -1 ? word[0] : word[0].slice(lastDot + 1);
    return new monaco.Range(position.lineNumber, position.column - segment.length, position.lineNumber, position.column);
  }

  function toCompletionItem(option: Completion, range: monaco.Range, index: number): monaco.languages.CompletionItem {
    // CodeMirror lets `apply` be either a string or a function; only the
    // string form carries the snippet text, and its `${}` field markers are
    // Monaco's `$0`.
    const apply = typeof option.apply === "string" ? option.apply : option.label;
    const isSnippet = typeof option.apply === "string" && apply.includes("${");
    return {
      label: option.label,
      detail: option.detail,
      kind: completionKindFor(option.type),
      range,
      sortText: sortTextFor(index),
      ...(isSnippet
        ? { insertText: apply.replace(/\$\{\}/g, "$0"), insertTextRules: monaco.languages.CompletionItemInsertTextRule.InsertAsSnippet }
        : { insertText: apply }),
    };
  }

  function completionKindFor(type: string | undefined): monaco.languages.CompletionItemKind {
    switch (type) {
      case "function":
        return monaco.languages.CompletionItemKind.Function;
      case "keyword":
        return monaco.languages.CompletionItemKind.Keyword;
      case "variable":
        return monaco.languages.CompletionItemKind.Variable;
      default:
        return monaco.languages.CompletionItemKind.Property;
    }
  }

  function sortTextFor(index: number): string {
    // Keep the curated vocabulary above the language service's own globals,
    // which are also offered for JavaScript.
    return String(index).padStart(3, "0");
  }

  /**
   * applyTheme derives a Monaco theme from the app's CSS variables. Monaco
   * parses theme colors with Color.fromHex (#RRGGBB/#RRGGBBAA only) while the
   * palette is authored in oklch(), and getComputedStyle hands custom
   * properties back verbatim, so the browser does the conversion: the value is
   * painted into a 1x1 canvas and read back as pixels.
   */
  function applyTheme(): void {
    const styles = getComputedStyle(document.documentElement);
    const background = cssColorToHex(styles.getPropertyValue("--background"));
    const foreground = cssColorToHex(styles.getPropertyValue("--foreground"));
    const muted = cssColorToHex(styles.getPropertyValue("--muted-foreground"));
    const border = cssColorToHex(styles.getPropertyValue("--border"));
    const accent = cssColorToHex(styles.getPropertyValue("--accent"));
    const base = document.documentElement.classList.contains("dark") ? "vs-dark" : "vs";
    if (!background || !foreground || !muted || !border || !accent) {
      // Half of the palette is unreadable (unsupported color syntax or no
      // DOM): fall back to the stock theme rather than feeding Monaco a
      // partial definition.
      monaco.editor.setTheme(base);
      return;
    }
    monaco.editor.defineTheme(monacoTheme, {
      base,
      inherit: true,
      rules: [],
      colors: {
        "editor.background": background,
        "editor.foreground": foreground,
        "editorLineNumber.foreground": muted,
        "editorLineNumber.activeForeground": foreground,
        "editorCursor.foreground": foreground,
        "editor.selectionBackground": withAlpha(accent, 0.55),
        "editor.inactiveSelectionBackground": withAlpha(accent, 0.3),
        "editor.lineHighlightBackground": withAlpha(accent, 0.3),
        "editor.lineHighlightBorder": withAlpha(border, 0.6),
        "editorWidget.background": background,
        "editorWidget.foreground": foreground,
        "editorWidget.border": border,
        "editorSuggestWidget.background": background,
        "editorSuggestWidget.foreground": foreground,
        "editorSuggestWidget.selectedBackground": withAlpha(accent, 0.55),
        "editorHoverWidget.background": background,
        "editorHoverWidget.foreground": foreground,
        "editorHoverWidget.border": border,
        "scrollbarSlider.background": withAlpha(muted, 0.4),
        "scrollbarSlider.hoverBackground": withAlpha(muted, 0.7),
      },
    });
    monaco.editor.setTheme(monacoTheme);
  }

  let colorProbe: CanvasRenderingContext2D | null = null;

  function cssColorToHex(css: string): string | null {
    const value = css.trim();
    if (!value) return null;
    if (!colorProbe) {
      const canvas = document.createElement("canvas");
      canvas.width = 1;
      canvas.height = 1;
      colorProbe = canvas.getContext("2d");
    }
    if (!colorProbe) return null;
    // An unsupported color syntax leaves fillStyle unchanged, which is what
    // distinguishes "canvas cannot convert this" from "converted to black".
    colorProbe.fillStyle = "#000000";
    const rejected = colorProbe.fillStyle;
    colorProbe.fillStyle = value;
    if (colorProbe.fillStyle === rejected) return null;
    colorProbe.clearRect(0, 0, 1, 1);
    colorProbe.fillRect(0, 0, 1, 1);
    const [red, green, blue, alpha] = colorProbe.getImageData(0, 0, 1, 1).data;
    const byte = (channel: number) => channel.toString(16).padStart(2, "0");
    return `#${byte(red)}${byte(green)}${byte(blue)}${byte(alpha)}`;
  }

  function withAlpha(hex: string, alpha: number): string {
    return `${hex.slice(0, 7)}${Math.round(Math.max(0, Math.min(1, alpha)) * 255).toString(16).padStart(2, "0")}`;
  }
</script>

<div class="flex h-full min-h-0 flex-col overflow-hidden bg-background">
  <div class="flex shrink-0 flex-wrap items-center gap-1 border-b border-border px-2 py-1">
    {#each [["find", "actions.find"], ["replace", "editor.action.startFindReplaceAction"], ["format", "editor.action.formatDocument"], ["undo", "undo"], ["redo", "redo"]] as [label, id]}<Button variant="ghost" size="xs" disabled={!editorReady} onclick={() => command(id)}>{$translate(`extensions.dev.${label}`)}</Button>{/each}
    <Button variant="ghost" size="xs" aria-pressed={wrap} onclick={() => { wrap = !wrap; editor?.updateOptions({ wordWrap: wrap ? "on" : "off" }); }}>{$translate("extensions.dev.wrap")}</Button>
  </div>
  <div bind:this={host} class="min-h-0 flex-1 overflow-hidden"></div>
  <div class="flex items-center justify-between gap-3 border-t border-border px-3 py-1 text-[11px] text-muted-foreground">
    <span>{language === "json" ? "JSON" : language === "plaintext" ? "Text" : "JavaScript"}</span>
    <span>Ln {cursorLine}, Col {cursorColumn}</span>
  </div>
</div>
