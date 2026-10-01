package extensions

import (
	"context"
	"strings"
	"testing"
)

// A module with only named exports (no `default`) used to nil-panic in the
// worker: goja's namespace.Get("default") returns a Go nil *Object rather than
// an undefined/null value, and ToObject on it segfaulted. It must surface as a
// clear compile-time error instead.
func TestWorker_NoDefaultExportIsClearError(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{ID: "logtest", Name: "logtest"}
	files := map[string]string{"index.js": `export const settings = {};
export const hooks = {
  async settings({ ctx }) {
    ctx.log("onResponse hook record");
    return {};
  }
};
`}
	_, err = m.CompileDraft(context.Background(), manifest, files)
	if err == nil {
		t.Fatalf("expected a clear compile error for a missing default export")
	}
	if !strings.Contains(err.Error(), "no default export") {
		t.Fatalf("expected 'no default export' in error, got: %v", err)
	}
}

// A draft without any default export must not panic in the worker subprocess
// (which surfaced to users as the opaque "runtime error: invalid memory
// address or nil pointer dereference").
func TestWorker_NilDefaultObjectNoPanic(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{ID: "logtest", Name: "logtest"}
	files := map[string]string{"index.js": `export const settings = {};`}
	_, err = m.CompileDraft(context.Background(), manifest, files)
	if err == nil {
		t.Fatalf("expected an error for a module without a default export")
	}
	if strings.Contains(err.Error(), "invalid memory address") {
		t.Fatalf("panic leaked into the error text: %v", err)
	}
}

// A valid extension (default export + named settings) still compiles and its
// ctx.log records flow back through the response logs.
func TestWorker_ValidExtensionLogsFlow(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{ID: "logtest", Name: "logtest"}
	files := map[string]string{"index.js": `export const settings = {};
export default {
  async onResponse(ctx) {
    ctx.log.info("onResponse hook record");
    ctx.log.error("error level record");
    return {};
  }
};`}
	compiled, err := m.CompileDraft(context.Background(), manifest, files)
	if err != nil {
		t.Fatalf("CompileDraft: %v", err)
	}
	_, logs, err := compiled.Invoke(context.Background(), "onResponse", Context{RequestID: "t1"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("Invoke onResponse: %v", err)
	}
	found := false
	for _, line := range logs {
		if strings.Contains(line, "info: onResponse hook record") {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected 'onResponse hook record' in logs, got %q", logs)
	}
}

// Bare console.* must work inside hooks (Node executor exposes console
// globally; goja used to throw "console is not defined").
func TestWorker_BareConsoleInHook(t *testing.T) {
	root := t.TempDir()
	m, err := NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest := Manifest{ID: "logtest", Name: "logtest"}
	files := map[string]string{"index.js": `export const settings = {};
export default {
  async onResponse(ctx, response) {
    console.log("bare console record");
    console.warn("bare warn record");
    return response;
  }
};`}
	compiled, err := m.CompileDraft(context.Background(), manifest, files)
	if err != nil {
		t.Fatalf("CompileDraft: %v", err)
	}
	_, logs, err := compiled.Invoke(context.Background(), "onResponse", Context{RequestID: "t1"}, []byte(`{}`))
	if err != nil {
		t.Fatalf("Invoke onResponse: %v", err)
	}
	joined := strings.Join(logs, "\n")
	if !strings.Contains(joined, "info: bare console record") || !strings.Contains(joined, "warn: bare warn record") {
		t.Fatalf("expected bare console records in logs, got %q", logs)
	}
}
