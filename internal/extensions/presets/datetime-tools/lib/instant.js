// Parsing the `datetime` / `from` / `to` inputs into an instant.
//
// Accepted forms are deliberately narrow so a wrong reading is an error rather
// than a plausible number:
//
//   epoch:1784123525         epoch seconds, or milliseconds when >= 1e11
//   2026-03-08T01:30:00Z     ISO 8601 carrying its own offset
//   2026-03-08 01:30         a wall-clock reading, interpreted in the zone
//   2026-03-08               midnight in the zone
//   now, today, tomorrow, yesterday
//   start of day, end of day
//   in 3 days, 2 weeks ago
//   next monday, last friday
//
// A wall-clock reading is resolved by lib/zoneinfo.js, so a reading inside a
// spring-forward gap is shifted forward (and reported) and one inside a
// fall-back overlap takes the earlier occurrence (and is reported).

import { SECONDS_PER_DAY, addCalendar, daysFromCivil, parseDuration, weekdayIndex } from "./civil.js";

const WEEKDAY_NAMES = {
  monday: 0, mon: 0,
  tuesday: 1, tue: 1, tues: 1,
  wednesday: 2, wed: 2,
  thursday: 3, thu: 3, thur: 3, thurs: 3,
  friday: 4, fri: 4,
  saturday: 5, sat: 5,
  sunday: 6, sun: 6,
};

const WALL_PATTERN = /^(\d{4})-(\d{2})-(\d{2})(?:[t ](\d{2}):(\d{2})(?::(\d{2}))?(?:\.\d{1,9})?)?\s*(z|[+-]\d{2}:?\d{2})?$/;

export function parseInstant(zones, zone, text, referenceTs, defaultZoneName) {
  const raw = String(text === undefined || text === null ? "" : text).trim();
  if (raw === "") {
    return null;
  }
  const flat = raw.toLowerCase().replace(/\s+/g, " ");

  const epoch = /^epoch:(-?\d+(?:\.\d+)?)$/.exec(flat);
  if (epoch) {
    const value = Number(epoch[1]);
    const milliseconds = Math.abs(value) >= 1e11;
    const seconds = milliseconds ? value / 1000 : value;
    if (!isFinite(seconds) || Math.abs(seconds) > 8e12) {
      throw new Error("epoch value " + epoch[1] + " is outside the range this tool can read");
    }
    return {
      ts: Math.floor(seconds),
      kind: "epoch",
      text: raw,
      milliseconds: milliseconds,
      offset: null,
      gap: null,
      ambiguous: null,
    };
  }

  if (flat === "now") {
    return relative(referenceTs, raw);
  }

  if (flat === "today" || flat === "tomorrow" || flat === "yesterday") {
    const shift = flat === "tomorrow" ? 1 : flat === "yesterday" ? -1 : 0;
    return withDay(zones, zone, startOfDay(zones, zone, referenceTs).ts, shift, raw);
  }

  if (flat === "start of day" || flat === "end of day") {
    const state = zones.parts(zone, referenceTs);
    const midnight = daysFromCivil(state.year, state.month, state.day) * SECONDS_PER_DAY;
    const naive = flat === "start of day" ? midnight : midnight + 23 * 3600 + 59 * 60 + 59;
    return wallResolved(zones, zone, naive, raw, null);
  }

  let durationText = null;
  const relativeMatch = /^in\s+(.+)$/.exec(flat);
  if (relativeMatch) {
    durationText = relativeMatch[1];
  } else if (/\s+ago$/.test(flat)) {
    durationText = flat;
  }
  if (durationText !== null) {
    const parsed = parseDuration(durationText);
    if (!parsed) {
      throw new Error("cannot read \"" + raw + "\" as a relative time; try \"in 3 days\" or \"2 weeks ago\"");
    }
    return shiftBy(zones, zone, referenceTs, parsed, raw);
  }

  const weekday = /^(next|last)\s+([a-z]+)$/.exec(flat);
  if (weekday) {
    const target = WEEKDAY_NAMES[weekday[2]];
    if (target === undefined) {
      throw new Error("no weekday named \"" + weekday[2] + "\" in \"" + raw + "\"");
    }
    const state = zones.parts(zone, referenceTs);
    const today = daysFromCivil(state.year, state.month, state.day);
    const current = weekdayIndex(today);
    let delta = weekday[1] === "next" ? (target - current + 7) % 7 : -((current - target + 7) % 7);
    if (delta === 0) {
      delta = weekday[1] === "next" ? 7 : -7;
    }
    return wallResolved(zones, zone, (today + delta) * SECONDS_PER_DAY, raw, null);
  }

  const wall = WALL_PATTERN.exec(flat);
  if (wall) {
    const year = Number(wall[1]);
    const month = Number(wall[2]);
    const day = Number(wall[3]);
    if (month < 1 || month > 12) {
      throw new Error("month " + wall[2] + " is not between 01 and 12 in \"" + raw + "\"");
    }
    const limit = daysInMonth(year, month);
    if (day < 1 || day > limit) {
      throw new Error("day " + wall[3] + " does not exist in " + wall[1] + "-" + wall[2] + " (the month has " + limit + " days)");
    }
    const hour = wall[4] === undefined ? 0 : Number(wall[4]);
    const minute = wall[5] === undefined ? 0 : Number(wall[5]);
    const second = wall[6] === undefined ? 0 : Number(wall[6]);
    if (hour > 23 || minute > 59 || second > 59) {
      throw new Error("\"" + raw + "\" is not a valid clock reading (hours 00-23, minutes and seconds 00-59)");
    }
    const naive = daysFromCivil(year, month, day) * SECONDS_PER_DAY + hour * 3600 + minute * 60 + second;
    if (wall[7] === undefined) {
      return wallResolved(zones, zone, naive, raw, null);
    }
    const offset = parseOffset(wall[7], raw);
    return {
      ts: naive - offset * 60,
      kind: "instant",
      text: raw,
      offset: offset,
      gap: null,
      ambiguous: null,
    };
  }

  const hint = defaultZoneName === undefined || defaultZoneName === null ? "" : " (wall-clock readings are read as " + defaultZoneName + ")";
  throw new Error("cannot read \"" + raw + "\" as a time; use ISO 8601 (2026-03-08T01:30:00Z, 2026-03-08 01:30), epoch:1784123525, now/today/tomorrow/yesterday, or a phrase like \"in 3 days\"" + hint);
}

function daysInMonth(year, month) {
  return daysFromCivil(month === 12 ? year + 1 : year, month === 12 ? 1 : month + 1, 1) - daysFromCivil(year, month, 1);
}

function parseOffset(text, raw) {
  if (text === "z") {
    return 0;
  }
  const match = /^([+-])(\d{2}):?(\d{2})$/.exec(text);
  const hours = Number(match[2]);
  const minutes = Number(match[3]);
  if (hours > 18 || minutes > 59) {
    throw new Error("offset " + text + " in \"" + raw + "\" is not a valid UTC offset");
  }
  return (match[1] === "-" ? -1 : 1) * (hours * 60 + minutes);
}

function relative(ts, text) {
  return { ts: ts, kind: "relative", text: text, offset: null, gap: null, ambiguous: null };
}

function wallResolved(zones, zone, naive, text) {
  const resolved = zones.localToInstant(zone, naive);
  return {
    ts: resolved.ts,
    kind: "wall",
    text: text,
    offset: null,
    gap: resolved.gap,
    ambiguous: resolved.ambiguous,
  };
}

function startOfDay(zones, zone, ts) {
  const state = zones.parts(zone, ts);
  return zones.localToInstant(zone, daysFromCivil(state.year, state.month, state.day) * SECONDS_PER_DAY);
}

function withDay(zones, zone, ts, shift, text) {
  const state = zones.parts(zone, ts);
  const moved = daysFromCivil(state.year, state.month, state.day) + shift;
  return wallResolved(zones, zone, moved * SECONDS_PER_DAY, text, null);
}

// shiftBy applies a calendar duration to an instant in the zone's own calendar:
// years and months first (clamped at month ends), then whole days and weeks
// keeping the local clock time, then hours/minutes/seconds on the instant
// timeline.
export function shiftBy(zones, zone, ts, duration, text) {
  const state = zones.parts(zone, ts);
  const shifted = addCalendar({ year: state.year, month: state.month, day: state.day }, duration);
  const naive = daysFromCivil(shifted.year, shifted.month, shifted.day) * SECONDS_PER_DAY
    + state.hour * 3600 + state.minute * 60 + state.second;
  const resolved = zones.localToInstant(zone, naive);
  return {
    ts: resolved.ts + duration.elapsedSeconds,
    kind: "shifted",
    text: text,
    offset: null,
    gap: resolved.gap,
    ambiguous: resolved.ambiguous,
    clamped: shifted.clamped,
    base: ts,
  };
}
