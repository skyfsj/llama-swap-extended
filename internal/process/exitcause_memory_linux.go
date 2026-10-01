//go:build linux

package process

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// cgroup v2 counters live at a fixed path inside the container's own mount
// namespace, which is exactly the budget an OOM-adjacent death overran.
const (
	cgroupMemoryMaxPath   = "/sys/fs/cgroup/memory.max"
	cgroupMemoryPeakPath  = "/sys/fs/cgroup/memory.peak"
	cgroupMemoryEventPath = "/sys/fs/cgroup/memory.events"
)

// memoryPressureNote summarizes the memory conditions at the moment of death.
//
// It exists because memory exhaustion is the silent failure mode: the upstream
// prints no traceback, takes no signal, and simply stops mid-line. Without this
// note an operator has to reconstruct the cause from cgroup counters after the
// fact — the exact archaeology this diagnostic set out to remove.
//
// Every read is best-effort. An unreadable or non-cgroup host yields an empty
// note rather than an error, because this runs on the death-reporting path and
// must never mask the exit reason it is annotating.
func memoryPressureNote() string {
	var notes []string
	if max, ok := readCgroupBytes(cgroupMemoryMaxPath); ok && max > 0 {
		if peak, peakOK := readCgroupBytes(cgroupMemoryPeakPath); peakOK && peak > 0 {
			notes = append(notes, fmt.Sprintf("container memory peak %s of %s limit",
				formatGiB(peak), formatGiB(max)))
			if peak >= max {
				notes = append(notes, "peak reached the container limit")
			}
		}
	}
	if events, err := os.ReadFile(cgroupMemoryEventPath); err == nil {
		for _, line := range strings.Split(string(events), "\n") {
			fields := strings.Fields(line)
			if len(fields) != 2 || fields[0] != "oom_kill" {
				continue
			}
			if count, convErr := strconv.ParseUint(fields[1], 10, 64); convErr == nil && count > 0 {
				notes = append(notes, fmt.Sprintf("%d cgroup OOM kill(s) recorded", count))
			}
		}
	}
	if len(notes) == 0 {
		return ""
	}
	return "memory: " + strings.Join(notes, "; ")
}

func readCgroupBytes(path string) (uint64, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	text := strings.TrimSpace(string(data))
	if text == "" || text == "max" {
		return 0, false
	}
	value, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		return 0, false
	}
	return value, true
}

func formatGiB(bytes uint64) string {
	return fmt.Sprintf("%.1f GiB", float64(bytes)/float64(int64(1)<<30))
}

// memoryKillNote reports only the hard evidence of a memory kill, with no
// speculation about who caused it. Used when llama-swap itself sent the
// signal, so "the usual cause is the OOM killer" would be a guess the log
// cannot support.
func memoryKillNote() string {
	events, err := os.ReadFile(cgroupMemoryEventPath)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(events), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || fields[0] != "oom_kill" {
			continue
		}
		if count, convErr := strconv.ParseUint(fields[1], 10, 64); convErr == nil && count > 0 {
			return fmt.Sprintf("memory: %d cgroup OOM kill(s) recorded", count)
		}
	}
	return ""
}
