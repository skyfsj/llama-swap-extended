package server

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestServer_ProtocolCompatibilityPolicy(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","choices":[{"message":{"role":"assistant","content":"DONE"},"finish_reason":"stop"}]}`))
	}))
	defer backend.Close()
	cfg := config.Config{Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
	s := multiSurfaceServer(t, cfg)
	body := `{"model":"m","input":"hello","background":true}`
	for _, mode := range []string{"portable", "strict", "native"} {
		r := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("X-Llama-Swap-Compatibility", mode)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if mode == "portable" {
			if w.Code != 200 || !strings.Contains(w.Header().Get("X-Llama-Swap-Conversion-Diagnostics"), "background_dropped") {
				t.Fatalf("portable=%d %s headers=%v", w.Code, w.Body.String(), w.Header())
			}
		} else if w.Code < 400 {
			t.Fatalf("%s silently accepted loss: %d %s", mode, w.Code, w.Body.String())
		}
	}
	plan := postJSON(t, s, "/api/protocol/plan", `{"model":"m","source":"openai.responses","request":`+body+`}`)
	if plan.Code != 200 || !bytes.Contains(plan.Body.Bytes(), []byte(`"classification":"degraded"`)) {
		t.Fatalf("plan=%d %s", plan.Code, plan.Body.String())
	}
}

func TestServer_ConversationAutomaticSummary(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"m"}, `{"id":"summary","choices":[{"message":{"role":"assistant","content":"Remember K=7391 and completed tool A."},"finish_reason":"stop"}]}`), newStubRouter(nil, ""))
	defer s.store.Close()
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	id := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	w := postJSON(t, s, "/api/conversations/"+id+"/compact", `{"retain_messages":0}`)
	if w.Code != 200 {
		t.Fatalf("compaction=%d %s", w.Code, w.Body.String())
	}
	get := httptest.NewRecorder()
	s.ServeHTTP(get, httptest.NewRequest("GET", "/api/conversations/"+id, nil))
	if !strings.Contains(get.Body.String(), "K=7391") {
		t.Fatalf("summary missing: %s", get.Body.String())
	}
}
