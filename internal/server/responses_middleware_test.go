package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestResponseFromSSEKeepsCompletedOutputAndUsage(t *testing.T) {
	body := []byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"lookup\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":2}}}\n\n")
	response := responseFromSSE(body, "fallback")
	if response["id"] != "resp_1" || response["model"] != "m" {
		t.Fatalf("response identity = %#v", response)
	}
	if _, ok := response["output"]; !ok {
		t.Fatalf("completed output missing: %#v", response)
	}
	if _, ok := response["usage"]; !ok {
		t.Fatalf("completed usage missing: %#v", response)
	}
}

func TestResponseFromSSEKeepsIncompleteTerminalState(t *testing.T) {
	body := []byte("event: response.incomplete\ndata: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_incomplete\",\"model\":\"m\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[]}}\n\n")
	response := responseFromSSE(body, "fallback")
	if response["id"] != "resp_incomplete" || response["status"] != "incomplete" {
		t.Fatalf("incomplete response identity/status = %#v", response)
	}
	details, ok := response["incomplete_details"].(map[string]any)
	if !ok || details["reason"] != "max_output_tokens" {
		t.Fatalf("incomplete details = %#v", response["incomplete_details"])
	}
}

func TestResponseFromSSEReconstructsItemsWithoutCompletedEvent(t *testing.T) {
	body := []byte(`event: response.created
data: {"type":"response.created","response":{"id":"resp_legacy","model":"m","status":"in_progress"}}

event: response.output_item.done
data: {"type":"response.output_item.done","response_id":"resp_legacy","output_index":0,"item":{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"think"}]}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","response_id":"resp_legacy","output_index":1,"item_id":"fc_1","call_id":"call_1","name":"lookup","delta":"{\"q\":1}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","response_id":"resp_legacy","output_index":1,"item_id":"fc_1","call_id":"call_1","name":"lookup","arguments":"{\"q\":1}"}

data: {"type":"response.usage","usage":{"input_tokens":3,"output_tokens":2}}

data: [DONE]

`)
	response := responseFromSSE(body, "fallback")
	if response["id"] != "resp_legacy" || response["model"] != "m" {
		t.Fatalf("response identity = %#v", response)
	}
	output, ok := response["output"].([]any)
	if !ok || len(output) != 2 {
		t.Fatalf("reconstructed output = %#v", response["output"])
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("reasoning item = %#v", output[0])
	}
	tool := output[1].(map[string]any)
	if tool["type"] != "function_call" || tool["call_id"] != "call_1" || tool["name"] != "lookup" || tool["arguments"] != `{"q":1}` {
		t.Fatalf("tool item = %#v", tool)
	}
	usage, ok := response["usage"].(map[string]any)
	if !ok || usage["input_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", response["usage"])
	}
}

func TestResponseAffinityNativeGetForwardsUpstream(t *testing.T) {
	local := newStubRouter([]string{"m"}, `{"id":"resp_1","model":"m","status":"completed"}`)
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}}}}
	if err := s.store.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp_1", Model: "m", Backend: "vllm", Status: "completed",
		Response:  []byte(`{"id":"resp_1","model":"m","status":"completed","output":[{"type":"message","content":"stale"}]}`),
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_1", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body=%s", w.Code, w.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response["output"] != nil {
		t.Fatalf("native GET served stored chat chain: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"id":"resp_1"`) {
		t.Fatalf("upstream response missing: %s", w.Body.String())
	}
}

func TestResponseAffinityChatGetServesStoredChain(t *testing.T) {
	local := newStubRouter([]string{"m"}, `{"id":"wrong","model":"m"}`)
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}}}}
	if err := s.store.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp_2", Model: "m", Backend: "vllm", Status: "completed",
		Response:  []byte(`{"id":"resp_2","model":"m","status":"completed","output":[{"type":"message","content":"stored"}]}`),
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/responses/resp_2", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"stored"`) {
		t.Fatalf("chat chain response = %d %s", w.Code, w.Body.String())
	}
}

func TestClearTransformedBodyHeaders(t *testing.T) {
	header := make(http.Header)
	for _, key := range []string{"Content-Length", "Content-Encoding", "Content-Range", "Transfer-Encoding", "Trailer"} {
		header.Set(key, "stale")
	}
	header.Set("Content-Type", "application/json")
	clearTransformedBodyHeaders(header)
	for _, key := range []string{"Content-Length", "Content-Encoding", "Content-Range", "Transfer-Encoding", "Trailer"} {
		if got := header.Get(key); got != "" {
			t.Fatalf("%s = %q, want cleared", key, got)
		}
	}
	if got := header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want preserved", got)
	}
}

func TestResponsesAdapterStreamsChatSSEAndPersistsAffinity(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = w.Write([]byte(`data: {"id":"chat-1","model":"m","choices":[{"delta":{"content":"he","reasoning_content":"think","tool_calls":[{"index":0,"id":"call-1","function":{"name":"lookup","arguments":"{\"q\":"}}]}}]}` + "\n\n"))
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"llo","tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}],"usage":{"prompt_tokens":2,"completion_tokens":3}}` + "\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})
	handler := CreateResponsesAdapterMiddleware(cfg, st)(next)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "response.output_text.delta") || !strings.Contains(w.Body.String(), "response.completed") {
		t.Fatalf("stream response = %d %s", w.Code, w.Body.String())
	}
	if got := w.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("content type = %q", got)
	}
	affinity, found, err := st.GetResponseAffinity(context.Background(), "resp_chat-1", time.Now())
	if err != nil || !found {
		t.Fatalf("affinity found=%v err=%v", found, err)
	}
	if !strings.Contains(string(affinity.Response), `"text":"hello"`) {
		t.Fatalf("stored affinity = %s", affinity.Response)
	}
	for _, field := range []string{`"type":"reasoning"`, `"type":"function_call"`, `"call_id":"call-1"`, `"input_tokens":2`, `"output_tokens":3`} {
		if !strings.Contains(string(affinity.Response), field) {
			t.Fatalf("stored canonical affinity missing %s: %s", field, affinity.Response)
		}
	}
}

func TestServer_ResponsesChatAdapterEndToEnd(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	var upstreamBody []byte
	var upstreamPath string
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		upstreamPath = r.URL.Path
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-e2e","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	s.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","instructions":"be concise","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("end-to-end status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("end-to-end content type=%q", got)
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Responses response: %v", err)
	}
	if response["id"] != "resp_chat-e2e" || response["model"] != "m" || response["status"] != "completed" {
		t.Fatalf("converted response=%#v", response)
	}
	usage, ok := response["usage"].(map[string]any)
	if !ok || usage["input_tokens"] != float64(2) || usage["output_tokens"] != float64(1) {
		t.Fatalf("converted usage=%#v", response["usage"])
	}
	if !strings.Contains(string(upstreamBody), `"messages"`) || strings.Contains(string(upstreamBody), `"instructions"`) {
		t.Fatalf("upstream request was not converted: %s", upstreamBody)
	}
	if upstreamPath != "/v1/chat/completions" {
		t.Fatalf("upstream path=%q, want /v1/chat/completions", upstreamPath)
	}
	if !strings.Contains(string(upstreamBody), `"content":"hi"`) {
		t.Fatalf("converted input missing from upstream request: %s", upstreamBody)
	}
	if _, found, err := s.store.GetResponseAffinity(context.Background(), "resp_chat-e2e", time.Now()); err != nil || !found {
		t.Fatalf("end-to-end affinity found=%v err=%v", found, err)
	}
	_ = s.Shutdown(time.Second)
}

func TestRewriteResponsesChatBackendPathRestoresPublicPath(t *testing.T) {
	for _, test := range []struct {
		publicPath string
		chatPath   string
	}{
		{publicPath: "/v1/responses", chatPath: "/v1/chat/completions"},
		{publicPath: "/v/responses", chatPath: "/v/chat/completions"},
	} {
		req := httptest.NewRequest(http.MethodPost, test.publicPath+"?trace=1", strings.NewReader(`{"model":"m"}`))
		originalURI := req.RequestURI
		req = markResponsesChatRequest(req)
		restore := rewriteResponsesChatBackendPath(req)
		if req.URL.Path != test.chatPath {
			t.Fatalf("public %s backend path=%q, want %q", test.publicPath, req.URL.Path, test.chatPath)
		}
		if req.RequestURI != test.chatPath+"?trace=1" {
			t.Fatalf("public %s backend request URI=%q, want %q", test.publicPath, req.RequestURI, test.chatPath+"?trace=1")
		}
		restore()
		if req.URL.Path != test.publicPath || req.RequestURI != originalURI {
			t.Fatalf("public request not restored: path=%q uri=%q", req.URL.Path, req.RequestURI)
		}
	}
}

func TestServer_ResponsesChatAdapterStreamingEndToEnd(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-stream\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"hel\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"lo\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	s.routes()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream end-to-end status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, marker := range []string{"response.created", "response.output_text.delta", "response.completed", `"text":"hello"`, `"input_tokens":4`, `"output_tokens":2`} {
		if !strings.Contains(body, marker) {
			t.Fatalf("stream output missing %q: %s", marker, body)
		}
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("stream content type=%q", got)
	}
	affinity, found, err := s.store.GetResponseAffinity(context.Background(), "resp_chat-stream", time.Now())
	if err != nil || !found {
		t.Fatalf("stream affinity found=%v err=%v", found, err)
	}
	if !strings.Contains(string(affinity.Response), `"text":"hello"`) || !strings.Contains(string(affinity.Response), `"input_tokens":4`) {
		t.Fatalf("stream stored response=%s", affinity.Response)
	}
	_ = s.Shutdown(time.Second)
}

func TestServer_NativeResponsesAffinityGetCancelEndToEnd(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	var postIDs []string
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/v1/responses" {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatalf("read native request: %v", err)
			}
			var request map[string]any
			if err := json.Unmarshal(body, &request); err != nil {
				t.Fatalf("decode native request: %v", err)
			}
			id := "native-store"
			if stored, ok := request["store"].(bool); ok && !stored {
				id = "native-no-store"
			}
			postIDs = append(postIDs, id)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fmt.Sprintf(`{"id":%q,"object":"response","model":"m","status":"completed","output":[]}`, id))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/responses/native-store":
			if got := r.URL.Query().Get("model"); got != "m" {
				t.Errorf("native GET model query = %q, want m", got)
			}
			_, _ = io.WriteString(w, `{"id":"native-store","object":"response","model":"m","status":"completed","output":[]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses/native-store/cancel":
			if got := r.URL.Query().Get("model"); got != "m" {
				t.Errorf("native cancel model query = %q, want m", got)
			}
			_, _ = io.WriteString(w, `{"id":"native-store","object":"response","model":"m","status":"cancelled","output":[]}`)
		default:
			http.Error(w, "unexpected native route", http.StatusNotFound)
		}
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}},
	}}
	s.routes()

	post := func(storeValue bool) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"model":"m","input":"hello","store":%t}`, storeValue)
		recorder := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(recorder, req)
		return recorder
	}
	if recorder := post(true); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"native-store"`) {
		t.Fatalf("native POST = %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := post(false); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"native-no-store"`) {
		t.Fatalf("native store:false POST = %d %s", recorder.Code, recorder.Body.String())
	}
	if len(postIDs) != 2 || postIDs[0] != "native-store" || postIDs[1] != "native-no-store" {
		t.Fatalf("native POST ids = %#v", postIDs)
	}
	if _, found, err := s.store.GetResponseAffinity(context.Background(), "native-store", time.Now()); err != nil || !found {
		t.Fatalf("stored native affinity found=%v err=%v", found, err)
	}
	if _, found, err := s.store.GetResponseAffinity(context.Background(), "native-no-store", time.Now()); err != nil || found {
		t.Fatalf("store:false native affinity found=%v err=%v", found, err)
	}

	get := httptest.NewRecorder()
	s.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/v1/responses/native-store", nil))
	if get.Code != http.StatusOK || !strings.Contains(get.Body.String(), `"native-store"`) {
		t.Fatalf("native GET = %d %s", get.Code, get.Body.String())
	}
	cancel := httptest.NewRecorder()
	s.ServeHTTP(cancel, httptest.NewRequest(http.MethodPost, "/v1/responses/native-store/cancel", nil))
	if cancel.Code != http.StatusOK || !strings.Contains(cancel.Body.String(), `"cancelled"`) {
		t.Fatalf("native cancel = %d %s", cancel.Code, cancel.Body.String())
	}
	_ = s.Shutdown(time.Second)
}

func TestServer_NativeResponsesStreamingPersistsAffinityWithoutRewriting(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"native-stream\",\"model\":\"m\",\"status\":\"in_progress\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"native-stream\",\"model\":\"m\",\"status\":\"completed\",\"output\":[{\"type\":\"function_call\",\"call_id\":\"call-1\",\"name\":\"lookup\",\"arguments\":\"{}\"}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}},
	}}
	s.routes()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hello","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("native stream status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("native stream content type=%q", got)
	}
	if !strings.Contains(recorder.Body.String(), "response.completed") || !strings.Contains(recorder.Body.String(), "[DONE]") {
		t.Fatalf("native stream was rewritten or truncated: %s", recorder.Body.String())
	}
	affinity, found, err := s.store.GetResponseAffinity(context.Background(), "native-stream", time.Now())
	if err != nil || !found {
		t.Fatalf("native stream affinity found=%v err=%v", found, err)
	}
	if affinity.Status != "completed" || !strings.Contains(string(affinity.Response), `"call_id":"call-1"`) || !strings.Contains(string(affinity.Response), `"input_tokens":3`) {
		t.Fatalf("native stream affinity=%+v", affinity)
	}
	_ = s.Shutdown(time.Second)
}

func TestResponseAffinityPersistsAfterRequestCancellation(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
	persistResponseAffinity(req, st, "m", "vllm", []byte(`{"store":true}`), []byte(`{"id":"resp-cancelled","model":"m","output":[]}`), http.StatusOK)

	if _, found, err := st.GetResponseAffinity(context.Background(), "resp-cancelled", time.Now()); err != nil || !found {
		t.Fatalf("cancelled request affinity found=%v err=%v", found, err)
	}
}

func TestResponseAffinitySkipsOversizedCanonicalResponse(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// The adapter must not turn a large but otherwise valid response into an
	// unbounded SQLite affinity row. The response is still sent to the client;
	// this helper only controls restart/query persistence.
	response := []byte(`{"id":"resp-too-large","model":"m","output_text":"` + strings.Repeat("x", maxAffinityResponseBytes) + `"}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"store":true}`))
	persistResponseAffinity(req, st, "m", "vllm", []byte(`{"store":true}`), response, http.StatusOK)
	if _, found, err := st.GetResponseAffinity(context.Background(), "resp-too-large", time.Now()); err != nil || found {
		t.Fatalf("oversized affinity found=%v err=%v", found, err)
	}
}

func TestNativeResponseAffinityPersistsFailureState(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	persistNativeResponseAffinity(context.Background(), st, "m", "vllm", []byte(`{"id":"resp-failed","status":"failed","error":{"message":"backend unavailable"}}`), header, http.StatusBadGateway)
	affinity, found, err := st.GetResponseAffinity(context.Background(), "resp-failed", time.Now())
	if err != nil || !found {
		t.Fatalf("failed native affinity found=%v err=%v", found, err)
	}
	if affinity.Status != "failed" {
		t.Fatalf("native failure status = %q, want failed", affinity.Status)
	}
}

func TestNativeResponseAffinityUsesFailedSSEState(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	header := make(http.Header)
	header.Set("Content-Type", "text/event-stream")
	body := []byte("event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-sse-failed\",\"model\":\"m\",\"status\":\"failed\",\"error\":{\"message\":\"bad input\"}}}\n\n")
	persistNativeResponseAffinity(context.Background(), st, "m", "vllm", body, header, http.StatusOK)
	affinity, found, err := st.GetResponseAffinity(context.Background(), "resp-sse-failed", time.Now())
	if err != nil || !found {
		t.Fatalf("failed SSE affinity found=%v err=%v", found, err)
	}
	if affinity.Status != "failed" {
		t.Fatalf("native SSE failure status = %q, want failed", affinity.Status)
	}
}

func TestResponseAffinityPersistsAcrossStoreRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	first, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := first.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp-restart", Model: "m", Backend: "vllm", Status: "completed",
		Response:  []byte(`{"id":"resp-restart","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"persisted"}]}]}`),
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		_ = first.Close()
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	affinity, found, err := reopened.GetResponseAffinity(context.Background(), "resp-restart", time.Now())
	if err != nil || !found {
		t.Fatalf("reopened affinity found=%v err=%v", found, err)
	}
	if affinity.Model != "m" || !strings.Contains(string(affinity.Response), "persisted") {
		t.Fatalf("reopened affinity = %+v", affinity)
	}

	chatBody, err := addPreviousResponseChainForModel(
		context.Background(),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"continue"}]}`),
		[]byte(`{"model":"m","input":"continue","previous_response_id":"resp-restart"}`),
		"m", reopened,
	)
	if err != nil {
		t.Fatalf("reopened previous_response_id: %v", err)
	}
	if !strings.Contains(string(chatBody), "persisted") {
		t.Fatalf("reopened response chain missing stored output: %s", chatBody)
	}

	s := newTestServer(newStubRouter([]string{"m"}, `{"id":"wrong"}`), newStubRouter(nil, ""))
	old := s.store
	_ = old.Close()
	s.store = reopened
	s.metrics.store = reopened
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	// Rebuild the mux after swapping the store so scoped-auth middleware probes
	// the reopened persistent database rather than the closed in-memory fixture.
	s.routes()
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/responses/resp-restart", nil))
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "persisted") {
		t.Fatalf("restarted chat GET = %d %s", w.Code, w.Body.String())
	}
	_ = s.Shutdown(time.Second)
}

func TestPreviousResponseChainNormalizesResponsesOutputContent(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	response := map[string]any{
		"id": "resp-content",
		"output": []any{
			map[string]any{
				"type": "message",
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "output_text", "text": "hello", "annotations": []any{}},
					map[string]any{"type": "refusal", "refusal": "no thanks"},
				},
			},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "think"}}},
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "lookup", "arguments": map[string]any{"q": "x"}},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": map[string]any{"ok": true}},
		},
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp-content", Model: "m", Backend: "vllm", Status: "completed",
		Response: body, ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	chatBody, err := addPreviousResponseChainForModel(
		context.Background(),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"continue"}]}`),
		[]byte(`{"model":"m","input":"continue","previous_response_id":"resp-content"}`),
		"m", st,
	)
	if err != nil {
		t.Fatalf("normalize previous response chain: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	assistant := messages[1].(map[string]any)
	content := assistant["content"].([]any)
	if content[0].(map[string]any)["type"] != "text" || content[0].(map[string]any)["text"] != "hello" {
		t.Fatalf("assistant content was not normalized: %#v", assistant["content"])
	}
	if assistant["refusal"] != "no thanks" {
		t.Fatalf("assistant refusal = %#v", assistant["refusal"])
	}
	reasoning := messages[2].(map[string]any)
	if reasoning["reasoning_content"] != "think" {
		t.Fatalf("reasoning content = %#v", reasoning["reasoning_content"])
	}
	call := messages[3].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if call["function"].(map[string]any)["arguments"] != `{"q":"x"}` {
		t.Fatalf("tool arguments = %#v", call)
	}
	if messages[4].(map[string]any)["content"] != `{"ok":true}` {
		t.Fatalf("tool output = %#v", messages[4].(map[string]any)["content"])
	}
}

func TestPreviousResponseChainReplaysCustomAndToolSearchCalls(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	response := map[string]any{
		"id": "resp-custom",
		"output": []any{
			map[string]any{
				"type": "custom_tool_call", "id": "ctc_call-1", "call_id": "call-1",
				"name": "apply_patch", "input": "*** Begin Patch",
			},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call-1", "output": "patched"},
			map[string]any{
				"type": "tool_search_call", "call_id": "call-2", "status": "completed",
				"execution": "client", "arguments": map[string]any{"query": "gmail"},
			},
		},
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp-custom", Model: "m", Backend: "vllm", Status: "completed",
		Response: body, ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	chatBody, err := addPreviousResponseChainForModel(
		context.Background(),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"continue"}]}`),
		[]byte(`{"model":"m","input":"continue","previous_response_id":"resp-custom"}`),
		"m", st,
	)
	if err != nil {
		t.Fatalf("replay previous response chain: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	messages := chat["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("expected 4 messages, got %d: %#v", len(messages), messages)
	}
	custom := messages[1].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if custom["function"].(map[string]any)["name"] != "apply_patch" {
		t.Fatalf("custom call name = %#v", custom)
	}
	if custom["function"].(map[string]any)["arguments"] != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom call arguments = %#v", custom["function"])
	}
	if messages[2].(map[string]any)["tool_call_id"] != "call-1" {
		t.Fatalf("custom output call id = %#v", messages[2])
	}
	search := messages[3].(map[string]any)["tool_calls"].([]any)[0].(map[string]any)
	if search["function"].(map[string]any)["name"] != "tool_search" {
		t.Fatalf("tool search name = %#v", search)
	}
	if search["function"].(map[string]any)["arguments"] != `{"query":"gmail"}` {
		t.Fatalf("tool search arguments = %#v", search["function"])
	}
}

func TestPreviousResponseChainRejectsUnrepresentableMessagePart(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp-annotation", Model: "m", Backend: "vllm", Status: "completed",
		Response:  []byte(`{"id":"resp-annotation","output":[{"type":"message","content":[{"type":"output_text","text":"hello","annotations":[{"type":"url_citation"}]}]}]}`),
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	_, err = addPreviousResponseChainForModel(
		context.Background(),
		[]byte(`{"model":"m","messages":[]}`),
		[]byte(`{"model":"m","input":"continue","previous_response_id":"resp-annotation"}`),
		"m", st,
	)
	if err == nil || !strings.Contains(err.Error(), "annotations") {
		t.Fatalf("expected annotation conversion error, got %v", err)
	}
}

func TestResponsesAdapterRejectsPreviousResponseFromAnotherModel(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if err := st.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp-other", Model: "other", Backend: "vllm", Status: "completed",
		Response:  []byte(`{"id":"resp-other","model":"other","output":[]}`),
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"target": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	called := false
	handler := CreateResponsesAdapterMiddleware(cfg, st)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"target","input":"continue","previous_response_id":"resp-other"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if called {
		t.Fatal("chat adapter should reject a cross-model response chain before dispatch")
	}
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "belongs to model") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestResponsesAdapterRejectsOversizedRequestBeforeConversion(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	called := false
	handler := CreateResponsesAdapterMiddleware(cfg, nil)(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	body := bytes.Repeat([]byte{'x'}, backendTransformBodyLimit+1)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "m", ModelID: "m"}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if called {
		t.Fatal("oversized Responses request reached the adapter")
	}
	if w.Code != http.StatusRequestEntityTooLarge || !strings.Contains(w.Body.String(), "exceeds") {
		t.Fatalf("oversized request response = %d %s", w.Code, w.Body.String())
	}
}

func TestResponseAffinityDoesNotConsumeUnknownLengthBody(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}},
	}}
	body := []byte(`{"model":"m","input":"preserve"}`)
	handler := CreateResponseAffinityMiddleware(cfg, nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("downstream body read: %v", err)
		}
		if !bytes.Equal(got, body) {
			t.Fatalf("downstream body = %q, want %q", got, body)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.ContentLength = -1
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "m", ModelID: "m"}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("unknown-length affinity response = %d %s", w.Code, w.Body.String())
	}
}

func TestResponseAffinityUnknownLengthStoreFalseSkipsPersistence(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}},
	}}
	handler := CreateResponseAffinityMiddleware(cfg, st)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-store-false","model":"m","status":"completed"}`))
	}))
	body := []byte(`{"model":"m","input":"preserve","store":false}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.ContentLength = -1
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "m", ModelID: "m"}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("store:false response = %d %s", w.Code, w.Body.String())
	}
	if _, found, err := st.GetResponseAffinity(context.Background(), "resp-store-false", time.Now()); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("unknown-length store:false request created response affinity")
	}
}

func TestResponseAffinitySkipsTruncatedOversizedResponse(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}},
	}}
	// Put a valid response id at the beginning so a naive parser of the capped
	// prefix would still find it. The full response must reach the client, but
	// the incomplete in-memory copy must never become restart-visible affinity.
	body := append([]byte(`{"id":"resp-oversized","model":"m","status":"completed","output":[{"type":"message","content":"`), bytes.Repeat([]byte{'x'}, maxAffinityResponseBytes)...)
	body = append(body, '"', '}', ']', '}')
	handler := CreateResponseAffinityMiddleware(cfg, st)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"large","store":true}`))
	req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: "m", ModelID: "m"}))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, req)
	if w.Code != http.StatusOK || len(w.Body.Bytes()) != len(body) {
		t.Fatalf("oversized response = status %d bytes %d, want status 200 bytes %d", w.Code, w.Body.Len(), len(body))
	}
	if _, found, err := st.GetResponseAffinity(context.Background(), "resp-oversized", time.Now()); err != nil {
		t.Fatal(err)
	} else if found {
		t.Fatal("truncated oversized response unexpectedly created response affinity")
	}
}

func TestResponseAffinityOversizedUnknownLengthBodyIsReplayed(t *testing.T) {
	body := append(bytes.Repeat([]byte{'x'}, backendTransformBodyLimit), 'y')
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.ContentLength = -1
	peek, inspectable := readRequestBodyForAffinity(req)
	if inspectable || peek != nil {
		t.Fatalf("oversized unknown body peek = %d bytes, inspectable=%v", len(peek), inspectable)
	}
	replayed, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(replayed, body) {
		t.Fatalf("replayed body length=%d, want=%d", len(replayed), len(body))
	}
}

func TestResponsesAdapterRejectsMalformedStoredResponseChain(t *testing.T) {
	st, err := store.New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, response := range []string{
		`{"id":"resp-invalid","model":"m","output":{}}`,
		`{"id":"resp-invalid","model":"m","output":[{"type":"function_call"}]}`,
		`{"id":"resp-invalid","model":"m","output":[{"type":"unknown"}]}`,
	} {
		if err := st.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
			ResponseID: "resp-invalid", Model: "m", Backend: "vllm", Status: "completed",
			Response: []byte(response), ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), UpdatedAt: time.Now(),
		}); err != nil {
			t.Fatal(err)
		}
		_, err := addPreviousResponseChainForModel(
			context.Background(),
			[]byte(`{"model":"m","messages":[{"role":"user","content":"continue"}]}`),
			[]byte(`{"model":"m","input":"continue","previous_response_id":"resp-invalid"}`),
			"m", st,
		)
		if err == nil {
			t.Fatalf("stored response %s should be rejected", response)
		}
	}
}

func TestResponseSSEOutputIndexRejectsNonFiniteAndOverflow(t *testing.T) {
	for _, value := range []any{float64(-1), float64(1.5), float64(1 << 63), "2"} {
		if got := responseSSEOutputIndex(map[string]any{"output_index": value}); got != 0 {
			t.Fatalf("malformed output_index %#v normalized to %d", value, got)
		}
	}
	if got := responseSSEOutputIndex(map[string]any{"output_index": float64(2)}); got != 2 {
		t.Fatalf("valid output_index normalized to %d", got)
	}
}
