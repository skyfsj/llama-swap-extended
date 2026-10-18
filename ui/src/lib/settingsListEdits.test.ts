import { describe, expect, it } from "vitest";
import {
  appendRow,
  listAddOp,
  listRemoveOp,
  listReplaceOp,
  mergeDraftChanges,
  REDACTED,
  removeRow,
  replaceRow,
  secretRows,
} from "./settingsListEdits";

describe("secretRows", () => {
  it("marks scalar entries from the redaction sentinel", () => {
    const rows = secretRows(["[REDACTED]", "${env.API_KEY}"]);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({ kind: "scalar", index: 0, redacted: true, secret: REDACTED });
    expect(rows[1]).toMatchObject({ kind: "scalar", index: 1, redacted: false, secret: "${env.API_KEY}" });
  });

  it("keeps structured entries whole so their name and models survive an edit", () => {
    const entry = { name: "web", key: REDACTED, models: ["a"] };
    const rows = secretRows([entry]);
    expect(rows[0]).toMatchObject({ kind: "entry", index: 0, label: "web", field: "key", redacted: true });
    expect(rows[0].kind === "entry" && rows[0].entry).toBe(entry);
  });

  it("falls back to value/secret aliases when the entry has no key field", () => {
    expect(secretRows([{ value: REDACTED }])[0]).toMatchObject({ kind: "entry", field: "value", redacted: true });
    expect(secretRows([{ secret: "sk-live" }])[0]).toMatchObject({ kind: "entry", field: "secret", redacted: false });
  });

  it("ignores values that are not sequences", () => {
    expect(secretRows("sk-scalar")).toEqual([]);
    expect(secretRows(undefined)).toEqual([]);
  });
});

describe("list operations", () => {
  // Regression: sending the whole masked list back made the server restore
  // secrets by position, so removing one of two redacted keys put the first
  // secret back into the slot the survivor left behind.
  it("addresses one entry at a time", () => {
    expect(listAddOp("/apiKeys", "sk-new")).toEqual({ op: "add", path: "/apiKeys/-", value: "sk-new" });
    expect(listRemoveOp("/apiKeys", 1)).toEqual({ op: "remove", path: "/apiKeys/1" });
    expect(listReplaceOp("/apiKeys", 2, "sk-rotated")).toEqual({ op: "replace", path: "/apiKeys/2", value: "sk-rotated" });
  });

  it("keeps the draft array aligned with the operations", () => {
    const value = ["[REDACTED]", "sk-b"];
    expect(removeRow(value, 0)).toEqual(["sk-b"]);
    expect(replaceRow(value, 1, "sk-new")).toEqual(["[REDACTED]", "sk-new"]);
    expect(appendRow(value, "sk-c")).toEqual(["[REDACTED]", "sk-b", "sk-c"]);
  });
});

describe("mergeDraftChanges", () => {
  // The generated diff replaces a changed top-level key wholesale; a redacted
  // list cannot survive that, so recorded operations take its place.
  it("prefers recorded operations over the wholesale replace for their path", () => {
    const generated = [
      { op: "replace" as const, path: "/logLevel", value: "debug" },
      { op: "replace" as const, path: "/apiKeys", value: ["[REDACTED]", "sk-new"] },
    ];
    const recorded = { "/apiKeys": [listAddOp("/apiKeys", "sk-new")] };
    expect(mergeDraftChanges(generated, recorded)).toEqual([
      { op: "replace", path: "/logLevel", value: "debug" },
      { op: "add", path: "/apiKeys/-", value: "sk-new" },
    ]);
  });

  it("keeps the order the operator worked in", () => {
    const recorded = {
      "/apiKeys": [listRemoveOp("/apiKeys", 0), listReplaceOp("/apiKeys", 0, "sk-new")],
    };
    expect(mergeDraftChanges([], recorded)).toEqual([
      { op: "remove", path: "/apiKeys/0" },
      { op: "replace", path: "/apiKeys/0", value: "sk-new" },
    ]);
  });

  it("ignores a path whose operations cancel out", () => {
    const generated = [{ op: "replace" as const, path: "/apiKeys", value: ["sk-kept"] }];
    expect(mergeDraftChanges(generated, { "/apiKeys": [] })).toEqual(generated);
  });
});
