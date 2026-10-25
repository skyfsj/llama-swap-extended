package auditstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	// Unreferenced blocks younger than BlockInsertGrace are kept: an insert in
	// flight may still be about to reference them. Aging past the grace makes
	// them reclaimable.
	time.Sleep(BlockInsertGrace + time.Second)
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

// rawBody returns a payload planBody classifies as a single raw block, so
// tests can reason about one body = one block = one frame.
func rawBody(tag string) []byte {
	return []byte("raw audit body " + tag + " " + strings.Repeat(tag+" ", 200))
}

// Compaction moves surviving blocks to new offsets. The database is the
// authoritative index a restart reloads, so the new offsets must be persisted
// before the pass returns — skipping that turned every moved block into a
// "decode audit block: invalid input: magic number mismatch" failure after the
// next restart.
func TestStore_CompactionOffsetsSurviveRestart(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	first, err := store.Insert(ctx, Record{ID: "1", ActivityID: 1, Timestamp: time.Now(), RequestBody: rawBody("first"), SizeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	_ = first
	second, err := store.Insert(ctx, Record{ID: "2", ActivityID: 2, Timestamp: time.Now(), RequestBody: rawBody("second"), SizeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// Delete the first conversation: compaction drops its block and moves the
	// second one to the front of the file. Blocks deleted inside the insert
	// grace are kept, so age past it first.
	if err := store.Delete(ctx, "1"); err != nil {
		t.Fatal(err)
	}
	time.Sleep(BlockInsertGrace + time.Second)
	if _, _, err := store.CompactSegments(ctx); err != nil {
		t.Fatal(err)
	}

	// Simulate the restart the in-memory index does not survive.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}

	// The reloaded index comes from the database, so the new offset must have
	// been persisted during compaction. Asserting the row — not the read — is
	// what pins this: the repair scanner would silently fix a stale offset on
	// first read, and the persistence regression would hide behind it.
	var storedOffset int64
	if err := store.db.QueryRow(`SELECT offset FROM blocks WHERE hash = ?`, second.RequestBodyRef).Scan(&storedOffset); err != nil {
		t.Fatal(err)
	}
	if storedOffset != 0 {
		t.Fatalf("compacted block offset = %d in the database, want 0; offsets were not persisted", storedOffset)
	}

	got, err := store.Materialize(second.RequestBodyRef)
	if err != nil {
		t.Fatalf("materialize after compaction + restart: %v", err)
	}
	if !bytes.Equal(got, rawBody("second")) {
		t.Fatalf("body corrupted across compaction + restart: %q", got[:40])
	}
}

// The repair scanner rebuilds stale offsets from the segment frames themselves.
// This simulates the state older builds left behind: a compaction rewrote the
// file but the database kept the pre-rewrite offsets.
func TestStore_RepairRecoversStaleIndex(t *testing.T) {
	dir := t.TempDir()
	store, err := Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	// Three raw bodies, one block each: filler, recoverable, and one whose
	// bytes are destroyed outright.
	filler, err := store.Insert(ctx, Record{ID: "1", ActivityID: 1, Timestamp: time.Now(), RequestBody: rawBody("filler"), SizeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	kept, err := store.Insert(ctx, Record{ID: "2", ActivityID: 2, Timestamp: time.Now(), RequestBody: rawBody("kept"), SizeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}
	lost, err := store.Insert(ctx, Record{ID: "3", ActivityID: 3, Timestamp: time.Now(), RequestBody: rawBody("lost"), SizeBytes: 1})
	if err != nil {
		t.Fatal(err)
	}

	// Record the true layout, then simulate a compaction that dropped the
	// filler and crashed before persisting: the file loses the filler's frame
	// and shifts everything after it forward, while the database rows — and
	// therefore a fresh process — still point at the old positions.
	var fillerOffset, fillerLength int64
	if err := store.db.QueryRow(`SELECT offset, length FROM blocks WHERE hash = ?`, filler.RequestBodyRef).Scan(&fillerOffset, &fillerLength); err != nil {
		t.Fatal(err)
	}
	segmentFile := filepath.Join(dir, "segments", segmentName(time.Now())+".seg")
	raw, err := os.ReadFile(segmentFile)
	if err != nil {
		t.Fatal(err)
	}
	spliced := append(append([]byte{}, raw[:fillerOffset]...), raw[fillerOffset+fillerLength:]...)
	if err := os.WriteFile(segmentFile, spliced, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	// The stale index points kept at the filler's old position, so the first
	// read fails to decode and the repair scanner rebuilds the offsets in the
	// same call — recovery is transparent to the reader.
	got, err := store.Materialize(kept.RequestBodyRef)
	if err != nil {
		t.Fatalf("repair did not recover the shifted block: %v", err)
	}
	if !bytes.Equal(got, rawBody("kept")) {
		t.Fatal("repaired block has wrong content")
	}
	gotLost, err := store.Materialize(lost.RequestBodyRef)
	if err != nil {
		t.Fatalf("surviving block after the lost one was not recovered: %v", err)
	}
	if !bytes.Equal(gotLost, rawBody("lost")) {
		t.Fatal("recovered trailing block has wrong content")
	}

	// The rebuilt offsets are persisted: a fresh process reads without repair.
	var repairedOffset int64
	if err := store.db.QueryRow(`SELECT offset FROM blocks WHERE hash = ?`, kept.RequestBodyRef).Scan(&repairedOffset); err != nil {
		t.Fatal(err)
	}
	if repairedOffset != 0 {
		t.Fatalf("repaired offset = %d, want 0 (the filler's frame was spliced out)", repairedOffset)
	}

	// The filler's bytes are provably absent from the whole file: its row must
	// be gone so reads report a clean miss instead of a decode error.
	if _, err := store.Materialize(filler.RequestBodyRef); !errors.Is(err, ErrBlockNotFound) {
		t.Fatalf("provably missing block: want ErrBlockNotFound, got %v", err)
	}
	var count int
	if err := store.db.QueryRow(`SELECT COUNT(*) FROM blocks WHERE hash = ?`, filler.RequestBodyRef).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("index kept a row whose bytes no longer exist")
	}
}

// Compaction rewrites segment files while the request path keeps appending.
// The append gate must hold: bytes written during a rewrite used to be
// stranded by the rename, leaving rows that pointed into the void.
func TestStore_ConcurrentAppendsDuringCompaction(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	const conversations = 60
	type saved struct {
		ref  string
		body []byte
	}
	bodies := make(map[string]saved)
	var bodiesMu sync.Mutex

	var wg sync.WaitGroup
	errCh := make(chan error, 2)

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < conversations; i++ {
			id := fmt.Sprintf("c%03d", i)
			body := rawBody(id)
			record, err := store.Insert(ctx, Record{ID: id, ActivityID: i + 1, Timestamp: time.Now(), RequestBody: body, SizeBytes: 1})
			if err != nil {
				errCh <- err
				return
			}
			bodiesMu.Lock()
			bodies[id] = saved{ref: record.RequestBodyRef, body: body}
			bodiesMu.Unlock()
			// Delete every third conversation so compaction always has dead
			// blocks to move and rewrite. Blocks deleted inside the insert
			// grace stay put; ones from earlier iterations age out during the
			// run, so rewrites keep happening alongside the appends.
			if i%3 == 0 {
				if err := store.Delete(ctx, id); err != nil {
					errCh <- err
					return
				}
				bodiesMu.Lock()
				delete(bodies, id)
				bodiesMu.Unlock()
			}
			time.Sleep(30 * time.Millisecond)
		}
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 12; i++ {
			if _, _, err := store.CompactSegments(ctx); err != nil {
				errCh <- err
				return
			}
			time.Sleep(250 * time.Millisecond)
		}
	}()

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
	bodiesMu.Lock()
	defer bodiesMu.Unlock()

	if _, _, err := store.CompactSegments(ctx); err != nil {
		t.Fatal(err)
	}
	for id, want := range bodies {
		got, err := store.Materialize(want.ref)
		if err != nil {
			t.Fatalf("conversation %s unreadable after concurrent compaction: %v", id, err)
		}
		if !bytes.Equal(got, want.body) {
			t.Fatalf("conversation %s corrupted by concurrent compaction", id)
		}
	}
}

// The append gate is what makes compaction safe: a put targeting a segment
// under rewrite must block until the pass releases the segment, because the
// rename would otherwise strand bytes the writer already recorded. This pins
// the blocking behavior directly — the stress test above cannot hit the
// sub-millisecond window reliably.
func TestStore_AppendWaitsForCompactionGate(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()

	if _, err := store.Insert(ctx, Record{ID: "1", ActivityID: 1, Timestamp: time.Now(), RequestBody: rawBody("seed"), SizeBytes: 1}); err != nil {
		t.Fatal(err)
	}

	segment := segmentName(time.Now())
	store.segments.mu.Lock()
	store.segments.compacting[segment] = true
	store.segments.mu.Unlock()

	var gatedRef string
	blocked := make(chan struct{}, 1)
	go func() {
		record, err := store.Insert(ctx, Record{ID: "2", ActivityID: 2, Timestamp: time.Now(), RequestBody: rawBody("gated"), SizeBytes: 1})
		if err != nil {
			t.Error(err)
		}
		gatedRef = record.RequestBodyRef
		blocked <- struct{}{}
	}()

	select {
	case <-blocked:
		t.Fatal("put completed while its segment was being compacted; the append gate did not hold")
	case <-time.After(150 * time.Millisecond):
		// Still waiting: the gate is holding the append.
	}

	store.segments.mu.Lock()
	delete(store.segments.compacting, segment)
	store.segments.appendGate.Broadcast()
	store.segments.mu.Unlock()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("put never completed after the gate released")
	}

	got, err := store.Materialize(gatedRef)
	if err != nil {
		t.Fatalf("gated body unreadable after compaction released: %v", err)
	}
	if !bytes.Equal(got, rawBody("gated")) {
		t.Fatal("gated body corrupted")
	}
}
