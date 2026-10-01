package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const maxAffinityResponseBytes = 4 * 1024 * 1024
const affinityWriteTimeout = 5 * time.Second

// affinityWriteContext deliberately outlives the client request. A caller can
// disconnect immediately after receiving the final response bytes, which
// cancels r.Context() before the recorder has finished persisting the response
// chain. Affinity is durable control-plane state, so a short bounded background
// transaction keeps GET/previous_response_id usable without allowing a stuck
// database write to outlive the server indefinitely.
func affinityWriteContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), affinityWriteTimeout)
}

// CreateResponseAffinityMiddleware records native Responses ids without
// buffering the response seen by the client. The chat adapter owns its own
// recorder, so this middleware skips that protocol to avoid duplicate rows.
func CreateResponseAffinityMiddleware(cfg config.Config, st *store.Store) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if st == nil || r.Method != http.MethodPost || (r.URL.Path != "/v1/responses" && r.URL.Path != "/v/responses") {
				next.ServeHTTP(w, r)
				return
			}
			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				next.ServeHTTP(w, r)
				return
			}
			modelConfig, ok := cfg.Models[data.ModelID]
			if ok && modelConfig.Backend.EffectiveProtocol() == config.AdapterProtocolResponsesToChat {
				next.ServeHTTP(w, r)
				return
			}
			var request map[string]any
			requestBody, inspectable := readRequestBodyForAffinity(r)
			// A chunked or oversized request cannot be safely buffered for the
			// store flag.  Preserve the original stream, but fail closed for
			// affinity persistence: without seeing store:true we must not create
			// retrievable state for a request that may have asked for store:false.
			if !inspectable {
				next.ServeHTTP(w, r)
				return
			}
			if json.NewDecoder(bytes.NewReader(requestBody)).Decode(&request) == nil {
				if stored, ok := request["store"].(bool); ok && !stored {
					next.ServeHTTP(w, r)
					return
				}
			}
			recorder := &affinityResponseWriter{ResponseWriter: w, body: bytes.NewBuffer(nil)}
			next.ServeHTTP(recorder, r)
			// A response larger than the affinity bound is still forwarded in full,
			// but its in-memory copy is intentionally incomplete. Never parse that
			// prefix as a canonical response: a truncated SSE stream could retain an
			// id and accidentally create a seemingly valid, restart-visible chain.
			if !recorder.truncated {
				persistNativeResponseAffinity(r.Context(), st, data.ModelID, modelConfig.Backend.Type, recorder.body.Bytes(), recorder.Header(), recorder.status)
			}
		})
	}
}

// readRequestBodyForAffinity peeks at a bounded request while preserving the
// complete stream for the downstream native Responses handler. The boolean is
// false when the body was not safely inspectable (for example a known body
// larger than the bound or a read error); callers must then avoid persisting
// affinity because store:false cannot be ruled out.
func readRequestBodyForAffinity(r *http.Request) ([]byte, bool) {
	if r == nil || r.Body == nil || r.Body == http.NoBody {
		return nil, true
	}
	// Affinity only peeks at the request to honor store:false. Do not consume
	// an oversized known body just to inspect that flag: the downstream native
	// Responses handler must still receive the original stream. Unknown-length
	// bodies are read up to a hard bound and then replayed below, so small
	// chunked requests still get correct store:false semantics.
	if r.ContentLength > backendTransformBodyLimit {
		return nil, false
	}
	original := r.Body
	body, err := io.ReadAll(io.LimitReader(original, backendTransformBodyLimit+1))
	if err != nil {
		replayRequestBody(r, body, original)
		return nil, false
	}
	if len(body) > backendTransformBodyLimit {
		replayRequestBody(r, body, original)
		return nil, false
	}
	_ = original.Close()
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	return body, true
}

func replayRequestBody(r *http.Request, consumed []byte, original io.ReadCloser) {
	if r == nil {
		return
	}
	// The bounded reader may have consumed only a prefix. MultiReader puts that
	// prefix back before the unread original stream, while the close wrapper
	// still closes the original transport body when net/http is done.
	r.Body = &replayReadCloser{Reader: io.MultiReader(bytes.NewReader(consumed), original), closer: original}
}

type replayReadCloser struct {
	io.Reader
	closer io.Closer
	once   sync.Once
	err    error
}

func (r *replayReadCloser) Close() error {
	r.once.Do(func() {
		if r.closer != nil {
			r.err = r.closer.Close()
		}
	})
	return r.err
}

func persistNativeResponseAffinity(_ context.Context, st *store.Store, model, backend string, body []byte, headers http.Header, status int) {
	// Native Responses ids are useful for routing subsequent GET/cancel calls
	// even when the initial request failed (for example a provider may return a
	// response.failed SSE lifecycle with HTTP 200, or an error response with a
	// stable id and HTTP 5xx). Keep the bounded canonical body and mark its
	// state below instead of dropping every non-2xx response.
	if st == nil || len(body) == 0 || len(body) > maxAffinityResponseBytes {
		return
	}
	contentType := strings.ToLower(headers.Get("Content-Type"))
	var response map[string]any
	if strings.Contains(contentType, "text/event-stream") {
		response = responseFromSSE(body, model)
	} else if json.Unmarshal(body, &response) != nil {
		return
	}
	id, _ := response["id"].(string)
	if strings.TrimSpace(id) == "" {
		return
	}
	canonical, err := json.Marshal(response)
	if err != nil {
		return
	}
	state := responseAffinityState(response, status)
	now := time.Now()
	ctx, cancel := affinityWriteContext()
	defer cancel()
	_ = st.UpsertResponseAffinity(ctx, store.ResponseAffinity{ResponseID: id, Model: model, Backend: backend, Status: state, Response: canonical, ExpiresAt: now.Add(30 * 24 * time.Hour), CreatedAt: now, UpdatedAt: now})
}

// responseAffinityState prefers the protocol-level Responses status when one
// is present, falling back to the HTTP status for native error envelopes.
// Keeping this normalization in one place makes native and Chat-adapted
// persistence agree on completed/failed/cancelled/incomplete states.
func responseAffinityState(response map[string]any, httpStatus int) string {
	if response != nil {
		if state, ok := response["status"].(string); ok && strings.TrimSpace(state) != "" {
			return strings.TrimSpace(state)
		}
	}
	if httpStatus >= 200 && httpStatus < 300 {
		return "completed"
	}
	return "failed"
}

type affinityResponseWriter struct {
	http.ResponseWriter
	body      *bytes.Buffer
	status    int
	truncated bool
}

func (w *affinityResponseWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (w *affinityResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if w.body.Len() < maxAffinityResponseBytes {
		remaining := maxAffinityResponseBytes - w.body.Len()
		if len(data) <= remaining {
			_, _ = w.body.Write(data)
		} else if remaining > 0 {
			_, _ = w.body.Write(data[:remaining])
			w.truncated = true
		} else {
			w.truncated = true
		}
	} else if len(data) > 0 {
		w.truncated = true
	}
	return w.ResponseWriter.Write(data)
}

func (w *affinityResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
