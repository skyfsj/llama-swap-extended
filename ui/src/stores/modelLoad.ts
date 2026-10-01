import { writable } from "svelte/store";
import { loadModel, unloadSingleModel } from "./api";
import type { Model } from "../lib/types";

export const pendingLoads = writable<Record<string, boolean>>({});
const loadControllers = new Map<string, AbortController>();

export async function handleLoadModel(id: string): Promise<void> {
  if (isPending(id)) return;

  const controller = new AbortController();
  loadControllers.set(id, controller);
  pendingLoads.update((p) => ({ ...p, [id]: true }));
  try {
    await loadModel(id, controller.signal);
  } catch (e) {
    console.error(e);
  } finally {
    loadControllers.delete(id);
    pendingLoads.update((p) => {
      const next = { ...p };
      delete next[id];
      return next;
    });
  }
}

export function cancelLoad(id: string): void {
  loadControllers.get(id)?.abort();
}

export function isPending(id: string): boolean {
  let val = false;
  pendingLoads.subscribe((p) => (val = !!p[id]))();
  return val;
}

export function onToggleLoad(m: Model): void {
  if (m.state === "stopped" && isPending(m.id)) {
    cancelLoad(m.id);
    // Aborting the load fetch does not reach the backend: the swap goroutine
    // waits on the server's own context, not the request's. Ask for an unload
    // as well so a start that already crossed into the backend is aborted; on
    // a backend that has not started yet the unload is a harmless no-op.
    void unloadSingleModel(m.id).catch(() => {});
  } else if (m.state === "stopped") {
    void handleLoadModel(m.id);
  } else if (m.state === "ready" || m.state === "sleeping") {
    void unloadSingleModel(m.id);
  }
}

export function statusDotColor(m: Model | undefined): string {
  if (!m) return "bg-muted-foreground/40";
  if (m.state === "ready") return "bg-success";
  if (m.state === "sleeping") return "bg-info";
  if (m.state === "starting" || m.state === "stopping") return "bg-warning";
  return "bg-muted-foreground/40";
}
