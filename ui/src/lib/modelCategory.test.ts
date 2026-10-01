import { describe, it, expect } from "vitest";
import { categoryOf, inCategory, filterByCategory } from "./modelCategory";
import type { Model } from "./types";

function makeModel(overrides: Partial<Model> = {}): Model {
  return {
    id: "test-model",
    state: "ready",
    name: "Test Model",
    description: "",
    unlisted: false,
    disabled: false,
    maintenance: false,
    peerID: "",
    ...overrides,
  };
}

describe("categoryOf", () => {
  it("defaults to chat when no capabilities are declared", () => {
    expect(categoryOf(makeModel())).toBe("chat");
  });

  it("keeps vision models in chat", () => {
    const model = makeModel({ capabilities: { vision: true } });
    expect(categoryOf(model)).toBe("chat");
  });

  it("classifies image generation and image-to-image models as image", () => {
    expect(categoryOf(makeModel({ capabilities: { image_generation: true } }))).toBe("image");
    expect(categoryOf(makeModel({ capabilities: { image_to_image: true } }))).toBe("image");
  });

  it("classifies speech and transcription models", () => {
    expect(categoryOf(makeModel({ capabilities: { audio_speech: true } }))).toBe("speech");
    expect(categoryOf(makeModel({ capabilities: { audio_transcriptions: true } }))).toBe("transcription");
  });

  it("classifies reranker models", () => {
    expect(categoryOf(makeModel({ capabilities: { reranker: true } }))).toBe("rerank");
  });

  it("classifies translation models", () => {
    expect(categoryOf(makeModel({ capabilities: { translation: true } }))).toBe("translation");
  });

  it("prefers the specialized category over chat for mixed capabilities", () => {
    const model = makeModel({
      capabilities: { translation: true, vision: true, function_calling: true },
    });
    expect(categoryOf(model)).toBe("translation");
  });
});

describe("inCategory", () => {
  it("rejects an image model from the chat category", () => {
    const imageModel = makeModel({ capabilities: { image_generation: true } });
    expect(inCategory(imageModel, "image")).toBe(true);
    expect(inCategory(imageModel, "chat")).toBe(false);
    expect(inCategory(imageModel, "translation")).toBe(false);
  });
});

describe("filterByCategory", () => {
  it("returns only models of the requested category", () => {
    const models = [
      makeModel({ id: "a" }),
      makeModel({ id: "b", capabilities: { image_generation: true } }),
      makeModel({ id: "c", capabilities: { translation: true } }),
      makeModel({ id: "d", capabilities: { reranker: true } }),
    ];
    expect(filterByCategory(models, "translation").map((m) => m.id)).toEqual(["c"]);
    expect(filterByCategory(models, "chat").map((m) => m.id)).toEqual(["a"]);
    expect(filterByCategory(models, "speech")).toEqual([]);
  });
});
