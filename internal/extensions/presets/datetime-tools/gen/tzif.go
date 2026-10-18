package main

// Reads a zone's transitions straight out of its TZif file.
//
// The TZif format stores, per transition, the UTC offset, the DST flag and the
// abbreviation index, which is exactly what the preset's table needs. Parsing it
// here is more reliable than shelling out to zdump: the BSD zdump on macOS does
// not expand the footer's POSIX rule past the last explicit transition (it
// reported nothing for Nuuk between 2038 and 2041), and Go's time.Location does
// not expose the DST flag at all.
//
// Format reference: RFC 8536. A version 2+ file carries a 32-bit block followed
// by a 64-bit one; the 64-bit block is the one to read for a table that runs to
// 2041.

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

type tzState struct {
	at     int64
	offset int
	abbr   string
	dst    bool
}

type tzif struct {
	transitions []tzState
	// rule is the footer's POSIX rule, expanded for instants past the last
	// explicit transition.
	rule *posixRule
	// types is the raw ttinfo list, used when an instant predates the first
	// transition (RFC 8536 says to use the first standard-time type then).
	types []tzState
}

func readTZif(zone string) (*tzif, error) {
	path := filepath.Join(zoneInfoRoot(), zone)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) < 44 || string(data[0:4]) != "TZif" {
		return nil, fmt.Errorf("%s is not a TZif file", zone)
	}
	version := data[4]
	// A version 2 or later file has a second header with 64-bit times.
	block := data
	if version >= '2' {
		first, err := skipBlock(data, 4)
		if err != nil {
			return nil, err
		}
		block = data[first:]
		if len(block) < 44 || string(block[0:4]) != "TZif" {
			return nil, fmt.Errorf("%s: the second TZif block is missing", zone)
		}
		timeSize := 8
		return parseBlock(block, timeSize)
	}
	return parseBlock(block, 4)
}

// skipBlock returns the offset just past one complete TZif block whose time
// values are 4 bytes wide.
func skipBlock(data []byte, headerSize int) (int, error) {
	counts := data[20:44]
	timeCount := int(binary.BigEndian.Uint32(counts[12:16]))
	typeCount := int(binary.BigEndian.Uint32(counts[16:20]))
	charCount := int(binary.BigEndian.Uint32(counts[20:24]))
	leapCount := int(binary.BigEndian.Uint32(counts[8:12]))
	isStdCount := int(binary.BigEndian.Uint32(counts[4:8]))
	isUtCount := int(binary.BigEndian.Uint32(counts[0:4]))
	size := 44 + timeCount*4 + timeCount + typeCount*6 + charCount + leapCount*8 + isStdCount + isUtCount
	if headerSize != 0 {
		size += 0
	}
	if len(data) < size {
		return 0, fmt.Errorf("TZif block is truncated")
	}
	return size, nil
}

func parseBlock(data []byte, timeSize int) (*tzif, error) {
	counts := data[20:44]
	isUtCount := int(binary.BigEndian.Uint32(counts[0:4]))
	isStdCount := int(binary.BigEndian.Uint32(counts[4:8]))
	leapCount := int(binary.BigEndian.Uint32(counts[8:12]))
	timeCount := int(binary.BigEndian.Uint32(counts[12:16]))
	typeCount := int(binary.BigEndian.Uint32(counts[16:20]))
	charCount := int(binary.BigEndian.Uint32(counts[20:24]))
	position := 44
	if len(data) < position+timeCount*timeSize+timeCount+typeCount*6+charCount {
		return nil, fmt.Errorf("TZif block is truncated")
	}
	times := make([]int64, timeCount)
	for index := 0; index < timeCount; index++ {
		if timeSize == 8 {
			times[index] = int64(binary.BigEndian.Uint64(data[position : position+8]))
			position += 8
		} else {
			times[index] = int64(int32(binary.BigEndian.Uint32(data[position : position+4])))
			position += 4
		}
	}
	indexes := data[position : position+timeCount]
	position += timeCount
	typeBytes := data[position : position+typeCount*6]
	position += typeCount * 6
	characters := data[position : position+charCount]

	types := make([]tzState, typeCount)
	for index := 0; index < typeCount; index++ {
		base := index * 6
		offset := int(int32(binary.BigEndian.Uint32(typeBytes[base : base+4])))
		isdst := typeBytes[base+4] != 0
		designation := int(typeBytes[base+5])
		name := ""
		if designation < len(characters) {
			end := designation
			for end < len(characters) && characters[end] != 0 {
				end++
			}
			name = string(characters[designation:end])
		}
		types[index] = tzState{offset: offset / 60, abbr: name, dst: isdst}
	}
	states := make([]tzState, 0, timeCount)
	for index := 0; index < timeCount; index++ {
		typeIndex := int(indexes[index])
		if typeIndex >= len(types) {
			continue
		}
		state := types[typeIndex]
		state.at = times[index]
		states = append(states, state)
	}
	_ = leapCount
	_ = isStdCount
	_ = isUtCount
	parsed := &tzif{transitions: states, types: types}
	// The footer sits after the 64-bit block's data.
	footerStart := position + leapCount*12 + isStdCount + isUtCount
	if footerStart < len(data) {
		if rule, err := readFooter(data[footerStart:]); err == nil {
			parsed.rule = rule
		}
	}
	return parsed, nil
}

// zoneInfoRoot resolves the system zoneinfo directory, following the symlink
// macOS uses (/usr/share/zoneinfo -> /var/db/timezone/zoneinfo).
func zoneInfoRoot() string {
	// ZONEINFO_DIR points at a compiled release (a zic output tree), so the table
	// can be generated from a pinned tzdata rather than from whatever the host
	// ships. ZONEINFO is honoured too: Go's toolchain uses it for the same
	// purpose.
	for _, name := range []string{"ZONEINFO_DIR", "ZONEINFO"} {
		if value := os.Getenv(name); value != "" {
			if resolved, err := filepath.EvalSymlinks(value); err == nil {
				return resolved
			}
			return value
		}
	}
	if resolved, err := filepath.EvalSymlinks("/usr/share/zoneinfo"); err == nil {
		return resolved
	}
	return "/usr/share/zoneinfo"
}
