package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/protocol/realtime"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// CreateRealtimeMiddleware resolves the model from the first
// session.update WebSocket frame before the normal model scheduler runs. The
// frame is retained byte-for-byte and replayed through the hijacked reader so
// the selected backend observes exactly what the client sent.
func CreateRealtimeMiddleware(cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if (r.URL.Path != "/v1/realtime" && r.URL.Path != "/v/realtime") || !swaputil.IsWebSocketUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}

			if hj, ok := w.(http.Hijacker); ok {
				conn, rw, err := hj.Hijack()
				if err != nil {
					// The underlying writer may already have committed part of the
					// upgrade. Keep the normal HTTP path out of this branch and let
					// the caller's server connection report the transport error.
					return
				}
				// The first session.update frame is read after HTTP has handed the
				// connection to us. A client can cancel the request before sending
				// that frame; keep the read interruptible by closing the hijacked
				// connection when its context is cancelled. The result channel is
				// buffered so the reader goroutine can always publish after the
				// cancellation path returns.
				ctx, cancel := context.WithCancel(r.Context())
				defer cancel()
				type readResult struct {
					frame realtime.Frame
					wire  []byte
					err   error
				}
				readCh := make(chan readResult, 1)
				go func() {
					frame, wire, readErr := realtime.ReadMessage(rw.Reader)
					readCh <- readResult{frame: frame, wire: wire, err: readErr}
				}()
				var frame realtime.Frame
				var wire []byte
				var readErr error
				select {
				case result := <-readCh:
					frame, wire, readErr = result.frame, result.wire, result.err
				case <-ctx.Done():
					_ = conn.Close()
					return
				}
				if readErr != nil {
					_ = conn.Close()
					return
				}
				model, modelErr := realtime.ExtractModel(frame)
				if modelErr != nil {
					writeHijackedError(rw, http.StatusBadRequest, modelErr.Error())
					_ = conn.Close()
					return
				}
				identity := identityFromContext(r.Context())
				if identity.ID != "" && !identity.Has(auth.ScopeInference) {
					writeHijackedError(rw, http.StatusForbidden, "forbidden: missing scope "+auth.ScopeInference)
					_ = conn.Close()
					return
				}
				if real, found := cfg.RealModelName(model); found {
					model = real
				}
				if len(identity.Models) > 0 && !identity.HasModel(model) {
					writeHijackedError(rw, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
					_ = conn.Close()
					return
				}
				query := r.URL.Query()
				query.Set("model", model)
				r.URL.RawQuery = query.Encode()
				// ReverseProxy reads client frames from the hijacked reader. Put the
				// already-consumed first frame back in front of any bytes buffered
				// by net/http so it can be sent upstream after the 101 handshake.
				reader := bufio.NewReader(io.MultiReader(bytes.NewReader(wire), rw.Reader))
				bridge := &realtimeHijackedWriter{header: make(http.Header), conn: conn, rw: bufio.NewReadWriter(reader, rw.Writer)}
				defer bridge.finish()
				go func() {
					<-ctx.Done()
					_ = conn.Close()
				}()
				next.ServeHTTP(bridge, r.WithContext(ctx))
				return
			}

			// httptest.ResponseRecorder and HTTP/2 do not expose Hijacker. Accept
			// a frame supplied as the request body for deterministic unit tests;
			// production HTTP/1 upgrades use the branch above.
			if r.Body == nil || r.Body == http.NoBody {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "realtime session frame is required")
				return
			}
			wire, err := io.ReadAll(io.LimitReader(r.Body, realtime.MaxFramePayload+33))
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read realtime session frame")
				return
			}
			if len(wire) > realtime.MaxFramePayload+32 {
				swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, "realtime session frame exceeds payload limit")
				return
			}
			model, _, err := realtime.FirstModel(wire)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			identity := identityFromContext(r.Context())
			if identity.ID != "" && !identity.Has(auth.ScopeInference) {
				swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: missing scope "+auth.ScopeInference)
				return
			}
			if real, found := cfg.RealModelName(model); found {
				model = real
			}
			if len(identity.Models) > 0 && !identity.HasModel(model) {
				swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
				return
			}
			query := r.URL.Query()
			query.Set("model", model)
			r.URL.RawQuery = query.Encode()
			r.Body = io.NopCloser(bytes.NewReader(wire))
			next.ServeHTTP(w, r)
		})
	}
}

// realtimeHijackedWriter is a ResponseWriter facade over an already-hijacked
// connection. ReverseProxy calls Hijack on it and receives a reader with the
// first client frame replayed above. If an upstream middleware rejects the
// request before Hijack, Write emits a minimal valid HTTP response instead of
// writing a bare JSON body onto the TCP stream.
type realtimeHijackedWriter struct {
	header    http.Header
	conn      net.Conn
	rw        *bufio.ReadWriter
	status    int
	handedOff bool
	written   bool
}

func (w *realtimeHijackedWriter) Header() http.Header { return w.header }

func (w *realtimeHijackedWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *realtimeHijackedWriter) Write(data []byte) (int, error) {
	if w.handedOff {
		return w.rw.Write(data)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if err := w.writeHTTPResponse(data); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (w *realtimeHijackedWriter) Flush() {
	if w.rw != nil {
		_ = w.rw.Flush()
	}
}

func (w *realtimeHijackedWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	w.handedOff = true
	return w.conn, w.rw, nil
}

func (w *realtimeHijackedWriter) writeHTTPResponse(body []byte) error {
	if w.rw == nil {
		return fmt.Errorf("realtime connection is unavailable")
	}
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	if w.header.Get("Content-Type") == "" {
		w.header.Set("Content-Type", "application/json")
	}
	w.header.Set("Content-Length", fmt.Sprintf("%d", len(body)))
	if _, err := fmt.Fprintf(w.rw, "HTTP/1.1 %d %s\r\n", status, http.StatusText(status)); err != nil {
		return err
	}
	for key, values := range w.header {
		for _, value := range values {
			if _, err := fmt.Fprintf(w.rw, "%s: %s\r\n", key, value); err != nil {
				return err
			}
		}
	}
	if _, err := w.rw.WriteString("\r\n"); err != nil {
		return err
	}
	if _, err := w.rw.Write(body); err != nil {
		return err
	}
	if err := w.rw.Flush(); err != nil {
		return err
	}
	w.written = true
	return nil
}

// finish closes the HTTP side of a hijacked request when the downstream
// handler rejected it with only WriteHeader (or returned without writing at
// all). ReverseProxy normally takes ownership through Hijack; this fallback
// prevents an early authorization/dispatch failure from leaving a client
// waiting forever for headers on an otherwise valid TCP connection.
func (w *realtimeHijackedWriter) finish() {
	if w == nil || w.handedOff || w.written || w.rw == nil {
		return
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	_ = w.writeHTTPResponse(nil)
}

func writeHijackedError(rw *bufio.ReadWriter, status int, message string) {
	if rw == nil {
		return
	}
	body, err := json.Marshal(map[string]string{"error": message})
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(rw, "HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n", status, http.StatusText(status), len(body))
	_, _ = rw.Write(body)
	_ = rw.Flush()
}
