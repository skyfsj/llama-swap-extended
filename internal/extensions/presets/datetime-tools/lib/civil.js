// Calendar and clock arithmetic, with no timezone of its own.
//
// Every value here works on "naive" seconds: a count of seconds from
// 1970-01-01T00:00:00Z treated as if it were UTC, so it is really a wall-clock
// reading. lib/zoneinfo.js is what turns a naive reading into an instant and
// back. Keeping the two apart is what makes DST arithmetic explicit instead of
// accidental.

export const MONTHS = [
  "January", "February", "March", "April", "May", "June",
  "July", "August", "September", "October", "November", "December",
];

export const MONTH_ABBREVIATIONS = [
  "Jan", "Feb", "Mar", "Apr", "May", "Jun",
  "Jul", "Aug", "Sep", "Oct", "Nov", "Dec",
];

// Monday first: %u and ISO weeks are Monday-based, and the tables are only
// indexed through weekdayIndex().
export const WEEKDAYS = [
  "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday", "Sunday",
];

export const WEEKDAY_ABBREVIATIONS = ["Mon", "Tue", "Wed", "Thu", "Fri", "Sat", "Sun"];

export const SECONDS_PER_DAY = 86400;

// daysFromCivil / civilFromDays are Howard Hinnant's algorithms: exact for
// every year, no Date object involved, so no year-0..99 quirk and no host
// timezone leaking in.
export function daysFromCivil(year, month, day) {
  const shifted = year - (month <= 2 ? 1 : 0);
  const era = Math.floor(shifted / 400);
  const yearOfEra = shifted - era * 400;
  const dayOfYear = Math.floor((153 * (month + (month > 2 ? -3 : 9)) + 2) / 5) + day - 1;
  const dayOfEra = yearOfEra * 365 + Math.floor(yearOfEra / 4) - Math.floor(yearOfEra / 100) + dayOfYear;
  return era * 146097 + dayOfEra - 719468;
}

export function civilFromDays(days) {
  const shifted = days + 719468;
  const era = Math.floor(shifted / 146097);
  const dayOfEra = shifted - era * 146097;
  const yearOfEra = Math.floor(
    (dayOfEra - Math.floor(dayOfEra / 1460) + Math.floor(dayOfEra / 36524) - Math.floor(dayOfEra / 146096)) / 365,
  );
  const year = yearOfEra + era * 400;
  const dayOfYear = dayOfEra - (365 * yearOfEra + Math.floor(yearOfEra / 4) - Math.floor(yearOfEra / 100));
  const monthPart = Math.floor((5 * dayOfYear + 2) / 153);
  const day = dayOfYear - Math.floor((153 * monthPart + 2) / 5) + 1;
  const month = monthPart + (monthPart < 10 ? 3 : -9);
  return { year: year + (month <= 2 ? 1 : 0), month: month, day: day };
}

// weekdayIndex returns 0 for Monday .. 6 for Sunday. Day 0 (1970-01-01) was a
// Thursday, hence the +3.
export function weekdayIndex(days) {
  return ((days + 3) % 7 + 7) % 7;
}

export function dayOfYear(days) {
  const civil = civilFromDays(days);
  return days - daysFromCivil(civil.year, 1, 1) + 1;
}

export function daysInMonth(year, month) {
  return daysFromCivil(month === 12 ? year + 1 : year, month === 12 ? 1 : month + 1, 1) - daysFromCivil(year, month, 1);
}

// isoWeek returns the ISO 8601 week-year, week number and 1-based weekday
// through the Thursday of the same week, which is what defines the week-year at
// the January boundary.
export function isoWeek(days) {
  const mondayIndex = weekdayIndex(days);
  const thursday = days - mondayIndex + 3;
  const civil = civilFromDays(thursday);
  return {
    year: civil.year,
    week: Math.floor((thursday - daysFromCivil(civil.year, 1, 1)) / 7) + 1,
    weekday: mondayIndex + 1,
  };
}

export function pad(value, width) {
  return String(value).padStart(width, "0");
}

export function yearText(year) {
  if (year >= 0 && year < 10000) {
    return pad(year, 4);
  }
  return year < 0 ? "-" + pad(-year, 4) : String(year);
}

// formatOffset spells an offset the way a reader checks it: UTC+05:45, UTC-04:00.
export function formatOffset(minutes) {
  const sign = minutes < 0 ? "-" : "+";
  const absolute = Math.abs(minutes);
  return "UTC" + sign + pad(Math.floor(absolute / 60), 2) + ":" + pad(absolute % 60, 2);
}

// offsetDigits is the strftime %z / ISO 8601 form: +0545 or -04:00.
export function offsetDigits(minutes, colon) {
  const sign = minutes < 0 ? "-" : "+";
  const absolute = Math.abs(minutes);
  return sign + pad(Math.floor(absolute / 60), 2) + (colon ? ":" : "") + pad(absolute % 60, 2);
}

// STRFTIME_TOKENS is both the implementation table and the documentation the
// tool description repeats, so the two cannot drift apart.
export const STRFTIME_TOKENS = [
  "%Y", "%y", "%m", "%d", "%e", "%H", "%I", "%M", "%S", "%p", "%z", "%Z",
  "%A", "%a", "%B", "%b", "%j", "%U", "%W", "%G", "%V", "%u", "%s",
  "%F", "%T", "%R", "%%",
];

function tokenText(token, parts) {
  const iso = isoWeek(parts.days);
  const weekday = weekdayIndex(parts.days);
  switch (token) {
    case "Y": return yearText(parts.year);
    case "y": return pad(((parts.year % 100) + 100) % 100, 2);
    case "m": return pad(parts.month, 2);
    case "d": return pad(parts.day, 2);
    case "e": return parts.day < 10 ? " " + parts.day : String(parts.day);
    case "H": return pad(parts.hour, 2);
    case "I": return pad(parts.hour % 12 === 0 ? 12 : parts.hour % 12, 2);
    case "M": return pad(parts.minute, 2);
    case "S": return pad(parts.second, 2);
    case "p": return parts.hour < 12 ? "AM" : "PM";
    case "z": return offsetDigits(parts.offset, false);
    case "Z": return parts.abbrev;
    case "A": return WEEKDAYS[weekday];
    case "a": return WEEKDAY_ABBREVIATIONS[weekday];
    case "B": return MONTHS[parts.month - 1];
    case "b": return MONTH_ABBREVIATIONS[parts.month - 1];
    case "j": return pad(dayOfYear(parts.days), 3);
    case "u": return String(weekday + 1);
    case "V": return pad(iso.week, 2);
    case "G": return yearText(iso.year);
    case "U": return pad(parts.weekMode === "us" ? sundayWeek(parts.days) : iso.week, 2);
    case "W": return pad(parts.weekMode === "us" ? mondayWeek(parts.days) : iso.week, 2);
    case "s": return String(Math.floor(parts.ts));
    case "F": return yearText(parts.year) + "-" + pad(parts.month, 2) + "-" + pad(parts.day, 2);
    case "T": return pad(parts.hour, 2) + ":" + pad(parts.minute, 2) + ":" + pad(parts.second, 2);
    case "R": return pad(parts.hour, 2) + ":" + pad(parts.minute, 2);
    default: return null;
  }
}

// sundayWeek and mondayWeek are the C strftime %U and %W: week 0 is the days
// before the first Sunday, respectively the first Monday, of the year.
function sundayWeek(days) {
  const dayIndex = dayOfYear(days) - 1;
  const sundayIndex = (weekdayIndex(days) + 1) % 7;
  return Math.floor((dayIndex + 7 - sundayIndex) / 7);
}

function mondayWeek(days) {
  const dayIndex = dayOfYear(days) - 1;
  return Math.floor((dayIndex + 7 - weekdayIndex(days)) / 7);
}

// strftime renders parts (see localParts in lib/zoneinfo.js). An unknown token
// is an error rather than a silently mangled timestamp: a model that wrote %Q
// gets told, and one that wrote %d gets its answer.
export function strftime(parts, pattern) {
  return String(pattern).replace(/%([A-Za-z%])/g, function (match, token) {
    if (token === "%") {
      return "%";
    }
    const text = tokenText(token, parts);
    if (text === null) {
      throw new Error("unknown strftime token %" + token + "; supported: " + STRFTIME_TOKENS.join(" "));
    }
    return text;
  });
}

// ---- durations -------------------------------------------------------------

const DURATION_UNITS = {
  y: "years", yr: "years", yrs: "years", year: "years", years: "years",
  mo: "months", mos: "months", mon: "months", mons: "months", month: "months", months: "months",
  w: "weeks", wk: "weeks", wks: "weeks", week: "weeks", weeks: "weeks",
  d: "days", dy: "days", day: "days", days: "days",
  h: "hours", hr: "hours", hrs: "hours", hour: "hours", hours: "hours",
  m: "minutes", mi: "minutes", min: "minutes", mins: "minutes", minute: "minutes", minutes: "minutes",
  s: "seconds", sec: "seconds", secs: "seconds", second: "seconds", seconds: "seconds",
};

// parseDuration reads "1 year 2 months", "2h30m", "-3 days" or "2 weeks ago".
// Years, months, weeks and days must be whole numbers; a fractional day or week
// is folded into hours, which are elapsed time.
export function parseDuration(text) {
  let body = String(text === undefined || text === null ? "" : text).toLowerCase().trim();
  if (body === "") {
    return null;
  }
  let negate = false;
  body = body.replace(/^in\s+/, "");
  if (/\s+ago$/.test(body)) {
    negate = true;
    body = body.replace(/\s+ago$/, "");
  }
  const parts = { years: 0, months: 0, weeks: 0, days: 0, hours: 0, minutes: 0, seconds: 0 };
  // Pairs are matched by one pattern so "1 month", "1month" and "2h30m" are the
  // same input; anything between, before or after the pairs must be a
  // separator, so a stray word is an error rather than being ignored.
  const pattern = /([+-]?\d+(?:\.\d+)?)\s*([a-z]+)/g;
  let consumed = 0;
  let pairs = 0;
  let pair = pattern.exec(body);
  while (pair !== null) {
    if (body.slice(consumed, pair.index).replace(/[\s,]+/g, "") !== "") {
      return null;
    }
    const unit = DURATION_UNITS[pair[2]];
    if (!unit) {
      return null;
    }
    // Number() keeps the token's own sign, so "-1 week" is minus a week.
    parts[unit] += Number(pair[1]);
    pairs += 1;
    consumed = pattern.lastIndex;
    pair = pattern.exec(body);
  }
  if (pairs === 0 || body.slice(consumed).replace(/[\s,]+/g, "") !== "") {
    return null;
  }
  if (negate) {
    const names = Object.keys(parts);
    for (let index = 0; index < names.length; index += 1) {
      parts[names[index]] = -parts[names[index]];
    }
  }
  // Keep the calendar steps integral: a fractional day becomes elapsed hours.
  parts.years = Math.trunc(parts.years);
  parts.months = Math.trunc(parts.months);
  parts.hours += (parts.weeks - Math.trunc(parts.weeks)) * 7 * 24 + (parts.days - Math.trunc(parts.days)) * 24;
  parts.weeks = Math.trunc(parts.weeks);
  parts.days = Math.trunc(parts.days);
  parts.elapsedSeconds = parts.hours * 3600 + parts.minutes * 60 + parts.seconds;
  parts.calendarDays = parts.weeks * 7 + parts.days;
  parts.monthsTotal = parts.years * 12 + parts.months;
  parts.empty = parts.monthsTotal === 0 && parts.calendarDays === 0 && parts.elapsedSeconds === 0;
  return parts;
}

// durationText echoes what was applied, in the order it was applied: the
// calendar steps first, then the elapsed one, so the answer reads the way the
// arithmetic was performed.
export function durationText(duration) {
  if (duration.empty) {
    return "0 seconds";
  }
  const pieces = [];
  const years = Math.trunc(duration.monthsTotal / 12);
  const months = duration.monthsTotal - years * 12;
  if (years !== 0) {
    pieces.push(years + (Math.abs(years) === 1 ? " year" : " years"));
  }
  if (months !== 0) {
    pieces.push(months + (Math.abs(months) === 1 ? " month" : " months"));
  }
  if (duration.calendarDays !== 0) {
    if (duration.calendarDays % 7 === 0) {
      const weeks = duration.calendarDays / 7;
      pieces.push(weeks + (Math.abs(weeks) === 1 ? " week" : " weeks"));
    } else {
      pieces.push(duration.calendarDays + (Math.abs(duration.calendarDays) === 1 ? " day" : " days"));
    }
  }
  const elapsed = duration.elapsedSeconds;
  if (elapsed !== 0) {
    const sign = elapsed < 0 ? "-" : "";
    const total = Math.abs(elapsed);
    const hours = Math.trunc(total / 3600);
    const minutes = Math.trunc((total % 3600) / 60);
    const seconds = total % 60;
    if (hours !== 0) {
      pieces.push(sign + hours + (hours === 1 ? " hour" : " hours"));
    }
    if (minutes !== 0) {
      pieces.push(sign + minutes + (minutes === 1 ? " minute" : " minutes"));
    }
    if (seconds !== 0 || (hours === 0 && minutes === 0)) {
      pieces.push(sign + seconds + (seconds === 1 ? " second" : " seconds"));
    }
  }
  return pieces.join(" ");
}

// addCalendar applies the calendar part of a duration to wall-clock fields and
// clamps the day to the target month: 2026-01-31 + 1 month is 2026-02-28.
export function addCalendar(fields, duration) {
  let year = fields.year;
  let month = fields.month;
  let day = fields.day;
  let clamped = false;
  if (duration.monthsTotal !== 0) {
    const months = year * 12 + (month - 1) + duration.monthsTotal;
    year = Math.floor(months / 12);
    month = months - year * 12 + 1;
    const limit = daysInMonth(year, month);
    if (day > limit) {
      day = limit;
      clamped = true;
    }
  }
  if (duration.calendarDays !== 0) {
    const shifted = civilFromDays(daysFromCivil(year, month, day) + duration.calendarDays);
    year = shifted.year;
    month = shifted.month;
    day = shifted.day;
  }
  return { year: year, month: month, day: day, clamped: clamped };
}
