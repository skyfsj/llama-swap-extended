import { describe, expect, it } from "vitest";
import {
  extensionTreeRows,
  fileKind,
  isEntryPoint,
  normalizeDirectoryPath,
  normalizeFilePath,
  removedFilePaths,
  settingSectionOf,
  sortFilePaths,
} from "./extensionFiles";

describe("extensionFiles", () => {
  it("normalizes usable paths", () => {
    expect(normalizeFilePath("lib/util.js")).toBe("lib/util.js");
    expect(normalizeFilePath("./lib/util.js")).toBe("lib/util.js");
    expect(normalizeFilePath("  index.js  ")).toBe("index.js");
  });

  it("normalizes directory names without requiring an extension", () => {
    // A bare name like "1" or "utils" is a legal directory even though it is
    // not a legal file.
    expect(normalizeDirectoryPath("lib")).toBe("lib");
    expect(normalizeDirectoryPath("1")).toBe("1");
    expect(normalizeDirectoryPath("utils-v2/")).toBe("utils-v2");
    expect(normalizeDirectoryPath("../escape")).toBe("");
    expect(normalizeDirectoryPath(".hidden")).toBe("");
    // Files still require a managed extension.
    expect(normalizeFilePath("1")).toBe("");
  });

  it("rejects paths the server would refuse", () => {
    for (const raw of [
      "",
      "   ",
      "../escape.js",
      "lib/../../escape.js",
      "/absolute.js",
      "C:\\index.js",
      "locales\\zh.js",
      "node_modules/pkg/index.js",
      ".hidden.js",
      ".config/util.js",
      "notes.bin",
      "lib",
      "lib/bridge.ts",
    ]) {
      expect(normalizeFilePath(raw), raw).toBe("");
    }
  });

  it("identifies file kinds and the entrypoint", () => {
    expect(fileKind("index.js")).toBe("javascript");
    expect(fileKind("lib/util.mjs")).toBe("javascript");
    expect(fileKind("package.json")).toBe("json");
    expect(fileKind("notes.md")).toBe("text");
    expect(isEntryPoint("index.js")).toBe(true);
    expect(isEntryPoint("./index.js")).toBe(true);
    expect(isEntryPoint("lib/index.js")).toBe(false);
  });

  it("sorts shallow files before nested ones", () => {
    expect(sortFilePaths(["lib/b.js", "index.js", "lib/a.js", "package.json"])).toEqual([
      "index.js",
      "package.json",
      "lib/a.js",
      "lib/b.js",
    ]);
  });

  it("reports removed files", () => {
    expect(removedFilePaths(["index.js", "lib/a.js"], ["index.js"])).toEqual(["lib/a.js"]);
    expect(removedFilePaths(["index.js"], ["index.js"])).toEqual([]);
  });

  it("defaults the settings section", () => {
    expect(settingSectionOf({})).toBe("general");
    expect(settingSectionOf({ section: "  " })).toBe("general");
    expect(settingSectionOf({ section: "auth" })).toBe("auth");
  });
});


describe("extensionTreeRows", () => {
  it("groups shared folders and retains depth when everything is expanded", () => {
    // undefined expands every directory (the legacy default).
    const rows = extensionTreeRows(["lib/a.js", "index.js", "lib/deep/b.js", "lib/a.js"]);
    expect(rows.map((row) => [row.path, row.depth, row.directory])).toEqual([["lib", 0, true], ["lib/deep", 1, true], ["lib/deep/b.js", 2, false], ["lib/a.js", 1, false], ["index.js", 0, false]]);
  });
  it("hides descendants of directories not listed as expanded", () => {
    // Directories start collapsed: only "lib" listed as expanded shows its child.
    expect(extensionTreeRows(["lib/a.js", "index.js"], { lib: true }).map((row) => row.path)).toEqual(["lib", "lib/a.js", "index.js"]);
    expect(extensionTreeRows(["lib/a.js", "index.js"], {}).map((row) => row.path)).toEqual(["lib", "index.js"]);
  });
});
