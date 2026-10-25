package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func conversationFixtureInput(format dialect.Format, text string, stream bool) []byte {
	body := protocolFixtureObj(protocolFixtureRequest(format, stream))
	delete(body, "tools")
	switch format {
	case dialect.OpenAIChat, dialect.ClaudeMessages:
		body["messages"] = []any{map[string]any{"role": "user", "content": text}}
	case dialect.OpenAIResponses:
		body["input"] = text
	case dialect.Gemini:
		body["contents"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}}}
	case dialect.GeminiInteractions:
		body["input"] = text
	}
	return protocolFixtureJSON(body)
}

func postConversationTurn(t *testing.T, s *Server, id string, format dialect.Format, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, protocolFixturePath(format, strings.Contains(string(body), `"stream":true`)), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(conversationHeader, id)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	return w
}

func TestServer_ConversationProtocolMatrix(t *testing.T) {
	for _, client := range protocolFixtureFormats {
		for _, upstream := range protocolFixtureFormats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s__%s/stream=%t", client, upstream, stream), func(t *testing.T) {
					calls := 0
					var second []byte
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls++
						data, _ := io.ReadAll(r.Body)
						if calls == 2 {
							second = data
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(200)
							w.(http.Flusher).Flush()
							wire := protocolFixtureEncodeSSE(protocolFixtureFinalStream(upstream))
							wire = bytes.ReplaceAll(wire, []byte("final"), []byte(fmt.Sprintf("native_%d", calls)))
							w.Write(wire)
						} else {
							w.Header().Set("Content-Type", "application/json")
							body := protocolFixtureObj(protocolFixtureFinalResponse(upstream))
							if upstream != dialect.Gemini {
								body["id"] = fmt.Sprintf("native_%d", calls)
							}
							w.Write(protocolFixtureJSON(body))
						}
					}))
					defer backend.Close()
					cfg := config.Config{Models: map[string]config.ModelConfig{}, Providers: config.ProviderDictionaryConfig{"provider": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "adapter", Type: protocolFixtureSurface(upstream), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
					s := multiSurfaceServer(t, cfg)
					created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
					if created.Code != 201 {
						t.Fatalf("create: %d %s", created.Code, created.Body.String())
					}
					id := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
					for _, text := range []string{"FIRST", "NEXT"} {
						body := conversationFixtureInput(client, text, stream)
						req := httptest.NewRequest("POST", protocolFixturePath(client, stream), bytes.NewReader(body))
						req.Header.Set("Content-Type", "application/json")
						req.Header.Set(conversationHeader, id)
						w := httptest.NewRecorder()
						s.ServeHTTP(w, req)
						if w.Code != 200 || !strings.Contains(w.Body.String(), "DONE") {
							t.Fatalf("turn status=%d body=%s", w.Code, w.Body.String())
						}
					}
					if calls != 2 {
						t.Fatalf("upstream calls=%d", calls)
					}
					var actual map[string]any
					_ = json.Unmarshal(second, &actual)
					if upstream == dialect.OpenAIResponses || upstream == dialect.GeminiInteractions {
						key := "previous_response_id"
						expected := "native_1"
						if upstream == dialect.GeminiInteractions {
							key = "previous_interaction_id"
						} else if stream {
							expected = "resp_native_1"
						}
						if actual[key] != expected {
							t.Errorf("native continuation not used: %s", second)
						}
					} else if !strings.Contains(string(second), "FIRST") || !strings.Contains(string(second), "DONE") {
						t.Errorf("native transcript lost: %s", second)
					}
					c, err := s.store.GetConversation(t.Context(), id, "anonymous")
					if err != nil || c.Version != 2 {
						t.Fatalf("conversation version=%d err=%v", c.Version, err)
					}
					state, err := conversation.Decode(c.State)
					if err != nil || len(state.Messages) != 4 {
						t.Fatalf("portable state messages=%d err=%v state=%s", len(state.Messages), err, c.State)
					}
				})
			}
		}
	}
}

func TestServer_ConversationCheckpointForkAndCompact(t *testing.T) {
	local := newStubRouter([]string{"m"}, string(protocolFixtureFinalResponse(dialect.OpenAIChat)))
	s := newTestServer(local, newStubRouter(nil, ""))
	defer s.store.Close()
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	id := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	w := postConversationTurn(t, s, id, dialect.OpenAIChat, conversationFixtureInput(dialect.OpenAIChat, "FIRST", false))
	if w.Code != 200 {
		t.Fatalf("turn: %d %s", w.Code, w.Body.String())
	}
	cp := postJSON(t, s, "/api/conversations/"+id+"/checkpoints", `{}`)
	checkpoint := protocolFixtureStr(protocolFixtureObj(cp.Body.Bytes())["id"])
	if cp.Code != 201 {
		t.Fatalf("checkpoint: %d %s", cp.Code, cp.Body.String())
	}
	fork := postJSON(t, s, "/api/conversations/"+id+"/fork", fmt.Sprintf(`{"checkpoint_id":%q}`, checkpoint))
	if fork.Code != 201 {
		t.Fatalf("fork: %d %s", fork.Code, fork.Body.String())
	}
	compact := postJSON(t, s, "/api/conversations/"+id+"/compact", `{"summary":"FIRST was answered","retain_messages":0}`)
	if compact.Code != 200 {
		t.Fatalf("compact: %d %s", compact.Code, compact.Body.String())
	}
	c, err := s.store.GetConversation(t.Context(), id, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	state, _ := conversation.Decode(c.State)
	if len(state.Messages) != 0 || state.Summary != "FIRST was answered" {
		t.Fatalf("compaction state: %s", c.State)
	}
}

func TestServer_ConversationProviderIsolationAndCheckpoint(t *testing.T) {
	var seenA, seenB [][]byte
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenA = append(seenA, body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"msg_a","model":"m","type":"message","role":"assistant","content":[{"type":"thinking","thinking":"readable summary","signature":"A_SIGNATURE"},{"type":"text","text":"A_DONE"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seenB = append(seenB, body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_b","model":"m","status":"completed","output":[{"type":"reasoning","id":"rs_b","encrypted_content":"B_OPAQUE","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"B_DONE"}]}]}`)
	}))
	defer b.Close()
	cfg := config.Config{Models: map[string]config.ModelConfig{}, Providers: config.ProviderDictionaryConfig{}}
	for _, entry := range []struct{ id, url, kind string }{{"a", a.URL, config.ProviderTypeAnthropic}, {"b", b.URL, config.ProviderTypeOpenAIResponses}} {
		cfg.Providers[entry.id] = config.ProviderConfig{ApiKeys: []config.ProviderKey{{Value: "key-" + entry.id}}, Adapters: []config.ProviderAdapter{{Name: "adapter", Type: entry.kind, Endpoint: entry.url, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}
	}
	s := multiSurfaceServer(t, cfg)
	created := postJSON(t, s, "/api/conversations", `{}`)
	id := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	turn := func(id, model, text string) {
		t.Helper()
		body := protocolFixtureJSON(map[string]any{"model": model, "input": text})
		w := postConversationTurn(t, s, id, dialect.OpenAIResponses, body)
		if w.Code != 200 {
			t.Fatalf("turn: %d %s", w.Code, w.Body.String())
		}
	}
	turn(id, "a/m", "FIRST")
	cp := postJSON(t, s, "/api/conversations/"+id+"/checkpoints", `{}`)
	cpID := protocolFixtureStr(protocolFixtureObj(cp.Body.Bytes())["id"])
	turn(id, "b/m", "SECOND")
	if strings.Contains(string(seenB[0]), "A_SIGNATURE") {
		t.Fatalf("Anthropic signature leaked to Responses: %s", seenB[0])
	}
	turn(id, "a/m", "THIRD")
	if strings.Contains(string(seenA[1]), "B_OPAQUE") || strings.Contains(string(seenA[1]), "A_SIGNATURE") {
		t.Fatalf("stale or foreign opaque state replayed: %s", seenA[1])
	}
	if !strings.Contains(string(seenA[1]), "SECOND") || !strings.Contains(string(seenA[1]), "B_DONE") {
		t.Fatalf("provider switch lost portable turns: %s", seenA[1])
	}
	fork := postJSON(t, s, "/api/conversations/"+id+"/fork", fmt.Sprintf(`{"checkpoint_id":%q}`, cpID))
	branch := protocolFixtureStr(protocolFixtureObj(fork.Body.Bytes())["id"])
	if fork.Code != 201 {
		t.Fatalf("fork: %d %s", fork.Code, fork.Body.String())
	}
	turn(branch, "a/m", "BRANCH")
	if !strings.Contains(string(seenA[2]), "A_SIGNATURE") {
		t.Fatalf("matching checkpoint did not restore native opaque state: %s", seenA[2])
	}
}
