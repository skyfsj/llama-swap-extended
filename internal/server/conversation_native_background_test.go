package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

const (
	nativeAckBody  = `{"id":"resp_native_1","object":"response","status":"in_progress","background":true,"output":[]}`
	nativeDoneBody = `{"id":"resp_native_1","object":"response","status":"completed","background":true,"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"DONE"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
)

// nativeBackgroundConfig wires one Responses provider whose underlying API
// stays reachable for the provider-owned job endpoints.
func nativeBackgroundConfig(endpoint, providerType string) config.Config {
	return config.Config{
		Gateway: config.GatewayConfig{Cooldown: config.GatewayCooldownConfig{RateLimit: "10ms", Transient: "10ms"}},
		Providers: config.ProviderDictionaryConfig{"p": {
			Type:     providerType,
			ApiKeys:  []config.ProviderKey{{Value: "k"}},
			Adapters: []config.ProviderAdapter{{Name: "a", Type: providerType, Endpoint: endpoint, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}},
		}},
	}
}

func postNativeGeneration(t *testing.T, s *Server, conversationID string) string {
	t.Helper()
	body := protocolFixtureJSON(map[string]any{
		"format":    dialect.OpenAIResponses,
		"execution": "native",
		"request":   json.RawMessage(conversationFixtureInput(dialect.OpenAIResponses, "BACKGROUND", false)),
	})
	queued := postJSON(t, s, "/api/conversations/"+conversationID+"/generations", string(body))
	if queued.Code != 202 {
		t.Fatalf("enqueue: %d %s", queued.Code, queued.Body.String())
	}
	id := protocolFixtureStr(protocolFixtureObj(queued.Body.Bytes())["id"])
	if id == "" {
		t.Fatalf("empty generation id: %s", queued.Body.String())
	}
	return id
}

// A provider-owned background generation is submitted once, watched server
// side, and committed as a checkpoint when the provider finishes. The saved
// native state must let the next turn continue the same provider job instead
// of replaying the whole history.
func TestServer_ConversationNativeBackgroundCommitsAndContinues(t *testing.T) {
	var mu sync.Mutex
	var posts int
	var continued string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			body, _ := io.ReadAll(r.Body)
			mu.Lock()
			posts++
			first := posts == 1
			if !first {
				var raw map[string]any
				_ = json.Unmarshal(body, &raw)
				previous, _ := raw["previous_response_id"].(string)
				continued = previous
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if first {
				_, _ = w.Write([]byte(nativeAckBody))
				return
			}
			_, _ = w.Write([]byte(`{"id":"resp_native_2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SECOND"}]}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/responses/resp_native_1":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(nativeDoneBody))
		default:
			t.Errorf("unexpected upstream call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := multiSurfaceServer(t, nativeBackgroundConfig(upstream.URL, config.ProviderTypeOpenAIResponses))
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	conversationID := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	jobID := postNativeGeneration(t, s, conversationID)

	g := awaitBackground(t, s, jobID)
	if g.State != "completed" || g.HTTPStatus != 200 || !strings.Contains(string(g.Result), "DONE") {
		t.Fatalf("job: state=%s status=%d result=%s", g.State, g.HTTPStatus, g.Result)
	}
	c, err := s.store.GetConversation(t.Context(), conversationID, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 {
		t.Fatalf("conversation version = %d, want 1", c.Version)
	}
	state, err := conversation.Decode(c.State)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Messages) != 2 || len(state.Native) != 1 {
		t.Fatalf("checkpoint = %d messages, %d native scopes; want 2/1: %s", len(state.Messages), len(state.Native), c.State)
	}
	var replyText string
	for _, part := range state.Messages[1].Parts {
		replyText += part.Text
	}
	if state.Messages[1].Role != dialect.RoleAssistant || !strings.Contains(replyText, "DONE") {
		t.Fatalf("assistant turn missing from checkpoint: %s", c.State)
	}

	// The next turn must continue the provider job rather than replay history.
	next := conversationFixtureInput(dialect.OpenAIResponses, "NEXT", false)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(next)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(conversationHeader, conversationID)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "SECOND") {
		t.Fatalf("continuation: %d %s", w.Code, w.Body.String())
	}
	mu.Lock()
	got := continued
	mu.Unlock()
	if got != "resp_native_1" {
		t.Fatalf("continuation sent previous_response_id=%q, want resp_native_1", got)
	}
}

// A provider that is briefly unavailable must not lose the job: the watcher
// keeps polling while the failure can still clear on its own.
func TestServer_ConversationNativeBackgroundSurvivesTransientFailure(t *testing.T) {
	var gets atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(nativeAckBody))
			return
		}
		if gets.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"try again"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeDoneBody))
	}))
	defer upstream.Close()

	s := multiSurfaceServer(t, nativeBackgroundConfig(upstream.URL, config.ProviderTypeOpenAIResponses))
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	conversationID := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	jobID := postNativeGeneration(t, s, conversationID)

	g := awaitBackgroundLong(t, s, jobID, 15*time.Second)
	if g.State != "completed" {
		t.Fatalf("a transient upstream failure ended the job: state=%s result=%s", g.State, g.Result)
	}
	if gets.Load() < 2 {
		t.Fatalf("polls = %d, want the failed poll to be retried", gets.Load())
	}
}

// Cancellation reaches the provider and leaves the checkpoint untouched.
func TestServer_ConversationNativeBackgroundCancellation(t *testing.T) {
	var cancelCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(nativeAckBody))
		case r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_native_1","object":"response","status":"in_progress","output":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/responses/resp_native_1/cancel":
			cancelCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp_native_1","object":"response","status":"cancelled","output":[]}`))
		default:
			t.Errorf("unexpected upstream call: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	s := multiSurfaceServer(t, nativeBackgroundConfig(upstream.URL, config.ProviderTypeOpenAIResponses))
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	conversationID := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	jobID := postNativeGeneration(t, s, conversationID)

	if cancel := postJSON(t, s, "/api/conversation-generations/"+jobID+"/cancel", `{}`); cancel.Code != 200 {
		t.Fatalf("cancel: %d %s", cancel.Code, cancel.Body.String())
	}
	g := awaitBackgroundLong(t, s, jobID, 15*time.Second)
	if g.State != "cancelled" {
		t.Fatalf("state = %s result=%s", g.State, g.Result)
	}
	if cancelCalls.Load() == 0 {
		t.Fatal("the provider never received the cancellation")
	}
	c, err := s.store.GetConversation(t.Context(), conversationID, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 0 {
		t.Fatalf("cancelled generation advanced the checkpoint to version %d", c.Version)
	}
}

// A client can ask for provider-native background execution against a surface
// that has no native background concept. The request must be refused instead
// of iterating the job in the gateway and calling the result "native".
func TestServer_ConversationNativeBackgroundRequiresNativeSurface(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat_1","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"DONE"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	// A Chat-dialect provider cannot host a provider-owned Responses job.
	s := multiSurfaceServer(t, nativeBackgroundConfig(upstream.URL, config.ProviderTypeOpenAI))
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	conversationID := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	jobID := postNativeGeneration(t, s, conversationID)

	g := awaitBackground(t, s, jobID)
	if g.State != "failed" {
		t.Fatalf("non-native surface produced %s: %s", g.State, g.Result)
	}
	c, err := s.store.GetConversation(t.Context(), conversationID, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 0 {
		t.Fatalf("a non-native generation advanced the checkpoint to %d", c.Version)
	}
}

// After a restart the job keeps running upstream. Polling must re-attach to
// the saved scope and finish the checkpoint instead of reporting a loss.
func TestServer_ConversationNativeBackgroundResumesAfterRestart(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(nativeAckBody))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nativeDoneBody))
	}))
	defer upstream.Close()

	s := multiSurfaceServer(t, nativeBackgroundConfig(upstream.URL, config.ProviderTypeOpenAIResponses))
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	conversationID := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	jobID := postNativeGeneration(t, s, conversationID)
	if g := awaitBackground(t, s, jobID); g.State != "completed" {
		t.Fatalf("seed generation: %s %s", g.State, g.Result)
	}

	// Rebuild the record as another gateway process would have left it: the
	// saved scope, a stale epoch, and a version the checkpoint has not taken.
	c, err := s.store.GetConversation(t.Context(), conversationID, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	state, err := conversation.Decode(c.State)
	if err != nil {
		t.Fatal(err)
	}
	var native conversation.Native
	for _, entry := range state.Native {
		native = entry
	}
	data := nativeCheckpointData{
		State:         conversation.State{Messages: state.Messages[:1]},
		Native:        native,
		Version:       c.Version,
		Configuration: s.nativeGenerationConfiguration(),
		ResponseID:    "resp_native_1",
	}
	encoded, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	resumed := store.ConversationGeneration{
		ID: "ggen_resume", Owner: "anonymous", ConversationID: conversationID, Model: "m",
		Epoch: "process-that-died", Format: string(dialect.OpenAIResponses), State: "queued",
	}
	if err := s.store.CreateConversationGeneration(t.Context(), resumed); err != nil {
		t.Fatal(err)
	}
	resumed.State = "in_progress"
	resumed.Native = encoded
	if err := s.store.UpdateConversationGeneration(t.Context(), resumed, "queued"); err != nil {
		t.Fatal(err)
	}

	get := httptest.NewRecorder()
	s.ServeHTTP(get, httptest.NewRequest(http.MethodGet, "/api/conversation-generations/ggen_resume", nil))
	if get.Code != 200 {
		t.Fatalf("poll: %d %s", get.Code, get.Body.String())
	}
	g := awaitBackgroundLong(t, s, "ggen_resume", 15*time.Second)
	if g.State != "completed" {
		t.Fatalf("resumed job: state=%s result=%s", g.State, g.Result)
	}
	after, err := s.store.GetConversation(t.Context(), conversationID, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if after.Version != c.Version+1 {
		t.Fatalf("resumed commit took version %d, want %d", after.Version, c.Version+1)
	}
}

// awaitBackgroundLong is awaitBackground with a caller-chosen budget for the
// tests that deliberately exercise a retry.
func awaitBackgroundLong(t *testing.T, s *Server, id string, budget time.Duration) store.ConversationGeneration {
	t.Helper()
	deadline := time.NewTimer(budget)
	defer deadline.Stop()
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	for {
		g, err := s.store.GetConversationGeneration(t.Context(), id, "anonymous")
		if err != nil {
			t.Fatal(err)
		}
		if g.State != "queued" && g.State != "in_progress" {
			return g
		}
		select {
		case <-tick.C:
		case <-deadline.C:
			t.Fatalf("background generation timed out: %+v", g)
		}
	}
}
