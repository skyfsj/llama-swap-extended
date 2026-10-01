package logmon

import (
	"bytes"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// gatedSink blocks every Write until gate is closed. It stands in for the log
// destinations that stall in production: a terminal whose reader has gone away,
// a container log pipe nobody is draining.
type gatedSink struct {
	gate chan struct{}
	mu   sync.Mutex
	got  bytes.Buffer
}

func newGatedSink() *gatedSink {
	return &gatedSink{gate: make(chan struct{})}
}

func (s *gatedSink) Write(p []byte) (int, error) {
	<-s.gate
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got.Write(p)
}

func (s *gatedSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.got.String()
}

// A blocked sink must not become backpressure: Write has to keep accepting
// bytes so os/exec can keep draining the child's pipe.
func TestDrainWriter_SlowSinkDoesNotBlockWriter(t *testing.T) {
	sink := newGatedSink()
	w := NewDrainWriter(sink)

	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 100; i++ {
			if n, err := w.Write([]byte("data")); err != nil || n != 4 {
				t.Errorf("Write = %d, %v; want 4, nil", n, err)
				return
			}
		}
	}()

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Write blocked on a stalled sink; the child's pipe would fill up")
	}

	// Let the delivery goroutine finish so the test leaves nothing behind.
	close(sink.gate)
	w.Close()
}

// The failure this whole type exists to prevent: a sink that never returns must
// not stall the child. The child here writes far more than a pipe buffer, so
// any backpressure into its stdout would hang it.
func TestDrainWriter_StalledSinkDoesNotStallChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	sink := newGatedSink()
	w := NewDrainWriter(sink)

	const lines = 3000
	const lineLen = 37
	cmd := exec.Command("sh", "-c",
		fmt.Sprintf("i=0; while [ $i -lt %d ]; do printf 'abcdefghijklmnopqrstuvwxyz0123456789\\n'; i=$((i+1)); done", lines))
	cmd.Stdout = w
	cmd.Stderr = w

	done := startChild(t, cmd)
	if err := awaitChild(t, cmd, done, "child hung while the log sink was stalled: output reached the pipe and stopped"); err != nil {
		t.Fatalf("child died while the log sink was stalled: %v", err)
	}

	close(sink.gate)
	w.Close()

	// The child wrote more than the queue can hold while the sink was stalled,
	// so the delivered payload plus the reported gaps must account for all of
	// it: output may be dropped when the sink cannot keep up, but it must never
	// be lost silently.
	payload, dropped := stripDropMarkers(sink.String())
	if got := len(payload) + dropped; got != lines*lineLen {
		t.Fatalf("delivered %d + dropped %d = %d bytes, want %d", len(payload), dropped, got, lines*lineLen)
	}
}

// Write sits on os/exec's copier goroutine, where a short count becomes
// ErrShortWrite and a torn-down pipe. It must always report the full length.
func TestDrainWriter_WriteConsumesEveryByte(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"newline", "line\n"},
		{"no trailing newline", "partial"},
		{"empty", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := NewDrainWriter(failingWriter{})
			n, err := w.Write([]byte(tc.in))
			if err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if n != len(tc.in) {
				t.Fatalf("Write returned n=%d, want %d", n, len(tc.in))
			}
			w.Close()
		})
	}
}

// A sink that fails must not stop the pipe from being drained, and the failure
// must not surface as a short write either.
func TestDrainWriter_FailingSinkIsContained(t *testing.T) {
	w := NewDrainWriter(failingWriter{})
	for i := 0; i < 10; i++ {
		if n, err := w.Write([]byte("line\n")); err != nil || n != 5 {
			t.Fatalf("Write = %d, %v; want 5, nil", n, err)
		}
	}
	w.Close()
}

// Close flushes what is queued, so a caller can report on a process only after
// its last output has landed.
func TestDrainWriter_CloseFlushesQueue(t *testing.T) {
	var sink bytes.Buffer
	w := NewDrainWriter(&sink)
	for i := 0; i < 50; i++ {
		if _, err := w.Write([]byte("tail\n")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}
	w.Close()

	if got := strings.Count(sink.String(), "tail\n"); got != 50 {
		t.Fatalf("sink got %d of 50 queued chunks", got)
	}
	// Close is idempotent, and a late Write must not panic on a closed queue.
	w.Close()
	if n, err := w.Write([]byte("after close\n")); err != nil || n != 12 {
		t.Fatalf("Write after Close = %d, %v; want 12, nil", n, err)
	}
}

// When the sink is so far behind that the queue fills, the gap must be visible
// in the stream rather than silent.
func TestDrainWriter_ReportsDroppedBytes(t *testing.T) {
	sink := newGatedSink()
	w := NewDrainWriter(sink)

	const chunk = "chunk-000\n"
	const total = drainQueueSize + 50
	for i := 0; i < total; i++ {
		if _, err := w.Write([]byte(chunk)); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	close(sink.gate)
	w.Close()

	payload, dropped := stripDropMarkers(sink.String())
	if dropped == 0 {
		t.Fatalf("sink = %q, want a gap marker for the bytes the queue could not hold", truncateForMessage(payload))
	}
	if len(payload)+dropped != total*len(chunk) {
		t.Fatalf("delivered %d + dropped %d bytes, want %d", len(payload), dropped, total*len(chunk))
	}
	// Whatever survived must be whole chunks, not a partially written one.
	if n := strings.Count(payload, "chunk-"); n != len(payload)/len(chunk) {
		t.Fatalf("payload = %q, want only whole chunks", truncateForMessage(payload))
	}
}

// End to end through the production chain: a child's stdout crosses a
// DrainWriter, a Monitor and a LinePrefixWriter on its way to a shared log that
// is slow to accept it.
func TestDrainWriter_MonitorPrefixChainSurvivesSlowSink(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	sink := newGatedSink()
	prefixWriter := NewLinePrefixWriter("[m] ", sink)
	monitor := NewWriter(prefixWriter)
	drain := NewDrainWriter(monitor)

	cmd := exec.Command("sh", "-c", `printf 'tokens='; sleep 0.3; printf '4096\n'; echo next-line`)
	cmd.Stdout = drain
	cmd.Stderr = drain

	done := startChild(t, cmd)
	if err := awaitChild(t, cmd, done, "child hung through DrainWriter->Monitor->PrefixWriter"); err != nil {
		t.Fatalf("child died through DrainWriter->Monitor->PrefixWriter: %v", err)
	}

	// The child has exited, so every byte it wrote is either delivered or
	// queued; Close lands the rest.
	close(sink.gate)
	drain.Close()

	if got := sink.String(); !strings.Contains(got, "[m] tokens=4096") || !strings.Contains(got, "[m] next-line") {
		t.Fatalf("shared log = %q, want both prefixed lines", truncateForMessage(got))
	}
	// The monitor's history is what the log panel reads and must match.
	if history := string(monitor.GetHistory()); !strings.Contains(history, "tokens=4096") || !strings.Contains(history, "next-line") {
		t.Fatalf("monitor history = %q, want the complete output", truncateForMessage(history))
	}
}

func truncateForMessage(s string) string {
	const limit = 512
	if len(s) <= limit {
		return s
	}
	return s[:limit] + "..."
}

// dropMarkerRE matches the in-stream gap marker written by flushDropped,
// including the newline it inserts ahead of itself.
var dropMarkerRE = regexp.MustCompile(`\n— (\d+) bytes dropped —\n`)

// stripDropMarkers removes the gap markers from a captured stream and returns
// the payload that actually arrived along with the number of bytes the markers
// accounted for.
func stripDropMarkers(s string) (payload string, dropped int) {
	payload = dropMarkerRE.ReplaceAllStringFunc(s, func(marker string) string {
		count, err := strconv.Atoi(dropMarkerRE.FindStringSubmatch(marker)[1])
		if err != nil {
			return ""
		}
		dropped += count
		return ""
	})
	return payload, dropped
}
