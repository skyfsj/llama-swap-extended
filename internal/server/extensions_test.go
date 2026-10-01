package server

import (
	"context"
	"encoding/json"
	"fmt"
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

func TestMain(m *testing.M) {
	if len(os.Args) == 2 && os.Args[1] == "-extension-worker" {
		extensions.RunWorker(os.Stdin, os.Stdout)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func extensionTestServer(t *testing.T, local *stubRouter) *Server {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, "lookup")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	manifest := "id: lookup\nenabled: true\npriority: 10\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object", properties: { q: { type: "string" } } } } }],
  onRequest(ctx, request) { if (ctx.endpoint !== "anthropic.messages" && Array.isArray(request.messages)) request.messages.push({role:"system",content:"extension"}); return request; },
  onToolCall(ctx, call) { return { content: "found:" + call.arguments.q }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	_, ok := m.Compiled("lookup")
	if !ok {
		t.Fatal("extension did not load")
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	return s
}

func TestServer_ExtensionsToolConflict(t *testing.T) {
	tool := extensions.Tool{Type: "function", Execution: "server", Function: json.RawMessage(`{"name":"lookup","parameters":{"type":"object"}}`)}
	client := map[string]any{"type": "function", "function": map[string]any{"name": "lookup", "description": "client"}}
	for _, tc := range []struct {
		policy      string
		wantError   bool
		serverOwned bool
	}{{"skip", false, false}, {"override", false, true}, {"error", true, false}} {
		item := &extensions.Compiled{Manifest: extensions.Manifest{ID: "example", ToolConflict: tc.policy}, Tools: []extensions.Tool{tool}}
		request := map[string]any{"tools": []any{client}}
		owners := map[string]*extensions.Compiled{}
		err := injectExtensionTools(request, item, true, owners)
		if (err != nil) != tc.wantError || (owners["lookup"] != nil) != tc.serverOwned {
			t.Fatalf("policy %s: err=%v owners=%+v", tc.policy, err, owners)
		}
		if tc.policy == "skip" && toolName(request["tools"].([]any)[0], true) != "lookup" {
			t.Fatal("client tool was lost")
		}
		anthropic := map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": "client", "input_schema": map[string]any{"type": "object"}}}}
		anthropicOwners := map[string]*extensions.Compiled{}
		anthropicErr := injectAnthropicTools(anthropic, item, anthropicOwners)
		if (anthropicErr != nil) != tc.wantError || (anthropicOwners["lookup"] != nil) != tc.serverOwned {
			t.Fatalf("Anthropic policy %s: err=%v owners=%+v", tc.policy, anthropicErr, anthropicOwners)
		}
		if tc.policy == "skip" {
			clientTool := anthropic["tools"].([]any)[0].(map[string]any)
			if clientTool["description"] != "client" {
				t.Fatal("Anthropic client tool was overwritten")
			}
		}
	}
}

func TestServer_ExtensionsManagementFollowsDeploymentAuth(t *testing.T) {
	s := extensionTestServer(t, newStubRouter([]string{"m"}, ""))
	// A keyless deployment opens every management route, this one included:
	// requiring a key here alone would make it stricter than /api/config,
	// which can already rewrite the whole configuration.
	request := httptest.NewRequest(http.MethodGet, "/api/extensions", nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, request)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "lookup") {
		t.Fatalf("keyless management=%d %s", w.Code, w.Body.String())
	}
	// With a key configured, the config-admin scope on the route applies.
	s.cfg.RequiredAPIKeys = []string{"secret"}
	s.routes()
	request = httptest.NewRequest(http.MethodGet, "/api/extensions", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, request)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("keyed management without a credential status=%d", w.Code)
	}
	request = httptest.NewRequest(http.MethodGet, "/api/extensions", nil)
	request.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, request)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "lookup") {
		t.Fatalf("authenticated management=%d %s", w.Code, w.Body.String())
	}
	// Disabled extensions stay a 409 whichever way the deployment is keyed:
	// the manager is only installed when the feature is on.
	disabled := newTestServer(newStubRouter(nil, ""), newStubRouter([]string{"m"}, ""))
	disabled.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, RequiredAPIKeys: []string{"secret"}, Extensions: config.ExtensionsConfig{Enabled: false}}
	disabled.routes()
	request = httptest.NewRequest(http.MethodGet, "/api/extensions", nil)
	request.Header.Set("Authorization", "Bearer secret")
	w = httptest.NewRecorder()
	disabled.ServeHTTP(w, request)
	if w.Code != http.StatusConflict {
		t.Fatalf("disabled extensions status=%d", w.Code)
	}
}

func TestServer_ExtensionsAnthropicDryRunToolSchema(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) { t.Fatal("dry run dispatched to model") }
	s := extensionTestServer(t, local)
	s.cfg.RequiredAPIKeys = []string{"secret"}
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/api/extensions/lookup/test", strings.NewReader(`{"context":{"resolvedModel":"m","endpoint":"anthropic.messages"},"request":{"model":"m","messages":[{"role":"user","content":"hi"}]}}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Authorization", "Bearer secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"input_schema"`) {
		t.Fatalf("dry run=%d %s", w.Code, w.Body.String())
	}
}

func TestServer_ExtensionsStreamWithoutStreamHooksPassesThrough(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "pass")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: pass\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { onRequest(ctx, request) { return request; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	const upstream = "event: ping\ndata: {\"type\":\"ping\"}\n\n: keepalive\n\ndata: [DONE]\n\n"
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, upstream)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true,"max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || w.Body.String() != upstream {
		t.Fatalf("stream=%d %q", w.Code, w.Body.String())
	}
}

func TestServer_ExtensionsContinueOnRequestErrorDiscardsMutation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "recover")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: recover\nenabled: true\ncontinueOnError: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { onRequest(ctx, request) { request.messages.push({role:"system",content:"bad"}); throw new Error("failed"); } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), "bad") {
			t.Fatalf("failed mutation reached upstream: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") || m.List()[0].Status != "error" {
		t.Fatalf("response=%d body=%s status=%+v", w.Code, w.Body.String(), m.List())
	}
}

func TestServer_ExtensionsInterceptClientTool(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "intercept")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: intercept\nenabled: true\ninterceptClientTools: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { onToolCall(ctx, call) { if (call.name === "client_tool") return {handled:true,content:"intercepted"}; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !strings.Contains(string(body), "intercepted") {
			t.Fatalf("intercepted result missing: %s", body)
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), "done") {
		t.Fatalf("response=%d rounds=%d %s", w.Code, rounds, w.Body.String())
	}
}

func TestServer_ExtensionsMatchResolvedAliasProfileSelector(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"name":"lookup"`) {
			t.Fatalf("resolved extension tool missing: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	manifest := filepath.Join(s.extensions.Directory(), "lookup", "manifest.yaml")
	if err := os.WriteFile(manifest, []byte("id: lookup\nenabled: true\nmatch:\n  models: [m]\n  profiles: [coding]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.extensions.Reload(context.Background()); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfigFromReader(strings.NewReader("models:\n  m:\n    cmd: echo ${PORT}\n    aliases: [alias-m]\nselectors:\n  selected:\n    strategy: pin\n    targets: [alias-m]\nprofiles:\n  coding:\n    pins:\n      public: selected\n"))
	if err != nil {
		t.Fatal(err)
	}
	cfg.Extensions = config.ExtensionsConfig{Enabled: true, Directory: s.extensions.Directory()}
	s.cfg = cfg
	s.activeProfile = "coding"
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"public","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "ok") {
		t.Fatalf("response=%d %s", w.Code, w.Body.String())
	}
}

func TestServer_ExtensionsAnthropicToolInjectionAndLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"input_schema":{"properties"`) || !strings.Contains(string(body), `"name":"lookup"`) {
			t.Fatalf("Anthropic tool not injected: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"msg-1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"s1","name":"lookup","input":{"q":"x"}}],"stop_reason":"tool_use","usage":{"input_tokens":2,"output_tokens":3}}`)
			return
		}
		if !strings.Contains(string(body), `"tool_result"`) || !strings.Contains(string(body), `"tool_use_id":"s1"`) || !strings.Contains(string(body), "found:x") {
			t.Fatalf("Anthropic tool result missing: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"msg-2","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn","usage":{"input_tokens":4,"output_tokens":5}}`)
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), `"text":"done"`) || !strings.Contains(w.Body.String(), `"input_tokens":6`) {
		t.Fatalf("response=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
}

func TestServer_ExtensionsAnthropicMixedResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"msg-1","type":"message","role":"assistant","model":"m","content":[{"type":"tool_use","id":"s1","name":"lookup","input":{"q":"x"}},{"type":"tool_use","id":"c1","name":"client_tool","input":{}}],"stop_reason":"tool_use"}`)
			return
		}
		if !strings.Contains(string(body), "found:x") || !strings.Contains(string(body), "client-result") {
			t.Fatalf("Anthropic continuation missing results: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"msg-2","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	}
	s := extensionTestServer(t, local)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	first := post(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"client_tool","input_schema":{"type":"object"}}]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	calls := extensionCalls(payload, "anthropic.messages")
	if len(calls) != 1 || calls[0].Name != "client_tool" || !strings.HasPrefix(calls[0].ID, "call_ls_") {
		t.Fatalf("visible calls=%+v", calls)
	}
	second := post(fmt.Sprintf(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"client-result"}]}]}`, calls[0].ID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second=%d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
}

func TestServer_ExtensionsAnthropicStreamToolLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Searching \"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"s1\",\"name\":\"lookup\",\"input\":{}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"q\\\":\\\"x\\\"}\"}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "found:x") {
			t.Fatalf("missing Anthropic stream tool result: %s", body)
		}
		_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-2\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":4,\"output_tokens\":0}}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Found\"}}\n\n")
		_, _ = io.WriteString(w, "event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
		_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":5}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	content := w.Body.String()
	if w.Code != http.StatusOK || rounds != 2 || strings.Count(content, "event: message_start") != 1 || strings.Count(content, "event: message_stop") != 1 || strings.Contains(content, `"type":"tool_use"`) || !strings.Contains(content, "Searching ") || !strings.Contains(content, "Found") || !strings.Contains(content, `"input_tokens":6`) || !strings.Contains(content, `"output_tokens":8`) {
		t.Fatalf("Anthropic stream=%d rounds=%d %s", w.Code, rounds, content)
	}
}

func TestServer_ExtensionsAnthropicStreamMixedResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		if rounds == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":2,\"output_tokens\":0}}}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"tool_use\",\"id\":\"s1\",\"name\":\"lookup\",\"input\":{\"q\":\"x\"}}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n")
			_, _ = io.WriteString(w, "event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"c1\",\"name\":\"client_tool\",\"input\":{}}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":1}\n\n")
			_, _ = io.WriteString(w, "event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":3}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "found:x") || !strings.Contains(string(body), "client-result") {
			t.Fatalf("stream mixed continuation missing results: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg-2","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"done"}],"stop_reason":"end_turn"}`)
	}
	s := extensionTestServer(t, local)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	first := post(`{"model":"m","stream":true,"max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"client_tool","input_schema":{"type":"object"}}]}`)
	content := first.Body.String()
	if first.Code != http.StatusOK || strings.Count(content, "event: message_stop") != 1 || strings.Contains(content, `"name":"lookup"`) || !strings.Contains(content, `"name":"client_tool"`) {
		t.Fatalf("first=%d %s", first.Code, content)
	}
	var publicID string
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		block, _ := event["content_block"].(map[string]any)
		if block["name"] == "client_tool" {
			publicID, _ = block["id"].(string)
		}
	}
	if !strings.HasPrefix(publicID, "call_ls_") {
		t.Fatalf("public id = %q", publicID)
	}
	second := post(fmt.Sprintf(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"client-result"}]}]}`, publicID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second=%d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
}

func TestServer_ExtensionsChatToolLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), `"name":"lookup"`) {
			t.Fatalf("round %d missing injected tool: %s", rounds, body)
		}
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			if !strings.Contains(string(body), "extension") {
				t.Fatalf("onRequest did not run: %s", body)
			}
			_, _ = io.WriteString(w, `{"id":"a","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3}}`)
			return
		}
		if !strings.Contains(string(body), "found:x") {
			t.Fatalf("tool result missing: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":5}}`)
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), `"content":"done"`) {
		t.Fatalf("response = %d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"prompt_tokens":6`) || !strings.Contains(w.Body.String(), `"completion_tokens":8`) {
		t.Fatalf("usage was not accumulated: %s", w.Body.String())
	}
	page, err := s.store.ListActivity(context.Background(), store.ActivityQuery{Limit: 10, Page: 1})
	if err != nil || len(page.Data) != 1 {
		t.Fatalf("activity rows = %d, err=%v", len(page.Data), err)
	}
}

func TestServer_ExtensionsMixedToolsResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"a","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !strings.Contains(string(body), "found:x") || !strings.Contains(string(body), "client-result") {
			t.Fatalf("continuation missing results: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	first := post(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first response = %d %s", first.Code, first.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	calls := extensionCalls(payload, "chat.completions")
	if len(calls) != 1 || calls[0].Name != "client_tool" || !strings.HasPrefix(calls[0].ID, "call_ls_") {
		t.Fatalf("visible calls = %+v", calls)
	}
	second := post(fmt.Sprintf(`{"model":"m","messages":[{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, calls[0].ID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second response = %d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
}

func TestServer_ExtensionsResponsesAdapterToolLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("backend path = %s", r.URL.Path)
		}
		if !strings.Contains(string(body), `"name":"lookup"`) {
			t.Fatalf("missing tool in chat dialect: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"a","model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !strings.Contains(string(body), "found:x") || !strings.Contains(string(body), "hi") {
			t.Fatalf("responses continuation missing history: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"b","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	model := s.cfg.Models["m"]
	model.Backend = config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}
	s.cfg.Models["m"] = model
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), `"text":"done"`) {
		t.Fatalf("response = %d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
}

func TestServer_ExtensionsNativeResponsesToolLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/responses" {
			t.Fatalf("native path = %s", r.URL.Path)
		}
		if !strings.Contains(string(body), `"name":"lookup"`) {
			t.Fatalf("missing native tool: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"resp-a","model":"m","output":[{"id":"fc-s1","type":"function_call","call_id":"s1","name":"lookup","arguments":"{\"q\":\"x\"}"}],"usage":{"input_tokens":2,"output_tokens":3}}`)
			return
		}
		if !strings.Contains(string(body), "found:x") || !strings.Contains(string(body), "hi") {
			t.Fatalf("native continuation missing history: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"resp-b","model":"m","output":[{"id":"msg-1","type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":4,"output_tokens":5}}`)
	}
	s := extensionTestServer(t, local)
	model := s.cfg.Models["m"]
	model.Backend = config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}
	s.cfg.Models["m"] = model
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), `"text":"done"`) || !strings.Contains(w.Body.String(), `"input_tokens":6`) {
		t.Fatalf("response = %d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
}

func TestServer_ExtensionsNativeResponsesMixedResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"resp-a","model":"m","output":[{"type":"function_call","call_id":"s1","name":"lookup","arguments":"{\"q\":\"x\"}"},{"type":"function_call","call_id":"c1","name":"client_tool","arguments":"{}"}]}`)
			return
		}
		if !strings.Contains(string(body), "found:x") || !strings.Contains(string(body), "client-result") {
			t.Fatalf("native mixed continuation missing results: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"resp-b","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}
	s := extensionTestServer(t, local)
	model := s.cfg.Models["m"]
	model.Backend = config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}
	s.cfg.Models["m"] = model
	s.routes()
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	first := post(`{"model":"m","input":"hi","tools":[{"type":"function","name":"client_tool","parameters":{"type":"object"}}]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first = %d %s", first.Code, first.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(first.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	calls := extensionCalls(payload, "responses")
	if len(calls) != 1 || calls[0].Name != "client_tool" || !strings.HasPrefix(calls[0].ID, "call_ls_") {
		t.Fatalf("visible calls = %+v", calls)
	}
	second := post(fmt.Sprintf(`{"model":"m","input":[{"type":"function_call_output","call_id":%q,"output":"client-result"}]}`, calls[0].ID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second = %d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
}

func TestServer_ExtensionsChatStreamToolLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Searching \"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "found:x") {
			t.Fatalf("missing stream tool result: %s", body)
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Found\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	content := w.Body.String()
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(content, "Searching ") || !strings.Contains(content, "Found") || strings.Contains(content, "lookup") || strings.Count(content, "[DONE]") != 1 {
		t.Fatalf("stream = %d rounds=%d %s", w.Code, rounds, content)
	}
}

// Regression: backends with enable_force_include_usage (production vLLM) attach
// the cumulative usage to every frame. The stream bridge used to drop any frame
// carrying usage, erasing the whole reply whenever a server-tool extension was
// active. Content frames with usage must stream through, and the usage
// high-water mark must not double-count.
func TestServer_ExtensionsChatStreamUsageEveryFrame(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Searching \"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":2,\"total_tokens\":12}}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Found\"},\"finish_reason\":null}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":3,\"total_tokens\":13}}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"model\":\"m\",\"created\":1,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\ndata: [DONE]\n\n")
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	content := w.Body.String()
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(content, "Searching ") || !strings.Contains(content, "Found") || strings.Count(content, "[DONE]") != 1 {
		t.Fatalf("stream = %d rounds=%d %s", w.Code, rounds, content)
	}
	// The final usage chunk must report the last cumulative values once, not a
	// sum of every per-frame usage (which would inflate completion_tokens).
	if !strings.Contains(content, "\"completion_tokens\":4") || strings.Contains(content, "\"completion_tokens\":8") {
		t.Fatalf("usage not high-water marked: %s", content)
	}
}

func TestServer_ExtensionsResponsesStreamToolLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Searching \"},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		body, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(body), "found:x") {
			t.Fatalf("missing responses stream tool result: %s", body)
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Found\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"b\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	s := extensionTestServer(t, local)
	model := s.cfg.Models["m"]
	model.Backend = config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}
	s.cfg.Models["m"] = model
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","stream":true,"input":"hi"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	content := w.Body.String()
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(content, "Searching ") || !strings.Contains(content, "Found") || !strings.Contains(content, `"text":"Searching Found"`) || strings.Contains(content, `"type":"function_call"`) || strings.Count(content, "event: response.completed") != 1 {
		t.Fatalf("stream = %d rounds=%d %s", w.Code, rounds, content)
	}
}
