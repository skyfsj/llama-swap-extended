package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/store/auditstore"
)

// auditExternal reports the dedicated audit store when this Store has one. An
// in-memory store keeps audit rows in the operational database: nothing is
// durable there, so the extra database and segment files would only add
// surprises to tests and embedders.
func (s *Store) auditExternal() *auditstore.Store {
	if s == nil {
		return nil
	}
	return s.audit
}

// AuditExternal reports whether the dedicated audit store is active.
func (s *Store) AuditExternal() bool { return s.auditExternal() != nil }

// auditRecord converts the public row struct to the audit store's record.
func auditRecord(record AuditConversation) auditstore.Record {
	return auditstore.Record{
		ID:                  record.ID,
		ActivityID:          record.ActivityID,
		RequestID:           record.RequestID,
		KeyID:               record.KeyID,
		Model:               record.Model,
		SessionID:           record.SessionID,
		ReqPath:             record.ReqPath,
		Timestamp:           record.Timestamp,
		RequestHeaders:      record.RequestHeaders,
		RequestBody:         record.RequestBody,
		ResponseHeaders:     record.ResponseHeaders,
		ResponseBody:        record.ResponseBody,
		ResponseStatus:      record.ResponseStatus,
		InputTokens:         record.InputTokens,
		OutputTokens:        record.OutputTokens,
		CachedTokens:        record.CachedTokens,
		CacheCreationTokens: record.CacheCreationTokens,
		ReasoningTokens:     record.ReasoningTokens,
		CacheHitRatio:       record.CacheHitRatio,
		CacheCreationRatio:  record.CacheCreationRatio,
		RepairApplied:       record.RepairApplied,
		PrefixHash:          record.PrefixHash,
		EstimatedCost:       record.EstimatedCost,
		Complete:            record.Complete,
		SizeBytes:           record.SizeBytes,
	}
}

// auditConversation projects a stored record back onto the public struct.
func auditConversation(record auditstore.Record) AuditConversation {
	return AuditConversation{
		ID:                  record.ID,
		ActivityID:          record.ActivityID,
		RequestID:           record.RequestID,
		KeyID:               record.KeyID,
		Model:               record.Model,
		SessionID:           record.SessionID,
		ReqPath:             record.ReqPath,
		Timestamp:           record.Timestamp,
		RequestHeaders:      record.RequestHeaders,
		ResponseHeaders:     record.ResponseHeaders,
		RequestBodyRef:      record.RequestBodyRef,
		ResponseBodyRef:     record.ResponseBodyRef,
		RequestBodyBytes:    record.RequestBodySize,
		ResponseBodyBytes:   record.ResponseBodySize,
		ResponseStatus:      record.ResponseStatus,
		InputTokens:         record.InputTokens,
		OutputTokens:        record.OutputTokens,
		CachedTokens:        record.CachedTokens,
		CacheCreationTokens: record.CacheCreationTokens,
		ReasoningTokens:     record.ReasoningTokens,
		CacheHitRatio:       record.CacheHitRatio,
		CacheCreationRatio:  record.CacheCreationRatio,
		RepairApplied:       record.RepairApplied,
		PrefixHash:          record.PrefixHash,
		EstimatedCost:       record.EstimatedCost,
		Complete:            record.Complete,
		SizeBytes:           record.SizeBytes,
	}
}

func auditQuery(q AuditQuery) auditstore.Query {
	return auditstore.Query{
		KeyID:     q.KeyID,
		Model:     q.Model,
		Models:    q.Models,
		SessionID: q.SessionID,
		Start:     q.Start,
		End:       q.End,
		Limit:     q.Limit,
		Offset:    q.Offset,
		Page:      q.Page,
		Sort:      q.Sort,
		Order:     q.Order,
	}
}

// OpenBody returns a reader for one stored audit body. The audit store keeps
// bodies compressed in segments, so the bytes are materialized into a reader
// rather than a file descriptor; the HTTP layer streams it the same way.
func (s *Store) OpenBody(ref string) (io.ReadSeeker, int64, time.Time, error) {
	audit := s.auditExternal()
	if audit == nil {
		file, size, err := s.OpenBlob(ref)
		if err != nil {
			return nil, 0, time.Time{}, err
		}
		info, err := file.Stat()
		if err != nil {
			file.Close()
			return nil, 0, time.Time{}, fmt.Errorf("stat audit body: %w", err)
		}
		return file, size, info.ModTime(), nil
	}
	body, err := audit.Materialize(ref)
	if err != nil {
		if errors.Is(err, auditstore.ErrBlockNotFound) {
			return nil, 0, time.Time{}, ErrBlobNotFound
		}
		return nil, 0, time.Time{}, err
	}
	return newBytesReadSeeker(body), int64(len(body)), time.Time{}, nil
}

// attachActivityPhases merges the phase telemetry the detail view's speed
// section needs. The conversation lives in the audit store, but the activity
// row — which owns first-token/decode/duration and the speed curve — is still
// in the operational database, so the fields are read separately and merged
// with the same COALESCE sentinels the previous single-query join produced.
// attachActivityPhasesBatch fills the phase telemetry of many audit
// conversations in one query. It replaces one query per row on the audit list
// path: the operational database has a single connection, so a 500-row page
// used to serialize 500 round trips against every concurrent request.
//
// The per-row semantics are unchanged: a conversation whose activity row is
// missing keeps the -1 sentinels, and each row is matched by its own id.
func (s *Store) attachActivityPhasesBatch(ctx context.Context, conversations []AuditConversation) error {
	if s == nil || s.db == nil || len(conversations) == 0 {
		return nil
	}
	// Every row starts from the sentinels, including the ones with no activity
	// id: a row that stays untouched must read exactly like a row whose
	// activity row is missing.
	for index := range conversations {
		conversations[index].FirstTokenMs = -1
		conversations[index].DecodeMs = -1
	}
	ids := make([]int64, 0, len(conversations))
	seen := make(map[int]struct{}, len(conversations))
	for _, conversation := range conversations {
		if conversation.ActivityID < 1 {
			continue
		}
		if _, duplicate := seen[conversation.ActivityID]; duplicate {
			continue
		}
		seen[conversation.ActivityID] = struct{}{}
		ids = append(ids, int64(conversation.ActivityID))
	}
	if len(ids) == 0 {
		return nil
	}

	// SQLite's default bound-parameter limit is high, but a huge page would
	// still build an impractical statement, so the ids are chunked.
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := min(start+chunk, len(ids))
		batch := ids[start:end]
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(batch)), ",")
		args := make([]any, len(batch))
		for index, id := range batch {
			args[index] = id
		}
		rows, err := s.db.QueryContext(ctx,
			`SELECT id, first_token_ms, decode_ms, duration_ms, speed_timeline FROM activity WHERE id IN (`+placeholders+`)`,
			args...)
		if err != nil {
			return fmt.Errorf("read activity phases for audit conversations: %w", err)
		}
		for rows.Next() {
			var (
				id         int64
				firstToken sql.NullInt64
				decode     sql.NullInt64
				duration   sql.NullInt64
				timeline   sql.NullString
			)
			if err := rows.Scan(&id, &firstToken, &decode, &duration, &timeline); err != nil {
				rows.Close()
				return fmt.Errorf("read activity phases for audit conversations: %w", err)
			}
			for index := range conversations {
				if int64(conversations[index].ActivityID) != id {
					continue
				}
				applyActivityPhases(&conversations[index], firstToken, decode, duration, timeline)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return fmt.Errorf("read activity phases for audit conversations: %w", err)
		}
		rows.Close()
	}
	return nil
}

// applyActivityPhases merges one activity row's phase telemetry into a
// conversation. It is shared by the single-row and batched readers so both
// interpret the columns identically.
func applyActivityPhases(conversation *AuditConversation, firstToken, decode, duration sql.NullInt64, timeline sql.NullString) {
	if conversation == nil {
		return
	}
	conversation.FirstTokenMs = -1
	conversation.DecodeMs = -1
	if firstToken.Valid {
		conversation.FirstTokenMs = normalizeDecodeMs(int(firstToken.Int64))
	}
	if decode.Valid {
		conversation.DecodeMs = normalizeDecodeMs(int(decode.Int64))
	}
	if duration.Valid {
		conversation.DurationMs = int(duration.Int64)
	}
	if timeline.Valid {
		conversation.SpeedTimeline = timeline.String
	}
}

func (s *Store) attachActivityPhases(ctx context.Context, conversation *AuditConversation) error {
	if conversation == nil {
		return nil
	}
	conversation.FirstTokenMs = -1
	conversation.DecodeMs = -1
	if s == nil || s.db == nil || conversation.ActivityID < 1 {
		return nil
	}
	var firstToken, decode, duration sql.NullInt64
	var timeline sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT first_token_ms, decode_ms, duration_ms, speed_timeline FROM activity WHERE id = ?`,
		conversation.ActivityID).Scan(&firstToken, &decode, &duration, &timeline)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read activity phases for audit conversation: %w", err)
	}
	applyActivityPhases(conversation, firstToken, decode, duration, timeline)
	return nil
}

// SessionSummary is the accumulated view of one audit session.
type SessionSummary = auditstore.SessionSummary

// AuditSession returns the accumulated totals of one session.
func (s *Store) AuditSession(ctx context.Context, sessionID string) (SessionSummary, bool, error) {
	audit := s.auditExternal()
	if audit == nil {
		return SessionSummary{}, false, nil
	}
	return audit.Session(normalizeContext(ctx), sessionID)
}

// AuditStats reports the audit store's size summary.
func (s *Store) AuditStats(ctx context.Context) (auditstore.Stats, error) {
	audit := s.auditExternal()
	if audit == nil {
		return auditstore.Stats{}, nil
	}
	return audit.Stats(normalizeContext(ctx))
}

// MaintainAudit reclaims audit storage after retention or budget purges: whole
// expired day segments are unlinked, and the remaining segments are compacted
// so unreferenced blocks release their bytes. It returns bytes reclaimed.
func (s *Store) MaintainAudit(ctx context.Context, before time.Time) (int64, int, error) {
	audit := s.auditExternal()
	if audit == nil {
		return 0, 0, nil
	}
	ctx = normalizeContext(ctx)
	reclaimed, err := audit.DropExpiredSegments(ctx, before)
	if err != nil {
		return reclaimed, 0, err
	}
	more, dropped, err := audit.CompactSegments(ctx)
	if err != nil {
		return reclaimed + more, dropped, err
	}
	return reclaimed + more, dropped, nil
}

// legacyAuditPending reports whether the operational database still holds the
// pre-split audit tables.
func (s *Store) legacyAuditPending(ctx context.Context) (bool, error) {
	if s == nil || s.db == nil {
		return false, nil
	}
	var name string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'audit_conversations'`).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("probe legacy audit table: %w", err)
	}
	return name != "", nil
}

// LegacyAuditPending reports whether the one-time migration still has work.
func (s *Store) LegacyAuditPending(ctx context.Context) bool {
	audit := s.auditExternal()
	if audit == nil {
		return false
	}
	ctx = normalizeContext(ctx)
	if value, found, err := audit.Meta(ctx, auditMetaMigrated); err == nil && found && value != "" {
		return false
	}
	pending, err := s.legacyAuditPending(ctx)
	return err == nil && pending
}

const (
	auditMetaMigrated  = "legacy_migrated_at"
	auditMetaHighwater = "legacy_highwater_rowid"
	legacyAuditBatch   = 500
)

// AuditMigrationProgress describes one migration pass.
type AuditMigrationProgress struct {
	// existing is the conversation count before the migration started, used by
	// the completion check; it is not part of the reported progress.
	existing  int
	Migrated  int
	Remaining int
	Total     int
	Done      bool
	Bytes     int64
}

// MigrateLegacyAudit copies the pre-split audit rows into the dedicated store,
// compressing and chunking their bodies on the way. It is idempotent and
// resumable: a watermark in the audit database's meta table means a restart
// continues where it stopped instead of re-copying everything.
//
// When the copy is complete the legacy table is dropped. The legacy blob
// directory is left for the operator (or removed by the caller) because it is
// the only remaining copy until the audit store has been verified.
func (s *Store) MigrateLegacyAudit(ctx context.Context, progress func(AuditMigrationProgress)) (AuditMigrationProgress, error) {
	audit := s.auditExternal()
	if audit == nil {
		return AuditMigrationProgress{Done: true}, nil
	}
	ctx = normalizeContext(ctx)
	if value, found, err := audit.Meta(ctx, auditMetaMigrated); err == nil && found && value != "" {
		return AuditMigrationProgress{Done: true}, nil
	}
	pending, err := s.legacyAuditPending(ctx)
	if err != nil {
		return AuditMigrationProgress{}, err
	}
	if !pending {
		_ = audit.SetMeta(ctx, auditMetaMigrated, time.Now().UTC().Format(time.RFC3339))
		return AuditMigrationProgress{Done: true}, nil
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_conversations`).Scan(&total); err != nil {
		return AuditMigrationProgress{}, fmt.Errorf("count legacy audit rows: %w", err)
	}
	watermark := int64(0)
	if value, found, err := audit.Meta(ctx, auditMetaHighwater); err == nil && found {
		if _, err := fmt.Sscanf(value, "%d", &watermark); err != nil {
			watermark = 0
		}
	}

	existing, err := audit.Count(ctx)
	if err != nil {
		return AuditMigrationProgress{}, err
	}
	result := AuditMigrationProgress{Total: total}
	result.existing = existing
	for {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		rows, lastRowID, err := s.legacyAuditBatch(ctx, watermark)
		if err != nil {
			return result, err
		}
		if len(rows) == 0 {
			break
		}
		for _, row := range rows {
			bytes, err := s.insertMigrated(ctx, audit, row)
			if err != nil {
				return result, err
			}
			result.Migrated++
			result.Bytes += bytes
		}
		watermark = lastRowID
		if err := audit.SetMeta(ctx, auditMetaHighwater, fmt.Sprintf("%d", watermark)); err != nil {
			return result, err
		}
		result.Remaining = total - result.Migrated
		if progress != nil {
			progress(result)
		}
	}

	// Verify before destroying the source: the audit store must hold at least
	// the rows the migration copied. A mismatch leaves the legacy table and
	// blob files in place so the operator can retry instead of losing history.
	stored, err := audit.Count(ctx)
	if err != nil {
		return result, err
	}
	if want := result.existing + result.Migrated; stored < want {
		return result, fmt.Errorf("audit migration verification failed: copied %d of %d rows but the audit store holds %d (want at least %d); keeping the legacy table",
			result.Migrated, total, stored, want)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE IF EXISTS audit_conversations`); err != nil {
		return result, fmt.Errorf("drop legacy audit table: %w", err)
	}
	if err := audit.SetMeta(ctx, auditMetaMigrated, time.Now().UTC().Format(time.RFC3339)); err != nil {
		return result, err
	}
	result.Done = true
	result.Remaining = 0
	if progress != nil {
		progress(result)
	}
	return result, nil
}

// legacyAuditRow is one row of the pre-split table.
type legacyAuditRow struct {
	RowID             int64
	Record            AuditConversation
	ReqBlob, RespBlob string
}

func (s *Store) legacyAuditBatch(ctx context.Context, after int64) ([]legacyAuditRow, int64, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT rowid, id, activity_id, request_id, key_id, model_id, session_id, req_path, ts_created,
			req_headers_json, req_body, resp_headers_json, resp_body, resp_status_code,
			input_tokens, output_tokens, cached_tokens, cache_creation_tokens, reasoning_tokens,
			cache_hit_ratio, cache_creation_ratio, repair_applied, prefix_hash, estimated_cost, complete,
			size_bytes, req_blob, resp_blob
		FROM audit_conversations WHERE rowid > ? ORDER BY rowid LIMIT ?`, after, legacyAuditBatch)
	if err != nil {
		return nil, after, fmt.Errorf("read legacy audit rows: %w", err)
	}
	defer rows.Close()

	batch := make([]legacyAuditRow, 0, legacyAuditBatch)
	last := after
	for rows.Next() {
		var row legacyAuditRow
		var created int64
		var complete, repairApplied int
		if err := rows.Scan(
			&row.RowID, &row.Record.ID, &row.Record.ActivityID, &row.Record.RequestID, &row.Record.KeyID,
			&row.Record.Model, &row.Record.SessionID, &row.Record.ReqPath, &created,
			&row.Record.RequestHeaders, &row.Record.RequestBody, &row.Record.ResponseHeaders, &row.Record.ResponseBody,
			&row.Record.ResponseStatus, &row.Record.InputTokens, &row.Record.OutputTokens, &row.Record.CachedTokens,
			&row.Record.CacheCreationTokens, &row.Record.ReasoningTokens, &row.Record.CacheHitRatio,
			&row.Record.CacheCreationRatio, &repairApplied, &row.Record.PrefixHash, &row.Record.EstimatedCost,
			&complete, &row.Record.SizeBytes, &row.ReqBlob, &row.RespBlob,
		); err != nil {
			return nil, after, fmt.Errorf("scan legacy audit row: %w", err)
		}
		row.Record.Timestamp = time.Unix(created, 0)
		row.Record.Complete = complete != 0
		row.Record.RepairApplied = repairApplied != 0
		batch = append(batch, row)
		last = row.RowID
	}
	if err := rows.Err(); err != nil {
		return nil, after, fmt.Errorf("read legacy audit rows: %w", err)
	}
	return batch, last, nil
}

// insertMigrated writes one legacy row into the audit store, resolving its
// body from either the inline column or the legacy blob file.
func (s *Store) insertMigrated(ctx context.Context, audit *auditstore.Store, row legacyAuditRow) (int64, error) {
	record := auditRecord(row.Record)
	if len(record.RequestBody) == 0 && row.ReqBlob != "" {
		record.RequestBody = s.legacyBlobBytes(row.ReqBlob)
	}
	if len(record.ResponseBody) == 0 && row.RespBlob != "" {
		record.ResponseBody = s.legacyBlobBytes(row.RespBlob)
	}
	// The inline columns are dropped from the new schema: bodies live in
	// segments, so the request/response bytes are only used to build them.
	record.RequestBody = append([]byte(nil), record.RequestBody...)
	record.ResponseBody = append([]byte(nil), record.ResponseBody...)
	if record.SizeBytes == 0 {
		record.SizeBytes = int64(len(record.RequestBody) + len(record.ResponseBody))
	}
	saved, err := audit.Insert(ctx, record)
	if err != nil {
		return 0, fmt.Errorf("migrate audit row %s: %w", record.ID, err)
	}
	return saved.StoredBytes, nil
}

// legacyBlobBytes reads a body from the legacy blob file store. A missing file
// (already collected) yields no body rather than failing the migration.
func (s *Store) legacyBlobBytes(ref string) []byte {
	if s.blobs == nil {
		return nil
	}
	data, err := s.blobs.Get(ref)
	if err != nil {
		return nil
	}
	return data
}

// PurgeLegacyBlobs removes the pre-split blob directory after a verified
// migration. It reports the number of files removed.
func (s *Store) PurgeLegacyBlobs() (int, error) {
	if s == nil || s.blobs == nil {
		return 0, nil
	}
	dir := s.blobs.dir()
	if _, err := os.Stat(dir); errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	removed := 0
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		if removeErr := os.Remove(path); removeErr != nil {
			return removeErr
		}
		removed++
		return nil
	})
	if err != nil {
		return removed, fmt.Errorf("remove legacy audit blobs: %w", err)
	}
	if err := os.RemoveAll(dir); err != nil {
		return removed, fmt.Errorf("remove legacy audit blob directory: %w", err)
	}
	return removed, nil
}

// bytesReadSeeker adapts a byte slice to the reader the HTTP body endpoint
// streams from.
type bytesReadSeeker struct {
	data   []byte
	offset int64
}

func newBytesReadSeeker(data []byte) *bytesReadSeeker {
	return &bytesReadSeeker{data: data}
}

func (r *bytesReadSeeker) Read(p []byte) (int, error) {
	if r.offset >= int64(len(r.data)) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.offset:])
	r.offset += int64(n)
	return n, nil
}

func (r *bytesReadSeeker) Seek(offset int64, whence int) (int64, error) {
	var absolute int64
	switch whence {
	case io.SeekStart:
		absolute = offset
	case io.SeekCurrent:
		absolute = r.offset + offset
	case io.SeekEnd:
		absolute = int64(len(r.data)) + offset
	default:
		return 0, fmt.Errorf("invalid whence %d", whence)
	}
	if absolute < 0 {
		return 0, fmt.Errorf("negative seek position %d", absolute)
	}
	r.offset = absolute
	return absolute, nil
}

// hasAuditColumn renders the SQL that marks which activity rows have a stored
// conversation. With the dedicated audit store the probe moves to the caller
// (probeAuditPresence): the two databases cannot be joined, and a probe per row
// would be one cross-database round trip per row.
func (s *Store) hasAuditColumn() string {
	if s.auditExternal() != nil {
		return "0"
	}
	return "EXISTS (SELECT 1 FROM audit_conversations AS audit WHERE audit.activity_id = activity.id)"
}

// probeAuditPresence fills the per-row "has a conversation" flag in one query
// against the audit store.
func (s *Store) probeAuditPresence(ctx context.Context, entries []ActivityLogEntry) error {
	if len(entries) == 0 {
		return nil
	}
	audit := s.auditExternal()
	if audit == nil {
		return nil
	}
	ids := make([]int, 0, len(entries))
	for _, entry := range entries {
		if entry.ID > 0 {
			ids = append(ids, entry.ID)
		}
	}
	found, err := audit.HasActivity(normalizeContext(ctx), ids)
	if err != nil {
		return fmt.Errorf("probe audit conversations: %w", err)
	}
	for index := range entries {
		entries[index].HasAudit = found[entries[index].ID]
	}
	return nil
}
