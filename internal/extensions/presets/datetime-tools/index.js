// Date & Time Tools - current time, formatting, calendar arithmetic and
// timezone conversion, entirely offline.
//
// The sandbox has no Intl and no host timezone database, so the preset ships one
// as data: lib/zones.json holds 599 IANA names across 109 records covering
// 2022-01-01T00:00:00Z .. 2041-01-01T00:00:00Z, generated from the system's own
// tzdata (its version is recorded in the file) and verified
// against Intl at every transition. A timestamp outside that window uses the
// nearest recorded rule and the output says so - never silently.
//
// Two tools are installed:
//
//   get_datetime     now / format / add / diff in one zone
//   timezone_convert one instant, rendered in one or more zones
//
// The split that keeps the arithmetic honest: years/months move the local
// calendar (day clamped at month ends), days and weeks keep the local clock
// time, and hours/minutes/seconds move the instant timeline. A conversion of a
// wall-clock reading inside a spring-forward gap is shifted forward by the gap;
// a fall-back reading that happens twice takes the first occurrence. Both are
// reported in the output.

import table from "./lib/zones.json";
import { createZones } from "./lib/zoneinfo.js";
import {
  SECONDS_PER_DAY,
  STRFTIME_TOKENS,
  civilFromDays,
  daysFromCivil,
  dayOfYear,
  durationText,
  formatOffset,
  isoWeek,
  offsetDigits,
  parseDuration,
  strftime,
  weekdayIndex,
  yearText,
} from "./lib/civil.js";
import { parseInstant, shiftBy } from "./lib/instant.js";

const zones = createZones(table);

const TOOL_GET = "get_datetime";
const TOOL_CONVERT = "timezone_convert";
const DEFAULT_TIMEZONE = "UTC";
const DEFAULT_FORMAT = "%Y-%m-%d %H:%M:%S %Z";

// The detail line is deliberately separate from the caller's format: it always
// carries the fields the model needs to reason about the instant (weekday,
// zone abbreviation, true offset, DST state) even when a custom format was
// asked for.
const DETAIL_FORMAT = "%A %Y-%m-%d %H:%M:%S %Z";
const DIFF_UNITS = ["auto", "seconds", "minutes", "hours", "days", "weeks", "months"];
// Mean Gregorian month: a diff is measured on the instant timeline, so months
// cannot be calendar-length without lying about the measurement.
const MEAN_MONTH_SECONDS = 30.436875 * 86400;
const DST_NOTE_HORIZON = 2 * SECONDS_PER_DAY;

const WEEKDAY_PROSE = [
  "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday",
];

export const settings = {
  defaultTimezone: {
    type: "string",
    label: "Default timezone",
    hint: "Used when a call omits timezone / fromTimezone, e.g. Asia/Shanghai, America/New_York, Europe/Berlin. Accepts any IANA name or alias, UTC, or local for the daemon's own timezone.",
    default: DEFAULT_TIMEZONE,
    section: "Defaults",
  },
  defaultFormat: {
    type: "string",
    label: "Default format",
    hint: "strftime pattern used when a call omits format. Example: %Y-%m-%d %H:%M:%S %Z. A custom pattern always wins over this default.",
    default: DEFAULT_FORMAT,
    section: "Defaults",
  },
  weekNumbering: {
    type: "select",
    label: "Week numbering",
    hint: "iso makes %U and %W report the ISO 8601 week, which %V always is; us uses the C strftime rules, where %U counts from the first Sunday and %W from the first Monday of the year.",
    options: ["iso", "us"],
    default: "iso",
    section: "Output",
  },
  showEpoch: {
    type: "boolean",
    label: "Show epoch seconds",
    hint: "Adds Unix epoch seconds and milliseconds to a result. Turn it off when the caller only reads local time.",
    default: true,
    section: "Output",
  },
  listLimit: {
    type: "number",
    label: "Zone list limit",
    hint: "Maximum zones one timezone_convert call may report, and an upper bound on the close-name suggestions an unknown-zone error lists.",
    default: 20,
    min: 5,
    max: 100,
    section: "Output",
  },
};

const FORMAT_HINT = "strftime tokens: " + STRFTIME_TOKENS.join(" ") + ".";

// ---- configuration ---------------------------------------------------------

function readConfig(ctx) {
  const config = ctx && ctx.config ? ctx.config : {};
  const zone = String(config.defaultTimezone === undefined || config.defaultTimezone === null ? "" : config.defaultTimezone).trim();
  const format = String(config.defaultFormat === undefined || config.defaultFormat === null ? "" : config.defaultFormat);
  const limit = Math.floor(numberOr(config.listLimit, 20));
  return {
    timezone: zone === "" ? DEFAULT_TIMEZONE : zone,
    format: format === "" ? DEFAULT_FORMAT : format,
    weekNumbering: config.weekNumbering === "us" ? "us" : "iso",
    showEpoch: config.showEpoch !== false,
    listLimit: limit < 5 ? 5 : limit > 100 ? 100 : limit,
  };
}

function numberOr(value, fallback) {
  const parsed = Number(value);
  return isFinite(parsed) ? parsed : fallback;
}

function label(tool, message) {
  return tool + ": " + message;
}

// ---- zones -----------------------------------------------------------------

function zoneIn(config, name, tool) {
  const requested = String(name === undefined || name === null ? "" : name).trim();
  const wanted = requested === "" ? config.timezone : requested;
  const zone = zones.resolve(wanted);
  if (!zone) {
    const suggestions = zones.suggest(wanted, Math.min(config.listLimit, 6));
    throw new Error(label(tool, "unknown timezone \"" + wanted + "\""
      + (suggestions.length > 0 ? "; did you mean " + suggestions.join(", ") + "?" : "")
      + " The table holds " + zones.aliasCount + " IANA names (for example Asia/Shanghai, America/New_York, Europe/Berlin, Asia/Kolkata, Pacific/Auckland), plus UTC and local."));
  }
  return zone;
}

function zoneDisplay(zone) {
  return zone.kind === "local" ? "local (daemon's own timezone)" : zone.label;
}

// ---- formatting ------------------------------------------------------------

function pad2(value) {
  return value < 10 ? "0" + value : String(value);
}

function padRight(text, width) {
  let padded = text;
  while (padded.length < width) {
    padded += " ";
  }
  return padded;
}

function civilFromNaive(naive) {
  const days = Math.floor(naive / SECONDS_PER_DAY);
  const civil = civilFromDays(days);
  let rest = naive - days * SECONDS_PER_DAY;
  if (rest < 0) {
    rest += SECONDS_PER_DAY;
  }
  return {
    year: civil.year,
    month: civil.month,
    day: civil.day,
    hour: Math.floor(rest / 3600) % 24,
    minute: Math.floor(rest / 60) % 60,
    second: rest % 60,
  };
}

function dateText(civil) {
  return yearText(civil.year) + "-" + pad2(civil.month) + "-" + pad2(civil.day);
}

function clockText(civil) {
  return pad2(civil.hour) + ":" + pad2(civil.minute) + ":" + pad2(civil.second);
}

function isoLocal(state) {
  return dateText(state) + "T" + pad2(state.hour) + ":" + pad2(state.minute) + ":" + pad2(state.second) + offsetDigits(state.offset, true);
}

function strftimeParts(config, state) {
  return {
    ts: state.ts,
    days: state.days,
    year: state.year,
    month: state.month,
    day: state.day,
    hour: state.hour,
    minute: state.minute,
    second: state.second,
    offset: state.offset,
    abbrev: state.abbrev,
    weekMode: config.weekNumbering,
  };
}

function strf(config, state, pattern) {
  return strftime(strftimeParts(config, state), pattern);
}

function weekdayName(state) {
  return WEEKDAY_PROSE[weekdayIndex(state.days)];
}

function dstText(state) {
  return state.dst ? "DST: yes" : "DST: no";
}

function hourText(minutes) {
  const absolute = Math.abs(minutes);
  if (absolute % 60 === 0) {
    const hours = absolute / 60;
    return hours + (hours === 1 ? " hour" : " hours");
  }
  return absolute + " minutes";
}

function daysInYear(days) {
  const civil = civilFromDays(days);
  return daysFromCivil(civil.year + 1, 1, 1) - daysFromCivil(civil.year, 1, 1);
}

// detailLine carries the fields a reader checks a timestamp against: the
// weekday, the zone's own abbreviation, its true UTC offset, and whether a DST
// rule is in force.
function detailLine(config, state) {
  return strf(config, state, DETAIL_FORMAT) + ", " + formatOffset(state.offset) + ", " + dstText(state);
}

function summaryLine(config, state) {
  const iso = isoWeek(state.days);
  const pieces = [
    "ISO week " + iso.week + " of " + iso.year,
    "day " + dayOfYear(state.days) + " of " + daysInYear(state.days),
  ];
  if (config.showEpoch) {
    pieces.push("epoch " + state.ts + " s (" + (state.ts * 1000) + " ms)");
  }
  return pieces.join(", ");
}

// ---- notes -----------------------------------------------------------------

function windowNote(zone, state) {
  if (zone.kind !== "table" || !state.outOfWindow) {
    return null;
  }
  const side = state.outOfWindow === "before" ? "before" : "after";
  const rule = state.outOfWindow === "before" ? "earliest" : "final";
  return "the timezone table covers " + zones.windowText + " and this instant is " + side
    + " it, so the " + rule + " recorded rule for " + zone.label + " was used; DST may differ there.";
}

function resolutionNote(zone, result, state) {
  const notes = [];
  if (result.gap) {
    const before = civilFromNaive(result.gap.at + result.gap.before * 60);
    const after = civilFromNaive(result.gap.at + result.gap.after * 60);
    notes.push("\"" + result.text + "\" does not exist in " + zone.label + ": clocks moved forward on "
      + dateText(before) + " from " + clockText(before) + " " + result.gap.beforeAbbrev
      + " to " + clockText(after) + " " + result.gap.afterAbbrev
      + "; it was read as the same gap length later, " + clockText(state) + " " + state.abbrev + " (" + isoLocal(state) + ").");
  }
  if (result.ambiguous) {
    notes.push("\"" + result.text + "\" happens twice in " + zone.label + " (clocks moved back on "
      + dateText(civilFromNaive(result.ambiguous.at + result.ambiguous.second * 60)) + "); the first occurrence, "
      + clockText(state) + " " + state.abbrev + " (" + formatOffset(state.offset) + "), was used.");
  }
  return notes;
}

// transitionNote restates a DST change close to the instant, which is what a
// caller comparing two zones is usually asking about.
function transitionNote(zone, state) {
  const change = zones.transitionNear(zone, state.ts, DST_NOTE_HORIZON);
  if (!change) {
    return null;
  }
  // A zero shift is a label change, not a clock change: Manitoba moved from
  // CST to EST at the same -05:00 in tzdata 2026e, and "moved back 0 hours
  // (UTC-05:00 -> UTC-05:00)" says nothing useful.
  if (change.shiftMinutes === 0) {
    return null;
  }
  const localAt = civilFromNaive(change.at + change.beforeOffset * 60);
  return "local clocks " + (change.upcoming ? "move" : "moved") + " "
    + (change.shiftMinutes > 0 ? "forward" : "back") + " " + hourText(change.shiftMinutes)
    + " on " + dateText(localAt) + " at " + pad2(localAt.hour) + ":" + pad2(localAt.minute)
    + " (" + formatOffset(change.beforeOffset) + " -> " + formatOffset(change.afterOffset) + ").";
}

// ---- inputs ----------------------------------------------------------------

function nowSeconds() {
  return Math.floor(Date.now() / 1000);
}

function resolveInput(zone, text, referenceTs) {
  const parsed = parseInstant(zones, zone, text, referenceTs, zoneDisplay(zone));
  if (parsed) {
    return parsed;
  }
  return { ts: referenceTs, kind: "now", text: "now", offset: null, gap: null, ambiguous: null };
}

// ---- get_datetime ----------------------------------------------------------

function getDatetime(ctx, args) {
  const config = readConfig(ctx);
  const action = String(args.action === undefined || args.action === null ? "now" : args.action).trim().toLowerCase();
  if (action !== "now" && action !== "format" && action !== "add" && action !== "diff") {
    throw new Error(label(TOOL_GET, "unknown action \"" + args.action + "\"; use now, format, add or diff"));
  }
  if (action === "diff") {
    return diffResult(config, args);
  }
  const zone = zoneIn(config, args.timezone, TOOL_GET);
  const reference = args.reference === undefined || args.reference === null || String(args.reference).trim() === ""
    ? null
    : resolveInput(zone, String(args.reference), nowSeconds());
  const referenceTs = reference ? reference.ts : nowSeconds();

  let result;
  let duration = null;
  if (action === "add") {
    const text = typeof args.add === "string" ? args.add.trim() : "";
    if (text === "") {
      throw new Error(label(TOOL_GET, "action add needs add=\"<duration>\", for example add=\"1 month\", add=\"2h30m\" or add=\"-3 days\""));
    }
    duration = parseDuration(text);
    if (!duration) {
      throw new Error(label(TOOL_GET, "cannot read \"" + args.add + "\" as a duration; use forms like \"3 days\", \"2h30m\", \"1 year 2 months\" or \"-1 week\""));
    }
    // "0 seconds" is a valid duration that adds nothing. Reporting it as
    // unreadable told a caller its input was malformed when it was not.
    if (duration.empty) {
      const base = resolveInput(zone, args.datetime, referenceTs);
      const baseState = zones.parts(zone, base.ts);
      const lines = [];
      const requestedFormat = args.format === undefined || args.format === null || String(args.format) === "" ? null : String(args.format);
      if (requestedFormat !== null) {
        lines.push("formatted: " + strf(config, baseState, requestedFormat));
      }
      lines.push(isoLocal(baseState) + " (" + zoneDisplay(zone) + ")");
      lines.push("add:    " + durationText(duration) + " (no change)");
      lines.push("result: " + isoLocal(baseState) + " (" + zoneDisplay(zone) + ")");
      return lines.join("\n");
    }
    const base = resolveInput(zone, args.datetime, referenceTs);
    result = shiftBy(zones, zone, base.ts, duration, base.text);
    result.base = base;
  } else {
    result = resolveInput(zone, args.datetime, referenceTs);
  }

  const state = zones.parts(zone, result.ts);
  const requestedFormat = args.format === undefined || args.format === null || String(args.format) === "" ? null : String(args.format);
  const pattern = requestedFormat === null ? (action === "format" ? config.format : null) : requestedFormat;
  const lines = [];
  if (pattern !== null) {
    lines.push("formatted: " + strf(config, state, pattern));
  }
  if (action === "add") {
    const baseState = zones.parts(zone, result.base.ts);
    lines.push("base:   " + isoLocal(baseState) + " (" + zoneDisplay(zone) + ")");
    lines.push("add:    " + durationText(duration));
    lines.push("result: " + isoLocal(state) + " (" + zoneDisplay(zone) + ")");
  } else {
    lines.push(isoLocal(state) + " (" + zoneDisplay(zone) + ")");
  }
  lines.push(detailLine(config, state));
  lines.push(summaryLine(config, state));
  if (action === "add" && duration.calendarDays !== 0 && duration.monthsTotal !== 0) {
    lines.push("note: years and months shift the local calendar (the day is clamped to the target month); days and weeks keep the local clock time; hours, minutes and seconds move the instant timeline.");
  } else if (action === "add" && duration.monthsTotal !== 0) {
    lines.push("note: months shift the local calendar and the day is clamped to the last day of the target month.");
  } else if (action === "add" && duration.calendarDays !== 0) {
    lines.push("note: days and weeks keep the local clock time, so a DST day is still the same time tomorrow.");
  } else if (action === "add") {
    lines.push("note: hours, minutes and seconds move the instant timeline (1 hour is 3600 real seconds), so the local clock can jump at a DST change.");
  }
  if (result.clamped) {
    lines.push("note: the day was clamped to the last day of the target month.");
  }
  const notes = resolutionNote(zone, result, state);
  for (let index = 0; index < notes.length; index += 1) {
    lines.push("note: " + notes[index]);
  }
  const change = transitionNote(zone, state);
  if (change) {
    lines.push("note: " + change);
  }
  const window = windowNote(zone, state);
  if (window) {
    lines.push("note: " + window);
  }
  ctx.log.info(TOOL_GET + " " + action + " " + zone.label + " -> " + isoLocal(state));
  return lines.join("\n");
}

function unitForm(value) {
  if (Number.isInteger(value)) {
    return String(value);
  }
  const rounded = Math.round(value * 1000) / 1000;
  return String(rounded);
}

function breakdownText(seconds) {
  const total = Math.floor(Math.abs(seconds));
  const weeks = Math.floor(total / 604800);
  const days = Math.floor((total % 604800) / 86400);
  const hours = Math.floor((total % 86400) / 3600);
  const minutes = Math.floor((total % 3600) / 60);
  const rest = total % 60;
  const pieces = [];
  if (weeks > 0) {
    pieces.push(weeks + (weeks === 1 ? " week" : " weeks"));
  }
  if (days > 0) {
    pieces.push(days + (days === 1 ? " day" : " days"));
  }
  if (hours > 0) {
    pieces.push(hours + (hours === 1 ? " hour" : " hours"));
  }
  if (minutes > 0) {
    pieces.push(minutes + (minutes === 1 ? " minute" : " minutes"));
  }
  if (rest > 0 || pieces.length === 0) {
    pieces.push(rest + (rest === 1 ? " second" : " seconds"));
  }
  return pieces.slice(0, 4).join(" ");
}

function diffResult(config, args) {
  const zone = zoneIn(config, args.timezone, TOOL_GET);
  const reference = args.reference === undefined || args.reference === null || String(args.reference).trim() === ""
    ? null
    : resolveInput(zone, String(args.reference), nowSeconds());
  const referenceTs = reference ? reference.ts : nowSeconds();
  const fromText = args.from === undefined || args.from === null || String(args.from).trim() === "" ? args.datetime : args.from;
  if (fromText === undefined || fromText === null || String(fromText).trim() === "") {
    throw new Error(label(TOOL_GET, "action diff needs from and to (or datetime and to), for example from=\"2026-01-01\" to=\"now\""));
  }
  if (args.to === undefined || args.to === null || String(args.to).trim() === "") {
    throw new Error(label(TOOL_GET, "action diff needs to=\"<instant>\", for example to=\"now\" or to=\"2026-12-31\""));
  }
  const from = resolveInput(zone, String(fromText), referenceTs);
  const to = resolveInput(zone, String(args.to), referenceTs);
  const requested = String(args.unit === undefined || args.unit === null ? "auto" : args.unit).trim().toLowerCase() || "auto";
  if (DIFF_UNITS.indexOf(requested) < 0) {
    throw new Error(label(TOOL_GET, "unknown unit \"" + args.unit + "\"; use " + DIFF_UNITS.join(", ")));
  }
  const delta = to.ts - from.ts;
  const factors = { seconds: 1, minutes: 60, hours: 3600, days: 86400, weeks: 604800, months: MEAN_MONTH_SECONDS };
  let unit = requested;
  if (unit === "auto") {
    unit = Math.abs(delta) >= 86400 ? "days" : Math.abs(delta) >= 3600 ? "hours" : Math.abs(delta) >= 60 ? "minutes" : "seconds";
  }
  const value = delta / factors[unit];
  const fromState = zones.parts(zone, from.ts);
  const toState = zones.parts(zone, to.ts);
  const lines = [
    "from: " + isoLocal(fromState) + " (" + zoneDisplay(zone) + ", " + weekdayName(fromState) + ")",
    "to:   " + isoLocal(toState) + " (" + zoneDisplay(zone) + ", " + weekdayName(toState) + ")",
    "diff: " + unitForm(value) + " " + (Math.abs(value) === 1 ? unit.slice(0, -1) : unit) + " (" + breakdownText(delta) + ")"
      + (delta < 0 ? " - to is earlier than from" : ""),
    "note: measured on the instant timeline: a day is 86400 s and a month is the mean Gregorian month (30.436875 days), so a DST shift inside the interval shows as 23 or 25 real hours.",
  ];
  for (const result of [from, to]) {
    const notes = resolutionNote(zone, result, zones.parts(zone, result.ts));
    for (let index = 0; index < notes.length; index += 1) {
      lines.push("note: " + notes[index]);
    }
  }
  const window = windowNote(zone, toState) || windowNote(zone, fromState);
  if (window) {
    lines.push("note: " + window);
  }
  return lines.join("\n");
}

// ---- timezone_convert ------------------------------------------------------

function timezoneConvert(ctx, args) {
  const config = readConfig(ctx);
  if (args.toTimezone === undefined || args.toTimezone === null || String(args.toTimezone).trim() === "") {
    throw new Error(label(TOOL_CONVERT, "toTimezone is required, for example toTimezone=\"Europe/Berlin\""));
  }
  if (args.datetime === undefined || args.datetime === null || String(args.datetime).trim() === "") {
    throw new Error(label(TOOL_CONVERT, "datetime is required: an ISO 8601 timestamp, epoch:N, or a phrase like \"in 3 days\""));
  }
  const fromRequested = args.fromTimezone === undefined || args.fromTimezone === null ? "" : String(args.fromTimezone).trim();
  const sourceZone = zoneIn(config, fromRequested === "" ? config.timezone : fromRequested, TOOL_CONVERT);
  const result = parseInstant(zones, sourceZone, String(args.datetime), nowSeconds(), zoneDisplay(sourceZone));
  if (!result) {
    throw new Error(label(TOOL_CONVERT, "datetime is required: an ISO 8601 timestamp, epoch:N, or a phrase like \"in 3 days\""));
  }
  const toZone = zoneIn(config, args.toTimezone, TOOL_CONVERT);
  const list = [toZone];
  const seen = {};
  seen[toZone.label.toLowerCase()] = true;
  const requested = typeof args.targets === "string" ? args.targets.split(",") : Array.isArray(args.targets) ? args.targets : [];
  for (let index = 0; index < requested.length; index += 1) {
    const extra = zoneIn(config, requested[index], TOOL_CONVERT);
    if (!seen[extra.label.toLowerCase()]) {
      seen[extra.label.toLowerCase()] = true;
      list.push(extra);
    }
  }
  if (list.length > config.listLimit) {
    throw new Error(label(TOOL_CONVERT, "this call would report " + list.length + " zones and the extension's listLimit setting allows " + config.listLimit + "; raise it under Settings or ask for fewer zones"));
  }

  const pattern = args.format === undefined || args.format === null || String(args.format) === "" ? null : String(args.format);
  const sourceState = zones.parts(sourceZone, result.ts);
  const lines = [
    "Source: " + isoLocal(sourceState) + " | " + zoneDisplay(sourceZone) + " | " + weekdayName(sourceState)
      + " | " + sourceState.abbrev + " | " + formatOffset(sourceState.offset) + " | " + dstText(sourceState)
      + (config.showEpoch ? " | epoch " + result.ts + " s" : ""),
  ];
  if (result.offset !== null && fromRequested !== "") {
    lines.push("note: the datetime carried its own UTC offset (" + offsetDigits(result.offset, true) + "), which decides the instant; fromTimezone=" + fromRequested + " only applies to readings without one.");
  }
  const notes = resolutionNote(sourceZone, result, sourceState);
  for (let index = 0; index < notes.length; index += 1) {
    lines.push("note: " + notes[index]);
  }

  // Render every line first so the local-time column can be aligned; the
  // offsets and abbreviations then read down the page.
  const rendered = [];
  let width = 0;
  for (let index = 0; index < list.length; index += 1) {
    const state = zones.parts(list[index], result.ts);
    const local = pattern === null ? isoLocal(state) : strf(config, state, pattern);
    if (local.length > width) {
      width = local.length;
    }
    rendered.push({ zone: list[index], state: state, local: local });
  }
  for (let index = 0; index < rendered.length; index += 1) {
    const entry = rendered[index];
    const change = transitionNote(entry.zone, entry.state);
    lines.push(entry.zone.label + ": " + padRight(entry.local, width) + " | " + weekdayName(entry.state) + " | "
      + entry.state.abbrev + " | " + formatOffset(entry.state.offset) + " | " + dstText(entry.state)
      + (change === null ? "" : " | " + change));
  }
  const window = windowNote(toZone, zones.parts(toZone, result.ts));
  if (window) {
    lines.push("note: " + window);
  }
  ctx.log.info(TOOL_CONVERT + " " + sourceZone.label + " -> " + list.length + " zone(s)");
  return lines.join("\n");
}

// ---- tool surface ----------------------------------------------------------

const TOOL_DEFINITIONS = [
  {
    type: "function",
    execution: "server",
    function: {
      name: TOOL_GET,
      description: "Get the current time, format or shift a timestamp, or measure the distance between two timestamps, in any IANA timezone. Offline: an embedded table of 599 IANA names covers 2022-01-01 to 2041-01-01 with the real DST transitions; an instant outside that window uses the nearest recorded rule and says so. Every answer carries the instant as ISO 8601 with the zone's true UTC offset, the weekday, the zone abbreviation, the UTC offset, DST status, ISO week number and day of the year"
        + " (plus epoch seconds unless that setting is off). Actions: now (default), format (render strftime), add (calendar arithmetic), diff (elapsed time between from and to)."
        + " add applies years then months on the local calendar (2026-01-31 + 1 month = 2026-02-28), then days and weeks keeping the local clock time (the same time tomorrow even across a DST change), then hours/minutes/seconds on the instant timeline (1 hour is 3600 real seconds)."
        + " diff measures the instant timeline and says so. Examples: {action:now, timezone:Asia/Shanghai}; "
        + "{action:format, timezone:America/New_York, datetime:epoch:1784123525, format:%A %Y-%m-%d %H:%M %Z}; "
        + "{action:add, timezone:Europe/Berlin, datetime:2026-01-31, add:1 month}; "
        + "{action:diff, from:2026-01-01, to:now, unit:days}."
        + " datetime accepts an ISO 8601 timestamp with Z or ±HH:MM, YYYY-MM-DD, YYYY-MM-DD HH:MM[:SS], epoch:1784123525 (seconds, or milliseconds at 1e11 or more), or a phrase: now, today, tomorrow, yesterday, start of day, end of day, in 3 days, 2 weeks ago, next monday, last friday (those resolve against reference, default now, at the target zone)."
        + " timezone accepts an IANA name or alias, UTC, or local for the daemon's own timezone. " + FORMAT_HINT,
      parameters: {
        type: "object",
        properties: {
          action: {
            type: "string",
            enum: ["now", "format", "add", "diff"],
            description: "now (default) reports the current instant; format renders datetime with format; add shifts datetime by add; diff measures to minus from.",
          },
          timezone: {
            type: "string",
            description: "Target zone: IANA name, alias, UTC, or local. Defaults to the extension's defaultTimezone setting (UTC unless changed).",
          },
          datetime: {
            type: "string",
            description: "The instant to read or shift (default now). ISO 8601 with Z or ±HH:MM, YYYY-MM-DD [HH:MM[:SS]], epoch:1784123525, or a phrase such as now, today, tomorrow, yesterday, start of day, end of day, in 3 days, 2 weeks ago, next monday, last friday.",
          },
          format: {
            type: "string",
            description: "strftime pattern for the formatted line, for example %A %Y-%m-%d %H:%M %Z. " + FORMAT_HINT,
          },
          add: {
            type: "string",
            description: "Duration for action=add, for example \"3 days\", \"2h30m\", \"1 year 2 months\", \"-1 week\". A negative duration subtracts.",
          },
          from: {
            type: "string",
            description: "Start instant for action=diff (defaults to datetime). Takes the same forms as datetime.",
          },
          to: {
            type: "string",
            description: "End instant for action=diff, for example \"now\" or \"2026-12-31\". Takes the same forms as datetime.",
          },
          unit: {
            type: "string",
            enum: DIFF_UNITS,
            description: "Unit for action=diff: auto (default, the largest that fits), seconds, minutes, hours, days, weeks, or months (mean Gregorian month).",
          },
          reference: {
            type: "string",
            description: "Instant that relative phrases and \"now\" resolve against (default: the real current time). Set it to make an answer reproducible.",
          },
        },
      },
    },
  },
  {
    type: "function",
    execution: "server",
    function: {
      name: TOOL_CONVERT,
      description: "Convert one instant between IANA timezones and show the same moment in each requested zone - use it for \"what time is it for each of these people\". DST is handled explicitly: a local time inside a spring-forward gap is shifted forward by the gap length and the shift is reported; a fall-back time that happens twice takes the first (earlier) occurrence and says so; a DST change within two days of the instant is reported as e.g. \"local clocks move forward 1 hour on 2026-03-29 at 02:00 (UTC+01:00 -> UTC+02:00)\"."
        + " Each line carries the zone's local time, weekday, abbreviation, true UTC offset and DST state."
        + " Example: {datetime:\"2026-03-08 01:30\", fromTimezone:\"America/New_York\", toTimezone:\"Europe/Berlin\", targets:[\"Asia/Shanghai\",\"America/Los_Angeles\"]}."
        + " datetime takes the same forms as get_datetime; when it carries its own offset (Z or ±HH:MM) that offset decides the instant and fromTimezone is ignored for it."
        + " The embedded table covers 2022-01-01 to 2041-01-01 and the output names the nearest rule used outside it. format is an optional strftime pattern for the local time (default ISO 8601), and targets is capped by the listLimit setting.",
      parameters: {
        type: "object",
        properties: {
          datetime: {
            type: "string",
            description: "The instant to convert. ISO 8601 with an optional Z or ±HH:MM offset, YYYY-MM-DD [HH:MM[:SS]], epoch:1784123525, or a phrase such as now, tomorrow, in 2 hours, 2 weeks ago, next monday (resolved against the current time).",
          },
          fromTimezone: {
            type: "string",
            description: "Zone the datetime's wall-clock reading belongs to (defaults to the defaultTimezone setting). Ignored when the datetime carries its own UTC offset.",
          },
          toTimezone: {
            type: "string",
            description: "Zone to convert to, for example Europe/Berlin or America/Los_Angeles.",
          },
          format: {
            type: "string",
            description: "Optional strftime pattern for the local time on each line, for example %Y-%m-%d %H:%M %Z. Default is ISO 8601 with the offset.",
          },
          targets: {
            type: "array",
            items: { type: "string" },
            description: "Extra zones to show at the same instant, for example [\"Asia/Shanghai\",\"America/New_York\"]. toTimezone is always reported first.",
          },
        },
        required: ["datetime", "toTimezone"],
      },
    },
  },
];

export default {
  tools: TOOL_DEFINITIONS,

  async onToolCall(ctx, call) {
    if (!call || (call.name !== TOOL_GET && call.name !== TOOL_CONVERT)) {
      return undefined;
    }
    const args = call.arguments && typeof call.arguments === "object" ? call.arguments : {};
    const tool = call.name;
    try {
      const content = tool === TOOL_GET ? getDatetime(ctx, args) : timezoneConvert(ctx, args);
      return { content: content };
    } catch (error) {
      const message = error && error.message ? error.message : String(error);
      if (message.indexOf(tool + ": ") === 0) {
        throw error;
      }
      throw new Error(label(tool, message));
    }
  },
};
