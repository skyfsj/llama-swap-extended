package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestConfigManagerPatchPreservesCommentsAndETag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "# keep this comment\nstartPort: 5800\nmodels: {}\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	validation, err := m.ValidatePatch(context.Background(), []byte(`[{"op":"replace","path":"/startPort","value":5900}]`))
	if err != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v", validation, err)
	}
	updated, _, err := m.ApplyPatch(context.Background(), []byte(`[{"op":"replace","path":"/startPort","value":5900}]`), snapshot.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ETag == snapshot.ETag {
		t.Fatal("etag did not change")
	}
	got, _ := os.ReadFile(path)
	if !strings.Contains(string(got), "# keep this comment") || !strings.Contains(string(got), "startPort: 5900") {
		t.Fatalf("comment/order not preserved: %s", got)
	}
	if _, _, err := m.ApplyPatch(context.Background(), []byte(`[{"op":"replace","path":"/startPort","value":6000}]`), snapshot.ETag); err == nil {
		t.Fatal("stale etag should fail")
	}
}

func TestConfigManagerYAMLEditorValidatesAndApplies(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	valid, err := m.ValidateYAML(context.Background(), []byte("startPort: 5900\nmodels: {}\n"))
	if err != nil || !valid.Valid {
		t.Fatalf("valid YAML result = %+v err=%v", valid, err)
	}
	invalid, err := m.ValidateYAML(context.Background(), []byte("startPort: [broken\n"))
	if err != nil || invalid.Valid || len(invalid.Issues) == 0 {
		t.Fatalf("invalid YAML result = %+v err=%v", invalid, err)
	}
	updated, _, err := m.ApplyYAML(context.Background(), []byte("startPort: 5900\nmodels: {}\n"), snapshot.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ETag == snapshot.ETag {
		t.Fatal("YAML apply did not update etag")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "startPort: 5900") {
		t.Fatalf("YAML edit was not written: %s", content)
	}
}

func TestConfigManagerFallsBackForBusyMountedConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	old := []byte("startPort: 5800\nmodels: {}\n")
	updated := []byte("startPort: 5900\nmodels: {}\n")
	if err := os.WriteFile(path, old, 0o600); err != nil {
		t.Fatal(err)
	}

	previousRename := renameConfigFile
	renameConfigFile = func(_, _ string) error { return syscall.EBUSY }
	t.Cleanup(func() { renameConfigFile = previousRename })
	if err := atomicWriteWithBackup(path, updated); err != nil {
		t.Fatalf("busy mounted config write failed: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(updated) {
		t.Fatalf("updated config = %q, want %q", got, updated)
	}
	backup, err := os.ReadFile(path + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != string(old) {
		t.Fatalf("config backup = %q, want %q", backup, old)
	}
}

func TestConfigManagerApplySourcesRollsBackOnReloadFailure(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "config.yaml")
	overlay := filepath.Join(dir, "10-overlay.yaml")
	if err := os.WriteFile(base, []byte("startPort: 5800\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("logRequests: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(base, dir)
	if err != nil {
		t.Fatal(err)
	}
	sources, err := m.ReadSources(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m.SetReload(func(Config) error { return errors.New("reject candidate") })
	updates := map[string][]byte{base: []byte("startPort: 5900\nmodels: {}\n"), overlay: []byte("logRequests: true\n")}
	if _, err := m.ApplySources(context.Background(), updates, sources.ETag); err == nil {
		t.Fatal("expected reload rejection")
	}
	gotBase, _ := os.ReadFile(base)
	gotOverlay, _ := os.ReadFile(overlay)
	if string(gotBase) != string(sources.Data[base]) || string(gotOverlay) != string(sources.Data[overlay]) {
		t.Fatalf("sources were not restored: base=%q overlay=%q", gotBase, gotOverlay)
	}
}

func TestConfigManagerYAMLEditorPreservesRedactedSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "apiKeys:\n  - key: super-secret\n    scopes: [inference]\nmodels: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.YAML, "super-secret") || !strings.Contains(snapshot.YAML, "[REDACTED]") {
		t.Fatalf("snapshot redaction mismatch: %s", snapshot.YAML)
	}
	if _, _, err := m.ApplyYAML(context.Background(), []byte(snapshot.YAML), snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(updated), "super-secret") {
		t.Fatalf("redacted secret was not restored: %s", updated)
	}
}

func TestConfigManagerPatchRestoresUntouchedRedactedSecrets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "startPort: 5800\nmodelFiles:\n  maxFiles: 2000\n  downloads:\n    enabled: true\n    modelScopeToken: real-token-123\nmodels: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(snapshot.YAML, "[REDACTED]") || strings.Contains(snapshot.YAML, "real-token-123") {
		t.Fatalf("snapshot did not redact the token: %s", snapshot.YAML)
	}
	// The settings editor replaces the whole section with its draft. The
	// untouched token still carries the redaction sentinel.
	untouched := []byte(`[{"op":"replace","path":"/modelFiles","value":{"maxFiles":2000,"downloads":{"enabled":true,"modelScopeToken":"[REDACTED]"}}}]`)
	if _, _, err := m.ApplyPatch(context.Background(), untouched, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "[REDACTED]") || !strings.Contains(string(got), "modelScopeToken: real-token-123") {
		t.Fatalf("untouched token was overwritten: %s", got)
	}
	// A changed token must replace the file value instead of being restored.
	updated, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	changed := []byte(`[{"op":"replace","path":"/modelFiles","value":{"maxFiles":2000,"downloads":{"enabled":true,"modelScopeToken":"new-token-456"}}}]`)
	if _, _, err := m.ApplyPatch(context.Background(), changed, updated.ETag); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "modelScopeToken: new-token-456") || strings.Contains(string(got), "real-token-123") {
		t.Fatalf("changed token was not replaced: %s", got)
	}
}

func TestValidationPathFromError(t *testing.T) {
	cases := []struct {
		message string
		want    string
	}{
		{"modelFiles.maxFiles must be between 0 and 100000", "/modelFiles/maxFiles"},
		{"modelFiles.downloads.retryBackoff must be between 0 and 1h", "/modelFiles/downloads/retryBackoff"},
		{"resourceBudget.vramMiB and ramMiB must be >= 0", "/resourceBudget/vramMiB"},
		{"modelFiles.sources cannot contain an empty name", "/modelFiles/sources"},
		{"model alpha: backend.lmcache.port must be 0..65535", ""},
		{"yaml: unmarshal errors: line 2: cannot unmarshal !!str into int", ""},
	}
	for _, tc := range cases {
		if got := validationPathFromError(errors.New(tc.message)); got != tc.want {
			t.Errorf("validationPathFromError(%q) = %q, want %q", tc.message, got, tc.want)
		}
	}
	if validationPathFromError(nil) != "" {
		t.Error("validationPathFromError(nil) should be empty")
	}
}

func TestConfigManagerValidatePatchReportsFieldPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "startPort: 5800\nmodels: {}\nmodelFiles:\n  maxFiles: 100\n  downloads:\n    enabled: true\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	validation, err := m.ValidatePatch(context.Background(), []byte(`[{"op":"replace","path":"/modelFiles/maxFiles","value":200000}]`))
	if err != nil {
		t.Fatal(err)
	}
	if validation.Valid {
		t.Fatal("out-of-range maxFiles should be invalid")
	}
	if len(validation.Issues) != 1 {
		t.Fatalf("issues = %+v, want exactly one issue", validation.Issues)
	}
	if got := validation.Issues[0].Path; got != "/modelFiles/maxFiles" {
		t.Fatalf("issue path = %q, want /modelFiles/maxFiles", got)
	}
	if !strings.Contains(validation.Issues[0].Message, "modelFiles.maxFiles") {
		t.Fatalf("issue message = %q, want it to name the failing field", validation.Issues[0].Message)
	}
	// Duration limits are enforced by the loader too; the path must reach the
	// nested field so the settings UI can anchor the message to it.
	validation, err = m.ValidatePatch(context.Background(), []byte(`[{"op":"replace","path":"/modelFiles/downloads/retryBackoff","value":"1h30m"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if validation.Valid || len(validation.Issues) != 1 {
		t.Fatalf("validation = %+v, want one issue for out-of-range retryBackoff", validation)
	}
	if got := validation.Issues[0].Path; got != "/modelFiles/downloads/retryBackoff" {
		t.Fatalf("issue path = %q, want /modelFiles/downloads/retryBackoff", got)
	}
	// Errors that do not name a single field (type mismatches, parse
	// failures) keep an empty path so the UI shows them in the banner.
	validation, err = m.ValidatePatch(context.Background(), []byte(`[{"op":"replace","path":"/startPort","value":"abc"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if validation.Valid {
		t.Fatal("string startPort should be invalid")
	}
	if len(validation.Issues) == 0 || validation.Issues[0].Path != "" {
		t.Fatalf("issues = %+v, want a single issue with an empty path", validation.Issues)
	}
}

func TestConfigManagerPatchReplaceCreatesMissingKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "startPort: 5800\nmodels: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// The settings form edits the parsed config, where loader defaults such
	// as logLevel: info appear even when the source YAML never declared the
	// key. Replacing that key must create it in the file, not error.
	patch := []byte(`[{"op":"replace","path":"/logLevel","value":"debug"}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v, want a valid patch", validation, err)
	}
	_, _, err = m.ApplyPatch(context.Background(), patch, snapshot.ETag)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "logLevel: debug") {
		t.Fatalf("missing key was not created in the file: %s", got)
	}
}

func TestConfigManagerCanceledPatchDoesNotWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "startPort: 5800\nmodels: {}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, err = m.ApplyPatch(ctx, []byte(`[{"op":"replace","path":"/startPort","value":5900}]`), snapshot.ETag)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled patch error = %v, want context.Canceled", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != content {
		t.Fatalf("canceled patch changed source: %s", got)
	}
}

func TestConfigManagerSnapshotHonorsCanceledContextAndNilReceiver(t *testing.T) {
	var nilManager *ConfigManager
	if _, err := nilManager.Snapshot(context.Background()); err == nil || err.Error() != "config manager is nil" {
		t.Fatalf("nil manager error = %v", err)
	}
	if _, err := nilManager.ValidatePatch(context.Background(), []byte(`[]`)); err == nil || err.Error() != "config manager is nil" {
		t.Fatalf("nil ValidatePatch error = %v", err)
	}
	if _, _, err := nilManager.ApplyPatch(context.Background(), []byte(`[]`), ""); err == nil || err.Error() != "config manager is nil" {
		t.Fatalf("nil ApplyPatch error = %v", err)
	}
	if _, err := nilManager.ValidateYAML(context.Background(), []byte("{}\n")); err == nil || err.Error() != "config manager is nil" {
		t.Fatalf("nil ValidateYAML error = %v", err)
	}
	if _, _, err := nilManager.ApplyYAML(context.Background(), []byte("{}\n"), ""); err == nil || err.Error() != "config manager is nil" {
		t.Fatalf("nil ApplyYAML error = %v", err)
	}
	if _, err := nilManager.Rollback(context.Background()); err == nil || err.Error() != "config manager is nil" {
		t.Fatalf("nil Rollback error = %v", err)
	}

	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("models: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := m.Snapshot(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled snapshot error = %v, want context.Canceled", err)
	}
}

func TestConfigManagerPatchPreservesReplacedNodeComments(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "models:\n  m:\n    # command comment\n    cmd: llama-server # keep inline comment\n    proxy: http://127.0.0.1:9999\n    args: [--ctx-size] # keep sequence comment\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte(`[{"op":"replace","path":"/models/m/cmd","value":"new-server"},{"op":"replace","path":"/models/m/args/0","value":"--ctx-size=8192"}]`)
	if validation, validateErr := m.ValidatePatch(context.Background(), patch); validateErr != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v", validation, validateErr)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	if !strings.Contains(text, "# command comment") || !strings.Contains(text, "# keep inline comment") || !strings.Contains(text, "# keep sequence comment") {
		t.Fatalf("replacement dropped YAML comments: %s", text)
	}
	if !strings.Contains(text, "cmd: new-server") || !strings.Contains(text, "--ctx-size=8192") {
		t.Fatalf("replacement values missing: %s", text)
	}
}

func TestConfigManagerPatchSequenceAndRedactsStructuredKeys(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "models:\n  m:\n    cmd: echo ${PORT}\n    backend:\n      args: [--model, old.gguf]\napiKeys:\n  - key: super-secret\n    scopes: [inference]\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.YAML, "super-secret") {
		t.Fatalf("snapshot leaked api key: %s", snapshot.YAML)
	}
	if !json.Valid(snapshot.Config) {
		t.Fatalf("snapshot config is not valid JSON: %s", snapshot.Config)
	}
	if strings.Contains(string(snapshot.Config), "super-secret") {
		t.Fatalf("snapshot config leaked api key: %s", snapshot.Config)
	}
	patch := []byte(`[{"op":"replace","path":"/models/m/backend/args/1","value":"new.gguf"},{"op":"add","path":"/models/m/backend/args/-","value":"--ctx-size"}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v", validation, err)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	updated, _ := os.ReadFile(path)
	if !strings.Contains(string(updated), "new.gguf") || !strings.Contains(string(updated), "--ctx-size") {
		t.Fatalf("sequence patch not applied: %s", updated)
	}
}

func TestConfigManagerSnapshotRedactsLiteralEnvironmentValues(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	content := "models:\n  m:\n    env: [\"OPENAI_API_KEY=literal-secret\", \"CUDA_VISIBLE_DEVICES=1\", \"2\", \"3\", \"4\", \"MODEL_ROOT=${env.MODEL_ROOT}\"]\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(snapshot.YAML, "literal-secret") || strings.Contains(string(snapshot.Config), "literal-secret") {
		t.Fatalf("snapshot leaked literal environment secret: yaml=%s config=%s", snapshot.YAML, snapshot.Config)
	}
	if !strings.Contains(snapshot.YAML, "OPENAI_API_KEY=[REDACTED]") {
		t.Fatalf("snapshot did not redact environment value: %s", snapshot.YAML)
	}
	if !strings.Contains(snapshot.YAML, "CUDA_VISIBLE_DEVICES=1") {
		t.Fatalf("snapshot redacted non-sensitive runtime configuration: %s", snapshot.YAML)
	}
	for _, value := range []string{"\"2\"", "\"3\"", "\"4\""} {
		if !strings.Contains(snapshot.YAML, value) {
			t.Fatalf("snapshot redacted safe numeric environment entry %s: %s", value, snapshot.YAML)
		}
	}
	if !strings.Contains(snapshot.YAML, "${env.MODEL_ROOT}") {
		t.Fatalf("snapshot did not preserve environment placeholder: %s", snapshot.YAML)
	}
}

func TestIsSafeAnonymousEnvironmentValue(t *testing.T) {
	for _, value := range []string{"0", "1,2,3,4", " 2 "} {
		if !IsSafeAnonymousEnvironmentValue(value) {
			t.Fatalf("value %q was not recognized as a safe numeric environment entry", value)
		}
	}
	for _, value := range []string{"INHERITED", "token", "1,,2", "", "gpu:0"} {
		if IsSafeAnonymousEnvironmentValue(value) {
			t.Fatalf("value %q was incorrectly recognized as a safe anonymous environment entry", value)
		}
	}
}

func TestConfigManagerConfigDirKeepsSourceOwnership(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "10-base.yaml")
	if err := os.WriteFile(base, []byte("# base comment\nmodels:\n  base:\n    cmd: llama-server\n    proxy: http://localhost:9999\n"), 0600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	add := []byte(`[{"op":"add","path":"/models/new","value":{"cmd":"vllm","proxy":"http://localhost:9998"}}]`)
	if _, _, err := m.ApplyPatch(context.Background(), add, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	baseAfter, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseAfter) != "# base comment\nmodels:\n  base:\n    cmd: llama-server\n    proxy: http://localhost:9999\n" {
		t.Fatalf("base source was rewritten: %s", baseAfter)
	}
	managed := filepath.Join(dir, "90-llama-swap-managed.yaml")
	managedAfter, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(managedAfter), "base:") || !strings.Contains(string(managedAfter), "new:") {
		t.Fatalf("managed overlay contains the wrong entities: %s", managedAfter)
	}

	snapshot, err = m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	replace := []byte(`[{"op":"replace","path":"/models/base/cmd","value":"updated"}]`)
	if _, _, err := m.ApplyPatch(context.Background(), replace, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	baseAfter, err = os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(baseAfter), "cmd: updated") || strings.Contains(string(baseAfter), "models:\n  new:") {
		t.Fatalf("existing entity was not updated in its source: %s", baseAfter)
	}
}

func TestConfigManagerRejectsReadOnlySourcePatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "10-readonly.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\n"), 0400); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte(`[{"op":"replace","path":"/startPort","value":5900}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil {
		t.Fatal(err)
	}
	if validation.Valid {
		t.Fatal("read-only source patch should be rejected during validation")
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err == nil {
		t.Fatal("read-only source patch should fail")
	}
}

func TestConfigManagerWritesNewEntityToManagedOverlayWhenBaseReadOnly(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "10-base.yaml")
	content := "models:\n  base:\n    cmd: llama --port ${PORT}\n"
	if err := os.WriteFile(base, []byte(content), 0400); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Writable {
		t.Fatal("managed overlay should make a read-only base configuration writable")
	}
	patch := []byte(`[{"op":"add","path":"/models/new","value":{"cmd":"vllm --port ${PORT}"}}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation=%+v err=%v", validation, err)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	baseAfter, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseAfter) != content {
		t.Fatalf("read-only base was rewritten: %s", baseAfter)
	}
	managed, err := os.ReadFile(filepath.Join(dir, "90-llama-swap-managed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(managed), "new:") || strings.Contains(string(managed), "base:") {
		t.Fatalf("managed overlay contains the wrong entities: %s", managed)
	}
}

func TestConfigManagerWritesNewRuntimeToManagedOverlay(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "10-base.yaml")
	content := "runtimes:\n  base:\n    kind: vllm\n    source:\n      type: pypi\n"
	if err := os.WriteFile(base, []byte(content), 0400); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte(`[{"op":"add","path":"/runtimes/new","value":{"kind":"llamacpp","source":{"type":"release","url":"https://github.com/ggml-org/llama.cpp/releases/download/b1/llama.tar.gz"}}}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation=%+v err=%v", validation, err)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	baseAfter, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseAfter) != content {
		t.Fatalf("read-only base was rewritten: %s", baseAfter)
	}
	managed, err := os.ReadFile(filepath.Join(dir, "90-llama-swap-managed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	managedText := string(managed)
	if !strings.Contains(managedText, "new:") || strings.Contains(managedText, "base:") {
		t.Fatalf("managed overlay contains the wrong runtimes: %s", managedText)
	}
}

func TestConfigManagerWritesNewModelFileSourceToManagedOverlay(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "10-base.yaml")
	content := "modelFiles:\n  sources:\n    base:\n      type: directory\n      path: /models\n"
	if err := os.WriteFile(base, []byte(content), 0400); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte(`[{"op":"add","path":"/modelFiles/sources/extra","value":{"type":"directory","path":"/data/models"}}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation=%+v err=%v", validation, err)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	baseAfter, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseAfter) != content {
		t.Fatalf("read-only base was rewritten: %s", baseAfter)
	}
	managed, err := os.ReadFile(filepath.Join(dir, "90-llama-swap-managed.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	managedText := string(managed)
	if !strings.Contains(managedText, "extra:") || strings.Contains(managedText, "base:") {
		t.Fatalf("managed overlay contains the wrong model file sources: %s", managedText)
	}
}

func TestPatchNeedsRestartUsesPathBoundaries(t *testing.T) {
	if patchNeedsRestart([]PatchOp{{Path: "/store/pathology"}}) {
		t.Fatal("store/pathology must not require restart")
	}
	if !patchNeedsRestart([]PatchOp{{Path: "/store/path"}}) || !patchNeedsRestart([]PatchOp{{Path: "/runtimeManager/root/child"}}) {
		t.Fatal("store.path and runtimeManager.root descendants must require restart")
	}
	if !patchNeedsRestart([]PatchOp{{Op: "move", Path: "/runtimeManager", From: "/store/path"}}) {
		t.Fatal("copy/move source paths must participate in restart detection")
	}
}

func TestConfigManagerSnapshotReportsSourceOwnership(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "10-base.yaml")
	overlay := filepath.Join(dir, "20-overlay.yaml")
	if err := os.WriteFile(base, []byte("startPort: 5800\nmodels:\n  base:\n    cmd: llama\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("models:\n  extra:\n    cmd: vllm\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Ownership["/startPort"] != base || snapshot.Ownership["/models/base/cmd"] != base || snapshot.Ownership["/models/extra/cmd"] != overlay {
		t.Fatalf("ownership = %#v", snapshot.Ownership)
	}
}

func TestConfigManagerConfigDirReplacesExactBaseField(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "10-base.yaml")
	overlay := filepath.Join(dir, "20-overlay.yaml")
	if err := os.WriteFile(base, []byte("startPort: 5800\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("audit:\n  enabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte(`[{"op":"replace","path":"/startPort","value":5900}]`)
	if validation, validateErr := m.ValidatePatch(context.Background(), patch); validateErr != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v", validation, validateErr)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "startPort: 5900") {
		t.Fatalf("base exact field was not updated: %s", data)
	}
	data, err = os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "startPort") {
		t.Fatalf("unrelated overlay was rewritten: %s", data)
	}
}

func TestConfigManagerRFC6902TestCopyMove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("models:\n  one:\n    cmd: llama\n    proxy: http://127.0.0.1:9001\n  two:\n    cmd: vllm\n    proxy: http://127.0.0.1:9002\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	patch := []byte(`[{"op":"test","path":"/models/one/cmd","value":"llama"},{"op":"copy","from":"/models/one","path":"/models/copied"},{"op":"move","from":"/models/two","path":"/models/moved"}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v", validation, err)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if !strings.Contains(text, "copied:") || !strings.Contains(text, "moved:") || strings.Contains(text, "two:\n") {
		t.Fatalf("copy/move not applied: %s", text)
	}
}

func TestConfigManagerRFC6902RejectsFailedTestAndMissingMember(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("startPort: 5800\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, patch := range []string{
		`[{"op":"test","path":"/startPort","value":5801}]`,
		`[{"op":"replace","path":"/startPort"}]`,
		`[{"op":"copy","path":"/other"}]`,
	} {
		validation, validateErr := m.ValidatePatch(context.Background(), []byte(patch))
		if validateErr == nil && validation.Valid {
			t.Fatalf("patch should be rejected: %s", patch)
		}
	}
}

func TestConfigManagerRFC6902AddRejectsMissingParent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("models: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	validation, err := m.ValidatePatch(context.Background(), []byte(`[{"op":"add","path":"/models/new/backend/args","value":[]}]`))
	if err != nil {
		t.Fatal(err)
	}
	if validation.Valid {
		t.Fatal("add with a missing intermediate parent should be rejected")
	}
}

func TestConfigManagerRejectsSymlinkedRootsAndSkipsSymlinkSources(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	target := filepath.Join(dir, "target.yaml")
	if err := os.WriteFile(target, []byte("startPort: 5800\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, configPath); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfigManager(configPath, ""); err == nil {
		t.Fatal("symlinked config path should be rejected")
	}

	configDir := filepath.Join(dir, "configs")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	valid := filepath.Join(configDir, "10-valid.yaml")
	linked := filepath.Join(configDir, "20-linked.yaml")
	if err := os.WriteFile(valid, []byte("startPort: 5900\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, linked); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", configDir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	managed := filepath.Join(configDir, "90-llama-swap-managed.yaml")
	if len(snapshot.Sources) != 2 || snapshot.Sources[0].Path != valid || snapshot.Sources[1].Path != managed || !snapshot.Sources[1].Managed {
		t.Fatalf("sources = %+v, symlink source was not skipped or managed destination missing", snapshot.Sources)
	}
}

func TestConfigManagerRejectsSymlinkedParentComponents(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creating symlinks requires additional Windows privileges")
	}
	dir := t.TempDir()
	realDir := filepath.Join(dir, "real")
	if err := os.Mkdir(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedDir := filepath.Join(dir, "linked")
	if err := os.Symlink(realDir, linkedDir); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(linkedDir, "config.yaml")
	if _, err := NewConfigManager(configPath, ""); err == nil {
		t.Fatal("config path below a symlinked parent should be rejected")
	}
	if _, err := NewConfigManager("", linkedDir); err == nil {
		t.Fatal("config directory below a symlinked parent should be rejected")
	}
}

func TestConfigManagerRejectsConfigDirectoryReplacedBySymlink(t *testing.T) {
	dir := t.TempDir()
	configDir := filepath.Join(dir, "configs")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(configDir, "10-base.yaml"), []byte("startPort: 5800\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", configDir)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(dir, "moved-configs")
	if err := os.Rename(configDir, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(moved, configDir); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("config directory replaced by symlink was accepted: %v", err)
	}
}

func TestConfigManagerEmptyDirectorySnapshotIsWritable(t *testing.T) {
	dir := t.TempDir()
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Writable || snapshot.ETag == "" {
		t.Fatalf("empty directory snapshot should be writable and etagged: %+v", snapshot)
	}
	managed := filepath.Join(dir, "90-llama-swap-managed.yaml")
	if len(snapshot.Sources) != 1 || snapshot.Sources[0].Path != managed || !snapshot.Sources[0].Managed {
		t.Fatalf("snapshot sources = %+v", snapshot.Sources)
	}
	patch := []byte(`[{"op":"add","path":"/startPort","value":5900}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v", validation, err)
	}
	updated, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ETag == snapshot.ETag {
		t.Fatal("etag did not change after creating managed source")
	}
	data, err := os.ReadFile(managed)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "startPort: 5900") {
		t.Fatalf("managed source was not written: %s", data)
	}
}

func TestConfigManagerMissingConfigPathSnapshotTargetsConfigFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	m, err := NewConfigManager(path, "")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := m.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Sources) != 1 || snapshot.Sources[0].Path != path || snapshot.Sources[0].Managed || !snapshot.Sources[0].Writable {
		t.Fatalf("missing config path should be the writable source: %+v", snapshot.Sources)
	}
	patch := []byte(`[{"op":"add","path":"/startPort","value":5900}]`)
	validation, err := m.ValidatePatch(context.Background(), patch)
	if err != nil || !validation.Valid {
		t.Fatalf("validation = %+v err=%v", validation, err)
	}
	if _, _, err := m.ApplyPatch(context.Background(), patch, snapshot.ETag); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "startPort: 5900") {
		t.Fatalf("config path was not written: %s", data)
	}
	if _, err := os.Stat(filepath.Join(dir, "90-llama-swap-managed.yaml")); !os.IsNotExist(err) {
		t.Fatalf("config-path-only manager unexpectedly created managed overlay, err=%v", err)
	}
}

func TestConfigManagerRollbackUsesNewestBackup(t *testing.T) {
	dir := t.TempDir()
	base := filepath.Join(dir, "10-base.yaml")
	overlay := filepath.Join(dir, "20-overlay.yaml")
	if err := os.WriteFile(base, []byte("startPort: 6000\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlay, []byte("logToStdout: proxy\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	baseBackup := base + ".bak"
	overlayBackup := overlay + ".bak"
	if err := os.WriteFile(baseBackup, []byte("startPort: 5800\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(overlayBackup, []byte("logToStdout: none\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Minute)
	newer := old.Add(time.Second)
	if err := os.Chtimes(baseBackup, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(overlayBackup, newer, newer); err != nil {
		t.Fatal(err)
	}
	m, err := NewConfigManager("", dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Rollback(context.Background()); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(overlay)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "logToStdout: none\n" {
		t.Fatalf("rollback restored wrong source: %s", data)
	}
	baseData, err := os.ReadFile(base)
	if err != nil {
		t.Fatal(err)
	}
	if string(baseData) != "startPort: 6000\n" {
		t.Fatalf("older backup should not be restored: %s", baseData)
	}
}
