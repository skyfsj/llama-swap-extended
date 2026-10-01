<script lang="ts">
  import { onMount } from "svelte";
  import { basicSetup, EditorView } from "codemirror";
  import { Compartment, EditorState, type Extension } from "@codemirror/state";
  import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
  import { tags } from "@lezer/highlight";
  import { yaml } from "@codemirror/lang-yaml";

  interface Props {
    value?: string;
    readonly?: boolean;
    invalid?: boolean;
    ariaLabel: string;
    onChange?: (value: string) => void;
  }

  let {
    value = $bindable(""),
    readonly = false,
    invalid = false,
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
      minHeight: "100%",
      padding: "0.75rem 0",
      caretColor: "var(--foreground)",
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
    ".cm-foldPlaceholder": {
      border: "1px solid var(--border)",
      backgroundColor: "var(--muted)",
      color: "var(--muted-foreground)",
    },
    "&.cm-focused": { outline: "2px solid var(--ring)", outlineOffset: "-2px" },
  });

  const yamlHighlighting = syntaxHighlighting(HighlightStyle.define([
    { tag: [tags.keyword, tags.bool, tags.null], color: "var(--primary)", fontWeight: "600" },
    { tag: [tags.string, tags.special(tags.string)], color: "var(--success)" },
    { tag: [tags.number, tags.integer, tags.float], color: "var(--warning)" },
    { tag: [tags.comment, tags.lineComment], color: "var(--muted-foreground)", fontStyle: "italic" },
    { tag: [tags.punctuation, tags.separator], color: "var(--muted-foreground)" },
  ]));

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
    editor = new EditorView({
      parent: host,
      state: EditorState.create({
        doc: value ?? "",
        extensions: [
          basicSetup,
          yaml(),
          yamlHighlighting,
          editorTheme,
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

<div id="config-yaml-editor" class:invalid class="yaml-code-editor rounded-lg border" bind:this={host}></div>

<style>
  .yaml-code-editor {
    block-size: clamp(16rem, calc(100dvh - 15rem), 48rem);
    border-color: var(--input);
    overflow: hidden;
  }

  :global(.yaml-code-editor .cm-editor) {
    block-size: 100%;
  }

  :global(.yaml-code-editor .cm-scroller) {
    overflow: auto;
  }

  .yaml-code-editor.invalid {
    border-color: var(--destructive);
  }

  @media (max-width: 640px) {
    .yaml-code-editor {
      block-size: clamp(14rem, calc(100dvh - 18rem), 32rem);
    }

    :global(.yaml-code-editor .cm-gutterElement) {
      padding-inline: 0.4rem;
    }

    :global(.yaml-code-editor .cm-line) {
      padding-inline: 0.625rem;
    }
  }
</style>
