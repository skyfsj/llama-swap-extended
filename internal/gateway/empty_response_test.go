package gateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// emptyResponseConfig builds a two-provider pool serving one bare model, the
// shape in which an empty answer from the first provider must continue on the
// second.
func emptyResponseConfig(t *testing.T, firstURL, secondURL string, retry *bool, hold string) config.Config {
	t.Helper()
	cfg := config.Config{
		Gateway: config.GatewayConfig{
			EmptyResponse: config.GatewayEmptyResponseConfig{Retry: retry, Hold: hold},
		},
		Providers: config.ProviderDictionaryConfig{
			"first": {
				Endpoint: firstURL,
				ApiKeys:  []config.ProviderKey{{Value: "first-key"}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "first-upstream"}},
			},
			"second": {
				Endpoint: secondURL,
				ApiKeys:  []config.ProviderKey{{Value: "second-key"}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "second-upstream"}},
			},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	return cfg
}

// A stream that carries lifecycle frames but never a token is not an answer.
// The request must continue on the next provider instead of committing an
// empty success to the client.
func TestGateway_EmptyStreamRetriesOnNextProvider(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, frame := range []string{
			`data: {"type":"response.created","response":{"id":"r1"}}`,
			`data: {"type":"response.output_item.added","item":{"type":"message","content":[]}}`,
			`data: {"type":"response.completed","response":{"output":[]}}`,
		} {
			_, _ = w.Write([]byte(frame + "\n\n"))
		}
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"pong\"}\n\n"))
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"pong\"}]}]}}\n\n"))
	}))
	defer second.Close()

	g, err := New(emptyResponseConfig(t, first.URL, second.URL, nil, "2s"), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	body := `{"model":"shared","input":"hi","stream":true}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, g.PlanRoutes("shared", false, "openai-responses", Session{}), Session{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("client did not receive the second provider's answer: %s", w.Body.String())
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 1 {
		t.Fatalf("calls first=%d second=%d, want one each", firstCalls.Load(), secondCalls.Load())
	}
}

func TestGateway_EmptyExhaustionIsFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"empty","choices":[]}`))
	}))
	defer upstream.Close()
	g, err := New(emptyResponseConfig(t, upstream.URL, upstream.URL, nil, "2s"), testLogger())
	if err != nil {
		t.Fatal(err)
	}
	defer g.Shutdown()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, g.PlanRoutes("shared", false, "openai", Session{}), Session{})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("empty success was reported as successful: status=%d body=%s", w.Code, w.Body.String())
	}
}

// A non-streaming 200 whose JSON carries no model output is likewise retried.
func TestGateway_EmptySuccessBodyWithNoContentRetries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c2","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2 (the empty answer retried)", calls.Load())
	}
}

// A tool-only answer is real output: it must be committed, never retried.
func TestGateway_ToolCallOnlyAnswerIsCommitted(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"t","type":"function","function":{"name":"f","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1: a tool call is a real answer", calls.Load())
	}
	var decoded map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("client body = %s", w.Body.String())
	}
}

// Disabling the rule restores the previous behaviour: the empty answer is
// committed as-is.
func TestGateway_EmptyResponseRetryCanBeDisabled(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	retry := false
	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	cfg.Gateway.EmptyResponse.Retry = &retry
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 when the retry is disabled", calls.Load())
	}
}

// An unparsable body is not evidence of an empty answer: the probe fails open
// so an unknown success shape is never retried.
func TestGateway_UnrecognizedSuccessBodyIsForwarded(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"result":"some other protocol"}`))
	}))
	defer upstream.Close()

	cfg := gwConfig(t, upstream.URL, config.ProviderTypeOpenAI)
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	route := g.RouteFor("prov/m1", false)
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"remote-m1"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTP(w, req, route, Session{})

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "some other protocol") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (unrecognized bodies are forwarded)", calls.Load())
	}
}

// responsesEmptyConfig is a single Responses-dialect provider serving one bare
// model. The dialect matters: the probe classifies a body by the surface the
// route speaks, so a Chat-typed provider would never inspect a Responses body.
func responsesEmptyConfig(t *testing.T, upstreamURL, hold string) config.Config {
	t.Helper()
	retry := true
	cfg := config.Config{
		Gateway: config.GatewayConfig{
			EmptyResponse: config.GatewayEmptyResponseConfig{Retry: &retry, Hold: hold},
		},
		Providers: config.ProviderDictionaryConfig{
			"p": {
				Type:     config.ProviderTypeOpenAIResponses,
				Endpoint: upstreamURL,
				ApiKeys:  []config.ProviderKey{{Value: "k"}},
				Models:   []config.ProviderModel{{Local: "shared", Remote: "upstream"}},
			},
		},
	}
	for id, p := range cfg.Providers {
		cfg.Providers[id] = normalizeProvider(t, p)
	}
	return cfg
}

// firstWriteRecorder reports when the gateway committed the response head,
// independently of how long the upstream keeps the body open afterwards.
type firstWriteRecorder struct {
	*httptest.ResponseRecorder
	once  sync.Once
	first chan struct{}
}

func newFirstWriteRecorder() *firstWriteRecorder {
	return &firstWriteRecorder{ResponseRecorder: httptest.NewRecorder(), first: make(chan struct{})}
}

func (w *firstWriteRecorder) mark() { w.once.Do(func() { close(w.first) }) }
func (w *firstWriteRecorder) WriteHeader(status int) {
	w.ResponseRecorder.WriteHeader(status)
	w.mark()
}
func (w *firstWriteRecorder) Write(data []byte) (int, error) {
	w.mark()
	return w.ResponseRecorder.Write(data)
}

// A provider-owned background submit answers with a lifecycle status and no
// output at all. That is a complete answer to the submit, not an empty one:
// holding it would stall the caller for the whole first-token grace period, and
// retrying it would start a second job upstream. The upstream keeps the body
// open after the acknowledgement, so only the commit decision is measured.
func TestGateway_BackgroundAcknowledgementIsForwardedAtOnce(t *testing.T) {
	var calls atomic.Int32
	release := make(chan struct{})
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(release) }) }
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		// Flushing without a length keeps the body chunked, which is the
		// streaming path where the probe may otherwise wait for a first token.
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_job_1","object":"response","status":"in_progress","output":[]}`))
		w.(http.Flusher).Flush()
		<-release
	}))
	defer upstream.Close()

	g, err := New(responsesEmptyConfig(t, upstream.URL, "3s"), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()
	// Registered last so it runs before Shutdown: a failed assertion must not
	// leave the in-flight upstream body open while the gateway drains.
	defer unblock()

	w := newFirstWriteRecorder()
	done := make(chan struct{})
	go func() {
		defer close(done)
		body := `{"model":"shared","input":"hi","background":true}`
		req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		g.ServeHTTPWithFailover(w, req, g.PlanRoutes("shared", false, "openai-responses", Session{}), Session{})
	}()

	select {
	case <-w.first:
	case <-time.After(time.Second):
		t.Fatal("the acknowledgement was not committed at once; it waited for content")
	}
	unblock()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the forwarded response never finished")
	}
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "in_progress") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (an acknowledgement is not retried)", calls.Load())
	}
}

// The same lifecycle shape with a terminal status still means "answered
// nothing", so the empty-response retry keeps applying to synchronous traffic.
func TestGateway_CompletedEmptyResponsesBodyStillRetries(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_1","object":"response","status":"completed","output":[]}`))
		w.(http.Flusher).Flush()
	}))
	defer upstream.Close()

	g, err := New(responsesEmptyConfig(t, upstream.URL, "5s"), testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(`{"model":"shared","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, g.PlanRoutes("shared", false, "openai-responses", Session{}), Session{})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("empty terminal answer was reported as successful: status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (the only provider cannot be retried past)", calls.Load())
	}
}

// An upstream that declares text/event-stream but answers one request with a
// complete JSON body is answering, not failing: the body carries no SSE frame
// at all, so "no content" cannot be told apart from "not a stream", and the
// answer must reach the client instead of being retried as empty.
func TestGateway_SSEContentTypeWithJSONAnswerIsForwarded(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`))
	}))
	defer upstream.Close()

	retry := true
	cfg := emptyResponseConfig(t, upstream.URL, upstream.URL, &retry, "2s")
	g, err := New(cfg, testLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer g.Shutdown()

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"shared","messages":[{"role":"user","content":"hi"}],"stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.ServeHTTPWithFailover(w, req, g.PlanRoutes("shared", false, "gemini", Session{}), Session{})

	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "pong") {
		t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("calls = %d, want 1 (a JSON answer is not an empty stream)", calls.Load())
	}
}
