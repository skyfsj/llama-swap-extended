<script lang="ts">
  import { CheckCircle2, Info, AlertTriangle, XCircle, LoaderCircle, X } from "@lucide/svelte";
  import { toast, type ToastLevel } from "../lib/toast";
  import { translate } from "../lib/i18n";

  const icons: Record<ToastLevel, typeof CheckCircle2> = {
    success: CheckCircle2,
    info: Info,
    warning: AlertTriangle,
    error: XCircle,
    loading: LoaderCircle,
  };

  // Visual language of the message banners already used across the app
  // (bg-success/10, bg-destructive/10, ...) on solid surfaces so text stays
  // readable above any page content.
  const levelClass: Record<ToastLevel, string> = {
    success: "border-success/40 bg-success/15 text-success",
    info: "border-border bg-popover text-foreground",
    warning: "border-warning/40 bg-warning/15 text-warning",
    error: "border-destructive/40 bg-destructive/15 text-destructive",
    loading: "border-border bg-popover text-foreground",
  };
</script>

<!--
  Global toast host. Mounted once in App.svelte next to ConfirmDialog; it sits
  above dialogs (z-[70] vs the dialog's z-[60]) so action feedback stays
  visible while a modal is open. Auto-dismiss pauses on hover or keyboard
  focus, and the live region is polite and never steals focus.
-->
{#if $toast.length > 0}
  <div
    class="fixed left-1/2 top-16 z-[70] flex w-[min(92vw,28rem)] -translate-x-1/2 flex-col gap-2 pb-[env(safe-area-inset-top)]"
    role="region"
    aria-label={$translate("toast.region")}
    onmouseenter={() => toast.setPaused(true)}
    onmouseleave={() => toast.setPaused(false)}
    onfocusin={() => toast.setPaused(true)}
    onfocusout={() => toast.setPaused(false)}
  >
    {#each $toast as item (item.id)}
      {@const Icon = icons[item.level]}
      <div
        class={`flex items-start gap-2.5 rounded-lg border px-3.5 py-2.5 text-sm shadow-lg transition-transform animate-in slide-in-from-top-2 ${levelClass[item.level]}`}
        data-level={item.level}
      >
        <Icon class={`mt-0.5 size-4 shrink-0 ${item.level === "loading" ? "animate-spin" : ""}`} aria-hidden="true" />
        <div class="min-w-0 flex-1">
          <p role="status" class="break-words leading-snug">{item.message}{#if item.count > 1}<span class="ml-1.5 rounded-full bg-foreground/10 px-1.5 text-xs tabular-nums">×{item.count}</span>{/if}</p>
        </div>
        <button
          type="button"
          class="-mr-1 -mt-0.5 shrink-0 rounded p-1 opacity-60 transition-opacity hover:opacity-100"
          aria-label={$translate("common.close")}
          onclick={() => toast.dismiss(item.id)}
        >
          <X class="size-3.5" aria-hidden="true" />
        </button>
      </div>
    {/each}
  </div>
{/if}
