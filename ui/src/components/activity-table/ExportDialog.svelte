<script lang="ts">
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { copyText } from "../../lib/clipboard";
  import { translate } from "../../lib/i18n";

  interface Props {
    markdown: string;
    open: boolean;
    onclose: () => void;
  }

  let { markdown, open, onclose }: Props = $props();

  let contentEl = $state<HTMLElement | null>(null);
  let preEl = $state<HTMLPreElement | null>(null);
  let status = $state<"idle" | "copied" | "failed">("idle");
  let statusTimer: ReturnType<typeof setTimeout> | null = null;

  // execCommand("copy") is the only route on plain http, and it can still be
  // refused. When it is, select the source so the user can finish the job with
  // Ctrl+C rather than leaving them with a button that silently does nothing.
  async function copy() {
    const copied = await copyText(markdown, contentEl);
    status = copied ? "copied" : "failed";
    if (!copied) selectSource();
    if (statusTimer !== null) clearTimeout(statusTimer);
    statusTimer = setTimeout(() => {
      status = "idle";
      statusTimer = null;
    }, 2000);
  }

  function selectSource() {
    if (!preEl) return;
    const range = document.createRange();
    range.selectNodeContents(preEl);
    const selection = window.getSelection();
    selection?.removeAllRanges();
    selection?.addRange(range);
  }

  $effect(() => {
    return () => {
      if (statusTimer !== null) clearTimeout(statusTimer);
    };
  });
</script>

<Dialog.Root
  {open}
  onOpenChange={(v) => {
    if (!v) onclose();
  }}
>
  <Dialog.Content
    bind:ref={contentEl}
    class="flex max-h-[90vh] w-[90%] flex-col gap-0 p-0 sm:max-w-[90%]"
  >
    <Dialog.Header class="border-border border-b px-4 py-3">
      <Dialog.Title class="text-lg font-bold">{$translate("activity.export.title")}</Dialog.Title>
      <Dialog.Description class="text-muted-foreground text-sm">
        {$translate("activity.export.description")}
      </Dialog.Description>
    </Dialog.Header>

    <div class="min-h-0 flex-1 overflow-auto p-4">
      <div class="bg-background border-border max-h-[60vh] overflow-auto rounded-md border">
        <pre
          bind:this={preEl}
          class="p-3 font-mono text-xs whitespace-pre">{markdown || $translate("activity.export.noRows")}</pre>
      </div>
    </div>

    <!--
      Dialog.Footer's -mx-4/-mb-4 cancel the default p-4 on Dialog.Content. This
      dialog sets p-0, so those offsets have to be zeroed or the bar hangs off
      the edges of the box.
    -->
    <Dialog.Footer
      class="border-border bg-card mx-0 mb-0 border-t px-4 py-3 sm:justify-end"
    >
      <Button variant="outline" onclick={onclose}>{$translate("common.close")}</Button>
      <Button onclick={copy} disabled={!markdown}>
        {status === "copied" ? $translate("common.copied") : status === "failed" ? $translate("common.pressCtrlC") : $translate("common.copy")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
