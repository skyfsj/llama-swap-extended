package gateway

import (
	"context"
	"io"
	"sync"
)

// bodyEvent is one chunk (or read error) produced by the pump.
type bodyEvent struct {
	data []byte
	err  error
}

// bodyPump reads an upstream response body on its own goroutine so the
// forwarding path can decide whether to commit while the body is still
// arriving. A plain reader cannot be both "inspect for a moment" and
// "continue from exactly where inspection stopped"; the pump gives both
// without a second copy of the data.
//
// The pump stops as soon as the reader is abandoned, so a retry does not leave
// a goroutine behind: closing stop (or cancelling the request) releases it even
// if it is blocked sending a chunk nobody reads.
type bodyPump struct {
	events <-chan bodyEvent
	stop   func()
}

func startBodyPump(ctx context.Context, body io.Reader) *bodyPump {
	events := make(chan bodyEvent, 8)
	done := make(chan struct{})
	var once sync.Once
	stop := func() { once.Do(func() { close(done) }) }

	go func() {
		defer close(events)
		for {
			buf := make([]byte, 32*1024)
			n, err := body.Read(buf)
			if n > 0 {
				select {
				case events <- bodyEvent{data: buf[:n]}:
				case <-done:
					return
				case <-ctx.Done():
					return
				}
			}
			if err != nil {
				if err != io.EOF {
					select {
					case events <- bodyEvent{err: err}:
					case <-done:
					case <-ctx.Done():
					}
				}
				return
			}
		}
	}()
	return &bodyPump{events: events, stop: stop}
}

// pumpReader exposes the not-yet-consumed part of a pumped body as an
// io.Reader, so the usual streaming forwarder keeps working unchanged.
type pumpReader struct {
	events  <-chan bodyEvent
	pending []byte
	err     error
}

func (r *pumpReader) Read(p []byte) (int, error) {
	for len(r.pending) == 0 {
		if r.err != nil {
			err := r.err
			r.err = nil
			return 0, err
		}
		ev, ok := <-r.events
		if !ok {
			return 0, io.EOF
		}
		if len(ev.data) > 0 {
			r.pending = ev.data
		}
		if ev.err != nil && len(ev.data) == 0 {
			r.err = ev.err
		} else if ev.err != nil {
			// Hand the data out first; the error follows on the next call.
			r.err = ev.err
		}
	}
	n := copy(p, r.pending)
	r.pending = r.pending[n:]
	return n, nil
}
