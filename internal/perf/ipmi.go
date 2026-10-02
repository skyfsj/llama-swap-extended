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

// dellOemReadingRe matches the instantaneous "Reading : 189 W" line of
// `ipmitool delloem powermonitor powerconsumption`. The cumulative/peak
// `powermonitor` output reports kWh and must not match — it is anchored on
// the bare "Reading" field whose unit is W (optionally decimals), never kWh.
var dellOemReadingRe = regexp.MustCompile(`(?m)^\s*Reading\s*:\s*([0-9]+(?:\.[0-9]+)?)\s*W\s*$`)

// sensorPowerNames is an alias kept for the rank arithmetic below.
var sensorPowerNames = sensorPowerPatterns

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
// Rows are matched by known sensor-name fragments (spanning BMC vendors)
// first; when none matches and there is exactly one Watts row, that row is
// used.
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
		lower := strings.ToLower(name)
		for i, pattern := range sensorPowerPatterns {
			if strings.Contains(lower, pattern) {
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

// sensorPowerPatterns are sensor label fragments, matched case-insensitively,
// whose wattage reading represents whole-node power draw. The list covers the
// labels observed across BMC vendors (order is preference): Supermicro and
// Dell "Pwr Consumption"/"System Level", HPE "Power Meter", Lenovo "Sys
// Power"/"System Power", Inspur "Total Power"/"SYS Pwr", GIGABYTE
// "SYS_POWER", OpenBIC (Wiwynn/QCT) "HSC Input Power", Huawei "Power N".
var sensorPowerPatterns = []string{
	"pwr consumption",
	"power meter",
	"system level",
	"sys power",
	"system power",
	"total power",
	"sys_power",
	"sys pwr",
	"hsc input power",
	"node power",
	"power ",
	"psu",
}

// nodePowerMinWatts and nodePowerMaxWatts bound a plausible whole-node
// reading. Some firmware bugs report 0 W or an implausible spike (65535 W
// has been observed on iDRAC); such values are invalid readings, not real
// consumption.
const (
	nodePowerMinWatts = 1
	nodePowerMaxWatts = 20000
)

// plausibleNodePower filters firmware garbage: 0 W, negative, or beyond the
// sanity ceiling.
func plausibleNodePower(watts float64) bool {
	return watts >= nodePowerMinWatts && watts <= nodePowerMaxWatts
}

// sampleNodePower runs one ipmitool round across vendor strategies, best
// first: standard DCMI, then Dell's OEM powermonitor, then the generic
// sensor table. The returned source names the mechanism that produced the
// value.
func sampleNodePower(ctx context.Context, tool string) (NodeStat, bool) {
	ctx, cancel := context.WithTimeout(ctx, nodePowerTimeout)
	defer cancel()

	type attempt struct {
		args   []string
		parse  func(string) (float64, string, bool)
		source string
	}
	// Newer BMCs accept `dcmi power`; older Supermicro firmware prints the
	// subcommand help unless the reading form is spelled out fully. Dell
	// iDRAC's DCMI is missing or broken on many generations and reports
	// garbage on others, so the OEM command outranks the sensor table there.
	attempts := []attempt{
		{[]string{"dcmi", "power", "reading"}, dcmiParse, "ipmi-dcmi"},
		{[]string{"dcmi", "power"}, dcmiParse, "ipmi-dcmi"},
		{[]string{"delloem", "powermonitor", "powerconsumption"}, dellOemParse, "ipmi-delloem"},
		{[]string{"sensor"}, sensorParse, "ipmi-sensor"},
	}
	for _, a := range attempts {
		out, err := exec.CommandContext(ctx, tool, a.args...).Output()
		if err != nil {
			continue
		}
		watts, name, ok := a.parse(string(out))
		if ok && plausibleNodePower(watts) {
			source := a.source
			if name != "" {
				source += ":" + name
			}
			return NodeStat{Timestamp: time.Now(), PowerW: watts, Source: source}, true
		}
	}
	return NodeStat{}, false
}

// dcmiParse adapts ParseIpmiDcmiPower to the attempt table.
func dcmiParse(output string) (float64, string, bool) {
	watts, ok := ParseIpmiDcmiPower(output)
	return watts, "", ok
}

// dellOemParse extracts "Reading : 189 W" from
// `ipmitool delloem powermonitor powerconsumption`.
func dellOemParse(output string) (float64, string, bool) {
	match := dellOemReadingRe.FindStringSubmatch(output)
	if match == nil {
		return 0, "", false
	}
	watts, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return 0, "", false
	}
	return watts, "", true
}

// sensorParse adapts ParseIpmiSensorPower to the attempt table.
func sensorParse(output string) (float64, string, bool) {
	watts, name, ok := ParseIpmiSensorPower(output)
	return watts, name, ok
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
	// A 0 W reading is a valid measurement (driver reset, BMC quirk): it
	// advances the anchor and integrates as zero, so the next real reading
	// does not bill the idle span at the pre-gap wattage. Only negative
	// readings are discarded as invalid; a late batch (a lagging reader
	// goroutine surviving a ring reset) must not rewind the anchor either.
	if watts < 0 || !ts.After(a.lastTS) {
		return
	}
	if !a.lastTS.IsZero() {
		if dt := ts.Sub(a.lastTS); dt > 0 && dt <= maxEnergyGap {
			a.totalWh += a.lastW * dt.Hours()
		}
	}
	a.lastTS = ts
	a.lastW = watts
}
