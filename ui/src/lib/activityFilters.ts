// Filter state for the activity table's filter drawer, kept separate from the
// component so the query-param building is unit testable.

export interface ActivityFilters {
  // Kept as strings so a cleared number input stays empty rather than 0.
  minID: string;
  maxID: string;
  model: string;
  keyID: string;
  sessionID: string;
  range: ActivityTimeRange;
  start: string;
  end: string;
}

export type ActivityTimeRange = "all" | "hour" | "day" | "week" | "month" | "custom";

const ACTIVITY_RANGE_DURATIONS: Record<Exclude<ActivityTimeRange, "all" | "custom">, number> = {
  hour: 60 * 60 * 1000,
  day: 24 * 60 * 60 * 1000,
  week: 7 * 24 * 60 * 60 * 1000,
  month: 30 * 24 * 60 * 60 * 1000,
};

export function emptyActivityFilters(): ActivityFilters {
  return { minID: "", maxID: "", model: "", keyID: "", sessionID: "", range: "all", start: "", end: "" };
}

/**
 * normalizeActivityFilters coerces an unknown value (typically JSON restored
 * from localStorage, possibly written by an older build) into a valid
 * ActivityFilters. Unrecognized or wrongly typed fields fall back to empty.
 */
export function normalizeActivityFilters(raw: unknown): ActivityFilters {
  const filters = emptyActivityFilters();
  if (typeof raw !== "object" || raw === null) return filters;

  const source = raw as Record<string, unknown>;
  for (const key of ["minID", "maxID", "model", "keyID", "sessionID", "start", "end"] as const) {
    if (typeof source[key] === "string") filters[key] = source[key] as string;
  }
  if (typeof source.range === "string" && isActivityTimeRange(source.range)) {
    filters.range = source.range;
  }
  return filters;
}

function isActivityTimeRange(value: string): value is ActivityTimeRange {
  return value === "all" || value === "hour" || value === "day" || value === "week" || value === "month" || value === "custom";
}

/** activeFilterCount counts how many filter fields are set. */
export function activeFilterCount(filters: ActivityFilters): number {
  let count = 0;
  if (filters.minID !== "") count++;
  if (filters.maxID !== "") count++;
  if (filters.model !== "") count++;
  if (filters.keyID !== "") count++;
  if (filters.sessionID !== "") count++;
  if (filters.range !== "all") {
    count++;
  } else {
    if (filters.start !== "") count++;
    if (filters.end !== "") count++;
  }
  return count;
}

export function hasActiveFilters(filters: ActivityFilters): boolean {
  return activeFilterCount(filters) > 0;
}

// toPositiveInt keeps out blank, non-numeric and out-of-range ids so the API
// never has to reject input the UI could have caught.
function toPositiveInt(value: string): number | null {
  if (value.trim() === "") return null;
  const parsed = Number(value);
  if (!Number.isInteger(parsed) || parsed < 1) return null;
  return parsed;
}

/**
 * appendActivityFilters writes the set filters onto a URLSearchParams as the
 * params /api/metrics/activity accepts. A caller-pinned model remains
 * authoritative because getActivity writes it before this helper runs.
 */
export function appendActivityFilters(query: URLSearchParams, filters: ActivityFilters, now = new Date()): void {
  if (filters.model.trim() !== "" && !query.has("model")) query.set("model", filters.model.trim());
  if (filters.keyID.trim() !== "") query.set("key_id", filters.keyID.trim());
  if (filters.sessionID.trim() !== "") query.set("session_id", filters.sessionID.trim());
  let startValue = filters.start;
  let endValue = filters.end;
  if (filters.range !== "all" && filters.range !== "custom") {
    const duration = ACTIVITY_RANGE_DURATIONS[filters.range];
    if (duration) {
      startValue = new Date(now.getTime() - duration).toISOString();
      endValue = now.toISOString();
    }
  }
  const start = toRFC3339(startValue);
  if (start !== null) query.set("start", start);
  const end = toRFC3339(endValue);
  if (end !== null) query.set("end", end);

  const minID = toPositiveInt(filters.minID);
  if (minID !== null) query.set("min_id", String(minID));

  const maxID = toPositiveInt(filters.maxID);
  if (maxID !== null) query.set("max_id", String(maxID));
}

function toRFC3339(value: string): string | null {
  const trimmed = value.trim();
  if (trimmed === "") return null;
  const timestamp = Date.parse(trimmed);
  if (Number.isNaN(timestamp)) return null;
  return new Date(timestamp).toISOString();
}
