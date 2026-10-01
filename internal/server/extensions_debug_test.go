package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// debugChatTestServer wires two extensions: "debugext" is disabled (the debug
// chat must still run it) and "noisy" is enabled and would match every model,
// proving the debug request runs the target extension in isolation.
func debugChatTestServer(t *testing.T, upstreamBody *string) *Server {
	t.Helper()
	root := t.TempDir()
	writeExtension := func(id, manifest, script string) {
		t.Helper()
		dir := filepath.Join(root, id)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeExtension("debugext", "id: debugext\nenabled: false\npriority: 10\n",
		`export default {
  tools: [{ type: "function", execution: "server", function: { name: "sample", description: "sample tool", parameters: { type: "object", properties: {} } } }],
  async onRequest(ctx, request) { ctx.log.info("debug-ext-ran"); request.temperature = 0.1; return request; },
  onToolCall(ctx, call) { return { content: "tool:" + call.name }; }
};`)
	writeExtension("noisy", "id: noisy\nenabled: true\npriority: 20\n",
		`export default {
  async onRequest(ctx, request) { if (Array.isArray(request.messages)) request.messages.push({role:"system", content:"noisy-ran"}); return request; }
};`)
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Compiled("debugext"); !ok {
		t.Fatal("debugext did not load")
	}
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"total_tokens":3}}`))
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	return s
}

func TestProxy_ExtensionDebugChatRunsTargetExtensionOnly(t *testing.T) {
	var upstreamBody string
	s := debugChatTestServer(t, &upstreamBody)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/debugext/debug/chat",
		strings.NewReader(`{"request":{"model":"m","stream":false,"messages":[{"role":"user","content":"hello"}]}}`))
	s.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("debug chat status = %d, body %s", recorder.Code, recorder.Body.String())
	}
	debugID := recorder.Header().Get("X-Extension-Debug-ID")
	if debugID == "" {
		t.Fatal("debug chat did not return X-Extension-Debug-ID")
	}
	if !strings.Contains(recorder.Body.String(), `"ok"`) {
		t.Fatalf("upstream response missing content: %s", recorder.Body.String())
	}

	var received map[string]any
	if err := json.Unmarshal([]byte(upstreamBody), &received); err != nil {
		t.Fatalf("upstream body invalid JSON: %s", upstreamBody)
	}
	if temperature, _ := received["temperature"].(float64); temperature != 0.1 {
		t.Fatalf("disabled debug extension did not run, upstream body: %s", upstreamBody)
	}
	if !strings.Contains(upstreamBody, `"sample"`) {
		t.Fatalf("debug extension tool was not injected: %s", upstreamBody)
	}
	if strings.Contains(upstreamBody, "noisy-ran") {
		t.Fatalf("unrelated enabled extension ran in a debug chat: %s", upstreamBody)
	}

	artifactRecorder := httptest.NewRecorder()
	artifactRequest := httptest.NewRequest(http.MethodGet, "/api/extensions/debugext/debug/artifact?request="+debugID, nil)
	s.ServeHTTP(artifactRecorder, artifactRequest)
	if artifactRecorder.Code != http.StatusOK {
		t.Fatalf("artifact status = %d, body %s", artifactRecorder.Code, artifactRecorder.Body.String())
	}
	var artifact struct {
		Steps     []map[string]any `json:"steps"`
		Truncated bool             `json:"truncated"`
		Done      bool             `json:"done"`
	}
	if err := json.Unmarshal(artifactRecorder.Body.Bytes(), &artifact); err != nil {
		t.Fatalf("artifact body invalid: %s", artifactRecorder.Body.String())
	}
	if !artifact.Done {
		t.Fatal("artifact capture not marked done")
	}
	var sawHook, sawTools bool
	for _, step := range artifact.Steps {
		switch step["kind"] {
		case "hook":
			if step["hook"] != "onRequest" {
				continue
			}
			sawHook = true
			logs, _ := step["logs"].([]any)
			if len(logs) == 0 || !strings.Contains(logs[0].(string), "debug-ext-ran") {
				t.Fatalf("onRequest step missing logs: %v", step)
			}
			changed, _ := step["changed"].([]any)
			if len(changed) == 0 {
				t.Fatalf("onRequest step missing changed keys: %v", step)
			}
		case "tools":
			sawTools = true
			injected, _ := step["injected"].([]any)
			if len(injected) != 1 || injected[0] != "sample" {
				t.Fatalf("tools step wrong injected list: %v", step)
			}
		}
	}
	if !sawHook || !sawTools {
		t.Fatalf("artifact steps missing hook or tools record: %v", artifact.Steps)
	}
}

func TestProxy_ExtensionDebugChatArtifactExpired(t *testing.T) {
	var upstreamBody string
	s := debugChatTestServer(t, &upstreamBody)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/api/extensions/debugext/debug/artifact?request=missing", nil)
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("unknown debug request status = %d", recorder.Code)
	}
}

// debugKVTestServer wires one extension that reads and writes ctx.kv, so the
// host-call round trip, storage permissions and scope handling are covered end
// to end through the inference chain.
func kvTestServerWithCapture(t *testing.T, script string, upstreamBody *string) *Server {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "kvy")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: kvy\nenabled: true\npermissions:\n  storage: persistent\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	return kvTestServerWithCaptureFor(t, root, "kvy", upstreamBody)
}

func kvTestServerWithCaptureFor(t *testing.T, root, id string, upstreamBody *string) *Server {
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*upstreamBody = string(body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.cfg.UI.Activity.SessionID = []string{"X-Session-ID"}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	return s
}

func TestProxy_ExtensionHostKVStorage(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default {
  async onRequest(ctx, request) {
    const before = await ctx.kv.get("hits", { scope: "global" });
    const previous = typeof before === "number" ? before : 0;
    await ctx.kv.set("hits", previous + 1, { scope: "global" });
    request.messages.push({ role: "system", content: "hits:" + (previous + 1) });
    return request;
  }
};`, &upstreamBody)
	post := func() string {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		request.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("chat status = %d body %s", recorder.Code, recorder.Body.String())
		}
		var upstream map[string]any
		if err := json.Unmarshal([]byte(upstreamBody), &upstream); err != nil {
			t.Fatalf("upstream body: %s", upstreamBody)
		}
		messages, _ := upstream["messages"].([]any)
		last := messages[len(messages)-1].(map[string]any)
		return last["content"].(string)
	}
	if first := post(); first != "hits:1" {
		t.Fatalf("first turn = %q", first)
	}
	if second := post(); second != "hits:2" {
		t.Fatalf("second turn = %q", second)
	}
	if count, err := s.store.CountExtensionKVs(context.Background(), "kvy"); err != nil || count != 1 {
		t.Fatalf("persisted rows = %d err=%v", count, err)
	}
}

func TestProxy_ExtensionHostKVScopeIsolation(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default {
  async onRequest(ctx, request) {
    const previous = await ctx.kv.get("v", { scope: "session" });
    await ctx.kv.set("v", (typeof previous === "number" ? previous : 0) + 1, { scope: "session" });
    request.messages.push({ role: "system", content: "v:" + await ctx.kv.get("v", { scope: "session" }) });
    return request;
  }
};`, &upstreamBody)
	run := func(session string) string {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("X-Session-ID", session)
		s.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("chat status = %d body %s", recorder.Code, recorder.Body.String())
		}
		var upstream map[string]any
		_ = json.Unmarshal([]byte(upstreamBody), &upstream)
		messages, _ := upstream["messages"].([]any)
		last := messages[len(messages)-1].(map[string]any)
		return last["content"].(string)
	}
	if a := run("session-a"); a != "v:1" {
		t.Fatalf("session-a first = %q", a)
	}
	if b := run("session-a"); b != "v:2" {
		t.Fatalf("session-a second = %q", b)
	}
	if c := run("session-b"); c != "v:1" {
		t.Fatalf("session-b isolated = %q", c)
	}
	if count, err := s.store.CountExtensionKVs(context.Background(), "kvy"); err != nil || count != 2 {
		t.Fatalf("scope rows = %d err=%v", count, err)
	}
}

func TestProxy_ExtensionHostEphemeralKV(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "kve")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// No permissions.storage: the default is ephemeral in-memory KV.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: kve\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	source := `export default {
  async onRequest(ctx, request) {
    const previous = await ctx.kv.get("hits");
    await ctx.kv.set("hits", (typeof previous === "number" ? previous : 0) + 1);
    request.messages.push({ role: "system", content: "hits:" + await ctx.kv.get("hits") });
    return request;
  }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	upstreamBody := ""
	s := kvTestServerWithCaptureFor(t, root, "kve", &upstreamBody)
	post := func() string {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
		request.Header.Set("Content-Type", "application/json")
		s.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("chat status = %d body %s", recorder.Code, recorder.Body.String())
		}
		var upstream map[string]any
		_ = json.Unmarshal([]byte(upstreamBody), &upstream)
		messages, _ := upstream["messages"].([]any)
		last := messages[len(messages)-1].(map[string]any)
		return last["content"].(string)
	}
	if first := post(); first != "hits:1" {
		t.Fatalf("first turn = %q", first)
	}
	if second := post(); second != "hits:2" {
		t.Fatalf("second turn = %q", second)
	}
	// The ephemeral store lives in-process and never touches the database.
	if count, err := s.store.CountExtensionKVs(context.Background(), "kve"); err != nil || count != 0 {
		t.Fatalf("ephemeral store wrote to the database: %d err=%v", count, err)
	}
}

func TestProxy_ExtensionForwardSkipsExtensions(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default {
  async onRequest(ctx, request) {
    const reply = await ctx.forward("m", { messages: [{ role: "user", content: "inner" }] });
    request.messages.push({ role: "system", content: "inner:" + reply.status + ":" + reply.body.choices[0].message.content });
    return request;
  }
};`, &upstreamBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("chat status = %d body %s", recorder.Code, recorder.Body.String())
	}
	// The outer hook ran once and observed the forward's buffered reply. The
	// inner request skipped extensions, so this assertion only passes if the
	// chain did not re-enter the hook (otherwise it would recurse forever).
	var upstream map[string]any
	_ = json.Unmarshal([]byte(upstreamBody), &upstream)
	messages, _ := upstream["messages"].([]any)
	if len(messages) != 1 {
		t.Fatalf("hook marked the forwarded request: %s", upstreamBody)
	}
	last := messages[0].(map[string]any)
	if last["content"] != "inner:200:ok" {
		t.Fatalf("forward reply = %v", last["content"])
	}
}

func TestProxy_ExtensionForwardDepthBudget(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default { async onRequest(ctx, request) { return request; } };`, &upstreamBody)
	s.cfg.Extensions.MaxForwardDepth = 1
	host := newExtensionHost(s, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(context.WithValue(httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).Context(), extensionForwardDepthKey{}, 1)))
	_, err := host.execForward(context.Background(), json.RawMessage(`{"model":"m","request":{"messages":[]}}`))
	if err == nil || !strings.Contains(err.Error(), "forward depth limit 1 reached") {
		t.Fatalf("depth budget error = %v", err)
	}
}

func TestProxy_ExtensionSessionUsageReported(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default {
  async onRequest(ctx, request) {
    const usage = await ctx.usage();
    request.messages.push({ role: "system", content: "usage:" + usage.requests + ":" + usage.inputTokens });
    return request;
  }
};`, &upstreamBody)
	if _, err := s.store.InsertActivity(context.Background(), store.ActivityLogEntry{
		Model: "m", KeyID: "key-9", SessionID: "lspg-x", ReqPath: "/v1/chat/completions",
		Tokens: TokenMetrics{InputTokens: 120, OutputTokens: 30},
	}); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Session-ID", "lspg-x")
	s.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("chat status = %d body %s", recorder.Code, recorder.Body.String())
	}
	var upstream map[string]any
	_ = json.Unmarshal([]byte(upstreamBody), &upstream)
	messages, _ := upstream["messages"].([]any)
	last := messages[len(messages)-1].(map[string]any)
	if last["content"] != "usage:1:120" {
		t.Fatalf("usage report = %v (metadata did not carry the key id is the usual cause)", last["content"])
	}
}

func TestProxy_ExtensionHostKVDisabled(t *testing.T) {
	var upstreamBody string
	s := kvTestServerWithCapture(t, `export default {
  async onRequest(ctx, request) {
    await ctx.kv.set("x", 1, { scope: "global" });
    return request;
  }
};`, &upstreamBody)
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/extensions/kvy/test", strings.NewReader(`{"context":{"endpoint":"chat.completions","resolvedModel":"m"},"request":{"model":"m","messages":[]}}`))
	s.ServeHTTP(recorder, request)
	if !strings.Contains(recorder.Body.String(), "storage is disabled in extension test mode") {
		t.Fatalf("dry-run rejection missing: %s", recorder.Body.String())
	}
}
