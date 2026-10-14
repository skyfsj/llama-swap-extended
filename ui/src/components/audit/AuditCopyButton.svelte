<script lang="ts">
  import { Copy, Check } from "@lucide/svelte";
  import { copyText } from "../../lib/clipboard";
  import { translate } from "../../lib/i18n";

  interface Props {
    text: string;
    title?: string;
    label?: string;
    size?: "xs" | "sm" | "default";
    variant?: "ghost" | "outline" | "secondary";
    class?: string;
  }

  let {
    text,
    title = "",
    label = "",
    size = "xs",
    variant = "ghost",
    class: className = "",
  }: Props = $props();

  let copied = $state(false);
  let timer: ReturnType<typeof setTimeout> | null = null;
  let buttonEl = $state<HTMLButtonElement | null>(null);

  async function handleCopy(e: MouseEvent) {
    e.stopPropagation();
    if (!text) return;
    const dialogContainer = buttonEl?.closest<HTMLElement>("[role='dialog']");
    const success = await copyText(text, dialogContainer);
    if (success) {
      copied = true;
      if (timer) clearTimeout(timer);
      timer = setTimeout(() => {
        copied = false;
        timer = null;
      }, 2000);
    }
  }

  let sizeClasses = $derived(
    size === "xs"
      ? "h-6 px-1.5 text-xs gap-1"
      : size === "sm"
        ? "h-7 px-2 text-xs gap-1.5"
        : "h-8 px-2.5 text-sm gap-2"
  );

  let variantClasses = $derived(
    variant === "outline"
      ? "border border-border bg-background hover:bg-muted"
      : variant === "secondary"
        ? "bg-secondary text-secondary-foreground hover:bg-secondary/80"
        : "text-muted-foreground hover:text-foreground hover:bg-muted/60"
  );
</script>

<button
  bind:this={buttonEl}
  type="button"
  class="inline-flex items-center justify-center rounded-md font-medium transition-colors focus-visible:outline-none focus-visible:ring-1 focus-visible:ring-ring disabled:pointer-events-none disabled:opacity-50 {sizeClasses} {variantClasses} {className}"
  title={title || (copied ? $translate("controlPlane.auditCopied") : $translate("controlPlane.auditCopy"))}
  onclick={handleCopy}
  disabled={!text}
  aria-label={title || $translate("controlPlane.auditCopy")}
>
  {#if copied}
    <Check class="size-3.5 text-emerald-500 animate-in fade-in zoom-in-50" aria-hidden="true" />
    {#if label}
      <span class="text-emerald-600 dark:text-emerald-400 font-normal">{$translate("controlPlane.auditCopied")}</span>
    {/if}
  {:else}
    <Copy class="size-3.5" aria-hidden="true" />
    {#if label}
      <span class="font-normal">{label}</span>
    {/if}
  {/if}
</button>
