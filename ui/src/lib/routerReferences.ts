// Which routing blocks of a configuration name a given model. The model delete
// flow uses this to warn that the delete will also adjust those blocks: the
// config validator rejects a routing block whose members no longer exist, so
// the server prunes the references as part of the delete.

type UnknownRecord = Record<string, unknown>;

function record(value: unknown): UnknownRecord {
  return value && typeof value === "object" && !Array.isArray(value) ? value as UnknownRecord : {};
}

function list(value: unknown): string[] {
  return Array.isArray(value) ? value.filter((item): item is string => typeof item === "string") : [];
}

// matrixNames reads every identifier a matrix references in its set
// expressions: the tokens of each DSL, plus the vars and evict cost keys that
// point at the model directly.
function matrixNames(matrix: unknown): string[] {
  const block = record(matrix);
  const names: string[] = [];
  for (const target of Object.values(record(block.vars))) {
    if (typeof target === "string") names.push(target);
  }
  names.push(...Object.keys(record(block.evict_costs)));
  for (const dsl of Object.values(record(block.sets))) {
    if (typeof dsl !== "string") continue;
    names.push(...dsl.split(/[\s&|()]+/).filter((token) => token !== "" && !token.startsWith("+")));
  }
  return names;
}

// RoutingBlockNames lists the routing blocks that name the model, in a stable
// order. Both the canonical routing tree and the legacy top-level keys are
// checked, because the loader normalizes one into the other.
export function routingBlockNamesFor(config: unknown, modelId: string): string[] {
  if (modelId === "") return [];
  const root = record(config);
  const routing = record(root.routing);
  const router = record(routing.router);
  const settings = record(router.settings);
  const scheduler = record(routing.scheduler);
  const schedulerSettings = record(scheduler.settings);
  const fifo = record(schedulerSettings.fifo);

  const blocks: Array<{ name: string; names: string[] }> = [
    { name: "matrix", names: [...matrixNames(settings.matrix), ...matrixNames(root.matrix)] },
    { name: "groups", names: [
      ...Object.values(record(settings.groups)).flatMap((group) => list(record(group).members)),
      ...Object.values(record(root.groups)).flatMap((group) => list(record(group).members)),
    ] },
    { name: "gpus", names: [
      ...Object.values(record(settings.gpus)).flatMap((models) => list(models)),
    ] },
    { name: "priority", names: Object.keys(record(fifo.priority)) },
    { name: "selectors", names: [
      ...Object.values(record(root.selectors)).flatMap((selector) => list(record(selector).targets)),
    ] },
    { name: "profiles", names: [
      ...Object.values(record(root.profiles)).flatMap((profile) => Object.values(record(record(profile).pins))),
    ].filter((value): value is string => typeof value === "string") },
  ];

  return blocks.filter((block) => block.names.includes(modelId)).map((block) => block.name);
}
