import { requestJson } from "./http";

/**
 * Client-side agent loop for the extension debug chat. Each user turn posts the
 * accumulated conversation to the debug chat endpoint, which runs the real
 * inference pipeline with only the debugged extension active; the extension's
 * hook activity comes back through a side-channel artifact fetch.
 */

export interface DebugChatStep {
  kind: "hook" | "tools" | "hostcall";
  hook?: string;
  extension?: string;
  durationMs?: number;
  logs?: string[];
  error?: string;
  changed?: { key: string; before?: unknown; after?: unknown }[];
  injected?: string[];
  input?: unknown;
  output?: unknown;
}

export interface DebugArtifact {
  steps: DebugChatStep[];
  truncated: boolean;
  done: boolean;
}

export async function fetchDebugArtifact(id: string, requestId: string): Promise<DebugArtifact> {
  return requestJson<DebugArtifact>(
    `/api/extensions/${encodeURIComponent(id)}/debug/artifact?request=${encodeURIComponent(requestId)}`,
  );
}

export interface DebugStreamEvent {
  content?: string;
  reasoning?: string;
  /** Assembled client-facing tool call; the debug chat never executes these. */
  toolName?: string;
  toolArguments?: string;
  finish?: string;
  done?: boolean;
}

interface StreamAccumulator {
  content: string;
  reasoning: string;
  finish: string | null;
  tools: Map<number, { name: string; arguments: string }>;
}

/**
 * parseDebugChatStream decodes a chat.completions SSE body, assembling streamed
 * tool_call deltas by index the same way the pipeline's continuation loop does.
 */
export async function* parseDebugChatStream(
  reader: ReadableStreamDefaultReader<Uint8Array>,
): AsyncGenerator<DebugStreamEvent> {
  const decoder = new TextDecoder();
  const state: StreamAccumulator = { content: "", reasoning: "", finish: null, tools: new Map() };
  let buffer = "";
  while (true) {
    const { done, value } = await reader.read();
    if (done) break;
    buffer += decoder.decode(value, { stream: true });
    const lines = buffer.split("\n");
    buffer = lines.pop() || "";
    for (const line of lines) {
      const event = parseDebugChatLine(line, state);
      if (event?.done) {
        yield* assembledToolCalls(state);
        yield event;
        return;
      }
      if (event) yield event;
    }
  }
  if (buffer) {
    const event = parseDebugChatLine(buffer, state);
    if (event?.done) {
      yield* assembledToolCalls(state);
      yield event;
      return;
    }
    if (event) yield event;
  }
  yield* assembledToolCalls(state);
  yield { done: true, finish: state.finish ?? undefined };
}

function* assembledToolCalls(state: StreamAccumulator): Generator<DebugStreamEvent> {
  for (const tool of [...state.tools.entries()].sort((a, b) => a[0] - b[0]).map(([, tool]) => tool)) {
    yield { toolName: tool.name, toolArguments: tool.arguments };
  }
}

function parseDebugChatLine(line: string, state: StreamAccumulator): DebugStreamEvent | null {
  const trimmed = line.trim();
  if (!trimmed.startsWith("data:")) return null;
  const data = trimmed.slice(5).trim();
  if (data === "[DONE]") return { done: true, finish: state.finish ?? undefined };
  let parsed: any;
  try {
    parsed = JSON.parse(data);
  } catch {
    return null;
  }
  const choice = parsed.choices?.[0];
  if (typeof choice?.finish_reason === "string" && choice.finish_reason) state.finish = choice.finish_reason;
  const delta = choice?.delta ?? {};
  let content = "";
  let reasoning = "";
  if (typeof delta.content === "string" && delta.content) {
    state.content += delta.content;
    content = delta.content;
  }
  const reasoningDelta = delta.reasoning_content || delta.reasoning;
  if (typeof reasoningDelta === "string" && reasoningDelta) {
    state.reasoning += reasoningDelta;
    reasoning = reasoningDelta;
  }
  if (Array.isArray(delta.tool_calls)) {
    for (const raw of delta.tool_calls) {
      const index = typeof raw.index === "number" ? raw.index : 0;
      const entry = state.tools.get(index) ?? { name: "", arguments: "" };
      if (typeof raw.function?.name === "string") entry.name += raw.function.name;
      if (typeof raw.function?.arguments === "string") entry.arguments += raw.function.arguments;
      state.tools.set(index, entry);
    }
  }
  if (!content && !reasoning) return null;
  return { content: content || undefined, reasoning: reasoning || undefined };
}

export interface DebugTurnResult {
  text: string;
  reasoning: string;
  finish: string | null;
  clientToolCalls: { name: string; arguments: string }[];
  artifact: DebugArtifact | null;
}

async function debugChatResponse(id: string, request: Record<string, unknown>, signal?: AbortSignal, files?: Record<string, string>): Promise<Response> {
  const response = await fetch(`/api/extensions/${encodeURIComponent(id)}/debug/chat`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ request, files }),
    signal,
  });
  if (!response.ok) {
    const detail = await response.text().catch(() => "");
    throw new Error(`HTTP ${response.status}${detail ? `: ${detail}` : ""}`);
  }
  return response;
}

async function waitForArtifact(id: string, requestId: string, signal?: AbortSignal): Promise<DebugArtifact | null> {
  if (!requestId) return null;
  // The capture is marked done right after the pipeline handler returns, which
  // races the stream's last bytes reaching the client; retry briefly.
  for (let attempt = 0; attempt < 5; attempt++) {
    if (attempt > 0) await new Promise((resolve) => setTimeout(resolve, 200));
    if (signal?.aborted) return null;
    try {
      const artifact = await fetchDebugArtifact(id, requestId);
      if (artifact.done) return artifact;
    } catch {
      return null;
    }
  }
  return null;
}

/** runDebugTurn streams one conversation turn and returns its captured activity. */
export async function runDebugTurn(params: {
  id: string;
  request: Record<string, unknown>;
  /** editor draft source; when set the turn runs the unsaved code live */
  files?: Record<string, string>;
  signal?: AbortSignal;
  onDelta?: (text: string) => void;
  onReasoning?: (text: string) => void;
}): Promise<DebugTurnResult> {
  const response = await debugChatResponse(params.id, params.request, params.signal, params.files);
  const requestId = response.headers.get("X-Extension-Debug-ID") ?? "";
  const reader = response.body?.getReader();
  if (!reader) throw new Error("response body is unreadable");

  let text = "";
  let reasoning = "";
  let finish: string | null = null;
  const clientToolCalls: { name: string; arguments: string }[] = [];
  try {
    for await (const event of parseDebugChatStream(reader)) {
      if (event.done) {
        finish = event.finish ?? finish;
        break;
      }
      if (event.toolName) {
        clientToolCalls.push({ name: event.toolName, arguments: event.toolArguments ?? "" });
        continue;
      }
      if (event.finish) finish = event.finish;
      if (event.content) {
        text += event.content;
        params.onDelta?.(event.content);
      }
      if (event.reasoning) {
        reasoning += event.reasoning;
        params.onReasoning?.(event.reasoning);
      }
    }
  } finally {
    reader.releaseLock();
  }
  const artifact = await waitForArtifact(params.id, requestId, params.signal);
  return { text, reasoning, finish, clientToolCalls, artifact };
}
