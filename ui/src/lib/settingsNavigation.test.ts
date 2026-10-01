import { describe, expect, it } from "vitest";
import {
  fieldsForSettingsNavigation,
  groupSettingsFields,
  settingsNavigationFor,
} from "./settingsNavigation";
import type { SettingsField } from "./settingsApi";

function field(path: string, extra: Partial<SettingsField> = {}): SettingsField {
  return {
    path,
    label: path,
    component: "text",
    ...extra,
  };
}

describe("settings navigation", () => {
  it("keeps schema-backed navigation items and omits sections with no visible field", () => {
    const sections = {
      general: [field("/startPort")],
      logging: [field("/logLevel")],
      upstream: [field("/upstream/ignorePaths", { hidden: true })],
    };

    expect(settingsNavigationFor(sections).map((item) => item.id)).toEqual(["general", "observability"]);
  });

  it("keeps runtime definitions in the runtime center instead of settings forms", () => {
    const sections = {
      runtimeManager: [field("/runtimeManager/buildWhileBusy")],
      runtimeDefinitions: [field("/runtimes/vllm")],
      resourceBudget: [field("/resourceBudget/vramMiB")],
    };

    expect(settingsNavigationFor(sections).map((item) => item.id)).not.toContain("runtimes");
  });

  it("derives navigation and group labels through the active locale translator", () => {
    const translate = (key: string) => `translated:${key}`;
    const sections = { general: [field("/startPort")] };

    expect(settingsNavigationFor(sections, translate)[0]).toMatchObject({
      label: "translated:settingsCenter.navigation.general.label",
      description: "translated:settingsCenter.navigation.general.description",
    });
    expect(groupSettingsFields("general", [field("/startPort")], translate)[0]).toMatchObject({
      title: "translated:settingsCenter.groups.lifecycle.title",
      description: "translated:settingsCenter.groups.lifecycle.description",
    });
  });

  it("combines the source sections assigned to a navigation page", () => {
    const sections = {
      logging: [field("/logLevel")],
      observability: [field("/metricsMaxInMemory")],
    };

    expect(fieldsForSettingsNavigation(sections, "observability").map((item) => item.path)).toEqual([
      "/logLevel",
      "/metricsMaxInMemory",
    ]);
  });

  it("splits one schema section across pages using keep/drop top-level filters", () => {
    // observability: logging + metrics, but the audit store is dropped
    const obs = {
      logging: [field("/logLevel")],
      observability: [field("/metricsMaxInMemory"), field("/audit/enabled"), field("/pricing/modelsDev/url")],
    };
    expect(fieldsForSettingsNavigation(obs, "observability").map((f) => f.path)).toEqual([
      "/logLevel",
      "/metricsMaxInMemory",
      "/pricing/modelsDev/url",
    ]);
    // audit: only the audit store, pulled from the same observability section
    expect(fieldsForSettingsNavigation({ observability: [field("/metricsMaxInMemory"), field("/audit/enabled")] }, "audit").map((f) => f.path)).toEqual([
      "/audit/enabled",
    ]);
    // routing: only the router, while profiles/selectors go to the selectors page
    const routing = {
      routing: [field("/routing/router/use"), field("/profiles/coding/pins"), field("/selectors/llama/targets")],
    };
    expect(fieldsForSettingsNavigation(routing, "routing").map((f) => f.path)).toEqual(["/routing/router/use"]);
    expect(fieldsForSettingsNavigation(routing, "selectors").map((f) => f.path)).toEqual([
      "/profiles/coding/pins",
      "/selectors/llama/targets",
    ]);
  });

  it("groups dynamic fields into reference-style cards without changing their paths", () => {
    const fields = [
      field("/modelFiles/maxFiles"),
      field("/modelFiles/downloads/workers"),
      field("/modelFiles/downloads/hf/token"),
    ];

    expect(groupSettingsFields("modelFiles", fields).map((group) => ({
      id: group.id,
      paths: group.fields.map((item) => item.path),
    }))).toEqual([
      { id: "modelFiles", paths: ["/modelFiles/maxFiles"] },
      { id: "downloads", paths: ["/modelFiles/downloads/workers"] },
      { id: "providers", paths: ["/modelFiles/downloads/hf/token"] },
    ]);
  });
});
