package main

// Expands the POSIX TZ rule in a TZif footer into explicit transitions.
//
// A TZif file stores transitions explicitly only as far as its author wrote them
// (commonly to 2037); past that, the footer's rule decides. The preset's table
// runs to 2041, so the rule has to be expanded - otherwise the last explicit
// state is repeated and a summer date near the end of the window answers with
// winter time.
//
// Format (POSIX TZ, as used in the TZif footer after a newline):
//
//	std offset [dst [offset] [,start[/time],end[/time]]]
//
// The offsets are positive *west* of Greenwich, so "CET-1" is UTC+01. Rules are
// Mm.w.d (month, week, weekday), Jn (1-365, no leap day) or n (0-365, with leap
// day), and default to 02:00 local time.

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type posixRule struct {
	stdName string
	stdOff  int // minutes east of Greenwich, the sign already flipped
	dstName string
	dstOff  int
	hasDst  bool
	start   posixDateRule
	end     posixDateRule
}

type posixDateRule struct {
	kind    byte // 'M', 'J', or 'N' for a plain day number
	month   int
	week    int
	weekday int
	day     int
	seconds int // local time of day the change happens, in seconds
}

// parsePosixRule parses a footer string such as "CET-1CEST,M3.5.0,M10.5.0/3".
func parsePosixRule(text string) (*posixRule, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil, fmt.Errorf("empty rule")
	}
	reader := &posixReader{text: text}
	rule := &posixRule{}
	var err error
	if rule.stdName, err = reader.readName(); err != nil {
		return nil, err
	}
	if rule.stdOff, err = reader.readOffset(); err != nil {
		return nil, err
	}
	if reader.done() {
		return rule, nil
	}
	rule.hasDst = true
	if rule.dstName, err = reader.readName(); err != nil {
		return nil, err
	}
	// The DST offset is optional and defaults to one hour ahead of standard.
	if !reader.done() && reader.peek() != ',' {
		offset, offsetErr := reader.readOffset()
		if offsetErr != nil {
			return nil, offsetErr
		}
		rule.dstOff = offset
	} else {
		rule.dstOff = rule.stdOff + 60
	}
	if reader.done() || reader.peek() != ',' {
		return rule, nil
	}
	_ = reader.take()
	if rule.start, err = reader.readDateRule(); err != nil {
		return nil, err
	}
	if reader.done() || reader.peek() != ',' {
		return nil, fmt.Errorf("the rule has no end date")
	}
	_ = reader.take()
	if rule.end, err = reader.readDateRule(); err != nil {
		return nil, err
	}
	return rule, nil
}

type posixReader struct {
	text     string
	position int
}

func (reader *posixReader) done() bool { return reader.position >= len(reader.text) }
func (reader *posixReader) peek() byte { return reader.text[reader.position] }
func (reader *posixReader) take() byte {
	value := reader.text[reader.position]
	reader.position++
	return value
}

func (reader *posixReader) readName() (string, error) {
	if reader.done() {
		return "", fmt.Errorf("the rule ends where a name was expected")
	}
	if reader.peek() == '<' {
		reader.take()
		start := reader.position
		for !reader.done() && reader.peek() != '>' {
			reader.position++
		}
		if reader.done() {
			return "", fmt.Errorf("an unterminated quoted name")
		}
		name := reader.text[start:reader.position]
		reader.take()
		return name, nil
	}
	start := reader.position
	for !reader.done() {
		character := reader.peek()
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') {
			reader.position++
			continue
		}
		break
	}
	if reader.position == start {
		return "", fmt.Errorf("a name was expected at %q", reader.text[start:])
	}
	return reader.text[start:reader.position], nil
}

// readOffset reads [+-]hh[:mm[:ss]] and returns minutes east of Greenwich. POSIX
// counts offsets the other way round, which is why the sign is flipped.
func (reader *posixReader) readOffset() (int, error) {
	sign := 1
	if !reader.done() && (reader.peek() == '+' || reader.peek() == '-') {
		if reader.take() == '-' {
			sign = -1
		}
	}
	// Accumulate each colon-separated part with its own weight. The first
	// version multiplied the running total by the next weight, which turned
	// "3:30" into 648030 seconds instead of 12600.
	weights := []int{3600, 60, 1}
	seconds := 0
	parts := 0
	for {
		value := 0
		digits := 0
		for !reader.done() && reader.peek() >= '0' && reader.peek() <= '9' {
			value = value*10 + int(reader.take()-'0')
			digits++
		}
		if digits == 0 {
			return 0, fmt.Errorf("an offset was expected")
		}
		if parts >= len(weights) {
			return 0, fmt.Errorf("an offset has more than three parts")
		}
		seconds += value * weights[parts]
		parts++
		if !reader.done() && reader.peek() == ':' && parts < len(weights) {
			reader.take()
			continue
		}
		break
	}
	// POSIX: "the value is added to local time to get UTC", so a positive number
	// means west of Greenwich.
	return -sign * seconds / 60, nil
}

func (reader *posixReader) readDateRule() (posixDateRule, error) {
	rule := posixDateRule{seconds: 2 * 3600}
	if reader.done() {
		return rule, fmt.Errorf("a date rule was expected")
	}
	switch reader.take() {
	case 'M':
		rule.kind = 'M'
		rule.month = reader.readNumber()
		if reader.done() || reader.take() != '.' {
			return rule, fmt.Errorf("a date rule needs the week")
		}
		rule.week = reader.readNumber()
		if reader.done() || reader.take() != '.' {
			return rule, fmt.Errorf("a date rule needs the weekday")
		}
		rule.weekday = reader.readNumber()
	case 'J':
		rule.kind = 'J'
		rule.day = reader.readNumber()
	default:
		reader.position--
		rule.kind = 'N'
		rule.day = reader.readNumber()
	}
	if !reader.done() && reader.peek() == '/' {
		reader.take()
		sign := 1
		if !reader.done() && reader.peek() == '-' {
			reader.take()
			sign = -1
		}
		seconds := 0
		parts := 0
		for {
			value := reader.readNumber()
			parts++
			if parts == 1 {
				seconds += value * 3600
			} else if parts == 2 {
				seconds += value * 60
			} else {
				seconds += value
			}
			if !reader.done() && reader.peek() == ':' && parts < 3 {
				reader.take()
				continue
			}
			break
		}
		rule.seconds = sign * seconds
	}
	return rule, nil
}

func (reader *posixReader) readNumber() int {
	value := 0
	for !reader.done() && reader.peek() >= '0' && reader.peek() <= '9' {
		value = value*10 + int(reader.take()-'0')
	}
	return value
}

// dayOf returns the UTC instant the rule fires in a given year.
func (rule posixDateRule) instant(year int, offsetMinutes int) time.Time {
	var date time.Time
	switch rule.kind {
	case 'M':
		// The week-th weekday of the month; week 5 means the last one.
		first := time.Date(year, time.Month(rule.month), 1, 0, 0, 0, 0, time.UTC)
		delta := (rule.weekday - int(first.Weekday()) + 7) % 7
		day := 1 + delta + (rule.week-1)*7
		date = time.Date(year, time.Month(rule.month), day, 0, 0, 0, 0, time.UTC)
		for date.Month() != time.Month(rule.month) {
			date = date.AddDate(0, 0, -7)
		}
	case 'J':
		date = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, rule.day-1)
		if rule.day > 59 && isLeap(year) {
			date = date.AddDate(0, 0, 1)
		}
	default:
		date = time.Date(year, 1, 1, 0, 0, 0, 0, time.UTC).AddDate(0, 0, rule.day)
	}
	// The stored time is local to the state that is ending, so subtract its
	// offset to get UTC.
	return date.Add(time.Duration(rule.seconds)*time.Second - time.Duration(offsetMinutes)*time.Minute)
}

func isLeap(year int) bool {
	return year%4 == 0 && (year%100 != 0 || year%400 == 0)
}

// expandRule returns the transitions the rule produces in [from, to).
func expandRule(rule *posixRule, from, to int64) []transition {
	if rule == nil || !rule.hasDst {
		return nil
	}
	var states []transition
	startYear := time.Unix(from, 0).UTC().Year() - 1
	endYear := time.Unix(to, 0).UTC().Year() + 1
	for year := startYear; year <= endYear; year++ {
		// The DST season opens at the start rule (in standard time) and closes
		// at the end rule (in daylight time).
		opening := rule.start.instant(year, rule.stdOff)
		closing := rule.end.instant(year, rule.dstOff)
		for _, candidate := range []struct {
			at   time.Time
			dst  bool
			name string
			off  int
		}{
			{opening, true, rule.dstName, rule.dstOff},
			{closing, false, rule.stdName, rule.stdOff},
		} {
			seconds := candidate.at.Unix()
			if seconds < from || seconds >= to {
				continue
			}
			states = append(states, transition{at: seconds, offset: candidate.off, abbr: candidate.name, dst: candidate.dst})
		}
	}
	return states
}

// readFooter returns the POSIX rule string from a TZif file, if it has one.
func readFooter(data []byte) (*posixRule, error) {
	// The footer is the last non-empty line: the file ends with "\n<rule>\n",
	// so splitting on newlines and taking the last piece that looks like a rule
	// is what finds it.
	lines := strings.Split(string(data), "\n")
	for index := len(lines) - 1; index >= 0; index-- {
		candidate := strings.TrimSpace(lines[index])
		if candidate == "" {
			continue
		}
		if candidate[0] == 'M' || candidate[0] == '<' || (candidate[0] >= 'A' && candidate[0] <= 'Z') {
			// A bare zone abbreviation also starts with a letter, so the parse
			// is what decides: a real rule carries an offset after the name.
			if rule, err := parsePosixRule(candidate); err == nil {
				return rule, nil
			}
		}
	}
	return nil, fmt.Errorf("no footer rule")
}

var _ = strconv.Itoa
