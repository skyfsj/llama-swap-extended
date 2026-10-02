package perf

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestPerf_ParseIpmiDcmiPower(t *testing.T) {
	const dcmi = `DCMI Power Management
Current Power State: System power is on
Instantiate Power Reading:
	Instantaneous power reading:                   245 Watts
	Minimum power over sampling duration:          120 Watts
	Maximum power over sampling duration:          500 Watts
	Average power over sampling duration:          240 Watts
`

	watts, ok := ParseIpmiDcmiPower(dcmi)
	if !ok || watts != 245 {
		t.Errorf("ParseIpmiDcmiPower = %v, %v; want 245, true", watts, ok)
	}
	if _, ok := ParseIpmiDcmiPower("no power reading here"); ok {
		t.Errorf("unrelated output should not parse")
	}
	if _, ok := ParseIpmiDcmiPower("power <command>\n    reading       Get power related readings"); ok {
		t.Errorf("dcmi help text should not parse as a reading")
	}
}

func TestPerf_ParseIpmiSensorPower(t *testing.T) {
	const sensors = `Sensor Name             | Value    | Units   | Status
Pwr Consumption         | 312.000  | Watts   | ok
CPU Temp                | 45.000   | degrees C | ok
Fan1                    | 2200.000 | RPM     | ok
`
	watts, name, ok := ParseIpmiSensorPower(sensors)
	if !ok || watts != 312 || name != "Pwr Consumption" {
		t.Errorf("ParseIpmiSensorPower = %v, %q, %v; want 312, Pwr Consumption, true", watts, name, ok)
	}

	// Prefer a known node sensor over an unrelated PSU row.
	const ambiguous = `PSU1 Input Power        | 180.000  | Watts   | ok
Pwr Consumption         | 312.000  | Watts   | ok
`
	watts, name, ok = ParseIpmiSensorPower(ambiguous)
	if !ok || watts != 312 || name != "Pwr Consumption" {
		t.Errorf("known sensor should win: %v, %q, %v", watts, name, ok)
	}

	// A single unmatched Watts row is still usable.
	const single = `Some Sensor | 42.000 | Watts | ok
Some Other | 1.000 | degrees C | ok
`
	if watts, _, ok := ParseIpmiSensorPower(single); !ok || watts != 42 {
		t.Errorf("single unmatched watts row should parse: %v, %v", watts, ok)
	}

	// Multiple unmatched rows are ambiguous on purpose.
	const multi = `A | 42.000 | Watts | ok
B | 43.000 | Watts | ok
`
	if _, _, ok := ParseIpmiSensorPower(multi); ok {
		t.Errorf("multiple unmatched watts rows should not parse")
	}
}

func TestPerf_EnergyAccumulator(t *testing.T) {
	acc := &energyAccumulator{}
	start := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	// First sample seeds the baseline only.
	acc.add(start, 100)
	if acc.totalWh != 0 {
		t.Fatalf("first sample should not integrate: %v", acc.totalWh)
	}

	// 1 minute at a constant 100 W = 100/60 Wh (within the max integration gap).
	acc.add(start.Add(time.Minute), 100)
	want := 100.0 / 60.0
	if diff := acc.totalWh - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("totalWh = %v, want ~%v", acc.totalWh, want)
	}

	// A long gap (missing data) must not be extrapolated.
	acc.add(start.Add(5*time.Hour), 200)
	if diff := acc.totalWh - want; diff > 0.01 || diff < -0.01 {
		t.Errorf("gap should not add energy: totalWh = %v", acc.totalWh)
	}

	// Negative wattage clamps to zero.
	acc.add(start.Add(5*time.Hour+time.Minute), -50)
	if acc.lastW != 0 {
		t.Errorf("negative watts should clamp to 0, got %v", acc.lastW)
	}
}

func TestPerf_MonitorEnergyIntegration(t *testing.T) {
	m := &Monitor{
		power:      config.PowerConfig{},
		gpuEnergy:  make(map[string]*energyAccumulator),
		nodeEnergy: &energyAccumulator{name: "node"},
	}
	base := time.Date(2026, 10, 2, 12, 0, 0, 0, time.UTC)

	m.integrateGpuEnergyLocked([]GpuStat{
		{Timestamp: base, ID: 0, Name: "TestGPU", UUID: "GPU-xyz", PowerDrawW: 200},
		{Timestamp: base.Add(time.Minute), ID: 0, Name: "TestGPU", UUID: "GPU-xyz", PowerDrawW: 200},
	})
	m.nodeEnergy.add(base.Add(time.Minute), 400)

	snapshot := m.Energy()
	if len(snapshot.GPUs) != 1 {
		t.Fatalf("GPU count = %d, want 1", len(snapshot.GPUs))
	}
	gpu := snapshot.GPUs[0]
	if gpu.UUID != "GPU-xyz" || gpu.Name != "TestGPU" {
		t.Errorf("labels = %+v", gpu)
	}
	// 200 W for one minute = 200/60 Wh.
	if diff := gpu.WattHours - 200.0/60.0; diff > 0.01 || diff < -0.01 {
		t.Errorf("GPU WattHours = %v, want ~%v", gpu.WattHours, 200.0/60.0)
	}
	if !m.NodePowerAvailable() {
		t.Errorf("node power should be available")
	}
}

func TestPerf_PrometheusEnergyMetrics(t *testing.T) {
	m := &Monitor{
		gpuEnergy: map[string]*energyAccumulator{
			"GPU-xyz": {id: 0, name: "Test GPU", uuid: "GPU-xyz", totalWh: 12.5},
		},
		nodeEnergy: &energyAccumulator{name: "node", totalWh: 99.25, lastW: 300},
	}

	recorder := httptest.NewRecorder()
	m.MetricsHandler()(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := recorder.Body.String()
	for _, want := range []string{
		`# TYPE llamaswap_gpu_energy_watt_hours_total counter`,
		`llamaswap_gpu_energy_watt_hours_total{id="0",name="Test GPU",uuid="GPU-xyz"} 12.5`,
		`# TYPE llamaswap_node_energy_watt_hours_total counter`,
		`llamaswap_node_energy_watt_hours_total 99.25`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}

	// Node energy stays hidden until a real reading arrived.
	m2 := &Monitor{
		gpuEnergy:  make(map[string]*energyAccumulator),
		nodeEnergy: &energyAccumulator{name: "node", totalWh: 50},
	}
	recorder2 := httptest.NewRecorder()
	m2.MetricsHandler()(recorder2, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if strings.Contains(recorder2.Body.String(), "llamaswap_node_energy_watt_hours_total") {
		t.Errorf("node energy should be absent without a real node reading")
	}
}
