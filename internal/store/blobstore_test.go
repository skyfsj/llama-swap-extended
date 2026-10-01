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
)

// readBlob streams a stored body the way the HTTP body endpoint does.
func readBlob(t *testing.T, st *Store, ref string) []byte {
	t.Helper()
	file, size, err := st.OpenBlob(ref)
	if err != nil {
		t.Fatalf("OpenBlob(%s): %v", ref, err)
	}
	defer file.Close()
	data, err := io.ReadAll(file)
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

func countBlobFiles(t *testing.T, dir string) int {
	t.Helper()
	n := 0
	err := filepath.WalkDir(filepath.Join(dir, "blobs"), func(path string, d os.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if !d.IsDir() {
			n++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk blob directory: %v", err)
	}
	return n
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
	var reqInline, respInline int
	if err := st.db.QueryRow(`SELECT length(req_body), length(resp_body) FROM audit_conversations WHERE id='blob-rt'`).Scan(&reqInline, &respInline); err != nil {
		t.Fatal(err)
	}
	if reqInline != 0 || respInline != 0 {
		t.Fatalf("bodies stored inline req=%d resp=%d, want externalized to files", reqInline, respInline)
	}
	if got := countBlobFiles(t, dir); got != 2 {
		t.Fatalf("blob files=%d, want 2", got)
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

// Identical bodies share one blob file: retries, batch replays and repeated
// media do not duplicate bytes on disk.
func TestStore_Blob_Dedup(t *testing.T) {
	st, dir := newFileBackedStore(t)
	now := time.Now().Truncate(time.Second)
	body := bytes.Repeat([]byte("shared-media"), 512)
	for _, id := range []string{"dup-1", "dup-2"} {
		if err := st.InsertAuditConversation(context.Background(), AuditConversation{
			ID: id, Model: "m", Timestamp: now, RequestBody: body,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := countBlobFiles(t, dir); got != 1 {
		t.Fatalf("blob files=%d, want 1 (deduplicated)", got)
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

// Deleting the referencing row and clearing unreferenced files leaves only
// the blobs that still have a live conversation; orphan files with no row at
// all are reclaimed too.
func TestStore_Blob_GC(t *testing.T) {
	st, dir := newFileBackedStore(t)
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
	// A blob with no referencing row, as if the row was deleted out-of-band.
	orphanHash, err := st.blobs.Put([]byte("orphan"))
	if err != nil {
		t.Fatal(err)
	}
	// A fresh blob whose referencing row has not committed yet: GC must skip
	// it (the audit writer stores the blob before the INSERT commits).
	freshHash, err := st.blobs.Put([]byte("fresh-uncommitted"))
	if err != nil {
		t.Fatal(err)
	}
	// Backdate the true orphan past the grace period so it is collectable.
	orphanPath, err := st.blobs.pathFor(orphanHash)
	if err != nil {
		t.Fatal(err)
	}
	stale := time.Now().Add(-2 * time.Hour)
	if err := os.Chtimes(orphanPath, stale, stale); err != nil {
		t.Fatal(err)
	}
	if got := countBlobFiles(t, dir); got != 4 {
		t.Fatalf("blob files before GC=%d, want 4", got)
	}
	if err := st.DeleteAuditConversation(context.Background(), "gone"); err != nil {
		t.Fatal(err)
	}
	deleted, err := st.GCUnusedBlobs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("GC deleted=%d, want 1 (backdated orphan); fresh blob must survive the grace period", deleted)
	}
	if got := countBlobFiles(t, dir); got != 3 {
		t.Fatalf("blob files after GC=%d, want 3", got)
	}
	freshPath, err := st.blobs.pathFor(freshHash)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(freshPath); err != nil {
		t.Fatalf("fresh blob was collected despite the grace period: %v", err)
	}
	full, found, err := st.GetAuditConversation(context.Background(), "kept")
	if err != nil || !found {
		t.Fatalf("kept row after GC found=%v err=%v", found, err)
	}
	if streamed := readBlob(t, st, full.RequestBodyRef); !bytes.Equal(streamed, keep) {
		t.Fatalf("kept row body len=%d, want %d sentinel bytes", len(streamed), len(keep))
	}
}

// A lost blob file must not fail the detail view: the reference is still
// published (so the metadata stays usable) while streaming that body reports
// the blob as missing instead of pretending it is empty.
func TestStore_Blob_MissingFileDegradesToEmptyBody(t *testing.T) {
	st, dir := newFileBackedStore(t)
	if err := st.InsertAuditConversation(context.Background(), AuditConversation{
		ID: "fragile", Model: "m", Timestamp: time.Now().Truncate(time.Second), RequestBody: []byte("fragile-media"),
	}); err != nil {
		t.Fatal(err)
	}
	var ref string
	if err := st.db.QueryRow(`SELECT req_blob FROM audit_conversations WHERE id='fragile'`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref == "" {
		t.Fatal("row did not externalize its body")
	}
	if err := os.Remove(filepath.Join(dir, "blobs", ref[:2], ref)); err != nil {
		t.Fatal(err)
	}
	full, found, err := st.GetAuditConversation(context.Background(), "fragile")
	if err != nil || !found {
		t.Fatalf("GetAuditConversation found=%v err=%v, want usable row", found, err)
	}
	if len(full.RequestBody) != 0 {
		t.Fatalf("lost blob returned an inline body (len=%d), want none", len(full.RequestBody))
	}
	if full.RequestBodyRef != ref {
		t.Fatalf("detail view ref=%q, want %q (metadata must stay usable)", full.RequestBodyRef, ref)
	}
	if _, _, err := st.OpenBlob(ref); !errors.Is(err, ErrBlobNotFound) {
		t.Fatalf("OpenBlob(lost blob) err=%v, want ErrBlobNotFound", err)
	}
}
