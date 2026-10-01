<script lang="ts">
  import { onMount, untrack } from "svelte";
  import { get } from "svelte/store";
  import { translate } from "../lib/i18n";
  import { toast } from "../lib/toast";
  import { exportExtension } from "../lib/extensionsApi";
  import { replace, push } from "svelte-spa-router";
  import { Button } from "$lib/components/ui/button/index.js";
  import { ArrowLeft, Save, Play, Settings2, FolderTree, Maximize2, Minimize2, ChevronDown, ChevronUp, X, FileCode2, FileJson, FileText, BookOpen, Download } from "@lucide/svelte";
  import * as Switch from "$lib/components/ui/switch/index.js";
  import ExtensionConfiguration from "../components/extensions/ExtensionConfiguration.svelte";
  import ConfirmDialog from "../components/ConfirmDialog.svelte";
  import ExtensionMonacoEditor from "../components/extensions/ExtensionMonacoEditor.svelte";
  import ExtensionFileTree from "../components/extensions/ExtensionFileTree.svelte";
  import ExtensionConsolePanel from "../components/extensions/ExtensionConsolePanel.svelte";
  import ExtensionWorkbench from "../components/extensions/ExtensionWorkbench.svelte";
  import {
    EXTENSION_ENTRY_POINT,
    blankExtension,
    settingDiagnosticsFrom,
    checkExtension,
    getExtension,
    saveExtension,
    type ExtensionDefinition,
    type ExtensionDiagnostic,
  } from "../lib/extensionsApi";
  import { fileKind, fileLabel, normalizeDirectoryPath, normalizeFilePath, sortFilePaths } from "../lib/extensionFiles";

  interface Props {
    params?: { id?: string } | null;
  }

  let { params = {} }: Props = $props();

  let dockTab = $state<"problems" | "logs" | "debug" | "benchmark">("debug");
  let dockOpen = $state(true);
  let dockExpanded = $state(false);
  let testing = $state(false);
  let revealLocation = $state<{ line: number; column: number } | undefined>();
  let settingsErrors = $state<Record<string, string>>({});
  let showSettings = $state(true);
  let showFiles = $state(true);
  let leaveOpen = $state(false);
  let pendingHref = "#/extensions";
  let allowNavigation = false;
  let loadRun = 0;
  let checkError = $state("");
  let draft = $state<ExtensionDefinition | null>(null);
  let savedSnapshot = $state("");
  let openFiles = $state<string[]>([]);
  let activePath = $state(EXTENSION_ENTRY_POINT);
  let busy = $state(false);
  let error = $state("");
  let diagnostics = $state<ExtensionDiagnostic[]>([]);
  let checking = $state(false);

  const dirty = $derived(
    draft !== null && (JSON.stringify({ manifest: draft.manifest, files: draft.files, directories: draft.directories ?? [] }) !== savedSnapshot),
  );
  const filePaths = $derived(draft ? sortFilePaths(Object.keys(draft.files)) : []);
  const settingKeys = $derived((draft?.settings ?? []).map((field) => field.key));

  // ===== 自动保存（本地草稿防丢失）=====
  // 变更 1.5s 后写入 localStorage；保存到服务器（部署）后清除。与部署无关，
  // 只保证崩溃/误关后草稿可恢复。
  const draftStorageKey = $derived(draft ? `extension-draft:${draft.manifest.id}` : null);
  let autoSaveTimer: ReturnType<typeof setTimeout> | undefined;

  function scheduleAutoSave(): void {
    clearTimeout(autoSaveTimer);
    autoSaveTimer = setTimeout(() => {
      if (!draft || !draftStorageKey || !dirty) return;
      try {
        localStorage.setItem(draftStorageKey, JSON.stringify({ manifest: draft.manifest, files: draft.files, directories: draft.directories ?? [] }));
      } catch { /* quota/private mode: auto-save is best effort */ }
    }, 1500);
  }

  function clearAutoSave(): void {
    clearTimeout(autoSaveTimer);
    if (draftStorageKey) localStorage.removeItem(draftStorageKey);
  }

  function restoreAutoSave(id: string): boolean {
    try {
      const raw = localStorage.getItem(`extension-draft:${id}`);
      if (!raw) return false;
      const saved = JSON.parse(raw) as ExtensionDefinition;
      if (!saved.files || typeof saved.files !== "object") return false;
      draft = { ...saved, manifest: { ...saved.manifest, id }, etag: "", status: "draft-restored" };
      savedSnapshot = snapshot({ ...saved, manifest: { ...saved.manifest, id } });
      return true;
    } catch {
      return false;
    }
  }

  function message(key: string, params?: Record<string, string | number>): string {
    return get(translate)(key, params);
  }

  // docsHref is a fully-resolved URL (not a bare hash) so target="_blank"
  // always opens a fresh tab with the docs route, independent of this
  // editor's hashchange guard.
  const docsHref = `${window.location.origin}${window.location.pathname}#/extensions/docs`;

  function snapshot(definition: ExtensionDefinition): string {
    return JSON.stringify({ manifest: definition.manifest, files: definition.files, directories: definition.directories ?? [] });
  }

  async function load(id?: string): Promise<void> {
    const run = ++loadRun;
    checkRun++;
    busy = true; draft = null; error = ""; settingsErrors = {};
    try {
      const fetched = id ? await getExtension(id) : blankExtension();
      if (run !== loadRun) return;
      // A locally auto-saved draft wins over the stored copy: it is newer work
      // the operator has not deployed yet. The stored version is untouched.
      const restored = id ? restoreAutoSave(id) : false;
      draft = restored ? draft : fetched;
      savedSnapshot = snapshot(fetched);
      openFiles = [EXTENSION_ENTRY_POINT];
      activePath = EXTENSION_ENTRY_POINT;
      diagnostics = [];
      error = "";
      if (restored) toast.info(message("extensions.draftRestored"));
    } catch (cause) {
      if (run === loadRun) error = String(cause);
    } finally {
      if (run === loadRun) busy = false;
    }
  }

  function updateSource(value: string): void {
    if (!draft || !activePath) return;
    draft.files = { ...draft.files, [activePath]: value };
  }

  async function exportArchive(): Promise<void> {
    if (!draft?.manifest.id || !draft.etag) return;
    try {
      const blob = await exportExtension(draft.manifest.id);
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      anchor.download = `${draft.manifest.id}.zip`;
      anchor.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
      toast.success(message("extensions.import.exported", { id: draft.manifest.id }));
    } catch (cause) { error = String(cause); }
  }

  async function save(): Promise<void> {
    if (!draft || busy) return;
    busy = true; error = "";
    try {
      settingsErrors = {};
      const saved = await saveExtension(draft);
      clearAutoSave();
      draft = saved;
      savedSnapshot = snapshot(saved);
      if (!params?.id) void replace(`/extensions/${encodeURIComponent(saved.manifest.id)}/editor`);
      openFiles = openFiles.filter((path) => Object.hasOwn(saved.files, path));
      if (!openFiles.includes(activePath)) activePath = openFiles[0] ?? EXTENSION_ENTRY_POINT;
      toast.success(message("extensions.saved"));
      await runCheck();
    } catch (cause) {
      error = String(cause);
      settingsErrors = settingDiagnosticsFrom(cause);
      showSettings = true;
    } finally {
      busy = false;
    }
  }

  let checkTimer: ReturnType<typeof setTimeout> | undefined;
  let checkRun = 0;

  function scheduleCheck(): void {
    checkRun++;
    checking = true;
    clearTimeout(checkTimer);
    checkTimer = setTimeout(() => { void runCheck(); }, 500);
  }

  async function runCheck(): Promise<void> {
    if (!draft || Object.keys(draft.files).length === 0) return;
    checking = true; checkError = "";
    const run = ++checkRun;
    try {
      const found = await checkExtension({ files: draft.files });
      if (run === checkRun) diagnostics = found;
    } catch (cause) {
      if (run === checkRun) checkError = String(cause);
    } finally {
      if (run === checkRun) checking = false;
    }
  }

  function openFile(path: string): void {
    if (!openFiles.includes(path)) openFiles = [...openFiles, path];
    activePath = path; revealLocation = undefined;
  }

  function closeFile(path: string): void {
    const index = openFiles.indexOf(path);
    openFiles = openFiles.filter((entry) => entry !== path);
    if (activePath === path) activePath = openFiles[Math.min(index, openFiles.length - 1)] ?? "";
  }

  const savedFiles = $derived(savedSnapshot ? (JSON.parse(savedSnapshot) as ExtensionDefinition).files : {});


  function createPath(raw: string, kind: "file" | "directory"): void {
    if (!draft) return;
    // A directory is materialized as a starter index.js: the extension's file
    // map has no empty folders, and a bare folder would vanish on the next save.
    const target = normalizeFilePath(kind === "directory" ? `${raw.replace(/\/+$/, "")}/index.js` : raw);
    if (!target) { error = message("extensions.files.invalidPath", { path: raw }); return; }
    if (draft.files[target] !== undefined) { error = message("extensions.files.exists", { path: target }); return; }
    draft.files = { ...draft.files, [target]: "" };
    if (!openFiles.includes(target)) openFiles = [...openFiles, target];
    activePath = target;
  }

  // createEntry handles the tree's create flow: directories are validated with
  // the directory rules (no extension required) and materialized with a
  // starter index.js; files need a managed extension.
  function createEntry(raw: string, kind: "file" | "directory"): void {
    if (!draft) return;
    if (kind === "directory") {
      const directory = normalizeDirectoryPath(raw);
      if (!directory) { error = message("extensions.files.invalidPath", { path: raw }); return; }
      if (draft.files[directory] !== undefined || draft.directories?.includes(directory)) { error = message("extensions.files.exists", { path: directory }); return; }
      // Empty directories are a first-class part of the definition; the save
      // materializes them on disk. Expanding one shows a hint, not a file.
      draft.directories = [...(draft.directories ?? []), directory].sort();
      return;
    }
    createPath(raw, "file");
  }

  // ===== tree operations (move / duplicate / rename / upload / delete) =====

  function movePath(from: string, toDirectory: string): void {
    if (!draft) return;
    const name = from.slice(from.lastIndexOf("/") + 1);
    const next = toDirectory ? `${toDirectory}/${name}` : name;
    if (next === from || next.startsWith(`${from}/`)) return;
    const isDirectory = draft.directories?.includes(from) ?? false;
    const files: Record<string, string> = {};
    for (const [key, value] of Object.entries(draft.files)) {
      if (key === from || key.startsWith(`${from}/`)) {
        if (isDirectory) continue; // empty directory: nothing to move
        const suffix = key.slice(from.length);
        files[`${next}${suffix}`] = value;
      } else {
        files[key] = value;
      }
    }
    draft.files = files;
    if (isDirectory) {
      draft.directories = (draft.directories ?? []).filter((entry) => entry !== from).concat(next).sort();
    }
    openFiles = openFiles.map((open) => (open === from || open.startsWith(`${from}/`) ? `${next}${open.slice(from.length)}` : open));
    if (activePath === from || activePath.startsWith(`${from}/`)) activePath = `${next}${activePath.slice(from.length)}`;
  }

  function duplicatePath(from: string, toDirectory: string): void {
    if (!draft) return;
    const name = from.slice(from.lastIndexOf("/") + 1);
    const base = toDirectory ? `${toDirectory}/${name}` : name;
    // An empty directory duplicates by re-declaring it under the new name.
    if (draft.directories?.includes(from)) {
      const dot = name.lastIndexOf(".");
      const stem = dot > 0 ? name.slice(0, dot) : name;
      const suffix = dot > 0 ? name.slice(dot) : "";
      let candidate = `${base.slice(0, base.length - name.length)}${stem}-copy${suffix}`;
      let counter = 2;
      while (draft.directories.includes(candidate) || draft.files[candidate] !== undefined) {
        candidate = `${base.slice(0, base.length - name.length)}${stem}-copy${counter}${suffix}`;
        counter++;
      }
      draft.directories = [...draft.directories, candidate].sort();
      return;
    }
    const dot = name.lastIndexOf(".");
    const stem = dot > 0 ? name.slice(0, dot) : name;
    const suffix = dot > 0 ? name.slice(dot) : "";
    let candidate = `${base.slice(0, base.length - name.length)}${stem}-copy${suffix}`;
    let counter = 2;
    while (draft.files[candidate] !== undefined) {
      candidate = `${base.slice(0, base.length - name.length)}${stem}-copy${counter}${suffix}`;
      counter++;
    }
    const files: Record<string, string> = { ...draft.files };
    const prefix = `${from}/`;
    let copiedAny = false;
    for (const [key, value] of Object.entries(draft.files)) {
      if (key === from) { files[candidate] = value; copiedAny = true; }
      else if (key.startsWith(prefix)) { files[`${candidate}${key.slice(from.length)}`] = value; copiedAny = true; }
    }
    // A directory duplicate copies at least one child; the dir itself is never
    // a file entry, so success is measured by children, not by the dir path.
    if (!copiedAny) return;
    draft.files = files;
  }

  async function importExternalFiles(list: FileList, targetDirectory: string): Promise<void> {
    if (!draft) return;
    const files = { ...draft.files };
    const added: string[] = [];
    for (const file of list) {
      const name = file.name.replace(/\\/g, "_");
      const target = targetDirectory ? `${targetDirectory}/${name}` : name;
      const normalized = normalizeFilePath(target);
      if (!normalized) { error = message("extensions.files.invalidPath", { path: file.name }); continue; }
      if (file.size > 256 * 1024) { error = message("extensions.files.tooLarge", { path: file.name }); continue; }
      files[normalized] = await file.text();
      added.push(normalized);
    }
    if (added.length === 0) return;
    draft.files = files;
    toast.success(message("extensions.files.imported", { count: added.length }));
    if (added.length === 1) { openFiles = [...new Set([...openFiles, added[0]])]; activePath = added[0]; }
  }

  function renamePath(path: string, next: string): void {
    if (!draft) return;
    const directory = path.includes("/") ? path.slice(0, path.lastIndexOf("/") + 1) : "";
    const isDirectory = draft.directories?.includes(path) ?? false;
    const normalized = isDirectory ? directory + normalizeDirectoryPath(next) : directory + normalizeFilePath(next);
    if (normalized === path) return;
    if (isDirectory) {
      if (!normalizeDirectoryPath(normalized)) { error = message("extensions.files.invalidPath", { path: next }); return; }
      if (draft.directories?.includes(normalized)) { error = message("extensions.files.exists", { path: normalized }); return; }
      draft.directories = (draft.directories ?? []).map((entry) => (entry === path ? normalized : entry)).sort();
      return;
    }
    if (!normalizeFilePath(normalized)) { error = message("extensions.files.invalidPath", { path: next }); return; }
    if (draft.files[normalized] !== undefined) { error = message("extensions.files.exists", { path: normalized }); return; }
    const files: Record<string, string> = {};
    for (const [key, value] of Object.entries(draft.files)) files[key === path ? normalized : key] = value;
    draft.files = files;
    openFiles = openFiles.map((open) => (open === path ? normalized : open));
    if (activePath === path) activePath = normalized;
  }

  // Batch delete asks once for the whole selection; a directory delete counts
  // everything beneath it.
  let deleteOpen = $state(false);
  let deleteSelection = $state<string[]>([]);
  // Editor tab context menu (VSCode style): close / close others / close to
  // the right / close all / copy path.
  let tabMenu = $state<{ path: string; x: number; y: number } | null>(null);

  function closeTabs(pathsToClose: string[]): void {
    openFiles = openFiles.filter((open) => !pathsToClose.includes(open));
    if (!openFiles.includes(activePath)) activePath = openFiles[0] ?? EXTENSION_ENTRY_POINT;
  }

  function requestDelete(pathsToDelete: string[]): void {
    const files = pathsToDelete.filter((path) => path !== EXTENSION_ENTRY_POINT);
    if (files.length === 0) { error = message("extensions.files.entryRequired"); return; }
    deleteSelection = files;
    deleteOpen = true;
  }

  function confirmDelete(): void {
    if (!draft) return;
    const doomed = new Set(deleteSelection);
    const files: Record<string, string> = {};
    for (const [key, value] of Object.entries(draft.files)) {
      if ([...doomed].some((path) => key === path || key.startsWith(`${path}/`))) continue;
      files[key] = value;
    }
    draft.files = files;
    draft.directories = (draft.directories ?? []).filter((entry) => ![...doomed].some((path) => entry === path || entry.startsWith(`${path}/`)));
    openFiles = openFiles.filter((open) => !doomed.has(open));
    if (doomed.has(activePath) || !openFiles.includes(activePath)) activePath = openFiles[0] ?? EXTENSION_ENTRY_POINT;
  }

  function cancel() {
    if (dirty) { pendingHref = "#/extensions"; leaveOpen = true; }
    else void push("/extensions");
  }

  onMount(() => {
    let activeHash = window.location.hash;
    const guardHash = (event: Event) => {
      if (!dirty || allowNavigation || window.location.hash === activeHash) { activeHash = window.location.hash; return; }
      pendingHref = window.location.hash;
      event.stopImmediatePropagation();
      window.history.pushState(window.history.state, "", activeHash);
      leaveOpen = true;
    };
    window.addEventListener("hashchange", guardHash, true);
    const beforeUnload = (event: BeforeUnloadEvent) => { if (dirty) { event.preventDefault(); event.returnValue = ""; } };
    const intercept = (event: MouseEvent) => {
      const anchor = (event.target as Element | null)?.closest?.("a[href]") as HTMLAnchorElement | null;
      if (!dirty || !anchor || anchor.target === "_blank" || event.metaKey || event.ctrlKey) return;
      const href = anchor.getAttribute("href");
      if (!href?.startsWith("#/") || href === window.location.hash) return;
      event.preventDefault(); event.stopPropagation(); pendingHref = href; leaveOpen = true;
    };
    document.addEventListener("click", intercept, true);
    window.addEventListener("beforeunload", beforeUnload);
    return () => { clearTimeout(checkTimer); clearTimeout(autoSaveTimer); checkRun++; loadRun++; window.removeEventListener("hashchange", guardHash, true); document.removeEventListener("click", intercept, true); window.removeEventListener("beforeunload", beforeUnload); };
  });

  $effect(() => {
    const id = params?.id;
    untrack(() => void load(id));
  });

  $effect(() => {
    if (draft?.files) scheduleCheck();
  });

  $effect(() => {
    if (draft && dirty) scheduleAutoSave();
  });
</script>

<svelte:window onkeydown={(event) => { if ((event.metaKey || event.ctrlKey) && event.key.toLowerCase() === "s") { event.preventDefault(); if (dirty && !busy && !testing) void save(); } }} />

<svelte:head><title>{draft ? draft.manifest.name || draft.manifest.id : message("extensions.title")}</title></svelte:head>

<div class="flex h-full min-h-[600px] min-w-0 flex-col overflow-hidden rounded-lg border border-border bg-card">
  <header class="flex shrink-0 flex-wrap items-center gap-3 border-b border-border px-4 py-3">
    <Button variant="ghost" size="icon" onclick={cancel} aria-label={message("extensions.editor.back")}><ArrowLeft class="size-4" /></Button>
    <div class="min-w-40 flex-1"><h1 class="truncate !pb-0 text-lg font-semibold leading-tight">{draft?.manifest.name || draft?.manifest.id || message("extensions.workspace.create")}</h1><p class="mt-1 truncate text-xs text-muted-foreground">{draft?.manifest.id || message("extensions.workspace.editSubtitle")}{#if draft?.manifest.description}<span class="ml-3">{draft.manifest.description}</span>{/if}</p></div>
    {#if draft}<Switch.Root bind:checked={draft.manifest.enabled} disabled={busy || testing} aria-label={message("extensions.enabled")} />{/if}
    {#if dirty}<span class="rounded-full bg-warning/15 px-2 py-1 text-xs text-warning">{message("extensions.unsaved")}</span>{/if}
    <div class="flex w-full flex-wrap items-center justify-end gap-2 sm:w-auto">
      <Button variant="ghost" size="icon" href={docsHref} target="_blank" rel="noopener noreferrer" aria-label={message("extensions.dev.docs")}><BookOpen class="size-4" /></Button>
      <Button variant="outline" disabled={!draft || busy} onclick={cancel}>{message("common.cancel")}</Button>
      <Button variant="outline" disabled={!draft || busy || testing} onclick={() => { dockTab = "debug"; dockOpen = true; }}><Play class="size-4" />{message("extensions.test")}</Button>
      <Button variant="outline" disabled={!draft || busy || testing || !draft.etag} onclick={() => void exportArchive()} aria-label={message("extensions.import.export")}><Download class="size-4" /></Button>
      <Button disabled={busy || testing || !draft?.manifest.id.trim() || !dirty} onclick={save}><Save class="size-4" />{message("extensions.save")}</Button>
    </div>
  </header>
  {#if error}<p role="alert" class="shrink-0 border-b border-destructive/40 bg-destructive/10 px-3 py-1.5 text-xs text-destructive">{error}</p>{/if}

  {#if draft}
  <div class="flex items-center justify-end gap-1 border-b border-border px-3 py-1 lg:hidden"><Button variant="ghost" size="sm" aria-pressed={showFiles} onclick={() => showFiles = !showFiles}><FolderTree class="size-4" />{message("extensions.files.tree")}</Button><Button variant="ghost" size="sm" aria-pressed={showSettings} onclick={() => showSettings = !showSettings}><Settings2 class="size-4" />{message("extensions.settings.tab")}</Button></div>
  <div class="editor-body flex min-h-0 flex-1 flex-col lg:flex-row">
    <div class="flex min-h-[480px] min-w-0 flex-1 flex-col">
      <div class="flex min-h-0 flex-1" class:hidden={dockExpanded} inert={busy}>
        {#if showFiles}<aside class="w-36 shrink-0 overflow-hidden border-r border-border xl:w-44" aria-label={message("extensions.files.tree")}><ExtensionFileTree paths={filePaths} directories={draft.directories ?? []} {activePath} onOpen={openFile} onCreate={createEntry} onDelete={requestDelete} onMove={movePath} onDuplicate={duplicatePath} onExternalFiles={importExternalFiles} onRename={renamePath} /></aside>{/if}
        <div class="flex min-h-0 min-w-0 flex-1 flex-col">
          <div class="flex h-9 shrink-0 items-stretch overflow-x-auto border-b border-border bg-muted/30 text-xs" role="tablist" aria-label={message("extensions.files.tree")}>{#each openFiles as path}<div class="group flex shrink-0 items-center border-r border-border border-t-2 border-t-transparent" class:!border-t-primary={path === activePath} class:bg-background={path === activePath} role="presentation" oncontextmenu={(event) => { event.preventDefault(); tabMenu = { path, x: event.clientX, y: event.clientY }; }}><button class="flex h-full items-center gap-2 px-3" role="tab" aria-selected={path === activePath} title={path} onclick={() => openFile(path)}>{#if fileKind(path) === "javascript"}<FileCode2 class="size-3.5 text-primary" />{:else if fileKind(path) === "json"}<FileJson class="size-3.5 text-muted-foreground" />{:else}<FileText class="size-3.5 text-muted-foreground" />{/if}<span>{fileLabel(path)}</span>{#if draft.files[path] !== savedFiles[path]}<span class="size-1.5 rounded-full bg-primary" aria-label={message("extensions.unsaved")}></span>{/if}</button><button class="mr-1 rounded p-1 text-muted-foreground hover:bg-accent hover:text-foreground" aria-label={message("extensions.dev.closeFile", { path: fileLabel(path) })} onclick={() => closeFile(path)}><X class="size-3" /></button></div>{/each}</div>
          <div class="min-h-0 flex-1 overflow-hidden">{#if activePath}{#key activePath}<ExtensionMonacoEditor value={draft.files?.[activePath] ?? ""} path={activePath} ariaLabel={message("extensions.script")} settingKeys={activePath === EXTENSION_ENTRY_POINT ? settingKeys : []} {diagnostics} {revealLocation} onChange={updateSource} />{/key}{:else}<div class="flex h-full items-center justify-center p-5 text-sm text-muted-foreground">{message("extensions.dev.selectFile")}</div>{/if}</div>
        </div>
      </div>
      <section class="dev-dock flex shrink-0 flex-col border-t border-border" class:dock-open={dockOpen} class:dock-expanded={dockExpanded}>
        <div class="flex h-10 shrink-0 items-center gap-1 border-b border-border bg-muted/20 px-2">
          {#each ["problems", "logs", "debug", "benchmark"] as view}<button class="h-full border-b-2 border-transparent px-3 text-xs text-muted-foreground hover:text-foreground disabled:opacity-50" class:!border-primary={dockTab === view && dockOpen} class:!text-foreground={dockTab === view && dockOpen} disabled={testing && dockTab !== view} aria-pressed={dockTab === view && dockOpen} onclick={() => { dockTab = view as typeof dockTab; dockOpen = true; }}>{message(view === "logs" ? "extensions.logs.title" : `extensions.dev.${view}`)}{#if view === "problems"}<span class="ml-1 rounded bg-muted px-1">{diagnostics.length}</span>{/if}</button>{/each}
          <div class="ml-auto flex gap-1"><Button variant="ghost" size="icon-xs" aria-label={message(dockExpanded ? "extensions.dev.restore" : "extensions.dev.expand")} onclick={() => { dockExpanded = !dockExpanded; dockOpen = true; }}>{#if dockExpanded}<Minimize2 class="size-3.5" />{:else}<Maximize2 class="size-3.5" />{/if}</Button><Button variant="ghost" size="icon-xs" aria-label={message("extensions.layout.toggleDock")} onclick={() => { dockOpen = !dockOpen; if (!dockOpen) dockExpanded = false; }}>{#if dockOpen}<ChevronDown class="size-4" />{:else}<ChevronUp class="size-4" />{/if}</Button></div>
        </div>
        <div class="min-h-0 flex-1 overflow-y-auto p-3" class:hidden={!dockOpen || dockTab !== "problems"}><div class="mb-3 flex items-center justify-between gap-2"><span class="text-xs text-muted-foreground">{message("extensions.dev.autoCheck")} · {checking ? message("extensions.checking") : checkError ? message("extensions.dev.checkFailed") : diagnostics.length ? message("extensions.diagnosticsCount", { count: diagnostics.length }) : message("extensions.noDiagnostics")}</span>{#if checkError}<Button variant="outline" size="sm" disabled={checking} onclick={() => void runCheck()}>{message("extensions.dev.retry")}</Button>{/if}</div>{#if checkError}<p role="alert" class="text-xs text-destructive">{checkError}</p>{/if}<ul class="space-y-1">{#each diagnostics as item}<li><button class="w-full rounded-md bg-muted/40 p-2 text-left text-xs hover:bg-muted" onclick={() => { openFile(item.path || EXTENSION_ENTRY_POINT); revealLocation = { line: item.line, column: item.column }; dockExpanded = false; }}><span class="font-mono text-primary">{item.path}:{item.line}:{item.column}</span> {item.message}</button></li>{/each}</ul></div>
        <div class="min-h-0 flex-1 flex flex-col" class:hidden={!dockOpen || dockTab !== "logs"}><ExtensionConsolePanel id={draft.manifest.id} compact={!dockExpanded} /></div>
        <div class="flex min-h-0 flex-1 flex-col" class:hidden={!dockOpen || (dockTab !== "debug" && dockTab !== "benchmark")}><ExtensionWorkbench preferredEndpoints={draft.manifest.match.endpoints} compact={!dockExpanded} id={draft.manifest.id} draftFiles={draft.files} mode={dockTab === "benchmark" ? "benchmark" : "debug"} onRunning={(value) => testing = value} /></div>
      </section>
    </div>
    {#if showSettings}<aside class="flex w-full shrink-0 flex-col border-t border-border lg:w-72 lg:border-t-0 lg:border-l" aria-label={message("extensions.settings.tab")}><div class="flex h-9 shrink-0 items-center border-b border-border bg-muted/20 px-4 text-xs font-medium">{message("extensions.dev.config")}</div><div class="min-h-0 flex-1 lg:overflow-y-auto"><fieldset disabled={busy || testing}><ExtensionConfiguration bind:draft errors={settingsErrors} /></fieldset></div></aside>{/if}
  </div>
  {:else}<p role="status" class="p-10 text-center text-sm text-muted-foreground">{busy ? message("common.loading") : message("extensions.workspace.loadFailed")}</p>{/if}

  <ConfirmDialog
    bind:open={deleteOpen}
    title={message(deleteSelection.length > 1 ? "extensions.files.deleteBatch" : "extensions.delete")}
    message={deleteSelection.length > 1 ? message("extensions.files.deleteBatchConfirm", { count: deleteSelection.length }) : message("extensions.deleteConfirm", { id: fileLabel(deleteSelection[0] ?? "") })}
    confirmLabel={message("extensions.delete")}
    onConfirm={confirmDelete}
  />
</div>

{#if tabMenu}
  <!-- svelte-ignore a11y_no_static_element_interactions -->
  <div
    class="fixed inset-0 z-40"
    role="presentation"
    onclick={() => tabMenu = null}
    oncontextmenu={(event) => { event.preventDefault(); tabMenu = null; }}
  ></div>
  <div class="fixed z-50 min-w-52 rounded-md border border-border bg-popover py-1 text-xs shadow-lg" style="left: {tabMenu.x}px; top: {tabMenu.y}px" role="menu">
    {#if tabMenu}
      {@const path = tabMenu.path}
      <button class="flex w-full items-center justify-between px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { closeTabs([path]); tabMenu = null; }}>
        {$translate("extensions.tabs.close")}<span class="ml-6 text-muted-foreground">⌘ W</span>
      </button>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { closeTabs(openFiles.filter((open) => open !== path)); tabMenu = null; }}>
        {$translate("extensions.tabs.closeOthers")}
      </button>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { closeTabs(openFiles.slice(openFiles.indexOf(path) + 1)); tabMenu = null; }}>
        {$translate("extensions.tabs.closeRight")}
      </button>
      <div class="my-1 border-t border-border"></div>
      <button class="flex w-full items-center justify-between px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { void navigator.clipboard?.writeText(path); tabMenu = null; }}>
        {$translate("extensions.tabs.copyPath")}<span class="ml-6 text-muted-foreground">⌥⌘ C</span>
      </button>
      <div class="my-1 border-t border-border"></div>
      <button class="block w-full px-3 py-1.5 text-left hover:bg-muted" role="menuitem" onclick={() => { closeTabs([...openFiles]); tabMenu = null; }}>
        {$translate("extensions.tabs.closeAll")}
      </button>
    {/if}
  </div>
{/if}

<ConfirmDialog bind:open={leaveOpen} title={message("extensions.unsaved")} message={message("extensions.workspace.leave")} confirmLabel={message("extensions.workspace.discard")} onConfirm={() => { leaveOpen = false; allowNavigation = true; clearAutoSave(); window.location.hash = pendingHref; }} />
<style>
  .dev-dock { height: 40px; }
  .dev-dock.dock-open { height: clamp(260px, 34vh, 360px); }
  .dev-dock.dock-expanded { height: auto; flex: 1; }
  @media (max-width: 1023px) { .editor-body { overflow-y: auto; } }
</style>
