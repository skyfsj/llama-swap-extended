<script lang="ts">
  import { Trash2, FilePlus, FolderPlus, Folder, FolderOpen, ChevronRight, ChevronDown, FileCode2, FileJson, FileText, Pencil, Copy, ClipboardPaste, Scissors } from "@lucide/svelte";
  import { translate } from "../../lib/i18n";
  import { untrack } from "svelte";
  import { extensionTreeRows, fileKind, isEntryPoint, type ExtensionTreeRow } from "../../lib/extensionFiles";

  interface Props {
    paths: string[];
    /** empty directories declared on the draft, shown alongside files */
    directories?: string[];
    activePath: string;
    onOpen: (path: string) => void;
    onCreate: (path: string, kind: "file" | "directory") => void;
    onDelete: (paths: string[]) => void;
    /** move one path (drag or cut/paste) to another directory */
    onMove: (from: string, toDirectory: string) => void;
    /** duplicate one path into the target directory (copy/paste) */
    onDuplicate: (from: string, toDirectory: string) => void;
    /** import files dropped from the operating system */
    onExternalFiles: (files: FileList, targetDirectory: string) => void;
    onRename: (path: string, next: string) => void;
  }

  let { paths, directories = [], activePath, onOpen, onCreate, onDelete, onMove, onDuplicate, onExternalFiles, onRename }: Props = $props();

  // Directories start collapsed; the map records the *expanded* ones. The
  // active file's ancestor directories are always expanded (effect below) so
  // the current tab is visible no matter what. The tree rows merge declared
  // empty directories with the file tree so they render as folders.
  let expanded = $state<Record<string, boolean>>({});
  const allPaths = $derived([...paths, ...directories.filter((entry) => !paths.some((path) => path.startsWith(`${entry}/`)))]);
  const rows = $derived(extensionTreeRows(allPaths, expanded, directories));

  $effect(() => {
    const parts = activePath.split("/");
    untrack(() => {
      const next = { ...expanded };
      for (let i = 1; i < parts.length; i++) next[parts.slice(0, i).join("/")] = true;
      expanded = next;
    });
  });

  // ===== multi-selection (VSCode style) =====
  let selected = $state<string[]>([]);
  let anchor = $state<string | null>(null);

  function selectRow(row: ExtensionTreeRow, event: MouseEvent): void {
    if (event.shiftKey && anchor) {
      const ids = rows.map((entry) => entry.path);
      const from = ids.indexOf(anchor);
      const to = ids.indexOf(row.path);
      if (from !== -1 && to !== -1) {
        const range = ids.slice(Math.min(from, to), Math.max(from, to) + 1);
        selected = [...new Set([...selected, ...range])];
        return;
      }
    }
    if (event.ctrlKey || event.metaKey) {
      selected = selected.includes(row.path) ? selected.filter((path) => path !== row.path) : [...selected, row.path];
      anchor = row.path;
      return;
    }
    selected = [row.path];
    anchor = row.path;
  }

  function selectionFiles(): string[] {
    const chosen = selected.length > 0 ? selected : activePath ? [activePath] : [];
    const files = new Set<string>();
    for (const path of chosen) {
      if (paths.includes(path) || directories.includes(path)) files.add(path);
      const prefix = `${path}/`;
      for (const entry of paths) if (entry.startsWith(prefix)) files.add(entry);
      for (const entry of directories) if (entry.startsWith(prefix)) files.add(entry);
    }
    return [...files];
  }

  // ===== inline create / rename =====
  // VSCode style: the input appears at the target position inside the tree.
  // creating.targetDirectory is where the new entry lands; commitCreate
  // builds the full path from it.
  let creating = $state<null | { kind: "file" | "directory"; targetDirectory: string; value: string }>(null);
  let renaming = $state<null | { path: string; value: string }>(null);
  // Right-click menu state: null path = workspace root menu (create only).
  let createMenu = $state<{ x: number; y: number; path: string | null } | null>(null);
  // Clipboard for cut/copy/paste within the tree. Cut moves on paste; copy
  // duplicates with a "-copy" suffix on name collision.
  let clipboard = $state<{ paths: string[]; cut: boolean } | null>(null);
  // A press-and-hold on a file opens the same menu a right-click does, so a
  // touch screen reaches rename, duplicate and delete without a pointer.
  let longPress = $state<{ timer: ReturnType<typeof setTimeout>; path: string } | null>(null);

  function pressStart(event: PointerEvent, path: string): void {
    if (event.pointerType === "mouse") return;
    longPress = {
      timer: setTimeout(() => {
        longPress = null;
        selected = [path];
        createMenu = { x: event.clientX, y: event.clientY, path };
      }, 450),
      path,
    };
  }

  function pressEnd(): void {
    if (longPress) clearTimeout(longPress.timer);
    longPress = null;
  }

  // beginCreate opens the inline input at the target position. For a
  // right-clicked directory the new entry lands inside it.
  function beginCreate(kind: "file" | "directory", targetDirectory = ""): void {
    createMenu = null;
    if (targetDirectory) expanded = { ...expanded, [targetDirectory]: true };
    creating = { kind, targetDirectory, value: "" };
  }

  function commitCreate(): void {
    if (!creating) return;
    const name = creating.value.trim().replace(/\/+$/, "");
    const kind = creating.kind;
    const directory = creating.targetDirectory;
    creating = null;
    if (!name) return;
    const full = directory ? `${directory}/${name}` : name;
    onCreate(full, kind);
  }

  function beginRename(path: string): void {
    createMenu = null;
    if (directories.includes(path) || paths.includes(path)) {
      const name = path.slice(path.lastIndexOf("/") + 1);
      renaming = { path, value: name };
    } else {
      renaming = { path, value: path };
    }
  }

  function commitRename(): void {
    if (!renaming) return;
    const { path, value } = renaming;
    renaming = null;
    if (value.trim() && value.trim() !== path.slice(path.lastIndexOf("/") + 1)) {
      onRename(path, value.trim());
    }
  }

  // ===== drag & drop =====
  let dragging = $state<string[]>([]);
  let dragOver = $state<string | null>(null);

  function rowDropTarget(row: ExtensionTreeRow): string {
    return row.directory ? row.path : row.path.slice(0, row.path.lastIndexOf("/"));
  }

  function handleDrop(event: DragEvent, targetDirectory: string): void {
    event.preventDefault();
    dragOver = null;
    const sources = dragging.length > 0 ? dragging : selected;
    dragging = [];
    const external = event.dataTransfer?.files;
    if (external && external.length > 0) {
      onExternalFiles(external, targetDirectory);
      return;
    }
    for (const source of sources) {
      if (targetDirectory === source || targetDirectory.startsWith(`${source}/`)) continue;
      const name = source.slice(source.lastIndexOf("/") + 1);
      const next = targetDirectory ? `${targetDirectory}/${name}` : name;
      if (next !== source) onMove(source, next);
    }
  }
</script>

<div
  class="flex h-full min-h-0 flex-col"
  oncontextmenu={(event) => {
    if ((event.target as HTMLElement).closest("li")) return;
    event.preventDefault();
    selected = [];
    createMenu = { x: event.clientX, y: event.clientY, path: null };
  }}
  ondragover={(event) => {
    event.preventDefault();
    if (event.dataTransfer) event.dataTransfer.dropEffect = "copy";
    dragOver = "";
  }}
  ondrop={(event) => handleDrop(event, "")}
  role="presentation"
>
  <div class="flex items-center gap-1 border-b border-border p-2"><span class="flex-1 text-xs text-muted-foreground">{$translate("extensions.files.tree")}</span><button class="rounded p-1 hover:bg-muted" aria-label={$translate("extensions.files.new")} onclick={() => beginCreate("file")}><FilePlus class="size-4" /></button><button class="rounded p-1 hover:bg-muted" aria-label={$translate("extensions.files.newDirectory")} onclick={() => beginCreate("directory")}><FolderPlus class="size-4" /></button></div>
  <ul class="min-h-0 flex-1 space-y-0.5 overflow-y-auto p-1 text-sm">
    {#if creating}
      <li>
        <div class="flex items-center gap-1 rounded-md border border-primary/60 px-1" style:padding-left={`${(creating.targetDirectory ? creating.targetDirectory.split("/").length : 0) * 12 + 4}px`}>
          {#if creating.kind === "directory"}<Folder class="size-4 shrink-0 text-muted-foreground" />{:else}<FileCode2 class="size-4 shrink-0 text-muted-foreground" />{/if}
          <input
            class="min-w-0 flex-1 bg-transparent px-1 py-1 text-xs outline-none"
            value={creating.value}
            placeholder={creating.kind === "directory" ? $translate("extensions.files.newDirectoryPlaceholder") : $translate("extensions.files.newPlaceholder")}
            oninput={(event) => { if (creating) creating.value = event.currentTarget.value; }}
            onkeydown={(event) => {
              if (event.key === "Enter") { event.preventDefault(); commitCreate(); }
              if (event.key === "Escape") { event.preventDefault(); creating = null; }
            }}
            onblur={(event) => {
              // Escape already cleared `creating`; blur after commit is a no-op.
              if (creating) commitCreate();
              void event;
            }}
            aria-label={$translate(creating.kind === "directory" ? "extensions.files.newDirectory" : "extensions.files.new")}
          />
        </div>
      </li>
    {/if}
    {#each rows as row (row.path)}
      <li>
        {#if renaming && renaming.path === row.path}
          <div class="flex items-center gap-1 rounded-md border border-primary/60 px-1" style:padding-left={`${row.depth * 12 + 4}px`}>
            <input
              class="min-w-0 flex-1 bg-transparent px-1 py-1 text-xs outline-none"
              value={renaming.value}
              oninput={(event) => { if (renaming) renaming.value = event.currentTarget.value; }}
              onkeydown={(event) => {
                if (event.key === "Enter") { event.preventDefault(); commitRename(); }
                if (event.key === "Escape") { event.preventDefault(); renaming = null; }
              }}
              onblur={() => { if (renaming) commitRename(); }}
              aria-label={$translate("extensions.files.rename")}
            />
          </div>
        {:else if row.directory}
          <div
            class={`group flex items-center rounded ${selected.includes(row.path) || dragOver === row.path ? "bg-accent" : ""}`}
            style:padding-left={`${row.depth * 12 + 4}px`}
            role="presentation"
            draggable="true"
            ondragstart={(event) => { dragging = selectionFiles(); event.dataTransfer?.setData("text/plain", row.path); }}
            ondragover={(event) => { event.preventDefault(); event.stopPropagation(); if (event.dataTransfer) event.dataTransfer.dropEffect = dragging.length > 0 ? "move" : "copy"; dragOver = row.path; }}
            ondragleave={() => { if (dragOver === row.path) dragOver = null; }}
            ondrop={(event) => handleDrop(event, row.path)}
            oncontextmenu={(event) => { event.preventDefault(); selected = [row.path]; anchor = row.path; createMenu = { x: event.clientX, y: event.clientY, path: row.path }; }}
          >
            <button class="flex w-full items-center gap-1.5 rounded py-1 text-left text-xs hover:bg-muted" aria-expanded={!!expanded[row.path]} onclick={(event) => { selectRow(row, event); expanded = { ...expanded, [row.path]: !expanded[row.path] }; }} onkeydown={(event) => { if (event.key === "ArrowLeft" || event.key === "ArrowRight") { event.preventDefault(); expanded = { ...expanded, [row.path]: event.key === "ArrowRight" }; } }}>
              {#if expanded[row.path]}<ChevronDown class="size-3 shrink-0 text-muted-foreground" /><FolderOpen class="size-4 shrink-0 text-muted-foreground" />{:else}<ChevronRight class="size-3 shrink-0 text-muted-foreground" /><Folder class="size-4 shrink-0 text-muted-foreground" />{/if}<span class="truncate">{row.name}</span>
            </button>
          </div>
        {:else}
          <div
            class={`group flex items-center gap-1 rounded ${selected.includes(row.path) ? "bg-primary/10" : ""} ${dragOver === row.path ? "bg-accent" : ""}`}
            style:padding-left={`${row.depth * 12 + 16}px`}
            draggable="true"
            ondragstart={(event) => { dragging = selectionFiles(); event.dataTransfer?.setData("text/plain", row.path); }}
            ondragover={(event) => { event.preventDefault(); event.stopPropagation(); if (event.dataTransfer) event.dataTransfer.dropEffect = dragging.length > 0 ? "move" : "copy"; dragOver = row.path; }}
            ondragleave={() => { if (dragOver === row.path) dragOver = null; }}
            ondrop={(event) => handleDrop(event, rowDropTarget(row))}
            oncontextmenu={(event) => { event.preventDefault(); selected = selected.includes(row.path) ? selected : [row.path]; anchor = row.path; createMenu = { x: event.clientX, y: event.clientY, path: row.path }; }}
            onpointerdown={(event) => pressStart(event, row.path)}
            onpointerup={pressEnd}
            onpointercancel={pressEnd}
            role="presentation"
          >
            <button class="flex min-w-0 flex-1 items-center gap-1.5 rounded py-1 text-left text-xs hover:bg-muted" aria-current={row.path === activePath ? "true" : undefined} onclick={(event) => { selectRow(row, event); onOpen(row.path); }} title={row.path}>
              {#if fileKind(row.path) === "javascript"}<FileCode2 class="size-4 shrink-0 text-primary" />{:else if fileKind(row.path) === "json"}<FileJson class="size-4 shrink-0 text-muted-foreground" />{:else}<FileText class="size-4 shrink-0 text-muted-foreground" />{/if}<span class="truncate" class:font-medium={isEntryPoint(row.path)}>{row.name}</span>
            </button>
          </div>
        {/if}
      </li>
    {/each}
    {#if paths.length === 0 && directories.length === 0 && !creating}
      <li class="p-2 text-xs text-muted-foreground">{$translate("extensions.files.emptyHint")}</li>
    {/if}
  </ul>
</div>

{#if createMenu}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-40"
    role="presentation"
    onclick={() => createMenu = null}
    oncontextmenu={(event) => { event.preventDefault(); createMenu = null; }}
  ></div>
  <div class="fixed z-50 min-w-44 rounded-md border border-border bg-popover py-1 text-xs shadow-lg" style="left: {createMenu.x}px; top: {createMenu.y}px" role="menu">
    {#if createMenu.path === null}
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => beginCreate("file")}>
        {$translate("extensions.files.new")}
      </button>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => beginCreate("directory")}>
        {$translate("extensions.files.newDirectory")}
      </button>
    {:else}
      {@const path = createMenu.path}
      {@const isDeclaredDirectory = directories.includes(path)}
      {@const targetDirectory = isDeclaredDirectory || paths.some((entry) => entry.startsWith(`${path}/`)) ? path : path.slice(0, path.lastIndexOf("/"))}
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => beginCreate("file", targetDirectory)}>
        {$translate("extensions.files.new")}
      </button>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => beginCreate("directory", targetDirectory)}>
        {$translate("extensions.files.newDirectory")}
      </button>
      <div class="my-1 border-t border-border"></div>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => beginRename(path)}>
        <span class="inline-flex items-center gap-2"><Pencil class="size-3" />{$translate("extensions.files.rename")}</span>
      </button>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { onDuplicate(path, targetDirectory); createMenu = null; }}>
        <span class="inline-flex items-center gap-2"><Copy class="size-3" />{$translate("extensions.duplicate")}</span>
      </button>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { clipboard = { paths: selectionFiles(), cut: true }; createMenu = null; }}>
        <span class="inline-flex items-center gap-2"><Scissors class="size-3" />{$translate("extensions.files.cut")}</span>
      </button>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { clipboard = { paths: selectionFiles(), cut: false }; createMenu = null; }}>
        <span class="inline-flex items-center gap-2"><Copy class="size-3" />{$translate("extensions.files.copy")}</span>
      </button>
      {#if clipboard && clipboard.paths.length > 0}
        {@const pasteTarget = isDeclaredDirectory ? path : targetDirectory}
        {@const clipboardPaths = clipboard.paths}
        {@const clipboardCut = clipboard.cut}
        <div class="my-1 border-t border-border"></div>
        <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { for (const source of clipboardPaths) { if (!clipboardCut || source !== path) onDuplicate(source, pasteTarget); } if (clipboardCut) { for (const source of clipboardPaths) onDelete([source]); } clipboard = null; createMenu = null; }}>
          <span class="inline-flex items-center gap-2"><ClipboardPaste class="size-3" />{$translate("extensions.files.paste")}</span>
        </button>
      {/if}
      <div class="my-1 border-t border-border"></div>
      <button class="block w-full px-3 py-1.5 text-left text-destructive hover:bg-destructive/10" role="menuitem" onclick={() => { const files = selectionFiles(); if (files.length > 0) onDelete(files); createMenu = null; }}>
        <span class="inline-flex items-center gap-2"><Trash2 class="size-3" />{$translate("extensions.delete")}</span>
      </button>
    {/if}
  </div>
{/if}
