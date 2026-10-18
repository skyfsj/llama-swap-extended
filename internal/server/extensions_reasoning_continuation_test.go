package server

import (
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
