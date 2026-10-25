package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestStore_PricingCatalogAutocomplete(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, price := range []Price{
		{Provider: "openai", Model: "gpt-5"},
		{Provider: "openai", Model: "gpt-5-mini"},
		{Provider: "anthropic", Model: "claude-sonnet"},
	} {
		if err := st.UpsertPrice(context.Background(), price); err != nil {
			t.Fatal(err)
		}
	}

	providers, err := st.ListPricingProviders(context.Background(), "ai", 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"openai"}; !reflect.DeepEqual(providers, want) {
		t.Fatalf("providers = %v, want %v", providers, want)
	}

	models, err := st.ListPricingModels(context.Background(), "openai", "mini", 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"gpt-5-mini"}; !reflect.DeepEqual(models, want) {
		t.Fatalf("models = %v, want %v", models, want)
	}
}

func TestStore_PricingCatalogRowsCanBePagedAndDeleted(t *testing.T) {
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, price := range []Price{
		{Provider: "openai", Model: "gpt-5", Input: 1},
		{Provider: "openai", Model: "gpt-5-mini", Input: 0.5},
		{Provider: "anthropic", Model: "claude-sonnet", Input: 3},
	} {
		if err := st.UpsertPrice(context.Background(), price); err != nil {
			t.Fatal(err)
		}
	}

	page, err := st.ListPricingPrices(context.Background(), "openai", "gpt", 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 2 || page.TotalPages != 2 || len(page.Data) != 1 || page.Data[0].Model != "gpt-5" {
		t.Fatalf("page = %+v, want one of two matching rows", page)
	}

	deleted, err := st.DeletePrice(context.Background(), "openai", "gpt-5")
	if err != nil || !deleted {
		t.Fatalf("DeletePrice() = %v, %v; want true, nil", deleted, err)
	}
	deleted, err = st.DeletePrice(context.Background(), "openai", "gpt-5")
	if err != nil || deleted {
		t.Fatalf("second DeletePrice() = %v, %v; want false, nil", deleted, err)
	}
}

// TestStore_CatalogETagAfterLocalEdits pins the ETag contract between operator
// edits and the synchronization. Deleting a catalog row invalidates the ETag so
// the next refresh refetches and restores what the source lists — an operator's
// deletion is temporary by design. Upserting does not: an operator row is
// protected by its source, so the validator still covers the unchanged catalog
// rows and a refresh never restores a rate the operator overwrote.
func TestStore_CatalogETagAfterLocalEdits(t *testing.T) {
	ctx := context.Background()
	newStore := func(t *testing.T) *Store {
		t.Helper()
		st, err := New("")
		if err != nil {
			t.Fatalf("new store: %v", err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	seed := func(t *testing.T, st *Store) {
		t.Helper()
		err := st.ReplacePricingSnapshot(ctx, []Price{
			{Provider: "openai", Model: "gpt-5", Input: 1.25, Output: 10},
		}, PricingMeta{ETag: `W/"abc"`, SyncedAt: time.Now(), Source: "https://models.dev/api.json"})
		if err != nil {
			t.Fatalf("seed snapshot: %v", err)
		}
	}

	t.Run("delete", func(t *testing.T) {
		st := newStore(t)
		seed(t, st)

		deleted, err := st.DeletePrice(ctx, "openai", "gpt-5")
		if err != nil || !deleted {
			t.Fatalf("delete: deleted=%v err=%v", deleted, err)
		}

		meta, err := st.GetPricingMeta(ctx)
		if err != nil {
			t.Fatalf("meta: %v", err)
		}
		if meta.ETag != "" {
			t.Errorf("ETag=%q want empty so the next sync refetches", meta.ETag)
		}
		// A missing row is not a reason to drop the validator.
		if _, err := st.DeletePrice(ctx, "openai", "nope"); err != nil {
			t.Fatalf("delete missing: %v", err)
		}
		meta, err = st.GetPricingMeta(ctx)
		if err != nil {
			t.Fatalf("meta: %v", err)
		}
		if meta.ETag != "" {
			t.Errorf("deleting a missing row changed the ETag to %q", meta.ETag)
		}
	})

	t.Run("upsert keeps the ETag and marks the row operator", func(t *testing.T) {
		st := newStore(t)
		seed(t, st)

		err := st.UpsertPrice(ctx, Price{Provider: "openai", Model: "gpt-5", Input: 99, Output: 10})
		if err != nil {
			t.Fatalf("upsert: %v", err)
		}

		meta, err := st.GetPricingMeta(ctx)
		if err != nil {
			t.Fatalf("meta: %v", err)
		}
		if meta.ETag != `W/"abc"` {
			t.Errorf("ETag=%q want unchanged: catalog rows are untouched and the operator row is source-protected", meta.ETag)
		}
		rows, err := st.FindPrices(ctx, "openai", "gpt-5")
		if err != nil || len(rows) != 1 {
			t.Fatalf("find: rows=%d err=%v", len(rows), err)
		}
		if rows[0].Input != 99 {
			t.Errorf("input=%v want the operator's 99", rows[0].Input)
		}
		if rows[0].Source != "operator" {
			t.Errorf("source=%q want operator", rows[0].Source)
		}
	})

	t.Run("a successful sync restores a deleted row", func(t *testing.T) {
		st := newStore(t)
		seed(t, st)
		if _, err := st.DeletePrice(ctx, "openai", "gpt-5"); err != nil {
			t.Fatalf("delete: %v", err)
		}

		err := st.ReplacePricingSnapshot(ctx, []Price{
			{Provider: "openai", Model: "gpt-5", Input: 1.25, Output: 10},
		}, PricingMeta{ETag: `W/"def"`, SyncedAt: time.Now(), Source: "https://models.dev/api.json"})
		if err != nil {
			t.Fatalf("resync: %v", err)
		}
		meta, err := st.GetPricingMeta(ctx)
		if err != nil {
			t.Fatalf("meta: %v", err)
		}
		if meta.ETag != `W/"def"` {
			t.Errorf("ETag=%q want the new validator", meta.ETag)
		}
		rows, err := st.FindPrices(ctx, "openai", "gpt-5")
		if err != nil || len(rows) != 1 {
			t.Fatalf("the deleted row did not come back: rows=%d err=%v", len(rows), err)
		}
	})
}

// TestStore_OperatorRowsSurviveCatalogRefresh pins the protection that keeps a
// hand-written price alive: the daily models.dev refresh rebuilds only the rows
// it owns, so a locally hosted model priced by hand keeps its rate, and a
// catalog entry whose identity an operator row holds never overwrites it.
func TestStore_OperatorRowsSurviveCatalogRefresh(t *testing.T) {
	ctx := context.Background()
	st, err := New("")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })

	err = st.ReplacePricingSnapshot(ctx, []Price{
		{Provider: "berget", Model: "Qwen/Qwen3.8-27B-FP8", Input: 0.46, Output: 3.48},
		{Provider: "openai", Model: "gpt-5", Input: 1.25, Output: 10},
	}, PricingMeta{ETag: `W/"v1"`, SyncedAt: time.Now(), Source: "https://models.dev/api.json"})
	if err != nil {
		t.Fatalf("seed snapshot: %v", err)
	}

	// Correct a catalog rate by hand and price a model the catalog lacks.
	if err := st.UpsertPrice(ctx, Price{Provider: "berget", Model: "Qwen/Qwen3.8-27B-FP8", Input: 0.9, Output: 7}); err != nil {
		t.Fatalf("upsert over catalog row: %v", err)
	}
	if err := st.UpsertPrice(ctx, Price{Provider: "local", Model: "HY-MT2-1.8B", Input: 0, Output: 0.2}); err != nil {
		t.Fatalf("upsert custom row: %v", err)
	}

	err = st.ReplacePricingSnapshot(ctx, []Price{
		{Provider: "berget", Model: "Qwen/Qwen3.8-27B-FP8", Input: 0.55, Output: 4}, // catalog now says 0.55
		{Provider: "openai", Model: "gpt-5", Input: 2, Output: 20},                  // refreshed rate
		{Provider: "deepseek", Model: "deepseek-flash", Input: 0.15, Output: 0.6},   // new entry
	}, PricingMeta{ETag: `W/"v2"`, SyncedAt: time.Now(), Source: "https://models.dev/api.json"})
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}

	rows, err := st.FindPrices(ctx, "berget", "Qwen/Qwen3.8-27B-FP8")
	if err != nil || len(rows) != 1 {
		t.Fatalf("shadowed row: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Input != 0.9 || rows[0].Source != "operator" {
		t.Errorf("shadowed row = %+v, want the operator's 0.9/operator", rows[0])
	}
	rows, err = st.FindPrices(ctx, "local", "HY-MT2-1.8B")
	if err != nil || len(rows) != 1 {
		t.Fatalf("custom row: rows=%d err=%v", len(rows), err)
	}
	if rows[0].Source != "operator" {
		t.Errorf("custom row source=%q want operator", rows[0].Source)
	}
	rows, err = st.FindPrices(ctx, "", "gpt-5")
	if err != nil || len(rows) != 1 || rows[0].Input != 2 || rows[0].Source != "catalog" {
		t.Fatalf("catalog row not refreshed: rows=%+v err=%v", rows, err)
	}
	rows, err = st.FindPrices(ctx, "", "deepseek-flash")
	if err != nil || len(rows) != 1 || rows[0].Source != "catalog" {
		t.Fatalf("new catalog row missing: rows=%+v err=%v", rows, err)
	}

	page, err := st.ListPricingPrices(ctx, "", "", 1, 50)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 4 {
		t.Errorf("total=%d want 4 (three catalog rows + one operator row)", page.Total)
	}
}
