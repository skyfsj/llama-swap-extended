package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func nilStoreContext() context.Context { return nil }

func TestStore_RevokeMissingAPIKeyReturnsNotFound(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.RevokeAPIKey(context.Background(), "missing", time.Now()); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("error=%v, want ErrAPIKeyNotFound", err)
	}
}

func TestStore_ControlMethodsAcceptNilContext(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()

	if err := s.UpsertAPIKey(nilStoreContext(), APIKeyRecord{ID: "nil-key", Name: "nil", KeyHash: KeyFingerprint("nil-secret"), Scopes: []string{"inference"}, CreatedAt: now}); err != nil {
		t.Fatalf("UpsertAPIKey: %v", err)
	}
	if _, err := s.ListAPIKeys(nilStoreContext(), true); err != nil {
		t.Fatalf("ListAPIKeys: %v", err)
	}
	if _, found, err := s.FindAPIKeyByHash(nilStoreContext(), KeyFingerprint("nil-secret")); err != nil || !found {
		t.Fatalf("FindAPIKeyByHash: found=%v err=%v", found, err)
	}
	if _, found, err := s.FindAPIKeyBySecret(nilStoreContext(), "nil-secret"); err != nil || !found {
		t.Fatalf("FindAPIKeyBySecret: found=%v err=%v", found, err)
	}
	if err := s.TouchAPIKey(nilStoreContext(), "nil-key", now); err != nil {
		t.Fatalf("TouchAPIKey: %v", err)
	}

	if err := s.InsertAuditConversation(nilStoreContext(), AuditConversation{ID: "nil-audit", Model: "m", Timestamp: now}); err != nil {
		t.Fatalf("InsertAuditConversation: %v", err)
	}
	if _, err := s.ListAuditConversations(nilStoreContext(), AuditQuery{Limit: 10}); err != nil {
		t.Fatalf("ListAuditConversations: %v", err)
	}
	if err := s.DeleteAuditConversation(nilStoreContext(), "nil-audit"); err != nil {
		t.Fatalf("DeleteAuditConversation: %v", err)
	}
	if err := s.PurgeAuditBefore(nilStoreContext(), now.Add(time.Hour)); err != nil {
		t.Fatalf("PurgeAuditBefore: %v", err)
	}
	if _, err := s.PurgeAuditOverBudget(nilStoreContext(), 1); err != nil {
		t.Fatalf("PurgeAuditOverBudget: %v", err)
	}
	if _, err := s.AuditTotalBytes(nilStoreContext()); err != nil {
		t.Fatalf("AuditTotalBytes: %v", err)
	}
	if err := s.ClearAuditConversations(nilStoreContext()); err != nil {
		t.Fatalf("ClearAuditConversations: %v", err)
	}

	if err := s.UpsertResponseAffinity(nilStoreContext(), ResponseAffinity{ResponseID: "nil-response", Model: "m", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatalf("UpsertResponseAffinity: %v", err)
	}
	if _, found, err := s.GetResponseAffinity(nilStoreContext(), "nil-response", now); err != nil || !found {
		t.Fatalf("GetResponseAffinity: found=%v err=%v", found, err)
	}
	if err := s.DeleteExpiredResponseAffinities(nilStoreContext(), now.Add(-time.Hour)); err != nil {
		t.Fatalf("DeleteExpiredResponseAffinities: %v", err)
	}
	if err := s.DeleteResponseAffinity(nilStoreContext(), "nil-response"); err != nil {
		t.Fatalf("DeleteResponseAffinity: %v", err)
	}

	if err := s.UpsertPrice(nilStoreContext(), Price{Provider: "p", Model: "m"}); err != nil {
		t.Fatalf("UpsertPrice: %v", err)
	}
	if _, err := s.FindPrices(nilStoreContext(), "p", "m"); err != nil {
		t.Fatalf("FindPrices: %v", err)
	}
	if _, err := s.GetPricingMeta(nilStoreContext()); err != nil {
		t.Fatalf("GetPricingMeta: %v", err)
	}
	if err := s.SetPricingMeta(nilStoreContext(), PricingMeta{ETag: "nil", SyncedAt: now}); err != nil {
		t.Fatalf("SetPricingMeta: %v", err)
	}
	if _, err := s.UsageSummary(nilStoreContext(), ActivityFilter{}); err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
}

func TestStore_ControlPlaneAPIKeysAndAffinity(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Second)
	if err := s.UpsertAPIKey(context.Background(), APIKeyRecord{ID: "k1", Name: "test", KeyHash: KeyFingerprint("secret"), KeySecret: "secret", Scopes: []string{"inference"}, AllowManagementLogin: true, CreatedAt: now}); err != nil {
		t.Fatal(err)
	}
	key, found, err := s.FindAPIKeyByHash(context.Background(), KeyFingerprint("secret"))
	if err != nil || !found || key.ID != "k1" {
		t.Fatalf("key=%+v found=%v err=%v", key, found, err)
	}
	if !key.AllowManagementLogin {
		t.Fatal("management-panel login flag was not persisted")
	}
	if key.KeySecret != "" {
		t.Fatalf("ordinary key lookup exposed retained secret=%q", key.KeySecret)
	}
	retained, found, err := s.GetAPIKeySecret(context.Background(), "k1")
	if err != nil || !found || retained != "secret" {
		t.Fatalf("GetAPIKeySecret secret=%q found=%v err=%v", retained, found, err)
	}
	keys, err := s.ListAPIKeys(context.Background(), false)
	if err != nil || len(keys) != 1 || keys[0].KeySecret != "" {
		t.Fatalf("ordinary key list exposed retained secret: keys=%+v err=%v", keys, err)
	}
	key.Name = "updated"
	if err := s.UpsertAPIKey(context.Background(), key); err != nil {
		t.Fatal(err)
	}
	retained, found, err = s.GetAPIKeySecret(context.Background(), "k1")
	if err != nil || !found || retained != "secret" {
		t.Fatalf("metadata update lost retained secret=%q found=%v err=%v", retained, found, err)
	}
	encoded, err := json.Marshal(key)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(encoded, []byte("secret")) {
		t.Fatalf("API key secret leaked through JSON: %s", encoded)
	}
	if err := s.UpsertResponseAffinity(context.Background(), ResponseAffinity{ResponseID: "resp_1", Model: "m1", Backend: "vllm", Status: "completed", ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now}); err != nil {
		t.Fatal(err)
	}
	affinity, found, err := s.GetResponseAffinity(context.Background(), "resp_1", now)
	if err != nil || !found || affinity.Model != "m1" {
		t.Fatalf("affinity=%+v found=%v err=%v", affinity, found, err)
	}
}

func TestStore_ResponseAffinityDefaultsRetentionAndBounds(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	before := time.Now()
	if err := s.UpsertResponseAffinity(context.Background(), ResponseAffinity{
		ResponseID: "resp-default-retention",
		Model:      "m",
		Response:   []byte(`{"id":"resp-default-retention"}`),
	}); err != nil {
		t.Fatalf("default retention upsert: %v", err)
	}
	affinity, found, err := s.GetResponseAffinity(context.Background(), "resp-default-retention", time.Now())
	if err != nil || !found {
		t.Fatalf("default retention affinity=%+v found=%v err=%v", affinity, found, err)
	}
	if affinity.ExpiresAt.Before(before.Add(29*24*time.Hour)) || affinity.ExpiresAt.After(time.Now().Add(31*24*time.Hour)) {
		t.Fatalf("default expiry=%v, want approximately 30 days from write", affinity.ExpiresAt)
	}

	if err := s.UpsertResponseAffinity(context.Background(), ResponseAffinity{
		ResponseID: "resp-too-large-store",
		Model:      "m",
		Response:   bytes.Repeat([]byte{'x'}, maxResponseAffinityBytes+1),
	}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized response error=%v, want bounded store error", err)
	}
	if _, found, err := s.GetResponseAffinity(context.Background(), "resp-too-large-store", time.Now()); err != nil || found {
		t.Fatalf("oversized response persisted: found=%v err=%v", found, err)
	}
}

func TestStore_ResponseAffinityUnavailableAndZeroTimeBoundaries(t *testing.T) {
	var unavailable *Store
	if err := unavailable.UpsertResponseAffinity(nilStoreContext(), ResponseAffinity{ResponseID: "resp", Model: "m"}); err == nil {
		t.Fatal("nil store upsert unexpectedly succeeded")
	}
	if _, found, err := unavailable.GetResponseAffinity(nilStoreContext(), "resp", time.Time{}); err == nil || found {
		t.Fatalf("nil store get = found=%v err=%v, want unavailable error", found, err)
	}
	if err := unavailable.DeleteResponseAffinity(nilStoreContext(), "resp"); err == nil {
		t.Fatal("nil store delete unexpectedly succeeded")
	}
	if err := unavailable.DeleteExpiredResponseAffinities(nilStoreContext(), time.Time{}); err == nil {
		t.Fatal("nil store expiry cleanup unexpectedly succeeded")
	}
}

func TestStore_RuntimeOperationsUpsertAndList(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	started := time.Unix(100, 0)
	updated := started.Add(time.Minute)
	if err := s.UpsertRuntimeOperation(context.Background(), RuntimeOperation{
		ID: "op-1", RuntimeName: "vllm", Action: "stage", State: "DOWNLOADING",
		Version: "1.0", StartedAt: started, UpdatedAt: started,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRuntimeOperation(context.Background(), RuntimeOperation{
		ID: "op-1", RuntimeName: "vllm", Action: "stage", State: "STAGED",
		Version: "1.0", Error: "", StartedAt: updated, UpdatedAt: updated,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertRuntimeOperation(context.Background(), RuntimeOperation{
		ID: "op-2", RuntimeName: "llamacpp", Action: "check", State: "ACTIVE",
		Version: "2.0", StartedAt: updated.Add(time.Minute), UpdatedAt: updated.Add(time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	operations, err := s.ListRuntimeOperations(context.Background(), "vllm", 10)
	if err != nil || len(operations) != 1 {
		t.Fatalf("vllm operations=%+v err=%v", operations, err)
	}
	if operations[0].ID != "op-1" || operations[0].State != "STAGED" || !operations[0].StartedAt.Equal(started) || !operations[0].UpdatedAt.Equal(updated) {
		t.Fatalf("operation=%+v, want latest state with original start", operations[0])
	}
	all, err := s.ListRuntimeOperations(nilStoreContext(), "", 10)
	if err != nil || len(all) != 2 || all[0].ID != "op-2" {
		t.Fatalf("all operations=%+v err=%v", all, err)
	}

	// The projection must survive a store reopen, not just an in-memory test.
	diskPath := filepath.Join(t.TempDir(), "control.db")
	disk, err := New(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := disk.UpsertRuntimeOperation(context.Background(), RuntimeOperation{
		ID: "persisted", RuntimeName: "vllm", Action: "activate", State: "ACTIVE",
		Version: "1.0", StartedAt: started, UpdatedAt: updated,
	}); err != nil {
		disk.Close()
		t.Fatal(err)
	}
	if err := disk.Close(); err != nil {
		t.Fatal(err)
	}
	disk, err = New(diskPath)
	if err != nil {
		t.Fatal(err)
	}
	defer disk.Close()
	persisted, err := disk.ListRuntimeOperations(context.Background(), "vllm", 10)
	if err != nil || len(persisted) != 1 || persisted[0].ID != "persisted" || persisted[0].Action != "activate" {
		t.Fatalf("persisted operations=%+v err=%v", persisted, err)
	}
}

func TestStore_RotateAPIKeyIsAtomic(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.UpsertAPIKey(ctx, APIKeyRecord{ID: "old", Name: "old", KeyHash: KeyFingerprint("old-secret"), KeySecret: "old-secret", Scopes: []string{"inference"}}); err != nil {
		t.Fatal(err)
	}
	if err := s.RotateAPIKey(ctx, "old", APIKeyRecord{ID: "new", Name: "new", KeyHash: KeyFingerprint("new-secret"), KeySecret: "new-secret", Scopes: []string{"logs"}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	old, found, err := s.FindAPIKeyByHash(ctx, KeyFingerprint("old-secret"))
	if err != nil || !found || old.RevokedAt == nil {
		t.Fatalf("old key = %+v found=%v err=%v, want revoked", old, found, err)
	}
	newKey, found, err := s.FindAPIKeyByHash(ctx, KeyFingerprint("new-secret"))
	if err != nil || !found || newKey.RevokedAt != nil {
		t.Fatalf("new key = %+v found=%v err=%v, want active", newKey, found, err)
	}
	if newKey.KeySecret != "" {
		t.Fatalf("ordinary rotated-key lookup exposed secret=%q", newKey.KeySecret)
	}
	retained, found, err := s.GetAPIKeySecret(ctx, "new")
	if err != nil || !found || retained != "new-secret" {
		t.Fatalf("rotated retained secret=%q found=%v err=%v", retained, found, err)
	}

	if err := s.RotateAPIKey(ctx, "missing", APIKeyRecord{ID: "never", KeyHash: KeyFingerprint("never")}, time.Now()); !errors.Is(err, ErrAPIKeyNotFound) {
		t.Fatalf("expected rotation of missing key to return ErrAPIKeyNotFound, got %v", err)
	}
	if _, found, err := s.FindAPIKeyByHash(ctx, KeyFingerprint("never")); err != nil || found {
		t.Fatalf("failed rotation inserted replacement: found=%v err=%v", found, err)
	}
}

func TestStore_AuditConversationQueryAndPurge(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	t0 := time.Now().Add(-2 * time.Hour)
	if err := s.InsertAuditConversation(context.Background(), AuditConversation{ID: "a1", KeyID: "k1", Model: "m1", SessionID: "s1", Timestamp: t0, RequestBody: []byte("request"), ResponseBody: []byte("response"), Complete: true}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAuditConversations(context.Background(), AuditQuery{KeyID: "k1", Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != "a1" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
	if err := s.PurgeAuditBefore(context.Background(), time.Now().Add(-time.Hour)); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAuditConversations(context.Background(), AuditQuery{Limit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("rows after purge=%+v err=%v", rows, err)
	}
}

func TestStore_AuditConversationLinksToActivityID(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	ctx := context.Background()
	activity, err := s.InsertActivity(ctx, ActivityLogEntry{
		Timestamp:      time.Unix(1700000000, 0),
		Model:          "linked-model",
		ReqPath:        "/v1/chat/completions",
		RespStatusCode: 200,
		Tokens: TokenMetrics{
			CachedTokens: 4,
			InputTokens:  12,
			OutputTokens: 8,
		},
	})
	if err != nil {
		t.Fatalf("InsertActivity: %v", err)
	}
	if err := s.InsertAuditConversation(ctx, AuditConversation{
		ID:             "internal-audit-key",
		ActivityID:     activity.ID,
		Model:          activity.Model,
		ReqPath:        activity.ReqPath,
		Timestamp:      activity.Timestamp,
		ResponseStatus: activity.RespStatusCode,
		InputTokens:    activity.Tokens.InputTokens,
		OutputTokens:   activity.Tokens.OutputTokens,
		CachedTokens:   activity.Tokens.CachedTokens,
		RequestBody:    []byte(`{"model":"linked-model"}`),
		ResponseBody:   []byte(`{"ok":true}`),
		Complete:       true,
	}); err != nil {
		t.Fatalf("InsertAuditConversation: %v", err)
	}

	page, err := s.ListActivity(ctx, ActivityQuery{Limit: 1, Page: 1})
	if err != nil || len(page.Data) != 1 {
		t.Fatalf("ListActivity page=%+v err=%v", page, err)
	}
	if !page.Data[0].HasAudit || page.Data[0].ID != activity.ID {
		t.Fatalf("activity=%+v, want has_audit with id %d", page.Data[0], activity.ID)
	}
	conversation, found, err := s.GetAuditConversationByActivityID(ctx, activity.ID)
	if err != nil || !found {
		t.Fatalf("GetAuditConversationByActivityID found=%v err=%v", found, err)
	}
	if conversation.ActivityID != activity.ID || conversation.ID != "internal-audit-key" {
		t.Fatalf("conversation=%+v", conversation)
	}
}

func TestStore_AuditConversationPageSortsAndCounts(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Second)
	for _, conversation := range []AuditConversation{
		{ID: "audit-c", Model: "m-c", Timestamp: now.Add(2 * time.Minute)},
		{ID: "audit-a", Model: "m-a", Timestamp: now},
		{ID: "audit-b", Model: "m-b", Timestamp: now.Add(time.Minute)},
	} {
		if err := s.InsertAuditConversation(context.Background(), conversation); err != nil {
			t.Fatal(err)
		}
	}

	first, err := s.ListAuditConversationsPage(context.Background(), AuditQuery{
		Limit: 2, Page: 1, Sort: "id", Order: "asc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 3 || first.TotalPages != 2 || first.Page != 1 || first.Limit != 2 {
		t.Fatalf("page metadata=%+v", first)
	}
	if len(first.Data) != 2 || first.Data[0].ID != "audit-a" || first.Data[1].ID != "audit-b" {
		t.Fatalf("first page order=%+v", first.Data)
	}

	second, err := s.ListAuditConversationsPage(context.Background(), AuditQuery{
		Limit: 2, Page: 2, Sort: "id", Order: "asc",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Data) != 1 || second.Data[0].ID != "audit-c" {
		t.Fatalf("second page order=%+v", second.Data)
	}
}

func TestStore_AuditModelAllowlistAndNewestOversizedRow(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, conversation := range []AuditConversation{
		{ID: "old", Model: "m-old", Timestamp: now.Add(-2 * time.Minute), RequestBody: []byte("old")},
		{ID: "new", Model: "m-new", Timestamp: now, RequestBody: []byte("new")},
	} {
		if err := s.InsertAuditConversation(context.Background(), conversation); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.ListAuditConversations(context.Background(), AuditQuery{Models: []string{"m-new"}, Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].Model != "m-new" {
		t.Fatalf("allowlist rows=%+v err=%v", rows, err)
	}
	// The newest row is larger than the budget on its own. The FIFO policy
	// drops the old row but keeps the newest diagnostic conversation.
	if _, err := s.PurgeAuditOverBudget(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	rows, err = s.ListAuditConversations(context.Background(), AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != "new" {
		t.Fatalf("rows after oversized purge=%+v err=%v", rows, err)
	}
}

// auditOverBudgetRows seeds a fresh store with three conversations whose
// cumulative size measured from the newest row is c=1, b=3, a=6, runs the
// byte-budget purge, and returns the surviving ids.
func auditOverBudgetRows(t *testing.T, budget int64) ([]string, int64) {
	t.Helper()
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Errorf("store.Close: %v", err)
		}
	})
	now := time.Now().Truncate(time.Second)
	for _, c := range []AuditConversation{
		{ID: "a", Model: "m", Timestamp: now.Add(-3 * time.Minute), RequestBody: bytes.Repeat([]byte("x"), 3)},
		{ID: "b", Model: "m", Timestamp: now.Add(-2 * time.Minute), RequestBody: bytes.Repeat([]byte("x"), 2)},
		{ID: "c", Model: "m", Timestamp: now.Add(-1 * time.Minute), RequestBody: bytes.Repeat([]byte("x"), 1)},
	} {
		if err := s.InsertAuditConversation(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	deleted, err := s.PurgeAuditOverBudget(context.Background(), budget)
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAuditConversations(context.Background(), AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, len(rows))
	for _, r := range rows {
		ids = append(ids, r.ID)
	}
	return ids, deleted
}

func sortedIDs(t *testing.T, ids []string) string {
	t.Helper()
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// The budget comparison is strict: a row whose cumulative size from the
// newest row equals the budget is retained, and the newest row is always kept.
func TestStore_PurgeAuditOverBudgetBoundary(t *testing.T) {
	if got, deleted := auditOverBudgetRows(t, 6); deleted != 0 || sortedIDs(t, got) != "a,b,c" {
		t.Fatalf("budget=6: ids=%v deleted=%d, want all rows retained", got, deleted)
	}
	// b's cumulative is exactly 3 == budget: strict > keeps it, only a goes.
	if got, deleted := auditOverBudgetRows(t, 3); deleted != 1 || sortedIDs(t, got) != "b,c" {
		t.Fatalf("budget=3: ids=%v deleted=%d, want b,c retained", got, deleted)
	}
	// The newest row alone exceeds the budget: everything older is dropped,
	// the newest conversation survives as the only recent diagnostic data.
	if got, deleted := auditOverBudgetRows(t, 1); deleted != 2 || sortedIDs(t, got) != "c" {
		t.Fatalf("budget=1: ids=%v deleted=%d, want only newest retained", got, deleted)
	}
}

// A corrupted negative size_bytes must be clamped to zero in both the byte
// total and the cumulative purge so one bad row cannot hide the whole table
// from the budget.
func TestStore_PurgeAuditOverBudgetClampsNegativeSize(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Second)
	for _, c := range []AuditConversation{
		{ID: "old", Model: "m", Timestamp: now.Add(-2 * time.Minute), RequestBody: bytes.Repeat([]byte("x"), 100)},
		{ID: "neg", Model: "m", Timestamp: now.Add(-1 * time.Minute), RequestBody: bytes.Repeat([]byte("x"), 100)},
	} {
		if err := s.InsertAuditConversation(context.Background(), c); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.db.Exec(`UPDATE audit_conversations SET size_bytes=-1000 WHERE id='neg'`); err != nil {
		t.Fatal(err)
	}
	total, err := s.AuditTotalBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if total != 100 {
		t.Fatalf("AuditTotalBytes=%d, want 100 (negative clamped to 0)", total)
	}
	deleted, err := s.PurgeAuditOverBudget(context.Background(), 50)
	if err != nil {
		t.Fatal(err)
	}
	if deleted != 1 {
		t.Fatalf("deleted=%d, want 1 (older row over budget after clamping)", deleted)
	}
	rows, err := s.ListAuditConversations(context.Background(), AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 || rows[0].ID != "neg" {
		t.Fatalf("remaining=%+v err=%v, want only neg", rows, err)
	}
}

// The list view is metadata only: raw request/response bodies stay in the
// database but are not shipped with every page, while the single-conversation
// endpoint still returns them in full.
func TestStore_ListAuditOmitsBodies(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now().Truncate(time.Second)
	if err := s.InsertAuditConversation(context.Background(), AuditConversation{
		ID: "with-body", Model: "m", Timestamp: now,
		RequestBody:  []byte(`{"prompt":"secret"}`),
		ResponseBody: []byte(`{"answer":"reply"}`),
	}); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAuditConversations(context.Background(), AuditQuery{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%+v err=%v, want 1", rows, err)
	}
	if len(rows[0].RequestBody) != 0 || len(rows[0].ResponseBody) != 0 {
		t.Fatalf("list view leaked bodies req=%q resp=%q", rows[0].RequestBody, rows[0].ResponseBody)
	}
	if rows[0].SizeBytes == 0 {
		t.Fatalf("list view dropped size_bytes")
	}
	full, found, err := s.GetAuditConversation(context.Background(), "with-body")
	if err != nil || !found {
		t.Fatalf("GetAuditConversation found=%v err=%v", found, err)
	}
	if string(full.RequestBody) != `{"prompt":"secret"}` || string(full.ResponseBody) != `{"answer":"reply"}` {
		t.Fatalf("detail view lost bodies req=%q resp=%q", full.RequestBody, full.ResponseBody)
	}
}

func TestStore_GetAuditConversation(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertAuditConversation(context.Background(), AuditConversation{ID: "lookup", Model: "m", Complete: true}); err != nil {
		t.Fatal(err)
	}
	conversation, found, err := s.GetAuditConversation(context.Background(), "lookup")
	if err != nil || !found || conversation.ID != "lookup" || conversation.Model != "m" || !conversation.Complete {
		t.Fatalf("conversation=%+v found=%v err=%v", conversation, found, err)
	}
	if _, found, err := s.GetAuditConversation(context.Background(), "missing"); err != nil || found {
		t.Fatalf("missing conversation found=%v err=%v", found, err)
	}
}

func TestStore_ClearAuditConversationsRemovesFutureRows(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.InsertAuditConversation(context.Background(), AuditConversation{ID: "future", Model: "m", Timestamp: time.Now().Add(24 * time.Hour), RequestBody: []byte("x")}); err != nil {
		t.Fatal(err)
	}
	if err := s.ClearAuditConversations(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := s.ListAuditConversations(context.Background(), AuditQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 0 {
		t.Fatalf("rows after clear = %+v", rows)
	}
}

func TestStore_UsageSummaryIncludesAggregateCacheHitRatio(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, entry := range []ActivityLogEntry{
		{Timestamp: now, Model: "m1", KeyID: "k1", SessionID: "s1", Tokens: TokenMetrics{InputTokens: 10, CachedTokens: 20}, CacheCreationTokens: 5},
		{Timestamp: now, Model: "m1", KeyID: "k1", SessionID: "s1", Tokens: TokenMetrics{InputTokens: 5}},
	} {
		if _, err := s.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := s.UsageSummary(context.Background(), ActivityFilter{KeyID: "k1", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 2 || summary.CacheHitRatio != 20.0/40.0 || summary.CacheCreationRatio != 5.0/40.0 {
		t.Fatalf("summary=%+v", summary)
	}
	if len(summary.ByModel) != 1 || summary.ByModel[0].CacheHitRatio != summary.CacheHitRatio || summary.ByModel[0].CacheCreationRatio != summary.CacheCreationRatio {
		t.Fatalf("model usage=%+v", summary.ByModel)
	}
}

func TestStore_AuditConversationPersistsCacheCreationRatio(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	record := AuditConversation{
		ID:                  "cache-ratio",
		Model:               "m",
		InputTokens:         20,
		CachedTokens:        30,
		CacheCreationTokens: 10,
		CacheHitRatio:       0.5,
		CacheCreationRatio:  10.0 / 60.0,
	}
	if err := s.InsertAuditConversation(context.Background(), record); err != nil {
		t.Fatal(err)
	}
	got, found, err := s.GetAuditConversation(context.Background(), record.ID)
	if err != nil || !found {
		t.Fatalf("GetAuditConversation found=%v err=%v", found, err)
	}
	if got.CacheCreationRatio != record.CacheCreationRatio {
		t.Fatalf("cache creation ratio=%v, want %v", got.CacheCreationRatio, record.CacheCreationRatio)
	}
}

func TestStore_UsageSummarySaturatesMalformedCounters(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	maxInt := int(^uint(0) >> 1)
	for _, entry := range []ActivityLogEntry{
		{Model: "extreme", Tokens: TokenMetrics{InputTokens: maxInt, OutputTokens: maxInt, CachedTokens: maxInt}, CacheCreationTokens: maxInt, ReasoningTokens: maxInt},
		{Model: "extreme", Tokens: TokenMetrics{InputTokens: maxInt, OutputTokens: -10, CachedTokens: -1}, CacheCreationTokens: -1, ReasoningTokens: maxInt},
	} {
		if _, err := s.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := s.UsageSummary(context.Background(), ActivityFilter{Models: []string{"extreme"}})
	if err != nil {
		t.Fatalf("UsageSummary: %v", err)
	}
	if summary.Requests != 2 || summary.InputTokens != maxInt || summary.OutputTokens != maxInt || summary.CachedTokens != maxInt || summary.CacheCreationTokens != maxInt || summary.ReasoningTokens != maxInt {
		t.Fatalf("saturated summary=%+v", summary)
	}
	if len(summary.ByModel) != 1 || summary.ByModel[0].InputTokens != maxInt {
		t.Fatalf("saturated model summary=%+v", summary.ByModel)
	}
}

func TestStore_AggregateCacheHitRatioAvoidsDoubleCountingIncludedPartitions(t *testing.T) {
	// OpenAI compatible rows include cached tokens in the input total, so
	// the aggregate must not add them to the denominator again.
	if got := aggregateCacheHitRatio(100, 40, 0); got != 0.4 {
		t.Fatalf("openai shape hit ratio = %v, want 0.4", got)
	}
	if got := aggregateCacheCreationRatio(100, 40, 10); got != 10.0/100.0 {
		t.Fatalf("openai shape creation ratio = %v, want 0.1", got)
	}
	// A partition sum larger than the input total is Anthropic's
	// partitioned shape and the partitions stay additive.
	if got := aggregateCacheHitRatio(30, 40, 10); got != 40.0/80.0 {
		t.Fatalf("anthropic shape hit ratio = %v, want 0.5", got)
	}
}

func TestStore_UsageSummaryOpenAICompatibleAggregateIsNotDoubleCounted(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now := time.Now()
	for _, entry := range []ActivityLogEntry{
		{Timestamp: now, Model: "vllm", KeyID: "k1", SessionID: "s1", Tokens: TokenMetrics{InputTokens: 79741, CachedTokens: 72720}},
		{Timestamp: now, Model: "vllm", KeyID: "k1", SessionID: "s1", Tokens: TokenMetrics{InputTokens: 43111, CachedTokens: 40400}},
	} {
		if _, err := s.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	summary, err := s.UsageSummary(context.Background(), ActivityFilter{KeyID: "k1", SessionID: "s1"})
	if err != nil {
		t.Fatal(err)
	}
	want := 113120.0 / 122852.0
	if summary.CacheHitRatio < want-1e-9 || summary.CacheHitRatio > want+1e-9 {
		t.Fatalf("summary cache hit ratio = %v, want %v", summary.CacheHitRatio, want)
	}
}

func TestStore_AggregateCacheHitRatioBoundsMalformedCounters(t *testing.T) {
	maxInt := int(^uint(0) >> 1)
	if got := aggregateCacheHitRatio(maxInt, maxInt, maxInt); got < 0 || got > 1 {
		t.Fatalf("overflowing counters produced ratio=%v", got)
	}
	if got := aggregateCacheHitRatio(-10, 2, -3); got != 1 {
		t.Fatalf("negative counters should be ignored, ratio=%v", got)
	}
	maxInt64 := int64(^uint64(0) >> 1)
	if got := saturatingInt64Add(maxInt64, 1); got != maxInt64 {
		t.Fatalf("saturatingInt64Add overflowed: got=%d", got)
	}
}

func TestStore_ReplacePricingSnapshotIsAtomic(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.UpsertPrice(ctx, Price{Provider: "provider", Model: "stale", Input: 9}); err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := s.ReplacePricingSnapshot(ctx, []Price{
		{Provider: "provider", Model: "fresh", Input: 1},
		{Provider: "other", Model: "second", Output: 2},
	}, PricingMeta{ETag: `"v2"`, Source: "test", SyncedAt: now}); err != nil {
		t.Fatal(err)
	}
	prices, err := s.FindPrices(ctx, "", "")
	if err != nil || len(prices) != 2 {
		t.Fatalf("prices after replacement=%+v err=%v", prices, err)
	}
	stale, err := s.FindPrices(ctx, "", "stale")
	if err != nil {
		t.Fatal(err)
	}
	if len(stale) != 0 {
		t.Fatalf("stale price survived replacement: %+v", stale)
	}
	meta, err := s.GetPricingMeta(ctx)
	if err != nil || meta.ETag != `"v2"` || meta.Source != "test" {
		t.Fatalf("pricing metadata=%+v err=%v", meta, err)
	}

	if err := s.ReplacePricingSnapshot(ctx, []Price{{Model: "valid"}, {Model: ""}}, PricingMeta{ETag: `"broken"`, SyncedAt: time.Now()}); err == nil {
		t.Fatal("invalid snapshot unexpectedly committed")
	}
	prices, err = s.FindPrices(ctx, "", "")
	if err != nil || len(prices) != 2 {
		t.Fatalf("prices after rolled back snapshot=%+v err=%v", prices, err)
	}
	meta, err = s.GetPricingMeta(ctx)
	if err != nil || meta.ETag != `"v2"` {
		t.Fatalf("metadata changed after rollback=%+v err=%v", meta, err)
	}
}

func TestStore_PricingRejectsInvalidRatesAndKeepsSnapshot(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.UpsertPrice(ctx, Price{Provider: "p", Model: "valid", Input: 1}); err != nil {
		t.Fatal(err)
	}
	for _, price := range []Price{
		{Provider: "p", Model: "negative", Input: -1},
		{Provider: "p", Model: "nan", Output: math.NaN()},
		{Provider: "p", Model: "infinite", CacheWrite: math.Inf(1)},
	} {
		if err := s.UpsertPrice(ctx, price); err == nil {
			t.Fatalf("invalid price %+v unexpectedly accepted", price)
		}
	}
	if err := s.ReplacePricingSnapshot(ctx, []Price{{Provider: "p", Model: "valid", Input: 1}, {Provider: "p", Model: "bad", Reasoning: -0.1}}, PricingMeta{}); err == nil {
		t.Fatal("invalid replacement snapshot unexpectedly accepted")
	}
	prices, err := s.FindPrices(ctx, "p", "valid")
	if err != nil || len(prices) != 1 || prices[0].Input != 1 {
		t.Fatalf("valid snapshot changed after rejected writes: prices=%+v err=%v", prices, err)
	}
}

func TestStore_PricingRejectsUnsafeIdentities(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	valid := Price{Provider: "provider name", Model: "model/with spaces", Input: 1}
	if err := s.UpsertPrice(ctx, valid); err != nil {
		t.Fatalf("valid price rejected: %v", err)
	}
	for _, price := range []Price{
		{Provider: " provider", Model: "model"},
		{Provider: "provider", Model: "model\u00a0name"},
		{Provider: "provider\u200b", Model: "model"},
		{Provider: "provider\n", Model: "model"},
		{Provider: string(make([]byte, maxPriceIdentityBytes+1)), Model: "model"},
		{Provider: "provider", Model: ""},
	} {
		if err := s.UpsertPrice(ctx, price); err == nil {
			t.Errorf("unsafe price identity unexpectedly accepted: provider=%q model=%q", price.Provider, price.Model)
		}
	}
	prices, err := s.FindPrices(ctx, valid.Provider, valid.Model)
	if err != nil || len(prices) != 1 || prices[0].Input != valid.Input {
		t.Fatalf("valid price changed after rejected identities: prices=%+v err=%v", prices, err)
	}
}

func TestStore_APIKeyBoundaryRejectsInvalidRecordsAndPreservesExisting(t *testing.T) {
	s, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	original := APIKeyRecord{
		ID:      "managed",
		Name:    "original",
		KeyHash: KeyFingerprint("original-secret"),
		Scopes:  []string{"inference"},
		Models:  []string{"qwen*"},
	}
	if err := s.UpsertAPIKey(ctx, original); err != nil {
		t.Fatalf("insert original key: %v", err)
	}

	cases := []APIKeyRecord{
		{ID: "managed", KeyHash: "raw-secret", Scopes: []string{"inference"}},
		{ID: "managed/other", KeyHash: KeyFingerprint("replacement"), Scopes: []string{"inference"}},
		{ID: "managed\u200b", KeyHash: KeyFingerprint("replacement"), Scopes: []string{"inference"}},
		{ID: "managed", KeyHash: KeyFingerprint("replacement"), Scopes: []string{"unknown"}},
		{ID: "managed", KeyHash: KeyFingerprint("replacement"), Models: []string{"qwen*7b"}},
		{ID: "managed", Name: "line\nbreak", KeyHash: KeyFingerprint("replacement")},
	}
	for _, candidate := range cases {
		if err := s.UpsertAPIKey(ctx, candidate); err == nil {
			t.Fatalf("invalid API key record unexpectedly accepted: %+v", candidate)
		}
	}
	got, found, err := s.FindAPIKeyBySecret(ctx, "original-secret")
	if err != nil || !found || got.ID != original.ID || got.KeyHash != original.KeyHash {
		t.Fatalf("original key changed after rejected writes: got=%+v found=%v err=%v", got, found, err)
	}

	upper := strings.ToUpper(KeyFingerprint("uppercase-secret"))
	if err := s.UpsertAPIKey(ctx, APIKeyRecord{ID: "  padded-id  ", Name: "  display name  ", KeyHash: upper, Scopes: []string{" logs ", "inference"}, Models: []string{" qwen* "}}); err != nil {
		t.Fatalf("normalized API key rejected: %v", err)
	}
	normalized, found, err := s.FindAPIKeyByHash(ctx, upper)
	if err != nil || !found || normalized.ID != "padded-id" || normalized.Name != "display name" || !slicesEqual(normalized.Scopes, []string{"inference", "logs"}) || !slicesEqual(normalized.Models, []string{"qwen*"}) {
		t.Fatalf("normalized API key=%+v found=%v err=%v", normalized, found, err)
	}
}

func TestStore_APIKeyMethodsOnNilStoreReturnErrors(t *testing.T) {
	var s *Store
	ctx := context.Background()
	record := APIKeyRecord{ID: "key", KeyHash: KeyFingerprint("secret")}
	if err := s.UpsertAPIKey(ctx, record); err == nil {
		t.Fatal("UpsertAPIKey on nil store unexpectedly succeeded")
	}
	if _, err := s.ListAPIKeys(ctx, true); err == nil {
		t.Fatal("ListAPIKeys on nil store unexpectedly succeeded")
	}
	if _, _, err := s.FindAPIKeyByHash(ctx, record.KeyHash); err == nil {
		t.Fatal("FindAPIKeyByHash on nil store unexpectedly succeeded")
	}
	if _, _, err := s.FindAPIKeyBySecret(ctx, "secret"); err == nil {
		t.Fatal("FindAPIKeyBySecret on nil store unexpectedly succeeded")
	}
	if _, _, err := s.GetAPIKeySecret(ctx, "key"); err == nil {
		t.Fatal("GetAPIKeySecret on nil store unexpectedly succeeded")
	}
	if err := s.TouchAPIKey(ctx, "key", time.Now()); err == nil {
		t.Fatal("TouchAPIKey on nil store unexpectedly succeeded")
	}
	if err := s.RevokeAPIKey(ctx, "key", time.Now()); err == nil {
		t.Fatal("RevokeAPIKey on nil store unexpectedly succeeded")
	}
	if err := s.RotateAPIKey(ctx, "key", record, time.Now()); err == nil {
		t.Fatal("RotateAPIKey on nil store unexpectedly succeeded")
	}
}

func TestStore_ControlPersistenceMethodsOnNilStoreReturnErrors(t *testing.T) {
	var s *Store
	ctx := context.Background()
	now := time.Now()

	if err := s.InsertAuditConversation(ctx, AuditConversation{ID: "audit", Model: "model"}); err == nil {
		t.Fatal("InsertAuditConversation on nil store unexpectedly succeeded")
	}
	if _, err := s.ListAuditConversations(ctx, AuditQuery{Limit: 1}); err == nil {
		t.Fatal("ListAuditConversations on nil store unexpectedly succeeded")
	}
	if _, _, err := s.GetAuditConversation(ctx, "audit"); err == nil {
		t.Fatal("GetAuditConversation on nil store unexpectedly succeeded")
	}
	if err := s.DeleteAuditConversation(ctx, "audit"); err == nil {
		t.Fatal("DeleteAuditConversation on nil store unexpectedly succeeded")
	}
	if err := s.ClearAuditConversations(ctx); err == nil {
		t.Fatal("ClearAuditConversations on nil store unexpectedly succeeded")
	}
	if err := s.PurgeAuditBefore(ctx, now); err == nil {
		t.Fatal("PurgeAuditBefore on nil store unexpectedly succeeded")
	}
	if _, err := s.PurgeAuditOverBudget(ctx, 1); err == nil {
		t.Fatal("PurgeAuditOverBudget on nil store unexpectedly succeeded")
	}
	if _, err := s.AuditTotalBytes(ctx); err == nil {
		t.Fatal("AuditTotalBytes on nil store unexpectedly succeeded")
	}

	if err := s.UpsertResponseAffinity(ctx, ResponseAffinity{ResponseID: "response", Model: "model"}); err == nil {
		t.Fatal("UpsertResponseAffinity on nil store unexpectedly succeeded")
	}
	if _, _, err := s.GetResponseAffinity(ctx, "response", now); err == nil {
		t.Fatal("GetResponseAffinity on nil store unexpectedly succeeded")
	}
	if err := s.DeleteResponseAffinity(ctx, "response"); err == nil {
		t.Fatal("DeleteResponseAffinity on nil store unexpectedly succeeded")
	}
	if err := s.DeleteExpiredResponseAffinities(ctx, now); err == nil {
		t.Fatal("DeleteExpiredResponseAffinities on nil store unexpectedly succeeded")
	}

	operation := RuntimeOperation{ID: "operation", RuntimeName: "runtime"}
	if err := s.UpsertRuntimeOperation(ctx, operation); err == nil {
		t.Fatal("UpsertRuntimeOperation on nil store unexpectedly succeeded")
	}
	if _, err := s.ListRuntimeOperations(ctx, "runtime", 1); err == nil {
		t.Fatal("ListRuntimeOperations on nil store unexpectedly succeeded")
	}

	if err := s.UpsertPrice(ctx, Price{Provider: "provider", Model: "model"}); err == nil {
		t.Fatal("UpsertPrice on nil store unexpectedly succeeded")
	}
	if err := s.ReplacePricingSnapshot(ctx, nil, PricingMeta{}); err == nil {
		t.Fatal("ReplacePricingSnapshot on nil store unexpectedly succeeded")
	}
	if _, err := s.GetPricingMeta(ctx); err == nil {
		t.Fatal("GetPricingMeta on nil store unexpectedly succeeded")
	}
	if err := s.SetPricingMeta(ctx, PricingMeta{}); err == nil {
		t.Fatal("SetPricingMeta on nil store unexpectedly succeeded")
	}
	if _, err := s.FindPrices(ctx, "provider", "model"); err == nil {
		t.Fatal("FindPrices on nil store unexpectedly succeeded")
	}
	if _, err := s.UsageSummary(ctx, ActivityFilter{}); err == nil {
		t.Fatal("UsageSummary on nil store unexpectedly succeeded")
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
