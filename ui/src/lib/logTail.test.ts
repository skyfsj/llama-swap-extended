import { describe, expect, it } from "vitest";
import {
  LOG_PANEL_LENGTH_LIMIT,
  LOG_TRUNCATION_MARKER,
  appendToRenderedLogTail,
  renderLogTail,
  trimLogTail,
} from "./logTail";

describe("trimLogTail", () => {
  it("leaves a short log untouched", () => {
    const text = "line one\nline two\n";
    expect(trimLogTail(text, 1000)).toEqual({ text, truncated: false });
  });

  it("drops the oldest content and keeps the newest", () => {
    const text = "OLD-HEAD\n" + "x".repeat(50) + "\nNEWEST\n";
    const result = trimLogTail(text, 30);
    expect(result.truncated).toBe(true);
    expect(result.text).toContain("NEWEST");
    expect(result.text).not.toContain("OLD-HEAD");
  });

  it("starts the retained window on a line boundary", () => {
    // The window lands mid-line; the fragment must not survive as the first
    // visible characters or the panel reads as corrupted output.
    const text = "header line\n" + "y".repeat(40) + "\nlast line\n";
    const result = trimLogTail(text, 25);
    expect(result.text.startsWith("last line")).toBe(true);
    // The surviving fragment of the truncated line is gone entirely.
    expect(result.text.includes("y".repeat(40))).toBe(false);
  });

  it("keeps the raw tail when a single line exceeds the limit", () => {
    // No newline in the window: truncating further would discard the only
    // content there is, so the bound yields to legibility of the marker.
    const text = "z".repeat(500);
    const result = trimLogTail(text, 100);
    expect(result.truncated).toBe(true);
    expect(result.text).toBe("z".repeat(100));
  });

  it("carries an earlier truncation forward", () => {
    const result = trimLogTail("short\n", 1000, true);
    expect(result.truncated).toBe(true);
  });
});

describe("renderLogTail", () => {
  it("marks a truncated tail exactly once", () => {
    const rendered = renderLogTail({ text: "kept\n", truncated: true });
    expect(rendered).toBe(LOG_TRUNCATION_MARKER + "kept\n");
    expect(rendered.split(LOG_TRUNCATION_MARKER).length - 1).toBe(1);
  });

  it("does not mark an untruncated tail", () => {
    expect(renderLogTail({ text: "kept\n", truncated: false })).toBe("kept\n");
  });
});

describe("appendToRenderedLogTail", () => {
  it("appends and keeps a bounded, line-aligned tail", () => {
    let log = "";
    for (let i = 0; i < 40; i++) {
      log = appendToRenderedLogTail(log, `line ${i} payload\n`, 64);
    }
    expect(log.startsWith(LOG_TRUNCATION_MARKER)).toBe(true);
    expect(log).toContain("line 39 payload");
    // The marker survives repeated appends instead of being pushed out of the
    // window, so a truncated panel never looks like complete output.
    expect(log.indexOf(LOG_TRUNCATION_MARKER, 1)).toBe(-1);
    // And the body begins on a boundary, not mid-line.
    const body = log.slice(LOG_TRUNCATION_MARKER.length);
    expect(body.startsWith("line ")).toBe(true);
  });

  it("never duplicates the marker across many appends", () => {
    let log = appendToRenderedLogTail("", "a\n".repeat(100), 32);
    for (let i = 0; i < 20; i++) {
      log = appendToRenderedLogTail(log, "b\n".repeat(10), 32);
    }
    expect(log.split(LOG_TRUNCATION_MARKER).length - 1).toBe(1);
  });
});

describe("log panel consistency", () => {
  // The model tab and the upstream tab render the same bytes — the upstream
  // feed is the model's lines with a model prefix. A cold start that the model
  // tab can show in full must not be missing its opening lines in the upstream
  // tab simply because that panel kept a smaller window.
  const startupTranscript = (() => {
    const lines: string[] = [];
    for (let i = 0; i < 3200; i++) {
      lines.push(`(Worker_TP${i % 4} pid=${1000 + i}) INFO 09-13 08:5${i % 10}:00 step ${i} of model initialization`);
    }
    return lines.join("\n") + "\n";
  })();

  it("is large enough for a cold-start transcript", () => {
    expect(startupTranscript.length).toBeGreaterThan(200 * 1024);
    expect(LOG_PANEL_LENGTH_LIMIT).toBeGreaterThan(startupTranscript.length);
  });

  it("retains the opening line in both the model and upstream views", () => {
    const firstLine = startupTranscript.split("\n")[0];
    // Model path: stream starts empty and accumulates.
    const modelView = appendToRenderedLogTail("", startupTranscript, LOG_PANEL_LENGTH_LIMIT);
    // Upstream path: same lines, model prefix added by the server.
    const prefixed = startupTranscript
      .split("\n")
      .map((line) => (line ? "[Qwen/Qwen3.8-27B-FP8] " + line : line))
      .join("\n");
    const upstreamView = appendToRenderedLogTail("", prefixed, LOG_PANEL_LENGTH_LIMIT);

    expect(modelView).toContain(firstLine);
    expect(upstreamView).toContain("[Qwen/Qwen3.8-27B-FP8] " + firstLine);
    expect(modelView.startsWith(LOG_TRUNCATION_MARKER)).toBe(false);
    expect(upstreamView.startsWith(LOG_TRUNCATION_MARKER)).toBe(false);
  });
});
