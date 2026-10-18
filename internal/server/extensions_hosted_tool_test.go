package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// TestServer_ExtensionInjectedHostedToolSurvivesStrip guards the marker that
// tells a hook-injected provider-hosted tool apart from a client-sent one: the
// adapter strips the latter (upstreams reject them) but must forward the former,
// or a preset like openai-web-search becomes a no-op.
// TestServer_ExtensionInjectedHostedToolSurvivesStripOnStreamBridge covers the
// second forward path: when any matched extension needs the stream bridge, the
// round request is built by serveExtensionStream, and it must carry the same
// marker or a streaming request loses the injected hosted tool.
func TestServer_ExtensionInjectedHostedToolSurvivesStripOnStreamBridge(t *testing.T) {
	root := t.TempDir()
	// The web-search preset injects a hosted tool; the second extension has a
	// server tool, which is what forces the stream bridge.
	for _, spec := range []struct{ id, manifest, script string }{
		{"websearch", "id: websearch\nenabled: true\npriority: 10\nmatch:\n  endpoints: [chat.completions]\n",
			`export default {
  async onRequest(ctx, request) { request.tools = request.tools || []; request.tools.push({ type: "web_search" }); return request; }
};`},
		{"server-tool", "id: server-tool\nenabled: true\npriority: 20\nmatch:\n  endpoints: [chat.completions]\n",
			`export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object" } } }],
  onToolCall(ctx, call) { return { content: "ok" }; }
};`},
	} {
		dir := filepath.Join(root, spec.id)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(spec.manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(spec.script), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	manager, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}

	var forwarded string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwarded = string(body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"id\":\"c1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{
		Models:     map[string]config.ModelConfig{"m": {}},
		Extensions: config.ExtensionsConfig{Enabled: true, Directory: root},
	}
	s.extensions = manager
	s.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(forwarded, "web_search") {
		t.Fatalf("the hook-injected hosted tool was stripped on the stream bridge: %s", forwarded)
	}
}

func TestServer_ExtensionInjectedHostedToolSurvivesStrip(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "websearch")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No manifest tools: the hosted tool is pushed by the request hook, exactly
	// like the openai-web-search preset.
	manifest := "id: websearch\nenabled: true\npriority: 10\nmatch:\n  endpoints: [chat.completions]\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  async onRequest(ctx, request) {
    request.tools = request.tools || [];
    request.tools.push({ type: "web_search" });
    return request;
  }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}

	var forwarded string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		forwarded = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{
		Models:     map[string]config.ModelConfig{"m": {}},
		Extensions: config.ExtensionsConfig{Enabled: true, Directory: root},
	}
	s.extensions = manager
	s.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(forwarded, "web_search") {
		t.Fatalf("the hook-injected hosted tool was stripped before forwarding: %s", forwarded)
	}
}
