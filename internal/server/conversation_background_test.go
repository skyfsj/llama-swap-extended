package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func awaitBackground(t *testing.T, s *Server, id string) store.ConversationGeneration {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
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
			t.Fatal("background generation timed out")
		}
	}
}

func TestServer_ConversationBackgroundMatrix(t *testing.T) {
	for _, client := range protocolFixtureFormats {
		for _, upstream := range protocolFixtureFormats {
			t.Run(fmt.Sprintf("%s__%s", client, upstream), func(t *testing.T) {
				backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					w.Header().Set("Content-Type", "application/json")
					w.Write(protocolFixtureFinalResponse(upstream))
				}))
				defer backend.Close()
				cfg := config.Config{Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: protocolFixtureSurface(upstream), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
				s := multiSurfaceServer(t, cfg)
				created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
				id := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
				body := protocolFixtureJSON(map[string]any{"format": client, "request": json.RawMessage(conversationFixtureInput(client, "BACKGROUND", false))})
				queued := postJSON(t, s, "/api/conversations/"+id+"/generations", string(body))
				if queued.Code != 202 {
					t.Fatalf("enqueue: %d %s", queued.Code, queued.Body.String())
				}
				jobID := protocolFixtureStr(protocolFixtureObj(queued.Body.Bytes())["id"])
				if jobID == "" {
					t.Fatalf("empty generation id: %s", queued.Body.String())
				}
				g := awaitBackground(t, s, jobID)
				if g.State != "completed" || g.HTTPStatus != 200 {
					t.Fatalf("job: %+v %s", g, g.Result)
				}
				c, err := s.store.GetConversation(t.Context(), id, "anonymous")
				if err != nil || c.Version != 1 {
					t.Fatalf("checkpoint: %+v %v", c, err)
				}
				for range 2 {
					r := httptest.NewRequest("GET", "/api/conversation-generations/"+jobID, nil)
					w := httptest.NewRecorder()
					s.ServeHTTP(w, r)
					if w.Code != 200 || !json.Valid(w.Body.Bytes()) {
						t.Fatalf("poll: %d %s", w.Code, w.Body.String())
					}
				}
				c, _ = s.store.GetConversation(t.Context(), id, "anonymous")
				if c.Version != 1 {
					t.Fatal("poll committed twice")
				}
			})
		}
	}
}

func TestServer_ConversationBackgroundCancellationAndRestart(t *testing.T) {
	started := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		started <- struct{}{}
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	defer backend.Close()
	cfg := config.Config{Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
	s := multiSurfaceServer(t, cfg)
	created := postJSON(t, s, "/api/conversations", `{"model":"m"}`)
	id := protocolFixtureStr(protocolFixtureObj(created.Body.Bytes())["id"])
	body := protocolFixtureJSON(map[string]any{"format": dialect.OpenAIResponses, "request": json.RawMessage(conversationFixtureInput(dialect.OpenAIResponses, "WAIT", false))})
	queued := postJSON(t, s, "/api/conversations/"+id+"/generations", string(body))
	jobID := protocolFixtureStr(protocolFixtureObj(queued.Body.Bytes())["id"])
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("not started")
	}
	for range 2 {
		cancel := postJSON(t, s, "/api/conversation-generations/"+jobID+"/cancel", `{}`)
		if cancel.Code != 200 {
			t.Fatalf("cancel: %d", cancel.Code)
		}
	}
	g := awaitBackground(t, s, jobID)
	if g.State != "cancelled" {
		t.Fatalf("state=%s", g.State)
	}
	c, _ := s.store.GetConversation(t.Context(), id, "anonymous")
	if c.Version != 0 {
		t.Fatal("cancelled turn committed")
	}
	orphan := store.ConversationGeneration{ID: "orphan", Owner: "anonymous", ConversationID: id, Model: "m", Epoch: "previous-runtime", Format: string(dialect.OpenAIResponses)}
	if err := s.store.CreateConversationGeneration(context.Background(), orphan); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest("GET", "/api/conversation-generations/orphan", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != 200 || protocolFixtureObj(w.Body.Bytes())["status"] != "interrupted" {
		t.Fatalf("restart: %d %s", w.Code, w.Body.String())
	}
}
