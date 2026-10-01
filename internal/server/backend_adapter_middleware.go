package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/backend"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// backendTransformBodyLimit bounds the amount of request/response data a
// protocol adapter may buffer. A streaming request is never buffered on the
// response side; a large non-streaming response is committed unchanged once
// this limit is reached.
const backendTransformBodyLimit = 64 << 20

// CreateBackendAdapterMiddleware invokes the adapter associated with the
// canonical model in the request context. Request transforms run before the
// upstream router consumes the body. Non-streaming JSON responses are buffered
// long enough for a response transform; SSE, WebSocket and explicitly flushed
// responses continue through untouched so adapter plumbing cannot break token
// streaming or the Realtime bridge.
//
// Responses↔Chat conversion is deliberately kept in
// CreateResponsesAdapterMiddleware. This middleware is the generic seam for a
// backend-specific adapter and does not infer protocol semantics from a model
// name or proxy URL.
func CreateBackendAdapterMiddleware(cfg config.Config, adapters map[string]backend.BackendAdapter) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if len(adapters) == 0 || r == nil || swaputil.IsWebSocketUpgrade(r) {
				next.ServeHTTP(w, r)
				return
			}
			data, ok := swaputil.ReadContext(r.Context())
			if !ok {
				var err error
				data, err = swaputil.FetchContext(r, cfg)
				if err != nil {
					next.ServeHTTP(w, r)
					return
				}
			}
			if strings.TrimSpace(data.ModelID) == "" {
				next.ServeHTTP(w, r)
				return
			}
			adapter := adapters[data.ModelID]
			if backend.IsNilAdapter(adapter) {
				next.ServeHTTP(w, r)
				return
			}

			body, hadBody, err := readBackendTransformRequestBody(r)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, err.Error())
				return
			}
			streaming := backendRequestStreams(r, body)
			request := backend.RequestTransform{Body: body, Header: cloneHeader(r.Header)}
			transformed, err := adapter.TransformRequest(r.Context(), r.URL.Path, request)
			if err != nil {
				status := http.StatusBadGateway
				if errors.Is(err, backend.ErrUnsupported) {
					status = http.StatusNotImplemented
				}
				swaputil.SendResponse(w, r, status, fmt.Sprintf("backend request transform: %v", err))
				return
			}
			if transformed.Header != nil {
				r.Header = cloneHeader(transformed.Header)
			}
			if hadBody || transformed.Body != nil {
				setBackendRequestBody(r, transformed.Body)
			}

			// Response transformations are whole-body operations. Keep the raw
			// transport for SSE and explicit stream requests, where buffering would
			// violate the protocol's latency and cancellation guarantees.
			if streaming || backendRequestStreams(r, transformed.Body) {
				next.ServeHTTP(w, r)
				return
			}
			buffer := newBackendTransformResponseWriter(w)
			next.ServeHTTP(buffer, r)
			if buffer.committed {
				return
			}
			if err := buffer.finish(r, adapter); err != nil {
				if !swaputil.ResponseStarted(w) {
					swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
				}
			}
		})
	}
}

func readBackendTransformRequestBody(r *http.Request) ([]byte, bool, error) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, false, nil
	}
	limited := io.LimitReader(r.Body, backendTransformBodyLimit+1)
	body, err := io.ReadAll(limited)
	if err != nil {
		return nil, true, fmt.Errorf("could not read backend request body: %w", err)
	}
	if len(body) > backendTransformBodyLimit {
		return nil, true, fmt.Errorf("backend request body exceeds %d bytes", backendTransformBodyLimit)
	}
	_ = r.Body.Close()
	return body, true, nil
}

func setBackendRequestBody(r *http.Request, body []byte) {
	if r == nil {
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Del("Transfer-Encoding")
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
}

func cloneHeader(header http.Header) http.Header {
	if header == nil {
		return make(http.Header)
	}
	return header.Clone()
}

func backendRequestStreams(r *http.Request, body []byte) bool {
	if r == nil {
		return true
	}
	if strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("stream")), "true") {
		return true
	}
	for _, value := range r.Header.Values("Accept") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(strings.SplitN(token, ";", 2)[0]), "text/event-stream") {
				return true
			}
		}
	}
	if len(body) == 0 || !json.Valid(body) {
		return false
	}
	var request struct {
		Stream bool `json:"stream"`
	}
	return json.Unmarshal(body, &request) == nil && request.Stream
}

type backendTransformResponseWriter struct {
	parent      http.ResponseWriter
	header      http.Header
	body        bytes.Buffer
	status      int
	committed   bool
	passthrough bool
}

func newBackendTransformResponseWriter(parent http.ResponseWriter) *backendTransformResponseWriter {
	header := make(http.Header)
	if parent != nil {
		header = parent.Header().Clone()
	}
	return &backendTransformResponseWriter{parent: parent, header: header}
}

func (w *backendTransformResponseWriter) Header() http.Header { return w.header }

func (w *backendTransformResponseWriter) WriteHeader(status int) {
	if w.status != 0 || w.committed {
		return
	}
	w.status = status
}

func (w *backendTransformResponseWriter) Write(data []byte) (int, error) {
	if w.committed {
		return w.parent.Write(data)
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if w.passthrough {
		return w.parent.Write(data)
	}
	if w.body.Len()+len(data) > backendTransformBodyLimit {
		if err := w.commitRaw(); err != nil {
			return 0, err
		}
		return w.parent.Write(data)
	}
	return w.body.Write(data)
}

// Flush is an explicit signal that the downstream handler needs streaming
// semantics. Commit what has been buffered and stop applying a whole-body
// transform from this point onward.
func (w *backendTransformResponseWriter) Flush() {
	if w.committed {
		if flusher, ok := w.parent.(http.Flusher); ok {
			flusher.Flush()
		}
		return
	}
	w.passthrough = true
	_ = w.commitRaw()
	if flusher, ok := w.parent.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *backendTransformResponseWriter) commitRaw() error {
	if w.committed {
		return nil
	}
	w.copyHeadersToParent()
	status := w.status
	if status == 0 {
		status = http.StatusOK
	}
	w.parent.WriteHeader(status)
	if w.body.Len() > 0 {
		if _, err := w.parent.Write(w.body.Bytes()); err != nil {
			return err
		}
	}
	w.committed = true
	return nil
}

func (w *backendTransformResponseWriter) finish(r *http.Request, adapter backend.BackendAdapter) error {
	if w.committed {
		return nil
	}
	if w.status == 0 {
		w.status = http.StatusOK
	}
	body := append([]byte(nil), w.body.Bytes()...)
	transformed := body
	changed := false
	if w.status >= 200 && w.status < 300 && isJSONResponse(w.header) {
		value, err := adapter.TransformResponse(r.Context(), r.URL.Path, body, w.header.Clone())
		if err != nil {
			return fmt.Errorf("backend response transform: %w", err)
		}
		transformed = value
		changed = !bytes.Equal(value, body)
	}
	if changed {
		clearBackendTransformedBodyHeaders(w.header)
	}
	w.copyHeadersToParent()
	w.parent.WriteHeader(w.status)
	if len(transformed) > 0 {
		if _, err := w.parent.Write(transformed); err != nil {
			return err
		}
	}
	w.committed = true
	return nil
}

func isJSONResponse(header http.Header) bool {
	contentType := strings.ToLower(strings.TrimSpace(strings.SplitN(header.Get("Content-Type"), ";", 2)[0]))
	return contentType == "" || contentType == "application/json" || strings.HasSuffix(contentType, "+json")
}

func (w *backendTransformResponseWriter) copyHeadersToParent() {
	if w.parent == nil {
		return
	}
	target := w.parent.Header()
	for key := range target {
		delete(target, key)
	}
	for key, values := range w.header {
		target[key] = append([]string(nil), values...)
	}
}

func clearBackendTransformedBodyHeaders(header http.Header) {
	for _, key := range []string{"Content-Length", "Content-Encoding", "Content-Range", "Transfer-Encoding", "Trailer"} {
		header.Del(key)
	}
}
