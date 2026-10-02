package config

import (
	"fmt"
	"sort"
	"time"
)

// PowerIpmiConfig configures the node-level power collector. Local mode uses
// ipmitool against the host BMC (no credentials); Command overrides the
// ipmitool binary path for non-standard installations.
type PowerIpmiConfig struct {
	Disabled bool   `yaml:"disabled" json:"disabled"`
	Command  string `yaml:"command,omitempty" json:"command,omitempty"`
}

// PowerTariff is one time-of-use electricity price window. Start and End are
// "HH:MM" wall clock times in the server's local timezone; End earlier than
// Start wraps across midnight (e.g. 22:00-06:00).
type PowerTariff struct {
	Name        string  `yaml:"name" json:"name"`
	PricePerKwh float64 `yaml:"pricePerKwh" json:"pricePerKwh"`
	Start       string  `yaml:"start" json:"start"`
	End         string  `yaml:"end" json:"end"`
}

// PowerConfig controls power monitoring, energy statistics and electricity
// cost accounting. The zero value keeps monitoring off-priced at zero until
// Effective fills defaults, matching AuditConfig semantics.
type PowerConfig struct {
	Disabled bool `yaml:"disabled" json:"disabled"`

	// ElectricityPricePerKwh is the base price per kWh. With no Tariffs every
	// sample is priced with it; otherwise it prices time no tariff covers.
	ElectricityPricePerKwh float64         `yaml:"electricityPricePerKwh" json:"electricityPricePerKwh"`
	Currency               string          `yaml:"currency" json:"currency"`
	Tariffs                []PowerTariff   `yaml:"tariffs,omitempty" json:"tariffs,omitempty"`
	RetentionDays          int             `yaml:"retentionDays" json:"retentionDays"`
	FlushInterval          time.Duration   `yaml:"flushInterval" json:"flushInterval"`
	Ipmi                   PowerIpmiConfig `yaml:"ipmi" json:"ipmi"`

	// ThermalGuard stops every model occupying an overheating GPU and puts it
	// into maintenance mode until the card cools below the recovery point.
	// Nil (the default) keeps the guard off for backwards compatibility.
	ThermalGuard *ThermalGuardConfig `yaml:"thermalGuard,omitempty" json:"thermalGuard,omitempty"`
}

// ThermalGuardConfig configures overheat protection. StopC must exceed
// ResumeC so a card hovering around one threshold cannot flap models on and
// off.
type ThermalGuardConfig struct {
	// StopC is the GPU core temperature in Celsius at which models on the
	// card are stopped and parked in maintenance. Required; sane values are
	// 80-90 for most datacenter and consumer cards.
	StopC float64 `yaml:"stopC" json:"stopC"`
	// ResumeC is the temperature at which parked models leave maintenance.
	// Defaults to StopC-5 when unset.
	ResumeC float64 `yaml:"resumeC" json:"resumeC"`
	// Consecutive samples above StopC before acting; each sample is one
	// performance.every tick. Defaults to 3 so a single sensor spike or a
	// transient reading does not unload serving models.
	Consecutive int `yaml:"consecutive" json:"consecutive"`
	// Failover forwards requests for maintenance models to configured
	// providers or peers. Defaults to true.
	Failover *bool `yaml:"failover" json:"failover"`
}

// Effective fills the thermal guard defaults.
func (t *ThermalGuardConfig) Effective() (*ThermalGuardConfig, error) {
	if t == nil {
		return nil, nil
	}
	guard := *t
	if guard.StopC <= 0 {
		return nil, fmt.Errorf("thermalGuard.stopC must be > 0, got %v", guard.StopC)
	}
	if guard.ResumeC == 0 {
		guard.ResumeC = guard.StopC - 5
	}
	if guard.ResumeC >= guard.StopC {
		return nil, fmt.Errorf("thermalGuard.resumeC (%v) must be lower than stopC (%v)", guard.ResumeC, guard.StopC)
	}
	if guard.Consecutive < 1 {
		guard.Consecutive = 3
	}
	if guard.Failover == nil {
		failover := true
		guard.Failover = &failover
	}
	return &guard, nil
}

// FailoverEnabled reports whether requests for maintenance models should be
// forwarded to other providers.
func (t *ThermalGuardConfig) FailoverEnabled() bool {
	return t != nil && (t.Failover == nil || *t.Failover)
}

// Effective returns the config with defaults applied to unset fields.
func (p PowerConfig) Effective() PowerConfig {
	if p.Currency == "" {
		p.Currency = "CNY"
	}
	if p.RetentionDays == 0 {
		p.RetentionDays = 90
	}
	if p.FlushInterval == 0 {
		p.FlushInterval = time.Minute
	}
	return p
}

// Validate checks the power config values.
func (p PowerConfig) Validate() error {
	if p.ElectricityPricePerKwh < 0 {
		return fmt.Errorf("electricityPricePerKwh must be >= 0, got %v", p.ElectricityPricePerKwh)
	}
	if p.FlushInterval < 15*time.Second {
		return fmt.Errorf("flushInterval must be at least 15s, got %v", p.FlushInterval)
	}
	if p.RetentionDays < 1 {
		return fmt.Errorf("retentionDays must be at least 1, got %d", p.RetentionDays)
	}
	if err := ValidateTariffs(p.Tariffs); err != nil {
		return err
	}
	if _, err := p.ThermalGuard.Effective(); err != nil {
		return err
	}
	return nil
}

// ValidateTariffs checks time-of-use window formats and overlap. Each window
// is normalized into non-wrapping segments of the day, so 22:00-06:00 and a
// 00:00-04:00 window correctly conflict.
func ValidateTariffs(tariffs []PowerTariff) error {
	type segment struct{ start, end int }

	var segments []segment
	for i, tariff := range tariffs {
		start, err := ParsePowerClock(tariff.Start)
		if err != nil {
			return fmt.Errorf("tariffs[%d] (%s): start: %v", i, tariff.Name, err)
		}
		end, err := ParsePowerClock(tariff.End)
		if err != nil {
			return fmt.Errorf("tariffs[%d] (%s): end: %v", i, tariff.Name, err)
		}
		if tariff.PricePerKwh < 0 {
			return fmt.Errorf("tariffs[%d] (%s): pricePerKwh must be >= 0, got %v", i, tariff.Name, tariff.PricePerKwh)
		}
		if start == end {
			return fmt.Errorf("tariffs[%d] (%s): start and end must differ", i, tariff.Name)
		}
		if end > start {
			segments = append(segments, segment{start, end})
		} else {
			// Wraps midnight: [start, 24:00) plus [00:00, end).
			segments = append(segments, segment{start, 24 * 60}, segment{0, end})
		}
	}

	sort.Slice(segments, func(i, j int) bool { return segments[i].start < segments[j].start })
	for i := 1; i < len(segments); i++ {
		if segments[i].start < segments[i-1].end {
			return fmt.Errorf("tariff time windows overlap around %02d:%02d",
				segments[i].start/60, segments[i].start%60)
		}
	}
	return nil
}

// ParsePowerClock parses an "HH:MM" wall clock time into minutes of day.
func ParsePowerClock(value string) (int, error) {
	var hour, minute int
	if _, err := fmt.Sscanf(value, "%d:%d", &hour, &minute); err != nil {
		return 0, fmt.Errorf("must be HH:MM, got %q", value)
	}
	if hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("out of range, got %q", value)
	}
	return hour*60 + minute, nil
}

// TariffPriceFor returns the price per kWh active at local wall clock time t:
// the covering tariff window's price, or the base price when no tariff covers
// it (or when no tariffs are configured at all). The second return value is
// the matched tariff name, empty for the base price.
func (p PowerConfig) TariffPriceFor(t time.Time) (float64, string) {
	minutes := t.Hour()*60 + t.Minute()
	for _, tariff := range p.Tariffs {
		start, err := ParsePowerClock(tariff.Start)
		if err != nil {
			continue
		}
		end, err := ParsePowerClock(tariff.End)
		if err != nil {
			continue
		}
		if start == end {
			continue
		}
		inWindow := minutes >= start && minutes < end
		if end < start {
			// Wraps midnight.
			inWindow = minutes >= start || minutes < end
		}
		if inWindow {
			return tariff.PricePerKwh, tariff.Name
		}
	}
	return p.ElectricityPricePerKwh, ""
}
