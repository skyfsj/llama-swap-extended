package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// The debug chat runs one conversation request through the real inference
// pipeline with a single extension forced into the chain. The pipeline's hook
// invocations record their activity into an extensionDebugCapture so the editor
// can show what the extension did to each turn without running it twice.

type extensionDebugKey struct{}

type extensionDebugSession struct {
	item    *extensions.Compiled
	capture *extensionDebugCapture
}

func debugSessionFrom(ctx context.Context) *extensionDebugSession {
	session, _ := ctx.Value(extensionDebugKey{}).(*extensionDebugSession)
	return session
}

func debugCaptureFrom(ctx context.Context) *extensionDebugCapture {
	if session := debugSessionFrom(ctx); session != nil {
		return session.capture
	}
	return nil
}

// extensionDebugCapture accumulates one debug request's extension activity.
// Steps are capped: a runaway extension must not grow the buffer without
// bound, and the capture only lives for debugArtifactTTL anyway.
type extensionDebugCapture struct {
	mu        sync.Mutex
	extID     string
	steps     []map[string]any
	truncated bool
	done      atomic.Bool
}

const (
	debugCaptureMaxSteps    = 200
	debugCaptureMaxLogLines = 100
	debugArtifactTTL        = 2 * time.Minute
)

func (c *extensionDebugCapture) record(step map[string]any) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.steps) >= debugCaptureMaxSteps {
		c.truncated = true
		return
	}
	c.steps = append(c.steps, step)
}

func (c *extensionDebugCapture) finish() {
	if c != nil {
		c.done.Store(true)
	}
}

// recordHook stores one hook invocation. Map-shaped inputs are reduced to the
// top-level keys that actually changed so a long conversation does not resend
// the whole history for every step.
func (c *extensionDebugCapture) recordHook(extensionID, hook string, duration time.Duration, input, output map[string]any, logs []string, hookErr error) {
	if c == nil || hook == "onStreamEvent" {
		return
	}
	step := map[string]any{"kind": "hook", "hook": hook, "extension": extensionID, "durationMs": float64(duration) / float64(time.Millisecond)}
	if len(logs) > 0 {
		if len(logs) > debugCaptureMaxLogLines {
			logs = append(append([]string{}, logs[:debugCaptureMaxLogLines]...), "…")
		}
		step["logs"] = logs
	}
	if hookErr != nil {
		step["error"] = hookErr.Error()
	}
	switch hook {
	case "onRequest", "onBeforeForward", "onResponse":
		step["changed"] = shallowExtensionDiff(input, output)
	case "onToolCall", "onToolResult":
		step["input"] = input
		if output != nil {
			step["output"] = output
		}
	default:
		return
	}
	c.record(step)
}

// recordInjectedTools stores the tool definitions the pipeline added for the
// debugged extension, so the chat can show what the model actually saw.
func (c *extensionDebugCapture) recordInjectedTools(injected []string) {
	if c == nil || len(injected) == 0 {
		return
	}
	c.record(map[string]any{"kind": "tools", "injected": injected})
}

func shallowExtensionDiff(before, after map[string]any) []map[string]any {
	changed := []map[string]any{}
	for key, afterValue := range after {
		beforeValue, existed := before[key]
		beforeJSON, _ := json.Marshal(beforeValue)
		afterJSON, _ := json.Marshal(afterValue)
		if existed && string(beforeJSON) == string(afterJSON) {
			continue
		}
		changed = append(changed, map[string]any{"key": key, "before": beforeValue, "after": afterValue})
	}
	return changed
}

// handleAPIExtensionDebugChat forwards one chat.completions request through the
// normal inference chain with only the named extension active. The chain is
// invoked handler-to-handler, so streaming responses flow straight back to the
// caller; the debug request id needed to fetch the captured activity travels
// in a response header set before the first byte is written.
func (s *Server) handleAPIExtensionDebugChat(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	item, ok := m.Compiled(r.PathValue("id"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "extension not found")
		return
	}
	var payload struct {
		Request map[string]any `json:"request"`
		// Files carries the editor draft so multi-turn debugging runs the
		// unsaved code live; empty means the stored definition.
		Files map[string]string `json:"files,omitempty"`
	}
	if err := decodeJSONBody(w, r, &payload, swaputil.MaxRequestBodySize); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if len(payload.Files) > 0 {
		draft, err := s.extensionUnderTest(r, m, r.PathValue("id"), payload.Files)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
			return
		}
		item = draft
	}
	if _, ok := payload.Request["model"].(string); !ok || strings.TrimSpace(payload.Request["model"].(string)) == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "request.model is required")
		return
	}
	if s.modelChainHandler == nil {
		swaputil.SendResponse(w, r, http.StatusConflict, "inference pipeline is unavailable")
		return
	}

	var token [16]byte
	if _, err := rand.Read(token[:]); err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "cannot allocate debug request id")
		return
	}
	requestID := hex.EncodeToString(token[:])
	capture := &extensionDebugCapture{extID: item.Manifest.ID}
	s.debugCaptures.Store(requestID, capture)
	time.AfterFunc(debugArtifactTTL, func() { s.debugCaptures.Delete(requestID) })

	body, err := json.Marshal(payload.Request)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	inner, err := http.NewRequestWithContext(
		contextWithDebugSession(r.Context(), item, capture),
		http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body)),
	)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	inner.Header.Set("Content-Type", "application/json")
	inner.Header.Set("X-Request-ID", requestID)
	// The inner chain re-authenticates the caller. The management panel key
	// carries the inference scope, and keyless deployments pass through; a
	// config-admin key without inference scope surfaces the inner 403 here.
	inner.Header.Set("Authorization", r.Header.Get("Authorization"))
	if cookie := r.Header.Get("Cookie"); cookie != "" {
		inner.Header.Set("Cookie", cookie)
	}

	w.Header().Set("X-Extension-Debug-ID", requestID)
	s.modelChainHandler.ServeHTTP(w, inner)
	capture.finish()
}

func contextWithDebugSession(ctx context.Context, item *extensions.Compiled, capture *extensionDebugCapture) context.Context {
	return context.WithValue(ctx, extensionDebugKey{}, &extensionDebugSession{item: item, capture: capture})
}

// handleAPIExtensionDebugArtifact answers with the extension activity captured
// for one debug chat request. The capture is reaped debugArtifactTTL after the
// request started, so a client that stops mid-stream cannot leak buffers.
func (s *Server) handleAPIExtensionDebugArtifact(w http.ResponseWriter, r *http.Request) {
	requestID := r.URL.Query().Get("request")
	if requestID == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "request query parameter is required")
		return
	}
	value, ok := s.debugCaptures.Load(requestID)
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "debug request not found or expired")
		return
	}
	capture, ok := value.(*extensionDebugCapture)
	if !ok || capture.extID != r.PathValue("id") {
		swaputil.SendResponse(w, r, http.StatusNotFound, "debug request not found or expired")
		return
	}
	capture.mu.Lock()
	steps := append([]map[string]any{}, capture.steps...)
	truncated := capture.truncated
	capture.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"steps": steps, "truncated": truncated, "done": capture.done.Load()})
}
