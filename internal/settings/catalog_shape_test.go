package settings

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/settings/spec"
)

// componentKinds is the JSON shape each settings-center editor can round-trip:
// the editor writes a value of exactly these kinds back into the draft.
var componentKinds = map[string][]string{
	spec.CompPassword: {"string"},
	spec.CompText:     {"string"},
	spec.CompSelect:   {"string"},
	spec.CompDuration: {"string", "number"},
	spec.CompNumber:   {"number"},
	spec.CompSwitch:   {"bool"},
	spec.CompList:     {"array"},
	spec.CompMap:      {"object"},
	spec.CompJSON:     {"string", "array", "object"},
	spec.CompTextarea: {"string"},
}

// entityEditors are served by dedicated editors that understand their own
// value shape instead of the generic field renderer.
var entityEditors = map[string]struct{}{"model": {}, "peer": {}, "profile": {}, "selector": {}, "runtime": {}}

func jsonShape(value any) string {
	switch value.(type) {
	case nil:
		return "null"
	case bool:
		return "bool"
	case string:
		return "string"
	case float64, int, int64, json.Number:
		return "number"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return "other"
	}
}

func valueAtPointer(root map[string]any, pointer string) (any, bool) {
	var parts []string
	for _, part := range strings.Split(strings.TrimPrefix(pointer, "/"), "/") {
		if part != "" {
			parts = append(parts, part)
		}
	}
	var current any = root
	for _, part := range parts {
		object, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		next, ok := object[part]
		if !ok {
			return nil, false
		}
		current = next
	}
	return current, true
}

func settingsSnapshotForFile(t *testing.T, path string) Snapshot {
	t.Helper()
	source, err := config.NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(source)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// TestSettingsCatalog_EditorsMatchConfigShapes pins the contract between the
// catalogue and the values it renders: an editor that writes a scalar into a
// sequence (or the reverse) produces a draft the loader rejects. The apiKeys
// regression is the reference case: it rendered a password input for a
// sequence, so any edit submitted one string and the save failed with
// "apiKeys must be a sequence of strings or objects".
func TestSettingsCatalog_EditorsMatchConfigShapes(t *testing.T) {
	example, err := os.ReadFile(filepath.Join("..", "..", "config.example.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, example, 0o600); err != nil {
		t.Fatal(err)
	}
	snapshot := settingsSnapshotForFile(t, path)
	var cfg map[string]any
	if err := json.Unmarshal(snapshot.Config, &cfg); err != nil {
		t.Fatal(err)
	}
	// Both accepted apiKeys forms are in play in real deployments; the example
	// only spells out the scalar form.
	cfg["apiKeys"] = []any{"sk-scalar", map[string]any{"name": "web", "key": "sk-structured", "models": []any{"m"}}}

	sections, ok := spec.Metadata()["sections"].(map[string][]map[string]any)
	if !ok {
		t.Fatal("settings metadata has no sections")
	}
	var issues []string
	for section, fields := range sections {
		for _, field := range fields {
			pointer, _ := field["path"].(string)
			component, _ := field["component"].(string)
			if hidden, _ := field["hidden"].(bool); hidden {
				continue
			}
			if editor, _ := field["editor"].(string); editor != "" {
				if _, dedicated := entityEditors[editor]; dedicated {
					continue
				}
			}
			value, found := valueAtPointer(cfg, pointer)
			if !found {
				continue
			}
			shape := jsonShape(value)
			if shape == "null" || shape == "other" {
				continue
			}
			allowed, known := componentKinds[component]
			if !known {
				issues = append(issues, fmt.Sprintf("%s %s: component %q has no shape contract (value is %s)", section, pointer, component, shape))
				continue
			}
			if !slicesContains(allowed, shape) {
				issues = append(issues, fmt.Sprintf("%s %s: component %q cannot round-trip a %s value", section, pointer, component, shape))
			}
		}
	}
	sort.Strings(issues)
	for _, issue := range issues {
		t.Errorf("%s", issue)
	}
}

// TestSettingsCatalog_APIKeysIsASensitiveList pins the catalogue metadata the
// settings center switches on. Rendered as anything scalar it submits a string
// for a sequence and the save fails; rendered without the sensitive flag the
// stored secrets reach the browser in clear text.
func TestSettingsCatalog_APIKeysIsASensitiveList(t *testing.T) {
	field, ok := spec.FindField(spec.Root(), "apiKeys")
	if !ok || field == nil {
		t.Fatalf("apiKeys is missing from the catalogue: field=%+v ok=%v", field, ok)
	}
	if field.Kind != spec.KScalarOrObject {
		t.Fatalf("kind=%v, want a scalar-or-object sequence", field.Kind)
	}
	if field.UI.Component != spec.CompList {
		t.Fatalf("component=%q, want %q so the field renders as a list", field.UI.Component, spec.CompList)
	}
	if !field.Sensitive {
		t.Fatal("apiKeys must stay sensitive: its entries are the secrets the server redacts")
	}
	items, ok := spec.SchemaDocument()["properties"].(map[string]any)["apiKeys"].(map[string]any)["items"].(map[string]any)
	if !ok {
		t.Fatalf("apiKeys schema has no items: %#v", spec.SchemaDocument()["properties"])
	}
	types, ok := items["type"].([]string)
	if !ok || !slicesContains(types, "string") || !slicesContains(types, "object") {
		t.Fatalf("apiKeys schema items=%#v, want both string and object accepted", items)
	}
}

func slicesContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
