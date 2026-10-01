package logmon

import (
	"bytes"
	"os/exec"
	"strings"
	"testing"
)

// A writer installed as exec.Cmd.Stdout must consume every byte of p. Returning
// n < len(p) with a nil error makes io.Copy stop with ErrShortWrite, which tears
// down the child's stdout pipe and kills it on its next write.
func TestLinePrefixWriter_ConsumesEveryByte(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"complete line", "hello\n"},
		{"partial line (no newline)", "partial without newline"},
		{"empty", ""},
		{"newline only", "\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sink bytes.Buffer
			w := NewLinePrefixWriter("[m] ", &sink)
			n, err := w.Write([]byte(tc.in))
			if err != nil {
				t.Fatalf("Write error: %v", err)
			}
			if n != len(tc.in) {
				t.Fatalf("Write returned n=%d, want %d (short write breaks io.Copy)", n, len(tc.in))
			}
		})
	}
}

// End-to-end proof of the consequence: a child that writes a partial line and
// then continues must keep a working stdout. The sleep keeps the two writes in
// separate pipe reads so the kernel cannot coalesce them.
func TestLinePrefixWriter_PartialLineDoesNotKillChild(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	var sink bytes.Buffer
	w := NewLinePrefixWriter("[m] ", &sink)

	// A partial-line write, a pause (so the copier must handle the short write
	// on its own), then the rest of the output.
	cmd := exec.Command("sh", "-c", `printf 'reached tokens='; sleep 0.3; printf '4096\n'; echo survived`)
	cmd.Stdout = w
	cmd.Stderr = w

	if err := awaitChild(t, cmd, startChild(t, cmd), "child hung after a partial-line write"); err != nil {
		t.Fatalf("child died after a partial-line write: %v (sink=%q)", err, sink.String())
	}

	if !strings.Contains(sink.String(), "reached tokens=4096") {
		t.Fatalf("sink = %q, want the reassembled partial line", sink.String())
	}
	if !strings.Contains(sink.String(), "survived") {
		t.Fatalf("sink = %q, output after the partial line never arrived", sink.String())
	}
}

// TestMonitorWrappingPrefixWriter_RealTopology pins the second half of the
// production chain: the router builds
// logmon.NewWriter(NewLinePrefixWriter("[model] ", upstreamLog)), so a model's
// output crosses a Monitor AND a prefix writer. Production interposes a
// DrainWriter ahead of this pair (see drain_test.go); a writer that is short
// even without that guard would still tear down a child's pipe wherever it is
// bound, so the contract is asserted directly here too.
func TestMonitorWrappingPrefixWriter_RealTopology(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("sh unavailable")
	}
	var upstream bytes.Buffer
	prefixWriter := NewLinePrefixWriter("[m] ", &upstream)
	monitor := NewWriter(prefixWriter)

	// A partial line, a stall, then the continuation and a normal line.
	cmd := exec.Command("sh", "-c", `printf 'tokens='; sleep 0.3; printf '4096\n'; echo next-line`)
	cmd.Stdout = monitor
	cmd.Stderr = monitor

	if err := awaitChild(t, cmd, startChild(t, cmd), "child hung through Monitor->PrefixWriter"); err != nil {
		t.Fatalf("child died through Monitor->PrefixWriter: %v (upstream=%q)", err, upstream.String())
	}

	got := upstream.String()
	if !strings.Contains(got, "[m] tokens=4096") {
		t.Fatalf("upstream = %q, want the prefixed reassembled line", got)
	}
	if !strings.Contains(got, "[m] next-line") {
		t.Fatalf("upstream = %q, output after the partial line was lost", got)
	}
	// The monitor's own history must be identical, since it is what the log
	// panel reads.
	if !strings.Contains(string(monitor.GetHistory()), "tokens=4096") {
		t.Fatalf("monitor history = %q, want the full line", monitor.GetHistory())
	}
}
