package server

// End-to-end thought-signature carry-over: a Chat client talks to a Gemini
// upstream through the adapter and gets no signatures (Chat has no field for
// them), so the relay must restore them from its own cache on the next turn.
// These tests drive the real middleware and the real multi-surface forward
// path, because the two have separate conversions and each one used to lose the
// cache on its own.
import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

const geminiCallWithSignature = `{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"SIG-UPSTREAM"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":3,"candidatesTokenCount":1,"totalTokenCount":4}}`

// geminiSignatureRecorder is a fake Gemini upstream that stamps every function
// call with a signature and never sends a functionCall.id, which is what the
// real API does. It records the bodies it receives.
type geminiSignatureRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (g *geminiSignatureRecorder) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bodies = append(g.bodies, string(body))
}

func (g *geminiSignatureRecorder) last() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		return ""
	}
	return g.bodies[len(g.bodies)-1]
}

func (g *geminiSignatureRecorder) count() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.bodies)
}

func TestE2E_ThoughtSignatureRestoredOnReplayThroughAdapter(t *testing.T) {
	upstream := &geminiSignatureRecorder{}
	next := withConvertedPath(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiCallWithSignature)
	})
	handler := CreateProtocolAdapterMiddleware(
		config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeGemini), newGeminiSignatureCache())(next)

	headers := map[string]string{"X-Session-Id": "conversation-sig"}

	// Turn 1: the client asks, the upstream answers with a signed call. The
	// signature never reaches the Chat client, so the relay must keep it.
	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gemini-4-argon","messages":[{"role":"user","content":"find x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
	first.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		first.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, first)
	if w.Code != http.StatusOK {
		t.Fatalf("turn 1 status=%d body=%s", w.Code, w.Body.String())
	}

	var response map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatalf("turn 1 body is not JSON: %s", w.Body.String())
	}
	choices, _ := response["choices"].([]any)
	if len(choices) == 0 {
		t.Fatalf("turn 1 response has no choices: %v", response)
	}
	message, _ := choices[0].(map[string]any)["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("turn 1 tool calls = %v", message)
	}
	callID, _ := calls[0].(map[string]any)["id"].(string)
	if callID == "" {
		t.Fatalf("turn 1 tool call has no id: %v", calls[0])
	}

	// Turn 2 replays the call the client received plus its result. The client
	// cannot attach the signature, so the converted body must still carry it.
	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gemini-4-argon","messages":[`+
			`{"role":"user","content":"find x"},`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":"`+callID+`","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},`+
			`{"role":"tool","tool_call_id":"`+callID+`","content":"found"},`+
			`{"role":"user","content":"and now?"}`+
			`],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
	second.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		second.Header.Set(key, value)
	}
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, second)
	if w.Code != http.StatusOK {
		t.Fatalf("turn 2 status=%d body=%s", w.Code, w.Body.String())
	}

	if upstream.count() != 2 {
		t.Fatalf("upstream calls = %d, want 2", upstream.count())
	}
	replayed := upstream.last()
	if !strings.Contains(replayed, "SIG-UPSTREAM") {
		t.Fatalf("the replayed call lost its signature, so Gemini 3 rejects the turn: %s", replayed)
	}
	if strings.Contains(replayed, geminiPlaceholderSignature) {
		t.Fatalf("the replayed call fell back to the placeholder although a signature was cached: %s", replayed)
	}
}

// A different conversation must not receive another conversation's signature:
// the signature is opaque and tied to the conversation that produced it.
func TestE2E_ThoughtSignatureNotSharedAcrossConversations(t *testing.T) {
	upstream := &geminiSignatureRecorder{}
	next := withConvertedPath(t, func(w http.ResponseWriter, r *http.Request) {
		upstream.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiCallWithSignature)
	})
	handler := CreateProtocolAdapterMiddleware(
		config.Config{Models: map[string]config.ModelConfig{}}, nil,
		providerDialectStub(config.ProviderTypeGemini), newGeminiSignatureCache())(next)

	first := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gemini-4-argon","messages":[{"role":"user","content":"find x"}]}`))
	first.Header.Set("Content-Type", "application/json")
	first.Header.Set("X-Session-Id", "conversation-a")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, first)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}

	// Conversation B replays the same positional call id; it must not inherit
	// conversation A's signature.
	second := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(
		`{"model":"gemini-4-argon","messages":[`+
			`{"role":"user","content":"other"},`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":"call_gemini_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},`+
			`{"role":"tool","tool_call_id":"call_gemini_1","content":"found"}`+
			`]}`))
	second.Header.Set("Content-Type", "application/json")
	second.Header.Set("X-Session-Id", "conversation-b")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, second)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(upstream.last(), "SIG-UPSTREAM") {
		t.Fatalf("a signature leaked across conversations: %s", upstream.last())
	}
}
