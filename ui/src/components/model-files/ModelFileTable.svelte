<script lang="ts">
  import { Check, ChevronDown, ChevronRight, Copy, Download, FileBox, FolderOpen, Trash2 } from "@lucide/svelte";
  import { Badge } from "$lib/components/ui/badge/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Table from "$lib/components/ui/table/index.js";
  import { formatAbsoluteTime, formatCapacity } from "$lib/format";
  import { locale, translate } from "$lib/i18n";
  import { groupModelFiles, modelFileDisplayPath, type ModelFileProject } from "$lib/modelFiles";
  import type { ModelDownload, ModelFile } from "$lib/types";

  interface Props {
    files: ModelFile[];
    downloads?: ModelDownload[];
    sourceNames: Record<string, string>;
    filterActive?: boolean;
    copiedId: string;
    deletingId: string;
    onCopy: (file: ModelFile) => void;
    onDelete: (file: ModelFile) => void;
  }

  let { files, downloads = [], sourceNames, filterActive = false, copiedId, deletingId, onCopy, onDelete }: Props = $props();
  let projects = $derived(groupModelFiles(files, sourceNames, downloads));
  let expandedProjects = $state<string[]>([]);

  function isProjectExpanded(key: string): boolean {
    return filterActive || expandedProjects.includes(key);
  }

  function toggleProject(key: string): void {
    expandedProjects = expandedProjects.includes(key)
      ? expandedProjects.filter((current) => current !== key)
      : [...expandedProjects, key];
  }

  function projectToggleLabel(project: ModelFileProject): string {
    return $translate(isProjectExpanded(project.key) ? "modelFiles.collapseProject" : "modelFiles.expandProject", { name: project.name });
  }

  function progress(task: ModelDownload): number {
    if (task.total_bytes > 0) return Math.min(100, Math.floor((task.downloaded_bytes / task.total_bytes) * 100));
    if (task.total_files > 0) return Math.min(100, Math.floor((task.completed_files / task.total_files) * 100));
    return 0;
  }

  function registered(file: ModelFile): string[] {
    return file.registered_models ?? [];
  }

  function inUse(file: ModelFile): string[] {
    return file.in_use_models ?? [];
  }

  function isStandaloneModel(project: ModelFileProject): boolean {
    return project.presentation === "file" && project.files.length === 1 && !project.download;
  }

</script>

{#snippet fileStatus(file: ModelFile)}
  {@const fileRegistered = registered(file)}
  {@const fileInUse = inUse(file)}
  {#if fileInUse.length > 0}
    <div class="flex flex-wrap items-center gap-1">
      <Badge variant="destructive">{$translate("modelFiles.inUse")}</Badge>
      <span class="text-muted-foreground truncate text-xs" title={fileInUse.join(", ")}>{fileInUse.join(", ")}</span>
    </div>
  {:else if fileRegistered.length > 0}
    <div class="flex flex-wrap items-center gap-1">
      <Badge variant="secondary">{$translate("modelFiles.registered")}</Badge>
      <span class="text-muted-foreground truncate text-xs" title={fileRegistered.join(", ")}>{fileRegistered.join(", ")}</span>
    </div>
  {:else}
    <span class="text-muted-foreground text-xs">{$translate("modelFiles.available")}</span>
  {/if}
{/snippet}

{#snippet fileActions(file: ModelFile)}
  {@const fileRegistered = registered(file)}
  {@const fileInUse = inUse(file)}
  <div class="flex justify-end gap-1">
    <Button
      variant="ghost"
      size="icon-sm"
      title={copiedId === file.id ? $translate("modelFiles.pathCopied") : $translate("modelFiles.copyPath")}
      aria-label={copiedId === file.id ? $translate("modelFiles.pathCopied") : $translate("modelFiles.copyPath")}
      onclick={() => onCopy(file)}
    >
      {#if copiedId === file.id}<Check class="text-success" />{:else}<Copy />{/if}
    </Button>
    <Button
      variant="destructive"
      size="icon-sm"
      title={fileInUse.length > 0
        ? $translate("modelFiles.deleteBlockedInUse")
        : fileRegistered.length > 0
          ? $translate("modelFiles.deleteBlockedRegistered")
          : $translate("modelFiles.delete")}
      aria-label={$translate("modelFiles.delete")}
      disabled={fileInUse.length > 0 || fileRegistered.length > 0 || deletingId !== ""}
      onclick={() => onDelete(file)}
    >
      <Trash2 />
    </Button>
  </div>
{/snippet}

<Table.Root class="min-w-[820px]">
  <Table.Header>
    <Table.Row>
      <Table.Head>{$translate("modelFiles.file")}</Table.Head>
      <Table.Head>{$translate("modelFiles.format")}</Table.Head>
      <Table.Head class="text-right">{$translate("modelFiles.size")}</Table.Head>
      <Table.Head>{$translate("modelFiles.modified")}</Table.Head>
      <Table.Head>{$translate("modelFiles.status")}</Table.Head>
      <Table.Head class="text-right">{$translate("modelFiles.actions")}</Table.Head>
    </Table.Row>
  </Table.Header>
  <Table.Body>
    {#each projects as project (project.key)}
      {@const taskProgress = project.download ? progress(project.download) : 0}
      {#if isStandaloneModel(project)}
        {@const file = project.files[0]}
        <Table.Row class="align-top">
          <Table.Cell class="max-w-[420px]">
            <div class="flex min-w-0 items-start gap-2">
              <FileBox class="text-muted-foreground mt-0.5 size-4 shrink-0" aria-hidden="true" />
              <div class="min-w-0">
                <div class="truncate font-medium" title={file.name}>{project.name}</div>
                <p class="text-muted-foreground mt-0.5 truncate text-xs">{project.sourceName}</p>
              </div>
            </div>
          </Table.Cell>
          <Table.Cell><Badge variant="outline">{file.format.toUpperCase()}</Badge></Table.Cell>
          <Table.Cell class="text-right tabular-nums">{formatCapacity(file.size, "—")}</Table.Cell>
          <Table.Cell class="whitespace-nowrap text-xs">{formatAbsoluteTime(file.modified_at, $locale)}</Table.Cell>
          <Table.Cell class="max-w-[220px]">{@render fileStatus(file)}</Table.Cell>
          <Table.Cell>{@render fileActions(file)}</Table.Cell>
        </Table.Row>
      {:else}
        <Table.Row class="bg-muted/35 hover:bg-muted/35">
          <Table.Cell colspan={6} class="py-2.5">
            <div class="flex min-w-0 flex-wrap items-center justify-between gap-x-4 gap-y-2">
              <div class="flex min-w-0 items-center gap-2">
                {#if filterActive}
                  <span class="flex size-7 items-center justify-center" aria-hidden="true"><ChevronDown /></span>
                {:else}
                  <Button
                    variant="ghost"
                    size="icon-sm"
                    class="size-7"
                    aria-expanded={isProjectExpanded(project.key)}
                    aria-label={projectToggleLabel(project)}
                    title={projectToggleLabel(project)}
                    onclick={() => toggleProject(project.key)}
                  >
                    {#if isProjectExpanded(project.key)}
                      <ChevronDown aria-hidden="true" />
                    {:else}
                      <ChevronRight aria-hidden="true" />
                    {/if}
                  </Button>
                {/if}
                <FolderOpen class="text-muted-foreground size-4 shrink-0" aria-hidden="true" />
                <div class="min-w-0">
                  <div class="flex min-w-0 flex-wrap items-center gap-2">
                    <span class="truncate font-medium" title={project.name}>{project.name}</span>
                    {#if project.revision}
                      <Badge variant="outline" title={project.revision}>{project.revision.slice(0, 12)}</Badge>
                    {/if}
                  </div>
                  <p class="text-muted-foreground mt-0.5 text-xs">{project.sourceName}</p>
                </div>
              </div>
              <div class="text-muted-foreground flex shrink-0 items-center gap-3 text-xs tabular-nums">
                {#if project.download}
                  <span>{$translate("modelFiles.files", { count: project.download.total_files || project.files.length })}</span>
                  <span>{formatCapacity(project.download.total_bytes || project.totalSize, "—")}</span>
                {:else}
                  <span>{$translate("modelFiles.files", { count: project.files.length })}</span>
                  <span>{formatCapacity(project.totalSize, "—")}</span>
                {/if}
              </div>
            </div>
          </Table.Cell>
        </Table.Row>
        {#if isProjectExpanded(project.key)}
        {#if project.download}
          <Table.Row class="align-top">
            <Table.Cell class="max-w-[420px] pl-8">
              <div class="flex min-w-0 items-start gap-2">
                <Download class="text-primary mt-0.5 size-4 shrink-0" aria-hidden="true" />
                <div class="min-w-0">
                  <div class="truncate font-medium" title={project.download.current_file}>
                    {project.download.current_file || $translate("modelFiles.download.preparing")}
                  </div>
                  <code class="text-muted-foreground block truncate text-xs">{project.download.repo_id} · {project.download.revision}</code>
                </div>
              </div>
            </Table.Cell>
            <Table.Cell>
              <Badge variant="outline">{project.download.provider === "modelscope" ? "MS" : "HF"}</Badge>
            </Table.Cell>
            <Table.Cell class="text-right text-xs tabular-nums">
              <span class="block">{formatCapacity(project.download.downloaded_bytes, "—")}</span>
              <span class="text-muted-foreground block">/ {formatCapacity(project.download.total_bytes, "—")}</span>
            </Table.Cell>
            <Table.Cell class="whitespace-nowrap text-xs text-muted-foreground">
              {project.download.completed_files}/{project.download.total_files} {$translate("modelFiles.download.files")}
            </Table.Cell>
            <Table.Cell class="min-w-48 max-w-[240px]">
              <div class="flex items-center gap-2">
                <Badge variant="secondary">{$translate(`modelFiles.download.${project.download.status}`)}</Badge>
                <span class="text-muted-foreground text-xs tabular-nums">{taskProgress}%</span>
              </div>
              <div class="bg-muted mt-2 h-1.5 overflow-hidden rounded-full" role="progressbar" aria-valuemin="0" aria-valuemax="100" aria-valuenow={taskProgress} aria-label={`${project.name} ${taskProgress}%`}>
                <div class="bg-primary h-full transition-[width]" style={`width: ${taskProgress}%`}></div>
              </div>
            </Table.Cell>
            <Table.Cell></Table.Cell>
          </Table.Row>
        {/if}
        {#each project.files as file (file.id)}
          <Table.Row class="align-top">
            <Table.Cell class="max-w-[420px] pl-8">
              <div class="flex min-w-0 items-start gap-2">
                <FileBox class="text-muted-foreground mt-0.5 size-4 shrink-0" aria-hidden="true" />
                <div class="min-w-0">
                  <div class="truncate font-medium" title={file.name}>{file.name}</div>
                  <code class="text-muted-foreground block truncate text-xs" title={file.path}>{modelFileDisplayPath(file)}</code>
                </div>
              </div>
            </Table.Cell>
            <Table.Cell><Badge variant="outline">{file.format.toUpperCase()}</Badge></Table.Cell>
            <Table.Cell class="text-right tabular-nums">{formatCapacity(file.size, "—")}</Table.Cell>
            <Table.Cell class="whitespace-nowrap text-xs">{formatAbsoluteTime(file.modified_at, $locale)}</Table.Cell>
            <Table.Cell class="max-w-[220px]">
              {@render fileStatus(file)}
            </Table.Cell>
            <Table.Cell>
              {@render fileActions(file)}
            </Table.Cell>
          </Table.Row>
        {/each}
        {/if}
      {/if}
    {/each}
  </Table.Body>
</Table.Root>
