package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// geminiExtensionServer builds a server whose extension advertises one server
// tool and matches the Gemini endpoint.
type geminiRoundHandler func(http.ResponseWriter, *http.Request, int)

func geminiExtensionServer(t *testing.T, upstream geminiRoundHandler) (*Server, *[]string) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "gcal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "id: gcal\nenabled: true\npriority: 10\nmatch:\n  endpoints: [gemini.generateContent]\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object", properties: { q: { type: "string" } } } } }],
  onToolCall(ctx, call) { return { content: "found:" + call.arguments.q }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	var bodies []string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		upstream(w, r, len(bodies))
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{
		Models: map[string]config.ModelConfig{
			// Native: the backend speaks Gemini, so the request is not converted
			// to Chat and the extension's Gemini tool path is exercised.
			"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}},
		},
		Extensions: config.ExtensionsConfig{Enabled: true, Directory: root},
	}
	s.cfg.UI.Activity.SessionID = []string{"X-Session-ID"}
	s.extensions = manager
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	return s, &bodies
}

// TestServer_ExtensionsGeminiServerToolExecutes guards the Gemini branch of the
// server-tool loop: the tool is advertised, the model's functionCall must be
// executed by the gateway, and the functionResponse must be sent back to the
// upstream before the client sees a final answer.
func TestServer_ExtensionsGeminiServerToolExecutes(t *testing.T) {
	s, bodies := geminiExtensionServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			// The local backend answers in Chat; the protocol adapter converts
			// it back to Gemini before the extension sees the turn.
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c2","object":"chat.completion","created":2,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Found it"},"finish_reason":"stop"}]}`)
	})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(*bodies) != 2 {
		t.Fatalf("rounds=%d, want 2 (the tool must be executed, not handed to the client): %s", len(*bodies), w.Body.String())
	}
	// Round 2 is the continuation the adapter converted back to the backend's
	// Chat dialect: the tool result must be there, paired with the call.
	second := (*bodies)[1]
	if !strings.Contains(second, "found:x") {
		t.Fatalf("second round missing the tool result: %s", second)
	}
	// Gemini carries no call ids, so the gateway mints one and must use the
	// same id on the replayed assistant turn and on the tool result.
	var round2 map[string]any
	if err := json.Unmarshal([]byte(second), &round2); err != nil {
		t.Fatalf("second round is not JSON: %v (%s)", err, second)
	}
	messages, _ := round2["messages"].([]any)
	var callID, resultID string
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
			call, _ := calls[0].(map[string]any)
			callID, _ = call["id"].(string)
		}
		if message["role"] == "tool" {
			resultID, _ = message["tool_call_id"].(string)
		}
	}
	if callID == "" || callID != resultID {
		t.Fatalf("call/result ids are not paired: call=%q result=%q (%s)", callID, resultID, second)
	}
	if strings.Contains(w.Body.String(), "functionCall") {
		t.Fatalf("client received an unexecuted server tool call: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "Found it") {
		t.Fatalf("final answer missing: %s", w.Body.String())
	}
	var parsed map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &parsed); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
}

func TestServer_ExtensionsGeminiStreamServerToolExecutes(t *testing.T) {
	s, bodies := geminiExtensionServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		if round == 1 {
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Searching \"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		} else {
			_, _ = io.WriteString(w, "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Found\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"c2\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		}
		if flusher != nil {
			flusher.Flush()
		}
	})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:streamGenerateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if len(*bodies) != 2 {
		t.Fatalf("rounds=%d, want 2: %s", len(*bodies), w.Body.String())
	}
	if !strings.Contains((*bodies)[1], "found:x") {
		t.Fatalf("stream continuation missing the tool result: %s", (*bodies)[1])
	}
	body := w.Body.String()
	if !strings.Contains(body, "Searching ") || !strings.Contains(body, "Found") {
		t.Fatalf("stream text lost: %s", body)
	}
	if strings.Contains(body, `"functionCall"`) {
		t.Fatalf("unexecuted tool call leaked to the client: %s", body)
	}
}

// A Gemini mixed round hands the caller its own functionCall and records the
// extension's own call. Gemini's classic function parts carry no ids, so the
// result that comes back is paired by name and the recorded round is looked up
// the same way.
func TestServer_ExtensionsGeminiMixedResume(t *testing.T) {
	s, bodies := geminiExtensionServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c2","object":"chat.completion","created":2,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"Found it"},"finish_reason":"stop"}]}`)
	})
	first := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	first.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	s.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first=%d body=%s", firstRecorder.Code, firstRecorder.Body.String())
	}
	if len(*bodies) != 1 {
		t.Fatalf("rounds=%d, the mixed round must return to the caller", len(*bodies))
	}
	// The caller sees its own call; the extension's call stayed hidden.
	visible := firstRecorder.Body.String()
	if !strings.Contains(visible, `"name":"client_tool"`) {
		t.Fatalf("the caller's own call is missing: %s", visible)
	}
	if strings.Contains(visible, `"name":"lookup"`) || strings.Contains(visible, "found:x") {
		t.Fatalf("the extension's own call leaked to the caller: %s", visible)
	}
	// The caller runs its tool and sends the results back by name, exactly as a
	// Gemini client does.
	second := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"client-result"}}}]}]}`))
	second.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	s.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK || len(*bodies) != 2 {
		t.Fatalf("second=%d rounds=%d %s", secondRecorder.Code, len(*bodies), secondRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), "Found it") {
		t.Fatalf("final answer missing: %s", secondRecorder.Body.String())
	}
	// Upstream sees the whole round: the extension's own call with its result and
	// the caller's result, all paired.
	upstream := (*bodies)[1]
	for _, want := range []string{"found:x", "client-result", "lookup", "client_tool"} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("upstream conversation is missing %q: %s", want, upstream)
		}
	}
}

// Two caller calls of the same tool in one round are ordinary model output (two
// cities, two files). The record is keyed by name, so both must be recorded
// without a key collision, and the caller's two functionResponses must pair with
// them in order.
func TestServer_ExtensionsGeminiSameNameCallsInOneRound(t *testing.T) {
	var bodies []string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			calls := []any{
				map[string]any{"id": "s1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":\"x\"}"}},
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{\"path\":\"/a\"}"}},
				map[string]any{"id": "c2", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{\"path\":\"/b\"}"}},
			}
			_, _ = w.Write(mustJSON(t, map[string]any{"id": "a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}}))
			return
		}
		_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"Found it"},"finish_reason":"stop"}]}`)
	}
	m, err := extensions.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = m
	s := newTestServer(local, newStubRouter(nil, ""))
	root := t.TempDir()
	dir := filepath.Join(root, "gcal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "id: gcal\nenabled: true\npriority: 10\nmatch:\n  endpoints: [gemini.generateContent]\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object", properties: { q: { type: "string" } } } } }],
  onToolCall(ctx, call) { return { content: "found:" + call.arguments.q }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = manager
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })

	first := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	first.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	s.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first=%d %.300s", firstRecorder.Code, firstRecorder.Body.String())
	}
	if len(bodies) != 1 {
		t.Fatalf("rounds=%d, the mixed round must return to the caller", len(bodies))
	}
	// The caller runs both of its calls and answers by name, in order.
	second := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[
			{"role":"user","parts":[{"text":"hi"}]},
			{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{"path":"/a"}}},{"functionCall":{"name":"client_tool","args":{"path":"/b"}}}]},
			{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"a-content"}}},{"functionResponse":{"name":"client_tool","response":{"content":"b-content"}}}]}]}`))
	second.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	s.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("second=%d %.300s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), "Found it") {
		t.Fatalf("final answer missing: %.200s", secondRecorder.Body.String())
	}
	upstream := bodies[len(bodies)-1]
	for _, want := range []string{"found:x", "a-content", "b-content", "lookup", "client_tool"} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("upstream conversation is missing %q: %s", want, upstream)
		}
	}
}

// The record of a name-keyed surface belongs to the caller's identity: another
// key's round with the same tool name must neither replace it nor be folded into
// it.
func TestServer_ExtensionsGeminiRecordsAreScopedToIdentity(t *testing.T) {
	s, _ := geminiExtensionServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	})
	s.cfg.RequiredAPIKeys = []string{"key-a", "key-b"}
	s.routes()

	ask := func(key, user, args string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":%q}]}]}`, user)
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	// The stub always asks with q=x, so both identities record the same round;
	// what must survive is that each key's record stays its own.
	if w := ask("key-a", "alpha", `{"q":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("key-a first=%d %.200s", w.Code, w.Body.String())
	}
	if w := ask("key-b", "beta", `{"q":"x"}`); w.Code != http.StatusOK {
		t.Fatalf("key-b first=%d %.200s", w.Code, w.Body.String())
	}
	// Both records must still be addressable: key-a's continuation finds its own.
	cont := ask("key-a", "alpha", `{"q":"x"}`)
	if cont.Code == http.StatusOK {
		// A plain generateContent with no functionResponse is not a continuation;
		// it is served as a new turn, which is fine. The real assertion is below.
		t.Log("plain turn served")
	}
	replay := `{"contents":[
		{"role":"user","parts":[{"text":"alpha"}]},
		{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"a-content"}}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(replay))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer key-a")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("key-a continuation was refused: %d %.300s", w.Code, w.Body.String())
	}
}

// A replayed turn whose caller call does not match the record (same tool name,
// different arguments) belongs to another conversation: it is refused, not folded.
func TestServer_ExtensionsGeminiMismatchedReplayRefused(t *testing.T) {
	s, _ := geminiExtensionServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	})
	first := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	first.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	s.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first=%d %.200s", firstRecorder.Code, firstRecorder.Body.String())
	}
	// Same tool name, different arguments: a different conversation's call.
	replay := `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{"path":"/elsewhere"}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"other"}}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(replay))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "does not match the pending round") {
		t.Fatalf("status=%d %.300s", w.Code, w.Body.String())
	}
}

// A streamed Gemini round with two calls keeps both (they arrive as separate
// one-part chunks), records the round, and ends with a finishReason.
func TestServer_ExtensionsGeminiStreamKeepsEveryCallAndFinish(t *testing.T) {
	s, bodies := geminiExtensionServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if round == 1 {
			_, _ = io.WriteString(w, "data: "+string(mustJSON(t, map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Searching "}}}}))+"\n\n")
			_, _ = io.WriteString(w, "data: "+string(mustJSON(t, map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "s1", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":\"x\"}"}}}}}}}))+"\n\n")
			_, _ = io.WriteString(w, "data: "+string(mustJSON(t, map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 1, "id": "c1", "function": map[string]any{"name": "client_tool", "arguments": "{}"}}}}}}}))+"\n\n")
			_, _ = io.WriteString(w, "data: "+string(mustJSON(t, map[string]any{"id": "c1", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}))+"\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: "+string(mustJSON(t, map[string]any{"id": "c2", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "Found"}}}}))+"\n\n")
		_, _ = io.WriteString(w, "data: "+string(mustJSON(t, map[string]any{"id": "c2", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}}))+"\n\ndata: [DONE]\n\n")
	})
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:streamGenerateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, "Searching ") {
		t.Fatalf("stream text lost: %.300s", body)
	}
	if strings.Contains(body, `"name":"lookup"`) {
		t.Fatalf("the extension's own call leaked to the client: %.400s", body)
	}
	if !strings.Contains(body, "client_tool") {
		t.Fatalf("the caller's own call is missing from the stream: %.400s", body)
	}
	if !strings.Contains(body, "finishReason") {
		t.Fatalf("the stream ended without a finishReason: %.400s", body)
	}
	// The round really was mixed (both calls seen): the caller can continue it,
	// the extension's own call runs then, and nothing ran before the replay. With
	// the calls collapsed into one slot the round was never recorded and this
	// replay reached the upstream without the extension's result.
	replay := `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"file content"}}}]}]}`
	second := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:streamGenerateContent", strings.NewReader(replay))
	second.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	s.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("the streamed mixed round could not be continued: %d %.300s", secondRecorder.Code, secondRecorder.Body.String())
	}
	upstream := (*bodies)[len(*bodies)-1]
	for _, want := range []string{"found:x", "file content"} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("the continued conversation is missing %q: %s", want, upstream)
		}
	}
}

// Two conversations of the same caller can call the same tool with the same
// arguments; the prompt is what tells them apart, so a superseded round is
// refused instead of running the other conversation's server call.
func TestServer_ExtensionsGeminiIdenticalCallerArgsDoNotCrossConversations(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), `"role":"tool"`) {
			_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			return
		}
		// The server call differs per conversation, the caller's own call does not.
		prompt := "alpha"
		if strings.Contains(string(raw), "beta") {
			prompt = "beta"
		}
		calls := []any{
			map[string]any{"id": "s1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": string(mustJSON(t, map[string]any{"q": prompt}))}},
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
		}
		_, _ = w.Write(mustJSON(t, map[string]any{"id": "a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}}))
	}
	m, err := extensions.NewManager(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_ = m
	root := t.TempDir()
	dir := filepath.Join(root, "gcal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: gcal\nenabled: true\npriority: 10\nmatch:\n  endpoints: [gemini.generateContent]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object", properties: { q: { type: "string" } } } } }],
  onToolCall(ctx, call) { return { content: "found:" + call.arguments.q }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = manager
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })

	ask := func(prompt string) *httptest.ResponseRecorder {
		body := fmt.Sprintf(`{"contents":[{"role":"user","parts":[{"text":%q}]}]}`, prompt)
		req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, req)
		return w
	}
	if w := ask("alpha"); w.Code != http.StatusOK {
		t.Fatalf("alpha=%d %.200s", w.Code, w.Body.String())
	}
	if w := ask("beta"); w.Code != http.StatusOK {
		t.Fatalf("beta=%d %.200s", w.Code, w.Body.String())
	}
	// Alpha replays its own turn, with the same client arguments as beta's round.
	replay := `{"contents":[
		{"role":"user","parts":[{"text":"alpha"}]},
		{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"a-content"}}}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(replay))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "does not match the pending round") {
		t.Fatalf("alpha's superseded round was not refused: %d %.300s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "found:beta") {
		t.Fatalf("beta's round was folded into alpha's conversation: %.300s", w.Body.String())
	}
}

// A Gemini continuation that appends a new user message must still complete the
// round: the fingerprint covers the conversation before the model turn, so the
// appended message cannot make the record look like another conversation's.
func TestServer_ExtensionsGeminiContinuationKeepsNewMessage(t *testing.T) {
	s, bodies := geminiExtensionServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		w.Header().Set("Content-Type", "application/json")
		if round == 1 {
			_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c2","choices":[{"index":0,"message":{"role":"assistant","content":"Found it"},"finish_reason":"stop"}]}`)
	})
	first := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent",
		strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	first.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	s.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first=%d %.200s", firstRecorder.Code, firstRecorder.Body.String())
	}
	// The caller answers its own call and then adds something new.
	replay := `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"file content"}}}]},
		{"role":"user","parts":[{"text":"and now something else"}]}]}`
	second := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(replay))
	second.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	s.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK {
		t.Fatalf("a continuation with new content was refused: %d %.300s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if !strings.Contains(secondRecorder.Body.String(), "Found it") {
		t.Fatalf("final answer missing: %.200s", secondRecorder.Body.String())
	}
	upstream := (*bodies)[len(*bodies)-1]
	for _, want := range []string{"found:x", "file content", "and now something else"} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("upstream conversation is missing %q: %s", want, upstream)
		}
	}
}

// A request hook may rewrite the prompt (that is its purpose). The record's
// conversation fingerprint is taken before the hooks, so the caller's own replay
// still matches its round instead of being refused as a different conversation.
func TestServer_ExtensionsGeminiHookRewrittenPromptStillContinues(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "gcal")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: gcal\nenabled: true\npriority: 10\nmatch:\n  endpoints: [gemini.generateContent]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object", properties: { q: { type: "string" } } } } }],
  onRequest(ctx, request) { request.contents[0].parts.push({ text: "hint" }); return request; },
  onToolCall(ctx, call) { return { content: "found:" + call.arguments.q }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	bodies := []string{}
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if len(bodies) == 1 {
			_, _ = io.WriteString(w, `{"id":"c1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"c2","choices":[{"index":0,"message":{"role":"assistant","content":"Found it"},"finish_reason":"stop"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = manager
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })

	first := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	first.Header.Set("Content-Type", "application/json")
	firstRecorder := httptest.NewRecorder()
	s.ServeHTTP(firstRecorder, first)
	if firstRecorder.Code != http.StatusOK {
		t.Fatalf("first=%d %.200s", firstRecorder.Code, firstRecorder.Body.String())
	}
	replay := `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"file content"}}}]}]}`
	second := httptest.NewRequest(http.MethodPost, "/v1beta/models/m:generateContent", strings.NewReader(replay))
	second.Header.Set("Content-Type", "application/json")
	secondRecorder := httptest.NewRecorder()
	s.ServeHTTP(secondRecorder, second)
	if secondRecorder.Code != http.StatusOK || !strings.Contains(secondRecorder.Body.String(), "Found it") {
		t.Fatalf("a hook-rewritten prompt broke the continuation: %d %.300s", secondRecorder.Code, secondRecorder.Body.String())
	}
	if upstream := bodies[len(bodies)-1]; !strings.Contains(upstream, "found:x") {
		t.Fatalf("the deferred call did not run: %s", upstream)
	}
}
