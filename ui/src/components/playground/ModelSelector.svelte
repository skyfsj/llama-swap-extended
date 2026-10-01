<script lang="ts">
  import { playgroundModels, profileModels, selectorModels } from "../../stores/api";
  import { filterByCategory, type ModelCategory } from "../../lib/modelCategory";
  import * as Select from "$lib/components/ui/select/index.js";
  import { translate } from "../../lib/i18n";

  interface Props {
    value: string;
    placeholder?: string;
    disabled?: boolean;
    /** Only models of this category are listed (see lib/modelCategory). */
    category?: ModelCategory;
    /**
     * Optional second group listed after the primary one under its own
     * label — e.g. chat (LLM) models alongside translation models.
     */
    extraCategory?: ModelCategory;
    extraCategoryLabelKey?: string;
    /** Label for the primary category group (defaults to "local"). */
    groupLabelKey?: string;
    /**
     * List every remaining local model in a trailing "other" group. Specialized
     * endpoints use this so a model without a capability declaration stays
     * reachable instead of silently disappearing from the selector.
     */
    others?: boolean;
    othersLabelKey?: string;
  }

  let {
    value = $bindable(),
    placeholder,
    disabled = false,
    category = "chat",
    extraCategory,
    extraCategoryLabelKey,
    groupLabelKey,
    others = false,
    othersLabelKey,
  }: Props = $props();
  let placeholderText = $derived(placeholder ?? $translate("playground.modelSelector.default"));

  const listed = $derived(
    $playgroundModels.filter(
      (model) =>
        !model.unlisted &&
        (model.playgroundType === "model" || model.playgroundType === "peer")
    )
  );
  const local = $derived(listed.filter((model) => !model.peerID));
  const peers = $derived(listed.filter((model) => model.peerID));

  // Strict category filtering: an image model never appears in the chat
  // selector and vice versa. Profiles and selectors are chat routing
  // constructs, so they only apply to the chat category.
  const matching = $derived(filterByCategory(local, category));
  const matchingPeers = $derived(filterByCategory(peers, category));
  const extra = $derived(extraCategory ? filterByCategory(local, extraCategory) : []);
  const extraPeers = $derived(extraCategory ? filterByCategory(peers, extraCategory) : []);
  // everything not already shown in the primary/extra groups
  const shown = $derived(
    new Set([
      ...matching.map((m) => m.id),
      ...matchingPeers.map((m) => m.id),
      ...extra.map((m) => m.id),
      ...extraPeers.map((m) => m.id),
    ])
  );
  const otherModels = $derived(others ? local.filter((m) => !shown.has(m.id)) : []);
  const otherPeerModels = $derived(others ? peers.filter((m) => !shown.has(m.id)) : []);
  const showProfiles = $derived(category === "chat" && $profileModels.length > 0);
  const showSelectors = $derived(category === "chat" && $selectorModels.length > 0);
  const hasModels = $derived(
    matching.length > 0 ||
      matchingPeers.length > 0 ||
      extra.length > 0 ||
      extraPeers.length > 0 ||
      otherModels.length > 0 ||
      otherPeerModels.length > 0 ||
      showProfiles ||
      showSelectors
  );
</script>

{#if hasModels}
  <Select.Root
    type="single"
    {value}
    onValueChange={(v) => v !== undefined && (value = v)}
    {disabled}
  >
    <Select.Trigger class="pg-trigger min-w-0 flex-1 basis-48">{value || placeholderText}</Select.Trigger>
    <Select.Content class="max-h-[60vh]">
      <Select.Item value="">{placeholderText}</Select.Item>
      {#if showProfiles}
        <Select.Group>
          <Select.Label>{$translate("playground.modelSelector.profile")}</Select.Label>
          {#each $profileModels as model (model.id)}
            <Select.Item value={model.id}>{model.id}</Select.Item>
          {/each}
        </Select.Group>
        <Select.Separator />
      {/if}
      {#if matching.length > 0}
        <Select.Group>
          <Select.Label>{$translate(groupLabelKey ?? "playground.modelSelector.local")}</Select.Label>
          {#each matching as model (model.id)}
            <Select.Item value={model.id}>{model.id}</Select.Item>
            {#if model.aliases}
              {#each model.aliases as alias (alias)}
                <Select.Item value={alias}>↳ {alias}</Select.Item>
              {/each}
            {/if}
          {/each}
        </Select.Group>
        {#if matchingPeers.length > 0}
          <Select.Separator />
        {/if}
      {/if}
      {#if extra.length > 0 || extraPeers.length > 0}
        {#if matching.length > 0}
          <Select.Separator />
        {/if}
        <Select.Group>
          <Select.Label>{$translate(extraCategoryLabelKey ?? "playground.modelSelector.local")}</Select.Label>
          {#each extra as model (model.id)}
            <Select.Item value={model.id}>{model.id}</Select.Item>
            {#if model.aliases}
              {#each model.aliases as alias (alias)}
                <Select.Item value={alias}>↳ {alias}</Select.Item>
              {/each}
            {/if}
          {/each}
        </Select.Group>
      {/if}
      {#if matchingPeers.length > 0}
        <Select.Group>
          <Select.Label>{$translate("playground.modelSelector.peers")}</Select.Label>
          {#each matchingPeers as model (model.id)}
            <Select.Item value={model.id}>{model.id}</Select.Item>
            {#if model.aliases}
              {#each model.aliases as alias (alias)}
                <Select.Item value={alias}>↳ {alias}</Select.Item>
              {/each}
            {/if}
          {/each}
        </Select.Group>
      {/if}
      {#if otherModels.length > 0 || otherPeerModels.length > 0}
        {#if matching.length > 0 || extra.length > 0 || matchingPeers.length > 0}
          <Select.Separator />
        {/if}
        <Select.Group>
          <Select.Label>{$translate(othersLabelKey ?? "playground.modelSelector.groupOther")}</Select.Label>
          {#each otherModels as model (model.id)}
            <Select.Item value={model.id}>{model.id}</Select.Item>
            {#if model.aliases}
              {#each model.aliases as alias (alias)}
                <Select.Item value={alias}>↳ {alias}</Select.Item>
              {/each}
            {/if}
          {/each}
          {#each otherPeerModels as model (model.id)}
            <Select.Item value={model.id}>{model.id}</Select.Item>
            {#if model.aliases}
              {#each model.aliases as alias (alias)}
                <Select.Item value={alias}>↳ {alias}</Select.Item>
              {/each}
            {/if}
          {/each}
        </Select.Group>
      {/if}
      {#if showSelectors}
        <Select.Separator />
        <Select.Group>
          <Select.Label>{$translate("playground.modelSelector.selectors")}</Select.Label>
          {#each $selectorModels as model (model.id)}
            <Select.Item value={model.id}>{model.id}</Select.Item>
          {/each}
        </Select.Group>
      {/if}
    </Select.Content>
  </Select.Root>
{/if}
