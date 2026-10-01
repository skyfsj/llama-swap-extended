<script lang="ts">
  import { onMount } from "svelte";
  import { basicSetup, EditorView } from "codemirror";
  import type { ViewUpdate } from "@codemirror/view";
  import { Compartment, EditorState, type Extension } from "@codemirror/state";

  interface Props {
    value?: string;
    readonly?: boolean;
    invalid?: boolean;
    compact?: boolean;
    placeholder?: string;
    ariaLabel: string;
    onChange?: (value: string) => void;
  }

  let {
    value = $bindable(""),
    readonly = false,
    invalid = false,
    compact = false,
    placeholder = "",
    ariaLabel,
    onChange,
  }: Props = $props();

  let host = $state<HTMLDivElement | null>(null);
  let editor = $state<EditorView | null>(null);
  let syncingExternalValue = false;

  const accessibility = new Compartment();
  const editability = new Compartment();

  const editorTheme = EditorView.theme({
    "&": {
      backgroundColor: "var(--background)",
      color: "var(--foreground)",
      fontSize: "0.8125rem",
    },
    ".cm-scroller": {
      fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, 'Liberation Mono', monospace",
      lineHeight: "1.55",
      overflow: "auto",
    },
    ".cm-content": {
      position: "relative",
      minHeight: "100%",
      padding: "0.75rem 0",
      caretColor: "var(--foreground)",
    },
    ".cm-content::before": {
      content: "var(--launch-args-placeholder)",
      color: "var(--muted-foreground)",
      position: "absolute",
      top: "0.75rem",
      left: "0.875rem",
      pointerEvents: "none",
      visibility: "hidden",
      whiteSpace: "pre",
    },
    ".cm-empty .cm-content::before": {
      visibility: "visible",
    },
    ".cm-line": { padding: "0 0.875rem" },
    ".cm-gutters": {
      minHeight: "100%",
      borderRight: "1px solid var(--border)",
      backgroundColor: "var(--muted)",
      color: "var(--muted-foreground)",
    },
    ".cm-gutterElement": { padding: "0 0.625rem" },
    ".cm-activeLine": { backgroundColor: "color-mix(in oklab, var(--primary) 8%, transparent)" },
    ".cm-activeLineGutter": { backgroundColor: "color-mix(in oklab, var(--primary) 12%, transparent)" },
    ".cm-selectionBackground, &.cm-focused .cm-selectionBackground, ::selection": {
      backgroundColor: "color-mix(in oklab, var(--primary) 28%, transparent) !important",
    },
    ".cm-cursor, .cm-dropCursor": { borderLeftColor: "var(--foreground)" },
    ".cm-matchingBracket": {
      backgroundColor: "color-mix(in oklab, var(--primary) 18%, transparent)",
      outline: "1px solid color-mix(in oklab, var(--primary) 55%, transparent)",
    },
    "&.cm-focused": { outline: "2px solid var(--ring)", outlineOffset: "-2px" },
  });

  function placeholderExtension(): Extension {
    return EditorView.updateListener.of((update: ViewUpdate) => {
      const empty = update.state.doc.length === 0;
      const dom = update.view.dom;
      if (empty !== dom.classList.contains("cm-empty")) {
        dom.classList.toggle("cm-empty", empty);
      }
    });
  }

  function accessAttributes(): ReturnType<typeof EditorView.contentAttributes.of> {
    return EditorView.contentAttributes.of({
      "aria-label": ariaLabel,
      "aria-invalid": invalid ? "true" : "false",
      spellcheck: "false",
    });
  }

  function editableExtensions(): Extension {
    return [EditorState.readOnly.of(readonly), EditorView.editable.of(!readonly)];
  }

  function updateEditorValue(next: string): void {
    const view = editor;
    if (!view || view.state.doc.toString() === next) return;
    syncingExternalValue = true;
    view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: next } });
    syncingExternalValue = false;
  }

  $effect(() => {
    updateEditorValue(value ?? "");
  });

  $effect(() => {
    const view = editor;
    if (!view) return;
    view.dispatch({ effects: editability.reconfigure(editableExtensions()) });
  });

  $effect(() => {
    const view = editor;
    if (!view) return;
    view.dispatch({ effects: accessibility.reconfigure(accessAttributes()) });
  });

  onMount(() => {
    if (!host) return;
    host.classList.toggle("cm-empty", (value ?? "").length === 0);
    editor = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: value ?? "",
        extensions: [
          basicSetup,
          editorTheme,
          placeholderExtension(),
          EditorView.lineWrapping,
          editability.of(editableExtensions()),
          accessibility.of(accessAttributes()),
          EditorView.updateListener.of((update) => {
            if (!update.docChanged || syncingExternalValue) return;
            const next = update.state.doc.toString();
            value = next;
            onChange?.(next);
          }),
        ],
      }),
    });

    return () => {
      editor?.destroy();
      editor = null;
    };
  });
</script>

<div class="launch-args-editor rounded-lg border" class:invalid class:compact style={`--launch-args-placeholder: ${JSON.stringify(placeholder)}`} bind:this={host}></div>

<style>
  .launch-args-editor {
    block-size: clamp(10rem, calc(100dvh - 26rem), 24rem);
    border-color: var(--input);
    overflow: hidden;
  }

  .launch-args-editor.compact {
    block-size: clamp(6rem, 9rem, 10rem);
  }

  .launch-args-editor.invalid {
    border-color: var(--destructive);
  }

  :global(.launch-args-editor .cm-editor) {
    block-size: 100%;
  }

  :global(.launch-args-editor .cm-scroller) {
    overflow: auto;
  }

  @media (max-width: 640px) {
    .launch-args-editor {
      block-size: clamp(8rem, calc(100dvh - 28rem), 18rem);
    }

    .launch-args-editor.compact {
      block-size: 5rem;
    }
  }
</style>
