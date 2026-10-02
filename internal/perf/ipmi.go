package perf

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// ErrNoNodePowerTool reports that no IPMI tool is available for node-level
// power readings; monitoring falls back to GPU-level power only.
var ErrNoNodePowerTool = errors.New("no node power tool available")

// nodePowerEvery is the fixed sampling cadence for the IPMI collector. It is
// independent of performance.every because each reading shells out to
// ipmitool, which is far heavier than parsing nvidia-smi's streaming output.
const nodePowerEvery = 30 * time.Second

// nodePowerTimeout bounds a single ipmitool invocation; a BMC that does not
// answer within it is treated as unavailable for that round.
const nodePowerTimeout = 5 * time.Second

// ipmiProbeBackoffBase scales how long the collector waits after failures
// before probing again, avoiding a permanent stream of failing exec calls.
const ipmiProbeBackoffBase = 3

// dcmiPowerRe matches the "Instantaneous power reading" line of
// `ipmitool dcmi power` output.
var dcmiPowerRe = regexp.MustCompile(`Instantaneous power reading:\s+([0-9]+(?:\.[0-9]+)?)\s*Watts`)

// sensorPowerNames are sensor labels, matched case-insensitively, whose
// wattage reading represents whole-node power draw. Order is preference.
var sensorPowerNames = []string{"pwr consumption", "total power", "system level", "node power", "psu"}

// ParseIpmiDcmiPower extracts the instantaneous platform power in watts from
// `ipmitool dcmi power` output.
func ParseIpmiDcmiPower(output string) (float64, bool) {
	match := dcmiPowerRe.FindStringSubmatch(output)
	if match == nil {
		return 0, false
	}
	watts, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, false
	}
	return watts, true
}

// ParseIpmiSensorPower extracts a whole-node wattage from `ipmitool sensor`
// output. Each line looks like "Sensor Name | 42.000 | Watts | ok | ...".
// Rows are matched by known sensor names first; when none matches and there
// is exactly one Watts row, that row is used.
func ParseIpmiSensorPower(output string) (float64, string, bool) {
	type candidate struct {
		name  string
		watts float64
		rank  int
	}

	var found []candidate
	for _, line := range strings.Split(output, "\n") {
		fields := strings.Split(line, "|")
		if len(fields) < 3 {
			continue
		}
		name := strings.TrimSpace(fields[0])
		if !strings.EqualFold(strings.TrimSpace(fields[2]), "Watts") {
			continue
		}
		watts, err := strconv.ParseFloat(strings.TrimSpace(fields[1]), 64)
		if err != nil {
			continue
		}

		rank := -1
		for i, pattern := range sensorPowerNames {
			if strings.Contains(strings.ToLower(name), pattern) {
				rank = i
				break
			}
		}
		if rank >= 0 {
			found = append(found, candidate{name, watts, rank})
		} else {
			found = append(found, candidate{name, watts, len(sensorPowerNames)})
		}
	}

	if len(found) == 0 {
		return 0, "", false
	}

	// Best rank wins; ties keep the first row. Unmatched rows only count when
	// they are the only wattage source, so an unrelated PSU figure is not
	// mistaken for node power when a proper sensor also exists.
	best := found[0]
	for _, c := range found[1:] {
		if c.rank < best.rank {
			best = c
		}
	}
	if best.rank >= len(sensorPowerNames) && len(found) > 1 {
		return 0, "", false
	}
	return best.watts, best.name, true
}

// sampleNodePower runs one ipmitool round: DCMI first, plain sensors as
// fallback. The returned source names the mechanism that produced the value.
func sampleNodePower(ctx context.Context, tool string) (NodeStat, bool) {
	ctx, cancel := context.WithTimeout(ctx, nodePowerTimeout)
	defer cancel()

	// Newer BMCs accept `dcmi power`; older Supermicro firmware prints the
	// subcommand help unless the reading form is spelled out fully.
	for _, args := range [][]string{
		{"dcmi", "power", "reading"},
		{"dcmi", "power"},
	} {
		if out, err := exec.CommandContext(ctx, tool, args...).Output(); err == nil {
			if watts, ok := ParseIpmiDcmiPower(string(out)); ok {
				return NodeStat{Timestamp: time.Now(), PowerW: watts, Source: "ipmi-dcmi"}, true
			}
		}
	}
	if out, err := exec.CommandContext(ctx, tool, "sensor").Output(); err == nil {
		if watts, name, ok := ParseIpmiSensorPower(string(out)); ok {
			return NodeStat{Timestamp: time.Now(), PowerW: watts, Source: "ipmi-sensor:" + name}, true
		}
	}
	return NodeStat{}, false
}

// collectNodePower samples node power at nodePowerEvery. It keeps probing
// with exponential backoff after failures so a BMC that appears later (or a
// transient BMC outage) recovers automatically, logging only state changes.
func collectNodePower(ctx context.Context, command string, log *logmon.Monitor) (<-chan NodeStat, error) {
	tool := strings.TrimSpace(command)
	if tool == "" {
		path, err := exec.LookPath("ipmitool")
		if err != nil {
			return nil, ErrNoNodePowerTool
		}
		tool = path
	}

	ch := make(chan NodeStat, 1)
	go func() {
		defer close(ch)

		tick := time.NewTicker(nodePowerEvery)
		defer tick.Stop()

		available := false
		consecutiveFailures := 0
		skipTicks := 0
		loggedUnavailable := false

		sample := func() (NodeStat, bool) {
			stat, ok := sampleNodePower(ctx, tool)
			if ok {
				consecutiveFailures = 0
				skipTicks = 0
				if !available {
					available = true
					loggedUnavailable = false
					log.Infof("node power monitoring available via %s (%.0f W)", stat.Source, stat.PowerW)
				}
				return stat, true
			}
			consecutiveFailures++
			// Back off after the first few failures: each additional failure
			// adds one skipped round, capped at 15 rounds (7.5 minutes).
			if backoff := consecutiveFailures - ipmiProbeBackoffBase; backoff > 0 {
				skipTicks = backoff
				if skipTicks > 15 {
					skipTicks = 15
				}
			}
			if available {
				available = false
				loggedUnavailable = true
				log.Infof("node power monitoring became unavailable, will keep retrying")
			}
			return NodeStat{}, false
		}

		// Probe immediately so availability is known without waiting a tick.
		if stat, ok := sample(); ok {
			ch <- stat
		} else if loggedUnavailable {
			log.Infof("node power: ipmitool found but no usable power reading yet")
			loggedUnavailable = false // state-change logging resumes on recovery
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if skipTicks > 0 {
					skipTicks--
					continue
				}
				if stat, ok := sample(); ok {
					select {
					case ch <- stat:
					default:
					}
				}
			}
		}
	}()
	return ch, nil
}

// gpuEnergyKey identifies the GPU an energy accumulator belongs to, matching
// the Prometheus label rendering (uuid preferred, index fallback).
type gpuEnergyKey struct {
	id   int
	uuid string
}

func (k gpuEnergyKey) String() string {
	if k.uuid != "" {
		return k.uuid
	}
	return fmt.Sprintf("index-%d", k.id)
}

// energyAccumulator integrates power samples into watt-hours. Integration
// skips gaps longer than maxEnergyGap so missing data is never extrapolated
// as consumption.
type energyAccumulator struct {
	id      int
	name    string
	uuid    string
	lastTS  time.Time
	lastW   float64
	totalWh float64
}

// maxEnergyGap is the largest sample gap integrated as continuous draw.
const maxEnergyGap = 3 * time.Minute

func (a *energyAccumulator) add(ts time.Time, watts float64) {
	if watts < 0 {
		watts = 0
	}
	if !a.lastTS.IsZero() {
		if dt := ts.Sub(a.lastTS); dt > 0 && dt <= maxEnergyGap {
			a.totalWh += a.lastW * dt.Hours()
		}
	}
	a.lastTS = ts
	a.lastW = watts
}
