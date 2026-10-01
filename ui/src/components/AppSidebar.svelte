<script lang="ts">
  import { link } from "svelte-spa-router";
  import { FerrisWheel, Boxes, Activity, ScrollText, Gauge, Cpu, Sun, Moon, Monitor, ChevronRight, Settings, Server, KeyRound, FolderOpen, CircleAlert, Puzzle, BarChart3 } from "@lucide/svelte";
  import * as Sidebar from "$lib/components/ui/sidebar/index.js";
  import * as Collapsible from "$lib/components/ui/collapsible/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { toggleTheme, themeMode, appTitle } from "../stores/theme";
  import { currentRoute } from "../stores/route";
  import { playgroundActivity } from "../stores/playgroundActivity";
  import { performanceEnabled, models } from "../stores/api";
  import { showUnlistedModels } from "../stores/modelDisplay";
  import { modelsMenuOpen } from "../stores/sidebar";
  import type { Model } from "../lib/types";
  import { isComposingKey } from "../lib/ime";
  import { translate } from "../lib/i18n";
  import ConnectionStatus from "./ConnectionStatus.svelte";

  function handleTitleChange(newTitle: string): void {
    const sanitized = newTitle.replace(/\n/g, "").trim().substring(0, 64) || "llama-swap";
    appTitle.set(sanitized);
  }

  function handleKeyDown(e: KeyboardEvent): void {
    if (e.key === "Enter" && !isComposingKey(e)) {
      e.preventDefault();
      const target = e.currentTarget as HTMLElement;
      handleTitleChange(target.textContent || "(set title)");
      target.blur();
    }
  }

  function handleBlur(e: FocusEvent): void {
    const target = e.currentTarget as HTMLElement;
    handleTitleChange(target.textContent || "(set title)");
  }

  function isActive(path: string, current: string): boolean {
    return path === "/" ? current === "/" : current.startsWith(path);
  }

  let visibleLocalModels = $derived(
    $models.filter((model) => !model.peerID && ($showUnlistedModels || !model.unlisted)),
  );
  let visiblePeerModels = $derived(
    $models.filter((model) => model.peerID && ($showUnlistedModels || !model.unlisted)),
  );

  type DotColor = "grey" | "yellow" | "green" | "blue";
  function statusDotColor(model: Model): DotColor {
    if (model.state === "ready") return "green";
    if (model.state === "sleeping") return "blue";
    if (model.state === "starting" || model.state === "stopping") return "yellow";
    return "grey";
  }

  function configChanged(model: Model): boolean {
    return model.configStatus === "modified" || model.configStatus === "apply_failed";
  }

  const dotClass: Record<DotColor, string> = {
    grey: "bg-muted-foreground/40",
    yellow: "bg-warning",
    green: "bg-success",
    blue: "bg-info",
  };
</script>

{#snippet modelMenuItem(model: Model)}
  <Sidebar.MenuSubItem>
    <Sidebar.MenuSubButton
      isActive={$currentRoute === `/models/${encodeURIComponent(model.id)}`}
    >
      {#snippet child({ props })}
        <a href="/models/{encodeURIComponent(model.id)}" use:link {...props}>
          <span class={`size-2 shrink-0 rounded-full ${dotClass[statusDotColor(model)]}`}></span>
          <span class="flex-1 truncate">{model.id}</span>
          {#if configChanged(model)}
            <span
              class="text-warning flex size-4 shrink-0 items-center justify-center"
              title={$translate("models.configModified")}
              role="img"
              aria-label={$translate("models.configModified")}
            >
              <CircleAlert class="size-3.5" aria-hidden="true" />
            </span>
          {/if}
        </a>
      {/snippet}
    </Sidebar.MenuSubButton>
  </Sidebar.MenuSubItem>
{/snippet}

<Sidebar.Root collapsible="icon">
  <Sidebar.Header>
    <div class="flex items-center gap-2 px-2 py-1.5">
      <div class="flex shrink-0 items-center justify-center">
        <ConnectionStatus />
      </div>
      <h1
        contenteditable="true"
        class="truncate pb-0 text-base font-semibold outline-none rounded-md px-1 hover:bg-sidebar-accent group-data-[collapsible=icon]:hidden"
        onblur={handleBlur}
        onkeydown={handleKeyDown}
      >
        {$appTitle}
      </h1>
    </div>
  </Sidebar.Header>

  <Sidebar.Content>
    <Sidebar.Group class="py-1">
      <Sidebar.GroupLabel class="h-6 px-2 text-[11px]">{$translate("navigation.groups.workspace")}</Sidebar.GroupLabel>
      <Sidebar.GroupContent>
        <Sidebar.Menu class="gap-0.5">
          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={$currentRoute === "/" || isActive("/activity", $currentRoute)} tooltipContent={$translate("navigation.activity")}>
              {#snippet child({ props })}
                <a href="/" use:link {...props}>
                  <Activity />
                  <span>{$translate("navigation.activity")}</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>

          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/playground", $currentRoute)} tooltipContent={$translate("navigation.playground")}>
              {#snippet child({ props })}
                <a href="/playground" use:link {...props}>
                  <FerrisWheel />
                  <span class={$playgroundActivity ? "activity-link" : ""}>{$translate("navigation.playground")}</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>
        </Sidebar.Menu>
      </Sidebar.GroupContent>
    </Sidebar.Group>

    <Sidebar.Group class="py-1">
      <Sidebar.GroupLabel class="h-6 px-2 text-[11px]">{$translate("navigation.groups.models")}</Sidebar.GroupLabel>
      <Sidebar.GroupContent>
        <Sidebar.Menu class="gap-0.5">
          <Sidebar.MenuItem>
            <Collapsible.Root
              open={$modelsMenuOpen}
              onOpenChange={(v) => modelsMenuOpen.set(v)}
              class="gap-0"
            >
              <Sidebar.MenuButton
                isActive={$currentRoute.startsWith("/models")}
                tooltipContent={$translate("navigation.models")}
              >
                {#snippet child({ props })}
                  <a href="/models" use:link {...props}>
                    <Boxes />
                    <span>{$translate("navigation.models")}</span>
                    <span
                      class="ml-auto transition-transform duration-200 {$modelsMenuOpen ? 'rotate-90' : ''}"
                      role="button"
                      tabindex="0"
                      aria-label={$translate("models.toggleSection")}
                      onclick={(e) => {
                        e.preventDefault();
                        e.stopPropagation();
                        modelsMenuOpen.update((v) => !v);
                      }}
                      onkeydown={(e) => {
                        if (e.key === 'Enter' || e.key === ' ') {
                          e.preventDefault();
                          e.stopPropagation();
                          modelsMenuOpen.update((v) => !v);
                        }
                      }}
                    >
                      <ChevronRight />
                    </span>
                  </a>
                {/snippet}
              </Sidebar.MenuButton>
              <Collapsible.Content>
                <Sidebar.MenuSub>
                  {#each visibleLocalModels as model (model.id)}
                    {@render modelMenuItem(model)}
                  {/each}
                  {#if visiblePeerModels.length > 0}
                    <li class="text-sidebar-foreground/70 px-2 pt-2 pb-1 text-xs font-medium">
                      {$translate("models.peerModels")}
                    </li>
                    {#each visiblePeerModels as model (model.id)}
                      {@render modelMenuItem(model)}
                    {/each}
                  {/if}
                </Sidebar.MenuSub>
              </Collapsible.Content>
            </Collapsible.Root>
          </Sidebar.MenuItem>

          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/model-files", $currentRoute)} tooltipContent={$translate("navigation.modelFiles")}>
              {#snippet child({ props })}
                <a href="/model-files" use:link {...props}><FolderOpen /><span>{$translate("navigation.modelFiles")}</span></a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>

          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/runtimes", $currentRoute)} tooltipContent={$translate("navigation.runtimes")}>
              {#snippet child({ props })}
                <a href="/runtimes" use:link {...props}><Server /><span>{$translate("navigation.runtimes")}</span></a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>
        </Sidebar.Menu>
      </Sidebar.GroupContent>
    </Sidebar.Group>

    <Sidebar.Group class="py-1">
      <Sidebar.GroupLabel class="h-6 px-2 text-[11px]">{$translate("navigation.groups.observe")}</Sidebar.GroupLabel>
      <Sidebar.GroupContent>
        <Sidebar.Menu class="gap-0.5">
          {#if $performanceEnabled}
            <Sidebar.MenuItem>
              <Sidebar.MenuButton isActive={isActive("/performance", $currentRoute)} tooltipContent={$translate("navigation.performance")}>
                {#snippet child({ props })}
                  <a href="/performance" use:link {...props}>
                    <Gauge />
                    <span>{$translate("navigation.performance")}</span>
                  </a>
                {/snippet}
              </Sidebar.MenuButton>
            </Sidebar.MenuItem>
          {/if}

          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/hardware", $currentRoute)} tooltipContent={$translate("navigation.hardware")}>
              {#snippet child({ props })}
                <a href="/hardware" use:link {...props}>
                  <Cpu />
                  <span>{$translate("navigation.hardware")}</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>

          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/logs", $currentRoute)} tooltipContent={$translate("navigation.logs")}>
              {#snippet child({ props })}
                <a href="/logs" use:link {...props}>
                  <ScrollText />
                  <span>{$translate("navigation.logs")}</span>
                </a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>
        </Sidebar.Menu>
      </Sidebar.GroupContent>
    </Sidebar.Group>

    <Sidebar.Group class="py-1">
      <Sidebar.GroupLabel class="h-6 px-2 text-[11px]">{$translate("navigation.groups.access")}</Sidebar.GroupLabel>
      <Sidebar.GroupContent>
        <Sidebar.Menu class="gap-0.5">
          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/extensions", $currentRoute)} tooltipContent={$translate("navigation.extensions")}>
              {#snippet child({ props })}
                <a href="/extensions" use:link {...props}><Puzzle /><span>{$translate("navigation.extensions")}</span></a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>
          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/keys", $currentRoute)} tooltipContent={$translate("navigation.apiKeys")}>
              {#snippet child({ props })}
                <a href="/keys" use:link {...props}><KeyRound /><span>{$translate("navigation.apiKeys")}</span></a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>
          <Sidebar.MenuItem>
            <Sidebar.MenuButton isActive={isActive("/usage", $currentRoute)} tooltipContent={$translate("navigation.usageRecords")}>
              {#snippet child({ props })}
                <a href="/usage" use:link {...props}><BarChart3 /><span>{$translate("navigation.usageRecords")}</span></a>
              {/snippet}
            </Sidebar.MenuButton>
          </Sidebar.MenuItem>
        </Sidebar.Menu>
      </Sidebar.GroupContent>
    </Sidebar.Group>
  </Sidebar.Content>

  <Sidebar.Footer>
    <div
      class="flex items-center justify-between gap-2 px-1 group-data-[collapsible=icon]:flex-col-reverse"
    >
      <Sidebar.MenuButton
        isActive={isActive("/settings", $currentRoute)}
        tooltipContent={$translate("navigation.settings")}
      >
        {#snippet child({ props })}
          <a href="/settings" use:link {...props}>
            <Settings />
            <span>{$translate("navigation.settings")}</span>
          </a>
        {/snippet}
      </Sidebar.MenuButton>
      <Button
        variant="ghost"
        size="icon"
        onclick={toggleTheme}
        title={$translate("common.toggleThemeCurrent", { mode: $translate(`settings.${$themeMode}`) })}
      >
        {#if $themeMode === "system"}
          <Monitor />
        {:else if $themeMode === "light"}
          <Sun />
        {:else}
          <Moon />
        {/if}
        <span class="sr-only">{$translate("common.toggleTheme")}</span>
      </Button>
    </div>
  </Sidebar.Footer>
  <Sidebar.Rail />
</Sidebar.Root>
