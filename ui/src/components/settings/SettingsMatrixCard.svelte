<script lang="ts">
  import { Plus, Trash2 } from "@lucide/svelte";
  import { Button } from "$lib/components/ui/button/index.js";
  import { translate } from "$lib/i18n";
  import {
    combinationsToDsl,
    matrixFromConfig,
    matrixIdentifiers,
    matrixToConfig,
    type MatrixDraft,
  } from "$lib/matrixDsl";

  interface Props {
    /** The matrix configuration block, or undefined when it is absent. */
    value: unknown;
    /** Model IDs the vars table may point at. */
    modelOptions: string[];
    disabled?: boolean;
    onChange: (next: Record<string, unknown>) => void;
  }

  let {
    value,
    modelOptions,
    disabled = false,
    onChange,
  }: Props = $props();

  // The draft is rebuilt whenever the incoming configuration changes (a reload,
  // another field's edit), and kept locally while the operator edits so a
  // keystroke is not overwritten by a snapshot refresh. The initial value is
  // seeded by the effect below, whose first run hydrates from the prop.
  let draft = $state<MatrixDraft>(matrixFromConfig(undefined));
  let appliedValue = $state("");

  $effect(() => {
    const serialized = JSON.stringify(value ?? null);
    if (serialized === appliedValue) return;
    appliedValue = serialized;
    draft = matrixFromConfig(value);
  });

  function commit(): void {
    // matrixToConfig omits empty blocks entirely, so clearing the last entry
    // removes the matrix rather than leaving a block the loader rejects.
    onChange(matrixToConfig(draft) as unknown as Record<string, unknown>);
  }

  function addVar(): void {
    draft.vars = [...draft.vars, { name: "", model: "" }];
    commit();
  }

  function removeVar(index: number): void {
    draft.vars = draft.vars.filter((_, position) => position !== index);
    commit();
  }

  function addCost(): void {
    draft.evictCosts = [...draft.evictCosts, { key: "", cost: "1" }];
    commit();
  }

  function removeCost(index: number): void {
    draft.evictCosts = draft.evictCosts.filter((_, position) => position !== index);
    commit();
  }

  function addSet(): void {
    draft.sets = [...draft.sets, { name: "", combinations: [{ members: [] }], rawDSL: "", unrepresentable: false }];
    commit();
  }

  function removeSet(index: number): void {
    draft.sets = draft.sets.filter((_, position) => position !== index);
    commit();
  }

  function addCombination(setIndex: number): void {
    draft.sets = draft.sets.map((set, position) =>
      position === setIndex ? { ...set, combinations: [...set.combinations, { members: [] }] } : set,
    );
    commit();
  }

  function removeCombination(setIndex: number, combinationIndex: number): void {
    draft.sets = draft.sets.map((set, position) =>
      position === setIndex
        ? { ...set, combinations: set.combinations.filter((_, row) => row !== combinationIndex) }
        : set,
    );
    commit();
  }

  // A member is toggled inside one combination row: adding an already present
  // name removes it, so the control works as a multi-select without a nested
  // list editor.
  function toggleMember(setIndex: number, combinationIndex: number, name: string): void {
    draft.sets = draft.sets.map((set, position) => {
      if (position !== setIndex) return set;
      return {
        ...set,
        combinations: set.combinations.map((combination, row) => {
          if (row !== combinationIndex) return combination;
          const members = combination.members.includes(name)
            ? combination.members.filter((member) => member !== name)
            : [...combination.members, name];
          return { members };
        }),
      };
    });
    commit();
  }

  function setRawDSL(setIndex: number, dsl: string): void {
    draft.sets = draft.sets.map((set, position) =>
      position === setIndex ? { ...set, rawDSL: dsl } : set,
    );
    commit();
  }

  const costKeys = $derived.by(() => [
    ...new Set([
      ...draft.vars.map((entry) => entry.name.trim()).filter((name) => name !== ""),
      ...modelOptions,
    ]),
  ]);
</script>

<section class="matrix-card settings-section-card" aria-labelledby="settings-card-matrix">
  <header class="settings-section-card__header">
    <div>
      <h3 id="settings-card-matrix">{$translate("settingsCenter.matrix.title")}</h3>
      <p>{$translate("settingsCenter.matrix.description")}</p>
    </div>
  </header>

  <div class="matrix-card__blocks">
    <div class="matrix-card__block">
      <div class="matrix-card__blockHead">
        <h4>{$translate("controlPlane.matrixVars")}</h4>
        <Button size="sm" variant="outline" onclick={addVar} {disabled}><Plus aria-hidden="true" />{$translate("controlPlane.schemaAddVariable")}</Button>
      </div>
      <p class="matrix-card__hint">{$translate("settingsCenter.matrix.varsHint")}</p>
      {#if draft.vars.length === 0}
        <p class="matrix-card__empty">{$translate("settingsCenter.matrix.noVars")}</p>
      {:else}
        <table class="matrix-card__table">
          <thead><tr><th>{$translate("controlPlane.matrixVariableName")}</th><th>{$translate("controlPlane.matrixModel")}</th><th></th></tr></thead>
          <tbody>
            {#each draft.vars as entry, index (index)}
              <tr>
                <td><input class="matrix-card__input" value={entry.name} disabled={disabled} placeholder="small" oninput={(event) => { draft.vars = draft.vars.map((row, position) => position === index ? { ...row, name: event.currentTarget.value } : row); commit(); }} /></td>
                <td>
                  <select class="matrix-card__input" value={entry.model} disabled={disabled} onchange={(event) => { draft.vars = draft.vars.map((row, position) => position === index ? { ...row, model: event.currentTarget.value } : row); commit(); }}>
                    <option value="">{$translate("controlPlane.matrixSelectModel")}</option>
                    {#each modelOptions as option (option)}
                      <option value={option}>{option}</option>
                    {/each}
                  </select>
                </td>
                <td><Button size="icon-sm" variant="ghost" aria-label={$translate("common.delete")} onclick={() => removeVar(index)} {disabled}><Trash2 aria-hidden="true" /></Button></td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </div>

    <div class="matrix-card__block">
      <div class="matrix-card__blockHead">
        <h4>{$translate("controlPlane.matrixEvictCosts")}</h4>
        <Button size="sm" variant="outline" onclick={addCost} {disabled}><Plus aria-hidden="true" />{$translate("settingsCenter.matrix.addCost")}</Button>
      </div>
      <p class="matrix-card__hint">{$translate("settingsCenter.matrix.costsHint")}</p>
      {#if draft.evictCosts.length === 0}
        <p class="matrix-card__empty">{$translate("settingsCenter.matrix.noCosts")}</p>
      {:else}
        <table class="matrix-card__table">
          <thead><tr><th>{$translate("controlPlane.matrixModel")}</th><th>{$translate("controlPlane.matrixCost")}</th><th></th></tr></thead>
          <tbody>
            {#each draft.evictCosts as entry, index (index)}
              <tr>
                <td>
                  <select class="matrix-card__input" value={entry.key} disabled={disabled} onchange={(event) => { draft.evictCosts = draft.evictCosts.map((row, position) => position === index ? { ...row, key: event.currentTarget.value } : row); commit(); }}>
                    <option value="">—</option>
                    {#each costKeys as option (option)}
                      <option value={option}>{option}</option>
                    {/each}
                  </select>
                </td>
                <td><input class="matrix-card__input" type="number" min="1" step="1" value={entry.cost} disabled={disabled} oninput={(event) => { draft.evictCosts = draft.evictCosts.map((row, position) => position === index ? { ...row, cost: event.currentTarget.value } : row); commit(); }} /></td>
                <td><Button size="icon-sm" variant="ghost" aria-label={$translate("common.delete")} onclick={() => removeCost(index)} {disabled}><Trash2 aria-hidden="true" /></Button></td>
              </tr>
            {/each}
          </tbody>
        </table>
      {/if}
    </div>

    <div class="matrix-card__block">
      <div class="matrix-card__blockHead">
        <h4>{$translate("controlPlane.matrixSets")}</h4>
        <Button size="sm" variant="outline" onclick={addSet} {disabled}><Plus aria-hidden="true" />{$translate("controlPlane.schemaAddSet")}</Button>
      </div>
      <p class="matrix-card__hint">{$translate("settingsCenter.matrix.setsHint")}</p>
      {#if draft.sets.length === 0}
        <p class="matrix-card__empty">{$translate("settingsCenter.matrix.noSets")}</p>
      {:else}
        <div class="matrix-card__sets">
          {#each draft.sets as set, setIndex (setIndex)}
            <div class="matrix-card__set">
              <div class="matrix-card__setHead">
                <input class="matrix-card__input matrix-card__input--name" value={set.name} disabled={disabled} placeholder={$translate("controlPlane.matrixSetName")} oninput={(event) => { draft.sets = draft.sets.map((row, position) => position === setIndex ? { ...row, name: event.currentTarget.value } : row); commit(); }} />
                <Button size="icon-sm" variant="ghost" aria-label={$translate("common.delete")} onclick={() => removeSet(setIndex)} {disabled}><Trash2 aria-hidden="true" /></Button>
              </div>
              {#if set.unrepresentable}
                <p class="matrix-card__warning" role="alert">{$translate("settingsCenter.matrix.unrepresentable")}</p>
                <textarea class="matrix-card__input matrix-card__dsl" rows="2" value={set.rawDSL} disabled={disabled} oninput={(event) => setRawDSL(setIndex, event.currentTarget.value)}></textarea>
              {:else}
                <p class="matrix-card__dslPreview"><code>{combinationsToDsl(set.combinations) || "—"}</code></p>
                {#each set.combinations as combination, combinationIndex (combinationIndex)}
                  <div class="matrix-card__row">
                    <span class="matrix-card__rowOp">{combinationIndex === 0 ? "" : "|"}</span>
                    <div class="matrix-card__chips">
                      {#each combination.members as member, memberIndex (member)}
                        <button type="button" class="matrix-card__chip" disabled={disabled} onclick={() => toggleMember(setIndex, combinationIndex, member)}>
                          {member}
                          {#if memberIndex < combination.members.length - 1}<span class="matrix-card__chipAnd">&amp;</span>{/if}
                        </button>
                      {/each}
                      <select class="matrix-card__input matrix-card__add" value="" disabled={disabled} onchange={(event) => { if (event.currentTarget.value) toggleMember(setIndex, combinationIndex, event.currentTarget.value); }}>
                        <option value="">+ {$translate("controlPlane.matrixAddModel")}</option>
                        {#each matrixIdentifiers(draft, modelOptions) as option (option)}
                          {#if !combination.members.includes(option)}
                            <option value={option}>{option}</option>
                          {/if}
                        {/each}
                      </select>
                    </div>
                    <Button size="icon-sm" variant="ghost" aria-label={$translate("common.delete")} onclick={() => removeCombination(setIndex, combinationIndex)} {disabled}><Trash2 aria-hidden="true" /></Button>
                  </div>
                {/each}
                <div class="matrix-card__setFoot">
                  <Button size="sm" variant="ghost" onclick={() => addCombination(setIndex)} {disabled}><Plus aria-hidden="true" />{$translate("settingsCenter.matrix.addCombination")}</Button>
                </div>
              {/if}
            </div>
          {/each}
        </div>
      {/if}
    </div>
  </div>
</section>

<style>
  .matrix-card__blocks {
    display: grid;
    gap: 1.1rem;
  }

  .matrix-card__block {
    display: grid;
    gap: 0.45rem;
  }

  .matrix-card__blockHead {
    display: flex;
    align-items: center;
    justify-content: space-between;
    gap: 0.5rem;
  }

  .matrix-card__blockHead h4 {
    margin: 0;
    font-size: 0.85rem;
    font-weight: 600;
  }

  .matrix-card__hint,
  .matrix-card__empty {
    margin: 0;
    color: var(--muted-foreground);
    font-size: 0.72rem;
  }

  .matrix-card__table {
    width: 100%;
    border-collapse: collapse;
    font-size: 0.78rem;
  }

  .matrix-card__table th {
    text-align: start;
    color: var(--muted-foreground);
    font-weight: 500;
    padding-block: 0.3rem;
  }

  .matrix-card__input {
    width: 100%;
    min-width: 0;
    border: 1px solid var(--input);
    border-radius: 0.4rem;
    background: var(--background);
    padding: 0.3rem 0.45rem;
    font-size: 0.78rem;
  }

  .matrix-card__dsl {
    font-family: ui-monospace, SFMono-Regular, Menlo, monospace;
    font-size: 0.74rem;
  }

  .matrix-card__sets {
    display: grid;
    gap: 0.6rem;
  }

  .matrix-card__set {
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.55rem;
    padding: 0.6rem;
    display: grid;
    gap: 0.45rem;
  }

  .matrix-card__setHead {
    display: flex;
    gap: 0.4rem;
    align-items: center;
  }

  .matrix-card__row {
    display: flex;
    align-items: center;
    gap: 0.45rem;
  }

  .matrix-card__rowOp {
    width: 0.7rem;
    color: var(--muted-foreground);
    font-size: 0.8rem;
    text-align: center;
  }

  .matrix-card__chips {
    display: flex;
    flex-wrap: wrap;
    align-items: center;
    gap: 0.3rem;
    flex: 1;
  }

  .matrix-card__chip {
    display: inline-flex;
    align-items: center;
    gap: 0.25rem;
    border: 1px solid color-mix(in oklab, var(--border) 82%, transparent);
    border-radius: 0.35rem;
    background: var(--muted);
    padding: 0.15rem 0.4rem;
    font-size: 0.74rem;
    cursor: pointer;
  }

  .matrix-card__chipAnd {
    color: var(--muted-foreground);
  }

  .matrix-card__add {
    width: auto;
    max-width: 9rem;
  }

  .matrix-card__dslPreview {
    margin: 0;
    color: var(--muted-foreground);
    font-size: 0.72rem;
    overflow-wrap: anywhere;
  }

  .matrix-card__warning {
    margin: 0;
    color: var(--destructive);
    font-size: 0.72rem;
  }

  .matrix-card__setFoot {
    display: flex;
    justify-content: flex-end;
  }
</style>
