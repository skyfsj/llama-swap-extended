import { getActivity } from "../stores/api";
import type { ActivityPage } from "../lib/types";

// The Activity page mounts several panels over the same request records: the
// overview, the requests tab and the flat "all" view each render a
// RequestRecords table. They used to fetch independently, so opening the page
// issued three identical requests, and every filter or page change multiplied
// that by three again.
//
// This module keeps one in-flight request per parameter set. Panels that ask for
// the same records share it, so N panels produce one HTTP request.

export interface ActivityRecordsKey {
  modelId: string;
  page: number;
  limit: number;
  sort: string;
  order: "asc" | "desc";
  filters: unknown;
}

// Only the newest request per key is tracked, and it is dropped once it settles:
// request records change constantly, and serving a page from a cache older than
// the last refresh would show rows that no longer exist.
const inflight = new Map<string, Promise<ActivityPage>>();

/**
 * The key of one records request. Everything the server answer depends on is
 * part of it, so two panels asking for different pages or filters do not
 * collide, while two panels asking for the same page do.
 */
export function activityRecordsKey(key: ActivityRecordsKey): string {
  return JSON.stringify([
    key.modelId,
    key.page,
    key.limit,
    key.sort,
    key.order,
    key.filters,
  ]);
}

/**
 * Fetches a page of request records, reusing the request already running for the
 * same key.
 *
 * A panel that mounts while a request is in flight gets that request's promise
 * rather than starting its own. The caller still owns its own staleness guard
 * (a request id compared after the await), because a shared promise resolves for
 * every subscriber at once.
 */
export function fetchActivityRecords(
  key: string,
  params: Parameters<typeof getActivity>[0],
  request: (params: Parameters<typeof getActivity>[0]) => Promise<ActivityPage> = getActivity,
): Promise<ActivityPage> {
  const running = inflight.get(key);
  if (running) return running;
  const pending = request(params).finally(() => {
    // Only the promise this call installed is removed: a newer request for the
    // same key must not be cleared by an older one settling.
    if (inflight.get(key) === pending) inflight.delete(key);
  });
  inflight.set(key, pending);
  return pending;
}
