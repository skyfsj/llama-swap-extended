package extensions

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeExtensionTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for name, data := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, []byte(data), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// multiFileSource imports a sibling module so the bundle proves the whole tree
// took part in the build, not just the entrypoint.
const multiFileSource = `import { greeting } from "./lib/greeting.js";
export default { onRequest(ctx, request) { request.mark = greeting(); return request; } };`

func TestExtensions_MultiFileRoundTrip(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	draft := Definition{
		Manifest: Manifest{ID: "multi", Name: "Multi", Enabled: true},
		Files: map[string]string{
			"index.js":        multiFileSource,
			"lib/greeting.js": `export const greeting = () => "imported";`,
			"docs/readme.md":  "notes",
		},
	}
	saved, err := m.Save(context.Background(), draft, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(saved.Files) != 3 {
		t.Fatalf("saved files = %v", saved.Files)
	}
	onDisk, err := m.Get("multi")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if onDisk.Files["lib/greeting.js"] != draft.Files["lib/greeting.js"] {
		t.Fatalf("sibling module = %q", onDisk.Files["lib/greeting.js"])
	}
	if onDisk.ETag != saved.ETag {
		t.Fatalf("etag drift: %s vs %s", onDisk.ETag, saved.ETag)
	}
	item, ok := m.Compiled("multi")
	if !ok {
		t.Fatal("extension not compiled")
	}
	out, _, err := item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	if !strings.Contains(string(out), `"imported"`) {
		t.Fatalf("imported hook output = %s", out)
	}
	// A reload of an unchanged tree must reuse the compiled bundle, which only
	// holds when the directory ETag and the compiled ETag agree.
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if again, _ := m.Compiled("multi"); again != item {
		t.Fatal("unchanged tree was recompiled")
	}
	writeExtensionTree(t, filepath.Join(root, "multi"), map[string]string{"lib/extra.js": "export const x = 1;"})
	if err := m.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Compiled("multi"); !ok {
		t.Fatal("extension lost after new module appeared on disk")
	}
}

func TestExtensions_SaveRemovesDeletedFiles(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	draft := Definition{
		Manifest: Manifest{ID: "prune"},
		Files: map[string]string{
			"index.js":       "export default { tools: [] };",
			"lib/dropped.js": "export const unused = true;",
		},
	}
	saved, err := m.Save(context.Background(), draft, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "prune", "lib", "dropped.js")); err != nil {
		t.Fatalf("module was not written: %v", err)
	}
	draft.Files = map[string]string{"index.js": "export default { tools: [] };"}
	if _, err := m.Save(context.Background(), draft, saved.ETag); err != nil {
		t.Fatalf("resave: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "prune", "lib", "dropped.js")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed module still on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "prune", "lib")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("emptied directory still on disk: %v", err)
	}
	remaining, err := m.Get("prune")
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Files) != 1 {
		t.Fatalf("remaining files = %v", remaining.Files)
	}
}

func TestExtensions_SaveRejectsUnsafePaths(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"../escape.js", "/absolute.js", "lib/../../escape.js", "node_modules/pkg/index.js", ".hidden.js", "script.exe", "", "lib/"} {
		if _, err := m.Save(context.Background(), Definition{Manifest: Manifest{ID: "unsafe"}, Files: map[string]string{"index.js": "export default {}", path: "x"}}, ""); err == nil {
			t.Fatalf("path %q was accepted", path)
		}
	}
	if _, err := os.Stat(filepath.Join(root, "unsafe")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rejected extension left a directory behind: %v", err)
	}
}

func TestExtensions_SaveRequiresIndexJS(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	_, err = m.Save(context.Background(), Definition{Manifest: Manifest{ID: "noentry"}, Files: map[string]string{"lib/only.js": "export const x = 1;"}}, "")
	if err == nil || !strings.Contains(err.Error(), "index.js") {
		t.Fatalf("missing entrypoint error = %v", err)
	}
}

func TestExtensions_SaveEnforcesSizeLimits(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	oversized := strings.Repeat("a", MaxExtensionFileBytes+1)
	if _, err := m.Save(context.Background(), Definition{Manifest: Manifest{ID: "toobig"}, Files: map[string]string{"index.js": "export default {}", "big.js": oversized}}, ""); err == nil {
		t.Fatal("oversized file was accepted")
	}
	files := map[string]string{"index.js": "export default {}"}
	for i := 0; i <= MaxExtensionFiles; i++ {
		files[fmt.Sprintf("lib/f%04d.js", i)] = "export const x = 1;"
	}
	if _, err := m.Save(context.Background(), Definition{Manifest: Manifest{ID: "toomany"}, Files: files}, ""); err == nil {
		t.Fatal("too many files were accepted")
	}
}

func TestExtensions_SourceStampSeesNewFile(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "watched")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: watched\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("export default {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	before := m.sourceStamp()
	if err := os.WriteFile(filepath.Join(dir, "lib.go"), []byte("not worth reading"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.sourceStamp() != before {
		t.Fatal("unmanaged file changed the stamp")
	}
	if err := os.MkdirAll(filepath.Join(dir, "lib"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "lib", "new.js"), []byte("export const x = 1;"), 0o600); err != nil {
		t.Fatal(err)
	}
	if m.sourceStamp() == before {
		t.Fatal("new module did not change the stamp")
	}
}

func TestExtensions_SettingsDeclarationRoundTrip(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	source := `export const settings = {
  apiKey: { type: "string", label: "API Key", secret: true, required: true },
  locale: { type: "select", label: "Language", options: ["zh", "en"], default: "zh" },
  maxResults: { type: "number", label: "Results", default: 5, min: 1, max: 20 },
  verbose: { type: "boolean", label: "Verbose", default: true }
};
export default { onRequest(ctx, request) {
  request.apiKey = ctx.config.apiKey;
  request.locale = ctx.config.locale;
  request.maxResults = ctx.config.maxResults;
  request.verbose = ctx.config.verbose;
  return request;
} };`
	draft := Definition{Manifest: Manifest{ID: "configured", Enabled: true, Config: map[string]any{"apiKey": "secret-value"}}, Files: map[string]string{"index.js": source}}
	saved, err := m.Save(context.Background(), draft, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(saved.Settings) != 4 {
		t.Fatalf("declared settings = %+v", saved.Settings)
	}
	byKey := map[string]SettingField{}
	for _, field := range saved.Settings {
		byKey[field.Key] = field
	}
	if field := byKey["apiKey"]; field.Component != "password" || !field.Required || !field.Secret {
		t.Fatalf("apiKey field = %+v", field)
	}
	if field := byKey["locale"]; field.Component != "select" || field.Default != "zh" || len(field.Options) != 2 {
		t.Fatalf("locale field = %+v", field)
	}
	if field := byKey["maxResults"]; field.Min == nil || *field.Min != 1 || field.Max == nil || *field.Max != 20 {
		t.Fatalf("maxResults field = %+v", field)
	}
	stored, err := m.Get("configured")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Manifest.Config) != 1 {
		t.Fatalf("manifest stored defaults: %+v", stored.Manifest.Config)
	}
	item, _ := m.Compiled("configured")
	out, _, err := item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
	if err != nil {
		t.Fatalf("invoke: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatal(err)
	}
	if result["apiKey"] != "secret-value" || result["locale"] != "zh" || result["maxResults"] != float64(5) || result["verbose"] != true {
		t.Fatalf("merged config = %+v", result)
	}
}

func TestExtensions_SettingsDeclarationIsValidated(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{
		"unknown property":       `export const settings = { key: { type: "string", labl: "Typo" } }; export default { tools: [] };`,
		"unknown type":           `export const settings = { key: { type: "colour" } }; export default { tools: [] };`,
		"select no options":      `export const settings = { key: { type: "select" } }; export default { tools: [] };`,
		"bad key":                `export const settings = { "not an id": { type: "string" } }; export default { tools: [] };`,
		"default not in options": `export const settings = { key: { type: "select", options: ["a"], default: "b" } }; export default { tools: [] };`,
	} {
		_, err := m.Save(context.Background(), Definition{Manifest: Manifest{ID: "broken"}, Files: map[string]string{"index.js": source}}, "")
		if err == nil {
			t.Fatalf("%s: save succeeded", name)
		}
	}
}

func TestExtensions_SettingsValuesAreValidatedOnSave(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	source := `export const settings = {
  locale: { type: "select", options: ["zh", "en"] },
  maxResults: { type: "number", min: 1, max: 20 },
  apiKey: { type: "string", required: true }
}; export default { tools: [] };`
	draft := Definition{Manifest: Manifest{ID: "values", Config: map[string]any{"locale": "fr", "maxResults": float64(99), "apiKey": "k"}}, Files: map[string]string{"index.js": source}}
	_, err = m.Save(context.Background(), draft, "")
	var settingsErr *SettingsValidationError
	if !errors.As(err, &settingsErr) {
		t.Fatalf("error = %v", err)
	}
	paths := map[string]string{}
	for _, diagnostic := range settingsErr.Diagnostics {
		paths[diagnostic.Path] = diagnostic.Message
	}
	if paths["locale"] == "" || paths["maxResults"] == "" {
		t.Fatalf("diagnostics = %+v", settingsErr.Diagnostics)
	}
	// A missing required key must not be filled in by a default.
	draft.Manifest.Config = map[string]any{"apiKey": "k", "locale": "zh"}
	if _, err := m.Save(context.Background(), draft, ""); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
}

// Saving stages the manifest through YAML and recompiles from disk, so a
// number setting arrives at validation as int, not the float64 a JSON body
// decodes to. Both shapes must pass the same check.
func TestExtensions_SettingsNumberSurvivesSaveRoundTrip(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	source := `export const settings = { maxResults: { type: "number", min: 1, max: 20 } }; export default { tools: [] };`
	draft := Definition{Manifest: Manifest{ID: "numbers", Config: map[string]any{"maxResults": float64(8)}}, Files: map[string]string{"index.js": source}}
	saved, err := m.Save(context.Background(), draft, "")
	if err != nil {
		t.Fatalf("valid number setting rejected: %v", err)
	}
	stored, err := m.Get("numbers")
	if err != nil {
		t.Fatal(err)
	}
	storedValue, ok := numericSettingValue(stored.Manifest.Config["maxResults"])
	if !ok || storedValue != 8 {
		t.Fatalf("stored config = %+v", stored.Manifest.Config)
	}
	if saved.Manifest.ID != "numbers" {
		t.Fatalf("saved = %+v", saved.Manifest)
	}
}

func TestExtensions_SettingLabelLocalization(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	source := `export const settings = {
  logLevel: { type: "select", options: ["debug", "info"], label: { en: "Log level", "zh-CN": "日志级别" }, hint: { en: "Verbosity", "zh-CN": "详细程度" } }
}; export default { tools: [] };`
	if _, err := m.Save(context.Background(), Definition{Manifest: Manifest{ID: "i18n"}, Files: map[string]string{"index.js": source}}, ""); err != nil {
		t.Fatalf("save: %v", err)
	}
	stored, err := m.Get("i18n")
	if err != nil {
		t.Fatal(err)
	}
	if len(stored.Settings) != 1 {
		t.Fatalf("settings = %+v", stored.Settings)
	}
	field := stored.Settings[0]
	if field.Label != "Log level" || field.Hint != "Verbosity" {
		t.Fatalf("canonical label/hint = %q/%q", field.Label, field.Hint)
	}
	if field.LabelTranslations["zh-CN"] != "日志级别" || field.HintTranslations["zh-CN"] != "详细程度" {
		t.Fatalf("translations = %+v/%+v", field.LabelTranslations, field.HintTranslations)
	}

	broken := `export const settings = {
  logLevel: { type: "select", options: ["a"], label: { de: "Log-Ebene" } }
}; export default { tools: [] };`
	if _, err := m.Save(context.Background(), Definition{Manifest: Manifest{ID: "broken"}, Files: map[string]string{"index.js": broken}}, ""); err == nil {
		t.Fatal("unsupported language accepted")
	}
}

func TestExtensions_AsyncHookRejectionMessage(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: rejected\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default { async onRequest() { throw new Error("boom"); }, onToolCall() { throw "plain string"; } };`
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	item, err := compileDirectory(context.Background(), root)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = item.Invoke(context.Background(), "onRequest", Context{}, json.RawMessage(`{"model":"m"}`))
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("async rejection message = %v", err)
	}
	_, _, err = item.Invoke(context.Background(), "onToolCall", Context{}, json.RawMessage(`{"model":"m"}`))
	if err == nil || !strings.Contains(err.Error(), "plain string") {
		t.Fatalf("sync rejection message = %v", err)
	}
}

func TestExtensions_TinyHookBudgetStillCompiles(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "manifest.yaml"), []byte("id: quick\nenabled: true\ntimeout: 100ms\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "index.js"), []byte("export default { tools: [] };"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The metadata pass must not inherit the hook budget: a worker process can
	// take longer than the shortest legal timeout just to start.
	if _, err := compileDirectory(context.Background(), root); err != nil {
		t.Fatalf("compile: %v", err)
	}
}

func TestExtensions_CheckTreeDiagnostics(t *testing.T) {
	// A broken sibling is reported against its own path and position, not the
	// file that imports it.
	diagnostics := CheckTree(map[string]string{
		"index.js":      `import { value } from "./lib/broken.js"; export default { onRequest(ctx, r) { r.value = value; return r; } };`,
		"lib/broken.js": "export const value = ;",
	})
	if len(diagnostics) != 1 || diagnostics[0].Path != "lib/broken.js" || diagnostics[0].Line != 1 || diagnostics[0].Column != 22 {
		t.Fatalf("sibling diagnostics = %+v", diagnostics)
	}

	// A broken entrypoint is reported against the entrypoint.
	entry := CheckTree(map[string]string{"index.js": "export default {"})
	if len(entry) != 1 || entry[0].Path != "index.js" || entry[0].Severity != "error" {
		t.Fatalf("entrypoint diagnostics = %+v", entry)
	}

	missing := CheckTree(map[string]string{
		"index.js":     `import { helper } from "./lib/missing.js"; export default { tools: [] };`,
		"package.json": `{"name":"x","version":"1.0.0"}`,
	})
	if len(missing) != 1 || missing[0].Path != "index.js" || missing[0].Severity != "error" {
		t.Fatalf("unresolved relative import = %+v", missing)
	}

	// A dependency that npm ci installs at save time must not look broken here.
	withDependency := CheckTree(map[string]string{
		"index.js":     `import isNumber from "is-number"; export default { onRequest(ctx, r) { r.valid = isNumber("42"); return r; } };`,
		"package.json": `{"name":"x","version":"1.0.0","dependencies":{"is-number":"7.0.0"}}`,
	})
	if len(withDependency) != 0 {
		t.Fatalf("bare import reported: %+v", withDependency)
	}

	nested := CheckTree(map[string]string{
		"index.js":     `import { value } from "./lib/index.js"; export default { onRequest(ctx, r) { r.value = value; return r; } };`,
		"lib/index.js": "export const value = 1;",
	})
	if len(nested) != 0 {
		t.Fatalf("directory import reported: %+v", nested)
	}
}

func TestExtensions_CheckFileDiagnostics(t *testing.T) {
	errorsFound := CheckFile("package.json", `{"name":"x",}`)
	if len(errorsFound) != 1 || errorsFound[0].Line != 1 {
		t.Fatalf("json diagnostics = %+v", errorsFound)
	}
	if clean := CheckFile("index.js", "export default {};"); len(clean) != 0 {
		t.Fatalf("clean file reported %+v", clean)
	}
	if bad := CheckFile("index.js", "const a = ;"); len(bad) != 1 || bad[0].Line != 1 || bad[0].Column < 1 {
		t.Fatalf("position diagnostics = %+v", bad)
	}
}

func TestExtensions_SingleFileTreeStillWorks(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	saved, err := m.Save(context.Background(), Definition{Manifest: Manifest{ID: "single"}, Files: map[string]string{"index.js": "export default { tools: [] };"}}, "")
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	if len(saved.Files) != 1 || saved.Files["index.js"] != "export default { tools: [] };" {
		t.Fatalf("files = %+v", saved.Files)
	}
	if _, err := os.Stat(filepath.Join(root, "single", "manifest.yaml")); err != nil {
		t.Fatalf("manifest missing: %v", err)
	}
	draft, err := m.Get("single")
	if err != nil {
		t.Fatal(err)
	}
	if _, present := draft.Files["manifest.yaml"]; present {
		t.Fatal("manifest leaked into the editable tree")
	}
}
