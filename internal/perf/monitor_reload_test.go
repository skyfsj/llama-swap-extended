package perf

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/stretchr/testify/require"
)

func monitorLoopGoroutines() int {
	buffer := make([]byte, 4<<20)
	count := 0
	for _, block := range strings.Split(string(buffer[:runtime.Stack(buffer, true)]), "\n\n") {
		if strings.Contains(block, "internal/perf.(*Monitor).Start.func") {
			count++
		}
	}
	return count
}

// TestMonitor_UpdateConfigDoesNotLeakSysGoroutine guards the stop-context
// capture: a loop that reads m.stopCtx per iteration can park on the *new*
// context after Stop/Start swapped it, leaking itself and double-pushing
// samples forever. One running monitor owns exactly two such loops on darwin
// (sys and GPU; the node loop only exists when IPMI is configured), so the
// count must stay flat no matter how often the config is replaced.
func TestMonitor_UpdateConfigDoesNotLeakSysGoroutine(t *testing.T) {
	m, err := New(config.PerformanceConfig{Every: 100 * time.Millisecond}, config.PowerConfig{}, newTestLogger())
	require.NoError(t, err)
	defer m.Stop()
	defer newTestLogger().Close()

	// Two restarts settle GPU discovery, which happens inside the loop.
	for i := 0; i < 2; i++ {
		m.UpdateConfig(config.PerformanceConfig{Every: time.Duration(100+i*10) * time.Millisecond}, config.PowerConfig{})
		time.Sleep(120 * time.Millisecond)
	}
	settled := monitorLoopGoroutines()

	for i := 0; i < 20; i++ {
		m.UpdateConfig(config.PerformanceConfig{Every: time.Duration(110+i) * time.Millisecond}, config.PowerConfig{})
		// Land inside ReadSysStats/sampleIoreg, the window where a loop that
		// re-reads the field would miss its own cancellation.
		time.Sleep(15 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	after := monitorLoopGoroutines()
	if after > settled {
		t.Fatalf("monitor loop goroutines grew from %d to %d after 20 config restarts", settled, after)
	}
}
