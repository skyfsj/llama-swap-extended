package logmon

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/event"
)

const DataEventID = 0x04

type DataEvent struct {
	Data []byte
}

func (e DataEvent) Type() uint32 {
	return DataEventID
}

// circularBuffer is a fixed-size circular byte buffer that overwrites
// oldest data when full. It provides O(1) writes and O(n) reads.
type circularBuffer struct {
	data []byte
	head int
	size int
	// wrapped records that at least one byte was discarded from the front.
	// Consumers use it to tell "complete history" apart from "tail only" —
	// after a wrap the retained bytes start at an arbitrary offset, which is
	// usually the middle of a line.
	wrapped bool
}

func newCircularBuffer(capacity int) *circularBuffer {
	return &circularBuffer{
		data: make([]byte, capacity),
		head: 0,
		size: 0,
	}
}

func (cb *circularBuffer) Write(p []byte) {
	if len(p) == 0 {
		return
	}

	cap := len(cb.data)

	if len(p) >= cap {
		copy(cb.data, p[len(p)-cap:])
		cb.head = 0
		cb.size = cap
		cb.wrapped = true
		return
	}

	firstPart := cap - cb.head
	if firstPart >= len(p) {
		copy(cb.data[cb.head:], p)
		cb.head = (cb.head + len(p)) % cap
	} else {
		copy(cb.data[cb.head:], p[:firstPart])
		copy(cb.data[:len(p)-firstPart], p[firstPart:])
		cb.head = len(p) - firstPart
	}

	cb.size += len(p)
	if cb.size > cap {
		cb.size = cap
		cb.wrapped = true
	}
}

// lastByte reports the most recently written byte, or 0 when nothing has been
// written. Callers use it to tell whether the buffer currently sits at the
// start of a line.
func (cb *circularBuffer) lastByte() byte {
	if cb.size == 0 {
		return 0
	}
	capacity := len(cb.data)
	return cb.data[(cb.head-1+capacity)%capacity]
}

func (cb *circularBuffer) GetHistory() []byte {
	if cb.size == 0 {
		return nil
	}

	result := make([]byte, cb.size)
	cap := len(cb.data)

	start := (cb.head - cb.size + cap) % cap

	if start+cb.size <= cap {
		copy(result, cb.data[start:start+cb.size])
	} else {
		firstPart := cap - start
		copy(result[:firstPart], cb.data[start:])
		copy(result[firstPart:], cb.data[:cb.size-firstPart])
	}

	return result
}

type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError

	// BufferSize bounds the per-monitor history retained for reconnecting
	// log streams and crash archives. It must comfortably hold a full cold
	// start of a verbose backend: a vLLM engine prints several hundred lines
	// while loading weights, and a 100 KiB tail was routinely discarding the
	// actual failure before the crash snapshot was taken. 1 MiB keeps a
	// complete startup transcript at a negligible memory cost (one buffer per
	// live process monitor).
	BufferSize = 1 << 20
)

// truncatedHistoryMarker prefixes a history that lost its head to the ring
// buffer wrap, so a reader can distinguish "process printed nothing yet" from
// "the snapshot only kept the tail".
const truncatedHistoryMarker = "\n— earlier output truncated —\n"

type Monitor struct {
	eventbus *event.Dispatcher
	mu       sync.RWMutex
	buffer   *circularBuffer
	bufferMu sync.RWMutex

	stdout io.Writer

	// broadcastCh hands log data to a dedicated goroutine that owns the
	// (backpressuring) event bus. Write performs a non-blocking send so that
	// slow subscribers can never stall the upstream process's stdout drain.
	broadcastCh chan []byte
	dropped     atomic.Uint64

	// broadcastMu guards broadcastCh and broadcastOpen so Close can never race
	// a send into a closed channel. Write takes it briefly per call; the hot
	// path cost is negligible next to the stdout write it follows.
	broadcastMu   sync.Mutex
	broadcastOpen bool

	level      Level
	prefix     string
	timeFormat string
}

func New() *Monitor {
	return NewWriter(os.Stdout)
}

func NewWriter(stdout io.Writer) *Monitor {
	// A nil writer would panic on first Write; discard instead so callers
	// without a sink (e.g. topology planners) cannot crash the process.
	if stdout == nil {
		stdout = io.Discard
	}
	m := &Monitor{
		eventbus:      event.NewDispatcherConfig(1000),
		buffer:        nil,
		stdout:        stdout,
		broadcastCh:   make(chan []byte, 1024),
		broadcastOpen: true,
		level:         LevelInfo,
		prefix:        "",
		timeFormat:    "",
	}
	go m.broadcastLoop()
	return m
}

func (w *Monitor) Write(p []byte) (n int, err error) {
	if len(p) == 0 {
		return 0, nil
	}

	// Record before displaying. The history buffer is what the log panel, the
	// crash archive and atLineStart() read, so it has to survive a sink that is
	// stalled, closed, or gone.
	w.bufferMu.Lock()
	if w.buffer == nil {
		w.buffer = newCircularBuffer(BufferSize)
	}
	w.buffer.Write(p)
	w.bufferMu.Unlock()

	bufferCopy := make([]byte, len(p))
	copy(bufferCopy, p)
	w.broadcastMu.Lock()
	if w.broadcastOpen {
		select {
		case w.broadcastCh <- bufferCopy:
		default:
			// Subscribers (e.g. the web UI log stream) can't keep up. Drop the
			// live broadcast rather than block: Write can run on a child
			// process's output delivery goroutine, and blocking it there backs
			// the child's stdout pipe up until the child itself stalls (issue
			// #875). GetHistory() still has the data for reconnecting clients,
			// and the dropped bytes are reported in-stream below.
			w.dropped.Add(uint64(len(p)))
		}
	}
	w.broadcastMu.Unlock()

	// Every byte was recorded and broadcast, so every byte was consumed: return
	// the full length, never a short count. exec.Cmd feeds a monitor through
	// io.Copy, where a short write becomes ErrShortWrite, the copier closes the
	// child's stdout pipe, and the child dies with SIGPIPE on its next write.
	// The sink's own error is dropped along with its count — this monitor
	// records and forwards, and no caller up the chain can act on a closed
	// terminal or an unwritable log file anyway (see DrainWriter, which keeps
	// this whole path off the child in the first place).
	_, _ = w.stdout.Write(p)
	return len(p), nil
}

func (w *Monitor) GetHistory() []byte {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	if w.buffer == nil {
		return nil
	}
	history := w.buffer.GetHistory()
	// Once the buffer has wrapped, the retained tail starts at an arbitrary
	// byte offset and therefore begins mid-line. Trim to the first complete
	// line so every consumer (log stream, crash archive) renders whole lines
	// instead of a fragment that looks like corrupted output. A truncated
	// history is explicitly labelled, so a reader can never mistake it for a
	// process that produced nothing before that point.
	if w.buffer.wrapped {
		if index := bytes.IndexByte(history, '\n'); index >= 0 && index+1 < len(history) {
			return append([]byte(truncatedHistoryMarker), history[index+1:]...)
		}
	}
	return history
}

// Clear releases the buffer memory, making it eligible for GC.
// The buffer will be lazily re-allocated on the next Write.
func (w *Monitor) Clear() {
	w.bufferMu.Lock()
	w.buffer = nil
	w.bufferMu.Unlock()
}

// Close stops the broadcast goroutine so a discarded monitor can be garbage
// collected along with its buffer. History stays readable for late GetHistory
// callers (crash archives, reconnecting clients), and later Writes keep
// filling the buffer but are no longer broadcast. Close is idempotent and
// safe to call concurrently with Write.
func (w *Monitor) Close() {
	w.broadcastMu.Lock()
	defer w.broadcastMu.Unlock()
	if !w.broadcastOpen {
		return
	}
	w.broadcastOpen = false
	close(w.broadcastCh)
}

// IsClosed reports whether Close has been called.
func (w *Monitor) IsClosed() bool {
	w.broadcastMu.Lock()
	defer w.broadcastMu.Unlock()
	return !w.broadcastOpen
}

// SeedHistory preloads the history buffer without forwarding data to the
// stdout sink or live subscribers. A replaced process seeds its replacement
// this way so operator-facing log history survives a restart.
func (w *Monitor) SeedHistory(data []byte) {
	if len(data) == 0 {
		return
	}
	w.bufferMu.Lock()
	if w.buffer == nil {
		w.buffer = newCircularBuffer(BufferSize)
	}
	w.buffer.Write(data)
	w.bufferMu.Unlock()
}

func (w *Monitor) OnLogData(callback func(data []byte)) context.CancelFunc {
	return event.Subscribe(w.eventbus, func(e DataEvent) {
		callback(e.Data)
	})
}

// broadcastLoop is the only place that publishes to the (backpressuring)
// event bus. If subscribers are slow it blocks here, never on Write. Before
// delivering a message it flushes any pending dropped-byte count as an
// in-stream marker so the UI shows where the gap is.
func (w *Monitor) broadcastLoop() {
	for msg := range w.broadcastCh {
		if dropped := w.dropped.Swap(0); dropped > 0 {
			notice := fmt.Appendf(nil, "\n— %d bytes dropped —\n", dropped)
			event.Publish(w.eventbus, DataEvent{Data: notice})
		}
		event.Publish(w.eventbus, DataEvent{Data: msg})
	}
}

func (w *Monitor) SetPrefix(prefix string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.prefix = prefix
}

func (w *Monitor) SetLogLevel(level Level) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.level = level
}

func (w *Monitor) SetLogTimeFormat(timeFormat string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.timeFormat = timeFormat
}

func (w *Monitor) formatMessage(level string, msg string) []byte {
	prefix := ""
	if w.prefix != "" {
		prefix = fmt.Sprintf("[%s] ", w.prefix)
	}
	timestamp := ""
	if w.timeFormat != "" {
		timestamp = fmt.Sprintf("%s ", time.Now().Format(w.timeFormat))
	}
	return fmt.Appendf(nil, "%s%s[%s] %s\n", timestamp, prefix, level, msg)
}

func (w *Monitor) log(level Level, msg string) {
	// level/prefix/timeFormat are mutated by the Set* setters under w.mu, so
	// the read side must hold at least a read lock to stay race-free.
	w.mu.RLock()
	if level < w.level {
		w.mu.RUnlock()
		return
	}
	formatted := w.formatMessage(level.String(), msg)
	w.mu.RUnlock()
	// An upstream that stops mid-line — a progress bar redrawn with \r, or a
	// process killed partway through a write — leaves the buffer without a
	// trailing newline. Appending our own line to it splices the two together,
	// which is how a lifecycle diagnostic ended up glued to the end of a
	// truncated warmup line instead of starting its own.
	if !w.atLineStart() {
		formatted = append([]byte("\n"), formatted...)
	}
	w.Write(formatted)
}

// atLineStart reports whether the last byte written to this monitor ended a
// line. An empty buffer counts as the start of a line.
func (w *Monitor) atLineStart() bool {
	w.bufferMu.RLock()
	defer w.bufferMu.RUnlock()
	if w.buffer == nil || w.buffer.size == 0 {
		return true
	}
	return w.buffer.lastByte() == '\n'
}

func (w *Monitor) Debug(msg string) { w.log(LevelDebug, msg) }
func (w *Monitor) Info(msg string)  { w.log(LevelInfo, msg) }
func (w *Monitor) Warn(msg string)  { w.log(LevelWarn, msg) }
func (w *Monitor) Error(msg string) { w.log(LevelError, msg) }

func (w *Monitor) Debugf(format string, args ...any) {
	w.log(LevelDebug, fmt.Sprintf(format, args...))
}

func (w *Monitor) Infof(format string, args ...any) {
	w.log(LevelInfo, fmt.Sprintf(format, args...))
}

func (w *Monitor) Warnf(format string, args ...any) {
	w.log(LevelWarn, fmt.Sprintf(format, args...))
}

func (w *Monitor) Errorf(format string, args ...any) {
	w.log(LevelError, fmt.Sprintf(format, args...))
}

func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	default:
		return "UNKNOWN"
	}
}
