package logarchive

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStore_RotatesEachIncidentKind(t *testing.T) {
	store, err := New(Config{Dir: t.TempDir(), MaxFiles: 2})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Save(KindInferenceCrash, "models/qwen", []byte{byte('0' + i)}); err != nil {
			t.Fatalf("Save crash %d: %v", i, err)
		}
	}
	if _, err := store.Save(KindRequestError, "api", []byte("request")); err != nil {
		t.Fatalf("Save request error: %v", err)
	}

	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 3 {
		t.Fatalf("entries=%d, want 3 (two crash plus one request)", len(entries))
	}
	crashes := 0
	for _, entry := range entries {
		if entry.Kind != KindInferenceCrash {
			continue
		}
		crashes++
		if strings.Contains(entry.Name, "/") || strings.Contains(entry.Name, "\\") {
			t.Fatalf("unsafe entry name %q", entry.Name)
		}
	}
	if crashes != 2 {
		t.Fatalf("crash entries=%d, want 2", crashes)
	}
	data, _, err := store.Read(entries[0].Name)
	if err != nil {
		t.Fatalf("Read newest: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("newest snapshot is empty")
	}
}

func TestStore_SetMaxFilesPrunesImmediately(t *testing.T) {
	dir := t.TempDir()
	store, err := New(Config{Dir: dir, MaxFiles: 3})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for i := 0; i < 3; i++ {
		if _, err := store.Save(KindRequestError, "model", []byte("request")); err != nil {
			t.Fatalf("Save %d: %v", i, err)
		}
	}
	if err := store.SetMaxFiles(1); err != nil {
		t.Fatalf("SetMaxFiles: %v", err)
	}
	entries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%d, want 1", len(entries))
	}
}

func TestStore_RejectsUnsafeReadName(t *testing.T) {
	store, err := New(Config{Dir: t.TempDir()})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, name := range []string{"../secret.log", "request-error__bad.log", ""} {
		if _, _, err := store.Read(name); err == nil {
			t.Errorf("Read(%q) succeeded, want error", name)
		}
	}
	outside := filepath.Join(filepath.Dir(t.TempDir()), "not-created")
	if _, err := os.Stat(outside); !os.IsNotExist(err) {
		t.Fatalf("test setup unexpectedly created %q", outside)
	}
}
