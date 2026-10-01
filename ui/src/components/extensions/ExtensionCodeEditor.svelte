<script lang="ts">
  import { onMount } from "svelte";
  import { basicSetup, EditorView } from "codemirror";
  import { EditorState, StateEffect, StateField } from "@codemirror/state";
  import { javascript } from "@codemirror/lang-javascript";
  import { json } from "@codemirror/lang-json";
  import { autocompletion, type CompletionContext, type CompletionSource } from "@codemirror/autocomplete";
  import { linter, type Diagnostic } from "@codemirror/lint";
  import { extensionCompletionSource, packageJsonCompletions } from "../../lib/extensionCompletions";
  import type { ExtensionDiagnostic } from "../../lib/extensionsApi";

  interface Props {
    value: string;
    ariaLabel: string;
    path: string;
    settingKeys?: string[];
    diagnostics?: ExtensionDiagnostic[];
    onChange: (value: string) => void;
  }

  let { value, ariaLabel, path, settingKeys = [], diagnostics = [], onChange }: Props = $props();

  let host: HTMLDivElement;
  let view: EditorView | null = null;
  let syncing = false;
  let cursorLine = $state(1);
  let cursorColumn = $state(1);

  const language = $derived(path.endsWith(".json") ? "json" : "javascript");

  // Server diagnostics arrive out of band, so they are held in a state field and
  // projected into CodeMirror's linter rather than re-created per keystroke.
  const setDiagnostics = StateEffect.define<ExtensionDiagnostic[]>();
  const diagnosticsField = StateField.define<ExtensionDiagnostic[]>({
    create: () => [],
    update: (current, transaction) => {
      for (const effect of transaction.effects) {
        if (effect.is(setDiagnostics)) return effect.value;
      }
      return current;
    },
  });

  /**
   * toDiagnostics turns server diagnostics for this file into editor ranges.
   * Diagnostics for sibling modules are left to their own tabs.
   */
  function toDiagnostics(reported: ExtensionDiagnostic[]): Diagnostic[] {
    if (!view) return [];
    const found = reported.filter((result) => (result.path || path) === path);
    const ranges: Diagnostic[] = [];
    for (const result of found) {
      const lineNumber = Math.min(Math.max(1, result.line || 1), view.state.doc.lines);
      const line = view.state.doc.line(lineNumber);
      const from = Math.min(line.from + Math.max(0, (result.column || 1) - 1), line.to);
      const to = Math.min(from + Math.max(1, result.length || 1), line.to);
      ranges.push({
        from, to,
        severity: result.severity === "warning" ? "warning" : "error",
        message: result.message,
      });
    }
    return ranges;
  }

  const linting = linter((view) => toDiagnostics(view.state.field(diagnosticsField)));

  const jsonCompletionSource: CompletionSource = (context: CompletionContext) => {
    const before = context.matchBefore(/[\w"]*$/);
    if (!before && !context.explicit) return null;
    return {
      from: context.pos - (before?.text.length ?? 0),
      options: packageJsonCompletions,
      validFor: /^[\w"]*$/,
    };
  };

  function completionSourceFor(): CompletionSource {
    const source = extensionCompletionSource(settingKeys);
    return (context) => source(context);
  }

  onMount(() => {
    view = new EditorView({
      state: EditorState.create({
        doc: value,
        extensions: [
          basicSetup,
          language === "json" ? json() : javascript(),
          autocompletion({
            override: [language === "json" ? jsonCompletionSource : completionSourceFor()],
            activateOnTyping: true,
            icons: false,
          }),
          diagnosticsField,
          linting,
          EditorView.updateListener.of((update) => {
            if (update.selectionSet || update.docChanged) {
              const position = update.state.selection.main.head;
              const line = update.state.doc.lineAt(position);
              cursorLine = line.number;
              cursorColumn = position - line.from + 1;
            }
            if (update.docChanged && !syncing) onChange(update.state.doc.toString());
          }),
          EditorView.contentAttributes.of({ "aria-label": ariaLabel }),
          EditorView.theme({
            "&": { backgroundColor: "var(--background)", color: "var(--foreground)", height: "100%", fontSize: "13px" },
            ".cm-scroller": { fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace", overflow: "auto" },
            ".cm-content": { padding: "0.5rem" },
            "&.cm-focused": { outline: "none" },
            ".cm-gutters": { backgroundColor: "var(--background)", color: "var(--muted-foreground)", borderRight: "1px solid var(--border)" },
            ".cm-tooltip": { backgroundColor: "var(--popover)", color: "var(--popover-foreground)", border: "1px solid var(--border)" },
            ".cm-tooltip-autocomplete ul li[aria-selected]": { backgroundColor: "var(--accent)", color: "var(--accent-foreground)" },
          }),
        ],
      }),
      parent: host,
    });
    return () => { view?.destroy(); view = null; };
  });

  // Pushed in from the page after a check round-trip.
  $effect(() => {
    view?.dispatch({ effects: setDiagnostics.of(diagnostics ?? []) });
  });

  $effect(() => {
    if (!view || view.state.doc.toString() === value) return;
    syncing = true;
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: value } });
    syncing = false;
  });
</script>

<div class="flex h-full min-h-0 flex-col overflow-hidden rounded-md border border-border bg-background">
  <div bind:this={host} class="min-h-0 flex-1 overflow-hidden"></div>
  <div class="flex items-center justify-between gap-3 border-t border-border px-3 py-1 text-[11px] text-muted-foreground">
    <span>{language === "json" ? "JSON" : "JavaScript"}</span>
    <span>Ln {cursorLine}, Col {cursorColumn}</span>
  </div>
</div>
