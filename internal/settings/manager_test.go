package settings

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestManagerStructuredPreviewCommitAndHistory(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := config.NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	manager, err := NewManager(source)
	if err != nil {
		t.Fatal(err)
	}
	base, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	draft := Draft{Mode: ModeStructured, Changes: []Change{{Op: "replace", Path: "/startPort", Value: 5900}}, Message: "test"}
	preview, err := manager.Preview(context.Background(), draft)
	if err != nil || !preview.Valid || preview.PreviewHash == "" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	result, err := manager.Commit(context.Background(), draft, base.ETag, preview.PreviewHash, Actor{ID: "test", Origin: "test"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Snapshot.ETag == base.ETag || result.Revision.ID == "" {
		t.Fatalf("commit did not produce a new revision: %+v", result)
	}
	content, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(content), "startPort: 5900") {
		t.Fatalf("updated source=%q err=%v", content, err)
	}
	history, err := manager.History(context.Background(), 10)
	if err != nil || len(history) < 2 {
		t.Fatalf("history=%+v err=%v", history, err)
	}
}

func TestManagerSourceCommitAndRestore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := config.NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	manager, _ := NewManager(source)
	base, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	draft := Draft{Mode: ModeSources, Sources: []SourceDraft{{SourceID: path, YAML: "startPort: 5900\nmodels: {}\n"}}}
	preview, err := manager.Preview(context.Background(), draft)
	if err != nil || !preview.Valid {
		t.Fatalf("source preview=%+v err=%v", preview, err)
	}
	result, err := manager.Commit(context.Background(), draft, base.ETag, preview.PreviewHash, Actor{ID: "test", Origin: "test"})
	if err != nil {
		t.Fatal(err)
	}
	restoreDraft := Draft{Mode: ModeRestore, RestoreRevision: base.Revision}
	restorePreview, err := manager.Preview(context.Background(), restoreDraft)
	if err != nil || !restorePreview.Valid {
		t.Fatalf("restore preview=%+v err=%v", restorePreview, err)
	}
	if _, err := manager.Commit(context.Background(), restoreDraft, result.Snapshot.ETag, restorePreview.PreviewHash, Actor{ID: "test", Origin: "test"}); err != nil {
		t.Fatal(err)
	}
	content, _ := os.ReadFile(path)
	if !strings.Contains(string(content), "startPort: 5800") {
		t.Fatalf("restore did not recover source: %s", content)
	}
}

func TestManagerPreviewRedactsSensitiveDiff(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	_ = os.WriteFile(path, []byte("apiKeys: []\nmodels: {}\n"), 0o600)
	source, _ := config.NewConfigManager(path, "")
	manager, _ := NewManager(source)
	preview, err := manager.Preview(context.Background(), Draft{Changes: []Change{{Op: "add", Path: "/apiKeys/0", Value: "secret-token"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Diff) != 1 || preview.Diff[0].Value != "secret-token" {
		t.Fatalf("preview should keep value for validation, got %+v", preview.Diff)
	}
	redacted := redactPreview(preview)
	if redacted.Diff[0].Value != "[REDACTED]" {
		t.Fatalf("history redaction failed: %+v", redacted.Diff)
	}
}

func TestManagerSnapshotWarnsAndPreservesUnknownTopLevelFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\nfutureFeature:\n  enabled: true\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source, _ := config.NewConfigManager(path, "")
	manager, _ := NewManager(source)
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Warnings) != 1 || snapshot.Warnings[0].Path != "/futureFeature" {
		t.Fatalf("warnings=%+v", snapshot.Warnings)
	}
	if !strings.Contains(snapshot.YAML, "futureFeature") {
		t.Fatalf("unknown field was not preserved: %s", snapshot.YAML)
	}
}

func TestManagerStructuredCommitMigratesLegacyRouting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "startPort: 5800\nmodels: {}\ngroups:\n  default:\n    members: []\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	source, _ := config.NewConfigManager(path, "")
	manager, _ := NewManager(source)
	base, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	draft := Draft{Mode: ModeStructured, Changes: []Change{{Op: "replace", Path: "/startPort", Value: 5900}}}
	preview, err := manager.Preview(context.Background(), draft)
	if err != nil || !preview.Valid || len(preview.Migration) == 0 {
		t.Fatalf("migration preview=%+v err=%v", preview, err)
	}
	if _, err := manager.Commit(context.Background(), draft, base.ETag, preview.PreviewHash, Actor{ID: "test", Origin: "test"}); err != nil {
		t.Fatal(err)
	}
	updated, _ := os.ReadFile(path)
	if strings.Contains(string(updated), "\ngroups:") || !strings.Contains(string(updated), "routing:") {
		t.Fatalf("legacy routing was not migrated: %s", updated)
	}
}
