package main

// Generates the datetime-tools preset's timezone table from the Go toolchain's
// own tzdata, which is what the daemon uses at runtime.
//
// Why not Node's Intl: the first version of this table was generated from ICU,
// and ICU on the build machine lagged the IANA release that the operating system
// ships. Morocco became permanent UTC+00 in tz 2026c; the table still said +01,
// so the preset answered with the wrong hour for a real place today. Generating
// from Go keeps the table on the same release the rest of the daemon walks by,
// and recording that release in the file makes the drift visible.
//
// Run with: go run ./internal/extensions/presets/datetime-tools/gen
import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const (
	windowStart = "2022-01-01T00:00:00Z"
	windowEnd   = "2041-01-01T00:00:00Z"
)

type zoneRecord struct {
	Names []string `json:"names"`
	O     []any    `json:"o,omitempty"`
	T     [][]any  `json:"t,omitempty"`
}

type table struct {
	Generated string                `json:"generated"`
	Tzdb      string                `json:"tzdb"`
	From      int64                 `json:"from"`
	To        int64                 `json:"to"`
	Fixed     map[string]zoneRecord `json:"fixed"`
	Zones     map[string]zoneRecord `json:"zones"`
}

func main() {
	start, err := time.Parse(time.RFC3339, windowStart)
	if err != nil {
		fatal(err)
	}
	end, err := time.Parse(time.RFC3339, windowEnd)
	if err != nil {
		fatal(err)
	}
	names := zoneNames()
	// Group names that behave identically, so an alias costs nothing: the check
	// is on the full transition list and the abbreviation, not just the offset.
	groups := map[string]*zoneRecord{}
	var order []string
	for _, name := range names {
		location, err := time.LoadLocation(name)
		if err != nil {
			continue
		}
		built := build(location, name, start, end)
		key := signature(built)
		existing, seen := groups[key]
		if seen {
			existing.Names = append(existing.Names, name)
			continue
		}
		built.Names = []string{name}
		groups[key] = &built
		order = append(order, key)
	}
	result := table{
		Generated: time.Now().UTC().Format("2006-01-02"),
		Tzdb:      tzdataVersion(),
		From:      start.Unix(),
		To:        end.Unix(),
		Fixed:     map[string]zoneRecord{},
		Zones:     map[string]zoneRecord{},
	}
	sort.Strings(order)
	for _, key := range order {
		record := groups[key]
		sort.Strings(record.Names)
		if len(record.T) > 0 {
			result.Zones[record.Names[0]] = *record
		} else {
			result.Fixed[record.Names[0]] = *record
		}
	}
	target := filepath.Join("internal", "extensions", "presets", "datetime-tools", "lib", "zones.json")
	encoded, err := json.Marshal(result)
	if err != nil {
		fatal(err)
	}
	if err := os.WriteFile(target, encoded, 0o600); err != nil {
		fatal(err)
	}
	fmt.Printf("wrote %s: %d fixed, %d with transitions, %d bytes\n",
		target, len(result.Fixed), len(result.Zones), len(encoded))
}

// transition is one tzdata state change, with the DST flag tzdata itself records.
type transition struct {
	at     int64
	offset int
	abbr   string
	dst    bool
}

// The TZif reader produces the same four fields, so the conversion is a cast.

// readTransitions reads the zone's transitions from its TZif file, which carries
// the DST flag per segment. zdump was the first choice, but the BSD version on
// macOS does not expand the footer rule past the final explicit transition, so a
// query between 2038 and 2041 answered with the wrong half of the year.
func readTransitions(zone string, start, end time.Time) []transition {
	file, err := readTZif(zone)
	if err != nil {
		return nil
	}
	var parsed []transition
	for _, entry := range file.transitions {
		// tzState and transition have the same shape on purpose: the TZif reader
		// and the table writer both speak (instant, offset, abbreviation, dst).
		parsed = append(parsed, transition(entry))
	}
	// Past its last explicit transition a TZif file is governed by the footer's
	// POSIX rule; without expanding it, a 2039 summer date repeats the last
	// written state and answers with winter time.
	if len(parsed) > 0 && file.rule != nil {
		parsed = append(parsed, expandRule(file.rule, parsed[len(parsed)-1].at, end.Unix())...)
	}
	if len(parsed) == 0 {
		return nil
	}
	sort.Slice(parsed, func(left, right int) bool { return parsed[left].at < parsed[right].at })
	return parsed
}

// build turns the transitions into a table record. The first entry is the state
// at the window start (not a transition); each later entry carries the DST flag
// tzdata reports for the segment it opens.
func build(location *time.Location, zone string, start, end time.Time) zoneRecord {
	transitions := readTransitions(zone, start, end)
	startUnix, endUnix := start.Unix(), end.Unix()
	if len(transitions) == 0 {
		// No zdump (or a zone it does not know): fall back to a single entry
		// from Go, with DST unknown and therefore reported as false.
		local := start.In(location)
		_, offset := local.Zone()
		return zoneRecord{O: []any{offset / 60, local.Format("MST"), false}}
	}
	// The state at the window start is the last transition at or before it.
	current := transitions[0]
	for _, entry := range transitions {
		if entry.at <= startUnix {
			current = entry
		}
	}
	record := zoneRecord{T: [][]any{{startUnix, current.offset, current.abbr, current.dst}}}
	for _, entry := range transitions {
		if entry.at <= startUnix || entry.at >= endUnix {
			continue
		}
		last := record.T[len(record.T)-1]
		if last[1] == entry.offset && last[2] == entry.abbr && last[3] == entry.dst {
			continue
		}
		record.T = append(record.T, []any{entry.at, entry.offset, entry.abbr, entry.dst})
	}
	// A zone whose state never changes over the window is a fixed entry, which
	// keeps the table small and the lookup simple.
	if len(record.T) == 1 {
		only := record.T[0]
		return zoneRecord{O: []any{only[1], only[2], only[3]}}
	}
	return record
}

// tzdataVersion reports the IANA release the system zoneinfo carries. Every
// distribution writes it in +VERSION; when it is missing the source is still
// named, but the test then has no release to compare against.
func tzdataVersion() string {
	for _, candidate := range []string{"+VERSION", "tzdata.zi"} {
		data, err := os.ReadFile(filepath.Join(zoneInfoRoot(), candidate))
		if err != nil {
			continue
		}
		if candidate == "tzdata.zi" {
			// tzdata.zi is the whole compiled source: the release is its first
			// "	# version <release>" line, not the file.
			for _, line := range strings.Split(string(data), "\n") {
				if strings.HasPrefix(line, "# version ") {
					return strings.TrimSpace(strings.TrimPrefix(line, "# version "))
				}
			}
			continue
		}
		return strings.TrimSpace(string(data))
	}
	return "unknown (system zoneinfo)"
}

func signature(record zoneRecord) string {
	encoded, _ := json.Marshal(record)
	return string(encoded)
}

// zoneNames lists every zone the zoneinfo directory knows, in the "Area/Place"
// form the IANA database uses.
func zoneNames() []string {
	// /usr/share/zoneinfo is a symlink on macOS (/etc/localtime's target), and
	// filepath.Walk does not follow a symlinked root: it would report the link
	// itself as the single file. Resolve it first.
	// ZONEINFO_DIR pins the release: this machine's tzdata lagged IANA 2026e, in
	// which Manitoba moved to permanent EST. Generating from whatever the host
	// happens to have is how the shipped table went stale in the first place.
	roots := []string{zoneInfoRoot()}
	var names []string
	seen := map[string]bool{}
	for _, candidate := range roots {
		root, err := filepath.EvalSymlinks(candidate)
		if err != nil {
			continue
		}
		_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil || info.IsDir() {
				return nil
			}
			relative, relErr := filepath.Rel(root, path)
			if relErr != nil {
				return nil
			}
			relative = filepath.ToSlash(relative)
			switch {
			case relative == "" || relative[0] == '.':
				return nil
			case len(relative) > 0 && (relative[0] == '+' || relative[0] == '-'):
				return nil
			case contains(relative, "posix/") || contains(relative, "right/"):
				return nil
			case relative == "iso3166.tab" || relative == "zone.tab" || relative == "leapseconds" || relative == "tzdata.zi":
				return nil
			}
			if !seen[relative] {
				seen[relative] = true
				names = append(names, relative)
			}
			return nil
		})
	}
	sort.Strings(names)
	return names
}

func contains(value, part string) bool {
	for index := 0; index+len(part) <= len(value); index++ {
		if value[index:index+len(part)] == part {
			return true
		}
	}
	return false
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
