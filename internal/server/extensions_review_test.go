package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// The round-1 fixture every surface shares: the model asks for the extension's
// own `lookup` and for the caller's `client_tool` in one round.
const mixedRoundFixture = `{"id":"a","choices":[{"index":0,"message":{"role":"assistant","content":"checking","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`

// sseFrame renders one SSE data frame from a Go value.
func sseFrame(payload map[string]any) string {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	return "data: " + string(encoded) + "\n\n"
}

// callFunctionArguments renders a call's function arguments for a counter-fixture.
func callFunctionArguments(call extensionCall) string { return call.Arguments }

func mixedRoundServer(t *testing.T, rounds func(w http.ResponseWriter, r *http.Request, round int)) *Server {
	t.Helper()
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		// The inner handler reads the body again, so hand it back.
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		round := 1
		if strings.Contains(string(body), `"role":"tool"`) {
			round = 2
		}
		rounds(w, r, round)
	}
	s := extensionTestServer(t, local)
	model := s.cfg.Models["m"]
	s.cfg.Models["m2"] = model
	return s
}

func postExtensionJSON(s *Server, path, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

// A streamed Anthropic turn must never repeat a content_block index: the client
// accumulates by index, so a repeat overwrites a block and leaves the paired
// result behind as an orphan, which then reaches the upstream conversation.
func TestServer_ExtensionsAnthropicStreamIndicesUnique(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		switch {
		case strings.Contains(string(body), `"role":"tool"`) || strings.Contains(string(body), `"tool_call_id"`):
			_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
		default:
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"checking\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
		}
	}
	s := extensionTestServer(t, local)
	// The model asks for the extension's tool only: the daemon runs it and must
	// keep streaming the turn, which is where the index bookkeeping matters.
	w := postExtensionJSON(s, "/v1/messages", `{"model":"m","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", w.Code, w.Body.String())
	}
	starts := map[int]string{}
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil || event["type"] != "content_block_start" {
			continue
		}
		index := int(event["index"].(float64))
		block, _ := event["content_block"].(map[string]any)
		kind, _ := block["type"].(string)
		if previous, exists := starts[index]; exists {
			t.Fatalf("content_block index %d was reused (%s then %s): %s", index, previous, kind, w.Body.String())
		}
		starts[index] = kind
	}
	if len(starts) < 3 {
		t.Fatalf("expected text + server call + result blocks, saw %v", starts)
	}
}

// The streamed Responses completed frame must list a server call once - as the
// artifact that carries its output - because that frame is what clients keep and
// what the daemon stores for previous_response_id.
func TestServer_ExtensionsResponsesStreamCompletedListsServerCallOnce(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(raw), `"role":"tool"`) {
			_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "b", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "done"}}}})))
			_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "b", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "checking"}}}})))
		fragments := []any{
			map[string]any{"index": 0, "id": "s1", "function": map[string]any{"name": "lookup", "arguments": string(mustJSON(t, map[string]any{"q": "x"}))}},
			map[string]any{"index": 1, "id": "c1", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
		}
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": fragments}}}})))
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	s := extensionTestServer(t, local)
	w := postExtensionJSON(s, "/v1/responses", `{"model":"m","input":"hi","stream":true}`)
	content := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", w.Code, content)
	}
	completed := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, "response.completed") {
			completed = strings.TrimPrefix(line, "data: ")
		}
	}
	if completed == "" {
		t.Fatalf("no response.completed frame: %.300s", content)
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(completed), &frame); err != nil {
		t.Fatal(err)
	}
	response, _ := frame["response"].(map[string]any)
	items, _ := response["output"].([]any)
	serverCalls, artifacts := 0, 0
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "mcp_call":
			if item["name"] == "lookup" {
				artifacts++
			}
		case "function_call":
			if item["name"] == "lookup" {
				serverCalls++
			}
		}
	}
	if artifacts != 1 || serverCalls != 0 {
		t.Fatalf("the server call must be listed once, as its artifact (artifacts=%d calls=%d): %s", artifacts, serverCalls, completed)
	}
	id, _ := response["id"].(string)
	if id == "" {
		t.Fatalf("completed frame has no response id: %.200s", completed)
	}
	affinity, found, err := s.store.GetResponseAffinity(context.Background(), id, time.Now())
	if err != nil || !found {
		t.Fatalf("affinity row found=%v err=%v (the stored chain must be the visible turn)", found, err)
	}
	stored := string(affinity.Response)
	if !strings.Contains(stored, "mcp_call") || strings.Contains(stored, `"name":"lookup","status":"completed"`) {
		t.Fatalf("stored chain is not the visible turn: %s", stored)
	}
}

// store:false means no retrievable state: the visible-response re-persist must
// honor it like the middleware it replaces.
func TestServer_ExtensionsResponsesStoreFalseSkipsAffinity(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		mark := "one"
		if strings.Contains(string(raw), "second") {
			mark = "two"
		}
		calls := []any{
			map[string]any{"id": "s-" + mark, "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":\"x\"}"}},
			map[string]any{"id": "c-" + mark, "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
		}
		_, _ = w.Write(mustJSON(t, map[string]any{"id": mark, "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}}))
	}
	s := extensionTestServer(t, local)
	for _, tc := range []struct {
		name  string
		body  string
		store bool
	}{
		{"default", `{"model":"m","input":"first"}`, true},
		{"store false", `{"model":"m","input":"second","store":false}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postExtensionJSON(s, "/v1/responses", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
			}
			id, _ := mustJSONObject(t, w.Body.Bytes())["id"].(string)
			if id == "" {
				t.Fatalf("no response id: %.200s", w.Body.String())
			}
			_, found, err := s.store.GetResponseAffinity(context.Background(), id, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if found != tc.store {
				t.Fatalf("affinity row found=%v for response %s, want %v", found, id, tc.store)
			}
		})
	}
}

// A recorded call whose arguments are not valid JSON is reported to the model as
// a failed tool, exactly like the in-request path: it must not run with no input.
func TestServer_ExtensionsDeferredMalformedArgumentsReportedToModel(t *testing.T) {
	var second map[string]any
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), `"role":"tool"`) {
			_ = json.Unmarshal(raw, &second)
			_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			return
		}
		calls := []any{
			map[string]any{"id": "s1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{not json"}},
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
		}
		_, _ = w.Write(mustJSON(t, map[string]any{"id": "a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}}))
	}
	s := extensionTestServer(t, local)
	first := postExtensionJSON(s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first=%d %.200s", first.Code, first.Body.String())
	}
	call := extensionCalls(mustJSONObject(t, first.Body.Bytes()), "chat.completions")[0]
	w := postExtensionJSON(s, "/v1/chat/completions", fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, call.ID, call.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("replay=%d %.200s", w.Code, w.Body.String())
	}
	upstream := fmt.Sprintf("%v", second["messages"])
	if !strings.Contains(upstream, "not valid JSON") {
		t.Fatalf("the malformed call was not reported to the model: %s", upstream)
	}
	if strings.Contains(upstream, "found:x") {
		t.Fatalf("the extension ran with empty arguments anyway: %s", upstream)
	}
}

// The continuation error branches: 400 for an incomplete request, 409 for state
// that changed or is in use, 403 for another identity's round.
func TestServer_ExtensionsContinuationStatusBranches(t *testing.T) {
	newMixed := func(t *testing.T) (*Server, string, string) {
		t.Helper()
		s := mixedRoundServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
			_, _ = w.Write([]byte(mixedRoundFixture))
		})
		first := postExtensionJSON(s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
		if first.Code != http.StatusOK {
			t.Fatalf("first=%d %.200s", first.Code, first.Body.String())
		}
		calls := extensionCalls(mustJSONObject(t, first.Body.Bytes()), "chat.completions")
		if len(calls) == 0 {
			t.Fatal("no caller call")
		}
		return s, calls[0].ID, callFunctionArguments(calls[0])
	}
	replay := func(id string) string {
		return fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, id, id)
	}

	t.Run("missing a client result", func(t *testing.T) {
		local := newStubRouter([]string{"m"}, "")
		local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.WriteString(w, `{"id":"a","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}},{"id":"c2","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
		}
		s := extensionTestServer(t, local)
		first := postExtensionJSON(s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
		var payload map[string]any
		if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		calls := extensionCalls(payload, "chat.completions")
		if len(calls) != 2 {
			t.Fatalf("expected two caller calls, got %+v", calls)
		}
		// Only one of the two results comes back.
		body := fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}},{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, calls[0].ID, calls[1].ID, calls[0].ID)
		w := postExtensionJSON(s, "/v1/chat/completions", body)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "missing a client tool result") {
			t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
		}
	})
	t.Run("claimed", func(t *testing.T) {
		s, id, _ := newMixed(t)
		record, err := s.store.LookupExtensionPending(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.store.ClaimExtensionPending(context.Background(), record.ID); err != nil {
			t.Fatal(err)
		}
		w := postExtensionJSON(s, "/v1/chat/completions", replay(id))
		if w.Code != http.StatusConflict {
			t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
		}
	})
	t.Run("profile changed", func(t *testing.T) {
		s, id, _ := newMixed(t)
		s.activeProfile = "another-profile"
		w := postExtensionJSON(s, "/v1/chat/completions", replay(id))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "profile changed") {
			t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
		}
	})
	t.Run("model changed", func(t *testing.T) {
		s, id, _ := newMixed(t)
		w := postExtensionJSON(s, "/v1/chat/completions", strings.Replace(replay(id), `"model":"m"`, `"model":"m2"`, 1))
		if w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "model changed") {
			t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
		}
	})
	t.Run("another identity", func(t *testing.T) {
		s, id, _ := newMixed(t)
		s.cfg.RequiredAPIKeys = []string{"key-a", "key-b"}
		s.routes()
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(replay(id)))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("Authorization", "Bearer key-b")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "identity mismatch") {
			t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
		}
	})
}

// A continuation is an ordinary request: request hooks run on it like any other.
func TestServer_ExtensionsContinuationRunsRequestHooks(t *testing.T) {
	var second map[string]any
	s := mixedRoundServer(t, func(w http.ResponseWriter, r *http.Request, round int) {
		raw, _ := io.ReadAll(r.Body)
		if round == 1 {
			_, _ = w.Write([]byte(mixedRoundFixture))
			return
		}
		_ = json.Unmarshal(raw, &second)
		_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	})
	first := postExtensionJSON(s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
	call := extensionCalls(mustJSONObject(t, first.Body.Bytes()), "chat.completions")[0]
	second = nil
	w := postExtensionJSON(s, "/v1/chat/completions", fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, call.ID, call.ID))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
	}
	// The fixture's onRequest appends a system message carrying "extension".
	if upstream := fmt.Sprintf("%v", second["messages"]); !strings.Contains(upstream, "extension") {
		t.Fatalf("onRequest did not run on the continuation: %s", upstream)
	}
}

// A successful streaming Chat Completions mixed round: the caller's own call
// arrives under a public id (never the extension's), the round is recorded, and
// the stream ends cleanly with a finish frame and [DONE].
func TestServer_ExtensionsChatStreamMixedResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds > 1 {
			_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "b", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "done"}}}})))
			_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "b", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "checking"}}}})))
		fragments := []any{
			map[string]any{"index": 0, "id": "s1", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":\"x\"}"}},
			map[string]any{"index": 1, "id": "c1", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
		}
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": fragments}}}})))
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	s := extensionTestServer(t, local)
	first := postExtensionJSON(s, "/v1/chat/completions", `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
	content := first.Body.String()
	if first.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", first.Code, content)
	}
	if rounds != 1 {
		t.Fatalf("rounds=%d, the mixed round must return to the caller", rounds)
	}
	if !strings.Contains(content, "checking") {
		t.Fatalf("stream text lost: %.300s", content)
	}
	if strings.Contains(content, `"name":"lookup"`) {
		t.Fatalf("the extension's own call leaked to the client: %.400s", content)
	}
	if !strings.Contains(content, `"name":"client_tool"`) {
		t.Fatalf("the caller's own call is missing: %.400s", content)
	}
	if strings.Count(content, "[DONE]") != 1 {
		t.Fatalf("the stream must end with exactly one [DONE]: %.300s", content)
	}
	var streamed map[string]any
	if err := json.Unmarshal([]byte(frameWithToolCalls(t, content)), &streamed); err != nil {
		t.Fatal(err)
	}
	choices, _ := streamed["choices"].([]any)
	delta, _ := choices[0].(map[string]any)["delta"].(map[string]any)
	fragments, _ := delta["tool_calls"].([]any)
	if len(fragments) != 1 {
		t.Fatalf("visible call fragments = %v (content=%.300s)", fragments, content)
	}
	fragment, _ := fragments[0].(map[string]any)
	publicID, _ := fragment["id"].(string)
	name, _ := fragment["function"].(map[string]any)["name"].(string)
	if !strings.HasPrefix(publicID, "call_ls_") || name != "client_tool" {
		t.Fatalf("visible call = %v", fragment)
	}
	// The caller replays the turn; the extension's own call runs then.
	replay := fmt.Sprintf(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, publicID, publicID)
	second := postExtensionJSON(s, "/v1/chat/completions", replay)
	if second.Code != http.StatusOK || rounds != 2 {
		t.Fatalf("second=%d rounds=%d %.300s", second.Code, rounds, second.Body.String())
	}
	if !strings.Contains(second.Body.String(), "done") || strings.Count(second.Body.String(), "[DONE]") != 1 {
		t.Fatalf("the continuation stream did not end cleanly: %.300s", second.Body.String())
	}
}

// frameWithToolCalls picks the streamed chunk that carries tool_calls, so the
// visible call can be inspected like a non-streaming body.
func frameWithToolCalls(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, "data: ") && strings.Contains(line, `"tool_calls"`) {
			return strings.TrimPrefix(line, "data: ")
		}
	}
	t.Fatalf("no tool_calls frame in %.300s", body)
	return ""
}

// A streaming Responses turn that runs several server-tool rounds must announce
// its items on a single monotonic output_index axis: an artifact emitted after an
// already-indexed item may neither repeat nor skip beyond what the completed
// frame lists, because clients accumulate by that index.
func TestServer_ExtensionsResponsesStreamIndicesStayOnOneAxis(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		// Two server-tool rounds before the answer: one artifact round was
		// contiguous even under the broken arithmetic, so the anchor has to be
		// exercised twice.
		if rounds > 2 {
			_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "d", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "done"}}}})))
			_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "d", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
			return
		}
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "thinking"}}}})))
		fragments := []any{map[string]any{"index": 0, "id": "s" + itoa(rounds), "function": map[string]any{"name": "lookup", "arguments": string(mustJSON(t, map[string]any{"q": itoa(rounds)}))}}}
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": fragments}}}})))
		_, _ = w.Write([]byte(sseFrame(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}
	s := extensionTestServer(t, local)
	s.cfg.Extensions.MaxToolRounds = 4
	s.routes()
	w := postExtensionJSON(s, "/v1/responses", `{"model":"m","input":"hi","stream":true}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", w.Code, w.Body.String())
	}
	seen := []int{}
	completed := ""
	for _, line := range strings.Split(w.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")
		var frame map[string]any
		if json.Unmarshal([]byte(payload), &frame) != nil {
			continue
		}
		if strings.Contains(payload, "response.completed") {
			completed = payload
		}
		if index, ok := frame["output_index"].(float64); ok && frame["type"] == "response.output_item.added" {
			seen = append(seen, int(index))
		}
	}
	if len(seen) < 5 {
		t.Fatalf("expected two artifact rounds plus the answer, saw %v", seen)
	}
	// A fresh turn starts at 0 and every item takes the next index: a gap means the
	// round offset was added twice.
	for i, index := range seen {
		if index != i {
			t.Fatalf("output_index is not contiguous from 0 (%v): the round offset was added twice", seen)
		}
	}
	if completed == "" {
		t.Fatalf("no response.completed frame: %.300s", w.Body.String())
	}
	var frame map[string]any
	if err := json.Unmarshal([]byte(completed), &frame); err != nil {
		t.Fatal(err)
	}
	response, _ := frame["response"].(map[string]any)
	items, _ := response["output"].([]any)
	artifacts, messages := 0, 0
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "mcp_call":
			artifacts++
		case "message":
			messages++
		}
	}
	if artifacts != 2 || messages < 1 {
		t.Fatalf("completed frame does not list both artifacts and the answer: %.300s", completed)
	}
	// The announced axis must cover exactly the items the completed frame lists.
	if seen[len(seen)-1] > len(items)+messages {
		t.Fatalf("last announced output_index %d runs past the completed frame (%d items): %v", seen[len(seen)-1], len(items), seen)
	}
}

// itoa keeps the fixture free of fmt in its hot path.
func itoa(value int) string {
	return fmt.Sprintf("%d", value)
}

// A buffered multi-choice round must not hand the caller a call from an alternate
// choice - and in particular not the extension's own server tool, which the caller
// never declared and cannot run.
func TestServer_ExtensionsChatMultiChoiceDoesNotLeakServerCalls(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		choice0 := map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}}}}, "finish_reason": "tool_calls"}
		choice1 := map[string]any{"index": 1, "message": map[string]any{"role": "assistant", "tool_calls": []any{
			map[string]any{"id": "s9", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{\"q\":\"x\"}"}}}}, "finish_reason": "tool_calls"}
		_, _ = w.Write(mustJSON(t, map[string]any{"id": "a", "choices": []any{choice0, choice1}}))
	}
	s := extensionTestServer(t, local)
	w := postExtensionJSON(s, "/v1/chat/completions", `{"model":"m","n":2,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "lookup") {
		t.Fatalf("the extension's own call leaked from an alternate choice: %.300s", body)
	}
	if !strings.Contains(body, "client_tool") {
		t.Fatalf("the caller's own call was dropped: %.300s", body)
	}
}

// Retention on the visible Responses path needs permission from both ends: the
// caller's own store:false is a privacy request a request hook must not override,
// and a hook that asks for no retention is honored too.
func TestServer_ExtensionsResponsesStoreFlagUnion(t *testing.T) {
	build := func(t *testing.T, hook string) *Server {
		t.Helper()
		root := t.TempDir()
		dir := filepath.Join(root, "policy")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: policy\nenabled: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object" } } }],
  onToolCall(ctx, call) { return { content: "found:x" }; }` + hook + `
};`
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
		manager, err := extensions.NewManager(root)
		if err != nil {
			t.Fatal(err)
		}
		local := newStubRouter([]string{"m"}, "")
		local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.ReadAll(r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustJSON(t, map[string]any{"id": "a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": []any{
				map[string]any{"id": "s1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": "{}"}},
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
			}}, "finish_reason": "tool_calls"}}}))
		}
		s := newTestServer(local, newStubRouter(nil, ""))
		s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
		s.extensions = manager
		s.routes()
		t.Cleanup(func() { _ = s.store.Close() })
		return s
	}
	storeOf := func(extra string) string {
		body := `{"model":"m","input":"hi"`
		if extra != "" {
			body += `,"store":` + extra
		}
		return body + `}`
	}
	for _, tc := range []struct {
		name   string
		hook   string
		body   string
		stored bool
	}{
		{"control", "", storeOf(""), true},
		{"caller false", "", storeOf("false"), false},
		{"hook false", `, onRequest(ctx, request) { request.store = false; return request; }`, storeOf(""), false},
		{"caller false, hook true", `, onRequest(ctx, request) { request.store = true; return request; }`, storeOf("false"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := build(t, tc.hook)
			w := postExtensionJSON(s, "/v1/responses", tc.body)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
			}
			id, _ := mustJSONObject(t, w.Body.Bytes())["id"].(string)
			if id == "" {
				t.Fatalf("no response id: %.200s", w.Body.String())
			}
			_, found, err := s.store.GetResponseAffinity(context.Background(), id, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			if found != tc.stored {
				t.Fatalf("affinity row found=%v, want %v (caller's privacy request and operator policy both have to allow retention)", found, tc.stored)
			}
		})
	}
}
