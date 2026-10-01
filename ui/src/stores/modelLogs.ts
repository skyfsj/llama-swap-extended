import { writable, type Readable } from "svelte/store";
import { t } from "../lib/i18n";
import {
  LOG_PANEL_LENGTH_LIMIT,
  renderLogTail,
  trimLogTail,
  type LogTail,
} from "../lib/logTail";

const INITIAL_RETRY_DELAY_MS = 250;
const MAX_RETRY_DELAY_MS = 5000;

function waitForRetry(delayMs: number, signal: AbortSignal): Promise<boolean> {
  if (signal.aborted) return Promise.resolve(false);

  return new Promise((resolve) => {
    const timer = setTimeout(() => {
      signal.removeEventListener("abort", onAbort);
      resolve(true);
    }, delayMs);
    const onAbort = () => {
      clearTimeout(timer);
      signal.removeEventListener("abort", onAbort);
      resolve(false);
    };
    signal.addEventListener("abort", onAbort, { once: true });
  });
}

function readLogChunk(
  reader: ReadableStreamDefaultReader<Uint8Array>,
  signal: AbortSignal,
): Promise<ReadableStreamReadResult<Uint8Array> | null> {
  return new Promise((resolve, reject) => {
    const onAbort = () => {
      cleanup();
      void reader.cancel().catch(() => undefined);
      resolve(null);
    };
    const cleanup = () => signal.removeEventListener("abort", onAbort);

    signal.addEventListener("abort", onAbort, { once: true });
    if (signal.aborted) {
      onAbort();
      return;
    }

    reader.read().then(
      (result) => {
        cleanup();
        resolve(result);
      },
      (error: unknown) => {
        cleanup();
        reject(error);
      },
    );
  });
}

/**
 * Stream a model's log tail by opening a long-lived fetch to
 * GET /logs/stream/{modelId} and accumulating text into a store. The
 * returned store is Readable: callers never write to it. The stream is
 * closed automatically when the last subscriber unsubscribes and reconnects
 * when the server, browser, or a reverse proxy closes the long-lived request.
 */
export function streamModelLog(modelId: string): Readable<string> {
  const store = writable<string>("");
  let subscriberCount = 0;
  let generation = 0;
  let controller: AbortController | null = null;

  async function run(runGeneration: number, signal: AbortSignal): Promise<void> {
    let retryDelay = INITIAL_RETRY_DELAY_MS;
    let connected = false;
    const isActive = () => subscriberCount > 0 && generation === runGeneration && !signal.aborted;


    while (isActive()) {
      let receivedData = false;
      try {
        const res = await fetch(`/logs/stream/${encodeURIComponent(modelId)}`, {
          method: "GET",
          cache: "no-store",
          credentials: "same-origin",
          signal,
        });
        if (!res.ok || !res.body) {
          if (!connected && isActive()) {
            store.set(`${t("errors.failedLoadLogs", { status: res.status })}\n`);
          }
        } else {
          connected = true;
          // Every connection starts with a complete history snapshot. Replace
          // the previous attempt instead of appending it, otherwise a retry
          // duplicates the whole 100KB tail in the panel.
          // Keep the tail, not a raw character slice: slicing at an arbitrary
          // offset leaves the panel opening mid-line, which is what made a
          // truncated startup transcript read as corrupted output.
          let tail: LogTail = { text: "", truncated: false };
          store.set("");
          const reader = res.body.getReader();
          const decoder = new TextDecoder();

          while (isActive()) {
            const result = await readLogChunk(reader, signal);
            if (result === null || result.done) break;
            tail = trimLogTail(
              tail.text + decoder.decode(result.value, { stream: true }),
              LOG_PANEL_LENGTH_LIMIT,
              tail.truncated,
            );
            receivedData = true;
            if (isActive()) store.set(renderLogTail(tail));
          }

          if (isActive()) {
            const flushed = decoder.decode();
            if (flushed) {
              tail = trimLogTail(tail.text + flushed, LOG_PANEL_LENGTH_LIMIT, tail.truncated);
              store.set(renderLogTail(tail));
            }
          }
        }
      } catch (err) {
        if (!isActive()) return;
        console.warn(`Log stream for ${modelId} closed; retrying:`, err);
        if (!connected) {
          store.set(`${t("errors.failedStreamLogs", { message: String(err) })}\n`);
        }
      }

      if (!isActive()) return;
      const delay = connected || receivedData ? INITIAL_RETRY_DELAY_MS : retryDelay;
      if (!(await waitForRetry(delay, signal))) return;
      retryDelay = connected || receivedData
        ? INITIAL_RETRY_DELAY_MS
        : Math.min(retryDelay * 2, MAX_RETRY_DELAY_MS);
    }
  }

  return {
    subscribe(sub: (v: string) => void) {
      subscriberCount++;
      const unsub = store.subscribe(sub);
      if (subscriberCount === 1) {
        const runGeneration = ++generation;
        controller = new AbortController();
        void run(runGeneration, controller.signal);
      }

      let activeSubscription = true;
      return () => {
        if (!activeSubscription) return;
        activeSubscription = false;
        unsub();
        subscriberCount--;
        if (subscriberCount === 0) {
          generation++;
          controller?.abort();
          controller = null;
          store.set("");
        }
      };
    },
  };
}
