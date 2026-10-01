package store

import (
	"context"
	"reflect"
	"testing"
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
