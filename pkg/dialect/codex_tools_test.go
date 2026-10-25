package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// TestConvertRequest_ResponsesNamespaceTools pins the Codex tool contract: a
// namespace declaration must reach a function-calling target as one function
// per member, named "<namespace>__<member>", because Codex dispatches the call
// it gets back by that exact flattened name.
func TestConvertRequest_ResponsesNamespaceTools(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"tools": [
			{"type":"function","name":"exec_command","description":"run","parameters":{"type":"object"}},
			{"type":"namespace","name":"mcp__cua_repl","description":"UI automation","tools":[
				{"type":"function","name":"js","description":"Run JS","parameters":{"type":"object"}},
				{"type":"function","name":"js_reset","description":"Reset","parameters":{"type":"object"}}
			]},
			{"type":"web_search"}
		],
		"input": [{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)

	converted, diagnostics, err := dialect.ConvertRequest(dialect.OpenAIResponses, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out struct {
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string `json:"name"`
				Description string `json:"description"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := map[string]string{}
	for _, tool := range out.Tools {
		names[tool.Function.Name] = tool.Function.Description
	}
	for _, want := range []string{"exec_command", "mcp__cua_repl__js", "mcp__cua_repl__js_reset"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("tool %q missing from converted tools %v", want, keysOf(names))
		}
	}
	if _, leaked := names["js"]; leaked {
		t.Fatal("namespace member must not be exposed under its bare name: a bare name collides across namespaces")
	}
	if description := names["mcp__cua_repl__js"]; !strings.Contains(description, "UI automation") {
		t.Fatalf("namespace description not folded into member: %q", description)
	}
	if !hasDiagnostic(diagnostics, "hosted_tool_dropped") {
		t.Fatal("web_search should still be reported as a dropped hosted tool")
	}
}

// TestConvertRequest_ResponsesAdditionalTools covers the incremental tool
// channel Codex uses: tools declared inside the input must join the top-level
// list, because no function-calling target has an input item for tool
// declarations.
func TestConvertRequest_ResponsesAdditionalTools(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"additional_tools","role":"developer","tools":[
				{"type":"function","name":"mcp__node_repl__js","description":"Run JS","parameters":{"type":"object"}},
				{"type":"namespace","name":"late","tools":[
					{"type":"function","name":"ping","parameters":{"type":"object"}}
				]}
			]}
		]
	}`)

	converted, diagnostics, err := dialect.ConvertRequest(dialect.OpenAIResponses, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out struct {
		Tools []struct {
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	names := map[string]bool{}
	for _, tool := range out.Tools {
		names[tool.Function.Name] = true
	}
	if !names["mcp__node_repl__js"] || !names["late__ping"] {
		t.Fatalf("additional_tools not merged into the tool list: %v", boolKeysOf(names))
	}
	for _, message := range out.Messages {
		if message["role"] == "developer" && message["content"] == nil {
			t.Fatal("an additional_tools item leaked into the transcript as an empty turn")
		}
	}
	if !hasDiagnostic(diagnostics, "additional_tools_flattened") {
		t.Fatal("merging additional_tools must be reported as a positional loss")
	}
}

// TestConvertRequest_ResponsesCustomTool pins the freeform tool contract: a
// custom tool becomes a function with one string argument, and its call is
// wrapped so the target can still answer in JSON.
func TestConvertRequest_ResponsesCustomTool(t *testing.T) {
	body := []byte(`{
		"model": "m",
		"tools": [{"type":"custom","name":"apply_patch","description":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: x"}}],
		"input": [
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
			{"type":"custom_tool_call","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"call_1","output":"ok"}
		]
	}`)

	converted, _, err := dialect.ConvertRequest(dialect.OpenAIResponses, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out struct {
		Tools    []map[string]any `json:"tools"`
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("tools = %d, want 1", len(out.Tools))
	}
	function := out.Tools[0]["function"].(map[string]any)
	if function["name"] != "apply_patch" {
		t.Fatalf("custom tool name = %v", function["name"])
	}
	parameters := function["parameters"].(map[string]any)
	properties := parameters["properties"].(map[string]any)
	if _, ok := properties["input"]; !ok {
		t.Fatalf("custom tool must take one string input argument, got %v", parameters)
	}
	var replayedArguments string
	for _, message := range out.Messages {
		calls, _ := message["tool_calls"].([]any)
		for _, rawCall := range calls {
			call := rawCall.(map[string]any)
			fn := call["function"].(map[string]any)
			if fn["name"] == "apply_patch" {
				replayedArguments = fn["arguments"].(string)
			}
		}
	}
	if replayedArguments != `{"input":"*** Begin Patch"}` {
		t.Fatalf("custom tool call arguments = %q", replayedArguments)
	}
}

func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func boolKeysOf(m map[string]bool) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	return keys
}

func hasDiagnostic(diagnostics []dialect.Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
