<script lang="ts">
  import { LogOut, LoaderCircle } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import * as Dialog from "$lib/components/ui/dialog/index.js";
  import { translate } from "../lib/i18n";
  import { authSession, signOut } from "../stores/auth";

  let submitting = $state(false);
  let error = $state("");
  let confirmOpen = $state(false);

  function requestSignOut(): void {
    if (!submitting) {
      error = "";
      confirmOpen = true;
    }
  }

  async function endSession(): Promise<void> {
    if (submitting) return;
    submitting = true;
    error = "";
    try {
      await signOut();
      confirmOpen = false;
    } catch (cause) {
      error = cause instanceof Error ? cause.message : String(cause);
    } finally {
      submitting = false;
    }
  }
</script>

{#if $authSession.configured && $authSession.authenticated}
  <div class="flex items-center gap-2">
    <Button
      variant="outline"
      size="sm"
      onclick={requestSignOut}
      disabled={submitting}
      aria-label={$translate("controlPlane.authSignOut")}
      title={$translate("controlPlane.authSignOut")}
    >
      {#if submitting}
        <LoaderCircle class="size-3.5 animate-spin" aria-hidden="true" />
        <span class="hidden sm:inline">{$translate("controlPlane.authSigningOut")}</span>
      {:else}
        <LogOut class="size-3.5" aria-hidden="true" />
        <span>{$translate("controlPlane.authSignOut")}</span>
      {/if}
    </Button>
    {#if error && !confirmOpen}
      <span class="text-destructive max-w-40 truncate text-xs" role="alert" title={error}>{error}</span>
    {/if}
  </div>
{/if}

<Dialog.Root bind:open={confirmOpen}>
  <Dialog.Content class="max-w-lg sm:max-w-lg" showCloseButton={!submitting}>
    <Dialog.Header>
      <Dialog.Title>{$translate("controlPlane.authSignOut")}</Dialog.Title>
      <Dialog.Description>{$translate("controlPlane.authSignOutConfirm")}</Dialog.Description>
    </Dialog.Header>
    {#if error}<div class="rounded-md border border-destructive/40 bg-destructive/10 p-3 text-sm" role="alert">{error}</div>{/if}
    <Dialog.Footer>
      <Button variant="outline" onclick={() => { confirmOpen = false; }} disabled={submitting}>{$translate("common.cancel")}</Button>
      <Button variant="destructive" onclick={() => void endSession()} disabled={submitting}>
        {#if submitting}<LoaderCircle class="size-4 animate-spin" aria-hidden="true" />{/if}
        {$translate("controlPlane.authSignOut")}
      </Button>
    </Dialog.Footer>
  </Dialog.Content>
</Dialog.Root>
