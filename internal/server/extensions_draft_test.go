package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// draftTestServer has one stored extension so the draft-inherits-manifest path
// can be exercised next to the brand-new-extension path.
func draftTestServer(t *testing.T) *Server {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "stored")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: stored\nenabled: false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte("export default { onRequest(ctx, request) { return request; } };\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	return s
}

func draftTestPayload(t *testing.T, files map[string]string) string {
	t.Helper()
	body := map[string]any{
		"context": map[string]any{"endpoint": "chat.completions", "resolvedModel": "m"},
		"request": map[string]any{"model": "m", "messages": []any{}},
		"files":   files,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestServer_ExtensionDebugChatRunsDraft(t *testing.T) {
	var upstreamBody string
	s := debugChatTestServer(t, &upstreamBody)
	// The draft marks the request; the stored debugext sets temperature only.
	draft := map[string]string{
		"index.js": `export default {
  async onRequest(ctx, request) { request.messages.push({ role: "system", content: "draft-ran" }); return request; }
};`,
	}
	payload, err := json.Marshal(map[string]any{
		"request": map[string]any{"model": "m", "stream": false, "messages": []any{}},
		"files":   draft,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/debugext/debug/chat", strings.NewReader(string(payload)))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("draft chat status = %d body %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(upstreamBody, "draft-ran") {
		t.Fatalf("draft source did not run: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, `"temperature":0.1`) {
		t.Fatalf("stored source ran instead of the draft: %s", upstreamBody)
	}
}

func TestServer_ExtensionDebugChatDraftCompileError(t *testing.T) {
	var upstreamBody string
	s := debugChatTestServer(t, &upstreamBody)
	payload := `{"request":{"model":"m","messages":[]},"files":{"index.js":"export default { onRequest( {"}}`
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/debugext/debug/chat", strings.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "draft failed to compile") {
		t.Fatalf("draft chat compile error: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestServer_ExtensionTestRunsDraftSource(t *testing.T) {
	s := draftTestServer(t)
	// The draft differs from the stored source; the result must reflect the
	// draft, proving debug runs live code without saving.
	draft := map[string]string{
		"index.js": "export default { onRequest(ctx, request) { request.temperature = 0.11; return request; } };\n",
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/stored/test", strings.NewReader(draftTestPayload(t, draft)))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("draft test status = %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Matched      bool           `json:"matched"`
		RequestAfter map[string]any `json:"requestAfter"`
		Error        string         `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error != "" {
		t.Fatalf("draft run errored: %s", result.Error)
	}
	// The stored extension is disabled, but a draft run debugs unsaved code so
	// the enabled flag must not gate matching.
	if !result.Matched {
		t.Fatal("draft test reported unmatched for a disabled draft")
	}
	if temperature, _ := result.RequestAfter["temperature"].(float64); temperature != 0.11 {
		t.Fatalf("draft source was not executed: %+v", result.RequestAfter)
	}
}

func TestServer_ExtensionTestRunsNewExtensionWithoutSaving(t *testing.T) {
	s := draftTestServer(t)
	draft := map[string]string{
		"index.js": "export default { onRequest(ctx, request) { request.marker = \"fresh\"; return request; } };\n",
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/brand-new/test", strings.NewReader(draftTestPayload(t, draft)))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("new-extension draft status = %d body %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		Matched      bool           `json:"matched"`
		RequestAfter map[string]any `json:"requestAfter"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	if !result.Matched || result.RequestAfter["marker"] != "fresh" {
		t.Fatalf("new extension draft did not run: %+v", result)
	}
	// Nothing was persisted: the manager still only knows the stored one.
	if ids := len(s.currentExtensions().List()); ids != 1 {
		t.Fatalf("draft test leaked an extension into the manager: %d items", ids)
	}
}

func TestServer_ExtensionTestDraftCompileErrorSurfaces(t *testing.T) {
	s := draftTestServer(t)
	draft := map[string]string{"index.js": "export default { onRequest( { "}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/stored/test", strings.NewReader(draftTestPayload(t, draft)))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "draft failed to compile") {
		t.Fatalf("compile error: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

func TestServer_ExtensionTestStoredPathUnchanged(t *testing.T) {
	s := draftTestServer(t)
	// Without files the stored (disabled) definition is used, so the run is
	// unmatched exactly as before.
	recorder := httptest.NewRecorder()
	body := `{"context":{"endpoint":"chat.completions","resolvedModel":"m"},"request":{"model":"m","messages":[]}}`
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/stored/test", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("stored test status = %d", recorder.Code)
	}
	var result struct {
		Matched bool `json:"matched"`
	}
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	if result.Matched {
		t.Fatal("stored disabled extension must stay unmatched")
	}
	// Unknown id without files is still a 404.
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/api/extensions/missing/test", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("missing extension status = %d", recorder.Code)
	}
}
