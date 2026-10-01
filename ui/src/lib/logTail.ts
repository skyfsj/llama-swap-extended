/**
 * Helpers for keeping a bounded log tail without slicing lines in half.
 *
 * A streaming log accumulates far faster than a panel can render it, so every
 * consumer keeps only a tail. `String.slice(-limit)` is the obvious way to do
 * that and it is wrong: it cuts at an arbitrary character offset, so the panel
 * opens mid-token — a startup transcript whose first characters are the tail of
 * a timestamp reads as corrupted output. These helpers cut at the next line
 * boundary instead and record that something was dropped, so a truncated tail
 * is visibly truncated rather than silently malformed.
 */

/** Marks the head of a tail whose earlier output was discarded. */
export const LOG_TRUNCATION_MARKER = "— earlier output truncated —\n";

/**
 * Retention window for every streaming log panel — model, proxy and upstream.
 *
 * These panels render the same underlying bytes: the model monitor keeps the
 * raw lines while the upstream feed is those same lines with a model prefix.
 * Giving them different windows is what made the two views disagree about a
 * single event: the model tab still held the opening lines of a failed start
 * while the upstream tab had already dropped them, so the log looked like it
 * "truncated differently" depending on which tab was open. One shared constant
 * makes the depth a property of the data rather than of the tab.
 *
 * The value must clear a vLLM cold start, which routinely exceeds 200 KB and is
 * exactly the transcript an operator reads when a start fails.
 */
export const LOG_PANEL_LENGTH_LIMIT = 1024 * 512;
/** Per-runtime-operation log limit (install/build output) for the runtime tab. */
export const RUNTIME_LOG_LENGTH_LIMIT = 1024 * 256;

export interface LogTail {
  text: string;
  truncated: boolean;
}

/**
 * Returns at most `limit` characters, starting at a line boundary.
 *
 * `alreadyTruncated` is threaded through so a caller that slices on every
 * incoming chunk reports "truncated" once and keeps reporting it afterwards,
 * even after later slices happen to land on a newline.
 */
export function trimLogTail(text: string, limit: number, alreadyTruncated = false): LogTail {
  if (limit <= 0 || text.length <= limit) {
    return { text, truncated: alreadyTruncated };
  }
  const cut = text.slice(-limit);
  const newline = cut.indexOf("\n");
  // No newline in the retained window means the stream produced one enormous
  // line; keeping it whole would defeat the bound, so keep the raw tail and
  // let the marker explain the missing head.
  return { text: newline >= 0 ? cut.slice(newline + 1) : cut, truncated: true };
}

/**
 * Renders a bounded tail with the marker applied exactly once.
 *
 * Callers keep the unmarked body and this flag; re-running trimLogTail on a
 * rendered string would either duplicate the marker or, worse, trim the marker
 * itself away and lose the explanation.
 */
export function renderLogTail(tail: LogTail): string {
  return tail.truncated ? LOG_TRUNCATION_MARKER + tail.text : tail.text;
}

/**
 * Appends streamed text to an already-rendered tail.
 *
 * `previous` is the rendered string (marker included when applicable), which is
 * what a store holds. The marker is stripped before appending so it cannot be
 * pushed out of the window and leave the surviving tail looking like intact
 * output.
 */
export function appendToRenderedLogTail(
  previous: string,
  incoming: string,
  limit: number,
): string {
  const wasTruncated = previous.startsWith(LOG_TRUNCATION_MARKER);
  const body = wasTruncated ? previous.slice(LOG_TRUNCATION_MARKER.length) : previous;
  return renderLogTail(trimLogTail(body + incoming, limit, wasTruncated));
}
