import { describe, expect, it } from "vitest";
import {
  isOfficialRuntimeTemplate,
  readinessLevel,
  runtimeConfigForTemplate,
  runtimeEnvironmentMissing,
  runtimeModelCompatibility,
  runtimeTemplateNameSuggestion,
  runtimeTemplates,
  runtimeWizardFirstStep,
  runtimeWizardValidation,
  sourceBoundaryForPaths,
} from "./runtimeCenter";

describe("runtime control center", () => {
  it("offers source-neutral templates without baking in a vendor repository", () => {
    expect(runtimeTemplates.map((template) => template.id)).toEqual([
      "vllm-official",
      "llamacpp-official",
      "llamacpp-source",
      "vllm-wheel",
      "vllm-git",
      "container",
    ]);
    expect(JSON.stringify(runtimeTemplates)).not.toContain("1Cat");
    expect(JSON.stringify(runtimeTemplates)).not.toContain("llamacpp-bundled");
  });

  it("defaults official presets to upstream sources that need no user input", () => {
    const vllm = runtimeConfigForTemplate("vllm-official");
    expect(vllm.source).toEqual({ type: "pypi" });
    expect(runtimeWizardValidation("vllm-official", vllm)).toEqual([]);
    const llama = runtimeConfigForTemplate("llamacpp-official");
    expect(llama.source).toEqual({ type: "git", repository: "https://github.com/ggml-org/llama.cpp", trackRef: "main" });
    expect(runtimeWizardValidation("llama-official", llama)).toEqual([]);
    expect(isOfficialRuntimeTemplate("vllm-official")).toBe(true);
    expect(isOfficialRuntimeTemplate("llamacpp-official")).toBe(true);
    expect(isOfficialRuntimeTemplate("vllm-wheel")).toBe(false);
  });

  it("suggests a runtime name for every template so presets need no other input", () => {
    expect(runtimeTemplateNameSuggestion("vllm-official")).toBe("vllm");
    expect(runtimeTemplateNameSuggestion("llamacpp-official")).toBe("llamacpp");
    expect(runtimeTemplateNameSuggestion("llamacpp-source")).toBe("llamacpp");
    expect(runtimeTemplateNameSuggestion("vllm-wheel")).toBe("vllm");
    expect(runtimeTemplateNameSuggestion("vllm-git")).toBe("vllm");
    expect(runtimeTemplateNameSuggestion("container")).toBe("vllm");
    expect(runtimeTemplateNameSuggestion("clone")).toBe("");
    for (const template of runtimeTemplates) {
      expect(template.defaultName).toMatch(/^[A-Za-z0-9][A-Za-z0-9._-]*$/);
    }
  });

  it("treats a missing environment as not installed even when a version is recorded", () => {
    expect(runtimeEnvironmentMissing({ name: "a", state: "DEGRADED", lastError: "vLLM runtime has no .venv" })).toBe(true);
    expect(runtimeEnvironmentMissing({ name: "a", state: "DEGRADED", lastError: "build path is not a real directory" })).toBe(true);
    expect(runtimeEnvironmentMissing({ name: "a", state: "DEGRADED", lastError: "active runtime manifest is missing" })).toBe(true);
    expect(runtimeEnvironmentMissing({ name: "a", state: "ACTIVE", lastError: "healthcheck failed" })).toBe(false);
    expect(runtimeEnvironmentMissing({ name: "a", state: "ACTIVE" })).toBe(false);
  });

  it("uses safe automatic idle-gated rollback defaults", () => {
    expect(runtimeConfigForTemplate("vllm-wheel").update).toMatchObject({
      policy: "automatic",
      activateOnlyWhenIdle: true,
      keepVersions: 2,
      rollbackOnFailure: true,
    });
  });

  it("blocks missing source fields before the settings transaction", () => {
    expect(runtimeWizardValidation("demo", runtimeConfigForTemplate("vllm-git"))).toContain("repository-required");
    expect(runtimeWizardValidation("demo", runtimeConfigForTemplate("container"))).toContain("image-required");
    expect(runtimeWizardValidation("demo", runtimeConfigForTemplate("vllm-wheel"))).toContain("wheel-path-required");
    expect(runtimeWizardValidation("demo", { kind: "llamacpp", mode: "native", source: { type: "release" } })).toContain("source-url-required");
    expect(runtimeWizardValidation("demo", { kind: "llamacpp", mode: "native", source: { type: "local" } })).toContain("local-source-required");
  });

  it("keeps legacy cmd and backend kind conflicts out of the binding step", () => {
    const config = runtimeConfigForTemplate("vllm-wheel");
    expect(runtimeModelCompatibility(config, { type: "llamacpp", args: ["serve"] }).compatible).toBe(false);
    expect(runtimeModelCompatibility(config, { type: "vllm", cmd: "python server.py", args: ["serve"] }).reason).toContain("legacy cmd");
    expect(runtimeModelCompatibility(config, { type: "vllm", args: ["serve"] }).compatible).toBe(true);
  });

  it("prioritizes blocking readiness over warnings and passes", () => {
    expect(readinessLevel([{ code: "w", level: "warning", title: "", detail: "" }])).toBe("warning");
    expect(readinessLevel([
      { code: "p", level: "pass", title: "", detail: "" },
      { code: "b", level: "block", title: "", detail: "" },
    ])).toBe("block");
  });

  it("blocks a wizard that would span different settings sources", () => {
    const snapshot = {
      ownership: { "/runtimes": "managed", "/models": "models.yaml" },
      sources: [
        { path: "managed", writable: true, managed: true },
        { path: "models.yaml", writable: true, managed: false },
      ],
    };
    expect(sourceBoundaryForPaths(snapshot, ["/runtimes", "/models"]).blocked).toBe(true);
  });

  it("blocks a wizard that would edit a read-only source", () => {
    const snapshot = {
      ownership: { "/runtimes": "distribution.yaml" },
      sources: [
        { path: "distribution.yaml", writable: false, managed: false },
        { path: "managed.yaml", writable: true, managed: true },
      ],
    };
    expect(sourceBoundaryForPaths(snapshot, ["/runtimes"]).blocked).toBe(true);
  });
});

describe("runtimeWizardFirstStep", () => {
  it("starts editing on the definition step", () => {
    // An existing runtime has no template to pick, so the operator must not
    // be sent through the template picker (and its "copy this runtime" radio)
    // just to change one source field.
    expect(runtimeWizardFirstStep("edit")).toBe(1);
  });

  it("starts creating and cloning from the template picker", () => {
    expect(runtimeWizardFirstStep("create")).toBe(0);
    expect(runtimeWizardFirstStep("clone")).toBe(0);
  });
});
