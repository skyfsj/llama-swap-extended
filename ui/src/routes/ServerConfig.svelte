<script lang="ts">
  import { onMount } from "svelte";
  import { push } from "svelte-spa-router";
  import { translate } from "../lib/i18n";
  onMount(() => { void push("/settings"); });
</script>

<div class="text-muted-foreground flex min-h-40 items-center justify-center text-sm" role="status">{$translate("settings.opening")}</div>
