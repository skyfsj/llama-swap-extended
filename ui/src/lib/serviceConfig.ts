import { property, record } from "./modelConfig";

export type InheritedBoolean = "" | "true" | "false";

export interface ServiceConfigDraft {
  healthCheckTimeout: string;
  logRequests: InheritedBoolean;
  logLevel: string;
  logTimeFormat: string;
  logToStdout: string;
  metricsMaxInMemory: string;
  startPort: string;
  sendLoadingState: InheritedBoolean;
  includeAliasesInList: InheritedBoolean;
  globalTTL: string;
  unloadTimeout: string;
  rollbackOnModelStartFailure: boolean;
  storePath: string;
  performanceDisabled: InheritedBoolean;
  performanceEvery: string;
  uiSessionHeadersEnabled: boolean;
  uiSessionHeaders: string[];
}

function numberText(value: unknown): string {
  return typeof value === "number" && Number.isFinite(value) ? String(value) : "";
}

function stringText(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function booleanText(value: unknown): InheritedBoolean {
  return typeof value === "boolean" ? String(value) as InheritedBoolean : "";
}

export function draftFromServiceConfig(config: unknown): ServiceConfigDraft {
  const root = record(config);
  const store = record(property(root, "store"));
  const performance = record(property(root, "performance"));
  const ui = record(property(root, "ui"));
  const activity = record(property(ui, "activity"));
  const sessionHeaders = property(activity, "session_id");
  return {
    healthCheckTimeout: numberText(property(root, "healthCheckTimeout")),
    logRequests: booleanText(property(root, "logRequests")),
    logLevel: stringText(property(root, "logLevel")),
    logTimeFormat: property(root, "logTimeFormat") === undefined ? "__inherit__" : stringText(property(root, "logTimeFormat")),
    logToStdout: stringText(property(root, "logToStdout")),
    metricsMaxInMemory: numberText(property(root, "metricsMaxInMemory")),
    startPort: numberText(property(root, "startPort")),
    sendLoadingState: booleanText(property(root, "sendLoadingState")),
    includeAliasesInList: booleanText(property(root, "includeAliasesInList")),
    globalTTL: numberText(property(root, "globalTTL")),
    unloadTimeout: numberText(property(root, "unloadTimeout")),
    rollbackOnModelStartFailure: property(root, "rollbackOnModelStartFailure") !== false,
    storePath: stringText(property(store, "path")),
    performanceDisabled: booleanText(property(performance, "disabled")),
    performanceEvery: stringText(property(performance, "every")),
    uiSessionHeadersEnabled: Array.isArray(sessionHeaders),
    uiSessionHeaders: Array.isArray(sessionHeaders) ? sessionHeaders.filter((item): item is string => typeof item === "string") : [],
  };
}

function integer(value: string, field: string, minimum = 0): number | undefined {
  if (!value.trim()) return undefined;
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < minimum) {
    throw new Error(`${field} must be an integer >= ${minimum}`);
  }
  return parsed;
}

function optionalBoolean(value: InheritedBoolean): boolean | undefined {
  return value === "" ? undefined : value === "true";
}

function same(left: unknown, right: unknown): boolean {
  return JSON.stringify(left) === JSON.stringify(right);
}

function rootOperation(config: Record<string, unknown>, key: string, next: unknown): unknown | undefined {
  const exists = Object.prototype.hasOwnProperty.call(config, key);
  const current = config[key];
  if (next === undefined) return exists ? { op: "remove", path: `/${key}` } : undefined;
  if (exists && same(current, next)) return undefined;
  return { op: exists ? "replace" : "add", path: `/${key}`, value: next };
}

function compactObject(value: Record<string, unknown>): Record<string, unknown> | undefined {
  return Object.keys(value).length > 0 ? value : undefined;
}

function setOrDelete(target: Record<string, unknown>, key: string, value: unknown): void {
  if (value === undefined || value === "") delete target[key];
  else target[key] = value;
}

export function buildServicePatch(config: unknown, draft: ServiceConfigDraft): unknown[] {
  const root = record(config);
  const next: Record<string, unknown> = {
    healthCheckTimeout: integer(draft.healthCheckTimeout, "healthCheckTimeout", 15),
    logRequests: optionalBoolean(draft.logRequests),
    logLevel: draft.logLevel || undefined,
    logTimeFormat: draft.logTimeFormat === "__inherit__" ? undefined : draft.logTimeFormat,
    logToStdout: draft.logToStdout || undefined,
    metricsMaxInMemory: integer(draft.metricsMaxInMemory, "metricsMaxInMemory"),
    startPort: integer(draft.startPort, "startPort", 1),
    sendLoadingState: optionalBoolean(draft.sendLoadingState),
    includeAliasesInList: optionalBoolean(draft.includeAliasesInList),
    globalTTL: integer(draft.globalTTL, "globalTTL"),
    unloadTimeout: integer(draft.unloadTimeout, "unloadTimeout"),
  };

  // rollbackOnModelStartFailure defaults to true when the key is absent, so an
  // enabled toggle writes nothing and only opting out produces an operation.
  // Writing the default explicitly would show up as a spurious change on every
  // unrelated save and drift the file away from its documented default.
  if (!draft.rollbackOnModelStartFailure) {
    next.rollbackOnModelStartFailure = false;
  }

  const store = { ...record(property(root, "store")) };
  setOrDelete(store, "path", draft.storePath.trim());
  next.store = compactObject(store);

  const performance = { ...record(property(root, "performance")) };
  setOrDelete(performance, "disabled", optionalBoolean(draft.performanceDisabled));
  setOrDelete(performance, "every", draft.performanceEvery.trim());
  next.performance = compactObject(performance);

  const ui = { ...record(property(root, "ui")) };
  const activity = { ...record(property(ui, "activity")) };
  setOrDelete(activity, "session_id", draft.uiSessionHeadersEnabled ? [...draft.uiSessionHeaders] : undefined);
  setOrDelete(ui, "activity", compactObject(activity));
  next.ui = compactObject(ui);

  const operations = Object.entries(next)
    .map(([key, value]) => rootOperation(root, key, value))
    .filter((operation): operation is NonNullable<typeof operation> => operation !== undefined);
  return operations;
}
