package spec

import "testing"

func TestCatalogHasStableTopLevelAndSensitiveFields(t *testing.T) {
	known := KnownTopLevel()
	for _, key := range []string{"models", "routing", "runtimeManager", "apiKeys", "groups", "matrix"} {
		if _, ok := known[key]; !ok {
			t.Fatalf("catalog lost top-level field %q", key)
		}
	}
	metadata := Metadata()
	if metadata["version"] != 1 {
		t.Fatalf("unexpected metadata version: %#v", metadata["version"])
	}
	schema := SchemaDocument()
	properties, ok := schema["properties"].(map[string]any)
	if !ok || properties["apiKeys"] == nil {
		t.Fatalf("schema does not expose apiKeys metadata: %#v", schema)
	}
}

func TestFindFieldResolvesEntityTemplate(t *testing.T) {
	field, ok := FindField(Root(), "models.example.cmd")
	if !ok || field == nil || field.Kind != KStringList {
		t.Fatalf("models template resolution failed: field=%+v ok=%v", field, ok)
	}
}

func TestCatalogLogStorageRetentionField(t *testing.T) {
	field, ok := FindField(Root(), "logStorage.maxFiles")
	if !ok || field == nil {
		t.Fatalf("logStorage.maxFiles is missing: field=%+v ok=%v", field, ok)
	}
	if field.Kind != KInt || field.Default != 5 || field.Min == nil || *field.Min != 1 || field.Max == nil || *field.Max != 100 {
		t.Fatalf("logStorage.maxFiles=%+v, want integer default 5 bounded 1..100", field)
	}

	metadata := Metadata()
	logging, ok := metadata["sections"].(map[string][]map[string]any)
	if !ok {
		t.Fatalf("metadata logging section has unexpected shape: %#v", metadata["sections"])
	}
	found := false
	for _, entry := range logging["logging"] {
		if entry["path"] == "/logStorage/maxFiles" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("metadata does not expose /logStorage/maxFiles: %#v", logging["logging"])
	}
}

// TestCatalogPreloadUsesModelProvider pins the settings-center wiring for
// hooks.on_startup.preload. Without the provider the field is a list of
// free-text rows, so an operator picking a model by hand can name one that is
// not configured — which config loading rejects.
func TestCatalogPreloadUsesModelProvider(t *testing.T) {
	field, ok := FindField(Root(), "hooks.on_startup.preload")
	if !ok || field == nil {
		t.Fatalf("hooks.on_startup.preload is missing: field=%+v ok=%v", field, ok)
	}
	if field.Kind != KStringList {
		t.Fatalf("kind=%v, want a string list", field.Kind)
	}
	if field.UI.Component != CompList {
		t.Fatalf("component=%q, want %q", field.UI.Component, CompList)
	}
	if field.UI.Provider != "models" {
		t.Fatalf("provider=%q, want %q so the field renders as a model multi-select", field.UI.Provider, "models")
	}
}
