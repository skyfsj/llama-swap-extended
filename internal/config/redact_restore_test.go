package config

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfigForRestoreTest(t *testing.T, content string) (*ConfigManager, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	return manager, path
}

func applyRestorePatch(t *testing.T, manager *ConfigManager, path, patch string) string {
	t.Helper()
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := manager.ApplyPatch(context.Background(), []byte(patch), snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(written)
}

// TestConfigManagerRestoreEnvEntriesByName covers the environment case: the
// variable name survives redaction, so a placeholder follows its own variable
// even when the operator removed or reordered the lines around it. Pairing by
// position instead handed the value of a neighbouring entry to the placeholder:
// deleting a line above a secret entry resurrected the deleted variable and
// dropped the secret.
func TestConfigManagerRestoreEnvEntriesByName(t *testing.T) {
	manager, path := writeConfigForRestoreTest(t, strings.Join([]string{
		"models:",
		"  m:",
		"    cmd: llama-server --port ${PORT}",
		"    env:",
		"      - \"A=1\"",
		"      - \"OPENAI_API_KEY=sk-secret\"",
		"      - \"B=2\"",
		"",
	}, "\n"))

	// The editor removes the first line (A=1) and keeps the redacted secret.
	written := applyRestorePatch(t, manager, path,
		`[{"op":"replace","path":"/models/m/env","value":["OPENAI_API_KEY=[REDACTED]","B=2"]}]`)
	if strings.Contains(written, "A=1") {
		t.Fatalf("deleted environment entry came back:\n%s", written)
	}
	if !strings.Contains(written, "OPENAI_API_KEY=sk-secret") {
		t.Fatalf("the redacted secret was not restored to its own variable:\n%s", written)
	}
	if !strings.Contains(written, "B=2") {
		t.Fatalf("untouched environment entry was lost:\n%s", written)
	}

	// Reordering the lines moves each secret with its variable.
	written = applyRestorePatch(t, manager, path,
		`[{"op":"replace","path":"/models/m/env","value":["B=2","OPENAI_API_KEY=[REDACTED]"]}]`)
	if !strings.Contains(written, "OPENAI_API_KEY=sk-secret") {
		t.Fatalf("reordered redacted entry lost its secret:\n%s", written)
	}
}

// TestConfigManagerRestoreKeepsAnonymousEnvPosition pins the fallback for
// entries without a NAME= prefix: they carry no identity, so the previous
// positional behaviour is preserved instead of failing the save.
func TestConfigManagerRestoreKeepsAnonymousEnvPosition(t *testing.T) {
	manager, path := writeConfigForRestoreTest(t, strings.Join([]string{
		"models:",
		"  m:",
		"    cmd: llama-server --port ${PORT}",
		"    env:",
		"      - \"INHERITED\"",
		"      - \"A=1\"",
		"",
	}, "\n"))
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.YAML, "[REDACTED]") {
		t.Fatalf("anonymous value was not redacted: %s", snapshot.YAML)
	}
	written := applyRestorePatch(t, manager, path,
		`[{"op":"replace","path":"/models/m/env","value":["[REDACTED]","A=1"]}]`)
	if !strings.Contains(written, "INHERITED") {
		t.Fatalf("anonymous redacted entry was not restored:\n%s", written)
	}
}

// TestConfigManagerRejectsUnresolvableRedactedSequences pins the guard for
// sequences whose placeholders cannot be paired with an original: an apiKeys
// list that changed length, and an env placeholder whose variable no longer
// exists. Both used to be restored by position, which wrote some other entry's
// secret; the save now fails before anything reaches disk.
func TestConfigManagerRejectsUnresolvableRedactedSequences(t *testing.T) {
	manager, path := writeConfigForRestoreTest(t, strings.Join([]string{
		"startPort: 5800",
		"apiKeys:",
		"  - sk-leaked",
		"  - sk-good",
		"models:",
		"  m:",
		"    cmd: llama-server --port ${PORT}",
		"    env:",
		"      - \"OPENAI_API_KEY=sk-secret\"",
		"",
	}, "\n"))

	// Deleting one of two redacted keys: position pairing would keep
	// "sk-leaked" — the key the operator removed — and drop "sk-good".
	snapshot, err := manager.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte(`[{"op":"replace","path":"/apiKeys","value":["[REDACTED]"]}]`)
	if _, _, err := manager.ApplyPatch(context.Background(), patch, snapshot.ETag); err == nil {
		t.Fatalf("a shorter redacted apiKeys list was accepted:\n%s", err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "sk-good") || !strings.Contains(string(written), "sk-leaked") {
		t.Fatalf("a rejected patch still rewrote the source:\n%s", written)
	}

	// A placeholder for a variable the stored config does not define has no
	// value to restore either.
	patch = []byte(`[{"op":"replace","path":"/models/m/env","value":["RENAMED_KEY=[REDACTED]"]}]`)
	if _, _, err := manager.ApplyPatch(context.Background(), patch, snapshot.ETag); err == nil {
		t.Fatal("a placeholder without a matching stored variable was accepted")
	}
}

// TestConfigManagerRestoreInPlaceRedactedReplacements pins that the common
// cases still restore: an unchanged length with a retyped neighbour, and a
// freshly typed apiKeys list that carries no placeholder at all.
func TestConfigManagerRestoreInPlaceRedactedReplacements(t *testing.T) {
	manager, path := writeConfigForRestoreTest(t, strings.Join([]string{
		"startPort: 5800",
		"apiKeys:",
		"  - sk-first",
		"  - sk-second",
		"",
	}, "\n"))

	written := applyRestorePatch(t, manager, path,
		`[{"op":"replace","path":"/apiKeys","value":["sk-retyped","[REDACTED]"]}]`)
	if !strings.Contains(written, "sk-retyped") || !strings.Contains(written, "sk-second") {
		t.Fatalf("in-place redacted replacement lost a value:\n%s", written)
	}
	if strings.Contains(written, "[REDACTED]") || strings.Contains(written, "sk-first") {
		t.Fatalf("placeholder or replaced value survived the commit:\n%s", written)
	}

	// A list written without placeholders is not a restore candidate at all,
	// so its length may change freely.
	written = applyRestorePatch(t, manager, path,
		`[{"op":"replace","path":"/apiKeys","value":["sk-a","sk-b","sk-c"]}]`)
	for _, want := range []string{"sk-a", "sk-b", "sk-c"} {
		if !strings.Contains(written, want) {
			t.Fatalf("fresh apiKeys list was not written (%s missing):\n%s", want, written)
		}
	}
	if strings.Contains(written, "sk-second") {
		t.Fatalf("replaced apiKeys list kept an old key:\n%s", written)
	}
}
