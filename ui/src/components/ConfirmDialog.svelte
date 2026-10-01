<script lang="ts">
  // Shared modal confirmation used for destructive or hard-to-reverse
  // actions. The parent owns `open` (bind:open) and the pending action;
  // this component only renders the question and the two choices. The
  // browser's window.confirm is deliberately not used: it blocks the page,
  // cannot be themed or localized through the app catalog, and cannot be
  // tested or extended (progress state, secondary actions, ...).
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "$lib/i18n";

  let {
    open = $bindable(false),
    title,
    message,
    confirmLabel,
    onConfirm,
    onOpenChange,
  }: {
    open?: boolean;
    title: string;
    message: string;
    confirmLabel?: string;
    onConfirm: () => void;
    onOpenChange?: (open: boolean) => void;
  } = $props();
</script>

<Dialog.Root bind:open onOpenChange={(value: boolean) => onOpenChange?.(value)}>
  <!-- z-[60]: confirmation dialogs can open while a detail sheet (z-50)
       is still open; they must stack above it or the sheet covers the
       buttons. The overlay is raised the same way. -->
  <Dialog.Content class="max-w-lg z-[60]" overlayClassName="z-[60]">
    <Dialog.Header>
      <Dialog.Title>{title}</Dialog.Title>
      <Dialog.Description>{message}</Dialog.Description>
    </Dialog.Header>
    <Dialog.Footer>
      <Button variant="outline" type="button" onclick={() => (open = false)}>
        {$translate("common.cancel")}
      </Button>
      <Button type="button" onclick={onConfirm}>
        {confirmLabel || $translate("common.confirm")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
