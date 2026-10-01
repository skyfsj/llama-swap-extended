//go:build linux || windows

package hw

import "testing"

func TestHardware_ParseNvidiaCSV(t *testing.T) {
	records, err := parseNvidiaCSV("0, NVIDIA GeForce RTX 4090, GPU-abc, 00000000:01:00.0, 24564, 570.124.06, 450.00\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].memoryBytes != 24564*1024*1024 || records[0].powerLimit != 450 {
		t.Fatalf("parseNvidiaCSV() = %+v", records)
	}
}

func TestHardware_NvidiaComputeCapabilityArchitectures(t *testing.T) {
	records := []nvidiaRecord{
		{index: 0, name: "Tesla P40"},
		{index: 1, name: "Tesla P40"},
		{index: 2, name: "NVIDIA GeForce RTX 3090"},
		{index: 3, name: "NVIDIA GeForce RTX 3090", architecture: "Ampere"},
	}
	applyNvidiaComputeCapabilities(records, "0, 6.1\n1, 6.1\n2, 8.6\n3, 8.6\n")

	want := []string{"Pascal", "Pascal", "Ampere", "Ampere"}
	for i := range records {
		if records[i].architecture != want[i] {
			t.Errorf("records[%d].architecture = %q, want %q", i, records[i].architecture, want[i])
		}
	}
}
