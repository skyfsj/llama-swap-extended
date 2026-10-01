import { writable } from "svelte/store";

/**
 * Global toast store. Callers push already-translated strings; this module
 * owns stacking, dedupe, timing and pause semantics only.
 *
 * - at most MAX_VISIBLE toasts are shown at once, the rest queue FIFO
 * - identical (level, message) pushes merge into a count badge instead of
 *   stacking duplicates
 * - success/info dismiss after 4s, warning 6s, error 8s; loading stays until
 *   updated or dismissed; hovering or focusing the host pauses every timer
 */

export type ToastLevel = "success" | "info" | "warning" | "error" | "loading";

export interface Toast {
  id: number;
  level: ToastLevel;
  message: string;
  /** merged duplicates; rendered as a ×N badge */
  count: number;
  /** remaining milliseconds; null for a sticky (loading) toast */
  remaining: number | null;
}

const MAX_VISIBLE = 4;
const TICK_MS = 250;

const DURATIONS: Record<ToastLevel, number | null> = {
  success: 4000,
  info: 4000,
  warning: 6000,
  error: 8000,
  loading: null,
};

let nextID = 1;
let paused = false;
const queue: Toast[] = [];
const { subscribe, update } = writable<Toast[]>([]);

// One interval drives every visible timer; it stops itself when nothing is
// left, so an idle app runs no background work.
let interval: ReturnType<typeof setInterval> | undefined;

function startTimer(): void {
  if (interval) return;
  interval = setInterval(() => {
    let active = false;
    update((items) => {
      const kept: Toast[] = [];
      for (const item of items) {
        if (item.remaining === null) {
          active = true;
          kept.push(item);
          continue;
        }
        const remaining = paused ? item.remaining : item.remaining - TICK_MS;
        if (remaining > 0) {
          active = true;
          if (remaining !== item.remaining) kept.push({ ...item, remaining });
          else kept.push(item);
        }
      }
      while (kept.length < MAX_VISIBLE && queue.length > 0) {
        active = true;
        kept.push(queue.shift()!);
      }
      return kept;
    });
    if (!active) {
      clearInterval(interval);
      interval = undefined;
    }
  }, TICK_MS);
}

export interface ToastOptions {
  /** override the level default; 0 keeps the toast sticky */
  duration?: number | null;
}

function push(level: ToastLevel, message: string, options?: ToastOptions): number {
  const duration = options?.duration === undefined ? DURATIONS[level] : options.duration;
  let visibleID = 0;
  update((items) => {
    const existing = items.find((item) => item.level === level && item.message === message);
    if (existing) {
      // Merge identical messages into a count badge rather than duplicating.
      visibleID = existing.id;
      return items.map((item) => (item.id === existing.id ? { ...item, count: item.count + 1 } : item));
    }
    const entry: Toast = { id: nextID++, level, message, count: 1, remaining: duration };
    const visible = items.slice();
    if (visible.length < MAX_VISIBLE) {
      visible.push(entry);
      startTimer();
    } else {
      queue.push(entry);
    }
    visibleID = entry.id;
    return visible;
  });
  return visibleID;
}

function dismiss(id: number): void {
  const queued = queue.findIndex((item) => item.id === id);
  if (queued >= 0) queue.splice(queued, 1);
  update((items) => {
    const visible = items.filter((item) => item.id !== id);
    while (visible.length < MAX_VISIBLE && queue.length > 0) visible.push(queue.shift()!);
    return visible;
  });
}

function updateToast(id: number, patch: { level?: ToastLevel; message?: string; duration?: number | null }): void {
  update((items) =>
    items.map((item) => {
      if (item.id !== id) return item;
      const level = patch.level ?? item.level;
      const remaining = patch.duration !== undefined ? patch.duration : level === item.level ? item.remaining : DURATIONS[level];
      return { ...item, level, message: patch.message ?? item.message, remaining };
    }),
  );
}

export const toast = {
  subscribe,
  success: (message: string, options?: ToastOptions) => push("success", message, options),
  info: (message: string, options?: ToastOptions) => push("info", message, options),
  warning: (message: string, options?: ToastOptions) => push("warning", message, options),
  error: (message: string, options?: ToastOptions) => push("error", message, options),
  loading: (message: string, options?: ToastOptions) => push("loading", message, options),
  update: updateToast,
  dismiss,
  /** drop everything, including the queue (test seam) */
  reset: () => {
    queue.length = 0;
    update(() => []);
    if (interval) {
      clearInterval(interval);
      interval = undefined;
    }
    paused = false;
  },
  /** pause or resume every auto-dismiss timer (hover/focus) */
  setPaused: (value: boolean) => { paused = value; },
  /**
   * Track a promise: a sticky loading toast is swapped to success/error with
   * the resolved message (string or function of the settled value).
   */
  promise<T>(promise: Promise<T>, messages: { loading: string; success: string | ((value: T) => string); error: string | ((reason: unknown) => string) }): Promise<T> {
    const id = push("loading", messages.loading);
    return promise.then(
      (value) => {
        const text = typeof messages.success === "function" ? messages.success(value) : messages.success;
        updateToast(id, { level: "success", message: text, duration: DURATIONS.success });
        return value;
      },
      (reason: unknown) => {
        const text = typeof messages.error === "function" ? messages.error(reason) : messages.error;
        updateToast(id, { level: "error", message: text, duration: DURATIONS.error });
        throw reason;
      },
    );
  },
};
