package perf

import (
	"errors"
	"testing"
)

func TestPerf_CPUPercentsOrZeroFallsBackForDarwinSamplingFailure(t *testing.T) {
	values := cpuPercentsOrZero(nil, errors.New("host_processor_info returned nil cpuload"))
	if len(values) < 1 {
		t.Fatal("fallback CPU samples are empty")
	}
	for _, value := range values {
		if value != 0 {
			t.Fatalf("fallback CPU value=%v want 0", value)
		}
	}
}

func TestPerf_CPUPercentsOrZeroPreservesReportedSamples(t *testing.T) {
	want := []float64{12.5, 37.5}
	got := cpuPercentsOrZero(want, nil)
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("CPU samples=%v want %v", got, want)
	}
}
