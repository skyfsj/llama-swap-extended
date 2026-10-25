package perf

import (
	"context"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
)

// fakeGpuCollector stands in for the platform GPU collector. Each call to its
// start method runs the next step: a step either fails to start, or returns a
// channel that closes without a sample, or delivers one sample and then closes
// or stays open until ctx is done. Its wait method records every restart wait
// and returns at once.
type fakeGpuCollector struct {
	mu    sync.Mutex
	calls int
	steps []fakeGpuStep
	waits []time.Duration
}

type fakeGpuStep struct {
	err      error
	noSample bool
	stayOpen bool
}

func (f *fakeGpuCollector) start(ctx context.Context, _ time.Duration, _ *logmon.Monitor) (chan []GpuStat, error) {
	f.mu.Lock()
	f.calls++
	call := f.calls
	step := fakeGpuStep{stayOpen: true}
	if call <= len(f.steps) {
		step = f.steps[call-1]
	}
	f.mu.Unlock()

	if step.err != nil {
		return nil, step.err
	}

	ch := make(chan []GpuStat, 1)
	go func() {
		defer close(ch)
		if step.noSample {
			// exits before it reports anything
			return
		}
		// ID carries the call number so a test can tell collectors apart
		select {
		case ch <- []GpuStat{{ID: call}}:
		case <-ctx.Done():
			return
		}
		if step.stayOpen {
			<-ctx.Done()
		}
	}()
	return ch, nil
}

func (f *fakeGpuCollector) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func (f *fakeGpuCollector) wait(ctx context.Context, d time.Duration) bool {
	f.mu.Lock()
	f.waits = append(f.waits, d)
	f.mu.Unlock()

	select {
	case <-ctx.Done():
		return false
	default:
		return true
	}
}

func (f *fakeGpuCollector) waitHistory() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]time.Duration(nil), f.waits...)
}

func newGpuTestMonitor(t *testing.T, f *fakeGpuCollector) (*Monitor, chan []GpuStat) {
	t.Helper()
	m, err := New(config.PerformanceConfig{Every: 100 * time.Millisecond}, config.PowerConfig{Ipmi: config.PowerIpmiConfig{Disabled: true}}, logmon.NewWriter(io.Discard))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.gpuStats = f.start
	m.gpuWait = f.wait

	_, gpuCh, _, unsub := m.Subscribe()
	t.Cleanup(unsub)
	return m, gpuCh
}

func waitGpuStat(t *testing.T, gpuCh chan []GpuStat, wantID int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case g := <-gpuCh:
			for _, stat := range g {
				if stat.ID >= wantID {
					return
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for GPU stats from collector %d", wantID)
		}
	}
}

// waitCalls waits until the fake has been started want times. Listener
// delivery is best-effort (a subscriber that is not reading drops the batch),
// so tests that only care that the collector was restarted poll the call count
// instead of chasing every sample.
func waitCalls(t *testing.T, f *fakeGpuCollector, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if f.callCount() >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("collector calls = %d, want %d", f.callCount(), want)
}

func TestStart_RestartsGpuCollectorAfterItStops(t *testing.T) {
	// the first collector delivers one sample and exits, like nvidia-smi dying
	f := &fakeGpuCollector{steps: []fakeGpuStep{{stayOpen: false}}}
	m, gpuCh := newGpuTestMonitor(t, f)

	m.Start()
	defer m.Stop()

	waitGpuStat(t, gpuCh, 1)
	waitCalls(t, f, 2)
	if got := f.callCount(); got != 2 {
		t.Fatalf("collector calls = %d, want 2", got)
	}
}

func TestStart_RetriesWhenGpuCollectorRestartFails(t *testing.T) {
	f := &fakeGpuCollector{steps: []fakeGpuStep{
		{stayOpen: false},
		{err: ErrNoGpuTool},
		{err: errors.New("nvidia-smi start failed")},
	}}
	m, gpuCh := newGpuTestMonitor(t, f)

	m.Start()
	defer m.Stop()

	waitGpuStat(t, gpuCh, 1)
	waitCalls(t, f, 4)
	if got := f.callCount(); got != 4 {
		t.Fatalf("collector calls = %d, want 4", got)
	}
}

func TestStart_GpuCollectorNotRestartedAfterStop(t *testing.T) {
	f := &fakeGpuCollector{}
	m, gpuCh := newGpuTestMonitor(t, f)

	m.Start()
	waitGpuStat(t, gpuCh, 1)
	m.Stop()

	// several restart delays
	time.Sleep(100 * time.Millisecond)
	if got := f.callCount(); got != 1 {
		t.Fatalf("collector calls after Stop = %d, want 1", got)
	}
}

func TestStart_GpuCollectorUnavailableAtStartIsNotRetried(t *testing.T) {
	f := &fakeGpuCollector{steps: []fakeGpuStep{{err: ErrNoGpuTool}}}
	m, _ := newGpuTestMonitor(t, f)

	m.Start()
	defer m.Stop()

	time.Sleep(100 * time.Millisecond)
	if got := f.callCount(); got != 1 {
		t.Fatalf("collector calls = %d, want 1 (no retry when the first start fails)", got)
	}
	if waits := f.waitHistory(); len(waits) != 0 {
		t.Fatalf("restart waits = %v, want none", waits)
	}
}

func TestStart_GpuRestartWaitDoublesUpToMax(t *testing.T) {
	// one working collector, then restarts that exit without stats or fail
	// to start, then a collector that works and stays up
	f := &fakeGpuCollector{steps: []fakeGpuStep{
		{stayOpen: false},
		{noSample: true},
		{err: errors.New("nvidia-smi start failed")},
		{noSample: true},
		{noSample: true},
	}}
	m, gpuCh := newGpuTestMonitor(t, f)

	m.Start()
	defer m.Stop()

	waitGpuStat(t, gpuCh, 1)
	waitCalls(t, f, 6)

	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		30 * time.Second,
		30 * time.Second,
	}
	if got := f.waitHistory(); len(got) != len(want) {
		t.Fatalf("restart waits = %v, want %v", got, want)
	}
	for i := range want {
		if f.waitHistory()[i] != want[i] {
			t.Fatalf("restart waits = %v, want %v", f.waitHistory(), want)
		}
	}
}

func TestStart_GpuRestartWaitResetsAfterStats(t *testing.T) {
	f := &fakeGpuCollector{steps: []fakeGpuStep{
		{stayOpen: false},
		{noSample: true},
		{noSample: true},
		{stayOpen: false}, // delivers stats, so the wait starts over
		{noSample: true},
	}}
	m, gpuCh := newGpuTestMonitor(t, f)

	m.Start()
	defer m.Stop()

	// Only the first and fourth collectors deliver a sample; the rest exit
	// without one or fail to start. Poll the call count so a dropped
	// best-effort delivery cannot make the test flaky.
	waitGpuStat(t, gpuCh, 1)
	waitCalls(t, f, 6)

	want := []time.Duration{
		5 * time.Second,
		10 * time.Second,
		20 * time.Second,
		5 * time.Second,
		10 * time.Second,
	}
	got := f.waitHistory()
	if len(got) != len(want) {
		t.Fatalf("restart waits = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("restart waits = %v, want %v", got, want)
		}
	}
}
