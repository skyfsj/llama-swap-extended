package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func testRotation(t *testing.T, dir string, maxDiskMiB, retainDays int) *extensionLogRotation {
	t.Helper()
	return newExtensionLogRotation(dir, config.ExtensionsConfig{LogMaxDiskMiB: maxDiskMiB, LogRetainDays: retainDays})
}

func TestServer_ExtensionLogRotationPrune(t *testing.T) {
	dir := t.TempDir()
	r := testRotation(t, dir, 1, 7)

	// Write two files: one fresh, one backdated beyond retention.
	fresh := filepath.Join(extensionLogDir(dir), "fresh.log")
	old := filepath.Join(extensionLogDir(dir), "old.log")
	if err := os.MkdirAll(extensionLogDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fresh, []byte(strings.Repeat("a", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(old, []byte(strings.Repeat("b", 512)), 0o600); err != nil {
		t.Fatal(err)
	}
	stale := time.Now().AddDate(0, 0, -10)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}

	r.prune()
	if _, err := os.Stat(fresh); err != nil {
		t.Fatalf("fresh log pruned: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("stale log survived prune: %v", err)
	}
}

func TestServer_ExtensionLogRotationSizeCap(t *testing.T) {
	dir := t.TempDir()
	// 1 MiB cap with 1.4 MiB total: the oldest file is trimmed to fit.
	r := testRotation(t, dir, 1, 30)
	if err := os.MkdirAll(extensionLogDir(dir), 0o700); err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(extensionLogDir(dir), "old.log")
	newer := filepath.Join(extensionLogDir(dir), "newer.log")
	if err := os.WriteFile(old, make([]byte, 700<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newer, make([]byte, 700<<10), 0o600); err != nil {
		t.Fatal(err)
	}
	// Backdate "old" so it is the prune candidate.
	stale := time.Now().Add(-time.Hour)
	if err := os.Chtimes(old, stale, stale); err != nil {
		t.Fatal(err)
	}

	r.prune()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("oldest log survived size prune: %v", err)
	}
	if _, err := os.Stat(newer); err != nil {
		t.Fatalf("newer log pruned: %v", err)
	}
}
