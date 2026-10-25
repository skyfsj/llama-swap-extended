package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestStore_ConversationGenerationOwnershipAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	g := ConversationGeneration{ID: "ggen", Owner: "owner", ConversationID: "gconv", Model: "m", Epoch: "process", Format: "openai.responses"}
	if err := s.CreateConversationGeneration(t.Context(), g); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversationGeneration(t.Context(), g.ID, "foreign"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("foreign read: %v", err)
	}
	g.State = "in_progress"
	if err := s.UpdateConversationGeneration(t.Context(), g, "queued"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateConversationGeneration(t.Context(), g, "queued"); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("duplicate execution claim: %v", err)
	}
	if err := s.DeleteConversationGeneration(t.Context(), g.ID, g.Owner); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("active deletion: %v", err)
	}
	g.State = "completed"
	g.Result = []byte(`{"id":"response","status":"completed"}`)
	g.HTTPStatus = 200
	if err := s.UpdateConversationGeneration(t.Context(), g, "in_progress"); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	loaded, err := s.GetConversationGeneration(t.Context(), g.ID, g.Owner)
	if err != nil || loaded.State != "completed" || string(loaded.Result) != string(g.Result) {
		t.Fatalf("restart: %+v %v", loaded, err)
	}
	if err := s.DeleteConversationGeneration(t.Context(), g.ID, "foreign"); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("foreign deletion: %v", err)
	}
	if err := s.DeleteConversationGeneration(t.Context(), g.ID, g.Owner); err != nil {
		t.Fatal(err)
	}
}
