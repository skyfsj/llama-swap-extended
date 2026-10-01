// AuditConversation byte fields are encoded by Go's JSON encoder as base64
// strings. Keep decoding in a small, browser-safe helper so the route and
// future exports render the same concrete request/response content.
export type AuditBytes = string | number[] | Uint8Array | undefined;

export interface AuditEvent {
  type: string;
  data: unknown;
  text: string;
  finishReason: string;
}

export interface AuditBodyView {
  raw: string;
  formatted: string;
  text: string;
  events: AuditEvent[];
  finishReason: string;
  structured: boolean;
}

export interface AuditToolField {
  name: string;
  value: string;
}

export type AuditTranscriptItem =
  | { kind: "message"; role: "system" | "user" | "assistant"; text: string }
  | { kind: "tool_call"; id: string; name: string; fields: AuditToolField[] }
  | { kind: "tool_result"; id: string; name: string; text: string }
  | { kind: "image"; label: string; dataUrl: string; sizeBytes: number }
  | { kind: "file"; label: string; dataUrl: string; sizeBytes: number };

// formatBytes renders an approximate byte size for media labels.
export function formatBytes(size: number): string {
  if (!Number.isFinite(size) || size <= 0) return "0 B";
  const units = ["B", "KB", "MB", "GB"];
  let value = size;
  let unit = 0;
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024;
    unit++;
  }
  const rounded = unit === 0 ? String(Math.round(value)) : value.toFixed(value >= 100 ? 0 : 1);
  return `${rounded} ${units[unit]}`;
}

// base64ByteSize approximates the decoded size of a base64 payload
// (3 bytes per 4 characters, minus padding).
export function base64ByteSize(payload: string): number {
  const padding = payload.endsWith("==") ? 2 : payload.endsWith("=") ? 1 : 0;
  return Math.max(0, Math.floor((payload.length * 3) / 4) - padding);
}

interface MediaDraft {
  kind: "image" | "file";
  label: string;
  dataUrl: string;
  sizeBytes: number;
}

// mediaFromUrl recognizes a standalone attachment reference: an inline data
// URL or a remote http(s) URL. It is the shared piece behind mediaFromPart and
// behind bare-string tool results, which carry an image as the whole value
// rather than inside a typed part.
function mediaFromUrl(url: string, label?: string): MediaDraft | null {
  const trimmed = url.trim();
  const match = /^data:([\w.+-]+\/([\w.+-]+))?;base64,([A-Za-z0-9+/=]+)$/.exec(trimmed);
  if (match) {
    const size = base64ByteSize(match[3]);
    const mime = match[1] ?? "application/octet-stream";
    const isImage = mime.startsWith("image/");
    return { kind: isImage ? "image" : "file", label: label || mime, dataUrl: trimmed, sizeBytes: size };
  }
  if (/^data:[\w.+-]+\/[\w.+-]+,/.test(trimmed)) {
    const mime = /^data:([\w.+-]+\/[\w.+-]+),/.exec(trimmed)?.[1] ?? "application/octet-stream";
    return { kind: "file", label: label || mime, dataUrl: trimmed, sizeBytes: trimmed.length };
  }
  // Remote references still render as images when the browser can fetch them.
  if (/^https?:\/\//.test(trimmed)) {
    return { kind: "image", label: label || "image", dataUrl: trimmed, sizeBytes: 0 };
  }
  return null;
}

// mediaFromPart recognizes the attachment shapes the audit captures across
// the OpenAI, Anthropic, and Responses payloads: inline data URLs, Anthropic
// base64 sources, and the structured image/file content parts. Non-media
// parts return null so the transcript keeps its text extraction.
function mediaFromPart(part: unknown): MediaDraft | null {
  const item = record(part);
  if (!item) return null;
  const type = nonEmptyString(item.type).toLowerCase();
  const fromUrl = mediaFromUrl;

  if (type === "image_url" || type === "input_image") {
    const url = nonEmptyString(item.image_url)
      || nonEmptyString(record(item.image_url)?.url)
      || nonEmptyString(item.url);
    const draft = url ? fromUrl(url) : null;
    if (draft) return draft;
  }

  if (type === "image" || type === "document") {
    const source = record(item.source);
    if (source && nonEmptyString(source.type) === "base64") {
      const mime = nonEmptyString(source.media_type) || (type === "image" ? "image/png" : "application/octet-stream");
      const data = nonEmptyString(source.data);
      if (data) {
        const dataUrl = `data:${mime};base64,${data}`;
        return { kind: type === "image" ? "image" : "file", label: mime, dataUrl, sizeBytes: base64ByteSize(data) };
      }
    }
  }

  if (type === "file") {
    const file = record(item.file);
    const fileData = nonEmptyString(file?.file_data) || nonEmptyString(item.file_data);
    if (fileData) {
      const draft = fromUrl(fileData, nonEmptyString(file?.filename) || undefined);
      if (draft) return { ...draft, kind: "file", label: nonEmptyString(file?.filename) || draft.label };
    }
  }

  // Generic fallback: any part carrying a data URL in a url/data field.
  for (const key of ["url", "data", "image_url", "file_data"]) {
    const value = item[key];
    if (typeof value !== "string") continue;
    const draft = fromUrl(value);
    if (draft) return draft;
  }
  return null;
}

// mediaItemsFromContent maps the media parts of one content array into
// transcript items; plain text yields nothing.
//
// A bare string is media when it is itself an attachment reference: an
// image-returning tool (view_image and friends) puts the whole data URL in the
// result instead of wrapping it in a typed part. Skipping strings left those
// images rendered as nothing but the "[image/png 1.0 MB]" text placeholder,
// even though the bytes were right there in the capture.
function mediaItemsFromContent(content: unknown): AuditTranscriptItem[] {
  const toItem = (draft: MediaDraft): AuditTranscriptItem => ({
    kind: draft.kind,
    label: draft.label,
    dataUrl: draft.dataUrl,
    sizeBytes: draft.sizeBytes,
  });

  if (typeof content === "string") {
    const draft = mediaFromUrl(content);
    return draft ? [toItem(draft)] : [];
  }
  if (!Array.isArray(content)) return [];

  const items: AuditTranscriptItem[] = [];
  for (const part of content) {
    if (typeof part === "string") {
      const draft = mediaFromUrl(part);
      if (draft) items.push(toItem(draft));
      continue;
    }
    if (typeof part !== "object" || part === null) continue;
    const draft = mediaFromPart(part);
    if (draft) items.push(toItem(draft));
  }
  return items;
}

export function decodeAuditBytes(value: AuditBytes): string {
  if (value === undefined) return "";
  // The streamed body endpoint hands over raw bytes; decoding them directly
  // avoids the copy a Uint8Array would take through the array-like path below.
  if (value instanceof Uint8Array) {
    return new TextDecoder().decode(value);
  }
  if (typeof value !== "string") {
    try {
      return new TextDecoder().decode(new Uint8Array(value));
    } catch {
      return String(value);
    }
  }
  if (value === "") return "";
  try {
    if (typeof atob === "function" && value.length % 4 === 0 && /^[A-Za-z0-9+/]*={0,2}$/.test(value)) {
      const decoded = atob(value);
      const bytes = Uint8Array.from(decoded, (character) => character.charCodeAt(0));
      // A legacy plain string such as "test" is syntactically valid base64
      // but decodes to invalid UTF-8. Fatal decoding keeps that value intact.
      return new TextDecoder("utf-8", { fatal: true }).decode(bytes);
    }
  } catch {
    // Legacy stores may contain plain text that happens to look like base64.
  }
  return value;
}

function record(value: unknown): Record<string, unknown> | null {
  return value && typeof value === "object" && !Array.isArray(value) ? value as Record<string, unknown> : null;
}

function nonEmptyString(value: unknown): string {
  return typeof value === "string" ? value : "";
}

function contentText(value: unknown): string {
  if (typeof value === "string") return value;
  if (!Array.isArray(value)) return "";
  return value.map((entry) => {
    if (typeof entry === "string") return entry;
    const item = record(entry);
    if (!item) return "";
    return nonEmptyString(item.text) || nonEmptyString(item.content) || nonEmptyString(record(item.text)?.value);
  }).join("");
}

function extractText(value: unknown): string {
  const root = record(value);
  if (!root) return typeof value === "string" ? value : "";

  const direct = contentText(root.text) || contentText(root.output_text) || contentText(root.content);
  if (direct) return direct;

  const delta = record(root.delta);
  if (delta) {
    const deltaText = contentText(delta.content) || contentText(delta.text) || contentText(delta.output_text);
    if (deltaText) return deltaText;
  } else if (typeof root.delta === "string") {
    return root.delta;
  }

  const message = record(root.message);
  if (message) {
    const messageText = contentText(message.content) || contentText(message.text);
    if (messageText) return messageText;
  }

  const item = record(root.item);
  if (item) {
    const itemText = contentText(item.content) || contentText(item.text);
    if (itemText) return itemText;
  }

  if (Array.isArray(root.output)) {
    const outputText = root.output.map((entry) => extractText(entry)).join("");
    if (outputText) return outputText;
  }

  if (Array.isArray(root.choices)) {
    return root.choices.map((choice) => {
      const choiceRecord = record(choice);
      if (!choiceRecord) return "";
      return extractText(choiceRecord.delta) || extractText(choiceRecord.message) || contentText(choiceRecord.text);
    }).join("");
  }

  // Some adapters wrap the canonical payload in a data field.
  if (root.data !== undefined && root.data !== value) return extractText(root.data);
  if (root.response !== undefined && root.response !== value) return extractText(root.response);
  return "";
}

function extractFinishReason(value: unknown): string {
  const root = record(value);
  if (!root) return "";
  const direct = nonEmptyString(root.finish_reason) || nonEmptyString(root.stop_reason) || nonEmptyString(root.status);
  if (direct) return direct;
  if (Array.isArray(root.choices)) {
    for (const choice of root.choices) {
      const reason = extractFinishReason(choice);
      if (reason) return reason;
    }
  }
  if (root.data !== undefined && root.data !== value) return extractFinishReason(root.data);
  if (root.response !== undefined && root.response !== value) return extractFinishReason(root.response);
  return "";
}

function parseJSON(value: string): unknown | undefined {
  try {
    return JSON.parse(value);
  } catch {
    return undefined;
  }
}

function parseSSE(raw: string): AuditEvent[] {
  const normalized = raw.replace(/\r\n?/g, "\n");
  if (!/(^|\n)(event|data):/.test(normalized)) return [];
  const events: AuditEvent[] = [];
  let eventType = "message";
  let dataLines: string[] = [];

  const flush = () => {
    if (dataLines.length === 0) {
      eventType = "message";
      return;
    }
    const source = dataLines.join("\n");
    dataLines = [];
    if (source.trim() === "[DONE]") {
      events.push({ type: eventType === "message" ? "done" : eventType, data: source.trim(), text: "", finishReason: "" });
      eventType = "message";
      return;
    }
    const parsed = parseJSON(source);
    const data = parsed === undefined ? source : parsed;
    events.push({
      type: eventType,
      data,
      text: extractText(data),
      finishReason: extractFinishReason(data),
    });
    eventType = "message";
  };

  for (const line of [...normalized.split("\n"), ""]) {
    if (line === "") {
      flush();
      continue;
    }
    if (line.startsWith(":")) continue;
    const separator = line.indexOf(":");
    const field = separator < 0 ? line : line.slice(0, separator);
    let value = separator < 0 ? "" : line.slice(separator + 1);
    if (value.startsWith(" ")) value = value.slice(1);
    if (field === "event") eventType = value || "message";
    if (field === "data") dataLines.push(value);
  }
  return events;
}

function prettyJSON(value: unknown): string {
  try {
    return JSON.stringify(value, null, 2);
  } catch {
    return String(value);
  }
}

export function formatAuditBody(value: AuditBytes): AuditBodyView {
  const raw = decodeAuditBytes(value);
  if (raw.trim() === "") {
    return { raw, formatted: "", text: "", events: [], finishReason: "", structured: false };
  }

  const events = parseSSE(raw);
  if (events.length > 0) {
    const text = events.map((event) => event.text).join("");
    const finishReason = [...events].reverse().find((event) => event.finishReason)?.finishReason ?? "";
    return {
      raw,
      formatted: events.map((event) => `${event.type}\n${prettyJSON(event.data)}`).join("\n\n"),
      text,
      events,
      finishReason,
      structured: true,
    };
  }

  const parsed = parseJSON(raw);
  if (parsed !== undefined) {
    return {
      raw,
      formatted: prettyJSON(parsed),
      text: extractText(parsed),
      events: [],
      finishReason: extractFinishReason(parsed),
      structured: true,
    };
  }

  return { raw, formatted: raw, text: raw, events: [], finishReason: "", structured: false };
}

function contentPartsText(value: unknown): string {
  if (typeof value === "string") return value;
  if (!Array.isArray(value)) return "";
  return value.map((part) => {
    if (typeof part === "string") return part;
    const item = record(part);
    if (!item) return "";
    const type = nonEmptyString(item.type);
    if (type === "tool_use" || type === "tool_result" || type === "function_call" || type === "function_call_output") return "";
    return nonEmptyString(item.text)
      || nonEmptyString(item.content)
      || nonEmptyString(item.output_text)
      || nonEmptyString(record(item.text)?.value);
  }).filter(Boolean).join("\n");
}

function readableValue(value: unknown): string {
  if (typeof value === "string") {
    // A data URL in a tool field or result would otherwise print an
    // unreadable base64 wall; name the attachment instead.
    if (value.startsWith("data:") && value.length > 512) {
      const mime = /^data:([\w.+-]+\/[\w.+-]+);/.exec(value)?.[1] ?? "attachment";
      return `[${mime} ${formatBytes(base64ByteSize(value))}]`;
    }
    return value;
  }
  if (typeof value === "number" || typeof value === "boolean") return String(value);
  if (value === null || value === undefined) return "—";
  const media = mediaFromPart(value);
  if (media) return `[${media.label} ${formatBytes(media.sizeBytes)}]`;
  if (Array.isArray(value)) {
    const values = value.map(readableValue).filter(Boolean);
    return values.length <= 4 ? values.join(" · ") : `${values.slice(0, 4).join(" · ")} · … (${values.length})`;
  }
  const item = record(value);
  if (!item) return String(value);
  const values = Object.entries(item).map(([key, child]) => `${key}: ${readableValue(child)}`);
  return values.length <= 4 ? values.join(" · ") : `${values.slice(0, 4).join(" · ")} · … (${values.length})`;
}

function toolFields(value: unknown): AuditToolField[] {
  if (typeof value === "string") {
    const parsed = parseJSON(value);
    if (parsed === undefined) return value.trim() ? [{ name: "input", value }] : [];
    return toolFields(parsed);
  }
  const item = record(value);
  if (!item) return value === undefined ? [] : [{ name: "input", value: readableValue(value) }];
  return Object.entries(item).map(([name, child]) => ({ name, value: readableValue(child) }));
}

function toolCall(value: Record<string, unknown>): AuditTranscriptItem | null {
  const fn = record(value.function);
  const name = nonEmptyString(fn?.name) || nonEmptyString(value.name);
  if (!name) return null;
  const input = fn?.arguments ?? value.arguments ?? value.input ?? value.parameters;
  return {
    kind: "tool_call",
    id: nonEmptyString(value.id) || nonEmptyString(value.call_id),
    name,
    fields: toolFields(input),
  };
}

function toolResult(value: Record<string, unknown>): AuditTranscriptItem | null {
  const text = contentPartsText(value.content) || nonEmptyString(value.output) || nonEmptyString(value.result) || nonEmptyString(value.text);
  if (!text && value.content === undefined && value.output === undefined && value.result === undefined) return null;
  return {
    kind: "tool_result",
    id: nonEmptyString(value.tool_call_id) || nonEmptyString(value.call_id) || nonEmptyString(value.id),
    name: nonEmptyString(value.name),
    text: text || readableValue(value.content ?? value.output ?? value.result),
  };
}

function messageText(value: Record<string, unknown>): string {
  return contentPartsText(value.content)
    || nonEmptyString(value.content)
    || nonEmptyString(value.text)
    || nonEmptyString(value.output_text);
}

function appendContentTools(items: AuditTranscriptItem[], content: unknown): void {
  if (!Array.isArray(content)) return;
  for (const part of content) {
    const item = record(part);
    if (!item) continue;
    const type = nonEmptyString(item.type);
    if (type === "tool_use" || type === "function_call") {
      const call = toolCall(item);
      if (call) items.push(call);
    } else if (type === "tool_result" || type === "function_call_output") {
      const result = toolResult(item);
      if (result) items.push(result);
      items.push(...mediaItemsFromContent(item.content));
    }
  }
}

function appendMessage(items: AuditTranscriptItem[], value: unknown): void {
  const item = record(value);
  if (!item) return;
  const type = nonEmptyString(item.type);
  if (type === "tool_use" || type === "function_call") {
    const call = toolCall(item);
    if (call) items.push(call);
    return;
  }
  if (type === "tool_result" || type === "function_call_output") {
    const result = toolResult(item);
    if (result) items.push(result);
    items.push(...mediaItemsFromContent(item.content));
    return;
  }

  const roleValue = nonEmptyString(item.role);
  const role = roleValue === "system" || roleValue === "developer"
    ? "system"
    : roleValue === "assistant"
      ? "assistant"
      : roleValue === "user"
        ? "user"
        : "";
  const text = messageText(item).trim();
  if (role && text) items.push({ kind: "message", role, text });

  // Attachments ride next to their message: image parts become renderable
  // media items, other base64 payloads become downloadable file items.
  items.push(...mediaItemsFromContent(item.content));

  const calls = Array.isArray(item.tool_calls) ? item.tool_calls : [];
  for (const candidate of calls) {
    const call = toolCall(record(candidate) ?? {});
    if (call) items.push(call);
  }
  if (roleValue === "tool") {
    const result = toolResult(item);
    if (result) items.push(result);
  }
  appendContentTools(items, item.content);
}

// auditRequestTranscript extracts the chat-shaped portions of OpenAI,
// Anthropic, and Responses API requests. Other endpoint payloads intentionally
// return no items and remain available through the raw-data disclosure.
export function auditRequestTranscript(value: AuditBytes): AuditTranscriptItem[] {
  const parsed = parseJSON(decodeAuditBytes(value));
  const root = record(parsed);
  if (!root) return [];

  const items: AuditTranscriptItem[] = [];
  const system = contentPartsText(root.system) || nonEmptyString(root.system) || contentPartsText(root.instructions) || nonEmptyString(root.instructions);
  if (system) items.push({ kind: "message", role: "system", text: system });

  const messages = Array.isArray(root.messages) ? root.messages : Array.isArray(root.input) ? root.input : [];
  for (const message of messages) appendMessage(items, message);
  return items;
}

function toolIdentity(item: AuditTranscriptItem): string {
  if (item.kind === "message" || item.kind === "image" || item.kind === "file") return "";
  return item.kind === "tool_call"
    ? `call:${item.id}:${item.name}:${item.fields.map((field) => `${field.name}=${field.value}`).join("|")}`
    : `result:${item.id}:${item.name}:${item.text}`;
}

function collectToolItems(value: unknown, items: AuditTranscriptItem[], seen: Set<string>): void {
  if (Array.isArray(value)) {
    for (const entry of value) collectToolItems(entry, items, seen);
    return;
  }
  const item = record(value);
  if (!item) return;
  const type = nonEmptyString(item.type);
  const candidate = type === "tool_use" || type === "function_call"
    ? toolCall(item)
    : type === "tool_result" || type === "function_call_output"
      ? toolResult(item)
      : null;
  if (!candidate && nonEmptyString(item.role) === "tool") {
    // A chat-completions tool message carries its attachments in content;
    // the tool result itself stays in the text extraction.
    items.push(...mediaItemsFromContent(item.content));
  }
  if (candidate) {
    const id = toolIdentity(candidate);
    if (!seen.has(id)) {
      seen.add(id);
      items.push(candidate);
    }
    if (candidate.kind === "tool_result") {
      items.push(...mediaItemsFromContent(item.content));
    }
  }

  if (Array.isArray(item.tool_calls)) {
    for (const entry of item.tool_calls) {
      const call = toolCall(record(entry) ?? {});
      if (!call) continue;
      const id = toolIdentity(call);
      if (!seen.has(id)) {
        seen.add(id);
        items.push(call);
      }
    }
  }
  if (item.function_call !== undefined) collectToolItems(item.function_call, items, seen);
  if (item.tool_use !== undefined) collectToolItems(item.tool_use, items, seen);
  if (item.function_call_output !== undefined) collectToolItems(item.function_call_output, items, seen);
  if (item.tool_result !== undefined) collectToolItems(item.tool_result, items, seen);
  for (const key of ["output", "item", "message", "choices", "delta", "content", "data", "response"]) {
    if (item[key] !== undefined) collectToolItems(item[key], items, seen);
  }
}

// auditResponseTranscript coalesces streamed assistant deltas into one message
// and lists structured tool calls/results as separate, readable entries.
export function auditResponseTranscript(value: AuditBytes): AuditTranscriptItem[] {
  const view = formatAuditBody(value);
  const items: AuditTranscriptItem[] = [];
  if (view.text.trim()) items.push({ kind: "message", role: "assistant", text: view.text });

  const tools: AuditTranscriptItem[] = [];
  const seen = new Set<string>();
  const parsed = parseJSON(view.raw);
  if (parsed !== undefined) collectToolItems(parsed, tools, seen);
  for (const event of view.events) collectToolItems(event.data, tools, seen);
  return [...items, ...tools];
}

export function auditTranscript(request: AuditBytes, response: AuditBytes): AuditTranscriptItem[] {
  return [...auditRequestTranscript(request), ...auditResponseTranscript(response)];
}

// collapseDataUrls replaces inline base64 payloads longer than a short
// threshold with a sized placeholder, so the raw-data view of a multimodal
// capture stays readable. The full bytes remain available through the
// transcript's rendered image/file items.
export function collapseDataUrls(text: string): string {
  return text.replace(/data:([\w.+-]+\/[\w.+-]+);base64,([A-Za-z0-9+/=]{256,})/g, (_match, mime: string, payload: string) => {
    return `data:${mime};base64,[${formatBytes(base64ByteSize(payload))}]`;
  });
}

// fetchAuditConversationBody streams one stored transcript body. The detail view
// publishes only a reference and a size, so a conversation with attachments
// opens on its metadata immediately and its bodies arrive afterwards — base64
// inflation included in neither the first response nor the DOM.
//
// The conversation is addressed by activity ID when one is present, matching the
// detail view (the server accepts either).
export async function fetchAuditConversationBody(
  conversationID: string,
  which: "request" | "response",
): Promise<Uint8Array> {
  const response = await fetch(
    `/api/audit/conversations/${encodeURIComponent(conversationID)}/body/${which}`,
  );
  if (!response.ok) {
    throw new Error(`HTTP ${response.status}`);
  }
  return new Uint8Array(await response.arrayBuffer());
}
