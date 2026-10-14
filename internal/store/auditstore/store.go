package auditstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Store is the dedicated audit database plus its compressed body segments.
//
// Layout, rooted at <store directory>/audit:
//
//	audit.db          row metadata, block index, session accumulation
//	segments/*.seg    zstd frames holding the body bytes
type Store struct {
	db       *sql.DB
	segments *SegmentStore
	root     string
}

// Record is one conversation row as persisted. It mirrors the store package's
// audit conversation, which the API layer keeps exposing unchanged.
type Record struct {
	ID                  string
	ActivityID          int
	RequestID           string
	KeyID               string
	Model               string
	SessionID           string
	ReqPath             string
	Timestamp           time.Time
	RequestHeaders      []byte
	RequestBody         []byte
	ResponseHeaders     []byte
	ResponseBody        []byte
	ResponseStatus      int
	InputTokens         int
	OutputTokens        int
	CachedTokens        int
	CacheCreationTokens int
	ReasoningTokens     int
	CacheHitRatio       float64
	CacheCreationRatio  float64
	RepairApplied       bool
	PrefixHash          string
	EstimatedCost       float64
	Complete            bool
	SizeBytes           int64
	// StoredBytes counts what the record actually costs on disk: the compressed
	// size of the blocks it introduced. It replaces the logical size for the
	// byte budget, which is what an operator means by "audit is using 5 GB".
	StoredBytes int64
	// Body references resolve through Materialize. They are the content hashes
	// of the stored bodies, and are empty when the body was absent or empty.
	RequestBodyRef   string
	RequestBodySize  int64
	ResponseBodyRef  string
	ResponseBodySize int64
}

// Query filters a conversation listing. It mirrors the store package's query so
// the HTTP layer keeps the same parameters.
type Query struct {
	KeyID, Model, SessionID string
	Models                  []string
	Start, End              time.Time
	Limit, Offset           int
	Page                    int
	Sort                    string
	Order                   string
}

// Page is a page of conversations plus the total row count.
type Page struct {
	Data       []Record
	Page       int
	Limit      int
	Total      int
	TotalPages int
}

// SessionSummary is the accumulated view of one conversation session: every
// request of the session shares its context, so the totals describe what the
// session really cost rather than what one turn cost.
type SessionSummary struct {
	SessionID        string    `json:"sessionId"`
	RequestCount     int       `json:"requestCount"`
	InputTokens      int64     `json:"inputTokens"`
	OutputTokens     int64     `json:"outputTokens"`
	StoredBytes      int64     `json:"storedBytes"`
	RequestBytes     int64     `json:"requestBytes"`
	SharedBlockBytes int64     `json:"sharedBlockBytes"`
	FirstTimestamp   time.Time `json:"firstTimestamp"`
	LastTimestamp    time.Time `json:"lastTimestamp"`
	FirstActivityID  int       `json:"firstActivityId"`
	LastActivityID   int       `json:"lastActivityId"`
}

const schema = `
CREATE TABLE IF NOT EXISTS conversations (
    id TEXT PRIMARY KEY,
    activity_id INTEGER NOT NULL DEFAULT 0,
    request_id TEXT NOT NULL DEFAULT '',
    key_id TEXT NOT NULL DEFAULT '',
    model_id TEXT NOT NULL DEFAULT '',
    session_id TEXT NOT NULL DEFAULT '',
    req_path TEXT NOT NULL DEFAULT '',
    ts_created INTEGER NOT NULL,
    req_headers BLOB NOT NULL DEFAULT X'',
    resp_headers BLOB NOT NULL DEFAULT X'',
    req_body_ref TEXT NOT NULL DEFAULT '',
    resp_body_ref TEXT NOT NULL DEFAULT '',
    req_body_size INTEGER NOT NULL DEFAULT 0,
    resp_body_size INTEGER NOT NULL DEFAULT 0,
    resp_status_code INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cached_tokens INTEGER NOT NULL DEFAULT 0,
    cache_creation_tokens INTEGER NOT NULL DEFAULT 0,
    reasoning_tokens INTEGER NOT NULL DEFAULT 0,
    cache_hit_ratio REAL NOT NULL DEFAULT 0,
    cache_creation_ratio REAL NOT NULL DEFAULT 0,
    repair_applied INTEGER NOT NULL DEFAULT 0,
    prefix_hash TEXT NOT NULL DEFAULT '',
    estimated_cost REAL NOT NULL DEFAULT 0,
    complete INTEGER NOT NULL DEFAULT 0,
    size_bytes INTEGER NOT NULL DEFAULT 0,
    stored_bytes INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_conversations_created ON conversations (ts_created DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_key_created ON conversations (key_id, ts_created DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_model_created ON conversations (model_id, ts_created DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_session_created ON conversations (session_id, ts_created DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_conversations_activity ON conversations (activity_id);

-- bodies are the immutable decomposition of one payload; several conversations
-- share a body when the bytes repeat (a retry, a replayed batch).
CREATE TABLE IF NOT EXISTS bodies (
    hash TEXT PRIMARY KEY,
    size INTEGER NOT NULL,
    stored_size INTEGER NOT NULL,
    kind TEXT NOT NULL DEFAULT 'raw',
    descriptor BLOB NOT NULL,
    refcount INTEGER NOT NULL DEFAULT 0,
    first_seen INTEGER NOT NULL,
    last_seen INTEGER NOT NULL
);

-- blocks are the content-addressed pieces bodies are made of: one block per
-- conversation message, so the accumulated context a session re-sends every
-- turn keeps a single identity and a single stored copy.
CREATE TABLE IF NOT EXISTS blocks (
    hash TEXT PRIMARY KEY,
    size INTEGER NOT NULL,
    compressed_size INTEGER NOT NULL,
    segment TEXT NOT NULL,
    offset INTEGER NOT NULL,
    length INTEGER NOT NULL,
    refcount INTEGER NOT NULL DEFAULT 0,
    first_seen INTEGER NOT NULL,
    last_seen INTEGER NOT NULL
);

-- sessions accumulate one conversation's cost across its turns.
CREATE TABLE IF NOT EXISTS sessions (
    session_id TEXT PRIMARY KEY,
    request_count INTEGER NOT NULL DEFAULT 0,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    stored_bytes INTEGER NOT NULL DEFAULT 0,
    request_bytes INTEGER NOT NULL DEFAULT 0,
    shared_block_bytes INTEGER NOT NULL DEFAULT 0,
    first_ts INTEGER NOT NULL DEFAULT 0,
    last_ts INTEGER NOT NULL DEFAULT 0,
    first_activity_id INTEGER NOT NULL DEFAULT 0,
    last_activity_id INTEGER NOT NULL DEFAULT 0
);

-- meta carries the one-time migration watermark so a restart resumes instead
-- of re-copying the legacy table.
CREATE TABLE IF NOT EXISTS meta (
    key TEXT PRIMARY KEY,
    value TEXT NOT NULL
);
`

// Open prepares the audit database and its segment directory.
func Open(root string) (*Store, error) {
	if strings.TrimSpace(root) == "" {
		return nil, errors.New("audit store root is required")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	db, err := sql.Open("sqlite", auditDSN(filepath.Join(root, "audit.db")))
	if err != nil {
		return nil, fmt.Errorf("open audit database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping audit database: %w", err)
	}
	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create audit schema: %w", err)
	}
	segments, err := openSegmentStore(root)
	if err != nil {
		db.Close()
		return nil, err
	}
	store := &Store{db: db, segments: segments, root: root}
	if err := store.loadBlockIndex(); err != nil {
		db.Close()
		segments.close()
		return nil, err
	}
	return store, nil
}

func auditDSN(path string) string {
	return "file:" + path + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=foreign_keys(0)"
}

// Close releases the database and segment handles.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	var err error
	if s.segments != nil {
		err = s.segments.close()
	}
	if s.db != nil {
		if closeErr := s.db.Close(); err != nil {
			err = closeErr
		}
	}
	return err
}

// Root reports the directory the audit store owns.
func (s *Store) Root() string { return s.root }

func (s *Store) loadBlockIndex() error {
	rows, err := s.db.Query(`SELECT hash, size, compressed_size, segment, offset, length FROM blocks`)
	if err != nil {
		return fmt.Errorf("load audit block index: %w", err)
	}
	defer rows.Close()
	refs := make([]blockRef, 0, 1024)
	for rows.Next() {
		var ref blockRef
		if err := rows.Scan(&ref.Hash, &ref.Size, &ref.Compressed, &ref.Segment, &ref.Offset, &ref.Length); err != nil {
			return fmt.Errorf("scan audit block index: %w", err)
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read audit block index: %w", err)
	}
	s.segments.loadIndex(refs)
	return nil
}

// Insert persists one conversation, storing its bodies as content-addressed
// blocks. Writing the same body again — the next turn of a session re-sending
// its history, a retry — stores nothing new.
func (s *Store) Insert(ctx context.Context, record Record) (Record, error) {
	if s == nil || s.db == nil {
		return Record{}, errors.New("audit store is unavailable")
	}
	if strings.TrimSpace(record.ID) == "" {
		return Record{}, errors.New("audit conversation id is required")
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}
	// The logical size is always derived from the four captured parts, exactly
	// as the pre-split store did: callers that omit it still get a budget
	// denominator, and a caller that supplies one cannot skew the ratio.
	record.SizeBytes = int64(len(record.RequestHeaders) + len(record.RequestBody) +
		len(record.ResponseHeaders) + len(record.ResponseBody))

	reqDescriptor, reqStored, err := s.storeBody(record.RequestBody, record.Timestamp)
	if err != nil {
		return Record{}, err
	}
	respDescriptor, respStored, err := s.storeBody(record.ResponseBody, record.Timestamp)
	if err != nil {
		return Record{}, err
	}
	record.StoredBytes = reqStored + respStored

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Record{}, fmt.Errorf("begin audit insert: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	reqRef := ""
	if len(reqDescriptor) > 0 {
		reqRef, err = s.upsertBodyTx(ctx, tx, record.RequestBody, reqDescriptor, reqStored, record.Timestamp)
		if err != nil {
			return Record{}, err
		}
	}
	respRef := ""
	if len(respDescriptor) > 0 {
		respRef, err = s.upsertBodyTx(ctx, tx, record.ResponseBody, respDescriptor, respStored, record.Timestamp)
		if err != nil {
			return Record{}, err
		}
	}

	_, err = tx.ExecContext(ctx, `
		INSERT INTO conversations (
			id, activity_id, request_id, key_id, model_id, session_id, req_path, ts_created,
			req_headers, resp_headers, req_body_ref, resp_body_ref, req_body_size, resp_body_size,
			resp_status_code, input_tokens, output_tokens, cached_tokens, cache_creation_tokens,
			reasoning_tokens, cache_hit_ratio, cache_creation_ratio, repair_applied, prefix_hash,
			estimated_cost, complete, size_bytes, stored_bytes
		) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET
			activity_id=excluded.activity_id, request_id=excluded.request_id, key_id=excluded.key_id,
			model_id=excluded.model_id, session_id=excluded.session_id, req_path=excluded.req_path,
			ts_created=excluded.ts_created, req_headers=excluded.req_headers, resp_headers=excluded.resp_headers,
			req_body_ref=excluded.req_body_ref, resp_body_ref=excluded.resp_body_ref,
			req_body_size=excluded.req_body_size, resp_body_size=excluded.resp_body_size,
			resp_status_code=excluded.resp_status_code, input_tokens=excluded.input_tokens,
			output_tokens=excluded.output_tokens, cached_tokens=excluded.cached_tokens,
			cache_creation_tokens=excluded.cache_creation_tokens, reasoning_tokens=excluded.reasoning_tokens,
			cache_hit_ratio=excluded.cache_hit_ratio, cache_creation_ratio=excluded.cache_creation_ratio,
			repair_applied=excluded.repair_applied, prefix_hash=excluded.prefix_hash,
			estimated_cost=excluded.estimated_cost, complete=excluded.complete,
			size_bytes=excluded.size_bytes, stored_bytes=excluded.stored_bytes`,
		record.ID, record.ActivityID, record.RequestID, record.KeyID, record.Model, record.SessionID,
		record.ReqPath, record.Timestamp.Unix(), nonNilBytes(record.RequestHeaders), nonNilBytes(record.ResponseHeaders),
		reqRef, respRef, int64(len(record.RequestBody)), int64(len(record.ResponseBody)),
		record.ResponseStatus, record.InputTokens, record.OutputTokens, record.CachedTokens,
		record.CacheCreationTokens, record.ReasoningTokens, record.CacheHitRatio, record.CacheCreationRatio,
		boolInt(record.RepairApplied), record.PrefixHash, record.EstimatedCost, boolInt(record.Complete),
		record.SizeBytes, record.StoredBytes,
	)
	if err != nil {
		return Record{}, fmt.Errorf("insert audit conversation: %w", err)
	}
	if err := s.upsertSessionTx(ctx, tx, record, reqStored+respStored); err != nil {
		return Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return Record{}, fmt.Errorf("commit audit insert: %w", err)
	}
	record.RequestBodyRef = reqRef
	record.RequestBodySize = int64(len(record.RequestBody))
	record.ResponseBodyRef = respRef
	record.ResponseBodySize = int64(len(record.ResponseBody))
	return record, nil
}

// storeBody writes a body's blocks and returns its descriptor plus the number
// of bytes newly stored (deduplicated blocks cost nothing).
func (s *Store) storeBody(body []byte, at time.Time) ([]byte, int64, error) {
	if len(body) == 0 {
		return nil, 0, nil
	}
	existing, err := s.lookupBody(body)
	if err != nil {
		return nil, 0, err
	}
	if len(existing) > 0 {
		return existing, 0, nil
	}

	plan := planBody(body)
	descriptor := descriptor{Version: 1, Kind: plan.kind, Size: len(body)}
	stored := int64(0)
	segment := segmentName(at)

	if plan.kind == kindChat {
		ref, isNew, err := s.segments.put(plan.template, segment)
		if err != nil {
			return nil, 0, err
		}
		if err := s.recordBlock(ref, isNew, at); err != nil {
			return nil, 0, err
		}
		if isNew {
			stored += int64(ref.Compressed)
		}
		descriptor.Template = ref.Hash
	} else {
		ref, isNew, err := s.segments.put(plan.blocks[0], segment)
		if err != nil {
			return nil, 0, err
		}
		if err := s.recordBlock(ref, isNew, at); err != nil {
			return nil, 0, err
		}
		if isNew {
			stored += int64(ref.Compressed)
		}
		descriptor.Blocks = append(descriptor.Blocks, ref.Hash)
		return mustMarshal(descriptor), stored, nil
	}

	for _, block := range plan.blocks {
		ref, isNew, err := s.segments.put(block, segment)
		if err != nil {
			return nil, 0, err
		}
		if err := s.recordBlock(ref, isNew, at); err != nil {
			return nil, 0, err
		}
		if isNew {
			stored += int64(ref.Compressed)
		}
		descriptor.Blocks = append(descriptor.Blocks, ref.Hash)
	}
	return mustMarshal(descriptor), stored, nil
}

// lookupBody returns the descriptor of an already-stored identical body.
func (s *Store) lookupBody(body []byte) ([]byte, error) {
	hash := hashBlock(body)
	var descriptor []byte
	err := s.db.QueryRow(`SELECT descriptor FROM bodies WHERE hash = ?`, hash).Scan(&descriptor)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lookup audit body: %w", err)
	}
	return descriptor, nil
}

// recordBlock indexes a block and bumps its reference count when the body
// referencing it is created.
func (s *Store) recordBlock(ref blockRef, isNew bool, at time.Time) error {
	now := at.Unix()
	var err error
	if isNew {
		_, err = s.db.Exec(`
			INSERT INTO blocks (hash, size, compressed_size, segment, offset, length, refcount, first_seen, last_seen)
			VALUES (?,?,?,?,?,?,0,?,?)
			ON CONFLICT(hash) DO UPDATE SET last_seen=excluded.last_seen`,
			ref.Hash, ref.Size, ref.Compressed, ref.Segment, ref.Offset, ref.Length, now, now)
	} else {
		_, err = s.db.Exec(`UPDATE blocks SET last_seen = ? WHERE hash = ?`, now, ref.Hash)
	}
	if err != nil {
		return fmt.Errorf("index audit block: %w", err)
	}
	return nil
}

// upsertBodyTx records the body row. A body that already exists only bumps its
// reference count; a newly inserted body also bumps the reference counts of the
// blocks it is made of, which is what lets compaction know a block is still
// reachable.
func (s *Store) upsertBodyTx(ctx context.Context, tx *sql.Tx, body, descriptor []byte, stored int64, at time.Time) (string, error) {
	hash := hashBlock(body)
	now := at.Unix()
	kind := mustUnmarshal(descriptor).Kind
	if kind == "" {
		kind = kindRaw
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO bodies (hash, size, stored_size, kind, descriptor, refcount, first_seen, last_seen)
		VALUES (?,?,?,?,?,1,?,?)
		ON CONFLICT(hash) DO UPDATE SET refcount = refcount + 1, last_seen = excluded.last_seen`,
		hash, len(body), stored, kind, descriptor, now, now)
	if err != nil {
		return "", fmt.Errorf("store audit body: %w", err)
	}
	if affected, err := res.RowsAffected(); err == nil && affected == 0 {
		return hash, nil
	}
	// ON CONFLICT DO UPDATE reports one affected row either way, so the insert
	// path is told apart by the refcount the row now carries.
	var refcount int
	if err := tx.QueryRowContext(ctx, `SELECT refcount FROM bodies WHERE hash = ?`, hash).Scan(&refcount); err != nil {
		return "", fmt.Errorf("read audit body refcount: %w", err)
	}
	if refcount != 1 {
		return hash, nil
	}
	decoded := mustUnmarshal(descriptor)
	refs := decoded.Blocks
	if decoded.Template != "" {
		refs = append([]string{decoded.Template}, refs...)
	}
	for _, blockHash := range refs {
		if _, err := tx.ExecContext(ctx, `UPDATE blocks SET refcount = refcount + 1, last_seen = ? WHERE hash = ?`, now, blockHash); err != nil {
			return "", fmt.Errorf("reference audit block: %w", err)
		}
	}
	return hash, nil
}

// upsertSessionTx folds one request into its session's accumulated totals.
func (s *Store) upsertSessionTx(ctx context.Context, tx *sql.Tx, record Record, stored int64) error {
	if strings.TrimSpace(record.SessionID) == "" {
		return nil
	}
	ts := record.Timestamp.Unix()
	_, err := tx.ExecContext(ctx, `
		INSERT INTO sessions (
			session_id, request_count, input_tokens, output_tokens, stored_bytes, request_bytes,
			shared_block_bytes, first_ts, last_ts, first_activity_id, last_activity_id
		) VALUES (?,1,?,?,?,?,0,?,?,?,?)
		ON CONFLICT(session_id) DO UPDATE SET
			request_count = request_count + 1,
			input_tokens = input_tokens + excluded.input_tokens,
			output_tokens = output_tokens + excluded.output_tokens,
			stored_bytes = stored_bytes + excluded.stored_bytes,
			request_bytes = request_bytes + excluded.request_bytes,
			last_ts = CASE WHEN excluded.last_ts > last_ts THEN excluded.last_ts ELSE last_ts END,
			last_activity_id = CASE WHEN excluded.last_activity_id > last_activity_id THEN excluded.last_activity_id ELSE last_activity_id END,
			first_ts = CASE WHEN first_ts = 0 OR excluded.first_ts < first_ts THEN excluded.first_ts ELSE first_ts END,
			first_activity_id = CASE WHEN first_activity_id = 0 OR (excluded.first_activity_id != 0 AND excluded.first_activity_id < first_activity_id) THEN excluded.first_activity_id ELSE first_activity_id END`,
		record.SessionID, record.InputTokens, record.OutputTokens, stored, len(record.RequestBody),
		ts, ts, record.ActivityID, record.ActivityID)
	if err != nil {
		return fmt.Errorf("update audit session: %w", err)
	}
	return nil
}

// Session returns the accumulated view of one session.
func (s *Store) Session(ctx context.Context, sessionID string) (SessionSummary, bool, error) {
	if s == nil || s.db == nil || strings.TrimSpace(sessionID) == "" {
		return SessionSummary{}, false, nil
	}
	var summary SessionSummary
	var first, last int64
	err := s.db.QueryRowContext(ctx, `
		SELECT session_id, request_count, input_tokens, output_tokens, stored_bytes, request_bytes,
		       shared_block_bytes, first_ts, last_ts, first_activity_id, last_activity_id
		FROM sessions WHERE session_id = ?`, sessionID).
		Scan(&summary.SessionID, &summary.RequestCount, &summary.InputTokens, &summary.OutputTokens,
			&summary.StoredBytes, &summary.RequestBytes, &summary.SharedBlockBytes,
			&first, &last, &summary.FirstActivityID, &summary.LastActivityID)
	if errors.Is(err, sql.ErrNoRows) {
		return SessionSummary{}, false, nil
	}
	if err != nil {
		return SessionSummary{}, false, fmt.Errorf("read audit session: %w", err)
	}
	summary.FirstTimestamp = time.Unix(first, 0)
	summary.LastTimestamp = time.Unix(last, 0)
	summary.SharedBlockBytes = summary.RequestBytes - summary.StoredBytes
	if summary.SharedBlockBytes < 0 {
		summary.SharedBlockBytes = 0
	}
	return summary, true, nil
}

func mustMarshal(d descriptor) []byte {
	data, err := marshalDescriptor(d)
	if err != nil {
		return []byte(`{"v":1,"k":"raw"}`)
	}
	return data
}

func mustUnmarshal(data []byte) descriptor {
	d, err := unmarshalDescriptor(data)
	if err != nil {
		return descriptor{}
	}
	return d
}

func nonNilBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}
