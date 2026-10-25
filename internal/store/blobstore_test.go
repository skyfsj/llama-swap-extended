package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store/auditstore"
)

// readBlob streams a stored body the way the HTTP body endpoint does.
func readBlob(t *testing.T, st *Store, ref string) []byte {
	t.Helper()
	body, size, _, err := st.OpenBody(ref)
	if err != nil {
		t.Fatalf("OpenBody(%s): %v", ref, err)
	}
	if closer, ok := body.(io.Closer); ok {
		defer closer.Close()
	}
	data, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read blob %s: %v", ref, err)
	}
	if int64(len(data)) != size {
		t.Fatalf("blob %s reported size %d but streamed %d", ref, size, len(data))
	}
	return data
}

func newFileBackedStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	return st, dir
}

// A file-backed store externalizes non-empty bodies into the blob store: the
// database row keeps only the hash reference, the file holds the bytes, and the
// detail view publishes that reference plus the logical size so a client can
// stream the body on demand. size_bytes keeps counting the logical payload so
// the byte budget semantics are unchanged.
func TestStore_Blob_RoundTrip(t *testing.T) {
	st, dir := newFileBackedStore(t)
	now := time.Now().Truncate(time.Second)
	reqBody := bytes.Repeat([]byte("media-"), 1024)
	respBody := []byte(`{"answer":"ok"}`)
	if err := st.InsertAuditConversation(context.Background(), AuditConversation{
		ID: "blob-rt", ActivityID: 42, Model: "m", Timestamp: now, RequestBody: reqBody, ResponseBody: respBody,
	}); err != nil {
		t.Fatal(err)
	}
	// Bodies live in the compressed segments, not inline in a row.
	stats, err := st.AuditStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.SegmentBytes == 0 {
		t.Fatal("no segment bytes were written for the stored bodies")
	}
	if stats.StoredBytes >= stats.LogicalBytes {
		t.Fatalf("stored %d byte(s) of %d logical; expected compression", stats.StoredBytes, stats.LogicalBytes)
	}
	if _, err := os.Stat(filepath.Join(dir, "audit", "audit.db")); err != nil {
		t.Fatalf("dedicated audit database missing: %v", err)
	}
	full, found, err := st.GetAuditConversation(context.Background(), "blob-rt")
	if err != nil || !found {
		t.Fatalf("GetAuditConversation found=%v err=%v", found, err)
	}
	// The detail view must not read the bodies back: one response carrying every
	// turn is what made a conversation with attachments slow to open.
	if len(full.RequestBody) != 0 || len(full.ResponseBody) != 0 {
		t.Fatalf("detail view materialized bodies req=%d resp=%d, want references only",
			len(full.RequestBody), len(full.ResponseBody))
	}
	if full.RequestBodyRef == "" || full.ResponseBodyRef == "" {
		t.Fatalf("detail view refs req=%q resp=%q, want blob references", full.RequestBodyRef, full.ResponseBodyRef)
	}
	if full.RequestBodyBytes != int64(len(reqBody)) || full.ResponseBodyBytes != int64(len(respBody)) {
		t.Fatalf("body sizes req=%d resp=%d, want %d/%d",
			full.RequestBodyBytes, full.ResponseBodyBytes, len(reqBody), len(respBody))
	}
	if streamed := readBlob(t, st, full.RequestBodyRef); !bytes.Equal(streamed, reqBody) {
		t.Fatalf("streamed request body len=%d, want %d", len(streamed), len(reqBody))
	}
	if streamed := readBlob(t, st, full.ResponseBodyRef); !bytes.Equal(streamed, respBody) {
		t.Fatalf("streamed response body=%q, want %q", streamed, respBody)
	}
	if full.SizeBytes != int64(len(reqBody)+len(respBody)) {
		t.Fatalf("size_bytes=%d, want %d (logical size)", full.SizeBytes, len(reqBody)+len(respBody))
	}
	// The activity-id view is the one the Web UI uses, and it publishes the same
	// references.
	byActivity, found, err := st.GetAuditConversationByActivityID(context.Background(), full.ActivityID)
	if err != nil || !found {
		t.Fatalf("activity view found=%v err=%v", found, err)
	}
	if byActivity.RequestBodyRef != full.RequestBodyRef || byActivity.ResponseBodyBytes != full.ResponseBodyBytes {
		t.Fatalf("activity view refs req=%q resp=%d, want %q/%d",
			byActivity.RequestBodyRef, byActivity.ResponseBodyBytes, full.RequestBodyRef, full.ResponseBodyBytes)
	}
}

// Identical bodies share one stored copy: retries, batch replays and repeated
// media do not duplicate bytes on disk, and the second write stores nothing.
func TestStore_Blob_Dedup(t *testing.T) {
	st, _ := newFileBackedStore(t)
	now := time.Now().Truncate(time.Second)
	body := bytes.Repeat([]byte("shared-media"), 512)
	var stored int64
	for _, id := range []string{"dup-1", "dup-2"} {
		before, err := st.AuditStats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if err := st.InsertAuditConversation(context.Background(), AuditConversation{
			ID: id, Model: "m", Timestamp: now, RequestBody: body,
		}); err != nil {
			t.Fatal(err)
		}
		after, err := st.AuditStats(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		stored += after.StoredBytes - before.StoredBytes
	}
	stats, err := st.AuditStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if stats.Bodies != 1 {
		t.Fatalf("bodies=%d, want 1 (deduplicated)", stats.Bodies)
	}
	if stored >= int64(len(body)) {
		t.Fatalf("two identical bodies stored %d byte(s) of a %d byte payload; want one copy", stored, len(body))
	}
}

// In-memory stores stay inline: no blob directory is created and the detail
// view reads the body straight from the BLOB column.
func TestStore_Blob_InMemoryStaysInline(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	body := []byte("inline-media")
	if err := st.InsertAuditConversation(context.Background(), AuditConversation{
		ID: "inline", Model: "m", Timestamp: time.Now().Truncate(time.Second), RequestBody: body,
	}); err != nil {
		t.Fatal(err)
	}
	full, found, err := st.GetAuditConversation(context.Background(), "inline")
	if err != nil || !found || !bytes.Equal(full.RequestBody, body) {
		t.Fatalf("in-memory round-trip found=%v err=%v len=%d", found, err, len(full.RequestBody))
	}
}

// Reclaiming space follows the reference counts: a conversation that is gone
// releases its blocks, and compaction rewrites the segments without them while
// the surviving conversation keeps reading its own body.
func TestStore_Blob_GC(t *testing.T) {
	st, _ := newFileBackedStore(t)
	now := time.Now().Truncate(time.Second)
	keep := bytes.Repeat([]byte("keep-me"), 64)
	drop := bytes.Repeat([]byte("drop-me"), 64)
	for _, c := range []AuditConversation{
		{ID: "kept", Model: "m", Timestamp: now, RequestBody: keep},
		{ID: "gone", Model: "m", Timestamp: now, RequestBody: drop},
	} {
		if err := st.InsertAuditConversation(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	before, err := st.AuditStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteAuditConversation(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	// The keep-predicate shields refcount-0 blocks younger than
	// BlockInsertGrace (a fresh delete must not lose bytes a body insert is
	// about to reference): age the row past that window so the pass below
	// actually reclaims instead of being a guaranteed no-op.
	time.Sleep(auditstore.BlockInsertGrace + time.Second)
	reclaimed, dropped, err := st.MaintainAudit(context.Background(), time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if dropped == 0 || reclaimed == 0 {
		t.Fatalf("compaction reclaimed %d byte(s) and dropped %d block(s); want both non-zero", reclaimed, dropped)
	}
	after, err := st.AuditStats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if after.SegmentBytes >= before.SegmentBytes {
		t.Fatalf("segment bytes %d -> %d, want a smaller file", before.SegmentBytes, after.SegmentBytes)
	}
	full, found, err := st.GetAuditConversation(context.Background(), "kept")
	if err != nil || !found {
		t.Fatalf("kept row after compaction found=%v err=%v", found, err)
	}
	if streamed := readBlob(t, st, full.RequestBodyRef); !bytes.Equal(streamed, keep) {
		t.Fatalf("kept row body len=%d, want %d sentinel bytes", len(streamed), len(keep))
	}
}

// A lost segment file must not fail the detail view: the reference is still
// published (so the metadata stays usable) while streaming that body reports
// the bytes as missing instead of pretending they are empty.
func TestStore_Blob_MissingFileDegradesToEmptyBody(t *testing.T) {
	dir := t.TempDir()
	st, err := New(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	if err := st.InsertAuditConversation(context.Background(), AuditConversation{
		ID: "fragile", Model: "m", Timestamp: time.Now().Truncate(time.Second),
		RequestBody: bytes.Repeat([]byte("fragile-media"), 64),
	}); err != nil {
		t.Fatal(err)
	}
	full, found, err := st.GetAuditConversation(context.Background(), "fragile")
	if err != nil || !found {
		t.Fatalf("GetAuditConversation found=%v err=%v", found, err)
	}
	if full.RequestBodyRef == "" {
		t.Fatal("row did not store its body")
	}
	// Lose the segment that carries the body, as a pruned or damaged file
	// would. The store is closed first: its append writer holds the segment
	// open, and Windows cannot unlink a file another process (or the same
	// one) still has open. Reopening after the loss also proves the row
	// survives the restart-shaped version of this failure.
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	segments, err := os.ReadDir(filepath.Join(dir, "audit", "segments"))
	if err != nil {
		t.Fatal(err)
	}
	removed := 0
	for _, entry := range segments {
		if entry.IsDir() {
			continue
		}
		if err := os.Remove(filepath.Join(dir, "audit", "segments", entry.Name())); err != nil {
			t.Fatal(err)
		}
		removed++
	}
	if removed == 0 {
		t.Fatal("no segment file was written")
	}
	st, err = New(filepath.Join(dir, "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := st.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	full, found, err = st.GetAuditConversation(context.Background(), "fragile")
	if err != nil || !found {
		t.Fatalf("GetAuditConversation after loss found=%v err=%v, want usable row", found, err)
	}
	if full.RequestBodyRef == "" {
		t.Fatal("lost body dropped its reference; metadata must stay usable")
	}
	if _, _, _, err := st.OpenBody(full.RequestBodyRef); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("OpenBody(lost segment) err=%v, want ErrBlobNotFound", err)
	}
}
