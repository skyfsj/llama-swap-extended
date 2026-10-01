<script lang="ts">
  import type { Model } from "../../lib/types";
  import { capabilityMessageKeys } from "../../lib/capabilities";
  import * as Card from "$lib/components/ui/card/index.js";
  import Tag from "../Tag.svelte";
  import { translate } from "../../lib/i18n";

  interface Props {
    model: Model;
  }

  let { model }: Props = $props();

  let capabilities = $derived.by(() => {
    const caps = model?.capabilities ?? {};
    return Object.entries(caps).filter(([, v]) => v);
  });
  let contextLength = $derived(model?.context_length ?? 0);
</script>

<Card.Root class="shrink-0 gap-0 overflow-hidden py-0">
  <Card.Header class="border-b px-4 py-2">
    <Card.Title class="text-sm font-semibold">{$translate("modelDetail.capabilities")}</Card.Title>
  </Card.Header>
  <Card.Content class="p-3">
    {#if contextLength > 0}
      <dl class="mb-3 grid grid-cols-[auto_1fr] gap-x-4 gap-y-1 text-sm">
        <dt class="text-muted-foreground">{$translate("modelDetail.contextLength")}</dt>
        <dd class="font-medium tabular-nums">{contextLength.toLocaleString()}</dd>
      </dl>
    {/if}
    {#if capabilities.length > 0}
      <div class="flex flex-wrap gap-1.5">
        {#each capabilities as [key] (key)}
          <Tag>{capabilityMessageKeys[key] ? $translate(capabilityMessageKeys[key]) : key}</Tag>
        {/each}
      </div>
    {:else if contextLength === 0}
      <span class="text-muted-foreground text-sm">{$translate("modelDetail.noCapabilities")}</span>
    {/if}
  </Card.Content>
</Card.Root>
