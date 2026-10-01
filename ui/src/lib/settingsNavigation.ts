import type { SettingsField } from "./settingsApi";

export type SettingsTranslator = (key: string) => string;

export interface SettingsNavigationItem {
  id: string;
  /** Logical category the item belongs to (drives the sub-sidebar groupings). */
  group: string;
  label: string;
  description: string;
  sections: string[];
}

export interface SettingsFieldGroup {
  id: string;
  title: string;
  description: string;
  fields: SettingsField[];
}

/**
 * A sub-sidebar entry. `sections` are the schema sections to draw fields from;
 * `keep` / `drop` optionally narrow to specific top-level config keys so one
 * schema section can be split across several logical pages (e.g. the `routing`
 * section holds both the router and the profiles/selectors entities, and the
 * `observability` section holds both metrics and the audit store).
 */
interface SettingsNavigationSource {
  id: string;
  group: string;
  labelKey: string;
  descriptionKey: string;
  sections: string[];
  keep?: string[];
  drop?: string[];
}

interface SettingsGroupMeta {
  id: string;
  titleKey: string;
  descriptionKey: string;
}

const settingsNavigation: SettingsNavigationSource[] = [
  // 基础 (Basic)
  { id: "general", group: "basic", labelKey: "settingsCenter.navigation.general.label", descriptionKey: "settingsCenter.navigation.general.description", sections: ["general", "storage", "ui"] },
  { id: "observability", group: "basic", labelKey: "settingsCenter.navigation.observability.label", descriptionKey: "settingsCenter.navigation.observability.description", sections: ["logging", "observability"], drop: ["audit"] },
  // 模型与运行 (Models & runtime)
  { id: "modelFiles", group: "models", labelKey: "settingsCenter.navigation.modelFiles.label", descriptionKey: "settingsCenter.navigation.modelFiles.description", sections: ["modelFiles"] },
  { id: "models", group: "models", labelKey: "settingsCenter.navigation.models.label", descriptionKey: "settingsCenter.navigation.models.description", sections: ["models"] },
  { id: "performance", group: "models", labelKey: "settingsCenter.navigation.performance.label", descriptionKey: "settingsCenter.navigation.performance.description", sections: ["performance"] },
  // 调度 (Scheduling)
  { id: "routing", group: "scheduling", labelKey: "settingsCenter.navigation.routing.label", descriptionKey: "settingsCenter.navigation.routing.description", sections: ["routing"], keep: ["routing"] },
  { id: "selectors", group: "scheduling", labelKey: "settingsCenter.navigation.selectors.label", descriptionKey: "settingsCenter.navigation.selectors.description", sections: ["routing"], keep: ["profiles", "selectors"] },
  { id: "upstreamPeers", group: "scheduling", labelKey: "settingsCenter.navigation.upstreamPeers.label", descriptionKey: "settingsCenter.navigation.upstreamPeers.description", sections: ["peers", "upstream"] },
  // 安全 (Security)
  { id: "security", group: "security", labelKey: "settingsCenter.navigation.security.label", descriptionKey: "settingsCenter.navigation.security.description", sections: ["security", "integrations"] },
  { id: "audit", group: "security", labelKey: "settingsCenter.navigation.audit.label", descriptionKey: "settingsCenter.navigation.audit.description", sections: ["observability"], keep: ["audit"] },
  // 高级 (Advanced)
  { id: "macros", group: "advanced", labelKey: "settingsCenter.navigation.macros.label", descriptionKey: "settingsCenter.navigation.macros.description", sections: ["advanced"], keep: ["macros"] },
  { id: "advanced", group: "advanced", labelKey: "settingsCenter.navigation.advanced.label", descriptionKey: "settingsCenter.navigation.advanced.description", sections: ["advanced"], keep: ["hooks", "groups", "matrix"] },
];

export const SETTINGS_NAVIGATION_GROUPS = ["basic", "models", "scheduling", "security", "advanced"] as const;

const groupMeta: Record<string, SettingsGroupMeta> = {
  service: { id: "service", titleKey: "settingsCenter.groups.service.title", descriptionKey: "settingsCenter.groups.service.description" },
  lifecycle: { id: "lifecycle", titleKey: "settingsCenter.groups.lifecycle.title", descriptionKey: "settingsCenter.groups.lifecycle.description" },
  sessions: { id: "sessions", titleKey: "settingsCenter.groups.sessions.title", descriptionKey: "settingsCenter.groups.sessions.description" },
  storage: { id: "storage", titleKey: "settingsCenter.groups.storage.title", descriptionKey: "settingsCenter.groups.storage.description" },
  logging: { id: "logging", titleKey: "settingsCenter.groups.logging.title", descriptionKey: "settingsCenter.groups.logging.description" },
  metrics: { id: "metrics", titleKey: "settingsCenter.groups.metrics.title", descriptionKey: "settingsCenter.groups.metrics.description" },
  audit: { id: "audit", titleKey: "settingsCenter.groups.audit.title", descriptionKey: "settingsCenter.groups.audit.description" },
  pricing: { id: "pricing", titleKey: "settingsCenter.groups.pricing.title", descriptionKey: "settingsCenter.groups.pricing.description" },
  modelFiles: { id: "modelFiles", titleKey: "settingsCenter.groups.modelFiles.title", descriptionKey: "settingsCenter.groups.modelFiles.description" },
  downloads: { id: "downloads", titleKey: "settingsCenter.groups.downloads.title", descriptionKey: "settingsCenter.groups.downloads.description" },
  providers: { id: "providers", titleKey: "settingsCenter.groups.providers.title", descriptionKey: "settingsCenter.groups.providers.description" },
  security: { id: "security", titleKey: "settingsCenter.groups.security.title", descriptionKey: "settingsCenter.groups.security.description" },
  integrations: { id: "integrations", titleKey: "settingsCenter.groups.integrations.title", descriptionKey: "settingsCenter.groups.integrations.description" },
  performance: { id: "performance", titleKey: "settingsCenter.groups.performance.title", descriptionKey: "settingsCenter.groups.performance.description" },
  profiles: { id: "profiles", titleKey: "settingsCenter.groups.profiles.title", descriptionKey: "settingsCenter.groups.profiles.description" },
  selectors: { id: "selectors", titleKey: "settingsCenter.groups.selectors.title", descriptionKey: "settingsCenter.groups.selectors.description" },
  routing: { id: "routing", titleKey: "settingsCenter.groups.routing.title", descriptionKey: "settingsCenter.groups.routing.description" },
  peers: { id: "peers", titleKey: "settingsCenter.groups.peers.title", descriptionKey: "settingsCenter.groups.peers.description" },
  upstream: { id: "upstream", titleKey: "settingsCenter.groups.upstream.title", descriptionKey: "settingsCenter.groups.upstream.description" },
  models: { id: "models", titleKey: "settingsCenter.groups.models.title", descriptionKey: "settingsCenter.groups.models.description" },
  macros: { id: "macros", titleKey: "settingsCenter.groups.macros.title", descriptionKey: "settingsCenter.groups.macros.description" },
  advanced: { id: "advanced", titleKey: "settingsCenter.groups.advanced.title", descriptionKey: "settingsCenter.groups.advanced.description" },
  other: { id: "other", titleKey: "settingsCenter.groups.other.title", descriptionKey: "settingsCenter.groups.other.description" },
};

function sourceFor(id: string): SettingsNavigationSource | undefined {
  return settingsNavigation.find((item) => item.id === id);
}

function pathOf(field: SettingsField): string {
  return field.path.replace(/^\//, "");
}

function topLevelOf(field: SettingsField): string {
  return pathOf(field).split("/")[0] ?? "";
}

function matchesFilter(field: SettingsField, source: SettingsNavigationSource): boolean {
  const top = topLevelOf(field);
  if (source.keep && !source.keep.includes(top)) return false;
  if (source.drop && source.drop.includes(top)) return false;
  return true;
}

export function settingsNavigationFor(
  sections: Record<string, SettingsField[]>,
  translate: SettingsTranslator = (key) => key,
): SettingsNavigationItem[] {
  return settingsNavigation
    .filter((item) =>
      item.sections.some((section) => (sections[section] ?? []).some((field) => !field.hidden && matchesFilter(field, item))),
    )
    .map((item) => ({
      id: item.id,
      group: item.group,
      label: translate(item.labelKey),
      description: translate(item.descriptionKey),
      sections: item.sections,
    }));
}

export function fieldsForSettingsNavigation(
  sections: Record<string, SettingsField[]>,
  navigationID: string,
): SettingsField[] {
  const source = sourceFor(navigationID);
  if (!source) return [];
  return source.sections.flatMap((section) =>
    (sections[section] ?? []).filter((field) => !field.hidden && matchesFilter(field, source)),
  );
}

function groupID(navigationID: string, field: SettingsField): string {
  const path = pathOf(field);
  const topLevel = topLevelOf(field);

  if (navigationID === "general") {
    if (topLevel === "ui") return "sessions";
    if (topLevel === "store") return "storage";
    if (["globalTTL", "unloadTimeout", "startPort", "sendLoadingState", "includeAliasesInList"].includes(topLevel)) return "lifecycle";
    return "service";
  }
  if (navigationID === "observability") {
    if (topLevel === "audit") return "audit";
    if (topLevel === "pricing") return "pricing";
    if (["metricsMaxInMemory", "performance"].includes(topLevel)) return "metrics";
    return "logging";
  }
  if (navigationID === "audit") return "audit";
  if (navigationID === "modelFiles") {
    if (path.startsWith("modelFiles/downloads/hf") || path.startsWith("modelFiles/downloads/modelScope")) return "providers";
    if (path.startsWith("modelFiles/downloads")) return "downloads";
    return "modelFiles";
  }
  if (navigationID === "security") return topLevel === "anthropic" ? "integrations" : "security";
  if (navigationID === "performance") return "performance";
  if (navigationID === "routing") return "routing";
  if (navigationID === "selectors") return topLevel === "profiles" ? "profiles" : "selectors";
  if (navigationID === "upstreamPeers") return topLevel === "peers" ? "peers" : "upstream";
  if (navigationID === "macros") return "macros";
  if (navigationID === "models") return "models";
  if (navigationID === "advanced") return "advanced";
  return "other";
}

export function groupSettingsFields(
  navigationID: string,
  fields: SettingsField[],
  translate: SettingsTranslator = (key) => key,
): SettingsFieldGroup[] {
  const grouped = new Map<string, SettingsField[]>();
  for (const field of fields) {
    const id = groupID(navigationID, field);
    grouped.set(id, [...(grouped.get(id) ?? []), field]);
  }
  return [...grouped.entries()].map(([id, groupedFields]) => {
    const meta = groupMeta[id] ?? groupMeta.other;
    return {
      id: meta.id,
      title: translate(meta.titleKey),
      description: translate(meta.descriptionKey),
      fields: groupedFields,
    };
  });
}
