package server

import (
	"bytes"
	"compress/gzip"
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
	"time"

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
		err := injectExtensionTools(request, item, true, owners, nil)
		if (err != nil) != tc.wantError || (owners["lookup"] != nil) != tc.serverOwned {
			t.Fatalf("policy %s: err=%v owners=%+v", tc.policy, err, owners)
		}
		if tc.policy == "skip" && toolName(request["tools"].([]any)[0], true) != "lookup" {
			t.Fatal("client tool was lost")
		}
		anthropic := map[string]any{"tools": []any{map[string]any{"name": "lookup", "description": "client", "input_schema": map[string]any{"type": "object"}}}}
		anthropicOwners := map[string]*extensions.Compiled{}
		anthropicErr := injectAnthropicTools(anthropic, item, anthropicOwners, nil)
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
	const upstream = "data: {\"id\":\"chatcmpl_1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\n" +
		"data: {\"id\":\"chatcmpl_1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
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
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "event: message_start") || !strings.Contains(w.Body.String(), `"text":"hello"`) || !strings.Contains(w.Body.String(), "event: message_stop") {
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
		if !strings.Contains(string(body), `"type":"function"`) || !strings.Contains(string(body), `"function":{"name":"lookup"`) {
			t.Fatalf("Chat tool not injected: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"checking","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
			return
		}
		if !strings.Contains(string(body), `"tool_call_id":"s1"`) || !strings.Contains(string(body), "found:x") {
			t.Fatalf("Chat tool result missing: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}`)
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

// On the Anthropic surface a server-side call travels back to the caller as a
// server_tool_use block paired with an mcp_tool_result block, the caller replays
// the whole turn, and the daemon keeps no record of the round: the transcript
// carries the work. (Claude Code 2.1.278 accepts, echoes and does not answer
// that pair - see extensions_artifacts.go.)
func TestServer_ExtensionsAnthropicMixedResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var secondBody map[string]any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"checking","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_ = json.Unmarshal(body, &secondBody)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
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
	if strings.Contains(first.Body.String(), "call_ls_") {
		t.Fatalf("a visible surface must not rewrite caller ids: %s", first.Body.String())
	}
	content, _ := mustJSONObject(t, first.Body.Bytes())["content"].([]any)
	var artifactID, clientCallID string
	seen := map[string]bool{}
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		kind, _ := block["type"].(string)
		seen[kind] = true
		switch kind {
		case "server_tool_use":
			if block["name"] != "lookup" {
				t.Fatalf("server_tool_use name = %v", block["name"])
			}
			artifactID, _ = block["id"].(string)
			if !strings.HasPrefix(artifactID, "srvtoolu_") {
				t.Fatalf("server_tool_use id = %q", artifactID)
			}
		case "mcp_tool_result":
			if block["tool_use_id"] != artifactID {
				t.Fatalf("mcp_tool_result does not pair with the call: %v", block)
			}
			if !strings.Contains(fmt.Sprintf("%v", block["content"]), "found:x") {
				t.Fatalf("mcp_tool_result content = %v", block["content"])
			}
		case "tool_use":
			clientCallID, _ = block["id"].(string)
		}
	}
	if !seen["server_tool_use"] || !seen["mcp_tool_result"] || !seen["tool_use"] {
		t.Fatalf("the turn is missing artifacts or the caller's own call: %v", seen)
	}
	if clientCallID != "c1" {
		t.Fatalf("the caller's call was rewritten: %q", clientCallID)
	}
	// The caller replays the whole turn: the artifacts inside the assistant turn
	// and its own tool result in the next user turn.
	second := post(fmt.Sprintf(`{"model":"m","max_tokens":100,"messages":[
		{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"server_tool_use","id":%q,"name":"lookup","input":{"q":"x"}},{"type":"mcp_tool_result","tool_use_id":%q,"content":[{"type":"text","text":"found:x"}]},{"type":"tool_use","id":%q,"name":"client_tool","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"client-result"}]}]}`,
		artifactID, artifactID, clientCallID, clientCallID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second=%d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
	// The upstream (Chat) sees the artifact folded into an ordinary call/result
	// pair, next to the caller's own result.
	upstream := fmt.Sprintf("%v", secondBody["messages"])
	for _, want := range []string{"found:x", "client-result", "srvtoolu_", "lookup"} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("upstream conversation is missing %q: %s", want, upstream)
		}
	}
}

// A round that runs a server tool and needs no caller action still reports the
// work: the caller's answer carries the artifacts of every round plus the model's
// final text, so a later turn replays what actually happened.
func TestServer_ExtensionsAnthropicServerToolStaysVisible(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"looking","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), "done") {
		t.Fatalf("status=%d rounds=%d %s", w.Code, rounds, w.Body.String())
	}
	content, _ := mustJSONObject(t, w.Body.Bytes())["content"].([]any)
	var kinds []string
	for _, raw := range content {
		block, _ := raw.(map[string]any)
		kind, _ := block["type"].(string)
		kinds = append(kinds, kind)
	}
	joined := strings.Join(kinds, ",")
	if !strings.Contains(joined, "server_tool_use") || !strings.Contains(joined, "mcp_tool_result") || !strings.Contains(joined, "text") {
		t.Fatalf("the turn did not report its server work: %v", kinds)
	}
	if !strings.Contains(w.Body.String(), "found:x") {
		t.Fatalf("the result artifact is missing: %s", w.Body.String())
	}
}

// A caller that strips the artifacts (a proxy or SDK that normalizes content)
// loses the server work from its transcript, but the conversation stays
// coherent: the daemon runs what it is given and does not invent a result for a
// call that is no longer there.
func TestServer_ExtensionsAnthropicStrippedArtifactsStayConsistent(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var secondBody map[string]any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-1","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_ = json.Unmarshal(body, &secondBody)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if first := post(`{"model":"m","max_tokens":100,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"client_tool","input_schema":{"type":"object"}}]}`); first.Code != http.StatusOK {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	// The artifacts were dropped on the way back; only the caller's own call and
	// its result survive.
	second := post(`{"model":"m","max_tokens":100,"messages":[
		{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"client_tool","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"client-result"}]}]}`)
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second=%d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
	if upstream := fmt.Sprintf("%v", secondBody["messages"]); strings.Contains(upstream, "found:x") {
		t.Fatalf("a result appeared for a call the caller no longer carried: %s", upstream)
	}
}

func TestServer_ExtensionsAnthropicStreamToolLoop(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			if !strings.Contains(string(body), `"type":"function"`) || !strings.Contains(string(body), `"name":"lookup"`) {
				t.Fatalf("Chat extension tool missing: %s", body)
			}
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Searching \"}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"x\\\"}\"}}]}}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":3,\"total_tokens\":5}}\n\ndata: [DONE]\n\n")
			return
		}
		if !strings.Contains(string(body), "found:x") {
			t.Fatalf("missing Chat stream tool result: %s", body)
		}
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-2\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Found\"}}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":2,\"total_tokens\":6}}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-2\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":4,\"completion_tokens\":5,\"total_tokens\":9}}\n\ndata: [DONE]\n\n")
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

// The streaming Anthropic surface reports server-side work as blocks: the
// caller receives a server_tool_use block (arguments as input_json_delta frames,
// the only shape a client keeps), its result block, and its own tool_use block.
// It replays the turn and nothing is stored daemon-side.
func TestServer_ExtensionsAnthropicStreamMixedResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		if rounds == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}},{\"index\":1,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"client_tool\",\"arguments\":\"{}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-2","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
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
	if first.Code != http.StatusOK || strings.Count(content, "event: message_stop") != 1 {
		t.Fatalf("first=%d %s", first.Code, content)
	}
	// The server call is visible, with its arguments on the wire as a delta.
	if !strings.Contains(content, `"type":"server_tool_use"`) || !strings.Contains(content, `"type":"input_json_delta"`) || !strings.Contains(content, `\"q\":\"x\"`) {
		t.Fatalf("the server call is not visible in the stream: %s", content)
	}
	if !strings.Contains(content, `"type":"mcp_tool_result"`) || !strings.Contains(content, "found:x") {
		t.Fatalf("the server result is not visible in the stream: %s", content)
	}
	if strings.Contains(content, "call_ls_") {
		t.Fatalf("a visible surface must not rewrite caller ids: %s", content)
	}
	var artifactID, clientCallID string
	for _, line := range strings.Split(content, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		block, _ := event["content_block"].(map[string]any)
		switch block["type"] {
		case "server_tool_use":
			artifactID, _ = block["id"].(string)
		case "tool_use":
			clientCallID, _ = block["id"].(string)
		}
	}
	if !strings.HasPrefix(artifactID, "srvtoolu_") || clientCallID != "c1" {
		t.Fatalf("ids: artifact=%q client=%q", artifactID, clientCallID)
	}
	second := post(fmt.Sprintf(`{"model":"m","stream":true,"max_tokens":100,"messages":[
		{"role":"assistant","content":[{"type":"server_tool_use","id":%q,"name":"lookup","input":{"q":"x"}},{"type":"mcp_tool_result","tool_use_id":%q,"content":[{"type":"text","text":"found:x"}]},{"type":"tool_use","id":%q,"name":"client_tool","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":%q,"content":"client-result"}]}]}`,
		artifactID, artifactID, clientCallID, clientCallID))
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

// A Chat Completions mixed round hides the extension's own call and records the
// round: the caller gets its own call under a public id, the server call is
// written down unexecuted, and the conversation has to come back before it runs.
func TestServer_ExtensionsMixedToolsResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var secondBody map[string]any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"a","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_ = json.Unmarshal(body, &secondBody)
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
	payload := mustJSONObject(t, first.Body.Bytes())
	calls := extensionCalls(payload, "chat.completions")
	if len(calls) != 1 || calls[0].Name != "client_tool" || !strings.HasPrefix(calls[0].ID, "call_ls_") {
		t.Fatalf("visible calls = %+v", calls)
	}
	// The round is recorded, and the extension's own call is recorded as work to
	// do - not as an executed result.
	record, err := s.store.LookupExtensionPending(context.Background(), calls[0].ID)
	if err != nil {
		t.Fatalf("pending record: %v", err)
	}
	var payloadRecord extensionPendingPayload
	if err := json.Unmarshal(record.Payload, &payloadRecord); err != nil {
		t.Fatal(err)
	}
	kinds := map[string]string{}
	for _, entry := range payloadRecord.Calls {
		kinds[entry.Name] = entry.Kind
	}
	if kinds["lookup"] != pendingCallServer || kinds["client_tool"] != pendingCallClient {
		t.Fatalf("recorded round = %+v", payloadRecord.Calls)
	}
	// A bare continuation cannot be completed on this surface.
	bare := post(fmt.Sprintf(`{"model":"m","messages":[{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, calls[0].ID))
	if bare.Code != http.StatusBadRequest || !strings.Contains(bare.Body.String(), "replayed") {
		t.Fatalf("bare continuation = %d %s", bare.Code, bare.Body.String())
	}
	// The caller replays the turn it was given, then answers with its own result.
	second := post(fmt.Sprintf(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, calls[0].ID, calls[0].ID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second response = %d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
	// Upstream sees the whole call set under the model's own ids, in order, with
	// both results.
	encoded, _ := json.Marshal(secondBody["messages"])
	upstream := string(encoded)
	for _, want := range []string{"found:x", "client-result", `"id":"s1"`, `"id":"c1"`} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("upstream conversation is missing %q: %s", want, upstream)
		}
	}
	if strings.Contains(upstream, "call_ls_") {
		t.Fatalf("the public id leaked upstream: %s", upstream)
	}
}

// A continuation record holds the whole conversation, so a long session used to
// fail the turn with 502 "extension continuation exceeds 2 MiB" as soon as one
// round mixed an extension's server tool with a caller-run tool. The caller
// cannot be handed a call it has no way to resume, so the round degrades to a
// failed caller tool and the turn still ends with the model's answer.
func TestServer_ExtensionsOversizedMixedContinuationDegrades(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var secondBody map[string]any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			// The extension's own call alone carries more than the record budget: the
			// round cannot be written down, so the caller's call is degraded instead.
			huge := strings.Repeat("x", 1<<20)
			calls := []any{
				map[string]any{"id": "s1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": string(mustJSON(t, map[string]any{"q": huge}))}},
				map[string]any{"id": "c1", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
			}
			body := map[string]any{"id": "a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "tool_calls": calls}, "finish_reason": "tool_calls"}}}
			_, _ = w.Write(mustJSON(t, body))
			return
		}
		_ = json.Unmarshal(raw, &secondBody)
		_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"degraded"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	s.cfg.Extensions.MaxContinuationMiB = 1
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("an oversized record failed the turn: status=%d body=%.300s", w.Code, w.Body.String())
	}
	if rounds != 2 || !strings.Contains(w.Body.String(), "degraded") {
		t.Fatalf("the turn did not end with the model's answer: rounds=%d body=%.300s", rounds, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "client_tool") || strings.Contains(w.Body.String(), "call_ls_") {
		t.Fatalf("a call the caller cannot continue reached it: %s", w.Body.String())
	}
	// The degraded call is reported to the model, and the extension's own call still
	// ran, so the answer is grounded in the work that could be done.
	upstream := fmt.Sprintf("%v", secondBody["messages"])
	if !strings.Contains(upstream, "found:x") || !strings.Contains(upstream, "failed and is unavailable") {
		t.Fatalf("degraded round request = %.300s", upstream)
	}
}

// The stream bridge degrades the same capacity limit without an error frame:
// nothing of the round's caller-run call has reached the caller yet - the bridge
// holds call fragments until the round is classified - so the stream ends with
// the model's answer.
func TestServer_ExtensionsOversizedStreamMixedContinuationDegrades(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			huge := strings.Repeat("x", 1<<20)
			fragments := []any{
				map[string]any{"index": 0, "id": "s1", "type": "function", "function": map[string]any{"name": "lookup", "arguments": string(mustJSON(t, map[string]any{"q": huge}))}},
				map[string]any{"index": 1, "id": "c1", "type": "function", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
			}
			_ = fragments
			frame := map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": fragments}}}}
			_, _ = w.Write(append(append([]byte("data: "), mustJSON(t, frame)...), []byte("\n\n")...))
			_, _ = w.Write(append(append([]byte("data: "), mustJSON(t, map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})...), []byte("\n\ndata: [DONE]\n\n")...))
			return
		}
		raw, _ := io.ReadAll(r.Body)
		if !strings.Contains(string(raw), "found:x") || !strings.Contains(string(raw), "failed and is unavailable") {
			t.Fatalf("degraded continuation missing results: %.300s", raw)
		}
		_, _ = w.Write(append(append([]byte("data: "), mustJSON(t, map[string]any{"id": "b", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "degraded"}}}})...), []byte("\n\n")...))
		_, _ = w.Write(append(append([]byte("data: "), mustJSON(t, map[string]any{"id": "b", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}})...), []byte("\n\ndata: [DONE]\n\n")...))
	}
	s := extensionTestServer(t, local)
	s.cfg.Extensions.MaxContinuationMiB = 1
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	content := w.Body.String()
	if w.Code != http.StatusOK || rounds != 2 {
		t.Fatalf("status=%d rounds=%d %.300s", w.Code, rounds, content)
	}
	if strings.Contains(content, "client_tool") || strings.Contains(content, "call_ls_") {
		t.Fatalf("a call the caller cannot continue reached it: %.300s", content)
	}
	if !strings.Contains(content, "degraded") || strings.Count(content, "[DONE]") != 1 || !strings.Contains(content, `"finish_reason":"stop"`) {
		t.Fatalf("the stream did not end with the model's answer: %.300s", content)
	}
}

// The round limit is the other way into the mixed path: the last round withholds
// the extensions' own server tools, but an interceptor can still turn a
// caller-run call into a server call. When the continuation then cannot be
// stored, the remaining caller-run call must be dropped from the answer - from
// the body and from the stream - because it can never be resumed. The same
// fixture with the default ceiling hands the call over, which is what pins the
// limit as the only difference.
func TestServer_ExtensionsOversizedContinuationOnLastRoundDegrades(t *testing.T) {
	// run serves one last-round mixed request; limitMiB 0 keeps the default, and a
	// non-zero limit comes with an interceptor result larger than the whole budget.
	run := func(t *testing.T, stream bool, limitMiB int) (int, string) {
		t.Helper()
		root := t.TempDir()
		dir := filepath.Join(root, "intercept")
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: intercept\nenabled: true\ninterceptClientTools: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		source := `export default { onToolCall(ctx, call) { if (call.name === "intercepted_tool") return {handled:true,content:"intercepted"}; } };`
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		// The intercepted call's own arguments are what the record has to hold.
		arguments := "{}"
		if limitMiB > 0 {
			arguments = string(mustJSON(t, map[string]any{"q": strings.Repeat("x", 3<<19)}))
		}
		m, err := extensions.NewManager(root)
		if err != nil {
			t.Fatal(err)
		}
		local := newStubRouter([]string{"m"}, "")
		local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
			calls := []any{
				map[string]any{"index": 0, "id": "i1", "type": "function", "function": map[string]any{"name": "intercepted_tool", "arguments": arguments}},
				map[string]any{"index": 1, "id": "c1", "type": "function", "function": map[string]any{"name": "other_tool", "arguments": "{}"}},
			}
			if stream {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write(append(append([]byte("data: "), mustJSON(t, map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "thinking"}}}})...), []byte("\n\n")...))
				_, _ = w.Write(append(append([]byte("data: "), mustJSON(t, map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": calls}}}})...), []byte("\n\n")...))
				_, _ = w.Write(append(append([]byte("data: "), mustJSON(t, map[string]any{"id": "a", "model": "m", "choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}})...), []byte("\n\ndata: [DONE]\n\n")...))
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(mustJSON(t, map[string]any{"id": "a", "choices": []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "thinking", "tool_calls": calls}, "finish_reason": "tool_calls"}}}))
		}
		s := newTestServer(local, newStubRouter(nil, ""))
		s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root, MaxToolRounds: 1, MaxContinuationMiB: limitMiB}}
		s.extensions = m
		s.routes()
		t.Cleanup(func() { _ = s.store.Close() })

		tools := `"tools":[{"type":"function","function":{"name":"intercepted_tool","parameters":{"type":"object"}}},{"type":"function","function":{"name":"other_tool","parameters":{"type":"object"}}}]`
		request := `{"model":"m","messages":[{"role":"user","content":"hi"}],` + tools + `}`
		if stream {
			request = `{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}],` + tools + `}`
		}
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(request))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w.Code, w.Body.String()
	}

	t.Run("stored", func(t *testing.T) {
		status, answer := run(t, false, 0)
		if status != http.StatusOK {
			t.Fatalf("status=%d %.300s", status, answer)
		}
		if !strings.Contains(answer, "call_ls_") || !strings.Contains(answer, "other_tool") {
			t.Fatalf("the fixture is not a mixed round: %.300s", answer)
		}
	})
	t.Run("json", func(t *testing.T) {
		status, answer := run(t, false, 1)
		if status != http.StatusOK {
			t.Fatalf("the last round failed the turn: status=%d %.300s", status, answer)
		}
		if !strings.Contains(answer, "thinking") {
			t.Fatalf("the model's text was dropped: %.300s", answer)
		}
		if strings.Contains(answer, "other_tool") || strings.Contains(answer, "call_ls_") {
			t.Fatalf("a call the caller cannot continue reached it: %.300s", answer)
		}
	})
	t.Run("stream", func(t *testing.T) {
		status, answer := run(t, true, 1)
		if status != http.StatusOK {
			t.Fatalf("the last round failed the stream: status=%d %.300s", status, answer)
		}
		if !strings.Contains(answer, "thinking") || strings.Count(answer, "[DONE]") != 1 {
			t.Fatalf("the stream did not end with the model's answer: %.300s", answer)
		}
		if strings.Contains(answer, "other_tool") || strings.Contains(answer, "call_ls_") {
			t.Fatalf("a call the caller cannot continue reached it: %s", answer)
		}
		if !strings.Contains(answer, `"finish_reason":"stop"`) {
			t.Fatalf("the stream ended on a tool-call frame: %.300s", answer)
		}
	})
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

func TestServer_ExtensionsResponsesToolLoopUsesChatBackend(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/chat/completions" {
			t.Fatalf("backend path = %s", r.URL.Path)
		}
		if !strings.Contains(string(body), `"name":"lookup"`) {
			t.Fatalf("missing Chat function tool: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-a","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":2,"completion_tokens":3,"total_tokens":5}}`)
			return
		}
		if !strings.Contains(string(body), "found:x") || !strings.Contains(string(body), "hi") {
			t.Fatalf("Chat continuation missing history: %s", body)
		}
		_, _ = io.WriteString(w, `{"id":"chatcmpl-b","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":5,"total_tokens":9}}`)
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

func TestServer_ExtensionsResponsesMixedResumeUsesChatBackend(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var secondBody map[string]any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"chatcmpl-a","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_ = json.Unmarshal(body, &secondBody)
		_, _ = io.WriteString(w, `{"id":"chatcmpl-b","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
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
	if strings.Contains(first.Body.String(), "call_ls_") {
		t.Fatalf("a visible surface must not rewrite caller ids: %s", first.Body.String())
	}
	payload := mustJSONObject(t, first.Body.Bytes())
	output, _ := payload["output"].([]any)
	var artifactID, clientCallID string
	seen := map[string]bool{}
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "mcp_call":
			seen["mcp_call"] = true
			artifactID, _ = item["id"].(string)
			if item["name"] != "lookup" || !strings.Contains(fmt.Sprintf("%v", item["output"]), "found:x") {
				t.Fatalf("mcp_call = %v", item)
			}
		case "function_call":
			seen["function_call"] = true
			clientCallID, _ = item["call_id"].(string)
		}
	}
	if !seen["mcp_call"] || !seen["function_call"] {
		t.Fatalf("the turn is missing artifacts or the caller's own call: %v", seen)
	}
	if !strings.HasPrefix(artifactID, "mcp_") || clientCallID != "c1" {
		t.Fatalf("ids: artifact=%q client=%q", artifactID, clientCallID)
	}
	// The stored chain must be what the caller holds, artifact included: a
	// previous_response_id continuation replays that body, and a row without the
	// mcp_call item would rebuild a conversation whose server call has no result.
	responseID, _ := payload["id"].(string)
	if responseID == "" {
		t.Fatalf("response has no id: %s", first.Body.String())
	}
	affinity, found, err := s.store.GetResponseAffinity(context.Background(), responseID, time.Now())
	if err != nil || !found {
		t.Fatalf("affinity row found=%v err=%v", found, err)
	}
	if !strings.Contains(string(affinity.Response), "mcp_call") || !strings.Contains(string(affinity.Response), "found:x") {
		t.Fatalf("the stored chain lost the server work: %s", affinity.Response)
	}
	// The caller replays the whole turn: the artifact item and its own call, then
	// its own output.
	second := post(fmt.Sprintf(`{"model":"m","input":[
		{"type":"mcp_call","id":%q,"server_label":"llama-swap","name":"lookup","arguments":"{\"q\":\"x\"}","output":"found:x","status":"completed"},
		{"type":"function_call","call_id":%q,"name":"client_tool","arguments":"{}"},
		{"type":"function_call_output","call_id":%q,"output":"client-result"}]}`,
		artifactID, clientCallID, clientCallID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second = %d rounds=%d %s", second.Code, rounds, second.Body.String())
	}
	// The upstream (Chat) sees the artifact folded into an ordinary call/result
	// pair next to the caller's own result.
	upstream := fmt.Sprintf("%v", secondBody["messages"])
	for _, want := range []string{"found:x", "client-result", "lookup"} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("upstream conversation is missing %q: %s", want, upstream)
		}
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

// A tool that cannot run (quota exhausted, upstream down) must not take the
// conversation with it: the failure is reported to the model as the tool
// result, and the model gets another turn to answer without it.
func TestServer_ExtensionsInterceptedToolFailureIsReportedToModel(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "failing")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: failing\nenabled: true\ninterceptClientTools: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { onToolCall(ctx, call) { throw new Error("Volcengine networked search error: Free quota has been exhausted."); } };`), 0o600); err != nil {
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
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		// The second round must carry the upstream error text as the tool result.
		if !strings.Contains(string(body), "Free quota has been exhausted") {
			t.Fatalf("tool failure was not reported to the model: %s", body)
		}
		if !strings.Contains(string(body), `"tool_call_id":"c1"`) {
			t.Fatalf("tool result is not addressed to the call: %s", body)
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"answered without search"},"finish_reason":"stop"}]}`)
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
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if rounds != 2 || !strings.Contains(w.Body.String(), "answered without search") {
		t.Fatalf("rounds=%d body=%s", rounds, w.Body.String())
	}
}

// failOnToolError keeps the strict behavior for operators who want it.
func TestServer_ExtensionsToolFailureCanStayFatal(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "strict")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: strict\nenabled: true\ninterceptClientTools: true\nfailOnToolError: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { onToolCall(ctx, call) { throw new Error("boom"); } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"web_search","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
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
	if w.Code == http.StatusOK {
		t.Fatalf("strict extension did not fail the request: %s", w.Body.String())
	}
}

// Malformed tool arguments for a server-owned tool are reported as the tool
// result, so the model can correct itself instead of the request failing.
// (A malformed call for a *client* tool still reaches the client: the extension
// only fronts tools it declares.)
func TestServer_ExtensionsMalformedToolArgumentsAreReportedToModel(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "args")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: args\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "args_tool", parameters: { type: "object" } } }],
  onToolCall(ctx, call) { return { content: "ok" }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
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
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"args_tool","arguments":"{not json"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !strings.Contains(string(body), "not valid JSON") {
			t.Fatalf("malformed arguments were not reported: %s", body)
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"retried"},"finish_reason":"stop"}]}`)
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
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), "retried") {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
}

// A server tool whose handler returns nothing (a name check that never matched,
// a handler that echoed its arguments) must produce a *string* tool result.
// The old fallback echoed the handler input, which the engine rejected with a
// validation error and failed the whole conversation.
func TestServer_ExtensionsEmptyToolResultIsReportedAsFailure(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "mismatch")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: mismatch\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The declared tool is "owned_tool" but the handler only answers
	// "other_name" and returns undefined for everything else.
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "owned_tool", parameters: { type: "object" } } }],
  onToolCall(ctx, call) { if (call.name !== "other_name") return undefined; return { content: "never" }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
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
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"owned_tool","arguments":"{\"query\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		if !strings.Contains(string(body), `"content":"tool \"owned_tool\" failed`) {
			t.Fatalf("tool result is not a failure string: %s", body)
		}
		if strings.Contains(string(body), `"content":{"`) {
			t.Fatalf("tool result echoed an object as content: %s", body)
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
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
	if w.Code != http.StatusOK || rounds != 2 {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
}

// A tool that failed during the request is dropped from the tool list for the
// following rounds: a model that keeps re-calling a broken tool (an exhausted
// upstream quota) would otherwise burn every round and end with no answer.
func TestServer_ExtensionsFailedToolIsNotReoffered(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "broken")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: broken\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "broken_tool", parameters: { type: "object" } } }],
  onToolCall(ctx, call) { throw new Error("quota exhausted"); }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
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
			if !strings.Contains(string(body), "broken_tool") {
				t.Fatalf("tool was not offered in round one: %s", body)
			}
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"broken_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		// The tool *definition* must be gone; the assistant's own tool_call for
		// it stays, as it is part of the conversation.
		var round2 map[string]any
		if err := json.Unmarshal(body, &round2); err != nil {
			t.Fatalf("round two body is not JSON: %v", err)
		}
		if tools, _ := round2["tools"].([]any); len(tools) != 0 {
			t.Fatalf("failed tool was offered again: %s", body)
		}
		if !strings.Contains(string(body), "quota exhausted") {
			t.Fatalf("failure was not reported to the model: %s", body)
		}
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"answered without search"},"finish_reason":"stop"}]}`)
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
	if w.Code != http.StatusOK || rounds != 2 || !strings.Contains(w.Body.String(), "answered without search") {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
}

// Spending every tool round must not become a request failure: the client gets
// the model's last response.
func TestServer_ExtensionsRoundLimitReturnsLastResponse(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "looping")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: looping\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "loop_tool", parameters: { type: "object" } } }],
  onToolCall(ctx, call) { return { content: "ok" }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
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
		w.Header().Set("Content-Type", "application/json")
		// The model calls the tool on every round.
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"loop_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	maxRounds := 2
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root, MaxToolRounds: maxRounds}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("round limit failed the request: status=%d body=%s", w.Code, w.Body.String())
	}
	if rounds != maxRounds {
		t.Fatalf("rounds=%d, want %d", rounds, maxRounds)
	}
}

// A turn that spends every tool round used to end on the model's last tool
// call - a call the extension would have run but has no round left for. The
// stream closed without a finish frame and without [DONE], so the client
// showed an empty reply. The final round must withhold the server tools, let
// the model answer with what it has, and end the stream properly.
func TestServer_ExtensionsChatStreamRoundLimitEndsWithDone(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var lastTools []any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		lastTools, _ = body["tools"].([]any)
		w.Header().Set("Content-Type", "text/event-stream")
		// The model calls the server tool on every round.
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"looking \"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
	}
	s := extensionTestServer(t, local)
	maxRounds := 2
	s.cfg.Extensions.MaxToolRounds = maxRounds
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	content := w.Body.String()
	if w.Code != http.StatusOK || rounds != maxRounds {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, content)
	}
	if strings.Count(content, "[DONE]") != 1 {
		t.Fatalf("stream must end with exactly one [DONE]: %s", content)
	}
	if !strings.Contains(content, `"finish_reason":"stop"`) {
		t.Fatalf("stream must end on a stop frame, not a tool call: %s", content)
	}
	if strings.Contains(content, "lookup") {
		t.Fatalf("the withheld tool call leaked to the client: %s", content)
	}
	if len(lastTools) != 0 {
		t.Fatalf("the final round still offered the server tool: %v", lastTools)
	}
}

// The non-streaming round limit must not hand the client a tool call for a
// server tool either: it can neither see the tool definition nor run it.
func TestServer_ExtensionsRoundLimitWithholdsServerTools(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var lastTools []any
	lastToolsSet := false
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		lastTools, _ = body["tools"].([]any)
		lastToolsSet = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
	}
	s := extensionTestServer(t, local)
	maxRounds := 2
	s.cfg.Extensions.MaxToolRounds = maxRounds
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != maxRounds {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
	if !lastToolsSet || len(lastTools) != 0 {
		t.Fatalf("the final round still offered the server tool: %v", lastTools)
	}
	if calls := extensionCalls(mustJSONObject(t, w.Body.Bytes()), "chat.completions"); len(calls) != 0 {
		t.Fatalf("a withheld call was returned to the client: %+v", calls)
	}
	if !strings.Contains(w.Body.String(), "thinking") {
		t.Fatalf("the model's text was dropped: %s", w.Body.String())
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return encoded
}

func mustJSONObject(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("response is not JSON: %v (%s)", err, raw)
	}
	return out
}

// The Anthropic and Gemini injections used to ignore the skip list entirely, so
// a failed tool was re-offered every round and the model looped until the round
// limit - the "no reply" the operators saw. Both dialects must honor it.
func TestServer_ExtensionsAnthropicAndGeminiInjectionSkipsFailedTools(t *testing.T) {
	tool := extensions.Tool{Type: "function", Execution: "server", Function: json.RawMessage(`{"name":"lookup","parameters":{"type":"object"}}`)}
	item := &extensions.Compiled{Manifest: extensions.Manifest{ID: "example"}, Tools: []extensions.Tool{tool}}
	skip := map[string]bool{"lookup": true}

	anthropic := map[string]any{"tools": []any{map[string]any{"name": "client_tool", "input_schema": map[string]any{"type": "object"}}}}
	if err := injectAnthropicTools(anthropic, item, map[string]*extensions.Compiled{}, skip); err != nil {
		t.Fatal(err)
	}
	if len(anthropic["tools"].([]any)) != 1 {
		t.Fatalf("Anthropic tools = %v, the failed tool was re-offered", anthropic["tools"])
	}

	gemini := map[string]any{"tools": []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": "client_tool"}}}}}
	if err := injectGeminiTools(gemini, item, map[string]*extensions.Compiled{}, skip); err != nil {
		t.Fatal(err)
	}
	container := gemini["tools"].([]any)[0].(map[string]any)
	declarations := container["functionDeclarations"].([]any)
	for _, raw := range declarations {
		if declaration, _ := raw.(map[string]any); declaration["name"] == "lookup" {
			t.Fatalf("Gemini tools = %v, the failed tool was re-offered", gemini["tools"])
		}
	}
}

// dropWithheldCalls scrubs a call for a tool the round did not offer, in every
// endpoint shape, without touching the sibling calls or the model's text.
func TestServer_ExtensionsDropWithheldCalls(t *testing.T) {
	withheld := map[string]bool{"lookup": true}
	chat := map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "text", "tool_calls": []any{
		map[string]any{"id": "s1", "function": map[string]any{"name": "lookup", "arguments": "{}"}},
		map[string]any{"id": "c1", "function": map[string]any{"name": "client_tool", "arguments": "{}"}},
	}}, "finish_reason": "tool_calls"}}}
	dropWithheldCalls(chat, "chat.completions", withheld)
	calls := extensionCalls(chat, "chat.completions")
	if len(calls) != 1 || calls[0].Name != "client_tool" {
		t.Fatalf("chat calls = %+v", calls)
	}
	choice := chat["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "stop" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}

	anthropic := map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "text"},
		map[string]any{"type": "tool_use", "id": "s1", "name": "lookup", "input": map[string]any{}},
	}, "stop_reason": "tool_use"}
	dropWithheldCalls(anthropic, "anthropic.messages", withheld)
	if blocks := anthropic["content"].([]any); len(blocks) != 1 || anthropic["stop_reason"] != "end_turn" {
		t.Fatalf("anthropic body = %v", anthropic)
	}

	responses := map[string]any{"output": []any{
		map[string]any{"type": "message", "role": "assistant"},
		map[string]any{"type": "function_call", "call_id": "s1", "name": "lookup"},
	}}
	dropWithheldCalls(responses, "responses", withheld)
	if output := responses["output"].([]any); len(output) != 1 {
		t.Fatalf("responses body = %v", responses)
	}
}

// The withheld set is ownership-based: on the final round a client tool the
// caller declared stays with the caller, even when an extension's server tool
// shares its name and the conflict policy left the caller's definition in
// place. The call must come back to the caller instead of being dropped.
func TestServer_ExtensionsRoundLimitKeepsClientToolWithSameName(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "shared")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// The default conflict policy ("skip") keeps the caller's definition.
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: shared\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "lookup", parameters: { type: "object" } } }],
  onToolCall(ctx, call) { return { content: "server-side" }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var finalTools []any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		var body map[string]any
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		finalTools, _ = body["tools"].([]any)
		w.Header().Set("Content-Type", "application/json")
		// The caller's own tool is the one being called.
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"thinking","tool_calls":[{"id":"c1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root, MaxToolRounds: 2}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	// The extension skipped its own tool (the caller declared it first), so the
	// call belongs to the caller and the turn ends after one round.
	if w.Code != http.StatusOK || rounds != 1 {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
	if len(finalTools) != 1 {
		t.Fatalf("the caller's tool was withheld: %v", finalTools)
	}
	calls := extensionCalls(mustJSONObject(t, w.Body.Bytes()), "chat.completions")
	if len(calls) != 1 || calls[0].Name != "lookup" {
		t.Fatalf("the caller's tool call was dropped: %+v", calls)
	}
}

// A handler that returns structured data must not put an object into a chat
// tool message: engines reject a tool message whose content is an object, and
// the failure took the whole conversation down with it. The result is encoded
// as text instead.
func TestServer_ExtensionsStructuredToolResultIsEncodedAsText(t *testing.T) {
	call := extensionCall{ID: "c1", Name: "structured_tool", Arguments: "{}"}
	output := map[string]any{"content": map[string]any{"rows": float64(2), "items": []any{"a", "b"}}}

	chat := toolResultFor("chat.completions", call, output)
	content, ok := chat["content"].(string)
	if !ok {
		t.Fatalf("chat tool content is not text: %#v", chat["content"])
	}
	if !strings.Contains(content, `"rows":2`) {
		t.Fatalf("structured content was not encoded: %s", content)
	}
	responses := toolResultFor("responses", call, output)
	if _, ok := responses["output"].(string); !ok {
		t.Fatalf("responses tool output is not text: %#v", responses["output"])
	}
	anthropic := toolResultFor("anthropic.messages", call, output)
	if _, ok := anthropic["content"].(string); !ok {
		t.Fatalf("anthropic tool result is not text: %#v", anthropic["content"])
	}
}

// A round runs without the stream bridge, but the upstream may still compress
// its body: the proxy forwards the caller's Accept-Encoding, and a large answer
// over a CDN comes back gzipped. Parsing those raw bytes as JSON failed the
// request with "extension upstream response is invalid JSON" even though the
// answer was perfectly good.
func TestServer_ExtensionsGzippedUpstreamRound(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		body := `{"choices":[{"index":0,"message":{"role":"assistant","content":"zipped answer"},"finish_reason":"stop"}]}`
		if rounds == 1 {
			body = `{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`
		}
		var compressed bytes.Buffer
		zw := gzip.NewWriter(&compressed)
		if _, err := zw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		_, _ = w.Write(compressed.Bytes())
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept-Encoding", "gzip, deflate")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "zipped answer") {
		t.Fatalf("the gzipped answer was lost: %s", w.Body.String())
	}
}

// An upstream may answer a non-streaming request with an SSE transcript (a
// passthrough surface streams when it wants to). The round must assemble that
// transcript instead of handing raw `data:` lines to json.Unmarshal.
func TestServer_ExtensionsSSEUpstreamRoundIsAssembled(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		w.Header().Set("Content-Type", "text/event-stream")
		if rounds == 1 {
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}}]},\"finish_reason\":null}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"streamed answer\"},\"finish_reason\":null}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")
	}
	s := extensionTestServer(t, local)
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK || rounds != 2 {
		t.Fatalf("status=%d rounds=%d body=%s", w.Code, rounds, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "streamed answer") {
		t.Fatalf("the assembled answer was lost: %s", w.Body.String())
	}
}

// The Responses transcript keeps its text: assembling a stream for a
// non-streaming caller used to return only the tool calls, so a plain text
// answer came back with an empty output array.
func TestServer_ExtensionsResponsesStreamAssemblyKeepsText(t *testing.T) {
	transcript := strings.Join([]string{
		"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"in_progress\",\"model\":\"m\",\"output\":[]}}\n\n",
		"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"delta\":\"text answer\"}\n\n",
		"event: response.output_text.done\ndata: {\"type\":\"response.output_text.done\",\"item_id\":\"msg_1\",\"output_index\":0,\"content_index\":0,\"text\":\"text answer\"}\n\n",
		"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"object\":\"response\",\"status\":\"completed\",\"model\":\"m\",\"output\":[{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"text answer\",\"annotations\":[]}]}],\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n",
	}, "")
	var response map[string]any
	if err := assembleExtensionStream("responses", []byte(transcript), &response); err != nil {
		t.Fatalf("assemble: %v", err)
	}
	output, _ := response["output"].([]any)
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		for _, rawPart := range asSlice(item["content"]) {
			if part, _ := rawPart.(map[string]any); part["text"] == "text answer" {
				return
			}
		}
	}
	t.Fatalf("the streamed text is missing from the assembled output: %#v", response)
}

func asSlice(value any) []any {
	items, _ := value.([]any)
	return items
}

// A client that resends its whole conversation includes the results of calls
// that already ran. Their pending records were consumed when they did, and
// demanding one for them rejected a resumable conversation with 409
// "extension pending call not found". The request must be served instead.
func TestServer_ExtensionsReplayedConversationIsNotAConflict(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"b","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"resumed"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	// The replayed conversation: an assistant turn carrying a call_ls_ id whose
	// pending record does not exist, answered by a tool result in the same body.
	replay := `{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":"call_ls_00000000000000000000000000000000","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"call_ls_00000000000000000000000000000000","content":"historical result"},
		{"role":"user","content":"continue"}
	]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(replay))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a replayed conversation was rejected: status=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "resumed") {
		t.Fatalf("the request did not reach the model: %s", w.Body.String())
	}
}

// A bare continuation (results only, no assistant turn) cannot be completed on a
// hidden surface, and an unknown call id cannot be resumed at all: both are
// answered the way Anthropic answers an incomplete continuation - 400 - with the
// requirement spelled out, rather than sending the upstream tool results for
// calls it never made.
func TestServer_ExtensionsBareContinuationIsRejected(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	s := extensionTestServer(t, local)
	bare := `{"model":"m","messages":[{"role":"tool","tool_call_id":"call_ls_11111111111111111111111111111111","content":"result with no conversation"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(bare))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s (want the 400 for a continuation that cannot be completed)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "replayed") {
		t.Fatalf("body = %s", w.Body.String())
	}
}

// requestCarriesCall is the predicate behind the two behaviours above, one
// shape per client dialect.
func TestServer_ExtensionsRequestCarriesCall(t *testing.T) {
	const id = "call_ls_abc"
	chat := map[string]any{"messages": []any{map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": id}}}}}
	anthropic := map[string]any{"messages": []any{map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "tool_use", "id": id}}}}}
	responses := map[string]any{"input": []any{map[string]any{"type": "function_call", "call_id": id}}}
	for _, tc := range []struct {
		name     string
		request  map[string]any
		endpoint string
		want     bool
	}{
		{"chat", chat, "chat.completions", true},
		{"anthropic", anthropic, "anthropic.messages", true},
		{"responses", responses, "responses", true},
		{"chat other id", chat, "chat.completions", false},
	} {
		request := tc.request
		callID := id
		if tc.want == false {
			callID = "call_ls_other"
		}
		if got := requestCarriesCall(request, tc.endpoint, callID); got != tc.want {
			t.Fatalf("%s: requestCarriesCall = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// TestServer_ExtensionsOversizedRequestHookIsSkipped pins what happens when a
// conversation grows past the worker's protocol line (8 MiB, shared with the
// extension's own bundle): the hook cannot run, and the request must still
// succeed. A 502 here would make the deployment unusable for anyone whose
// history - usually because it carries an image - crossed that line.
func TestServer_ExtensionsOversizedRequestHookIsSkipped(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "greedy")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: greedy\nenabled: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// A hook that would mark the request; if it ran, the marker reaches upstream.
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(`export default { onRequest(ctx, request) { request.marked = true; return request; } };`), 0o600); err != nil {
		t.Fatal(err)
	}
	m, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	// What reached the model, per request: the oversized one must arrive
	// unmodified (the hook was skipped), the small one must carry the marker
	// (the hook ran), which is what proves the skip is about size alone.
	var bodies []string
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = m
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })

	// 12 MiB of history: past the 8 MiB line whatever the bundle costs.
	history := strings.Repeat("x", 12<<20)
	body := `{"model":"m","messages":[{"role":"user","content":"` + history + `"}]}`
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("an oversized request hook failed the request: %d %.300s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "ok") || len(bodies) != 1 {
		t.Fatalf("the request did not reach the model: %d %.200s", w.Code, w.Body.String())
	}
	if strings.Contains(bodies[0], "marked") {
		t.Fatalf("the skipped hook still mutated the request: %.200s", bodies[0])
	}

	// A normal-sized conversation still runs the hook, so the skip is specific
	// to the capacity limit.
	r = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	r.Header.Set("Content-Type", "application/json")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("a normal request failed: %d %s", w.Code, w.Body.String())
	}
	if len(bodies) != 2 || !strings.Contains(bodies[1], "marked") {
		t.Fatalf("the hook did not run on a normal-sized request: %v", bodies)
	}
}

// TestServer_ExtensionsOversizedForwardAndResponseHooksAreSkipped pins that every
// request-shaped hook behaves the same way at the protocol limit: the request
// (or the answer) continues without the hook rather than failing. A 502 for
// onBeforeForward dropped long conversations entirely; a 502 for onResponse
// additionally threw away a model answer that had already been produced.
func TestServer_ExtensionsOversizedForwardAndResponseHooksAreSkipped(t *testing.T) {
	for _, entry := range []struct {
		id   string
		hook string
	}{
		{"forwarder", "onBeforeForward"},
		{"responder", "onResponse"},
	} {
		root := t.TempDir()
		dir := filepath.Join(root, entry.id)
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte("id: "+entry.id+"\nenabled: true\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		source := `export default { ` + entry.hook + `(ctx, value) { value.touched = true; return value; } };`
		if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(source), 0o600); err != nil {
			t.Fatal(err)
		}
		m, err := extensions.NewManager(root)
		if err != nil {
			t.Fatal(err)
		}
		local := newStubRouter([]string{"m"}, "")
		local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
		}
		s := newTestServer(local, newStubRouter(nil, ""))
		s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
		s.extensions = m
		s.routes()
		t.Cleanup(func() { _ = s.store.Close() })

		history := strings.Repeat("x", 12<<20)
		body := `{"model":"m","messages":[{"role":"user","content":"` + history + `"}]}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("%s: an oversized %s hook failed the request: %d %.300s", entry.id, entry.hook, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), "ok") {
			t.Fatalf("%s: the answer was lost: %.200s", entry.id, w.Body.String())
		}
	}
}

// The extension's own calls of a mixed round are deferred, not executed when the
// round is handed over: they run when the caller comes back with the
// conversation, and a caller that abandons the turn never causes them to run.
func TestServer_ExtensionsDeferredServerToolRunsOnContinuation(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "marker")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	marker := filepath.Join(out, "ran.txt")
	manifest := "id: marker\nenabled: true\npermissions:\n  writeRoots:\n    - " + out + "\n"
	if err := os.WriteFile(filepath.Join(dir, "manifest.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	script := `export default {
  tools: [{ type: "function", execution: "server", function: { name: "mark", parameters: { type: "object" } } }],
  async onToolCall(ctx, call) { await ctx.files.write(` + fmt.Sprintf("%q", marker) + `, "ran"); return { content: "marked" }; }
};`
	if err := os.WriteFile(filepath.Join(dir, "index.js"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := extensions.NewManager(root)
	if err != nil {
		t.Fatal(err)
	}
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var secondBody map[string]any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"a","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"mark","arguments":"{}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_ = json.Unmarshal(raw, &secondBody)
		_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}, Extensions: config.ExtensionsConfig{Enabled: true, Directory: root}}
	s.extensions = manager
	s.routes()
	t.Cleanup(func() { _ = s.store.Close() })
	post := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	first := post(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"client_tool","parameters":{"type":"object"}}}]}`)
	if first.Code != http.StatusOK {
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	payload := mustJSONObject(t, first.Body.Bytes())
	calls := extensionCalls(payload, "chat.completions")
	if len(calls) != 1 || calls[0].Name != "client_tool" {
		t.Fatalf("visible calls = %+v", calls)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the extension's own tool ran before the caller came back (%v)", err)
	}
	// Abandoning the turn (a bare continuation) does not run it either.
	bare := post(fmt.Sprintf(`{"model":"m","messages":[{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, calls[0].ID))
	if bare.Code != http.StatusBadRequest {
		t.Fatalf("bare continuation = %d %.200s", bare.Code, bare.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the extension's own tool ran for an abandoned turn (%v)", err)
	}
	// The replay runs it and completes the round.
	second := post(fmt.Sprintf(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":%q,"content":"client-result"}]}`, calls[0].ID, calls[0].ID))
	if second.Code != http.StatusOK || rounds != 2 || !strings.Contains(second.Body.String(), "done") {
		t.Fatalf("second=%d rounds=%d %.200s", second.Code, rounds, second.Body.String())
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("the deferred tool did not run on the continuation: %v", err)
	}
	upstream := fmt.Sprintf("%v", secondBody["messages"])
	if !strings.Contains(upstream, "marked") || !strings.Contains(upstream, "client-result") {
		t.Fatalf("upstream conversation is missing results: %s", upstream)
	}
}

// A continuation that also carries new caller content keeps it: the request is the
// caller's own conversation now, not a stored base that replaced it.
func TestServer_ExtensionsContinuationKeepsNewMessages(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	var secondBody map[string]any
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if rounds == 1 {
			_, _ = io.WriteString(w, `{"id":"a","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"s1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"id":"c1","type":"function","function":{"name":"client_tool","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`)
			return
		}
		_ = json.Unmarshal(raw, &secondBody)
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
		t.Fatalf("first=%d %s", first.Code, first.Body.String())
	}
	calls := extensionCalls(mustJSONObject(t, first.Body.Bytes()), "chat.completions")
	if len(calls) != 1 {
		t.Fatalf("visible calls = %+v", calls)
	}
	second := post(fmt.Sprintf(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"","tool_calls":[{"id":%q,"type":"function","function":{"name":"client_tool","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":%q,"content":"client-result"},
		{"role":"user","content":"and now something else"}]}`, calls[0].ID, calls[0].ID))
	if second.Code != http.StatusOK || rounds != 2 {
		t.Fatalf("second=%d rounds=%d %.200s", second.Code, rounds, second.Body.String())
	}
	upstream := fmt.Sprintf("%v", secondBody["messages"])
	for _, want := range []string{"client-result", "and now something else", "found:x"} {
		if !strings.Contains(upstream, want) {
			t.Fatalf("upstream conversation lost %q: %s", want, upstream)
		}
	}
}
