package auditstore

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("open audit store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func chatTurn(history ...string) []byte {
	return []byte(`{"model":"m","messages":[` + strings.Join(history, ",") + `]}`)
}

func TestStore_RoundTripsBodiesThroughSegments(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("hello ", 300) + `"}]}`)
	response := []byte(`data: {"choices":[{"delta":{"content":"hi"}}]}` + "\n\n")

	saved, err := store.Insert(ctx, Record{
		ID: "1", ActivityID: 1, Model: "m", ReqPath: "/v1/chat/completions",
		Timestamp: time.Now(), RequestBody: body, ResponseBody: response,
		SizeBytes: int64(len(body) + len(response)),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	if saved.RequestBodyRef == "" || saved.ResponseBodyRef == "" {
		t.Fatalf("body references missing: %+v", saved)
	}
	if saved.StoredBytes <= 0 || saved.StoredBytes >= saved.SizeBytes {
		t.Fatalf("stored bytes = %d of %d logical; expected compression", saved.StoredBytes, saved.SizeBytes)
	}

	got, err := store.Materialize(saved.RequestBodyRef)
	if err != nil {
		t.Fatalf("materialize request: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("request body round-trip mismatch:\n got %s\nwant %s", got, body)
	}
	gotResponse, err := store.Materialize(saved.ResponseBodyRef)
	if err != nil {
		t.Fatalf("materialize response: %v", err)
	}
	if !bytes.Equal(gotResponse, response) {
		t.Fatal("response body round-trip mismatch")
	}
}

// The whole point of the design: a session re-sends its history every turn, and
// only the new messages cost storage.
func TestStore_SessionHistoryIsStoredOnce(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	system := `{"role":"system","content":"` + strings.Repeat("long system prompt ", 200) + `"}`

	turnOne := chatTurn(system, `{"role":"user","content":"first question"}`)
	first, err := store.Insert(ctx, Record{
		ID: "1", ActivityID: 1, SessionID: "s1", Timestamp: time.Now(),
		RequestBody: turnOne, SizeBytes: int64(len(turnOne)),
	})
	if err != nil {
		t.Fatalf("insert turn one: %v", err)
	}

	// The second turn replays the system prompt and the first exchange.
	turnTwo := chatTurn(system,
		`{"role":"user","content":"first question"}`,
		`{"role":"assistant","content":"`+strings.Repeat("a long answer ", 200)+`"}`,
		`{"role":"user","content":"second question"}`)
	second, err := store.Insert(ctx, Record{
		ID: "2", ActivityID: 2, SessionID: "s1", Timestamp: time.Now(),
		RequestBody: turnTwo, SizeBytes: int64(len(turnTwo)),
	})
	if err != nil {
		t.Fatalf("insert turn two: %v", err)
	}

	// The shared system prompt and first question were already stored, so the
	// second turn's new bytes are far below its logical size.
	if second.StoredBytes >= int64(len(turnTwo))/2 {
		t.Fatalf("turn two stored %d bytes of %d logical; history was not shared (first turn stored %d)",
			second.StoredBytes, len(turnTwo), first.StoredBytes)
	}

	summary, found, err := store.Session(ctx, "s1")
	if err != nil || !found {
		t.Fatalf("session summary found=%v err=%v", found, err)
	}
	if summary.RequestCount != 2 {
		t.Fatalf("request count = %d, want 2", summary.RequestCount)
	}
	if summary.RequestBytes != int64(len(turnOne)+len(turnTwo)) {
		t.Fatalf("session request bytes = %d, want %d", summary.RequestBytes, len(turnOne)+len(turnTwo))
	}
	if summary.SharedBlockBytes <= 0 {
		t.Fatal("session did not report shared context savings")
	}
}

func TestStore_IdenticalBodiesShareOneCopy(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("same ", 400) + `"}]}`)

	first, err := store.Insert(ctx, Record{ID: "1", ActivityID: 1, Timestamp: time.Now(), RequestBody: body})
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.Insert(ctx, Record{ID: "2", ActivityID: 2, Timestamp: time.Now(), RequestBody: body})
	if err != nil {
		t.Fatal(err)
	}
	if first.RequestBodyRef != second.RequestBodyRef {
		t.Fatalf("identical bodies got different references: %s vs %s", first.RequestBodyRef, second.RequestBodyRef)
	}
	if second.StoredBytes != 0 {
		t.Fatalf("repeated body stored %d new bytes, want 0", second.StoredBytes)
	}
}

func TestStore_ListFiltersAndPages(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now()
	for index, model := range []string{"a", "a", "b"} {
		if _, err := store.Insert(ctx, Record{
			ID: string(rune('1' + index)), ActivityID: index + 1, Model: model, KeyID: "k1",
			SessionID: "s1", ReqPath: "/v1/chat/completions", Timestamp: now.Add(time.Duration(index) * time.Minute),
			RequestBody: []byte(`{"model":"x"}`), SizeBytes: 12,
		}); err != nil {
			t.Fatal(err)
		}
	}

	page, err := store.List(ctx, Query{Model: "a", Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || len(page.Data) != 2 {
		t.Fatalf("model filter returned %d/%d rows, want 2", len(page.Data), page.Total)
	}
	page, err = store.List(ctx, Query{SessionID: "s1", Limit: 2, Page: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 || page.TotalPages != 2 || len(page.Data) != 1 {
		t.Fatalf("paged query = %+v", page)
	}
	// Newest first by default.
	page, err = store.List(ctx, Query{Limit: 10})
	if err != nil {
		t.Fatalf("unfiltered list: %v", err)
	}
	if len(page.Data) == 0 {
		t.Fatalf("unfiltered list returned no rows (total=%d)", page.Total)
	}
	if page.Data[0].Model != "b" {
		t.Fatalf("default order returned %q first, want the newest (b)", page.Data[0].Model)
	}
}

func TestStore_DeleteReleasesBodiesAndBudget(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("payload ", 500) + `"}]}`)

	first, err := store.Insert(ctx, Record{ID: "1", ActivityID: 1, Timestamp: time.Now().Add(-2 * time.Hour), RequestBody: body, SizeBytes: int64(len(body))})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Insert(ctx, Record{ID: "2", ActivityID: 2, Timestamp: time.Now(), RequestBody: body, SizeBytes: int64(len(body))}); err != nil {
		t.Fatal(err)
	}

	// Both conversations reference the same body: deleting one keeps it.
	if err := store.Delete(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize(first.RequestBodyRef); err != nil {
		t.Fatalf("shared body disappeared after deleting one reference: %v", err)
	}
	if err := store.Delete(ctx, "2"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Materialize(first.RequestBodyRef); err == nil {
		t.Fatal("body survived its last reference")
	}

	// Retention and the byte budget remove the remaining rows.
	for index := 0; index < 5; index++ {
		if _, err := store.Insert(ctx, Record{
			ID: string(rune('a' + index)), ActivityID: 100 + index,
			Timestamp:   time.Now().Add(time.Duration(index) * time.Minute),
			RequestBody: []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("x ", 200) + `"}]}`),
			SizeBytes:   500,
		}); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := store.PurgeOverBudget(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if deleted == 0 {
		t.Fatal("byte budget purge removed nothing")
	}
	stats, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Conversations == 0 {
		t.Fatal("budget purge removed every conversation; the newest row must survive")
	}
}

func TestStore_CompactionReclaimsUnreferencedBlocks(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("compress me ", 900) + `"}]}`)
	if _, err := store.Insert(ctx, Record{ID: "1", ActivityID: 1, Timestamp: time.Now(), RequestBody: body, SizeBytes: int64(len(body))}); err != nil {
		t.Fatal(err)
	}
	before, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	reclaimed, dropped, err := store.CompactSegments(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 || reclaimed == 0 {
		t.Fatalf("compaction reclaimed %d bytes / dropped %d blocks; want both non-zero", reclaimed, dropped)
	}
	after, err := store.Stats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Blocks != 0 || after.Bodies != 0 {
		t.Fatalf("index kept %d blocks / %d bodies after compaction", after.Blocks, after.Bodies)
	}
	if after.SegmentBytes >= before.SegmentBytes {
		t.Fatalf("segments did not shrink: %d -> %d", before.SegmentBytes, after.SegmentBytes)
	}
}

func TestStore_HasActivityProbe(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	if _, err := store.Insert(ctx, Record{ID: "7", ActivityID: 7, Timestamp: time.Now(), RequestBody: []byte(`{"a":1}`)}); err != nil {
		t.Fatal(err)
	}
	found, err := store.HasActivity(ctx, []int{7, 8})
	if err != nil {
		t.Fatal(err)
	}
	if !found[7] || found[8] {
		t.Fatalf("probe = %v, want only 7", found)
	}
}
