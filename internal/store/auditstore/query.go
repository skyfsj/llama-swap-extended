package auditstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// sortColumns whitelists the columns a listing may order by. It mirrors the
// legacy audit store so the HTTP layer's sort keys keep working.
var sortColumns = map[string]string{
	"id":               "id",
	"time":             "ts_created",
	"model":            "model_id",
	"req_path":         "req_path",
	"resp_status_code": "resp_status_code",
	"cached":           "cached_tokens",
	"cache_creation":   "cache_creation_tokens",
	"cache_hit_ratio":  "cache_hit_ratio",
	"prompt":           "input_tokens",
	"generated":        "output_tokens",
}

const (
	defaultLimit = 25
	maxLimit     = 500
)

func normalizeLimitOffset(limit, offset int) (int, int) {
	if limit <= 0 {
		limit = defaultLimit
	}
	if limit > maxLimit {
		limit = maxLimit
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func (q Query) orderBy() string {
	column, ok := sortColumns[strings.ToLower(strings.TrimSpace(q.Sort))]
	if !ok {
		column = "ts_created"
	}
	direction := "DESC"
	if strings.EqualFold(strings.TrimSpace(q.Order), "asc") {
		direction = "ASC"
	}
	if column == "ts_created" {
		return fmt.Sprintf("ORDER BY ts_created %s, id %s", direction, direction)
	}
	return fmt.Sprintf("ORDER BY %s %s, ts_created DESC, id DESC", column, direction)
}

// where builds the filter clause shared by list and count.
func (q Query) where() (string, []any) {
	clauses := make([]string, 0, 6)
	args := make([]any, 0, 8)
	if q.KeyID != "" {
		clauses = append(clauses, "key_id = ?")
		args = append(args, q.KeyID)
	}
	if q.Model != "" {
		clauses = append(clauses, "model_id = ?")
		args = append(args, q.Model)
	} else if len(q.Models) > 0 {
		placeholders := make([]string, len(q.Models))
		for index, model := range q.Models {
			placeholders[index] = "?"
			args = append(args, model)
		}
		clauses = append(clauses, "model_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if q.SessionID != "" {
		clauses = append(clauses, "session_id = ?")
		args = append(args, q.SessionID)
	}
	if !q.Start.IsZero() {
		clauses = append(clauses, "ts_created >= ?")
		args = append(args, q.Start.Unix())
	}
	if !q.End.IsZero() {
		clauses = append(clauses, "ts_created <= ?")
		args = append(args, q.End.Unix())
	}
	if len(clauses) == 0 {
		return "", args
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

const conversationColumns = `id, activity_id, request_id, key_id, model_id, session_id, req_path, ts_created,
	req_headers, resp_headers, req_body_ref, resp_body_ref, req_body_size, resp_body_size,
	resp_status_code, input_tokens, output_tokens, cached_tokens, cache_creation_tokens,
	reasoning_tokens, cache_hit_ratio, cache_creation_ratio, repair_applied, prefix_hash,
	estimated_cost, complete, size_bytes, stored_bytes`

// List returns one page of conversations without their bodies: the bytes are
// fetched on demand by MaterializeBody, which is what keeps a page render from
// carrying megabytes of transcript.
func (s *Store) List(ctx context.Context, q Query) (Page, error) {
	if s == nil || s.db == nil {
		return Page{}, errors.New("audit store is unavailable")
	}
	clause, args := q.where()
	limit, offset := normalizeLimitOffset(q.Limit, q.Offset)
	if q.Page > 1 {
		offset = (q.Page - 1) * limit
	}

	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`+clause, args...).Scan(&total); err != nil {
		return Page{}, fmt.Errorf("count audit conversations: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `SELECT `+conversationColumns+` FROM conversations`+clause+` `+q.orderBy()+` LIMIT ? OFFSET ?`,
		append(append([]any{}, args...), limit, offset)...)
	if err != nil {
		return Page{}, fmt.Errorf("list audit conversations: %w", err)
	}
	defer rows.Close()

	records := make([]Record, 0, limit)
	for rows.Next() {
		record, err := scanConversation(rows)
		if err != nil {
			return Page{}, err
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return Page{}, fmt.Errorf("read audit conversations: %w", err)
	}

	page := Page{Data: records, Page: q.Page, Limit: limit, Total: total}
	if page.Page < 1 {
		page.Page = 1
	}
	page.TotalPages = (total + limit - 1) / limit
	return page, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanConversation(scanner rowScanner) (Record, error) {
	var record Record
	var created int64
	var complete, repairApplied int
	if err := scanner.Scan(
		&record.ID, &record.ActivityID, &record.RequestID, &record.KeyID, &record.Model, &record.SessionID,
		&record.ReqPath, &created, &record.RequestHeaders, &record.ResponseHeaders,
		&record.RequestBodyRef, &record.ResponseBodyRef, &record.RequestBodySize, &record.ResponseBodySize,
		&record.ResponseStatus, &record.InputTokens, &record.OutputTokens, &record.CachedTokens,
		&record.CacheCreationTokens, &record.ReasoningTokens, &record.CacheHitRatio, &record.CacheCreationRatio,
		&repairApplied, &record.PrefixHash, &record.EstimatedCost, &complete, &record.SizeBytes, &record.StoredBytes,
	); err != nil {
		return Record{}, fmt.Errorf("scan audit conversation: %w", err)
	}
	record.Timestamp = time.Unix(created, 0)
	record.Complete = complete != 0
	record.RepairApplied = repairApplied != 0
	return record, nil
}

// Get returns one conversation by its internal id.
func (s *Store) Get(ctx context.Context, id string) (Record, bool, error) {
	return s.get(ctx, "id = ?", strings.TrimSpace(id))
}

// GetByActivityID returns one conversation by its activity row id, which is the
// identifier the UI uses.
func (s *Store) GetByActivityID(ctx context.Context, activityID int) (Record, bool, error) {
	if activityID < 1 {
		return Record{}, false, nil
	}
	return s.get(ctx, "activity_id = ?", activityID)
}

func (s *Store) get(ctx context.Context, where string, arg any) (Record, bool, error) {
	if s == nil || s.db == nil {
		return Record{}, false, errors.New("audit store is unavailable")
	}
	row := s.db.QueryRowContext(ctx, `SELECT `+conversationColumns+` FROM conversations WHERE `+where, arg)
	record, err := scanConversation(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Record{}, false, nil
	}
	if err != nil {
		return Record{}, false, err
	}
	return record, true, nil
}

// HasActivity reports which of the supplied activity ids have an audit record.
// The activity list needs this per row; a single query keeps it from becoming a
// probe per row across two databases.
func (s *Store) HasActivity(ctx context.Context, activityIDs []int) (map[int]bool, error) {
	found := make(map[int]bool, len(activityIDs))
	if s == nil || s.db == nil || len(activityIDs) == 0 {
		return found, nil
	}
	sorted := append([]int(nil), activityIDs...)
	sort.Ints(sorted)
	placeholders := make([]string, 0, len(sorted))
	args := make([]any, 0, len(sorted))
	for _, id := range sorted {
		placeholders = append(placeholders, "?")
		args = append(args, id)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT activity_id FROM conversations WHERE activity_id IN (`+strings.Join(placeholders, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("probe audit activity ids: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan audit activity id: %w", err)
		}
		found[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read audit activity ids: %w", err)
	}
	return found, nil
}

// Delete removes one conversation and releases the bodies it referenced.
func (s *Store) Delete(ctx context.Context, id string) error {
	if s == nil || s.db == nil {
		return errors.New("audit store is unavailable")
	}
	return s.deleteWhere(ctx, "id = ?", strings.TrimSpace(id))
}

// Clear removes every conversation. The segments are compacted by the next
// maintenance pass, which reclaims the space.
func (s *Store) Clear(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("audit store is unavailable")
	}
	return s.deleteWhere(ctx, "1 = 1")
}

// PurgeBefore removes conversations older than the cutoff.
func (s *Store) PurgeBefore(ctx context.Context, before time.Time) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("audit store is unavailable")
	}
	return s.deleteCount(ctx, "ts_created < ?", before.Unix())
}

// PurgeOverBudget removes the oldest conversations until the stored bytes fit
// the budget. The measurement is the compressed on-disk size, so the budget
// matches what du reports rather than the logical payload size.
func (s *Store) PurgeOverBudget(ctx context.Context, maxBytes int64) (int, error) {
	if s == nil || s.db == nil || maxBytes <= 0 {
		return 0, nil
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(stored_bytes),0) FROM conversations`).Scan(&total); err != nil {
		return 0, fmt.Errorf("sum audit stored bytes: %w", err)
	}
	if total <= maxBytes {
		return 0, nil
	}
	// Walk the oldest rows accumulating their stored size until the remainder
	// fits the budget, then delete exactly that prefix.
	rows, err := s.db.QueryContext(ctx, `SELECT id, stored_bytes FROM conversations ORDER BY ts_created ASC, id ASC`)
	if err != nil {
		return 0, fmt.Errorf("scan audit conversations for budget: %w", err)
	}
	var doomed []string
	remaining := total
	for rows.Next() {
		var id string
		var stored int64
		if err := rows.Scan(&id, &stored); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan audit conversation for budget: %w", err)
		}
		if remaining <= maxBytes {
			break
		}
		doomed = append(doomed, id)
		remaining -= stored
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read audit conversations for budget: %w", err)
	}
	if len(doomed) == 0 {
		return 0, nil
	}
	deleted := 0
	for _, id := range doomed {
		if err := s.deleteWhere(ctx, "id = ?", id); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// StoredBytes reports the compressed size of the referenced bodies plus the
// segment bytes not yet attributed to any row (blocks written but pruned).
func (s *Store) StoredBytes(ctx context.Context) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("audit store is unavailable")
	}
	var total int64
	if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(stored_bytes),0) FROM conversations`).Scan(&total); err != nil {
		return 0, fmt.Errorf("sum audit stored bytes: %w", err)
	}
	return total, nil
}

// deleteWhere removes rows matching the clause and releases body references,
// deleting bodies (and their block references) that no conversation uses
// anymore.
func (s *Store) deleteWhere(ctx context.Context, where string, args ...any) error {
	_, err := s.deleteCount(ctx, where, args...)
	return err
}

func (s *Store) deleteCount(ctx context.Context, where string, args ...any) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin audit delete: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `SELECT req_body_ref, resp_body_ref FROM conversations WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("select audit bodies to release: %w", err)
	}
	refs := make(map[string]int)
	for rows.Next() {
		var reqRef, respRef string
		if err := rows.Scan(&reqRef, &respRef); err != nil {
			rows.Close()
			return 0, fmt.Errorf("scan audit body reference: %w", err)
		}
		if reqRef != "" {
			refs[reqRef]++
		}
		if respRef != "" {
			refs[respRef]++
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("read audit body references: %w", err)
	}

	res, err := tx.ExecContext(ctx, `DELETE FROM conversations WHERE `+where, args...)
	if err != nil {
		return 0, fmt.Errorf("delete audit conversations: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("delete audit conversations: %w", err)
	}

	for hash, count := range refs {
		if err := releaseBodyTx(ctx, tx, hash, count); err != nil {
			return int(deleted), err
		}
	}
	if err := tx.Commit(); err != nil {
		return int(deleted), fmt.Errorf("commit audit delete: %w", err)
	}
	return int(deleted), nil
}

// releaseBodyTx drops references to a body; when the last one goes, the body
// row disappears and its blocks lose a reference.
func releaseBodyTx(ctx context.Context, tx *sql.Tx, hash string, count int) error {
	if hash == "" || count <= 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `UPDATE bodies SET refcount = refcount - ? WHERE hash = ?`, count, hash); err != nil {
		return fmt.Errorf("release audit body: %w", err)
	}
	var descriptor []byte
	var refcount int
	err := tx.QueryRowContext(ctx, `SELECT descriptor, refcount FROM bodies WHERE hash = ?`, hash).Scan(&descriptor, &refcount)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read audit body for release: %w", err)
	}
	if refcount > 0 {
		return nil
	}
	decoded := mustUnmarshal(descriptor)
	blockHashes := decoded.Blocks
	if decoded.Template != "" {
		blockHashes = append([]string{decoded.Template}, blockHashes...)
	}
	for _, blockHash := range blockHashes {
		if _, err := tx.ExecContext(ctx, `UPDATE blocks SET refcount = refcount - 1 WHERE hash = ?`, blockHash); err != nil {
			return fmt.Errorf("release audit block: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM bodies WHERE hash = ?`, hash); err != nil {
		return fmt.Errorf("delete audit body: %w", err)
	}
	return nil
}

// Materialize reconstructs a body from its stored blocks.
func (s *Store) Materialize(hash string) ([]byte, error) {
	if s == nil || s.db == nil {
		return nil, errors.New("audit store is unavailable")
	}
	hash = strings.TrimSpace(hash)
	if hash == "" {
		return nil, ErrBlockNotFound
	}
	var descriptor []byte
	err := s.db.QueryRow(`SELECT descriptor FROM bodies WHERE hash = ?`, hash).Scan(&descriptor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrBlockNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("read audit body descriptor: %w", err)
	}
	decoded := mustUnmarshal(descriptor)

	if decoded.Kind != kindChat || decoded.Template == "" {
		if len(decoded.Blocks) == 0 {
			return nil, nil
		}
		return s.blockData(decoded.Blocks[0])
	}
	template, err := s.blockData(decoded.Template)
	if err != nil {
		return nil, err
	}
	blocks := make([][]byte, 0, len(decoded.Blocks))
	for _, blockHash := range decoded.Blocks {
		data, err := s.blockData(blockHash)
		if err != nil {
			return nil, err
		}
		blocks = append(blocks, data)
	}
	body, err := reassemble(template, blocks)
	if err != nil {
		return nil, err
	}
	if decoded.Size > 0 && len(body) != decoded.Size {
		return nil, fmt.Errorf("audit body %s has %d bytes, want %d", hash, len(body), decoded.Size)
	}
	return body, nil
}

// BodySize reports the logical size of a stored body.
func (s *Store) BodySize(hash string) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("audit store is unavailable")
	}
	var size int64
	err := s.db.QueryRow(`SELECT size FROM bodies WHERE hash = ?`, strings.TrimSpace(hash)).Scan(&size)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrBlockNotFound
	}
	if err != nil {
		return 0, fmt.Errorf("read audit body size: %w", err)
	}
	return size, nil
}

// blockData reads one block, consulting the in-memory index first and the
// database second (a block written by another process instance).
//
// A decode failure means the index disagrees with the segment file — a stale
// offset from a compaction that crashed before its index update, or one written
// before offsets were persisted at all. One repair pass per segment rebuilds
// the misplaced offsets from the frames themselves; blocks are
// content-addressed, so every placement is verified against the recorded hash.
func (s *Store) blockData(hash string) ([]byte, error) {
	ref, ok := s.segments.lookup(hash)
	if !ok {
		var loaded blockRef
		err := s.db.QueryRow(`SELECT hash, size, compressed_size, segment, offset, length FROM blocks WHERE hash = ?`, hash).
			Scan(&loaded.Hash, &loaded.Size, &loaded.Compressed, &loaded.Segment, &loaded.Offset, &loaded.Length)
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrBlockNotFound
		}
		if err != nil {
			return nil, fmt.Errorf("read audit block index: %w", err)
		}
		s.segments.loadIndex([]blockRef{loaded})
		ref = loaded
	}
	data, err := s.segments.get(ref)
	if err == nil {
		return data, nil
	}
	if errors.Is(err, ErrBlockNotFound) {
		return nil, ErrBlockNotFound
	}
	if !s.repairSegment(ref.Segment) {
		return nil, err
	}
	// The repair either relocated the block (retry through the fresh in-memory
	// ref) or proved its bytes are gone (clean miss instead of a decode error).
	updated, ok := s.segments.lookup(hash)
	if !ok {
		return nil, ErrBlockNotFound
	}
	return s.segments.get(updated)
}

// auditRepairCooldown spaces out repair attempts per segment so a segment that
// cannot be fixed costs one scan per minute, not one per read.
const auditRepairCooldown = time.Minute

// repairSegment rebuilds the offsets of one segment from its frames. It
// reports whether the index changed. Attempts are serialized against
// compaction (both rewrite the same file) and rate-limited per segment.
func (s *Store) repairSegment(name string) bool {
	if s == nil || s.db == nil || strings.TrimSpace(name) == "" {
		return false
	}
	s.repairMu.Lock()
	defer s.repairMu.Unlock()
	if time.Since(s.repairAttempted[name]) < auditRepairCooldown {
		return false
	}
	s.repairAttempted[name] = time.Now()
	defer s.segments.lockCompaction()()

	ordered, err := s.segmentBlocksByCreation(name)
	if err != nil || len(ordered) == 0 {
		return false
	}
	found, missing, err := s.segments.repairIndex(name, ordered)
	if err != nil {
		return false
	}
	if len(found) == 0 && len(missing) == 0 {
		return false
	}
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return false
	}
	defer func() { _ = tx.Rollback() }()
	for _, ref := range found {
		if _, err := tx.Exec(`UPDATE blocks SET offset = ?, length = ? WHERE hash = ?`, ref.Offset, ref.Length, ref.Hash); err != nil {
			return false
		}
	}
	for _, hash := range missing {
		if _, err := tx.Exec(`DELETE FROM blocks WHERE hash = ? AND segment = ?`, hash, name); err != nil {
			return false
		}
	}
	if err := tx.Commit(); err != nil {
		return false
	}
	if len(found) > 0 {
		s.segments.loadIndex(found)
	}
	if len(missing) > 0 {
		s.segments.forget(missing)
	}
	return true
}

// segmentBlocksByCreation loads one segment's block references in file order:
// compaction preserves creation order and appends only grow the file, so
// creation time (then the recorded offset) is the order the frames appear in.
func (s *Store) segmentBlocksByCreation(name string) ([]blockRef, error) {
	rows, err := s.db.Query(`SELECT hash, size, compressed_size, segment, offset, length, first_seen FROM blocks WHERE segment = ?`, name)
	if err != nil {
		return nil, fmt.Errorf("list audit blocks for repair: %w", err)
	}
	defer rows.Close()
	type indexed struct {
		blockRef
		firstSeen int64
	}
	refs := make([]indexed, 0, 64)
	for rows.Next() {
		var entry indexed
		if err := rows.Scan(&entry.Hash, &entry.Size, &entry.Compressed, &entry.Segment, &entry.Offset, &entry.Length, &entry.firstSeen); err != nil {
			return nil, fmt.Errorf("scan audit blocks for repair: %w", err)
		}
		refs = append(refs, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read audit blocks for repair: %w", err)
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].firstSeen != refs[j].firstSeen {
			return refs[i].firstSeen < refs[j].firstSeen
		}
		return refs[i].Offset < refs[j].Offset
	})
	ordered := make([]blockRef, len(refs))
	for i, entry := range refs {
		ordered[i] = entry.blockRef
	}
	return ordered, nil
}

// BlockInsertGrace shields blocks whose bookkeeping is still in flight from a
// concurrent compaction. A block's bytes land in the segment before its index
// row exists, and the row is committed with refcount 0 before the referencing
// body bumps it to 1 — both windows span only the milliseconds of one Insert,
// but a compaction pass running in that window would otherwise drop bytes a
// committed transaction is about to reference. The cost is that freshly
// deleted blocks wait out the grace before their bytes are reclaimed, which
// the 10-minute maintenance cadence never notices. It is exported because the
// window is observable behaviour: callers (and tests) that want a compaction
// pass to actually reclaim freshly deleted bytes must age the data past it.
const BlockInsertGrace = 2 * time.Second

// CompactSegments rewrites the segment files keeping only referenced blocks and
// removes segments whose day has fully expired. It returns reclaimed bytes and
// the number of dropped blocks.
//
// Compaction moves the surviving blocks to new offsets, so the references the
// database recorded before the rewrite are stale the moment the new file
// lands. They are persisted here, before the call returns: the in-memory index
// keeps reads working either way, but a restart reloads offsets from the
// database, and skipping this write would turn every moved block into a
// decode failure after the next restart.
func (s *Store) CompactSegments(ctx context.Context) (int64, int, error) {
	if s == nil || s.db == nil {
		return 0, 0, errors.New("audit store is unavailable")
	}
	graceCutoff := time.Now().Add(-BlockInsertGrace).Unix()
	type blockRow struct {
		refcount int64
		lastSeen int64
	}
	rows := make(map[string]blockRow)
	{
		cursor, err := s.db.QueryContext(ctx, `SELECT hash, refcount, last_seen FROM blocks`)
		if err != nil {
			return 0, 0, fmt.Errorf("list audit block states: %w", err)
		}
		defer cursor.Close()
		for cursor.Next() {
			var hash string
			var row blockRow
			if err := cursor.Scan(&hash, &row.refcount, &row.lastSeen); err != nil {
				return 0, 0, fmt.Errorf("scan audit block states: %w", err)
			}
			rows[hash] = row
		}
		if err := cursor.Err(); err != nil {
			return 0, 0, fmt.Errorf("read audit block states: %w", err)
		}
	}
	keep := func(hash string) bool {
		row, known := rows[hash]
		if !known {
			// Bytes can be in the file before their index row commits; a hash
			// with no row at all is an insert in flight, not garbage. A failed
			// insert leaks its bytes until the next process restart.
			return true
		}
		if row.refcount > 0 {
			return true
		}
		// Unreferenced, but possibly still waiting for the body transaction
		// that bumps it to 1.
		return row.lastSeen >= graceCutoff
	}
	moved, reclaimed, dropped, err := s.segments.compact(keep)
	if err != nil {
		return reclaimed, dropped, err
	}
	if len(moved) > 0 {
		if err := s.persistBlockRefs(ctx, moved); err != nil {
			return reclaimed, dropped, fmt.Errorf("persist audit block offsets: %w", err)
		}
	}
	// Blocks whose bytes are gone must not stay in the index. The grace window
	// mirrors the keep predicate: a refcount-0 row can still be waiting for its
	// referencing body transaction.
	if _, err := s.db.ExecContext(ctx, `DELETE FROM blocks WHERE refcount <= 0 AND last_seen < ?`, graceCutoff); err != nil {
		return reclaimed, dropped, fmt.Errorf("prune audit block index: %w", err)
	}
	return reclaimed, dropped, nil
}

// persistBlockRefs writes compaction's new offsets in one transaction. A crash
// between the file rewrite and this commit leaves stale database offsets; the
// repair scanner on the read path rebuilds them from the frames themselves.
func (s *Store) persistBlockRefs(ctx context.Context, refs []blockRef) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, ref := range refs {
		if _, err := tx.ExecContext(ctx, `UPDATE blocks SET offset = ?, length = ? WHERE hash = ?`, ref.Offset, ref.Length, ref.Hash); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// DropExpiredSegments deletes whole day segments older than the cutoff: the
// conversations of that day are gone, so its bytes can go with one unlink.
func (s *Store) DropExpiredSegments(ctx context.Context, before time.Time) (int64, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("audit store is unavailable")
	}
	names, err := s.segments.segments()
	if err != nil {
		return 0, err
	}
	cutoff := segmentName(before)
	var reclaimed int64
	for _, name := range names {
		if name >= cutoff {
			continue
		}
		var remaining int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM blocks WHERE segment = ?`, name).Scan(&remaining); err != nil {
			return reclaimed, fmt.Errorf("count audit blocks in segment: %w", err)
		}
		if remaining > 0 {
			continue
		}
		freed, err := s.segments.dropSegment(name)
		if err != nil {
			return reclaimed, err
		}
		reclaimed += freed
	}
	return reclaimed, nil
}

// Stats summarizes the audit store for diagnostics: row count, logical and
// stored bytes, block count, and the on-disk segment size.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	if s == nil || s.db == nil {
		return Stats{}, errors.New("audit store is unavailable")
	}
	var stats Stats
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(size_bytes),0), COALESCE(SUM(stored_bytes),0),
		       COALESCE(SUM(CASE WHEN session_id != '' THEN 1 ELSE 0 END),0)
		FROM conversations`).Scan(&stats.Conversations, &stats.LogicalBytes, &stats.StoredBytes, &stats.WithSession)
	if err != nil {
		return Stats{}, fmt.Errorf("read audit stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(compressed_size),0) FROM blocks`).Scan(&stats.Blocks, &stats.BlockBytes); err != nil {
		return Stats{}, fmt.Errorf("read audit block stats: %w", err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM bodies`).Scan(&stats.Bodies); err != nil {
		return Stats{}, fmt.Errorf("read audit body stats: %w", err)
	}
	segmentBytes, err := s.segments.Bytes()
	if err != nil {
		return Stats{}, err
	}
	stats.SegmentBytes = segmentBytes
	stats.DedupRatio = 1
	if stats.LogicalBytes > 0 && stats.StoredBytes >= 0 {
		stats.DedupRatio = float64(stats.StoredBytes) / float64(stats.LogicalBytes)
	}
	return stats, nil
}

// Stats is the audit store's size summary.
type Stats struct {
	Conversations int     `json:"conversations"`
	WithSession   int     `json:"withSession"`
	Bodies        int     `json:"bodies"`
	Blocks        int     `json:"blocks"`
	LogicalBytes  int64   `json:"logicalBytes"`
	StoredBytes   int64   `json:"storedBytes"`
	BlockBytes    int64   `json:"blockBytes"`
	SegmentBytes  int64   `json:"segmentBytes"`
	DedupRatio    float64 `json:"dedupRatio"`
}

// Meta reads one value from the audit store's metadata table.
func (s *Store) Meta(ctx context.Context, key string) (string, bool, error) {
	if s == nil || s.db == nil {
		return "", false, errors.New("audit store is unavailable")
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key = ?`, key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read audit meta: %w", err)
	}
	return value, true, nil
}

// SetMeta records one metadata value.
func (s *Store) SetMeta(ctx context.Context, key, value string) error {
	if s == nil || s.db == nil {
		return errors.New("audit store is unavailable")
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO meta (key, value) VALUES (?,?) ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value); err != nil {
		return fmt.Errorf("write audit meta: %w", err)
	}
	return nil
}

// Count returns the number of stored conversations.
func (s *Store) Count(ctx context.Context) (int, error) {
	if s == nil || s.db == nil {
		return 0, errors.New("audit store is unavailable")
	}
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM conversations`).Scan(&total); err != nil {
		return 0, fmt.Errorf("count audit conversations: %w", err)
	}
	return total, nil
}
