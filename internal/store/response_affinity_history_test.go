package store

import (
	"bytes"
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestStore_ResponseAffinityHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	s, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	record := ResponseAffinity{ResponseID: "resp-history", Model: "m", IdentityID: "owner", Response: []byte(`{"id":"resp-history","status":"completed"}`), Request: []byte(`{"messages":[{"role":"user","content":"secret"}]}`)}
	if err := s.UpsertResponseAffinity(ctx, record); err != nil {
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
	stored, found, err := s.GetResponseAffinity(ctx, record.ResponseID, time.Now())
	if err != nil || !found || !bytes.Equal(stored.Request, record.Request) {
		t.Fatalf("history after restart: %+v found=%t err=%v", stored, found, err)
	}
	record.Request = nil
	if err := s.UpsertResponseAffinity(ctx, record); err != nil {
		t.Fatal(err)
	}
	stored, _, err = s.GetResponseAffinity(ctx, record.ResponseID, time.Now())
	if err != nil || len(stored.Request) == 0 {
		t.Fatalf("history removed by response update: %+v %v", stored, err)
	}
	record.Response = bytes.Repeat([]byte("x"), maxResponseAffinityBytes)
	if err := s.UpsertResponseAffinity(ctx, record); err == nil {
		t.Fatal("combined retained history and response exceeded the store limit")
	}
	record.Response = []byte(`{"id":"resp-history"}`)
	record.IdentityID = "other"
	if err := s.UpsertResponseAffinity(ctx, record); err != nil {
		t.Fatal(err)
	}
	stored, _, err = s.GetResponseAffinity(ctx, record.ResponseID, time.Now())
	if err != nil || len(stored.Request) != 0 {
		t.Fatalf("history transferred to another owner: %+v %v", stored, err)
	}
}
