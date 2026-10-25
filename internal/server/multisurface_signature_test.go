package server

// The multi-surface forward path converts per candidate, so it has its own
// copy of the thought-signature bookkeeping. It used to convert without
// recording a scope, without restoring cached signatures and without giving the
// response writer the cache, so a Chat client talking to a Gemini provider that
// also exposes a sibling surface lost every signature on the second turn.
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

// geminiSurfaceRecorder is a fake Gemini upstream that stamps function calls
// with a signature and omits functionCall.id, exactly as the API does.
type geminiSurfaceRecorder struct {
	mu     sync.Mutex
	bodies []string
}

func (g *geminiSurfaceRecorder) record(r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	g.mu.Lock()
	defer g.mu.Unlock()
	g.bodies = append(g.bodies, string(body))
}

func (g *geminiSurfaceRecorder) last() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.bodies) == 0 {
		return ""
	}
	return g.bodies[len(g.bodies)-1]
}

func (g *geminiSurfaceRecorder) callCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.bodies)
}

// multiSurfaceGeminiConfig exposes one model on a Gemini adapter and an
// Anthropic adapter. Two surfaces on one model is what selects the forward
// path; the Gemini adapter is listed first so it serves the request.
func multiSurfaceGeminiConfig(geminiURL, anthropicURL string) config.Config {
	return config.Config{
		Models: map[string]config.ModelConfig{
			"local-model": {Cmd: "echo hi", Proxy: "http://127.0.0.1:1"},
		},
		Providers: config.ProviderDictionaryConfig{
			"multi": {
				ApiKeys: []config.ProviderKey{{Value: "k-test"}},
				Adapters: []config.ProviderAdapter{
					{
						Name:     "Gemini",
						Type:     config.ProviderTypeGemini,
						Endpoint: geminiURL,
						Models:   []config.ProviderModel{{Local: "gemini-4-argon", Remote: "gemini-4-argon"}},
					},
					{
						Name:     "Messages",
						Type:     config.ProviderTypeAnthropic,
						Endpoint: anthropicURL,
						Models:   []config.ProviderModel{{Local: "gemini-4-argon", Remote: "claude-messages"}},
					},
				},
			},
		},
	}
}

func TestServer_MultiSurfaceRestoresGeminiThoughtSignature(t *testing.T) {
	gemini := &geminiSurfaceRecorder{}
	geminiUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gemini.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, geminiCallWithSignature)
	}))
	defer geminiUpstream.Close()

	anthropic := &surfaceRecorder{}
	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropic.record(r)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, claudeSurfaceResponse)
	}))
	defer anthropicUpstream.Close()

	s := multiSurfaceServer(t, multiSurfaceGeminiConfig(geminiUpstream.URL, anthropicUpstream.URL))

	// Turn 1: the Gemini surface answers a Chat client with a signed call.
	first := postJSON(t, s, "/v1/chat/completions",
		`{"model":"gemini-4-argon","messages":[{"role":"user","content":"find x"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("turn 1 status=%d body=%s", first.Code, first.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &response); err != nil {
		t.Fatalf("turn 1 body is not JSON: %s", first.Body.String())
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

	// Turn 2 replays it. A Chat client cannot carry the signature back, so the
	// forward path has to restore it or Gemini 3 rejects the turn.
	second := postJSON(t, s, "/v1/chat/completions",
		`{"model":"gemini-4-argon","messages":[`+
			`{"role":"user","content":"find x"},`+
			`{"role":"assistant","content":null,"tool_calls":[{"id":"`+callID+`","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},`+
			`{"role":"tool","tool_call_id":"`+callID+`","content":"found"},`+
			`{"role":"user","content":"and now?"}`+
			`],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`)
	if second.Code != http.StatusOK {
		t.Fatalf("turn 2 status=%d body=%s", second.Code, second.Body.String())
	}
	if gemini.callCount() < 2 {
		t.Fatalf("the Gemini surface was called %d times, want 2", gemini.callCount())
	}
	replayed := gemini.last()
	if !strings.Contains(replayed, "SIG-UPSTREAM") {
		t.Fatalf("the forward path lost the signature on replay: %s", replayed)
	}
	if strings.Contains(replayed, geminiPlaceholderSignature) {
		t.Fatalf("the forward path used the placeholder although a signature was cached: %s", replayed)
	}
}
