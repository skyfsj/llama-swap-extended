package store

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store/auditstore"
)

// legacyRow writes one row into the pre-split table the way the old binary
// would have, including an externalized body in the legacy blob directory.
func legacyRow(t *testing.T, st *Store, id string, activityID int, at time.Time, body []byte, external bool) string {
	t.Helper()
	reqBlob := ""
	inline := body
	if external && st.blobs != nil {
		ref, err := st.blobs.Put(body)
		if err != nil {
			t.Fatal(err)
		}
		reqBlob = ref
		// The legacy schema keeps req_body NOT NULL; an externalized row wrote
		// an empty blob there.
		inline = []byte{}
	}
	if _, err := st.db.Exec(`
		INSERT INTO audit_conversations (id, activity_id, key_id, model_id, session_id, req_path, ts_created,
			req_headers_json, req_body, resp_headers_json, resp_body, resp_status_code,
			input_tokens, output_tokens, estimated_cost, complete, size_bytes, req_blob, resp_blob)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		id, activityID, "k1", "m", "s1", "/v1/chat/completions", at.Unix(),
		`{"a":"b"}`, inline, `{}`, []byte{}, 200, 10, 5, 0.25, 1, len(body), reqBlob, ""); err != nil {
		t.Fatal(err)
	}
	return reqBlob
}

// The one-time migration copies the pre-split rows into the audit store with
// their bodies compressed, resolves externalized bodies from the legacy blob
// directory, and drops the legacy table once the copy is complete.
func TestStore_MigrateLegacyAudit(t *testing.T) {
	st, dir := newFileBackedStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	shared := []byte(`{"model":"m","messages":[{"role":"user","content":"` + strings.Repeat("history ", 300) + `"}]}`)

	legacyRow(t, st, "1", 1, now.Add(-3*time.Minute), shared, false)
	legacyRow(t, st, "2", 2, now.Add(-2*time.Minute), shared, true) // same body, externalized
	legacyRow(t, st, "3", 3, now.Add(-1*time.Minute), []byte("small"), true)

	if !st.LegacyAuditPending(ctx) {
		t.Fatal("migration not reported as pending")
	}
	progress, err := st.MigrateLegacyAudit(ctx, nil)
	if err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if progress.Migrated != 3 || !progress.Done {
		t.Fatalf("progress = %+v, want 3 migrated and done", progress)
	}
	if st.LegacyAuditPending(ctx) {
		t.Fatal("migration still reported as pending after completion")
	}

	// Every row resolves, and the two identical bodies share one stored copy.
	for _, id := range []string{"1", "2", "3"} {
		conversation, found, err := st.GetAuditConversation(ctx, id)
		if err != nil || !found {
			t.Fatalf("migrated row %s found=%v err=%v", id, found, err)
		}
		if conversation.SessionID != "s1" || conversation.InputTokens != 10 {
			t.Fatalf("migrated row %s lost metadata: %+v", id, conversation)
		}
		body, _, _, err := st.OpenBody(conversation.RequestBodyRef)
		if err != nil {
			t.Fatalf("migrated row %s body unreadable: %v", id, err)
		}
		read, _ := io.ReadAll(body)
		if len(read) == 0 {
			t.Fatalf("migrated row %s has an empty body", id)
		}
	}
	first, _, _ := st.GetAuditConversation(ctx, "1")
	second, _, _ := st.GetAuditConversation(ctx, "2")
	if first.RequestBodyRef != second.RequestBodyRef {
		t.Fatal("identical legacy bodies were not deduplicated during migration")
	}
	rebuilt, _, _, err := st.OpenBody(first.RequestBodyRef)
	if err != nil {
		t.Fatal(err)
	}
	read, _ := io.ReadAll(rebuilt)
	if !bytes.Equal(read, shared) {
		t.Fatalf("migrated body differs from the original (%d vs %d bytes)", len(read), len(shared))
	}

	// The legacy table and its blob files are gone; the audit directory holds
	// the compressed copy.
	var name string
	err = st.db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name='audit_conversations'`).Scan(&name)
	if err == nil {
		t.Fatal("legacy audit table still exists after migration")
	}
	if _, err := os.Stat(filepath.Join(dir, "audit", "audit.db")); err != nil {
		t.Fatalf("audit database missing: %v", err)
	}
	stats, err := st.AuditStats(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if stats.StoredBytes >= stats.LogicalBytes {
		t.Fatalf("migrated %d byte(s) of %d logical; expected compression and dedup", stats.StoredBytes, stats.LogicalBytes)
	}
}

// A restart in the middle of the migration resumes from the watermark instead
// of re-copying rows that are already stored.
func TestStore_MigrateLegacyAuditResumesFromWatermark(t *testing.T) {
	st, _ := newFileBackedStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	for index := 1; index <= 3; index++ {
		legacyRow(t, st, string(rune('0'+index)), index, now, []byte(strings.Repeat("x", 100+index)), false)
	}
	// Simulate an interrupted first pass: row 1 was copied and the watermark
	// advanced, row 2 crashed before its insert.
	if _, err := st.audit.Insert(ctx, auditstore.Record{
		ID: "1", ActivityID: 1, Model: "m", Timestamp: now, RequestBody: []byte(strings.Repeat("x", 101)),
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.audit.SetMeta(ctx, "legacy_highwater_rowid", "1"); err != nil {
		t.Fatal(err)
	}

	progress, err := st.MigrateLegacyAudit(ctx, nil)
	if err != nil {
		t.Fatalf("resume migration: %v", err)
	}
	if progress.Migrated != 2 {
		t.Fatalf("resumed migration copied %d row(s), want the 2 past the watermark", progress.Migrated)
	}
	page, err := st.ListAuditConversationsPage(ctx, AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 {
		t.Fatalf("rows after resume = %d, want 3", page.Total)
	}
}

// Activity rows have their own retention: trimming them must not touch the
// conversations, whose detail view degrades gracefully without the phase row.
func TestStore_PurgeActivityBefore(t *testing.T) {
	st, _ := newFileBackedStore(t)
	ctx := context.Background()
	now := time.Now().Truncate(time.Second)
	stale := now.Add(-48 * time.Hour)
	for index, at := range []time.Time{stale, stale, now} {
		entry, err := st.InsertActivity(ctx, ActivityLogEntry{
			Timestamp: at, Model: "m", ReqPath: "/v1/chat/completions", RespStatusCode: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
		if index == 1 {
			if err := st.InsertAuditConversation(ctx, AuditConversation{
				ID: "kept", ActivityID: entry.ID, Model: "m", Timestamp: at, RequestBody: []byte(`{"a":1}`),
			}); err != nil {
				t.Fatal(err)
			}
		}
	}
	deleted, err := st.PurgeActivityBefore(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 2 {
		t.Fatalf("purged %d activity row(s), want 2", deleted)
	}
	page, err := st.ListActivity(ctx, ActivityQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Data) != 1 {
		t.Fatalf("activity rows after purge = %d, want 1", len(page.Data))
	}
	// The conversation survives, and its detail view still loads with the
	// phase sentinels the missing activity row implies.
	conversation, found, err := st.GetAuditConversation(ctx, "kept")
	if err != nil || !found {
		t.Fatalf("conversation after activity purge found=%v err=%v", found, err)
	}
	if conversation.FirstTokenMs != -1 || conversation.DecodeMs != -1 || conversation.DurationMs != 0 {
		t.Fatalf("phase sentinels = %d/%d/%d, want -1/-1/0",
			conversation.FirstTokenMs, conversation.DecodeMs, conversation.DurationMs)
	}
}
