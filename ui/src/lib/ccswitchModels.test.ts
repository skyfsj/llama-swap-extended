import { describe, expect, it } from "vitest";
import { modelGroupAllowed, selectableModelNames, type ModelGroupLike } from "./ccswitchModels";

const groups: ModelGroupLike[] = [
  { id: "alpha-chat", aliases: ["alpha"] },
  { id: "fake-image", aliases: ["fake-img", "image-alias"] },
  { id: "gamma-chat" },
];

describe("modelGroupAllowed", () => {
  it("allows every group when the key has no allowlist", () => {
    for (const group of groups) {
      expect(modelGroupAllowed(group, [])).toBe(true);
    }
  });

  it("allows a group named by its own id", () => {
    expect(modelGroupAllowed(groups[0], ["alpha-chat"])).toBe(true);
    expect(modelGroupAllowed(groups[2], ["alpha-chat"])).toBe(false);
  });

  it("allows a group named by any of its aliases", () => {
    expect(modelGroupAllowed(groups[1], ["fake-img"])).toBe(true);
    expect(modelGroupAllowed(groups[1], ["image-alias"])).toBe(true);
    expect(modelGroupAllowed(groups[1], ["gamma-chat"])).toBe(false);
  });
});

describe("selectableModelNames", () => {
  it("offers every spelling of a permitted group", () => {
    expect(selectableModelNames(groups, ["alpha-chat"])).toEqual(["alpha", "alpha-chat"]);
  });

  it("offers the whole group when the key names an alias", () => {
    expect(selectableModelNames(groups, ["image-alias"])).toEqual(["fake-image", "fake-img", "image-alias"]);
  });

  it("offers every model for an unrestricted key, sorted for display", () => {
    expect(selectableModelNames(groups, [])).toEqual([
      "alpha",
      "alpha-chat",
      "fake-image",
      "fake-img",
      "gamma-chat",
      "image-alias",
    ]);
  });

  it("offers nothing when the allowlist matches no configured model", () => {
    expect(selectableModelNames(groups, ["deleted-model"])).toEqual([]);
  });
});
