package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_ConversationLeasesAndCheckpoints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	c := Conversation{ID: "gconv_one", Owner: "owner", Model: "m", State: []byte(`{"messages":[]}`)}
	if err := s.CreateConversation(ctx, c); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversation(ctx, c.ID, "other"); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("foreign owner read: %v", err)
	}
	if err := s.LeaseConversation(ctx, c.ID, c.Owner, 0, "token", time.Now().Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := s.LeaseConversation(ctx, c.ID, c.Owner, 0, "other", time.Now().Add(time.Minute)); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("concurrent writer admitted: %v", err)
	}
	c.State = []byte(`{"messages":[{"Role":"user","Parts":[{"Kind":"text","Text":"FIRST"}]}]}`)
	if err := s.CommitConversation(ctx, c, "wrong"); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("wrong lease committed: %v", err)
	}
	if err := s.CommitConversation(ctx, c, "token"); err != nil {
		t.Fatal(err)
	}
	if err := s.PutConversationCheckpoint(ctx, ConversationCheckpoint{ID: "gcp_one", ConversationID: c.ID, Owner: c.Owner, Version: 1}); err != nil {
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
	loaded, err := s.GetConversation(ctx, c.ID, c.Owner)
	if err != nil || loaded.Version != 1 {
		t.Fatalf("restart: %+v %v", loaded, err)
	}
	cp, err := s.GetConversationCheckpoint(ctx, "gcp_one", c.Owner)
	if err != nil || string(cp.State) != string(c.State) {
		t.Fatalf("checkpoint changed: %+v %v", cp, err)
	}
	if err := s.LeaseConversation(ctx, c.ID, c.Owner, 0, "stale", time.Now().Add(time.Minute)); !errors.Is(err, ErrConversationConflict) {
		t.Fatalf("stale version admitted: %v", err)
	}
	if err := s.DeleteConversation(ctx, c.ID, c.Owner); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetConversationCheckpoint(ctx, cp.ID, c.Owner); !errors.Is(err, ErrConversationNotFound) {
		t.Fatalf("deleted checkpoint readable: %v", err)
	}
}
