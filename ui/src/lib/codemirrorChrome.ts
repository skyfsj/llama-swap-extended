// Styling for the CodeMirror UI that lives outside the text area: the search
// panel, its buttons and fields, and the tooltip/autocomplete popups.
//
// CodeMirror ships these as plain DOM with class names only — no styling — so
// without this they render with browser defaults (white inputs, grey buttons)
// in a dark app. Every editor that mounts `basicSetup` should include it:
// basicSetup is what brings the search panel in the first place.
import { EditorView } from "@codemirror/view";
import type { Extension } from "@codemirror/state";

export const codeMirrorChromeTheme: Extension = EditorView.theme({
  // The panel strip (search, go-to-line) shares the editor surface.
  ".cm-panels": {
    backgroundColor: "var(--background)",
    color: "var(--foreground)",
    borderColor: "var(--border)",
  },
  ".cm-panels.cm-panels-top": { borderBottom: "1px solid var(--border)" },
  ".cm-panels.cm-panels-bottom": { borderTop: "1px solid var(--border)" },

  ".cm-search": {
    display: "flex",
    flexWrap: "wrap",
    alignItems: "center",
    gap: "0.375rem",
    padding: "0.5rem 0.75rem",
    fontSize: "0.75rem",
  },
  ".cm-search br": { display: "none" },
  ".cm-search label": {
    display: "inline-flex",
    alignItems: "center",
    gap: "0.25rem",
    color: "var(--muted-foreground)",
    whiteSpace: "nowrap",
    cursor: "pointer",
  },
  ".cm-search input[type=checkbox]": {
    accentColor: "var(--primary)",
    margin: 0,
  },
  ".cm-search .cm-textfield": { flex: "1 1 8rem", minWidth: "6rem" },

  // The same field/button look as the app's inputs, so a panel does not read
  // as a foreign widget dropped into the dialog.
  ".cm-textfield": {
    height: "1.75rem",
    padding: "0 0.5rem",
    border: "1px solid var(--input)",
    borderRadius: "var(--radius-md, 0.375rem)",
    backgroundColor: "var(--background)",
    color: "var(--foreground)",
    fontFamily: "inherit",
    fontSize: "0.75rem",
    outline: "none",
  },
  ".cm-textfield:focus": {
    borderColor: "var(--ring)",
    boxShadow: "0 0 0 2px color-mix(in oklab, var(--ring) 30%, transparent)",
  },
  ".cm-button": {
    height: "1.75rem",
    padding: "0 0.625rem",
    border: "1px solid var(--border)",
    borderRadius: "var(--radius-md, 0.375rem)",
    backgroundImage: "none",
    backgroundColor: "var(--muted)",
    color: "var(--foreground)",
    fontFamily: "inherit",
    fontSize: "0.75rem",
    cursor: "pointer",
  },
  ".cm-button:hover": { backgroundColor: "var(--accent)", color: "var(--accent-foreground)" },
  ".cm-button:active": { backgroundColor: "var(--accent)", color: "var(--accent-foreground)" },

  // The panel's close button is a bare button with a multiplication sign.
  ".cm-search button[name=close]": {
    height: "1.75rem",
    width: "1.75rem",
    marginLeft: "auto",
    border: "1px solid transparent",
    borderRadius: "var(--radius-md, 0.375rem)",
    backgroundColor: "transparent",
    color: "var(--muted-foreground)",
    fontSize: "0.875rem",
    lineHeight: 1,
    cursor: "pointer",
  },
  ".cm-search button[name=close]:hover": {
    backgroundColor: "var(--muted)",
    color: "var(--foreground)",
  },

  // Autocomplete and other hover popups.
  ".cm-tooltip": {
    border: "1px solid var(--border)",
    borderRadius: "var(--radius-md, 0.375rem)",
    backgroundColor: "var(--popover)",
    color: "var(--popover-foreground)",
    overflow: "hidden",
  },
  ".cm-tooltip.cm-tooltip-autocomplete > ul": {
    fontFamily: "ui-monospace, SFMono-Regular, Menlo, Monaco, Consolas, monospace",
    fontSize: "0.75rem",
    maxHeight: "14rem",
  },
  ".cm-tooltip.cm-tooltip-autocomplete > ul > li": { padding: "0.25rem 0.5rem" },
  ".cm-tooltip.cm-tooltip-autocomplete > ul > li[aria-selected]": {
    backgroundColor: "var(--accent)",
    color: "var(--accent-foreground)",
  },
  ".cm-completionLabel": { color: "inherit" },
  ".cm-completionDetail": { color: "var(--muted-foreground)", fontStyle: "normal" },

  // Lint diagnostics: the tooltip under a squiggle and the panel that lists
  // them. Both are plain DOM from @codemirror/lint, so they need the same
  // treatment as the search panel.
  ".cm-tooltip-lint": { padding: "0.25rem" },
  ".cm-diagnostic": {
    padding: "0.25rem 0.5rem",
    borderLeft: "3px solid var(--border)",
    fontFamily: "inherit",
    fontSize: "0.75rem",
  },
  ".cm-diagnostic-error": { borderLeftColor: "var(--destructive)" },
  ".cm-diagnostic-warning": { borderLeftColor: "var(--warning, var(--primary))" },
  ".cm-diagnostic-info": { borderLeftColor: "var(--primary)" },
  ".cm-diagnosticText": { color: "var(--foreground)" },
  ".cm-diagnosticSource": { color: "var(--muted-foreground)" },
  ".cm-panel.cm-panel-lint": {
    padding: "0.25rem 0.5rem",
    fontSize: "0.75rem",
    maxHeight: "12rem",
    overflowY: "auto",
  },
  ".cm-panel.cm-panel-lint ul": { margin: 0, padding: 0, listStyle: "none" },
  ".cm-panel.cm-panel-lint ul li": { padding: "0.125rem 0.25rem", cursor: "pointer" },
  ".cm-panel.cm-panel-lint ul li[aria-selected]": {
    backgroundColor: "var(--accent)",
    color: "var(--accent-foreground)",
  },
  ".cm-panel.cm-panel-lint .cm-diagnosticText": { color: "inherit" },

  // The go-to-line panel is the same dialog shape as the search panel.
  ".cm-panel.cm-gotoLine": { padding: "0.5rem 0.75rem", fontSize: "0.75rem" },
  ".cm-panel.cm-gotoLine label": { color: "var(--muted-foreground)" },

  // Interaction colors. CodeMirror's defaults are tuned for a light surface,
  // so on this one the selection and the active line are close to invisible —
  // every editor needs these, not just the ones that happened to define them.
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
});
