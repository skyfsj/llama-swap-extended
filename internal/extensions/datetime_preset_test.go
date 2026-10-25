package extensions

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dateTimeCall invokes one of the datetime-tools tools and returns the text it
// hands the model. The preset is bundled and inspected per call through the
// shared presetBundle helper, so the script runs in the real worker.
func dateTimeCall(t *testing.T, tool string, arguments map[string]any) string {
	t.Helper()
	compiled := presetBundle(t, "datetime-tools")
	if compiled == nil {
		t.Skip("datetime-tools is not in the catalog")
	}
	payload, err := json.Marshal(map[string]any{"name": tool, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{}, payload)
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	var result struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("output %s: %v", string(out), err)
	}
	return result.Content
}

func dateTimeError(t *testing.T, tool string, arguments map[string]any) string {
	t.Helper()
	compiled := presetBundle(t, "datetime-tools")
	if compiled == nil {
		t.Skip("datetime-tools is not in the catalog")
	}
	payload, err := json.Marshal(map[string]any{"name": tool, "arguments": arguments})
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = compiled.Invoke(context.Background(), "onToolCall", Context{}, payload)
	if err == nil {
		t.Fatal("the tool accepted input it should have rejected")
	}
	return err.Error()
}

// dateTimeContains asserts every substring is present. It exists so a failing
// test prints the whole answer, which is what a reader needs to see.
func dateTimeContains(t *testing.T, text string, wants []string) {
	t.Helper()
	for _, want := range wants {
		if !strings.Contains(text, want) {
			t.Fatalf("result is missing %q:\n%s", want, text)
		}
	}
}

func TestPreset_DateTimeSpringForwardGapIsShiftedAndReported(t *testing.T) {
	// 02:30 never happens in New York on 2026-03-08: the clock jumps from
	// 01:59:59 EST to 03:00:00 EDT. The reading must be shifted forward by the
	// gap length and the shift must be reported, not silently guessed.
	text := dateTimeCall(t, "timezone_convert", map[string]any{
		"datetime":     "2026-03-08 02:30",
		"fromTimezone": "America/New_York",
		"toTimezone":   "Europe/Berlin",
	})
	dateTimeContains(t, text, []string{
		"does not exist in America/New_York",
		"clocks moved forward on 2026-03-08",
		"2026-03-08T03:30:00-04:00",
		"2026-03-08T08:30:00+01:00",
		"UTC-04:00",
		"UTC+01:00",
		"EDT",
		// Abbreviations are the ones tzdata records: CET for Europe/Berlin,
		// where the older ICU-sourced table had the generic GMT+1.
		"CET",
	})
}

func TestPreset_DateTimeAddKeepsClockTimeForDaysAndElapsedTimeForHours(t *testing.T) {
	// 01:30 EST + 1 hour is 03:30 EDT on the instant timeline: one real hour,
	// two hours on the local clock. The same arithmetic with 1 day keeps the
	// local clock time instead.
	hours := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "add",
		"timezone": "America/New_York",
		"datetime": "2026-03-08 01:30",
		"add":      "1 hour",
	})
	dateTimeContains(t, hours, []string{
		"result: 2026-03-08T03:30:00-04:00",
		"instant timeline",
	})

	day := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "add",
		"timezone": "America/New_York",
		"datetime": "2026-03-07 12:00",
		"add":      "1 day",
	})
	dateTimeContains(t, day, []string{
		"base:   2026-03-07T12:00:00-05:00",
		"result: 2026-03-08T12:00:00-04:00",
		"keep the local clock time",
	})

	// A negative duration subtracts on the same timelines.
	back := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "add",
		"timezone": "America/New_York",
		"datetime": "2026-03-08 03:30",
		"add":      "-1 hour",
	})
	dateTimeContains(t, back, []string{
		"add:    -1 hour",
		"result: 2026-03-08T01:30:00-05:00",
	})
}

func TestPreset_TimezoneFallBackAmbiguityTakesFirstOccurrence(t *testing.T) {
	// 01:30 happens twice in New York on 2026-11-01. The first occurrence is
	// 01:30 EDT (UTC-04:00); the output must say which one it used.
	text := dateTimeCall(t, "timezone_convert", map[string]any{
		"datetime":     "2026-11-01 01:30",
		"fromTimezone": "America/New_York",
		"toTimezone":   "UTC",
	})
	dateTimeContains(t, text, []string{
		"happens twice in America/New_York",
		"first occurrence",
		"2026-11-01T01:30:00-04:00",
		"EDT",
		"2026-11-01T05:30:00+00:00",
	})
	if strings.Contains(text, "2026-11-01T01:30:00-05:00") {
		t.Fatalf("the later occurrence (EST) was used:\n%s", text)
	}
}

func TestPreset_DateTimeFixedOffsetZoneAndQuarterHourOffsets(t *testing.T) {
	// Asia/Shanghai never shifts inside the window; Asia/Kathmandu is +05:45,
	// which is where a formatter that assumes whole hours breaks.
	shanghai := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "Asia/Shanghai",
		"datetime": "2026-07-15T12:00:00Z",
		"format":   "%F %T %z %Z",
	})
	dateTimeContains(t, shanghai, []string{
		"formatted: 2026-07-15 20:00:00 +0800 CST",
		"2026-07-15T20:00:00+08:00",
		"UTC+08:00",
		"DST: no",
	})

	katmandu := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "Asia/Kathmandu",
		"datetime": "2026-07-15T12:00:00Z",
		"format":   "%H:%M %z",
	})
	dateTimeContains(t, katmandu, []string{
		"formatted: 17:45 +0545",
		"UTC+05:45",
	})
}

func TestPreset_DateTimeAliasMatchesCanonicalName(t *testing.T) {
	// Asia/Calcutta and Asia/Kolkata are the same zone; the answers may only
	// differ in the name the caller used.
	instant := map[string]any{"datetime": "epoch:1784123525", "format": "%F %T %Z"}
	calcutta := dateTimeCall(t, "get_datetime", map[string]any{
		"action": "format", "timezone": "Asia/Calcutta", "datetime": instant["datetime"], "format": instant["format"],
	})
	kolkata := dateTimeCall(t, "get_datetime", map[string]any{
		"action": "format", "timezone": "Asia/Kolkata", "datetime": instant["datetime"], "format": instant["format"],
	})
	dateTimeContains(t, calcutta, []string{"UTC+05:30", "+05:30"})
	normalizedCalcutta := strings.ReplaceAll(calcutta, "Asia/Calcutta", "Asia/Kolkata")
	if normalizedCalcutta != kolkata {
		t.Fatalf("aliases disagree:\n%s\n---\n%s", normalizedCalcutta, kolkata)
	}
}

func TestPreset_DateTimeMonthEndIsClamped(t *testing.T) {
	// Calendar months are not 30 days: adding one month to January 31 has to
	// land on the last day of February, in a leap year and outside one.
	plain := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "add",
		"timezone": "Europe/Berlin",
		"datetime": "2026-01-31",
		"add":      "1 month",
	})
	dateTimeContains(t, plain, []string{"result: 2026-02-28T00:00:00+01:00", "clamped to the last day"})

	leap := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "add",
		"timezone": "Europe/Berlin",
		"datetime": "2028-01-31",
		"add":      "1 month",
	})
	dateTimeContains(t, leap, []string{"result: 2028-02-29T00:00:00+01:00"})

	months := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "add",
		"timezone": "Europe/Berlin",
		"datetime": "2026-01-31",
		"add":      "1 year 1 month",
	})
	dateTimeContains(t, months, []string{"add:    1 year 1 month", "result: 2027-02-28T00:00:00+01:00"})
}

func TestPreset_DateTimeDiffInDays(t *testing.T) {
	// A diff is measured on the instant timeline, and the human breakdown has
	// to agree with the requested unit.
	text := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "diff",
		"timezone": "UTC",
		"from":     "2026-01-01T00:00:00Z",
		"to":       "2026-02-07T00:00:00Z",
		"unit":     "days",
	})
	dateTimeContains(t, text, []string{
		"diff: 37 days (5 weeks 2 days)",
		"from: 2026-01-01T00:00:00+00:00",
		"to:   2026-02-07T00:00:00+00:00",
		"instant timeline",
	})

	weeks := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "diff",
		"timezone": "UTC",
		"from":     "2026-01-01T00:00:00Z",
		"to":       "2026-01-08T12:00:00Z",
		"unit":     "weeks",
	})
	dateTimeContains(t, weeks, []string{"diff: 1.071 weeks (1 week 12 hours)"})
}

func TestPreset_DateTimeWarnsOutsideTheTableWindow(t *testing.T) {
	// The table starts in 2022. A 1995 instant cannot be answered from it, so
	// the answer has to name the rule it fell back to.
	text := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "Europe/Berlin",
		"datetime": "1995-06-15 12:00",
		"format":   "%F %T %Z",
	})
	dateTimeContains(t, text, []string{
		"covers 2022-01-01 to 2041-01-01",
		"earliest recorded rule",
		"formatted: 1995-06-15 12:00:00 CET",
	})
}

func TestPreset_DateTimeUnknownTimezoneNamesCloseMatches(t *testing.T) {
	// A misspelling must produce the intended zone, not a silent success.
	text := dateTimeError(t, "get_datetime", map[string]any{
		"action":   "now",
		"timezone": "Asia/Shangai",
	})
	dateTimeContains(t, text, []string{"get_datetime: unknown timezone \"Asia/Shangai\"", "Asia/Shanghai"})

	empty := dateTimeError(t, "timezone_convert", map[string]any{
		"datetime":   "2026-01-01",
		"toTimezone": "Mars/Olympus",
	})
	dateTimeContains(t, empty, []string{"unknown timezone \"Mars/Olympus\"", "timezone_convert: "})
}

func TestPreset_DateTimeUnknownFormatTokenIsRejected(t *testing.T) {
	text := dateTimeError(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "UTC",
		"datetime": "2026-07-15T12:00:00Z",
		"format":   "%Q",
	})
	dateTimeContains(t, text, []string{"get_datetime: ", "unknown strftime token %Q", "%Y"})
}

func TestPreset_DateTimeNowIsAPlausibleTimestamp(t *testing.T) {
	// `now` is not injectable, so the shape and the epoch are what can be
	// checked: an ISO 8601 instant with a numeric offset, and an epoch within a
	// few minutes of the test's own clock.
	text := dateTimeCall(t, "get_datetime", map[string]any{"action": "now", "timezone": "UTC"})
	lines := strings.Split(text, "\n")
	if len(lines) < 3 {
		t.Fatalf("answer is too short:\n%s", text)
	}
	stamp, err := regexp.MatchString(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}[+-]\d{2}:\d{2} \(UTC\)$`, lines[0])
	if err != nil || !stamp {
		t.Fatalf("first line is not an ISO 8601 instant in UTC: %q", lines[0])
	}
	if !strings.Contains(lines[1], "DST: no") || !strings.Contains(lines[1], "UTC+00:00") {
		t.Fatalf("UTC detail line is wrong: %q", lines[1])
	}
	if !strings.Contains(lines[2], "ISO week ") || !strings.Contains(lines[2], "day ") {
		t.Fatalf("summary line is missing week/day-of-year: %q", lines[2])
	}
	epoch := regexp.MustCompile(`epoch (-?\d+) s`).FindStringSubmatch(lines[2])
	if epoch == nil {
		t.Fatalf("summary line has no epoch seconds: %q", lines[2])
	}
	value, err := strconv.ParseInt(epoch[1], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if drift := time.Since(time.Unix(value, 0)); drift < -2*time.Minute || drift > 2*time.Minute {
		t.Fatalf("epoch %d is %s from the test clock", value, drift)
	}
}

// TestPreset_DateTimeDSTTableShipped proves the 48 KiB transition table made it
// into the bundle: a fixed 40-byte offset table could not answer both halves of
// the year, and Lord Howe's 30-minute shift could not be answered at all.
func TestPreset_DateTimeDSTTableShipped(t *testing.T) {
	winter := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "America/New_York",
		"datetime": "2026-01-15T12:00:00Z",
		"format":   "%F %T %Z",
	})
	dateTimeContains(t, winter, []string{"formatted: 2026-01-15 07:00:00 EST", "UTC-05:00", "DST: no"})

	summer := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "America/New_York",
		"datetime": "2026-07-15T12:00:00Z",
		"format":   "%F %T %Z",
	})
	dateTimeContains(t, summer, []string{"formatted: 2026-07-15 08:00:00 EDT", "UTC-04:00", "DST: yes"})

	// Australia/Lord_Howe shifts by 30 minutes, so a table of whole-hour rules
	// gets both the offset and the transition wrong.
	lordHoweSummer := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "Australia/Lord_Howe",
		"datetime": "2026-01-15T00:00:00Z",
		"format":   "%F %T %z",
	})
	dateTimeContains(t, lordHoweSummer, []string{"formatted: 2026-01-15 11:00:00 +1100"})

	lordHoweWinter := dateTimeCall(t, "get_datetime", map[string]any{
		"action":   "format",
		"timezone": "Australia/Lord_Howe",
		"datetime": "2026-07-15T00:00:00Z",
		"format":   "%F %T %z",
	})
	dateTimeContains(t, lordHoweWinter, []string{"formatted: 2026-07-15 10:30:00 +1030"})
}

func TestPreset_TimezoneConvertListsEveryTargetZone(t *testing.T) {
	// The "what time is it for each of these people" shape: one instant, one
	// line per target, with weekday, abbreviation, offset and DST state.
	text := dateTimeCall(t, "timezone_convert", map[string]any{
		"datetime":   "2026-06-15T12:00:00Z",
		"toTimezone": "Asia/Shanghai",
		"targets":    []string{"America/New_York", "Europe/Berlin"},
		"format":     "%Y-%m-%d %H:%M",
	})
	dateTimeContains(t, text, []string{
		"Source: 2026-06-15T12:00:00+00:00",
		"Asia/Shanghai: 2026-06-15 20:00 | Monday | CST | UTC+08:00 | DST: no",
		"America/New_York: 2026-06-15 08:00 | Monday | EDT | UTC-04:00 | DST: yes",
		"Europe/Berlin: 2026-06-15 14:00 | Monday | CEST | UTC+02:00 | DST: yes",
	})

	// A DST change close to the instant is called out with the offsets on both
	// sides of it.
	berlin := dateTimeCall(t, "timezone_convert", map[string]any{
		"datetime":   "2026-03-29T00:30:00Z",
		"toTimezone": "Europe/Berlin",
	})
	dateTimeContains(t, berlin, []string{"local clocks move forward 1 hour on 2026-03-29", "UTC+01:00 -> UTC+02:00"})
}

func TestPreset_TimezoneConvertOffsetInInputWins(t *testing.T) {
	// An explicit offset in the datetime decides the instant; fromTimezone must
	// not move it, and the answer says so.
	text := dateTimeCall(t, "timezone_convert", map[string]any{
		"datetime":     "2026-06-15T12:00:00+02:00",
		"fromTimezone": "America/New_York",
		"toTimezone":   "UTC",
	})
	dateTimeContains(t, text, []string{
		"carried its own UTC offset (+02:00)",
		"2026-06-15T10:00:00+00:00",
	})
}

func TestPreset_DateTimeToolSurface(t *testing.T) {
	compiled := presetBundle(t, "datetime-tools")
	if compiled == nil {
		t.Skip("datetime-tools is not in the catalog")
	}
	names := compiled.ToolNames()
	if strings.Join(names, ",") != "get_datetime,timezone_convert" {
		t.Fatalf("tools = %v", names)
	}
	// A call for a name this extension does not own is left alone.
	payload, err := json.Marshal(map[string]any{"name": "echo", "arguments": map[string]any{"text": "hi"}})
	if err != nil {
		t.Fatal(err)
	}
	out, _, err := compiled.Invoke(context.Background(), "onToolCall", Context{}, payload)
	if err != nil {
		t.Fatalf("pass-through call failed: %v", err)
	}
	if !strings.Contains(string(out), "echo") {
		t.Fatalf("a foreign tool call was not passed through: %s", string(out))
	}
}

// TestPreset_DateTimeTableMatchesSystemTzdata is the guard the first version
// lacked: the table was generated from the build machine's ICU, which lagged the
// IANA release the operating system ships, so Morocco answered +01 for a date on
// which the real offset was +00. A table that disagrees with the daemon's own
// time.LoadLocation is wrong, and the disagreement is what this test looks for -
// across every shipped name, at several instants, including around transitions.
func TestPreset_DateTimeTableMatchesSystemTzdata(t *testing.T) {
	// The pinned table is generated on Linux from a pinned tzdata version;
	// zoneinfo on other hosts comes from different sources (macOS ships its
	// own builds, Windows uses the registry) and would false-fail on every
	// DST-column difference. The guard's home is the Linux CI runner, whose
	// tzdata matches the generator.
	if runtime.GOOS != "linux" {
		t.Skip("the table mirrors Linux system tzdata")
	}
	preset, ok := PresetByID("datetime-tools")
	if !ok {
		t.Fatal("datetime-tools is not in the catalog")
	}
	raw, ok := preset.Files["lib/zones.json"]
	if !ok {
		t.Fatal("the preset has no timezone table")
	}
	var table struct {
		Generated string `json:"generated"`
		Tzdb      string `json:"tzdb"`
		From      int64  `json:"from"`
		To        int64  `json:"to"`
		Fixed     map[string]struct {
			Names []string `json:"names"`
			O     []any    `json:"o"`
		} `json:"fixed"`
		Zones map[string]struct {
			Names []string `json:"names"`
			T     [][]any  `json:"t"`
		} `json:"zones"`
		Note string `json:"-"`
	}
	if err := json.Unmarshal([]byte(raw), &table); err != nil {
		t.Fatalf("the timezone table is not valid JSON: %v", err)
	}
	if table.Tzdb == "" {
		t.Fatal("the table does not record the tzdb release it was generated from")
	}
	// The DST flag is part of the check: a table whose offsets are right but
	// whose DST column is inferred reports "DST: yes" for zones that permanently
	// left daylight time (Asuncion, Vancouver, Edmonton).
	lookup := func(zone string, ts int64) (int, string, bool, bool) {
		for _, data := range table.Zones {
			if !containsString(data.Names, zone) {
				continue
			}
			offset := 0
			label := ""
			dst := false
			for _, entry := range data.T {
				at, _ := entry[0].(float64)
				if int64(at) > ts {
					break
				}
				offset = int(entry[1].(float64))
				label, _ = entry[2].(string)
				if len(entry) > 3 {
					dst, _ = entry[3].(bool)
				}
			}
			return offset, label, dst, true
		}
		for _, data := range table.Fixed {
			if !containsString(data.Names, zone) {
				continue
			}
			dst := false
			if len(data.O) > 2 {
				dst, _ = data.O[2].(bool)
			}
			return int(data.O[0].(float64)), data.O[1].(string), dst, true
		}
		return 0, "", false, false
	}
	// The table encodes one IANA release, and only that release can judge it.
	// 2026e moved Canada/Winnipeg (and its links) to EST from 2026-11-01; a
	// host still carrying 2026d answers the old rule, so comparing across
	// releases reports the release difference as a table defect. Point
	// ZONEINFO_DIR at the release the table records — the same pin the
	// generator uses — to run the comparison; a host that already carries it
	// needs no pin.
	if hostTzdb := hostTzdataVersion(); hostTzdb != "" && hostTzdb != table.Tzdb {
		t.Skipf("host tzdata %s differs from the table's %s: nothing to compare (run with ZONEINFO_DIR=<%s tree>)",
			hostTzdb, table.Tzdb, table.Tzdb)
	}
	checked := 0
	for _, data := range table.Fixed {
		for _, name := range data.Names {
			location, err := loadZone(name)
			if err != nil {
				continue
			}
			// A fixed record has one state; sample it across the window.
			for _, ts := range []int64{table.From, table.From + 365*86400, (table.From + table.To) / 2, table.To - 86400} {
				offset, label, dst, found := lookup(name, ts)
				if !found {
					t.Fatalf("%s is not in the table", name)
				}
				local := time.Unix(ts, 0).In(location)
				_, seconds := local.Zone()
				wantOffset := seconds / 60
				wantLabel := local.Format("MST")
				wantDst := expectedDst(location, ts, wantOffset, wantLabel)
				checked++
				if offset != wantOffset || label != wantLabel || (wantDst != nil && dst != *wantDst) {
					t.Fatalf("%s at %s (constant-offset record): table says %d %q dst=%v, tzdata says %d %q (table generated %s from %s)",
						name, time.Unix(ts, 0).UTC().Format(time.RFC3339), offset, label, dst, wantOffset, wantLabel, table.Generated, table.Tzdb)
				}
			}
		}
	}
	for _, data := range table.Zones {
		for _, name := range data.Names {
			location, err := loadZone(name)
			if err != nil {
				// The name is not in the tzdata this build can see: nothing to
				// compare.
				continue
			}
			// Sample the start of the window, a winter instant, a summer
			// instant, and around every recorded transition.
			samples := []int64{table.From, table.From + 180*86400, table.From + 300*86400, table.To - 86400}
			for _, entry := range data.T {
				at := int64(entry[0].(float64))
				samples = append(samples, at-3600, at, at+3600)
			}
			// The midpoint of every segment as well: a segment can be years long
			// (a zone that permanently left DST), and a flag that is wrong only
			// for that stretch would slip between the transition samples.
			for index := 0; index < len(data.T); index++ {
				at := int64(data.T[index][0].(float64))
				next := table.To
				if index+1 < len(data.T) {
					next = int64(data.T[index+1][0].(float64))
				}
				if next-at > 7200 {
					samples = append(samples, at+(next-at)/2)
				}
			}
			for _, ts := range samples {
				if ts < table.From || ts >= table.To {
					continue
				}
				offset, label, dst, found := lookup(name, ts)
				if !found {
					t.Fatalf("%s is not in the table", name)
				}
				local := time.Unix(ts, 0).In(location)
				wantOffset := 0
				if _, seconds := local.Zone(); true {
					wantOffset = seconds / 60
				}
				wantLabel := local.Format("MST")
				// Go's Location API has no isdst, so the expected flag comes from
				// the same source the generator reads: the TZif file's type
				// table, matched by offset and abbreviation. A mismatch there is
				// a mismatch in the DST column, which is what this guards.
				wantDst := expectedDst(location, ts, wantOffset, wantLabel)
				checked++
				if offset != wantOffset || label != wantLabel || (wantDst != nil && dst != *wantDst) {
					gotDst := "unknown"
					if wantDst != nil {
						gotDst = fmt.Sprintf("%v", *wantDst)
					}
					t.Fatalf("%s at %s: table says %d %q dst=%v, system tzdata says %d %q dst=%s (table generated %s from %s; regenerate with go run ./internal/extensions/presets/datetime-tools/gen)",
						name, time.Unix(ts, 0).UTC().Format(time.RFC3339), offset, label, dst, wantOffset, wantLabel, gotDst, table.Generated, table.Tzdb)
				}
			}
		}
	}
	if checked < 1000 {
		t.Fatalf("only %d comparisons ran; the table looks truncated", checked)
	}
	t.Logf("%d comparisons against the system tzdata, no disagreement", checked)
}

// TestPreset_DateTimeZeroDurationIsANoOp pins that a valid zero duration is not
// reported as unreadable input.
func TestPreset_DateTimeZeroDurationIsANoOp(t *testing.T) {
	compiled := presetBundle(t, "datetime-tools")
	if compiled == nil {
		t.Fatal("datetime-tools is not in the catalog")
	}
	text := presetTool(t, compiled, "get_datetime", map[string]any{
		"action": "add", "timezone": "UTC", "datetime": "2026-06-15T12:00:00Z", "add": "0 seconds",
	})
	for _, want := range []string{"no change", "result: 2026-06-15T12:00:00+00:00", "0 seconds"} {
		if !strings.Contains(text, want) {
			t.Fatalf("zero-duration answer is missing %q:\n%s", want, text)
		}
	}
}

// tzifCounts are the six counts a TZif header carries.
type tzifCounts struct {
	isUt, isStd, leap, time, typ, char int
}

func tzifReadCounts(data []byte, at int) (tzifCounts, bool) {
	if at+44 > len(data) || string(data[at:at+4]) != "TZif" {
		return tzifCounts{}, false
	}
	read := func(offset int) int {
		return int(binary.BigEndian.Uint32(data[at+20+offset : at+24+offset]))
	}
	return tzifCounts{
		isUt: read(0), isStd: read(4), leap: read(8), time: read(12), typ: read(16), char: read(20),
	}, true
}

// tzifBlockSize is the size of one data block with the given time width.
func tzifBlockSize(counts tzifCounts, timeSize int) int {
	return counts.time*timeSize + counts.time + counts.typ*6 + counts.char +
		counts.leap*(timeSize+4) + counts.isStd + counts.isUt
}

// tzifDstAt reports the DST flag of the type in effect at ts, matched to the
// offset and abbreviation so a wrong type index cannot pass.
func tzifDstAt(data []byte, ts int64, offset int, label string) *bool {
	if len(data) < 44 || string(data[0:4]) != "TZif" {
		return nil
	}
	first, ok := tzifReadCounts(data, 0)
	if !ok {
		return nil
	}
	// A version 2 or later file repeats the header with 64-bit times, and that
	// second block is the one with the full transition list.
	timeSize := 4
	headerAt := 0
	if data[4] >= '2' {
		secondHeader := 44 + tzifBlockSize(first, 4)
		if _, ok := tzifReadCounts(data, secondHeader); ok {
			timeSize = 8
			headerAt = secondHeader
		}
	}
	counts, ok := tzifReadCounts(data, headerAt)
	if !ok {
		return nil
	}
	timesStart := headerAt + 44
	indexesStart := timesStart + counts.time*timeSize
	typesStart := indexesStart + counts.time
	charsStart := typesStart + counts.typ*6
	if charsStart+counts.char > len(data) || counts.time == 0 || counts.typ == 0 {
		return nil
	}
	at := func(index int) int64 {
		if timeSize == 8 {
			return int64(binary.BigEndian.Uint64(data[timesStart+index*8 : timesStart+index*8+8]))
		}
		return int64(int32(binary.BigEndian.Uint32(data[timesStart+index*4 : timesStart+index*4+4])))
	}
	typeFor := func(index int) *bool {
		base := typesStart + index*6
		if base+6 > len(data) {
			return nil
		}
		seconds := int(int32(binary.BigEndian.Uint32(data[base : base+4])))
		dst := data[base+4] != 0
		designation := int(data[base+5])
		name := ""
		if designation < counts.char {
			begin := charsStart + designation
			end := begin
			for end < charsStart+counts.char && data[end] != 0 {
				end++
			}
			name = string(data[begin:end])
		}
		if seconds/60 != offset || name != label {
			return nil
		}
		return &dst
	}
	// The state at ts is the last transition at or before it.
	chosen := -1
	for index := 0; index < counts.time; index++ {
		if at(index) <= ts {
			chosen = index
		} else {
			break
		}
	}
	if chosen < 0 {
		return nil
	}
	return typeFor(int(data[indexesStart+chosen]))
}

// expectedDst reads the DST flag tzdata records for the segment in effect at an
// instant, from the zone's TZif file. It returns nil when the file cannot be
// read, so a platform without zoneinfo skips the DST half rather than failing.
func expectedDst(location *time.Location, ts int64, offset int, label string) *bool {
	name := location.String()
	if name == "Local" || name == "" {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(testZoneInfoRoot(), name))
	if err != nil {
		return nil
	}
	return tzifDstAt(data, ts, offset, label)
}

// testZoneInfoRoot resolves the zoneinfo tree the table is checked against:
// the pinned release when ZONEINFO_DIR names one, otherwise the host's.
func testZoneInfoRoot() string {
	if root := os.Getenv("ZONEINFO_DIR"); root != "" {
		return root
	}
	root := "/usr/share/zoneinfo"
	if resolved, err := filepath.EvalSymlinks(root); err == nil {
		root = resolved
	}
	return root
}

// hostTzdataVersion names the release that tree carries, so the DST column is
// only compared against the release the table was generated from. Every
// distribution writes it in +VERSION; tzdata.zi names it on its first
// "# version" line.
func hostTzdataVersion() string {
	root := testZoneInfoRoot()
	for _, candidate := range []string{"+VERSION", "tzdata.zi"} {
		data, err := os.ReadFile(filepath.Join(root, candidate))
		if err != nil {
			continue
		}
		if candidate == "tzdata.zi" {
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "# version ") {
					return strings.TrimSpace(strings.TrimPrefix(line, "# version "))
				}
			}
			continue
		}
		return strings.TrimSpace(string(data))
	}
	return ""
}

// loadZone loads a zone from the pinned tzdata tree when one is configured
// (ZONEINFO_DIR, as the generator uses), falling back to the host's.
func loadZone(name string) (*time.Location, error) {
	if root := os.Getenv("ZONEINFO_DIR"); root != "" {
		if location, err := time.LoadLocationFromTZData(name, mustReadZoneFile(root, name)); err == nil {
			return location, nil
		}
	}
	return time.LoadLocation(name)
}

func mustReadZoneFile(root, name string) []byte {
	data, err := os.ReadFile(filepath.Join(root, name))
	if err != nil {
		return nil
	}
	return data
}
