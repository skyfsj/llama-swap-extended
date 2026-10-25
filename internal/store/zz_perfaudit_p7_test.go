package store

import (
	"context"
	"path/filepath"
	"testing"
)

// seedAuditActivities writes activity rows and audit conversations that
// reference them, so the phase attachment has something to join against.
func seedAuditActivities(t *testing.T, s *Store, count int) []int64 {
	t.Helper()
	ctx := context.Background()
	ids := make([]int64, 0, count)
	for i := range count {
		entry := ActivityLogEntry{
			Model:           "m",
			ReqPath:         "/v1/chat/completions",
			RespContentType: "application/json",
			RespStatusCode:  200,
			DurationMs:      100 + i,
			FirstTokenMs:    10 + i,
			DecodeMs:        20 + i,
			SpeedTimeline:   "[[0,1]]",
			Tokens:          TokenMetrics{InputTokens: 5, OutputTokens: 7},
		}
		stored, err := s.InsertActivity(ctx, entry)
		if err != nil {
			t.Fatalf("InsertActivity: %v", err)
		}
		ids = append(ids, int64(stored.ID))
	}
	return ids
}

// TestStore_AttachActivityPhasesBatchMatchesSingleRow pins the batched reader
// against the single-row reader it replaced: for every conversation the merged
// phase telemetry must be identical, whether it was filled one query at a time
// or in one query for the whole page.
func TestStore_AttachActivityPhasesBatchMatchesSingleRow(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	ids := seedAuditActivities(t, s, 12)
	conversations := make([]AuditConversation, 0, len(ids)+3)
	for _, id := range ids {
		conversations = append(conversations, AuditConversation{ID: "c" + itoa(id), ActivityID: int(id)})
	}
	// A conversation with no activity row, and one with an id that does not
	// exist, must keep the sentinels in both paths.
	conversations = append(conversations,
		AuditConversation{ID: "no-activity", ActivityID: 0},
		AuditConversation{ID: "missing-activity", ActivityID: 1 << 40},
	)

	batched := append([]AuditConversation(nil), conversations...)
	if err := s.attachActivityPhasesBatch(ctx, batched); err != nil {
		t.Fatalf("attachActivityPhasesBatch: %v", err)
	}
	for i := range conversations {
		single := conversations[i]
		if err := s.attachActivityPhases(ctx, &single); err != nil {
			t.Fatalf("attachActivityPhases(%s): %v", single.ID, err)
		}
		if single.FirstTokenMs != batched[i].FirstTokenMs {
			t.Fatalf("%s FirstTokenMs: single=%d batch=%d", single.ID, single.FirstTokenMs, batched[i].FirstTokenMs)
		}
		if single.DecodeMs != batched[i].DecodeMs {
			t.Fatalf("%s DecodeMs: single=%d batch=%d", single.ID, single.DecodeMs, batched[i].DecodeMs)
		}
		if single.DurationMs != batched[i].DurationMs {
			t.Fatalf("%s DurationMs: single=%d batch=%d", single.ID, single.DurationMs, batched[i].DurationMs)
		}
		if single.SpeedTimeline != batched[i].SpeedTimeline {
			t.Fatalf("%s SpeedTimeline: single=%q batch=%q", single.ID, single.SpeedTimeline, batched[i].SpeedTimeline)
		}
	}

	// The rows that reference a real activity row must carry its values.
	for i, id := range ids {
		want := AuditConversation{}
		want.FirstTokenMs = normalizeDecodeMs(10 + i)
		want.DecodeMs = normalizeDecodeMs(20 + i)
		want.DurationMs = 100 + i
		if batched[i].FirstTokenMs != want.FirstTokenMs || batched[i].DecodeMs != want.DecodeMs {
			t.Fatalf("row %d = %d/%d, want %d/%d", id, batched[i].FirstTokenMs, batched[i].DecodeMs, want.FirstTokenMs, want.DecodeMs)
		}
		if batched[i].DurationMs != want.DurationMs || batched[i].SpeedTimeline != "[[0,1]]" {
			t.Fatalf("row %d = %d/%q, want %d/%q", id, batched[i].DurationMs, batched[i].SpeedTimeline, want.DurationMs, "[[0,1]]")
		}
	}
	// The rows without an activity row keep the sentinels.
	for _, index := range []int{len(ids), len(ids) + 1} {
		if batched[index].FirstTokenMs != -1 || batched[index].DecodeMs != -1 {
			t.Fatalf("row %d kept %d/%d, want -1/-1", index, batched[index].FirstTokenMs, batched[index].DecodeMs)
		}
	}
}

// TestStore_AttachActivityPhasesBatchHandlesDuplicatesAndEmpty covers the
// degenerate inputs: the same activity id twice must be filled once and
// consistently, and an empty slice is a no-op.
func TestStore_AttachActivityPhasesBatchHandlesDuplicatesAndEmpty(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	if err := s.attachActivityPhasesBatch(ctx, nil); err != nil {
		t.Fatalf("empty batch: %v", err)
	}
	if err := s.attachActivityPhasesBatch(ctx, []AuditConversation{{ID: "x", ActivityID: 0}}); err != nil {
		t.Fatalf("batch without ids: %v", err)
	}

	ids := seedAuditActivities(t, s, 3)
	rows := []AuditConversation{
		{ID: "a", ActivityID: int(ids[0])},
		{ID: "b", ActivityID: int(ids[0])},
		{ID: "c", ActivityID: int(ids[1])},
		{ID: "d", ActivityID: int(ids[0])},
	}
	if err := s.attachActivityPhasesBatch(ctx, rows); err != nil {
		t.Fatalf("attachActivityPhasesBatch: %v", err)
	}
	for _, index := range []int{0, 1, 3} {
		if rows[index].DurationMs != 100 {
			t.Fatalf("row %d DurationMs = %d, want 100", index, rows[index].DurationMs)
		}
	}
	if rows[2].DurationMs != 101 {
		t.Fatalf("row 2 DurationMs = %d, want 101", rows[2].DurationMs)
	}
}

// TestStore_ListAuditConversationsPageFillsPhases is the end-to-end guard: the
// audit list endpoint returns conversations with their phase telemetry filled,
// which is what the batched reader now supplies.
func TestStore_ListAuditConversationsPageFillsPhases(t *testing.T) {
	dir := t.TempDir()
	s, err := New(filepath.Join(dir, "store.db"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer s.Close()

	ctx := context.Background()
	ids := seedAuditActivities(t, s, 3)
	for i, id := range ids {
		if err := s.InsertAuditConversation(ctx, AuditConversation{
			ID:         "conv-" + itoa(id),
			ActivityID: int(id),
			Complete:   true,
			SizeBytes:  1,
		}); err != nil {
			t.Fatalf("SaveAuditConversation(%d): %v", i, err)
		}
	}

	page, err := s.ListAuditConversationsPage(ctx, AuditQuery{Limit: 10})
	if err != nil {
		t.Fatalf("ListAuditConversationsPage: %v", err)
	}
	if len(page.Data) != len(ids) {
		t.Fatalf("page has %d conversations, want %d", len(page.Data), len(ids))
	}
	for _, conversation := range page.Data {
		if conversation.FirstTokenMs < 0 || conversation.DecodeMs < 0 {
			t.Fatalf("%s kept the sentinels: %d/%d", conversation.ID, conversation.FirstTokenMs, conversation.DecodeMs)
		}
		if conversation.DurationMs < 100 {
			t.Fatalf("%s DurationMs = %d, want >= 100", conversation.ID, conversation.DurationMs)
		}
		if conversation.SpeedTimeline != "[[0,1]]" {
			t.Fatalf("%s SpeedTimeline = %q, want [[0,1]]", conversation.ID, conversation.SpeedTimeline)
		}
	}
}

func itoa(value int64) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var digits []byte
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	if negative {
		return "-" + string(digits)
	}
	return string(digits)
}
