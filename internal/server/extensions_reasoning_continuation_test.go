package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// reasoningRoundUpstream is the streaming upstream both tests below drive: the
// first round thinks out loud and then calls the extension's server tool, the
// second round answers. The forwarded round bodies are collected so the test
// can read what the continuation actually carried.
func reasoningRoundUpstream(forwarded *[]string) func(http.ResponseWriter, *http.Request) {
	return func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*forwarded = append(*forwarded, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		if len(*forwarded) == 1 {
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"reasoning_content\":\"let me think\"}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
}

// TestServer_ExtensionStreamBridgeKeepsReasoningOnResponsesContinuation is the
// regression guard for the thinking-mode continuation: a Responses client whose
// round streams reasoning and then calls a server tool must get that reasoning
// back on the next round. Without it a thinking-mode upstream refuses the
// request ("The `reasoning_content` in the thinking mode must be passed back to
// the API") because the continuation's assistant turn has no reasoning at all.
func TestServer_ExtensionStreamBridgeKeepsReasoningOnResponsesContinuation(t *testing.T) {
	var forwarded []string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = reasoningRoundUpstream(&forwarded)
	s := extensionTestServer(t, local)

	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", w.Code, w.Body.String())
	}
	if len(forwarded) < 2 {
		t.Fatalf("expected a continuation round, got %d round(s)", len(forwarded))
	}
	if !strings.Contains(forwarded[1], `"reasoning_content":"let me think"`) {
		t.Fatalf("the continuation dropped the round's reasoning: %s", forwarded[1])
	}
}

// TestServer_ExtensionStreamBridgeKeepsReasoningOnChatContinuation covers the
// same guard for a Chat client: the assistant turn the bridge rebuilds carries
// reasoning_content rather than losing it with the stream.
func TestServer_ExtensionStreamBridgeKeepsReasoningOnChatContinuation(t *testing.T) {
	var forwarded []string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = reasoningRoundUpstream(&forwarded)
	s := extensionTestServer(t, local)

	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", w.Code, w.Body.String())
	}
	if len(forwarded) < 2 {
		t.Fatalf("expected a continuation round, got %d round(s)", len(forwarded))
	}
	if !strings.Contains(forwarded[1], `"reasoning_content":"let me think"`) {
		t.Fatalf("the continuation dropped the round's reasoning: %s", forwarded[1])
	}
}

// TestServer_ExtensionStreamBridgeKeepsToolCallIdentityAcrossNullFragments is
// the regression guard for the Xiaomi MiMo shape: every tool-call fragment
// after the first repeats "id": null and "function": {"name": null}. Rebuilding
// the round's native response used to overwrite the accumulated id and name
// with those nulls, so the continuation guard could not match the round it came
// from and the request died with "native tool continuation no longer matches
// the completed upstream turn" — after the server tools had already run and
// without the client ever seeing an error.
func TestServer_ExtensionStreamBridgeKeepsToolCallIdentityAcrossNullFragments(t *testing.T) {
	callIDs := []string{
		"call_6453c690e29b400d8e533924",
		"call_b72af15753e54794af0e4710",
		"call_c61f45a6d44a47cdb4a717c0",
	}
	var forwarded []string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwarded = append(forwarded, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		if len(forwarded) > 1 {
			_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		write := func(delta map[string]any) {
			encoded, _ := json.Marshal(map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": delta}}})
			_, _ = io.WriteString(w, "data: "+string(encoded)+"\n\n")
		}
		write(map[string]any{"reasoning_content": "let me think"})
		write(map[string]any{"content": "searching"})
		for index, callID := range callIDs {
			write(map[string]any{"tool_calls": []any{map[string]any{
				"index": index, "id": callID, "type": "function",
				"function": map[string]any{"name": "lookup", "arguments": `{"q":`},
			}}})
			// MiMo repeats the call on the following fragments with the fields
			// that do not change sent as explicit nulls, role included.
			write(map[string]any{"role": nil, "tool_calls": []any{map[string]any{
				"index": index, "id": nil, "type": "function",
				"function": map[string]any{"name": nil, "arguments": `"x"}`},
			}}})
		}
		write(map[string]any{})
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}
	s := extensionTestServer(t, local)

	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","stream":true}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)

	body := w.Body.String()
	if len(forwarded) < 2 {
		t.Fatalf("the tool round never continued: %s", body)
	}
	if strings.Contains(body, "native tool continuation") {
		t.Fatalf("the continuation guard rejected the round: %s", body)
	}
	if !strings.Contains(body, `"content":"done"`) && !strings.Contains(body, "done") {
		t.Fatalf("the answer is missing: %s", body)
	}
	for _, callID := range callIDs {
		if !strings.Contains(forwarded[1], `"id":"`+callID+`"`) {
			t.Fatalf("the continuation lost the call id %s: %s", callID, forwarded[1])
		}
	}
	if !strings.Contains(forwarded[1], `"name":"lookup"`) {
		t.Fatalf("the continuation lost the call name: %s", forwarded[1])
	}
	if !strings.Contains(forwarded[1], `"role":"assistant"`) {
		t.Fatalf("the continuation lost the assistant role: %s", forwarded[1])
	}
}
