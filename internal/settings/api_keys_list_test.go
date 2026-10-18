package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// TestSettings_APIKeysIndexOpsPreserveUntouchedSecrets covers the write path
// the settings center uses for apiKeys. The form only ever sees redacted
// entries, so it edits the list with index-scoped operations instead of
// re-sending the whole sequence: re-sending would make the server restore
// secrets by position, and deleting an entry would hand some other entry's
// secret the slot it left behind.
func TestSettings_APIKeysIndexOpsPreserveUntouchedSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	source := "startPort: 5800\nmodels: {}\napiKeys:\n  - sk-first\n  - name: web\n    key: sk-second\n    models: [a]\n  - sk-third\n"
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	sourceManager, err := config.NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(sourceManager)
	if err != nil {
		t.Fatal(err)
	}
	base, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	// A scalar value is what a single-value editor submits, and it is the
	// reason the field must never render as one.
	scalar := Draft{Mode: ModeStructured, Changes: []Change{{Op: "replace", Path: "/apiKeys", Value: "sk-only"}}}
	invalid, err := manager.Preview(context.Background(), scalar)
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Valid {
		t.Fatal("a scalar apiKeys value must be rejected")
	}
	const want = "apiKeys must be a sequence of strings or objects"
	if len(invalid.Diagnostics) != 1 || !strings.Contains(invalid.Diagnostics[0].Message, want) {
		t.Fatalf("diagnostics=%+v, want %q", invalid.Diagnostics, want)
	}

	// What the masked editor emits: rotate the structured entry's secret,
	// remove the last key, append a new one. The first entry is untouched.
	draft := Draft{Mode: ModeStructured, Changes: []Change{
		{Op: "replace", Path: "/apiKeys/1", Value: map[string]any{"name": "web", "key": "sk-rotated", "models": []any{"a"}}},
		{Op: "remove", Path: "/apiKeys/2"},
		{Op: "add", Path: "/apiKeys/-", Value: "sk-added"},
	}}
	preview, err := manager.Preview(context.Background(), draft)
	if err != nil {
		t.Fatal(err)
	}
	if !preview.Valid {
		t.Fatalf("preview is invalid: %+v", preview.Diagnostics)
	}
	if _, err := manager.Commit(context.Background(), draft, base.ETag, preview.PreviewHash, Actor{ID: "test", Origin: "test"}); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(written)
	for _, kept := range []string{"sk-first", "sk-rotated", "sk-added"} {
		if !strings.Contains(text, kept) {
			t.Errorf("committed config lost %q:\n%s", kept, text)
		}
	}
	for _, dropped := range []string{"sk-second", "sk-third", "[REDACTED]"} {
		if strings.Contains(text, dropped) {
			t.Errorf("committed config still contains %q:\n%s", dropped, text)
		}
	}
}
