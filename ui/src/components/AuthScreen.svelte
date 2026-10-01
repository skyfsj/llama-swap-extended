<script lang="ts">
  import { tick } from "svelte";
  import { ArrowRight, KeyRound, LoaderCircle, RefreshCw, ShieldCheck } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { Input } from "$lib/components/ui/input/index.js";
  import { translate } from "../lib/i18n";
  import { authSession, refreshAuthSession, signInWithAPIKey } from "../stores/auth";

  let secret = $state("");
  let submitting = $state(false);
  let formError = $state("");
  let keyInput = $state<HTMLInputElement | null>(null);

  $effect(() => {
    if (!$authSession.loading && !$authSession.authenticated) {
      void tick().then(() => keyInput?.focus());
    }
  });

  async function submit(): Promise<void> {
    if (!secret.trim()) {
      formError = $translate("controlPlane.authKeyRequired");
      return;
    }
    submitting = true;
    formError = "";
    try {
      await signInWithAPIKey(secret.trim());
      secret = "";
    } catch (cause) {
      formError = cause instanceof Error ? cause.message : String(cause);
    } finally {
      submitting = false;
    }
  }
</script>

<main class="bg-muted/30 flex min-h-dvh items-center justify-center p-4 sm:p-6">
  <section class="w-full max-w-md" aria-labelledby="auth-title">
    <div class="mb-5 flex items-center gap-3 px-1">
      <div class="bg-primary text-primary-foreground flex size-10 items-center justify-center rounded-lg shadow-sm">
        <ShieldCheck class="size-5" aria-hidden="true" />
      </div>
      <div class="min-w-0">
        <div class="text-foreground text-base font-semibold">llama-swap</div>
        <div class="text-muted-foreground text-xs">{$translate("controlPlane.authEyebrow")}</div>
      </div>
    </div>

    <div class="border-border bg-card rounded-lg border p-5 shadow-sm sm:p-6">
      <div class="mb-6">
        <h1 id="auth-title" class="text-xl font-semibold">{$translate("controlPlane.authTitle")}</h1>
        <p class="text-muted-foreground mt-1 text-sm">{$translate("controlPlane.authDescription")}</p>
      </div>

      {#if formError || $authSession.error}
        <div class="border-destructive/40 bg-destructive/10 text-destructive mb-4 rounded-md border px-3 py-2 text-sm" role="alert" aria-live="assertive">
          {formError || $authSession.error}
        </div>
      {/if}

      <form class="grid gap-4" onsubmit={(event) => { event.preventDefault(); void submit(); }}>
        <label class="grid gap-2 text-sm font-medium" for="api-key">
          <span>{$translate("controlPlane.authKeyLabel")}</span>
          <div class="relative">
            <KeyRound class="text-muted-foreground pointer-events-none absolute left-3 top-1/2 size-4 -translate-y-1/2" aria-hidden="true" />
            <Input
              bind:ref={keyInput}
              id="api-key"
              class="h-10 pl-9 font-mono text-sm"
              type="password"
              autocomplete="current-password"
              bind:value={secret}
              disabled={submitting}
              aria-describedby="api-key-help"
            />
          </div>
        </label>
        <p id="api-key-help" class="text-muted-foreground text-xs">{$translate("controlPlane.authSessionHint")}</p>
        <Button class="h-10 w-full" type="submit" disabled={submitting}>
          {#if submitting}
            <LoaderCircle class="size-4 animate-spin" aria-hidden="true" />
            {$translate("controlPlane.authSubmitting")}
          {:else}
            {$translate("controlPlane.authSubmit")}
            <ArrowRight class="size-4" aria-hidden="true" />
          {/if}
        </Button>
      </form>

      {#if $authSession.error}
        <div class="mt-4 border-t pt-4">
          <Button variant="outline" class="w-full" onclick={() => void refreshAuthSession()} disabled={submitting}>
            <RefreshCw class="size-4" aria-hidden="true" />
            {$translate("controlPlane.authRetry")}
          </Button>
        </div>
      {/if}
    </div>
  </section>
</main>
