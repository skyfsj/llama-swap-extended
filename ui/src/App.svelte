<script lang="ts">
  import { onMount } from "svelte";
  import type { Component, ComponentType, SvelteComponent } from "svelte";
  import Router from "svelte-spa-router";
  import { wrap } from "svelte-spa-router/wrap";
  import AppSidebar from "./components/AppSidebar.svelte";
  import ApiAuth from "./components/ApiAuth.svelte";
  import ConfirmDialog from "./components/ConfirmDialog.svelte";
  import ToastHost from "./components/ToastHost.svelte";
  import CCSwitchActions from "./components/CCSwitchActions.svelte";
  import AuthScreen from "./components/AuthScreen.svelte";
  import RouteLoadingImpl from "./components/RouteLoading.svelte";
  import PlaygroundStub from "./routes/PlaygroundStub.svelte";
  import * as Sidebar from "$lib/components/ui/sidebar/index.js";
  import * as Tooltip from "$lib/components/ui/tooltip/index.js";
  import { Separator } from "$lib/components/ui/separator/index.js";
  import * as Select from "$lib/components/ui/select/index.js";
  import {
    activeProfile,
    checkPerformanceEnabled,
    enableAPIEvents,
    profiles,
    setActiveProfile,
  } from "./stores/api";
  import { initScreenWidth, initSystemThemeListener, isDarkMode, themeName, appTitle, connectionState } from "./stores/theme";
  import { currentRoute } from "./stores/route";
  import { selectedPlaygroundTab } from "./stores/playground";
  import { authSession, refreshAuthSession } from "./stores/auth";
  import { locale, translate } from "./lib/i18n";
  import { settingsDirty } from "./stores/settingsGuard";
  import { isInternalRoute, isSameSettingsTarget } from "./lib/navGuard";

  // svelte-spa-router's types predate Svelte 5 (loadingComponent wants the
  // old class-component ComponentType); the cast is safe since Router.svelte
  // just instantiates whatever component object it's given.
  const RouteLoading = RouteLoadingImpl as unknown as ComponentType<SvelteComponent>;

  // Routes are lazy-loaded so their (and their dependencies') code isn't part
  // of the initial bundle; each becomes its own chunk fetched on first visit.
  // loadingComponent covers the (usually brief) fetch with a themed
  // placeholder instead of a blank/white flash.
  const routes = {
    "/": wrap({ asyncComponent: () => import("./routes/Activity.svelte"), loadingComponent: RouteLoading }),
    "/playground": PlaygroundStub,
    "/models": wrap({ asyncComponent: () => import("./routes/ModelsDash.svelte"), loadingComponent: RouteLoading }),
    "/models/:id": wrap({ asyncComponent: () => import("./routes/ModelDetail.svelte"), loadingComponent: RouteLoading }),
    "/model-files": wrap({ asyncComponent: () => import("./routes/ModelFiles.svelte"), loadingComponent: RouteLoading }),
    "/logs": wrap({ asyncComponent: () => import("./routes/LogViewer.svelte"), loadingComponent: RouteLoading }),
    "/activity": wrap({ asyncComponent: () => import("./routes/Activity.svelte"), loadingComponent: RouteLoading }),
    "/settings": wrap({ asyncComponent: () => import("./routes/Settings.svelte"), loadingComponent: RouteLoading }),
    "/performance": wrap({ asyncComponent: () => import("./routes/Performance.svelte"), loadingComponent: RouteLoading }),
    "/hardware": wrap({ asyncComponent: () => import("./routes/Hardware.svelte"), loadingComponent: RouteLoading }),
    "/runtimes": wrap({ asyncComponent: () => import("./routes/Runtimes.svelte"), loadingComponent: RouteLoading }),
    "/backends": wrap({ asyncComponent: () => import("./routes/Backends.svelte"), loadingComponent: RouteLoading }),
    "/keys": wrap({ asyncComponent: () => import("./routes/ApiKeys.svelte"), loadingComponent: RouteLoading }),
    "/usage": wrap({ asyncComponent: () => import("./routes/UsageRecords.svelte"), loadingComponent: RouteLoading }),
    "/extensions": wrap({ asyncComponent: () => import("./routes/Extensions.svelte"), loadingComponent: RouteLoading }),
    "/extensions/docs": wrap({ asyncComponent: () => import("./routes/ExtensionDocs.svelte"), loadingComponent: RouteLoading }),
    "/extensions/new": wrap({ asyncComponent: () => import("./routes/ExtensionEditor.svelte"), loadingComponent: RouteLoading }),
    "/extensions/:id/editor": wrap({ asyncComponent: () => import("./routes/ExtensionEditor.svelte"), loadingComponent: RouteLoading }),
    "/server-config": wrap({ asyncComponent: () => import("./routes/ServerConfig.svelte"), loadingComponent: RouteLoading }),
    "*": wrap({ asyncComponent: () => import("./routes/Activity.svelte"), loadingComponent: RouteLoading }),
  };

  const routeTitles: Record<string, string> = {
    "/": "navigation.activity",
    "/playground": "navigation.playground",
    "/models": "navigation.models",
    "/model-files": "navigation.modelFiles",
    "/activity": "navigation.activity",
    "/logs": "navigation.logs",
    "/settings": "navigation.settings",
    "/performance": "navigation.performance",
    "/hardware": "navigation.hardware",
    "/runtimes": "navigation.runtimes",
    "/backends": "navigation.backends",
    "/keys": "navigation.apiKeys",
    "/usage": "navigation.usageRecords",
    "/extensions": "navigation.extensions",
    "/server-config": "navigation.serverConfig",
  };

  let sectionTitle = $derived.by(() => {
    if ($currentRoute === "/playground") {
      return `${$translate("navigation.playground")} / ${$translate(`playground.tabs.${$selectedPlaygroundTab}`)}`;
    }
    if ($currentRoute === "/extensions/docs") return $translate("extensions.dev.docsTitle");
    if ($currentRoute.startsWith("/extensions/")) {
      return `${$translate("navigation.extensions")} / ${$translate("extensions.editor.open")}`;
    }
    if ($currentRoute.startsWith("/models/")) {
      const id = $currentRoute.slice("/models/".length);
      return id ? `${$translate("navigation.models")} / ${decodeURIComponent(id)}` : $translate("navigation.models");
    }
    if ($currentRoute === "/models") {
      return $translate("navigation.models");
    }
    return $translate(routeTitles[$currentRoute] ?? "navigation.activity");
  });
  let isSettingsRoute = $derived($currentRoute === "/settings");

  const noProfileValue = "__none__";
  let switchingProfile = $state(false);

  async function handleProfileChange(value: string): Promise<void> {
    switchingProfile = true;
    try {
      await setActiveProfile(value === noProfileValue ? null : value);
    } catch (error) {
      console.error(error);
    } finally {
      switchingProfile = false;
    }
  }

  function handleRouteLoaded(event: { detail: { route: string | RegExp; location?: string } }) {
    const route = event.detail.route;
    // Prefer the actual URL path so parameterised routes (e.g. /models/:id)
    // are reflected accurately in currentRoute for sidebar highlighting.
    const loc = event.detail.location;
    currentRoute.set(loc ?? (typeof route === "string" ? route : "/"));
  }

  $effect(() => {
    document.documentElement.classList.toggle("dark", $isDarkMode);
  });

  $effect(() => {
    const el = document.documentElement;
    if ($themeName === "default") el.removeAttribute("data-theme");
    else el.setAttribute("data-theme", $themeName);
  });

  $effect(() => {
    const icon = $connectionState === "connecting" ? "\u{1F7E1}" : $connectionState === "connected" ? "\u{1F7E2}" : "\u{1F534}";
    document.title = `${icon} ${$appTitle}`;
  });

  $effect(() => {
    document.documentElement.lang = $locale;
  });

  // Warn before leaving Settings with unsaved changes: browser refresh/close/
  // tab-leave via beforeunload, and in-app navigation via a capture-phase
  // interceptor on sidebar links. Only active while the settings draft is dirty.
  let leaveDialogOpen = $state(false);
  let pendingNavHref = $state("");

  function confirmLeaveSettings(): void {
    const href = pendingNavHref;
    leaveDialogOpen = false;
    pendingNavHref = "";
    // Replicates the router link's own navigation (hash-based) so the SPA
    // transitions exactly as an unguarded click would have.
    if (href) window.location.hash = href;
  }

  $effect(() => {
    if (!$settingsDirty) return;
    const beforeUnload = (event: BeforeUnloadEvent): void => {
      event.preventDefault();
      event.returnValue = "";
    };
    window.addEventListener("beforeunload", beforeUnload);
    const intercept = (event: MouseEvent): void => {
      const origin = event.target as Element | null;
      const anchor = origin?.closest?.("a[href]") as HTMLAnchorElement | null;
      if (!anchor) return;
      const href = anchor.getAttribute("href");
      if (anchor.getAttribute("target") === "_blank" || !href) return;
      if (!isInternalRoute(href) || isSameSettingsTarget(href)) return;
      // The modal decides whether to navigate, so the click is always
      // swallowed here; the dialog's confirm performs the navigation.
      event.preventDefault();
      event.stopPropagation();
      pendingNavHref = href;
      leaveDialogOpen = true;
    };
    document.addEventListener("click", intercept, true);
    return () => {
      window.removeEventListener("beforeunload", beforeUnload);
      document.removeEventListener("click", intercept, true);
    };
  });

  // Playground is always mounted (rather than routed) so it keeps its state
  // when the user navigates away, but it's still lazy-loaded on app start so
  // its dependencies (chat markdown/KaTeX/highlight.js rendering) don't block
  // the initial page load.
  let PlaygroundComponent = $state<Component | null>(null);
  let protectedServicesStarted = false;

  function startProtectedServices(): void {
    if (protectedServicesStarted) return;
    protectedServicesStarted = true;
    enableAPIEvents(true);
    checkPerformanceEnabled();
    import("./routes/Playground.svelte").then((m) => {
      PlaygroundComponent = m.default;
    });
  }

  function stopProtectedServices(): void {
    if (!protectedServicesStarted) return;
    protectedServicesStarted = false;
    enableAPIEvents(false);
  }

  onMount(() => {
    const cleanupScreenWidth = initScreenWidth();
    const cleanupSystemTheme = initSystemThemeListener();
    const unsubscribeSession = authSession.subscribe((session) => {
      if (!session.loading && (!session.configured || session.authenticated)) {
        startProtectedServices();
      } else {
        stopProtectedServices();
      }
    });
    void refreshAuthSession();

    return () => {
      cleanupScreenWidth();
      cleanupSystemTheme();
      unsubscribeSession();
      stopProtectedServices();
    };
  });
</script>

{#if $authSession.loading}
  <div class="bg-muted/30 flex min-h-dvh items-center justify-center" aria-live="polite">
    <div class="text-muted-foreground text-sm">{$translate("controlPlane.authChecking")}</div>
  </div>
{:else if $authSession.configured && !$authSession.authenticated}
  <AuthScreen />
{:else}
  <Tooltip.Provider>
    <Sidebar.Provider>
      <AppSidebar />
      <Sidebar.Inset class="h-screen min-w-0 overflow-hidden">
        {#if isSettingsRoute}
          <!-- Settings is nested inside the app shell: the primary sidebar and
               theme header are kept, and the settings page supplies its own
               page header + sub-navigation + content. It is NOT a separate
               full-bleed app (no second logo/nav of its own). -->
          <div class="min-h-0 flex-1">
            <Router {routes} on:routeLoaded={handleRouteLoaded} />
          </div>
        {:else}
          <header
            class="bg-background sticky top-0 z-10 flex h-14 shrink-0 items-center gap-2 border-b px-4"
          >
            <Sidebar.Trigger class="-ml-1" />
            <Separator orientation="vertical" class="mr-2 !h-4" />
            <h2 class="truncate pb-0 text-sm font-semibold">{sectionTitle}</h2>
            {#if $profiles.length > 0}
              <div class="ml-auto flex items-center gap-2">
                <CCSwitchActions />
                <ApiAuth />
                <span class="text-muted-foreground hidden text-xs sm:inline">{$translate("profile.label")}</span>
                <Select.Root
                  type="single"
                  value={$activeProfile ?? noProfileValue}
                  onValueChange={(value) => value && void handleProfileChange(value)}
                >
                  <Select.Trigger
                    class="w-40"
                    aria-label={$translate("profile.active")}
                    disabled={switchingProfile}
                  >
                    {$activeProfile ?? $translate("profile.none")}
                  </Select.Trigger>
                  <Select.Content>
                    <Select.Item value={noProfileValue}>{$translate("profile.none")}</Select.Item>
                    {#each $profiles as profile (profile.id)}
                      <Select.Item value={profile.id}>{profile.id}</Select.Item>
                    {/each}
                  </Select.Content>
                </Select.Root>
              </div>
            {:else}
              <div class="ml-auto flex items-center gap-2"><CCSwitchActions /><ApiAuth /></div>
            {/if}
          </header>

          <main class="min-h-0 flex-1 overflow-auto p-4">
            <div class="h-full" class:hidden={$currentRoute !== "/playground"}>
              {#if PlaygroundComponent}
                <PlaygroundComponent />
              {:else}
                <RouteLoading />
              {/if}
            </div>
            <div class="h-full" class:hidden={$currentRoute === "/playground"}>
              <Router {routes} on:routeLoaded={handleRouteLoaded} />
            </div>
          </main>
        {/if}
      </Sidebar.Inset>
    </Sidebar.Provider>

    <ConfirmDialog
      bind:open={leaveDialogOpen}
      onOpenChange={(open) => {
        if (!open) pendingNavHref = "";
      }}
      title={$translate("settingsCenter.unsavedChanges")}
      message={$translate("settingsCenter.leaveUnsaved")}
      confirmLabel={$translate("settingsCenter.leave")}
      onConfirm={confirmLeaveSettings}
    />

    <ToastHost />
  </Tooltip.Provider>
{/if}
