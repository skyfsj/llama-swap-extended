package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// codexToolRequest is the canonical request a Chat upstream is answering. It
// carries the flattening bindings the response side must restore.
func codexToolRequest(t *testing.T) *dialect.Request {
	t.Helper()
	body := []byte(`{
		"model": "m",
		"tools": [
			{"type":"function","name":"exec_command","parameters":{"type":"object"}},
			{"type":"namespace","name":"mcp__cua_repl","tools":[
				{"type":"function","name":"js","parameters":{"type":"object"}}
			]},
			{"type":"namespace","name":"mcp__node_repl","tools":[
				{"type":"function","name":"js","parameters":{"type":"object"}}
			]},
			{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}}
		]
	}`)
	request, _, err := dialect.DecodeRequest(dialect.OpenAIResponses, body)
	if err != nil {
		t.Fatalf("decode request: %v", err)
	}
	return request
}

// TestConvertResponse_ResponsesRestoresNamespace pins the fix for "unsupported
// call: js": a Chat upstream answers with the flattened name, and the response
// must put the member back in its namespace. Codex resolves a tool by
// (namespace, name), so a bare "js" has no dispatcher.
func TestConvertResponse_ResponsesRestoresNamespace(t *testing.T) {
	request := codexToolRequest(t)
	chat := []byte(`{
		"id":"chatcmpl-1","model":"m",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
			"tool_calls":[
				{"id":"call_a","type":"function","function":{"name":"mcp__cua_repl__js","arguments":"{\"code\":\"await cua.getState()\"}"}},
				{"id":"call_b","type":"function","function":{"name":"mcp__node_repl__js","arguments":"{\"code\":\"1+1\"}"}},
				{"id":"call_c","type":"function","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}}
			]}}]
	}`)

	converted, _, err := dialect.ConvertResponseWithRequest(dialect.OpenAIChat, dialect.OpenAIResponses, chat, request)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out struct {
		Output []struct {
			Type      string `json:"type"`
			CallID    string `json:"call_id"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Arguments string `json:"arguments"`
			Input     string `json:"input"`
		} `json:"output"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Output) != 3 {
		t.Fatalf("output items = %d, want 3: %s", len(out.Output), converted)
	}
	calls := map[string]struct{ namespace, name, kind, arguments, input string }{}
	for _, item := range out.Output {
		calls[item.CallID] = struct{ namespace, name, kind, arguments, input string }{
			item.Namespace, item.Name, item.Type, item.Arguments, item.Input,
		}
	}
	if call := calls["call_a"]; call.namespace != "mcp__cua_repl" || call.name != "js" {
		t.Fatalf("call_a restored as %+v, want namespace mcp__cua_repl name js", call)
	}
	if call := calls["call_b"]; call.namespace != "mcp__node_repl" || call.name != "js" {
		t.Fatalf("call_b restored as %+v: two namespaces share the member name js", call)
	}
	if call := calls["call_c"]; call.kind != "custom_tool_call" || call.input != "*** Begin Patch" {
		t.Fatalf("custom tool restored as %+v", call)
	}
}

// TestConvertResponse_ResponsesSynthesizesCallID covers a local upstream that
// answers without tool-call ids. A Responses client pairs the output with its
// function_call_output by call_id, so an empty one makes the call unusable.
func TestConvertResponse_ResponsesSynthesizesCallID(t *testing.T) {
	chat := []byte(`{
		"id":"chatcmpl-1","model":"m",
		"choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,
			"tool_calls":[
				{"type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}
			]}}]
	}`)

	converted, _, err := dialect.ConvertResponse(dialect.OpenAIChat, dialect.OpenAIResponses, chat)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out struct {
		Output []struct {
			CallID string `json:"call_id"`
		} `json:"output"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Output) != 1 || strings.TrimSpace(out.Output[0].CallID) == "" {
		t.Fatalf("a missing upstream call id must be synthesized, got %s", converted)
	}
}

// TestConvertResponse_ResponsesEchoesToolPolicy pins that the response object
// reports the policy the client actually sent, not a fixed auto/true/[].
func TestConvertResponse_ResponsesEchoesToolPolicy(t *testing.T) {
	request := codexToolRequest(t)
	request.ParallelTools = boolPtr(false)
	request.ToolChoice = &dialect.ToolChoice{Mode: dialect.ToolChoiceTool, Name: "mcp__cua_repl__js", Namespace: "mcp__cua_repl"}

	chat := []byte(`{"id":"c","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`)
	converted, _, err := dialect.ConvertResponseWithRequest(dialect.OpenAIChat, dialect.OpenAIResponses, chat, request)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parallel, ok := out["parallel_tool_calls"].(bool); !ok || parallel {
		t.Fatalf("parallel_tool_calls = %v, want false", out["parallel_tool_calls"])
	}
	choice, _ := out["tool_choice"].(map[string]any)
	if choice["name"] != "js" || choice["namespace"] != "mcp__cua_repl" {
		t.Fatalf("tool_choice = %v, want the namespaced member", out["tool_choice"])
	}
	tools, _ := out["tools"].([]any)
	var namespaceEntry map[string]any
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] == "namespace" && tool["name"] == "mcp__cua_repl" {
			namespaceEntry = tool
		}
	}
	if namespaceEntry == nil {
		t.Fatalf("tools did not keep the namespace grouping: %v", out["tools"])
	}
}

func boolPtr(value bool) *bool { return &value }
