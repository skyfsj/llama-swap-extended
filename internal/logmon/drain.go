package logmon

import (
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

// drainQueueSize bounds how many pending chunks may wait for the sink. The sink
// is normally a memory copy plus a terminal write, so the queue stays empty in
// practice; it only fills while the terminal itself is stalled (a slow
// `docker logs` reader, a blocked pipe). Buffering more than this would delay
// every other log consumer without making the stall any shorter.
const drainQueueSize = 256

// DrainWriter decouples a child process's stdout/stderr from the work of
// rendering it.
//
// os/exec drains a child's pipes with io.Copy running on its own goroutine,
// writing straight through to whatever cmd.Stdout was set to. When that writer
// is a log sink, everything the sink does — splitting lines, prefixing them,
// merging them into a shared log, writing the terminal — lands on the pipe's
// drain path. A slow sink then stops the copier, the pipe fills, and the child
// dies of SIGPIPE on its next write: a logging fault that reaches the operator
// as a model crash. The same wiring turns a bug in the formatting layer into a
// killed process.
//
// DrainWriter keeps the drain path to a memory copy and a non-blocking enqueue.
// A dedicated goroutine performs the downstream write afterwards, so the child
// is never waiting on the log. Write always reports the full length and never
// returns an error, because a short count or an error here tears down the
// child's pipe.
type DrainWriter struct {
	sink io.Writer

	queue chan []byte

	// dropped counts bytes discarded because the queue was full. The delivery
	// goroutine reports them in-stream so a gap is visible instead of silent.
	dropped atomic.Uint64

	mu     sync.Mutex
	closed bool

	done chan struct{}
}

// NewDrainWriter returns a writer that forwards to sink off the caller's
// goroutine. Close flushes what is queued and stops that goroutine.
func NewDrainWriter(sink io.Writer) *DrainWriter {
	// A nil sink would panic inside the delivery goroutine, where the panic
	// cannot be attributed to the caller. Discard instead.
	if sink == nil {
		sink = io.Discard
	}
	w := &DrainWriter{
		sink:  sink,
		queue: make(chan []byte, drainQueueSize),
		done:  make(chan struct{}),
	}
	go w.deliver()
	return w
}

// Write consumes p in full. It never blocks and never fails, so a child can
// always drain its pipe even while the log sink is stuck.
func (w *DrainWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}

	// Copy first: os/exec reuses its copy buffer as soon as Write returns.
	chunk := make([]byte, len(p))
	copy(chunk, p)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		// Past Close there is no goroutine left to deliver these bytes. Accept
		// them anyway: reporting anything else would tear down the pipe of a
		// child that is still writing. Close is only reached once the copiers
		// have stopped, so this is a safety net, not a routine path.
		return len(p), nil
	}
	select {
	case w.queue <- chunk:
	default:
		w.dropped.Add(uint64(len(p)))
	}
	return len(p), nil
}

// Close flushes queued output and stops the delivery goroutine. It returns once
// the sink has seen everything that was still queued, so a caller can rely on a
// process's last output being in place before reporting on it. Idempotent.
func (w *DrainWriter) Close() {
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return
	}
	w.closed = true
	close(w.queue)
	w.mu.Unlock()
	<-w.done
}

func (w *DrainWriter) deliver() {
	defer close(w.done)
	for chunk := range w.queue {
		w.flushDropped()
		if _, err := w.sink.Write(chunk); err != nil {
			continue
		}
	}
	// Drops counted after the last chunk was dequeued would otherwise never be
	// reported.
	w.flushDropped()
}

// flushDropped writes the in-stream gap marker, matching the one the Monitor's
// broadcast path emits so both show up the same way in the log panel.
func (w *DrainWriter) flushDropped() {
	dropped := w.dropped.Swap(0)
	if dropped == 0 {
		return
	}
	// Deliberately ignore the error: this writer exists so that no log-sink
	// failure can propagate back to the child's pipe.
	_, _ = w.sink.Write(fmt.Appendf(nil, "\n— %d bytes dropped —\n", dropped))
}
