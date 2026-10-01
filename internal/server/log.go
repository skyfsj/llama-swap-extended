package server

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// NewLoggers builds the proxy, upstream, and combined (mux) log monitors,
// wiring each one's output per the logToStdout config value. The proxy and
// upstream monitors write into muxlog (rather than os.Stdout directly) so
// muxlog accumulates a combined history for the /logs endpoints, while each
// monitor keeps its own per-source history and event subscribers.
//
// Behaviour matches the legacy ProxyManager:
//
//   - none:     everything discarded
//   - both:     proxy + upstream both routed to muxlog -> stdout
//   - upstream: only upstream routed to muxlog -> stdout; proxy discarded
//   - proxy:    only proxy routed to muxlog -> stdout; upstream discarded
//
// An empty or unrecognised value behaves like "proxy".
func NewLoggers(logToStdout string) (muxlog, proxylog, upstreamlog *logmon.Monitor) {
	switch logToStdout {
	case config.LogToStdoutNone:
		muxlog = logmon.NewWriter(io.Discard)
		proxylog = logmon.NewWriter(io.Discard)
		upstreamlog = logmon.NewWriter(io.Discard)
	case config.LogToStdoutBoth:
		muxlog = logmon.NewWriter(os.Stdout)
		proxylog = logmon.NewWriter(muxlog)
		upstreamlog = logmon.NewWriter(muxlog)
	case config.LogToStdoutUpstream:
		muxlog = logmon.NewWriter(os.Stdout)
		proxylog = logmon.NewWriter(io.Discard)
		upstreamlog = logmon.NewWriter(muxlog)
	default:
		// config.LogToStdoutProxy, and the fallback for an unset value.
		muxlog = logmon.NewWriter(os.Stdout)
		proxylog = logmon.NewWriter(muxlog)
		upstreamlog = logmon.NewWriter(io.Discard)
	}
	return muxlog, proxylog, upstreamlog
}

// handleLogs serves the historical proxy/upstream log. HTML clients are
// redirected to the UI.
func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
		// The combined proxy/upstream history has no durable model boundary.
		// Serving it to a model-scoped key would leak neighbouring models, so
		// require an unrestricted identity for this legacy aggregate endpoint.
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: combined logs require an unrestricted key")
		return
	}
	if strings.Contains(r.Header.Get("Accept"), "text/html") {
		http.Redirect(w, r, "/ui/", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/plain")
	w.Write(s.muxlog.GetHistory())
}

// getLogger resolves a log monitor by id. An empty id maps to the combined
// muxlog; "proxy" and "upstream" select the respective monitors.
func (s *Server) getLogger(logMonitorID string) (*logmon.Monitor, error) {
	switch logMonitorID {
	case "":
		return s.muxlog, nil
	case "proxy":
		return s.proxylog, nil
	case "upstream":
		return s.upstreamlog, nil
	default:
		if _, modelID, _, found := swaputil.FindModelInPath(s.currentConfig(), "/"+logMonitorID); found {
			if log, ok := s.local.ProcessLogger(modelID); ok {
				return log, nil
			}
		}
		return nil, fmt.Errorf("invalid logger. Use 'proxy', 'upstream' or a model's ID")
	}
}

// logStreamRecheckInterval bounds how long a model log stream keeps tailing a
// monitor that no longer belongs to the model's current process. Model
// processes are replaced on restarts and config reloads; each replacement
// creates a fresh monitor, so a stream that never re-resolves would tail an
// orphaned buffer forever while showing the operator a live-looking log.
const logStreamRecheckInterval = time.Second

// logStreamSendBuffer bounds how much live log data one HTTP stream may hold
// while its consumer catches up. Sized to cover a full render stall rather than
// a single frame, so a burst is queued instead of dropped.
const logStreamSendBuffer = 256

// handleLogStream tails a log monitor: it writes the history then streams live
// log data until the client disconnects, the server shuts down, or (for a
// model's monitor) the model's process is replaced.
func (s *Server) handleLogStream(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	w.Header().Set("Content-Type", "text/plain")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Transfer-Encoding", "chunked")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// prevent nginx from buffering streamed logs
	w.Header().Set("X-Accel-Buffering", "no")

	logMonitorID := strings.TrimPrefix(r.PathValue("logMonitorID"), "/")
	// Strip a query string if it leaked into the path segment.
	if idx := strings.Index(logMonitorID, "?"); idx != -1 {
		logMonitorID = logMonitorID[:idx]
	}
	if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
		// Only a concrete per-model logger can be safely scoped. The combined,
		// proxy, and upstream monitors intentionally remain unavailable because
		// their history and live events mix multiple models.
		_, modelID, _, found := swaputil.FindModelInPath(cfg, "/"+logMonitorID)
		if !found || !modelAllowedForIdentity(cfg, identity, modelID) {
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: log stream is not available for this API key")
			return
		}
	}

	logger, err := s.getLogger(logMonitorID)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	_, skipHistory := r.URL.Query()["no-history"]
	if !skipHistory {
		if history := logger.GetHistory(); len(history) != 0 {
			w.Write(history)
		}
	}
	// Commit the response even when the monitor has no history yet. Without
	// this flush, fetch() waits for the first log line before resolving, which
	// makes an idle or stopped model look like a permanently failed log panel.
	flusher.Flush()

	// The forwarding buffer must be wide enough to ride out a consumer's
	// worst-case stall. A log panel re-renders its whole bounded tail per
	// update, so a cold-start burst can keep the browser busy for a few hundred
	// milliseconds while the process keeps writing; a 10-slot buffer overflowed
	// in that window and the bytes surfaced as an in-stream "— N bytes dropped
	// —" notice. Queueing is not throttling: every chunk is still delivered, in
	// order, and the monitor keeps its own 1024-slot buffer upstream of this
	// one, so the two stages are no longer a 100x mismatch.
	sendChan := make(chan []byte, logStreamSendBuffer)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	// Handler-level drops happen when this stream's consumer (the HTTP
	// response) remains slower than the buffer above. Report them in-stream,
	// matching how the monitor itself reports backpressured drops.
	var droppedBytes atomic.Uint64
	cancelSub := logger.OnLogData(func(data []byte) {
		select {
		case sendChan <- data:
		case <-ctx.Done():
		default:
			droppedBytes.Add(uint64(len(data)))
		}
	})
	defer cancelSub()

	// Only per-model monitors can be replaced out from under this stream; the
	// combined, proxy, and upstream monitors live for the whole process.
	isModelStream := logMonitorID != "" && logMonitorID != "proxy" && logMonitorID != "upstream"
	ticker := time.NewTicker(logStreamRecheckInterval)
	defer ticker.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdownCtx.Done():
			return
		case data := <-sendChan:
			if dropped := droppedBytes.Swap(0); dropped > 0 {
				w.Write([]byte(fmt.Sprintf("\n— %d bytes dropped —\n", dropped)))
			}
			w.Write(data)
			flusher.Flush()
		case <-ticker.C:
			if !isModelStream {
				continue
			}
			current, err := s.getLogger(logMonitorID)
			if err != nil || current != logger {
				// The model's process was replaced (config reload, restart,
				// removal): end the response so the client reconnects and
				// resumes from the new monitor's history instead of tailing
				// an orphaned buffer forever.
				return
			}
		}
	}
}

// requestLogPathSkips lists path prefixes excluded from the access log because
// they are polled frequently and would drown out useful entries.
var requestLogPathSkips = []string{"/wol-health", "/api/performance", "/metrics"}

// RequestError is the context persisted for a failed HTTP request. It mirrors
// the access log fields plus two diagnostics an archive without them is nearly
// useless for: the model the request resolved to, and the error text the
// client was actually shown.
//
// It deliberately does not include the request or response bodies. Prompts
// and completions are the operator's data, not log material, and the error
// text is captured from the response the server wrote, so nothing here
// depends on re-reading a stream that has already been consumed.
type RequestError struct {
	ClientIP  string
	Method    string
	Path      string
	Proto     string
	UserAgent string
	Status    int
	BodyBytes int
	Duration  time.Duration
	// Model is the configured model ID the request resolved to, empty when the
	// request carried no model selector or none could be resolved.
	Model string
	// Detail is the client-visible error body, bounded and truncated. It is
	// empty for a response that carried no body.
	Detail string
}

// requestErrorDetailBytes bounds the response body copied into a RequestError.
// The error bodies this archive exists for are one or two lines of JSON, so
// the bound is generous while still being fixed: a streaming response that
// fails midway writes a prefix, not the whole stream.
const requestErrorDetailBytes = 8 << 10

// RequestErrorRecorder receives requests whose final status is an error or a
// client-closed 499. The variadic form keeps the middleware compatible with
// embedders and tests that only need the historical access-log behaviour.
type RequestErrorRecorder func(RequestError)

// statusRecorder wraps an http.ResponseWriter to capture the response status
// code and the number of body bytes written, so the access log can report
// them. Flush is forwarded so streaming handlers (SSE) still work, and Hijack
// is forwarded so httputil.ReverseProxy can upgrade websocket connections.
type statusRecorder struct {
	http.ResponseWriter
	status      int
	size        int
	wroteHeader bool
	// detail holds a bounded prefix of the response body, captured only once
	// the status is known to be an error. Writing it eagerly for every
	// response would copy the whole inference stream of every successful
	// request to capture the few that fail.
	detail     bytes.Buffer
	detailOver bool
}

// WriteHeader commits the status; see the comment on the type.
func (sr *statusRecorder) WriteHeader(code int) {
	// net/http commits the first status and ignores every later one, so the
	// access log has to do the same. Handlers do call WriteHeader after a
	// response has started — a shutdown or dispatch error arriving once the
	// loading stream has already sent its 200 — and recording the second code
	// would report a status the client never received.
	if sr.wroteHeader {
		return
	}
	sr.status = code
	sr.wroteHeader = true
	sr.ResponseWriter.WriteHeader(code)
}

// MarkStatus records code for the access log without writing to the client.
// This is the outermost recorder, so there is nothing further to forward to.
func (sr *statusRecorder) MarkStatus(code int) { sr.status = code }

// WroteHeader reports whether a response status reached the client.
func (sr *statusRecorder) WroteHeader() bool { return sr.wroteHeader }

func (sr *statusRecorder) Write(b []byte) (int, error) {
	// An implicit 200 from net/http still counts as a response the client
	// started receiving, so it must not be overwritten by a late sentinel.
	sr.wroteHeader = true
	n, err := sr.ResponseWriter.Write(b)
	sr.size += n
	if sr.status >= http.StatusBadRequest && sr.detail.Len() < requestErrorDetailBytes {
		if b2 := b; len(b2) > 0 {
			room := requestErrorDetailBytes - sr.detail.Len()
			if len(b2) <= room {
				sr.detail.Write(b2)
			} else {
				sr.detail.Write(b2[:room])
				sr.detailOver = true
			}
		}
	}
	return n, err
}

// detail returns the bounded captured body, with a marker when it was
// truncated so a reader never mistakes a prefix for the whole response.
func (sr *statusRecorder) detailText() string {
	if sr.detail.Len() == 0 {
		return ""
	}
	if sr.detailOver {
		return "… truncated …\n" + sr.detail.String()
	}
	return sr.detail.String()
}

func (sr *statusRecorder) Flush() {
	f, ok := sr.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	// Flushing commits net/http's implicit 200 and puts it on the wire, so the
	// client has started receiving a response even if nothing wrote a header.
	sr.wroteHeader = true
	f.Flush()
}

// Hijack forwards to the underlying ResponseWriter so httputil.ReverseProxy can
// take over the connection for websocket upgrades.
func (sr *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if hj, ok := sr.ResponseWriter.(http.Hijacker); ok {
		return hj.Hijack()
	}
	return nil, nil, fmt.Errorf("underlying ResponseWriter does not support hijacking")
}

// clientIP resolves the originating client address, preferring proxy headers
// over the raw connection address.
func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, found := strings.Cut(xff, ","); found {
			return strings.TrimSpace(first)
		}
		return strings.TrimSpace(xff)
	}
	if xr := r.Header.Get("X-Real-IP"); xr != "" {
		return strings.TrimSpace(xr)
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

// CreateRequestLogMiddleware returns middleware that records one access-log
// line per request to proxylog, in the legacy format:
//
//	clientIP "METHOD PATH PROTO" status bodySize "UA" duration
//
// Frequently-polled health/metrics paths are skipped. The path is captured
// before next runs because /upstream rewrites the request URL in place.
//
// This is the outermost middleware, so it also owns the request-diagnostics
// holder the inner layers publish the resolved model into — contexts flow
// downward only, so this is the only place the holder can be created for the
// whole chain to share.
func CreateRequestLogMiddleware(proxylog *logmon.Monitor, recorders ...RequestErrorRecorder) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Match on a path boundary rather than a bare prefix. With HasPrefix
			// alone, /metricsfoo was excluded from the access log even though it
			// is a distinct route — a client could suppress its own entry just
			// by choosing a path that extends one of these.
			for _, prefix := range requestLogPathSkips {
				if r.URL.Path == prefix || strings.HasPrefix(r.URL.Path, prefix+"/") {
					next.ServeHTTP(w, r)
					return
				}
			}

			start := time.Now()
			ip, method, path, proto, ua := clientIP(r), r.Method, r.URL.Path, r.Proto, r.UserAgent()

			// This is the outermost middleware, so the context here is still
			// the connection's own. Remember it before anything downstream
			// derives a cancellable child, so a request cancelled server-side
			// is not later reported as a client that hung up.
			r = swaputil.WithClientContext(r)
			// Diagnostics flow the other way: inner layers resolve the model
			// (or reject the request before they can), and publish it into a
			// holder this layer created. See swaputil.RequestDiagnostics.
			r = r.WithContext(swaputil.WithRequestDiagnostics(r.Context()))

			rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
			next.ServeHTTP(rec, r)

			// Cancellation branches (a client hanging up during a cold model
			// load, for one) return without writing anything, which would
			// otherwise be logged as the seeded 200. net/http cancels the
			// request context when the client disconnects or when the
			// top-level ServeHTTP returns; this middleware is inside that
			// call, so a done context here means the client really left.
			// Deriving it once covers every such branch, including ones added
			// later. See #1029.
			swaputil.MarkClientClosed(rec, r)

			duration := time.Since(start)
			proxylog.Infof("Request %s \"%s %s %s\" %d %d \"%s\" %v",
				ip, method, path, proto, rec.status, rec.size, ua, duration)
			if rec.status >= http.StatusBadRequest {
				event := RequestError{
					ClientIP:  ip,
					Method:    method,
					Path:      path,
					Proto:     proto,
					UserAgent: ua,
					Status:    rec.status,
					BodyBytes: rec.size,
					Duration:  duration,
					Model:     swaputil.RequestModel(r.Context()),
					Detail:    rec.detailText(),
				}
				for _, recorder := range recorders {
					if recorder != nil {
						recorder(event)
					}
				}
			}
		})
	}
}
