package logmon

import (
	"bytes"
	"io"
	"sync"
)

// linePrefixCarryLimit bounds the incomplete-line buffer. A process that
// streams progress bars separated by \r rather than \n never terminates a
// line; without a bound the carry would grow with the process output.
const linePrefixCarryLimit = 16 << 10

// LinePrefixWriter prepends a prefix to every complete line passing through
// it. It exists so the shared upstream log can attribute each line to the
// model whose process produced it: per-model monitors forward their output
// into one merged sink, which otherwise interleaves models anonymously.
//
// Incomplete trailing lines are carried between writes and flushed once
// terminated; an oversized fragment is flushed as its own line so the buffer
// stays bounded. Write errors from the next writer are swallowed: this is the
// merge stage of the shared log, and a slow or failing sink must degrade the
// log, never the process being logged. DrainWriter is what keeps that stage off
// the child's stdout drain path in the first place.
type LinePrefixWriter struct {
	prefix string
	next   io.Writer
	mu     sync.Mutex
	carry  []byte
}

func NewLinePrefixWriter(prefix string, next io.Writer) *LinePrefixWriter {
	return &LinePrefixWriter{prefix: prefix, next: next}
}

func (w *LinePrefixWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.carry = append(w.carry, p...)
	for {
		index := bytes.IndexByte(w.carry, '\n')
		if index < 0 {
			break
		}
		line := append([]byte(w.prefix), w.carry[:index+1]...)
		// A sink failure is deliberately swallowed: a broken shared log must not
		// become a broken process. The line leaves the carry either way.
		_, _ = w.next.Write(line)
		w.carry = w.carry[index+1:]
	}
	if len(w.carry) > linePrefixCarryLimit {
		line := append([]byte(w.prefix), w.carry...)
		line = append(line, '\n')
		_, _ = w.next.Write(line)
		w.carry = w.carry[:0]
	}
	// Every byte of p was accepted into the carry, so every byte was consumed.
	// Reporting anything less would be a short write, and a short write on a
	// pipe drain is what kills a child: io.Copy turns it into ErrShortWrite,
	// closes the child's stdout, and the child dies of SIGPIPE on its next
	// write. An unterminated fragment (a progress redraw, or the first half of
	// "reached tokens=4096") used to return zero here and killed vLLM mid-warmup
	// — CPython then exited with a bare code 120, its status when it cannot
	// flush stdout at interpreter shutdown.
	return len(p), nil
}
