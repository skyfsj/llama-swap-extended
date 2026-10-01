package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/mostlygeek/llama-swap/internal/auth"
)

// ErrAPIKeyNotFound is returned when a control-plane operation references an
// unknown key id. Keeping this sentinel at the store boundary lets HTTP and
// other callers distinguish a missing resource from a database failure.
var ErrAPIKeyNotFound = errors.New("api key not found")

type APIKeyRecord struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	KeyHash string `json:"-"`
	KeySalt []byte `json:"-"`
	// KeySecret is retained for explicit server-side integrations such as a
	// CC Switch import. It is never serialized by the control API; callers must
	// opt into a dedicated operation before reading it.
	KeySecret string `json:"-"`
	// Kind is either auth.KeyKindManagement (control-plane credential) or
	// auth.KeyKindAccess (model-access credential nested under a management
	// key). Existing rows are migrated to the management kind.
	Kind     string   `json:"kind"`
	ParentID string   `json:"parentId,omitempty"`
	Scopes   []string `json:"scopes"`
	Models   []string `json:"models,omitempty"`
	// AllowedIPs restricts the caller addresses an access key may be used
	// from. An empty list is unrestricted.
	AllowedIPs []string `json:"allowedIps,omitempty"`
	// MaxConcurrency caps the in-flight requests for an access key; 0 is
	// unlimited.
	MaxConcurrency       int        `json:"maxConcurrency"`
	Group                string     `json:"group,omitempty"`
	AllowManagementLogin bool       `json:"allowManagementLogin"`
	ExpiresAt            *time.Time `json:"expiresAt,omitempty"`
	CreatedAt            time.Time  `json:"createdAt"`
	LastUsedAt           *time.Time `json:"lastUsedAt,omitempty"`
	RevokedAt            *time.Time `json:"revokedAt,omitempty"`
	// ActiveRequests is not persisted. It is a live in-flight count the control
	// plane fills in so the UI can show an access key's concurrency against
	// its ceiling. It is always emitted (no omitempty) because a key that is
	// idle must be distinguishable from a projection that dropped the field.
	ActiveRequests int `json:"activeRequests"`
}

type AuditConversation struct {
	ID              string    `json:"id"`
	ActivityID      int       `json:"activityId,omitempty"`
	RequestID       string    `json:"requestId,omitempty"`
	KeyID           string    `json:"keyId,omitempty"`
	Model           string    `json:"model"`
	SessionID       string    `json:"sessionId,omitempty"`
	ReqPath         string    `json:"reqPath"`
	Timestamp       time.Time `json:"timestamp"`
	RequestHeaders  []byte    `json:"requestHeaders,omitempty"`
	RequestBody     []byte    `json:"requestBody,omitempty"`
	ResponseHeaders []byte    `json:"responseHeaders,omitempty"`
	ResponseBody    []byte    `json:"responseBody,omitempty"`
	// Bodies live in the content-addressed blob store once the conversation is
	// persisted; the detail view publishes the reference and logical size so a
	// client can stream the body on demand instead of receiving every turn
	// inline. Only bodies that were never externalized (an in-memory store)
	// stay populated above.
	RequestBodyRef      string  `json:"requestBodyRef,omitempty"`
	ResponseBodyRef     string  `json:"responseBodyRef,omitempty"`
	RequestBodyBytes    int64   `json:"requestBodyBytes,omitempty"`
	ResponseBodyBytes   int64   `json:"responseBodyBytes,omitempty"`
	ResponseStatus      int     `json:"responseStatus"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CachedTokens        int     `json:"cachedTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	ReasoningTokens     int     `json:"reasoningTokens,omitempty"`
	CacheHitRatio       float64 `json:"cacheHitRatio,omitempty"`
	CacheCreationRatio  float64 `json:"cacheCreationRatio,omitempty"`
	RepairApplied       bool    `json:"repairApplied,omitempty"`
	PrefixHash          string  `json:"prefixHash,omitempty"`
	// Phase telemetry copied from the joined activity row for the detail
	// view's speed section: FirstTokenMs (request admission to first visible
	// token), DecodeMs (first to last visible token, -1 when not measured)
	// and DurationMs (wall clock). SpeedTimeline is that row's bounded
	// [msSinceStart, cumulativeTokens] JSON curve.
	FirstTokenMs  int     `json:"firstTokenMs"`
	DecodeMs      int     `json:"decodeMs"`
	DurationMs    int     `json:"durationMs"`
	SpeedTimeline string  `json:"speedTimeline,omitempty"`
	EstimatedCost float64 `json:"estimatedCost"`
	Complete      bool    `json:"complete"`
	SizeBytes     int64   `json:"sizeBytes"`
}

type AuditQuery struct {
	KeyID, Model, SessionID string
	// Models is an optional exact allowlist used by model-scoped control-plane
	// identities. Model takes precedence when set for backwards compatibility
	// with callers that issue one-model queries.
	Models        []string
	Start, End    time.Time
	Limit, Offset int
	Page          int
	Sort          string
	Order         string
}

type AuditConversationPage struct {
	Data       []AuditConversation `json:"data"`
	Page       int                 `json:"page"`
	Limit      int                 `json:"limit"`
	Total      int                 `json:"total"`
	TotalPages int                 `json:"total_pages"`
}

type ResponseAffinity struct {
	ResponseID string    `json:"responseId"`
	Model      string    `json:"model"`
	Backend    string    `json:"backend,omitempty"`
	Status     string    `json:"status,omitempty"`
	Response   []byte    `json:"response,omitempty"`
	ExpiresAt  time.Time `json:"expiresAt"`
	CreatedAt  time.Time `json:"createdAt"`
	UpdatedAt  time.Time `json:"updatedAt"`
}

type Price struct {
	Provider   string    `json:"provider"`
	Model      string    `json:"model"`
	Input      float64   `json:"input"`
	Output     float64   `json:"output"`
	CacheRead  float64   `json:"cacheRead"`
	CacheWrite float64   `json:"cacheWrite"`
	Reasoning  float64   `json:"reasoning"`
	SyncedAt   time.Time `json:"syncedAt"`
}

type PricingMeta struct {
	ETag     string
	SyncedAt time.Time
	Source   string
}

const maxPriceIdentityBytes = 512

// Response affinity is a small, restart-visible cache of Responses objects
// used by previous_response_id and GET/cancel. Keep the same bounds at the
// store boundary that the HTTP middleware applies so direct callers cannot
// bypass retention or memory limits.
const (
	defaultResponseAffinityRetention = 30 * 24 * time.Hour
	maxResponseAffinityBytes         = 4 << 20
)

// RuntimeOperation mirrors the latest durable state of one runtime-manager
// operation. The runtime package keeps its append-only JSONL journal for
// recovery; this compact projection is used by control-plane queries and
// survives a process restart when the store is file-backed.
type RuntimeOperation struct {
	ID          string    `json:"id"`
	RuntimeName string    `json:"runtimeName"`
	Action      string    `json:"action"`
	State       string    `json:"state"`
	Version     string    `json:"version,omitempty"`
	Error       string    `json:"error,omitempty"`
	StartedAt   time.Time `json:"startedAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// UsageSummary is a bounded aggregate used by the management UI. Costs are
// estimates derived from the configured pricing catalog, never billing data.
type UsageSummary struct {
	Requests            int          `json:"requests"`
	InputTokens         int          `json:"inputTokens"`
	OutputTokens        int          `json:"outputTokens"`
	CachedTokens        int          `json:"cachedTokens"`
	CacheCreationTokens int          `json:"cacheCreationTokens"`
	CacheHitRatio       float64      `json:"cacheHitRatio,omitempty"`
	CacheCreationRatio  float64      `json:"cacheCreationRatio,omitempty"`
	ReasoningTokens     int          `json:"reasoningTokens"`
	EstimatedCost       float64      `json:"estimatedCost"`
	CostEstimated       bool         `json:"costEstimated"`
	ByModel             []ModelUsage `json:"byModel,omitempty"`
}

type ModelUsage struct {
	Model               string  `json:"model"`
	Requests            int     `json:"requests"`
	InputTokens         int     `json:"inputTokens"`
	OutputTokens        int     `json:"outputTokens"`
	CachedTokens        int     `json:"cachedTokens"`
	CacheCreationTokens int     `json:"cacheCreationTokens"`
	CacheHitRatio       float64 `json:"cacheHitRatio,omitempty"`
	CacheCreationRatio  float64 `json:"cacheCreationRatio,omitempty"`
	ReasoningTokens     int     `json:"reasoningTokens"`
	EstimatedCost       float64 `json:"estimatedCost"`
	CostEstimated       bool    `json:"costEstimated"`
}

func KeyFingerprint(raw string) string {
	h := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(h[:])
}

const (
	maxAPIKeyIDLength = 128
	apiKeyHashLength  = sha256.Size * 2
)

// normalizeAPIKeyRecord is the last validation boundary before an API key is
// written to SQLite. HTTP and config callers already normalize these fields,
// but Store is also a public package API used by embedders and startup
// migration code. Keeping the checks here prevents a direct caller from
// persisting an unknown scope, an unsafe model pattern, or a raw/partially
// formed digest that later code would treat as an authentication credential.
func normalizeAPIKeyRecord(record APIKeyRecord) (APIKeyRecord, error) {
	if err := validateAPIKeyID(record.ID); err != nil {
		return APIKeyRecord{}, err
	}
	record.ID = strings.TrimSpace(record.ID)

	name, err := auth.NormalizeKeyName(record.Name)
	if err != nil {
		return APIKeyRecord{}, err
	}
	record.Name = name

	kind, err := normalizeAPIKeyKind(record.Kind)
	if err != nil {
		return APIKeyRecord{}, err
	}
	record.Kind = kind

	parentID := strings.TrimSpace(record.ParentID)
	if parentID != "" {
		if err := validateAPIKeyID(parentID); err != nil {
			return APIKeyRecord{}, err
		}
	}
	record.ParentID = parentID

	scopes, err := auth.NormalizeScopes(record.Scopes)
	if err != nil {
		return APIKeyRecord{}, err
	}
	record.Scopes = make([]string, 0, len(scopes))
	for scope := range scopes {
		record.Scopes = append(record.Scopes, scope)
	}
	sort.Strings(record.Scopes)

	record.Models, err = auth.NormalizeModels(record.Models)
	if err != nil {
		return APIKeyRecord{}, err
	}

	record.AllowedIPs, err = auth.NormalizeAccessIPs(record.AllowedIPs)
	if err != nil {
		return APIKeyRecord{}, err
	}

	record.MaxConcurrency = auth.NormalizeMaxConcurrency(record.MaxConcurrency)

	record.Group, err = auth.NormalizeGroup(record.Group)
	if err != nil {
		return APIKeyRecord{}, err
	}

	if err := validateAPIKeyHash(record.KeyHash); err != nil {
		return APIKeyRecord{}, err
	}
	record.KeyHash = strings.ToLower(strings.TrimSpace(record.KeyHash))
	return record, nil
}

// normalizeAPIKeyKind maps the accepted spellings onto the canonical kinds.
// An empty kind is a management key: every row that existed before the
// management/access split was a full control-plane credential, and callers
// that predate the field keep writing records without one.
func normalizeAPIKeyKind(kind string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "", auth.KeyKindManagement:
		return auth.KeyKindManagement, nil
	case auth.KeyKindAccess:
		return auth.KeyKindAccess, nil
	default:
		return "", errors.New("unknown api key kind: " + kind)
	}
}

func validateAPIKeyID(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("api key id is required")
	}
	trimmed := strings.TrimSpace(id)
	if len(trimmed) > maxAPIKeyIDLength {
		return fmt.Errorf("api key id must be at most %d characters", maxAPIKeyIDLength)
	}
	for _, r := range id {
		if (unicode.IsSpace(r) && r != ' ') || unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || r == '/' || r == '\\' {
			return errors.New("api key id contains unsafe characters")
		}
	}
	return nil
}

func validateAPIKeyHash(hash string) error {
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) != apiKeyHashLength {
		return fmt.Errorf("api key hash must be a %d-character SHA-256 hex digest", apiKeyHashLength)
	}
	decoded, err := hex.DecodeString(hash)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("api key hash must be a SHA-256 hex digest")
	}
	return nil
}

func (s *Store) UpsertAPIKey(ctx context.Context, record APIKeyRecord) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	var err error
	if record, err = normalizeAPIKeyRecord(record); err != nil {
		return err
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now()
	}
	scopes, err := json.Marshal(record.Scopes)
	if err != nil {
		return err
	}
	models, err := json.Marshal(record.Models)
	if err != nil {
		return err
	}
	var expires any
	if record.ExpiresAt != nil {
		expires = record.ExpiresAt.Unix()
	}
	salt := record.KeySalt
	if salt == nil {
		salt = []byte{}
	}
	accessIPs, err := json.Marshal(record.AllowedIPs)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO api_keys(id,name,key_hash,key_salt,key_secret,scopes_json,models_json,allow_management_login,expires_at,created_at,last_used_at,revoked_at,kind,parent_id,access_ips_json,max_concurrency,group_name)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, key_hash=excluded.key_hash, key_salt=excluded.key_salt, scopes_json=excluded.scopes_json,
		key_secret=CASE WHEN excluded.key_secret <> '' THEN excluded.key_secret ELSE api_keys.key_secret END,
		models_json=excluded.models_json, allow_management_login=excluded.allow_management_login, expires_at=excluded.expires_at,
		revoked_at=excluded.revoked_at, kind=excluded.kind, parent_id=excluded.parent_id,
		access_ips_json=excluded.access_ips_json, max_concurrency=excluded.max_concurrency, group_name=excluded.group_name`,
		record.ID, record.Name, record.KeyHash, salt, record.KeySecret, string(scopes), string(models), record.AllowManagementLogin, expires,
		record.CreatedAt.Unix(), nullableTime(record.LastUsedAt), nullableTime(record.RevokedAt),
		record.Kind, record.ParentID, string(accessIPs), record.MaxConcurrency, record.Group)
	if err != nil {
		return fmt.Errorf("upsert api key: %w", err)
	}
	return nil
}

func nullableTime(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Unix()
}

// apiKeyColumns is the shared projection for every API-key read path so a new
// column cannot be added to one query and silently missing from another. The
// retained secret is deliberately excluded: it stays reachable only through
// GetAPIKeySecret, which requires an explicitly authorized caller.
const apiKeyColumns = `SELECT id,name,key_hash,key_salt,scopes_json,models_json,allow_management_login,expires_at,created_at,last_used_at,revoked_at,kind,parent_id,access_ips_json,max_concurrency,group_name FROM api_keys`

func (s *Store) ListAPIKeys(ctx context.Context, includeRevoked bool) ([]APIKeyRecord, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return nil, errors.New("store is unavailable")
	}
	query := apiKeyColumns
	if !includeRevoked {
		query += ` WHERE revoked_at IS NULL`
	}
	query += ` ORDER BY created_at DESC, id`
	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("list api keys: %w", err)
	}
	defer rows.Close()
	var out []APIKeyRecord
	for rows.Next() {
		r, err := scanAPIKey(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanAPIKey reads one full API-key row. It is shared by every key lookup so
// the kind, nesting and access-restriction columns stay in lockstep. The
// retained secret is not part of the projection; callers that legitimately
// need it go through GetAPIKeySecret.
func scanAPIKey(rows *sql.Rows) (APIKeyRecord, error) {
	var r APIKeyRecord
	var scopes, models, accessIPs, kind, parentID, group string
	var salt []byte
	var maxConcurrency sql.NullInt64
	var expires, created, last, revoked sql.NullInt64
	if err := rows.Scan(&r.ID, &r.Name, &r.KeyHash, &salt, &scopes, &models, &r.AllowManagementLogin,
		&expires, &created, &last, &revoked, &kind, &parentID, &accessIPs, &maxConcurrency, &group); err != nil {
		return APIKeyRecord{}, err
	}
	r.KeySalt = append([]byte(nil), salt...)
	r.Kind = kind
	r.ParentID = parentID
	r.Group = group
	r.MaxConcurrency = int(maxConcurrency.Int64)
	if err := json.Unmarshal([]byte(scopes), &r.Scopes); err != nil {
		return APIKeyRecord{}, err
	}
	if err := json.Unmarshal([]byte(models), &r.Models); err != nil {
		return APIKeyRecord{}, err
	}
	// An access-restriction column written before the kind split (or by an
	// embedder) can hold the JSON null literal; treat that as unrestricted.
	if trimmed := strings.TrimSpace(accessIPs); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(accessIPs), &r.AllowedIPs); err != nil {
			return APIKeyRecord{}, err
		}
	}
	r.CreatedAt = time.Unix(created.Int64, 0)
	r.ExpiresAt = scanTime(expires)
	r.LastUsedAt = scanTime(last)
	r.RevokedAt = scanTime(revoked)
	return r, nil
}

func scanTime(value sql.NullInt64) *time.Time {
	if !value.Valid {
		return nil
	}
	t := time.Unix(value.Int64, 0)
	return &t
}

func (s *Store) FindAPIKeyByHash(ctx context.Context, hash string) (APIKeyRecord, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return APIKeyRecord{}, false, errors.New("store is unavailable")
	}
	// Key fingerprints are stored as lowercase SHA-256 hex. Normalize lookup
	// input as well so direct embedders and administrative callers cannot miss a
	// valid key merely because they supplied an uppercase digest; malformed
	// lengths still fail closed without touching SQLite.
	hash = strings.ToLower(strings.TrimSpace(hash))
	if len(hash) != apiKeyHashLength {
		return APIKeyRecord{}, false, nil
	}
	var r APIKeyRecord
	var scopes, models, accessIPs, kind, parentID, group string
	var salt []byte
	var maxConcurrency sql.NullInt64
	var expires, created, last, revoked sql.NullInt64
	err := s.db.QueryRowContext(ctx, apiKeyColumns+` WHERE key_hash=?`, hash).
		Scan(&r.ID, &r.Name, &r.KeyHash, &salt, &scopes, &models, &r.AllowManagementLogin,
			&expires, &created, &last, &revoked, &kind, &parentID, &accessIPs, &maxConcurrency, &group)
	if errors.Is(err, sql.ErrNoRows) {
		return APIKeyRecord{}, false, nil
	}
	if err != nil {
		return APIKeyRecord{}, false, err
	}
	r.KeySalt = append([]byte(nil), salt...)
	r.Kind = kind
	r.ParentID = parentID
	r.Group = group
	r.MaxConcurrency = int(maxConcurrency.Int64)
	if err := json.Unmarshal([]byte(scopes), &r.Scopes); err != nil {
		return APIKeyRecord{}, false, err
	}
	if err := json.Unmarshal([]byte(models), &r.Models); err != nil {
		return APIKeyRecord{}, false, err
	}
	if trimmed := strings.TrimSpace(accessIPs); trimmed != "" && trimmed != "null" {
		if err := json.Unmarshal([]byte(accessIPs), &r.AllowedIPs); err != nil {
			return APIKeyRecord{}, false, err
		}
	}
	r.CreatedAt = time.Unix(created.Int64, 0)
	r.ExpiresAt = scanTime(expires)
	r.LastUsedAt = scanTime(last)
	r.RevokedAt = scanTime(revoked)
	return r, true, nil
}

// FindAPIKeyByID returns one key row by id, including revoked keys. It backs
// both access-key parent resolution during authentication and the control
// plane's single-key lookups.
func (s *Store) FindAPIKeyByID(ctx context.Context, id string) (APIKeyRecord, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return APIKeyRecord{}, false, errors.New("store is unavailable")
	}
	id = strings.TrimSpace(id)
	if err := validateAPIKeyID(id); err != nil {
		return APIKeyRecord{}, false, err
	}
	rows, err := s.db.QueryContext(ctx, apiKeyColumns+` WHERE id=?`, id)
	if err != nil {
		return APIKeyRecord{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return APIKeyRecord{}, false, rows.Err()
	}
	record, err := scanAPIKey(rows)
	if err != nil {
		return APIKeyRecord{}, false, err
	}
	if err := rows.Err(); err != nil {
		return APIKeyRecord{}, false, err
	}
	return record, true, nil
}

// GetAPIKeySecret returns the server-retained secret for one explicitly named
// key. The value is intentionally not part of ListAPIKeys' JSON contract; the
// caller must already have authorized the operation that needs it.
func (s *Store) GetAPIKeySecret(ctx context.Context, id string) (string, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return "", false, errors.New("store is unavailable")
	}
	id = strings.TrimSpace(id)
	if err := validateAPIKeyID(id); err != nil {
		return "", false, err
	}
	var secret string
	err := s.db.QueryRowContext(ctx, `SELECT key_secret FROM api_keys WHERE id=?`, id).Scan(&secret)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return secret, true, nil
}

// FindAPIKeyBySecret supports both the legacy unsalted digest and the salted
// digest used by WebUI-created keys. It never returns the raw secret and keeps
// the legacy lookup path indexed before falling back to a bounded scan of the
// salted records.
func (s *Store) FindAPIKeyBySecret(ctx context.Context, secret string) (APIKeyRecord, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return APIKeyRecord{}, false, errors.New("store is unavailable")
	}
	legacy, found, err := s.FindAPIKeyByHash(ctx, auth.Hash(secret))
	if err != nil || found {
		return legacy, found, err
	}
	keys, err := s.ListAPIKeys(ctx, true)
	if err != nil {
		return APIKeyRecord{}, false, err
	}
	for _, key := range keys {
		if len(key.KeySalt) > 0 && auth.VerifySalted(secret, key.KeySalt, key.KeyHash) {
			return key, true, nil
		}
	}
	return APIKeyRecord{}, false, nil
}

func (s *Store) TouchAPIKey(ctx context.Context, id string, when time.Time) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("api key id is required")
	}
	_, err := s.db.ExecContext(ctx, `UPDATE api_keys SET last_used_at=? WHERE id=?`, when.Unix(), id)
	return err
}

func (s *Store) RevokeAPIKey(ctx context.Context, id string, when time.Time) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("api key id is required")
	}
	result, err := s.db.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=?`, when.Unix(), id)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return ErrAPIKeyNotFound
	}
	return nil
}

// RevokeAPIKeyCascade revokes one key together with every access key nested
// under it. Leaving orphaned access keys active after their management parent
// is revoked would silently extend the parent's access well past the moment
// the operator believed it ended, so the walk is delegated to this one call.
// It is idempotent for a key that is already revoked, but returns
// ErrAPIKeyNotFound when the exact id does not exist so callers can keep
// reporting a 404 for an unknown key.
func (s *Store) RevokeAPIKeyCascade(ctx context.Context, id string, when time.Time) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	id = strings.TrimSpace(id)
	if err := validateAPIKeyID(id); err != nil {
		return err
	}
	keys, err := s.ListAPIKeys(ctx, true)
	if err != nil {
		return err
	}
	if findAPIKeyByID(keys, id) == nil {
		return fmt.Errorf("api key %q: %w", id, ErrAPIKeyNotFound)
	}
	children := make(map[string][]string, len(keys))
	for _, key := range keys {
		parent := strings.TrimSpace(key.ParentID)
		if parent == "" {
			continue
		}
		children[parent] = append(children[parent], key.ID)
	}
	revoked := make([]string, 0, len(children[id])+1)
	queue := append([]string(nil), children[id]...)
	seen := map[string]struct{}{id: {}}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if _, duplicate := seen[current]; duplicate {
			continue
		}
		seen[current] = struct{}{}
		revoked = append(revoked, current)
		queue = append(queue, children[current]...)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin api key cascade revoke: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, when.Unix(), id); err != nil {
		return fmt.Errorf("revoke api key %q: %w", id, err)
	}
	for _, childID := range revoked {
		if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, when.Unix(), childID); err != nil {
			return fmt.Errorf("revoke nested api key %q: %w", childID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit api key cascade revoke: %w", err)
	}
	return nil
}

// findAPIKeyByID returns the row with the given id, or nil when the key does
// not exist at all.
func findAPIKeyByID(keys []APIKeyRecord, id string) *APIKeyRecord {
	for i := range keys {
		if keys[i].ID == id {
			return &keys[i]
		}
	}
	return nil
}

// ChildAPIKeyCounts reports how many active access keys each management key
// owns. The control plane uses it to render the nesting summary without
// shipping every child row to the client.
func (s *Store) ChildAPIKeyCounts(ctx context.Context) (map[string]int, error) {
	ctx = normalizeContext(ctx)
	counts := map[string]int{}
	if s == nil || s.db == nil {
		return counts, errors.New("store is unavailable")
	}
	rows, err := s.db.QueryContext(ctx, `SELECT parent_id, COUNT(*) FROM api_keys WHERE revoked_at IS NULL AND parent_id <> '' GROUP BY parent_id`)
	if err != nil {
		return nil, fmt.Errorf("count nested api keys: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var parentID string
		var count int
		if err := rows.Scan(&parentID, &count); err != nil {
			return nil, err
		}
		counts[parentID] = count
	}
	return counts, rows.Err()
}

// RotateAPIKey atomically revokes an active key and inserts its replacement.
// A failed insert therefore cannot leave callers without the old credential,
// while a failed revoke can never expose both generations as active.
func (s *Store) RotateAPIKey(ctx context.Context, oldID string, replacement APIKeyRecord, revokedAt time.Time) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	if strings.TrimSpace(oldID) == "" {
		return errors.New("old api key id is required")
	}
	if err := validateAPIKeyID(oldID); err != nil {
		return fmt.Errorf("old api key id: %w", err)
	}
	oldID = strings.TrimSpace(oldID)
	var err error
	if replacement, err = normalizeAPIKeyRecord(replacement); err != nil {
		return err
	}
	if replacement.CreatedAt.IsZero() {
		replacement.CreatedAt = time.Now()
	}
	scopes, err := json.Marshal(replacement.Scopes)
	if err != nil {
		return err
	}
	models, err := json.Marshal(replacement.Models)
	if err != nil {
		return err
	}
	var expires any
	if replacement.ExpiresAt != nil {
		expires = replacement.ExpiresAt.Unix()
	}
	salt := replacement.KeySalt
	if salt == nil {
		salt = []byte{}
	}
	accessIPs, err := json.Marshal(replacement.AllowedIPs)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin api key rotation: %w", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at=? WHERE id=? AND revoked_at IS NULL`, revokedAt.Unix(), oldID)
	if err != nil {
		return fmt.Errorf("revoke api key during rotation: %w", err)
	}
	if affected, rowsErr := result.RowsAffected(); rowsErr != nil {
		return fmt.Errorf("check api key rotation: %w", rowsErr)
	} else if affected != 1 {
		return fmt.Errorf("active api key %q was not found: %w", oldID, ErrAPIKeyNotFound)
	}
	_, err = tx.ExecContext(ctx, `
		INSERT INTO api_keys(id,name,key_hash,key_salt,key_secret,scopes_json,models_json,allow_management_login,expires_at,created_at,last_used_at,revoked_at,kind,parent_id,access_ips_json,max_concurrency,group_name)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		replacement.ID, replacement.Name, replacement.KeyHash, salt, replacement.KeySecret, string(scopes), string(models), replacement.AllowManagementLogin, expires,
		replacement.CreatedAt.Unix(), nullableTime(replacement.LastUsedAt), nullableTime(replacement.RevokedAt),
		replacement.Kind, replacement.ParentID, string(accessIPs), replacement.MaxConcurrency, replacement.Group)
	if err != nil {
		return fmt.Errorf("insert replacement api key: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit api key rotation: %w", err)
	}
	return nil
}

func (s *Store) InsertAuditConversation(ctx context.Context, record AuditConversation) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	if record.ID == "" {
		return errors.New("audit conversation id is required")
	}
	if record.Timestamp.IsZero() {
		record.Timestamp = time.Now()
	}
	record.CacheCreationRatio = normalizeCacheRatio(record.CacheCreationRatio)
	if record.CacheCreationRatio == 0 && record.CacheCreationTokens > 0 {
		record.CacheCreationRatio = cacheCreationRatio(record.InputTokens, record.CachedTokens, record.CacheCreationTokens)
	}
	record.SizeBytes = int64(len(record.RequestHeaders) + len(record.RequestBody) + len(record.ResponseHeaders) + len(record.ResponseBody))
	reqBody, reqBlobRef := s.externalizeBlob(nonNilBytes(record.RequestBody))
	respBody, respBlobRef := s.externalizeBlob(nonNilBytes(record.ResponseBody))
	_, err := s.db.ExecContext(ctx, `INSERT OR REPLACE INTO audit_conversations
		(id,activity_id,request_id,key_id,model_id,session_id,req_path,ts_created,req_headers_json,req_body,resp_headers_json,resp_body,resp_status_code,input_tokens,output_tokens,cached_tokens,cache_creation_tokens,reasoning_tokens,cache_hit_ratio,cache_creation_ratio,repair_applied,prefix_hash,estimated_cost,complete,size_bytes,req_blob,resp_blob)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		record.ID, record.ActivityID, record.RequestID, record.KeyID, record.Model, record.SessionID, record.ReqPath, record.Timestamp.Unix(),
		normalizeJSONBlob(record.RequestHeaders), reqBody, normalizeJSONBlob(record.ResponseHeaders), respBody, record.ResponseStatus,
		record.InputTokens, record.OutputTokens, record.CachedTokens, record.CacheCreationTokens, record.ReasoningTokens, record.CacheHitRatio, record.CacheCreationRatio, boolInt(record.RepairApplied), record.PrefixHash, record.EstimatedCost, boolInt(record.Complete), record.SizeBytes, reqBlobRef, respBlobRef)
	if err != nil {
		return fmt.Errorf("insert audit conversation: %w", err)
	}
	return nil
}

// externalizeBlob moves a non-empty body into the content-addressed blob
// store and returns an empty body plus its hash reference. An empty body,
// an in-memory store, or a failed blob write all fall back to the inline
// BLOB column: losing audit data is worse than going over the byte budget,
// which the background maintenance pass will eventually converge.
func (s *Store) externalizeBlob(body []byte) ([]byte, string) {
	if s.blobs == nil || len(body) == 0 {
		return body, ""
	}
	hash, err := s.blobs.Put(body)
	if err != nil {
		return body, ""
	}
	return []byte{}, hash
}

// recordBlobRefs publishes the blob reference and logical size of an
// externalized body instead of reading it back into memory. The detail view
// then streams each body on demand (see OpenBlob), which keeps one response
// from carrying tens of megabytes of transcript — plus the third that Go's
// base64 encoding of []byte adds on top.
func (s *Store) recordBlobRefs(r *AuditConversation, reqBlob, respBlob string) {
	if s.blobs == nil {
		return
	}
	if reqBlob != "" {
		r.RequestBodyRef = reqBlob
		if size, err := s.blobs.Size(reqBlob); err == nil {
			r.RequestBodyBytes = size
		}
	}
	if respBlob != "" {
		r.ResponseBodyRef = respBlob
		if size, err := s.blobs.Size(respBlob); err == nil {
			r.ResponseBodyBytes = size
		}
	}
}

// OpenBlob streams one stored body. It is the read side of the lazy detail
// view: the caller serves the returned file directly (http.ServeContent) so a
// multi-megabyte transcript never has to be buffered. ErrBlobNotFound means the
// body is not available as a blob — the store has no blob store, or the
// reference is stale.
func (s *Store) OpenBlob(ref string) (*os.File, int64, error) {
	if s == nil || s.blobs == nil {
		return nil, 0, ErrBlobNotFound
	}
	return s.blobs.Open(ref)
}

func normalizeJSONBlob(b []byte) string {
	if len(b) == 0 {
		return "{}"
	}
	return string(b)
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

func (s *Store) ListAuditConversations(ctx context.Context, q AuditQuery) ([]AuditConversation, error) {
	page, err := s.ListAuditConversationsPage(ctx, q)
	if err != nil {
		return nil, err
	}
	return page.Data, nil
}

var auditSortColumns = map[string]string{
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

func auditOrderBy(q AuditQuery) string {
	column, ok := auditSortColumns[q.Sort]
	if !ok {
		column = "ts_created"
	}
	direction := "DESC"
	if strings.EqualFold(q.Order, "asc") {
		direction = "ASC"
	}
	if column == "id" {
		return " ORDER BY id " + direction
	}
	return " ORDER BY " + column + " " + direction + ", id " + direction
}

func auditWhere(q AuditQuery) (string, []any) {
	where := []string{"1=1"}
	args := []any{}
	if q.KeyID != "" {
		where = append(where, "key_id=?")
		args = append(args, q.KeyID)
	}
	if q.Model != "" {
		where = append(where, "model_id=?")
		args = append(args, q.Model)
	} else if models := normalizeAuditModels(q.Models); len(models) > 0 {
		placeholders := make([]string, len(models))
		for i, model := range models {
			placeholders[i] = "?"
			args = append(args, model)
		}
		where = append(where, "model_id IN ("+strings.Join(placeholders, ",")+")")
	}
	if q.SessionID != "" {
		where = append(where, "session_id=?")
		args = append(args, q.SessionID)
	}
	if !q.Start.IsZero() {
		where = append(where, "ts_created>=?")
		args = append(args, q.Start.Unix())
	}
	if !q.End.IsZero() {
		where = append(where, "ts_created<=?")
		args = append(args, q.End.Unix())
	}
	return " WHERE " + strings.Join(where, " AND "), args
}

func normalizeAuditQuery(q AuditQuery) AuditQuery {
	q.Limit, q.Offset = normalizeLimitOffset(q.Limit, q.Offset)
	if q.Page < 1 {
		q.Page = 1
	}
	// Same overflow guard as normalizeActivityQuery: only the lower bound is
	// enforced by the caller, and a huge page wraps the OFFSET negative.
	if q.Page > maxActivityPage {
		q.Page = maxActivityPage
	}
	return q
}

func (s *Store) ListAuditConversationsPage(ctx context.Context, q AuditQuery) (AuditConversationPage, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return AuditConversationPage{}, err
	}
	q = normalizeAuditQuery(q)
	where, args := auditWhere(q)
	var total int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audit_conversations`+where, args...).Scan(&total); err != nil {
		return AuditConversationPage{}, fmt.Errorf("count audit conversations: %w", err)
	}
	offset := q.Offset
	if q.Page > 1 {
		offset = (q.Page - 1) * q.Limit
	} else if q.Page == 1 && q.Offset == 0 {
		offset = 0
	}
	args = append(args, q.Limit, offset)
	// The list view is metadata: raw request/response bodies are only fetched
	// by the single-conversation endpoint. Pulling multi-megabyte blobs for
	// every row on a page made dashboard paging as expensive as opening the
	// conversations themselves.
	rows, err := s.db.QueryContext(ctx, `SELECT id,activity_id,request_id,key_id,model_id,session_id,req_path,ts_created,req_headers_json,resp_headers_json,resp_status_code,input_tokens,output_tokens,cached_tokens,cache_creation_tokens,reasoning_tokens,cache_hit_ratio,cache_creation_ratio,repair_applied,prefix_hash,estimated_cost,complete,size_bytes FROM audit_conversations`+where+auditOrderBy(q)+` LIMIT ? OFFSET ?`, args...)
	if err != nil {
		return AuditConversationPage{}, err
	}
	defer rows.Close()
	var out []AuditConversation
	for rows.Next() {
		var r AuditConversation
		var ts int64
		var reqHeaders, respHeaders string
		var complete, repairApplied int
		if err := rows.Scan(&r.ID, &r.ActivityID, &r.RequestID, &r.KeyID, &r.Model, &r.SessionID, &r.ReqPath, &ts, &reqHeaders, &respHeaders, &r.ResponseStatus, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.CacheCreationTokens, &r.ReasoningTokens, &r.CacheHitRatio, &r.CacheCreationRatio, &repairApplied, &r.PrefixHash, &r.EstimatedCost, &complete, &r.SizeBytes); err != nil {
			return AuditConversationPage{}, err
		}
		r.Timestamp = time.Unix(ts, 0)
		r.RequestHeaders = []byte(reqHeaders)
		r.ResponseHeaders = []byte(respHeaders)
		r.Complete = complete != 0
		r.RepairApplied = repairApplied != 0
		r.CacheHitRatio = normalizeCacheRatio(r.CacheHitRatio)
		r.CacheCreationRatio = normalizeCacheRatio(r.CacheCreationRatio)
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return AuditConversationPage{}, err
	}
	return AuditConversationPage{
		Data:       out,
		Page:       q.Page,
		Limit:      q.Limit,
		Total:      total,
		TotalPages: calculateTotalPages(total, q.Limit),
	}, nil
}

// GetAuditConversation returns one raw conversation by its stable id. The
// control plane uses this before destructive operations so model-scoped keys
// cannot delete a conversation belonging to another model.
func (s *Store) GetAuditConversation(ctx context.Context, id string) (AuditConversation, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return AuditConversation{}, false, errors.New("store is unavailable")
	}
	id = strings.TrimSpace(id)
	if id == "" {
		return AuditConversation{}, false, nil
	}
	var r AuditConversation
	var ts int64
	var reqHeaders, respHeaders, reqBlob, respBlob string
	var complete, repairApplied int
	err := s.db.QueryRowContext(ctx, `SELECT c.id,c.activity_id,c.request_id,c.key_id,c.model_id,c.session_id,c.req_path,c.ts_created,c.req_headers_json,c.req_body,c.resp_headers_json,c.resp_body,c.resp_status_code,c.input_tokens,c.output_tokens,c.cached_tokens,c.cache_creation_tokens,c.reasoning_tokens,c.cache_hit_ratio,c.cache_creation_ratio,c.repair_applied,c.prefix_hash,c.estimated_cost,c.complete,c.size_bytes,c.req_blob,c.resp_blob,COALESCE(a.first_token_ms,-1),COALESCE(a.decode_ms,-1),COALESCE(a.duration_ms,0),COALESCE(a.speed_timeline,'') FROM audit_conversations c LEFT JOIN activity a ON a.id = c.activity_id WHERE c.id=?`, id).
		Scan(&r.ID, &r.ActivityID, &r.RequestID, &r.KeyID, &r.Model, &r.SessionID, &r.ReqPath, &ts, &reqHeaders, &r.RequestBody, &respHeaders, &r.ResponseBody, &r.ResponseStatus, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.CacheCreationTokens, &r.ReasoningTokens, &r.CacheHitRatio, &r.CacheCreationRatio, &repairApplied, &r.PrefixHash, &r.EstimatedCost, &complete, &r.SizeBytes, &reqBlob, &respBlob, &r.FirstTokenMs, &r.DecodeMs, &r.DurationMs, &r.SpeedTimeline)
	if errors.Is(err, sql.ErrNoRows) {
		return AuditConversation{}, false, nil
	}
	if err != nil {
		return AuditConversation{}, false, err
	}
	r.FirstTokenMs = normalizeDecodeMs(r.FirstTokenMs)
	r.DecodeMs = normalizeDecodeMs(r.DecodeMs)
	r.Timestamp = time.Unix(ts, 0)
	r.RequestHeaders = []byte(reqHeaders)
	r.ResponseHeaders = []byte(respHeaders)
	r.Complete = complete != 0
	r.RepairApplied = repairApplied != 0
	r.CacheHitRatio = normalizeCacheRatio(r.CacheHitRatio)
	r.CacheCreationRatio = normalizeCacheRatio(r.CacheCreationRatio)
	s.recordBlobRefs(&r, reqBlob, respBlob)
	return r, true, nil
}

// GetAuditConversationByActivityID is the only lookup used by the unified
// request-record UI. Activity IDs are stable integers, so the detail view does
// not need to know or expose the audit table's internal string key.
func (s *Store) GetAuditConversationByActivityID(ctx context.Context, activityID int) (AuditConversation, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return AuditConversation{}, false, errors.New("store is unavailable")
	}
	if activityID < 1 {
		return AuditConversation{}, false, nil
	}
	var r AuditConversation
	var ts int64
	var reqHeaders, respHeaders, reqBlob, respBlob string
	var complete, repairApplied int
	err := s.db.QueryRowContext(ctx, `SELECT c.id,c.activity_id,c.request_id,c.key_id,c.model_id,c.session_id,c.req_path,c.ts_created,c.req_headers_json,c.req_body,c.resp_headers_json,c.resp_body,c.resp_status_code,c.input_tokens,c.output_tokens,c.cached_tokens,c.cache_creation_tokens,c.reasoning_tokens,c.cache_hit_ratio,c.cache_creation_ratio,c.repair_applied,c.prefix_hash,c.estimated_cost,c.complete,c.size_bytes,c.req_blob,c.resp_blob,COALESCE(a.first_token_ms,-1),COALESCE(a.decode_ms,-1),COALESCE(a.duration_ms,0),COALESCE(a.speed_timeline,'') FROM audit_conversations c LEFT JOIN activity a ON a.id = c.activity_id WHERE c.activity_id=?`, activityID).
		Scan(&r.ID, &r.ActivityID, &r.RequestID, &r.KeyID, &r.Model, &r.SessionID, &r.ReqPath, &ts, &reqHeaders, &r.RequestBody, &respHeaders, &r.ResponseBody, &r.ResponseStatus, &r.InputTokens, &r.OutputTokens, &r.CachedTokens, &r.CacheCreationTokens, &r.ReasoningTokens, &r.CacheHitRatio, &r.CacheCreationRatio, &repairApplied, &r.PrefixHash, &r.EstimatedCost, &complete, &r.SizeBytes, &reqBlob, &respBlob, &r.FirstTokenMs, &r.DecodeMs, &r.DurationMs, &r.SpeedTimeline)
	if errors.Is(err, sql.ErrNoRows) {
		return AuditConversation{}, false, nil
	}
	if err != nil {
		return AuditConversation{}, false, err
	}
	r.FirstTokenMs = normalizeDecodeMs(r.FirstTokenMs)
	r.DecodeMs = normalizeDecodeMs(r.DecodeMs)
	r.Timestamp = time.Unix(ts, 0)
	r.RequestHeaders = []byte(reqHeaders)
	r.ResponseHeaders = []byte(respHeaders)
	r.Complete = complete != 0
	r.RepairApplied = repairApplied != 0
	r.CacheHitRatio = normalizeCacheRatio(r.CacheHitRatio)
	r.CacheCreationRatio = normalizeCacheRatio(r.CacheCreationRatio)
	s.recordBlobRefs(&r, reqBlob, respBlob)
	return r, true, nil
}

func normalizeAuditModels(models []string) []string {
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	out := make([]string, 0, len(models))
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		if _, ok := seen[model]; ok {
			continue
		}
		seen[model] = struct{}{}
		out = append(out, model)
	}
	return out
}

func normalizeLimitOffset(limit, offset int) (int, int) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func (s *Store) DeleteAuditConversation(ctx context.Context, id string) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit_conversations WHERE id=?`, id)
	return err
}

// ClearAuditConversations removes every persisted raw conversation in one
// statement. Unlike a timestamp-based purge this also clears rows whose clock
// is ahead of the application host, which is required for the control-plane
// "clear now" action to be deterministic.
func (s *Store) ClearAuditConversations(ctx context.Context) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit_conversations`)
	return err
}

func (s *Store) PurgeAuditBefore(ctx context.Context, before time.Time) error {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM audit_conversations WHERE ts_created < ?`, before.Unix())
	return err
}

// AuditTotalBytes returns the current logical byte total of the audit table.
// size_bytes is maintained at write time, so the aggregate is a single column
// pass that never materializes conversation bodies.
func (s *Store) AuditTotalBytes(ctx context.Context) (int64, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return 0, errors.New("store is unavailable")
	}
	var total int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(CASE WHEN size_bytes < 0 THEN 0 ELSE size_bytes END), 0) FROM audit_conversations`).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("audit byte total: %w", err)
	}
	return total, nil
}

// PurgeAuditOverBudget enforces the FIFO byte budget in one statement. It
// removes the oldest conversations first (cumulative size measured from the
// newest row) and always retains the newest row, even when that single
// request is larger than the configured budget: a pathological request must
// not erase the only recent conversation. The single statement replaces a
// full-table scan plus a row-by-row deletion cascade, which held the store's
// only connection for as long as the cascade ran and could starve unrelated
// readers.
func (s *Store) PurgeAuditOverBudget(ctx context.Context, maxBytes int64) (int64, error) {
	ctx = normalizeContext(ctx)
	if maxBytes <= 0 {
		return 0, nil
	}
	if s == nil || s.db == nil {
		return 0, errors.New("store is unavailable")
	}
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM audit_conversations
		WHERE id IN (
			SELECT id FROM (
				SELECT id,
					SUM(CASE WHEN size_bytes < 0 THEN 0 ELSE size_bytes END)
						OVER (ORDER BY ts_created DESC, id DESC) AS kept
				FROM audit_conversations
			)
			WHERE kept > ?
			  AND id NOT IN (SELECT id FROM audit_conversations ORDER BY ts_created DESC, id DESC LIMIT 1)
		)`, maxBytes)
	if err != nil {
		return 0, fmt.Errorf("purge audit conversations over budget: %w", err)
	}
	deleted, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("purge audit conversations over budget: %w", err)
	}
	return deleted, nil
}

// GCUnusedBlobs removes blob files no audit conversation references anymore
// (rows purged by retention or the byte budget, deleted individually, or
// cleared). In-memory stores have no blobs and the call is a no-op. It
// returns the number of files removed; the server rates it through the
// background maintenance task.
func (s *Store) GCUnusedBlobs(ctx context.Context) (int, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil || s.blobs == nil {
		return 0, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT req_blob FROM audit_conversations WHERE req_blob <> ''
		UNION
		SELECT DISTINCT resp_blob FROM audit_conversations WHERE resp_blob <> ''`)
	if err != nil {
		return 0, fmt.Errorf("list audit blob references: %w", err)
	}
	defer rows.Close()
	keep := make(map[string]struct{})
	for rows.Next() {
		var ref string
		if err := rows.Scan(&ref); err != nil {
			return 0, fmt.Errorf("list audit blob references: %w", err)
		}
		keep[ref] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("list audit blob references: %w", err)
	}
	return s.blobs.DeleteUnreferenced(keep)
}

func (s *Store) UpsertResponseAffinity(ctx context.Context, a ResponseAffinity) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	if a.ResponseID == "" || a.Model == "" {
		return errors.New("response affinity requires response id and model")
	}
	if len(a.Response) > maxResponseAffinityBytes {
		return fmt.Errorf("response affinity response exceeds %d bytes", maxResponseAffinityBytes)
	}
	now := time.Now()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = a.CreatedAt
	}
	if a.ExpiresAt.IsZero() {
		a.ExpiresAt = now.Add(defaultResponseAffinityRetention)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO response_affinity(response_id,model_id,backend,status,response_json,expires_at,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?) ON CONFLICT(response_id) DO UPDATE SET model_id=excluded.model_id,backend=excluded.backend,status=excluded.status,response_json=excluded.response_json,expires_at=excluded.expires_at,updated_at=excluded.updated_at`, a.ResponseID, a.Model, a.Backend, a.Status, nonNilBytes(a.Response), a.ExpiresAt.Unix(), a.CreatedAt.Unix(), a.UpdatedAt.Unix())
	return err
}

func (s *Store) GetResponseAffinity(ctx context.Context, id string, now time.Time) (ResponseAffinity, bool, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return ResponseAffinity{}, false, errors.New("store is unavailable")
	}
	if id == "" {
		return ResponseAffinity{}, false, nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	var a ResponseAffinity
	var expires, created, updated int64
	err := s.db.QueryRowContext(ctx, `SELECT response_id,model_id,backend,status,response_json,expires_at,created_at,updated_at FROM response_affinity WHERE response_id=?`, id).Scan(&a.ResponseID, &a.Model, &a.Backend, &a.Status, &a.Response, &expires, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return ResponseAffinity{}, false, nil
	}
	if err != nil {
		return a, false, err
	}
	a.ExpiresAt = time.Unix(expires, 0)
	a.CreatedAt = time.Unix(created, 0)
	a.UpdatedAt = time.Unix(updated, 0)
	if !a.ExpiresAt.After(now) {
		_ = s.DeleteResponseAffinity(ctx, id)
		return ResponseAffinity{}, false, nil
	}
	return a, true, nil
}

func (s *Store) DeleteResponseAffinity(ctx context.Context, id string) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM response_affinity WHERE response_id=?`, id)
	return err
}

func (s *Store) DeleteExpiredResponseAffinities(ctx context.Context, now time.Time) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	if now.IsZero() {
		now = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM response_affinity WHERE expires_at<=?`, now.Unix())
	return err
}

// UpsertRuntimeOperation records the latest state for one operation id while
// retaining its original start time. Runtime Manager calls this only after
// its append-only journal has been fsynced, so a transient SQLite failure can
// never make the durable journal lie about a transition.
func (s *Store) UpsertRuntimeOperation(ctx context.Context, operation RuntimeOperation) error {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return errors.New("store is unavailable")
	}
	if strings.TrimSpace(operation.ID) == "" || strings.TrimSpace(operation.RuntimeName) == "" {
		return errors.New("runtime operation id and runtime name are required")
	}
	if operation.UpdatedAt.IsZero() {
		operation.UpdatedAt = time.Now()
	}
	if operation.StartedAt.IsZero() {
		operation.StartedAt = operation.UpdatedAt
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO runtime_operations
		(id,runtime_name,action,state,version,error,started_at,updated_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET runtime_name=excluded.runtime_name,
			action=excluded.action,state=excluded.state,version=excluded.version,
			error=excluded.error,updated_at=excluded.updated_at`,
		operation.ID, operation.RuntimeName, operation.Action, operation.State,
		operation.Version, operation.Error, operation.StartedAt.Unix(), operation.UpdatedAt.Unix())
	if err != nil {
		return fmt.Errorf("upsert runtime operation: %w", err)
	}
	return nil
}

// ListRuntimeOperations returns recent durable runtime operation projections.
// An empty runtimeName lists all runtimes; a positive limit is capped to keep
// control-plane responses bounded.
func (s *Store) ListRuntimeOperations(ctx context.Context, runtimeName string, limit int) ([]RuntimeOperation, error) {
	ctx = normalizeContext(ctx)
	if s == nil || s.db == nil {
		return nil, errors.New("store is unavailable")
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	where := "1=1"
	args := []any{}
	if runtimeName = strings.TrimSpace(runtimeName); runtimeName != "" {
		where = "runtime_name=?"
		args = append(args, runtimeName)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, `SELECT id,runtime_name,action,state,version,error,started_at,updated_at
		FROM runtime_operations WHERE `+where+` ORDER BY updated_at DESC,id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("list runtime operations: %w", err)
	}
	defer rows.Close()
	var operations []RuntimeOperation
	for rows.Next() {
		var operation RuntimeOperation
		var startedAt, updatedAt int64
		if err := rows.Scan(&operation.ID, &operation.RuntimeName, &operation.Action, &operation.State, &operation.Version, &operation.Error, &startedAt, &updatedAt); err != nil {
			return nil, err
		}
		operation.StartedAt = time.Unix(startedAt, 0)
		operation.UpdatedAt = time.Unix(updatedAt, 0)
		operations = append(operations, operation)
	}
	return operations, rows.Err()
}

// UpsertPrice stores one exact provider/model price. Rates are expressed in
// currency units per million tokens. A NULL rate is represented as zero by the
// public type and means that component is unavailable for estimation.
func (s *Store) UpsertPrice(ctx context.Context, price Price) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.ensureDB(); err != nil {
		return err
	}
	if err := validatePrice(price); err != nil {
		return err
	}
	if price.SyncedAt.IsZero() {
		price.SyncedAt = time.Now()
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pricing_catalog
		(provider,model,input_per_million,output_per_million,cache_read_per_million,cache_write_per_million,reasoning_per_million,synced_at)
		VALUES(?,?,?,?,?,?,?,?)
		ON CONFLICT(provider,model) DO UPDATE SET input_per_million=excluded.input_per_million,
		output_per_million=excluded.output_per_million,cache_read_per_million=excluded.cache_read_per_million,
		cache_write_per_million=excluded.cache_write_per_million,reasoning_per_million=excluded.reasoning_per_million,
		synced_at=excluded.synced_at`, price.Provider, price.Model, price.Input, price.Output,
		price.CacheRead, price.CacheWrite, price.Reasoning, price.SyncedAt.Unix())
	return err
}

// ReplacePricingSnapshot atomically replaces the models.dev catalog and its
// ETag metadata. Keeping the delete, inserts, and metadata update in one
// transaction means a cancelled or failed sync leaves the last successful
// snapshot intact instead of exposing a partially refreshed price table.
func (s *Store) ReplacePricingSnapshot(ctx context.Context, prices []Price, meta PricingMeta) error {
	if s == nil || s.db == nil {
		return errors.New("store is not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if meta.SyncedAt.IsZero() {
		meta.SyncedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin pricing snapshot: %w", err)
	}
	rollback := func(cause error) error {
		_ = tx.Rollback()
		return cause
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM pricing_catalog`); err != nil {
		return rollback(fmt.Errorf("clear pricing catalog: %w", err))
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO pricing_catalog
		(provider,model,input_per_million,output_per_million,cache_read_per_million,cache_write_per_million,reasoning_per_million,synced_at)
		VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		return rollback(fmt.Errorf("prepare pricing snapshot: %w", err))
	}
	for _, price := range prices {
		if err := validatePrice(price); err != nil {
			_ = stmt.Close()
			return rollback(err)
		}
		if price.SyncedAt.IsZero() {
			price.SyncedAt = meta.SyncedAt
		}
		if _, err := stmt.ExecContext(ctx, price.Provider, price.Model, price.Input, price.Output, price.CacheRead, price.CacheWrite, price.Reasoning, price.SyncedAt.Unix()); err != nil {
			_ = stmt.Close()
			return rollback(fmt.Errorf("insert pricing snapshot: %w", err))
		}
	}
	if err := stmt.Close(); err != nil {
		return rollback(fmt.Errorf("close pricing snapshot: %w", err))
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO pricing_meta(id,etag,synced_at,source) VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET etag=excluded.etag,synced_at=excluded.synced_at,source=excluded.source`, meta.ETag, meta.SyncedAt.Unix(), meta.Source); err != nil {
		return rollback(fmt.Errorf("update pricing metadata: %w", err))
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit pricing snapshot: %w", err)
	}
	return nil
}

func validatePrice(price Price) error {
	if err := validatePriceIdentity("provider", price.Provider, false); err != nil {
		return err
	}
	if err := validatePriceIdentity("model", price.Model, true); err != nil {
		return err
	}
	for _, rate := range []struct {
		name  string
		value float64
	}{
		{name: "input", value: price.Input}, {name: "output", value: price.Output},
		{name: "cache read", value: price.CacheRead}, {name: "cache write", value: price.CacheWrite},
		{name: "reasoning", value: price.Reasoning},
	} {
		if math.IsNaN(rate.value) || math.IsInf(rate.value, 0) || rate.value < 0 {
			return fmt.Errorf("price %s rate must be a finite non-negative number", rate.name)
		}
	}
	return nil
}

// validatePriceIdentity applies the same conservative identity boundary used
// by the models.dev importer to direct Store writes. Pricing lookups are exact
// provider/model matches and the values are exposed by the control-plane UI;
// rejecting surrounding padding and Unicode format/control characters avoids
// visually-colliding entries when an operator or an integration bypasses the
// importer. Provider is optional because the resolver supports provider-less
// unique matches, while model is always required.
func validatePriceIdentity(field, value string, required bool) error {
	if required && strings.TrimSpace(value) == "" {
		return fmt.Errorf("price %s is required", field)
	}
	if value == "" {
		return nil
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("price %s must not have surrounding whitespace", field)
	}
	if len(value) > maxPriceIdentityBytes {
		return fmt.Errorf("price %s exceeds %d bytes", field, maxPriceIdentityBytes)
	}
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || (unicode.IsSpace(r) && r != ' ') {
			return fmt.Errorf("price %s contains control or invisible whitespace", field)
		}
	}
	return nil
}

func (s *Store) GetPricingMeta(ctx context.Context) (PricingMeta, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.ensureDB(); err != nil {
		return PricingMeta{}, err
	}
	var meta PricingMeta
	var synced int64
	err := s.db.QueryRowContext(ctx, `SELECT etag,synced_at,source FROM pricing_meta WHERE id=1`).Scan(&meta.ETag, &synced, &meta.Source)
	if errors.Is(err, sql.ErrNoRows) {
		return meta, nil
	}
	if err != nil {
		return meta, err
	}
	if synced > 0 {
		meta.SyncedAt = time.Unix(synced, 0)
	}
	return meta, nil
}

func (s *Store) SetPricingMeta(ctx context.Context, meta PricingMeta) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.ensureDB(); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO pricing_meta(id,etag,synced_at,source) VALUES(1,?,?,?) ON CONFLICT(id) DO UPDATE SET etag=excluded.etag,synced_at=excluded.synced_at,source=excluded.source`, meta.ETag, meta.SyncedAt.Unix(), meta.Source)
	return err
}

func (s *Store) FindPrices(ctx context.Context, provider, model string) ([]Price, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.ensureDB(); err != nil {
		return nil, err
	}
	where := "1=1"
	args := []any{}
	if provider = strings.TrimSpace(provider); provider != "" {
		where += " AND provider=?"
		args = append(args, provider)
	}
	if model = strings.TrimSpace(model); model != "" {
		where += " AND model=?"
		args = append(args, model)
	}
	rows, err := s.db.QueryContext(ctx, `SELECT provider,model,input_per_million,output_per_million,cache_read_per_million,cache_write_per_million,reasoning_per_million,synced_at FROM pricing_catalog WHERE `+where+` ORDER BY provider,model`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Price
	for rows.Next() {
		var p Price
		var synced int64
		if err := rows.Scan(&p.Provider, &p.Model, &p.Input, &p.Output, &p.CacheRead, &p.CacheWrite, &p.Reasoning, &synced); err != nil {
			return nil, err
		}
		p.SyncedAt = time.Unix(synced, 0)
		out = append(out, p)
	}
	return out, rows.Err()
}

// UsageSummary aggregates activity with optional key/model/session/time
// filters. It intentionally does not expose request bodies.
func (s *Store) UsageSummary(ctx context.Context, filter ActivityFilter) (UsageSummary, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return UsageSummary{}, err
	}
	where, args := activityWhere(filter)
	aggregates, err := s.aggregateActivity(ctx, where, args)
	if err != nil {
		return UsageSummary{}, err
	}
	summary := usageSummaryFromAggregate(aggregates.total)
	summary.ByModel = modelUsageRows(aggregates.byModel)
	return summary, nil
}

func modelUsageRows(byModel map[string]*activityAggregate) []ModelUsage {
	models := make([]string, 0, len(byModel))
	for model := range byModel {
		models = append(models, model)
	}
	sort.Strings(models)
	rows := make([]ModelUsage, 0, len(models))
	for _, model := range models {
		if usage := byModel[model]; usage != nil {
			rows = append(rows, modelUsageFromAggregate(model, *usage))
		}
	}
	return rows
}

// activityAggregate is intentionally computed without SQL SUM(). SQLite
// raises an integer overflow error when a set of rows exceeds int64; control
// plane metrics should remain readable and bounded even when a legacy/imported
// row is malformed. Values are normalized to non-negative saturated counters.
type activityAggregate struct {
	Requests            int
	InputTokens         int
	OutputTokens        int
	CachedTokens        int
	CacheCreationTokens int
	ReasoningTokens     int
	EstimatedCost       float64
	CostEstimated       bool
}

type activityAggregates struct {
	total   activityAggregate
	byModel map[string]*activityAggregate
}

func (s *Store) aggregateActivity(ctx context.Context, where string, args []any) (activityAggregates, error) {
	ctx = normalizeContext(ctx)
	if err := s.ensureDB(); err != nil {
		return activityAggregates{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT model_id,input_tokens,output_tokens,cache_tokens,cache_creation_tokens,reasoning_tokens,estimated_cost,cost_estimated FROM activity`+where, args...)
	if err != nil {
		return activityAggregates{}, fmt.Errorf("aggregate activity: %w", err)
	}
	defer rows.Close()
	aggregates := activityAggregates{byModel: make(map[string]*activityAggregate)}
	for rows.Next() {
		var (
			model                         string
			input, output, cached, create sql.NullInt64
			reasoning                     sql.NullInt64
			estimatedCost                 sql.NullFloat64
			costEstimated                 sql.NullInt64
		)
		if err := rows.Scan(&model, &input, &output, &cached, &create, &reasoning, &estimatedCost, &costEstimated); err != nil {
			return activityAggregates{}, fmt.Errorf("aggregate activity row: %w", err)
		}
		addActivityAggregate(&aggregates.total, input, output, cached, create, reasoning, estimatedCost, costEstimated)
		modelAggregate := aggregates.byModel[model]
		if modelAggregate == nil {
			modelAggregate = &activityAggregate{}
			aggregates.byModel[model] = modelAggregate
		}
		addActivityAggregate(modelAggregate, input, output, cached, create, reasoning, estimatedCost, costEstimated)
	}
	if err := rows.Err(); err != nil {
		return activityAggregates{}, fmt.Errorf("aggregate activity rows: %w", err)
	}
	return aggregates, nil
}

func addActivityAggregate(aggregate *activityAggregate, input, output, cached, create, reasoning sql.NullInt64, estimatedCost sql.NullFloat64, costEstimated sql.NullInt64) {
	if aggregate == nil {
		return
	}
	aggregate.Requests = saturatingIntAdd(aggregate.Requests, 1)
	aggregate.InputTokens = saturatingIntAdd(aggregate.InputTokens, boundedNonNegativeInt(input))
	aggregate.OutputTokens = saturatingIntAdd(aggregate.OutputTokens, boundedNonNegativeInt(output))
	aggregate.CachedTokens = saturatingIntAdd(aggregate.CachedTokens, boundedNonNegativeInt(cached))
	aggregate.CacheCreationTokens = saturatingIntAdd(aggregate.CacheCreationTokens, boundedNonNegativeInt(create))
	aggregate.ReasoningTokens = saturatingIntAdd(aggregate.ReasoningTokens, boundedNonNegativeInt(reasoning))
	aggregate.EstimatedCost = saturatingCostAdd(aggregate.EstimatedCost, estimatedCost)
	if costEstimated.Valid && costEstimated.Int64 != 0 {
		aggregate.CostEstimated = true
	}
}

func boundedNonNegativeInt(value sql.NullInt64) int {
	if !value.Valid || value.Int64 <= 0 {
		return 0
	}
	maxInt := int(^uint(0) >> 1)
	if uint64(value.Int64) > uint64(maxInt) {
		return maxInt
	}
	return int(value.Int64)
}

func saturatingCostAdd(current float64, value sql.NullFloat64) float64 {
	if !value.Valid || math.IsNaN(value.Float64) || math.IsInf(value.Float64, 0) || value.Float64 <= 0 {
		return current
	}
	if math.IsNaN(current) || math.IsInf(current, 1) || current < 0 {
		current = 0
	}
	if math.IsInf(current, -1) || current > math.MaxFloat64-value.Float64 {
		return math.MaxFloat64
	}
	return current + value.Float64
}

func usageSummaryFromAggregate(aggregate activityAggregate) UsageSummary {
	return UsageSummary{
		Requests:            aggregate.Requests,
		InputTokens:         aggregate.InputTokens,
		OutputTokens:        aggregate.OutputTokens,
		CachedTokens:        aggregate.CachedTokens,
		CacheCreationTokens: aggregate.CacheCreationTokens,
		ReasoningTokens:     aggregate.ReasoningTokens,
		EstimatedCost:       aggregate.EstimatedCost,
		CostEstimated:       aggregate.CostEstimated,
		CacheHitRatio:       aggregateCacheHitRatio(aggregate.InputTokens, aggregate.CachedTokens, aggregate.CacheCreationTokens),
		CacheCreationRatio:  aggregateCacheCreationRatio(aggregate.InputTokens, aggregate.CachedTokens, aggregate.CacheCreationTokens),
	}
}

func modelUsageFromAggregate(model string, aggregate activityAggregate) ModelUsage {
	return ModelUsage{
		Model:               model,
		Requests:            aggregate.Requests,
		InputTokens:         aggregate.InputTokens,
		OutputTokens:        aggregate.OutputTokens,
		CachedTokens:        aggregate.CachedTokens,
		CacheCreationTokens: aggregate.CacheCreationTokens,
		ReasoningTokens:     aggregate.ReasoningTokens,
		EstimatedCost:       aggregate.EstimatedCost,
		CostEstimated:       aggregate.CostEstimated,
		CacheHitRatio:       aggregateCacheHitRatio(aggregate.InputTokens, aggregate.CachedTokens, aggregate.CacheCreationTokens),
		CacheCreationRatio:  aggregateCacheCreationRatio(aggregate.InputTokens, aggregate.CachedTokens, aggregate.CacheCreationTokens),
	}
}

// cacheAwareInputTotal returns the cache-aware input denominator for a token
// aggregate. InputTokens is the uncached remainder for Anthropic's
// partitioned usage shape, where the cache partitions are additive; for
// OpenAI-compatible usage it already includes them, and adding them again
// double counts. A partition sum larger than the input total cannot be part
// of it, so only that case adds the partitions.
func cacheAwareInputTotal(input, cached, creation int) int {
	partitions := saturatingIntAdd(cached, creation)
	if partitions > input {
		return saturatingIntAdd(input, partitions)
	}
	return input
}

// aggregateCacheHitRatio uses the explicit cache partitions when available.
// The denominator follows the cache-aware input total so mixed aggregates do
// not double count providers that include cached tokens in input tokens;
// treating it as an estimate keeps the aggregate conservative without
// pretending it is billing data.
func aggregateCacheHitRatio(input, cached, creation int) float64 {
	input = nonNegativeInt(input)
	cached = nonNegativeInt(cached)
	creation = nonNegativeInt(creation)
	denominator := cacheAwareInputTotal(input, cached, creation)
	if denominator <= 0 || cached <= 0 {
		return 0
	}
	ratio := float64(cached) / float64(denominator)
	if ratio < 0 {
		return 0
	}
	if ratio > 1 {
		return 1
	}
	return ratio
}

// aggregateCacheCreationRatio mirrors aggregateCacheHitRatio for cache-write
// partitions. The denominator intentionally uses the same cache-aware input
// total so hit and creation percentages remain comparable even when an
// upstream provider reports total input tokens instead of an explicit
// uncached partition.
func aggregateCacheCreationRatio(input, cached, creation int) float64 {
	input = nonNegativeInt(input)
	cached = nonNegativeInt(cached)
	creation = nonNegativeInt(creation)
	denominator := cacheAwareInputTotal(input, cached, creation)
	if denominator <= 0 || creation <= 0 {
		return 0
	}
	ratio := float64(creation) / float64(denominator)
	return normalizeCacheRatio(ratio)
}

func cacheCreationRatio(input, cached, creation int) float64 {
	return aggregateCacheCreationRatio(input, cached, creation)
}

func normalizeCacheRatio(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
		return 0
	}
	if value >= 1 {
		return 1
	}
	return value
}

func nonNegativeInt(value int) int {
	if value < 0 {
		return 0
	}
	return value
}

func saturatingIntAdd(values ...int) int {
	maxInt := int(^uint(0) >> 1)
	var total int
	for _, value := range values {
		if value <= 0 {
			continue
		}
		if total > maxInt-value {
			return maxInt
		}
		total += value
	}
	return total
}

func saturatingInt64Add(a, b int64) int64 {
	if a < 0 {
		a = 0
	}
	if b < 0 {
		b = 0
	}
	if a > math.MaxInt64-b {
		return math.MaxInt64
	}
	return a + b
}
