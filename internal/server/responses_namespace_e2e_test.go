package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

// codexNamespaceRequest is the shape Codex sends to /v1/responses: ordinary
// tools, namespace-wrapped MCP tools, and one tool declared outside the top
// level list. It is the request that used to lose cua_repl/node_repl/codex_app.
const codexNamespaceRequest = `{
	"model":"m","stream":false,
	"tools":[
		{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object"}},
		{"type":"namespace","name":"mcp__cua_repl","description":"UI automation","tools":[
			{"type":"function","name":"js","parameters":{"type":"object"}}
		]},
		{"type":"namespace","name":"mcp__node_repl","description":"Node REPL","tools":[
			{"type":"function","name":"js","parameters":{"type":"object"}},
			{"type":"function","name":"js_reset","parameters":{"type":"object"}}
		]}
	],
	"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"open a tab"}]},
		{"type":"additional_tools","role":"developer","tools":[
			{"type":"function","name":"mcp__codex_app__read_thread","parameters":{"type":"object"}}
		]}
	]
}`

// TestServer_ResponsesNamespaceToolsReachChatUpstream verifies that the tools
// Codex declares in namespaces survive conversion to the Chat upstream under
// distinguishable names. A collapsed or dropped declaration is what produced
// "unsupported call: js": the model answered with a name the daemon could not
// map back to a namespace.
func TestServer_ResponsesNamespaceToolsReachChatUpstream(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	var upstreamTools []string
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read transformed request: %v", err)
		}
		var request struct {
			Tools []struct {
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatalf("decode converted request: %v", err)
		}
		for _, tool := range request.Tools {
			upstreamTools = append(upstreamTools, tool.Function.Name)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-e2e","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}]}`))
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	s.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(codexNamespaceRequest))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	got := strings.Join(upstreamTools, ",")
	for _, want := range []string{"mcp__cua_repl__js", "mcp__node_repl__js", "mcp__node_repl__js_reset", "mcp__codex_app__read_thread"} {
		if !strings.Contains(got, want) {
			t.Fatalf("upstream tools %v missing %q: two namespaces both declare js, so a bare name cannot address them", upstreamTools, want)
		}
	}
	_ = s.Shutdown(0)
}

// TestServer_ResponsesNamespaceCallRoundTrip verifies the full round trip: the
// Chat upstream answers with the flattened name and the client must receive
// the call back under its namespace.
func TestServer_ResponsesNamespaceCallRoundTrip(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-e2e","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_x","type":"function","function":{"name":"mcp__cua_repl__js","arguments":"{\"code\":\"await cua.getState()\"}"}}]}}]}`))
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	s.routes()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(codexNamespaceRequest))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"output"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Responses body: %v", err)
	}
	if len(response.Output) != 1 {
		t.Fatalf("output=%#v", response.Output)
	}
	call := response.Output[0]
	if call.Type != "function_call" || call.CallID != "call_x" {
		t.Fatalf("call=%#v", call)
	}
	if call.Namespace != "mcp__cua_repl" || call.Name != "js" {
		t.Fatalf("call restored as namespace=%q name=%q, want mcp__cua_repl/js", call.Namespace, call.Name)
	}
	_ = s.Shutdown(0)
}

func TestServer_ResponsesNamespaceStreamCallRoundTrip(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	local.serveHTTP = func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, `data: {"id":"chat-e2e","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_x","type":"function","function":{"name":"mcp__cua_repl__js","arguments":"{\"code\":"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"chat-e2e","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"await cua.getState()\"}"}}]}}]}`+"\n\n")
		_, _ = io.WriteString(w, `data: {"id":"chat-e2e","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	s.routes()

	body := strings.Replace(codexNamespaceRequest, `"stream":false`, `"stream":true`, 1)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	type itemEvent struct {
		item map[string]any
	}
	var added, done itemEvent
	var completed map[string]any
	for _, line := range strings.Split(recorder.Body.String(), "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.output_item.added":
			added.item, _ = event["item"].(map[string]any)
		case "response.output_item.done":
			done.item, _ = event["item"].(map[string]any)
		case "response.completed":
			completed, _ = event["response"].(map[string]any)
		}
	}
	for name, item := range map[string]map[string]any{"added": added.item, "done": done.item} {
		if item == nil || item["namespace"] != "mcp__cua_repl" || item["name"] != "js" {
			t.Fatalf("%s item = %v, want namespace mcp__cua_repl and name js", name, item)
		}
	}
	output, _ := completed["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("completed output = %v", completed["output"])
	}
	call, _ := output[0].(map[string]any)
	if call["namespace"] != "mcp__cua_repl" || call["name"] != "js" {
		t.Fatalf("completed call = %v", call)
	}
	_ = s.Shutdown(0)
}
