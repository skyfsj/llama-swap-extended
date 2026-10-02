package config

import (
	"testing"
	"time"
)

func TestConfig_PowerEffectiveDefaults(t *testing.T) {
	got := PowerConfig{}.Effective()
	if got.Currency != "CNY" {
		t.Errorf("Currency = %q, want CNY", got.Currency)
	}
	if got.RetentionDays != 90 {
		t.Errorf("RetentionDays = %d, want 90", got.RetentionDays)
	}
	if got.FlushInterval != time.Minute {
		t.Errorf("FlushInterval = %v, want 1m", got.FlushInterval)
	}

	explicit := PowerConfig{
		Currency:      "USD",
		RetentionDays: 7,
		FlushInterval: 30 * time.Second,
	}.Effective()
	if explicit.Currency != "USD" || explicit.RetentionDays != 7 || explicit.FlushInterval != 30*time.Second {
		t.Errorf("Effective overwrote explicit values: %+v", explicit)
	}
}

func TestConfig_PowerValidate(t *testing.T) {
	valid := PowerConfig{}.Effective()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}

	cases := []struct {
		name   string
		mutate func(*PowerConfig)
	}{
		{"negative base price", func(p *PowerConfig) { p.ElectricityPricePerKwh = -0.5 }},
		{"short flush interval", func(p *PowerConfig) { p.FlushInterval = 5 * time.Second }},
		{"zero retention", func(p *PowerConfig) { p.RetentionDays = 0 }},
		{"negative tariff price", func(p *PowerConfig) {
			p.Tariffs = []PowerTariff{{Name: "peak", PricePerKwh: -1, Start: "08:00", End: "22:00"}}
		}},
		{"bad start format", func(p *PowerConfig) {
			p.Tariffs = []PowerTariff{{Name: "peak", PricePerKwh: 1, Start: "8am", End: "22:00"}}
		}},
		{"bad end format", func(p *PowerConfig) {
			p.Tariffs = []PowerTariff{{Name: "peak", PricePerKwh: 1, Start: "08:00", End: "24:30"}}
		}},
		{"zero length window", func(p *PowerConfig) {
			p.Tariffs = []PowerTariff{{Name: "flat", PricePerKwh: 1, Start: "08:00", End: "08:00"}}
		}},
		{"overlapping windows", func(p *PowerConfig) {
			p.Tariffs = []PowerTariff{
				{Name: "peak", PricePerKwh: 1, Start: "08:00", End: "22:00"},
				{Name: "high", PricePerKwh: 2, Start: "21:00", End: "23:00"},
			}
		}},
		{"wrap window overlapping plain window", func(p *PowerConfig) {
			p.Tariffs = []PowerTariff{
				{Name: "valley", PricePerKwh: 0.3, Start: "22:00", End: "06:00"},
				{Name: "night", PricePerKwh: 0.4, Start: "04:00", End: "07:00"},
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := PowerConfig{}.Effective()
			tc.mutate(&cfg)
			if err := cfg.Validate(); err == nil {
				t.Fatalf("expected validation error")
			}
		})
	}

	t.Run("non overlapping including wrap", func(t *testing.T) {
		cfg := PowerConfig{
			Tariffs: []PowerTariff{
				{Name: "peak", PricePerKwh: 1.0, Start: "08:00", End: "22:00"},
				{Name: "valley", PricePerKwh: 0.3, Start: "22:00", End: "06:00"},
			},
		}.Effective()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("peak/valley split should validate: %v", err)
		}
	})
}

func TestConfig_ThermalGuardEffective(t *testing.T) {
	// Nil guard stays nil and disabled.
	if guard, err := (*ThermalGuardConfig)(nil).Effective(); guard != nil || err != nil {
		t.Errorf("nil guard = %v, %v; want nil, nil", guard, err)
	}

	guard, err := (&ThermalGuardConfig{StopC: 85}).Effective()
	if err != nil {
		t.Fatalf("defaults: %v", err)
	}
	if guard.ResumeC != 80 {
		t.Errorf("ResumeC = %v, want 80 (stopC-5)", guard.ResumeC)
	}
	if guard.Consecutive != 3 {
		t.Errorf("Consecutive = %d, want 3", guard.Consecutive)
	}
	if !guard.FailoverEnabled() {
		t.Errorf("failover should default to enabled")
	}

	explicit, err := (&ThermalGuardConfig{StopC: 90, ResumeC: 70, Consecutive: 1, Failover: &[]bool{false}[0]}).Effective()
	if err != nil {
		t.Fatalf("explicit: %v", err)
	}
	if explicit.ResumeC != 70 || explicit.Consecutive != 1 || explicit.FailoverEnabled() {
		t.Errorf("explicit values overwritten: %+v", explicit)
	}

	for name, guard := range map[string]*ThermalGuardConfig{
		"zero stopC":           {StopC: 0},
		"resume above stop":    {StopC: 80, ResumeC: 85},
		"resume equal to stop": {StopC: 80, ResumeC: 80},
	} {
		if _, err := guard.Effective(); err == nil {
			t.Errorf("%s: expected validation error", name)
		}
	}

	cfg := PowerConfig{ThermalGuard: &ThermalGuardConfig{StopC: 80, ResumeC: 85}}.Effective()
	if err := cfg.Validate(); err == nil {
		t.Errorf("invalid thermal guard should fail PowerConfig.Validate")
	}
}

func TestConfig_ParsePowerClock(t *testing.T) {
	if got, err := ParsePowerClock("06:05"); err != nil || got != 6*60+5 {
		t.Errorf("ParsePowerClock(06:05) = %d, %v; want 365, nil", got, err)
	}
	for _, bad := range []string{"", "8", "24:00", "12:60", "-1:00"} {
		if _, err := ParsePowerClock(bad); err == nil {
			t.Errorf("ParsePowerClock(%q) should fail", bad)
		}
	}
}

func TestConfig_PowerTariffPriceFor(t *testing.T) {
	tariffs := PowerConfig{
		ElectricityPricePerKwh: 0.5,
		Tariffs: []PowerTariff{
			{Name: "peak", PricePerKwh: 1.0, Start: "08:00", End: "22:00"},
			{Name: "valley", PricePerKwh: 0.3, Start: "22:00", End: "06:00"},
		},
	}

	cases := []struct {
		hour     int
		minute   int
		price    float64
		tariffAt string
	}{
		{7, 59, 0.5, ""},
		{8, 0, 1.0, "peak"},
		{21, 59, 1.0, "peak"},
		{22, 0, 0.3, "valley"},
		{23, 30, 0.3, "valley"},
		{0, 0, 0.3, "valley"},
		{5, 59, 0.3, "valley"},
		{6, 0, 0.5, ""},
	}
	for _, tc := range cases {
		price, name := tariffs.TariffPriceFor(time.Date(2026, 10, 2, tc.hour, tc.minute, 0, 0, time.Local))
		if price != tc.price || name != tc.tariffAt {
			t.Errorf("%02d:%02d: got (%v, %q), want (%v, %q)", tc.hour, tc.minute, price, name, tc.price, tc.tariffAt)
		}
	}

	// Without tariffs the base price applies everywhere.
	single := PowerConfig{ElectricityPricePerKwh: 0.6}
	if price, name := single.TariffPriceFor(time.Now()); price != 0.6 || name != "" {
		t.Errorf("single price config: got (%v, %q), want (0.6, %q)", price, name, "")
	}
}
