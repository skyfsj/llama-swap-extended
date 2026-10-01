package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// parse converts a JSON object literal into a decoded map. Tests are written
// with JSON literals so they mirror the CC Switch Rust test bodies 1:1.
func parse(t *testing.T, literal string) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal([]byte(literal), &out); err != nil {
		t.Fatalf("parse test JSON: %v\n%s", err, literal)
	}
	return out
}

// jsonEq compares two values by their canonical JSON encoding so numeric
// shapes and key order never matter.
func jsonEq(t *testing.T, name string, got, want any) {
	t.Helper()
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("%s:\n  got  %s\n  want %s", name, gotJSON, wantJSON)
	}
}

func convertRequest(t *testing.T, literal string) map[string]any {
	t.Helper()
	data, err := ResponsesToChat([]byte(literal))
	if err != nil {
		t.Fatalf("ResponsesToChat: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode converted request: %v", err)
	}
	return out
}

func messages(t *testing.T, result map[string]any) []any {
	t.Helper()
	list, ok := result["messages"].([]any)
	if !ok {
		t.Fatalf("result has no messages array: %v", result)
	}
	return list
}

func messageRoles(t *testing.T, result map[string]any) []string {
	t.Helper()
	list := messages(t, result)
	roles := make([]string, 0, len(list))
	for _, raw := range list {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		roles = append(roles, role)
	}
	return roles
}

func rolesEq(t *testing.T, result map[string]any, want ...string) {
	t.Helper()
	got := messageRoles(t, result)
	gotJSON, _ := json.Marshal(got)
	wantJSON, _ := json.Marshal(want)
	if string(gotJSON) != string(wantJSON) {
		t.Fatalf("message roles:\n  got  %s\n  want %s", gotJSON, wantJSON)
	}
}

func convertResponse(t *testing.T, literal string) map[string]any {
	t.Helper()
	data, err := ChatResponseToResponses([]byte(literal))
	if err != nil {
		t.Fatalf("ChatResponseToResponses: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode converted response: %v", err)
	}
	return out
}

// ---------------------------------------------------------------------------
// Request conversion
// ---------------------------------------------------------------------------

func TestResponsesRequestWithStreamInjectsIncludeUsage(t *testing.T) {
	result := convertRequest(t, `{
		"model": "kimi-k2.6",
		"input": [{"role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
		"stream": true
	}`)
	if result["stream"] != true {
		t.Fatalf("stream should be true, got %v", result["stream"])
	}
	jsonEq(t, "stream_options", result["stream_options"], map[string]any{"include_usage": true})
}

func TestResponsesRequestWithoutStreamOmitsStreamOptions(t *testing.T) {
	result := convertRequest(t, `{
		"model": "kimi-k2.6",
		"input": [{"role": "user", "content": [{"type": "input_text", "text": "hi"}]}]
	}`)
	if _, exists := result["stream_options"]; exists {
		t.Fatalf("stream_options should be omitted")
	}
}

func TestResponsesRequestMergesIncludeUsageIntoExistingStreamOptions(t *testing.T) {
	result := convertRequest(t, `{
		"model": "kimi-k2.6",
		"input": [{"role": "user", "content": [{"type": "input_text", "text": "hi"}]}],
		"stream": true,
		"stream_options": {"continuous_usage_stats": true}
	}`)
	jsonEq(t, "stream_options", result["stream_options"], map[string]any{
		"include_usage":          true,
		"continuous_usage_stats": true,
	})
}

func TestResponsesRequestMapsInputFileContentParts(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [{
			"role": "user",
			"content": [
				{"type": "input_text", "text": "Summarize this."},
				{"type": "input_file", "file_id": "file_123", "file_url": "https://example.com/spec.pdf", "filename": "spec.pdf"},
				{"type": "input_audio", "input_audio": {"data": "UklGRg==", "format": "wav"}}
			]
		}]
	}`)
	content := messages(t, result)[0].(map[string]any)["content"].([]any)
	jsonEq(t, "content[0].type", content[0].(map[string]any)["type"], "text")
	jsonEq(t, "content[1].type", content[1].(map[string]any)["type"], "file")
	file := content[1].(map[string]any)["file"].(map[string]any)
	jsonEq(t, "file.file_id", file["file_id"], "file_123")
	if _, exists := file["file_url"]; exists {
		t.Fatalf("file_url must not leak into the Chat file part")
	}
	jsonEq(t, "file.filename", file["filename"], "spec.pdf")
	jsonEq(t, "content[2].type", content[2].(map[string]any)["type"], "input_audio")
	jsonEq(t, "content[2].format", content[2].(map[string]any)["input_audio"].(map[string]any)["format"], "wav")
}

func TestResponsesRequestDoesNotEmitChatFileForURLOnlyInputFile(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [{
			"role": "user",
			"content": [
				{"type": "input_text", "text": "Summarize this URL file."},
				{"type": "input_file", "file_url": "https://example.com/spec.pdf"}
			]
		}]
	}`)
	jsonEq(t, "content", messages(t, result)[0].(map[string]any)["content"], "Summarize this URL file.")
}

func TestResponsesRequestMapsTopLevelInputFileItem(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [{"type": "input_file", "file_id": "file_top", "filename": "top.pdf"}]
	}`)
	first := messages(t, result)[0].(map[string]any)
	if first["role"] != "user" {
		t.Fatalf("role should default to user, got %v", first["role"])
	}
	content := first["content"].([]any)
	jsonEq(t, "content[0].type", content[0].(map[string]any)["type"], "file")
	jsonEq(t, "file.file_id", content[0].(map[string]any)["file"].(map[string]any)["file_id"], "file_top")
}

func TestTopLevelUserContentPartClearsPendingReasoning(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "reasoning", "summary": [{"text": "stale reasoning"}]},
			{"type": "input_text", "text": "Please run the tool."},
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}"}
		],
		"tools": [{"type": "function", "name": "lookup", "parameters": {"type": "object"}}]
	}`)
	list := messages(t, result)
	if list[0].(map[string]any)["role"] != "user" {
		t.Fatalf("first message should be user")
	}
	jsonEq(t, "user content", list[0].(map[string]any)["content"], "Please run the tool.")
	jsonEq(t, "reasoning placeholder", list[1].(map[string]any)["reasoning_content"], "tool call")
}

func TestResponsesToChatMapsMessagesToolsAndLimits(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"instructions": "You are concise.",
		"input": [
			{
				"role": "user",
				"content": [
					{"type": "input_text", "text": "Weather?"},
					{"type": "input_image", "image_url": "data:image/png;base64,abc"},
					{"type": "input_text", "text": "Use Celsius."}
				]
			},
			{"type": "function_call", "call_id": "call_1", "name": "get_weather", "arguments": "{\"city\":\"Tokyo\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Sunny"}
		],
		"tools": [{
			"type": "function",
			"name": "get_weather",
			"description": "Get weather",
			"parameters": {"type": "object"},
			"strict": true
		}],
		"tool_choice": {"type": "function", "name": "get_weather"},
		"max_output_tokens": 100,
		"reasoning": {"effort": "high"},
		"stream": true
	}`)
	list := messages(t, result)
	if result["model"] != "gpt-5.4" {
		t.Fatalf("model mismatch: %v", result["model"])
	}
	if list[0].(map[string]any)["role"] != "system" {
		t.Fatalf("instructions should become a system message")
	}
	userContent := list[1].(map[string]any)["content"].([]any)
	jsonEq(t, "content[0].type", userContent[0].(map[string]any)["type"], "text")
	jsonEq(t, "content[1].type", userContent[1].(map[string]any)["type"], "image_url")
	jsonEq(t, "content[2].text", userContent[2].(map[string]any)["text"], "Use Celsius.")
	toolCalls := list[2].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "tool call id", toolCalls[0].(map[string]any)["id"], "call_1")
	if list[3].(map[string]any)["role"] != "tool" {
		t.Fatalf("output should become a tool message")
	}
	tools := result["tools"].([]any)
	jsonEq(t, "tool name", tools[0].(map[string]any)["function"].(map[string]any)["name"], "get_weather")
	jsonEq(t, "tool strict", tools[0].(map[string]any)["function"].(map[string]any)["strict"], true)
	jsonEq(t, "tool_choice", result["tool_choice"].(map[string]any)["function"].(map[string]any)["name"], "get_weather")
	jsonEq(t, "max_tokens", result["max_tokens"], float64(100))
	jsonEq(t, "reasoning_effort", result["reasoning_effort"], "high")
}

func TestResponsesToChatDefaultsToolParameters(t *testing.T) {
	cases := []struct {
		name string
		tool string
	}{
		{"null parameters", `{"type": "function", "name": "codex_app__automation_update", "description": "Update an automation.", "parameters": null}`},
		{"nested null parameters", `{"type": "function", "function": {"name": "codex_app__automation_update", "description": "Update an automation.", "parameters": null}}`},
		{"nested missing parameters", `{"type": "function", "function": {"name": "codex_app__automation_update", "description": "Update an automation."}}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			result := convertRequest(t, fmt.Sprintf(`{
				"model": "gpt-5.4",
				"tools": [%s],
				"input": "hi"
			}`, testCase.tool))
			tools := result["tools"].([]any)
			jsonEq(t, "parameters", tools[0].(map[string]any)["function"].(map[string]any)["parameters"],
				map[string]any{"type": "object", "properties": map[string]any{}})
		})
	}
}

func TestResponsesToChatNormalizesExplicitNullToolParameterType(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tools": [{
			"type": "function",
			"name": "search",
			"parameters": {
				"type": null,
				"properties": {"query": {"type": "string"}},
				"required": ["query"]
			}
		}],
		"input": "hi"
	}`)
	parameters := result["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	jsonEq(t, "type", parameters["type"], "object")
	jsonEq(t, "properties.query.type", parameters["properties"].(map[string]any)["query"].(map[string]any)["type"], "string")
	jsonEq(t, "required", parameters["required"], []any{"query"})
}

func TestResponsesToChatDefaultsOneOfToolParametersType(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tools": [{
			"type": "function",
			"name": "lookup",
			"parameters": {"oneOf": [
				{"type": "object", "properties": {"id": {"type": "string"}}},
				{"type": "object", "properties": {"slug": {"type": "string"}}}
			]}
		}],
		"input": "hi"
	}`)
	parameters := result["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	jsonEq(t, "type", parameters["type"], "object")
	if _, exists := parameters["oneOf"]; !exists {
		t.Fatalf("oneOf must be preserved")
	}
}

func TestResponsesToChatExposesToolSearchAndLoadedNamespaceTools(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tools": [{"type": "tool_search"}],
		"input": [
			{
				"type": "tool_search_call",
				"call_id": "call_tool_search_1",
				"status": "completed",
				"execution": "client",
				"arguments": {"query": "Gmail search emails", "limit": 5}
			},
			{
				"type": "tool_search_output",
				"call_id": "call_tool_search_1",
				"status": "completed",
				"execution": "client",
				"tools": [{
					"type": "namespace",
					"name": "mcp__codex_apps__gmail",
					"description": "Find and reference emails from your inbox.",
					"tools": [{
						"type": "function",
						"name": "_search_emails",
						"description": "Search Gmail for emails matching a query.",
						"strict": false,
						"parameters": {
							"type": "object",
							"properties": {"query": {"type": "string"}, "max_results": {"type": "integer"}},
							"required": ["query"]
						}
					}]
				}]
			},
			{"type": "message", "role": "user", "content": "Search unread inbox mail."}
		]
	}`)
	var toolNames []string
	for _, raw := range result["tools"].([]any) {
		function, _ := raw.(map[string]any)["function"].(map[string]any)
		name, _ := function["name"].(string)
		toolNames = append(toolNames, name)
	}
	if !containsString(toolNames, "tool_search") || !containsString(toolNames, "mcp__codex_apps__gmail___search_emails") {
		t.Fatalf("tool names missing: %v", toolNames)
	}
	list := messages(t, result)
	toolCalls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "tool_search call name", toolCalls[0].(map[string]any)["function"].(map[string]any)["name"], "tool_search")
	if list[1].(map[string]any)["role"] != "tool" {
		t.Fatalf("tool_search_output should become a tool message")
	}
	jsonEq(t, "tool_call_id", list[1].(map[string]any)["tool_call_id"], "call_tool_search_1")
	if content, ok := list[1].(map[string]any)["content"].(string); !ok || !strings.Contains(content, "mcp__codex_apps__gmail") {
		t.Fatalf("tool content should embed the loaded namespace: %v", list[1].(map[string]any)["content"])
	}
}

func TestResponsesToChatMapsCustomToolAndChoice(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tools": [{"type": "custom", "name": "apply_patch", "description": "Apply a patch to files."}],
		"tool_choice": {"type": "custom", "name": "apply_patch"},
		"input": [{
			"type": "custom_tool_call",
			"id": "ctc_1",
			"call_id": "call_patch",
			"name": "apply_patch",
			"input": "*** Begin Patch\n*** End Patch"
		}]
	}`)
	tools := result["tools"].([]any)
	function := tools[0].(map[string]any)["function"].(map[string]any)
	jsonEq(t, "tool name", function["name"], "apply_patch")
	jsonEq(t, "required input", function["parameters"].(map[string]any)["required"].([]any)[0], "input")
	jsonEq(t, "tool_choice", result["tool_choice"].(map[string]any)["function"].(map[string]any)["name"], "apply_patch")
	toolCalls := messages(t, result)[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "custom call arguments", toolCalls[0].(map[string]any)["function"].(map[string]any)["arguments"],
		`{"input":"*** Begin Patch\n*** End Patch"}`)
}

func TestResponsesToChatPreservesCustomToolMetadataInDescription(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tools": [{
			"type": "custom",
			"name": "apply_patch",
			"description": "Use the apply_patch tool to edit files.",
			"format": {"type": "grammar", "syntax": "lark", "definition": "start: begin_patch hunk+ end_patch"}
		}],
		"input": "hi"
	}`)
	description, _ := result["tools"].([]any)[0].(map[string]any)["function"].(map[string]any)["description"].(string)
	if !strings.HasPrefix(description, "Original tool definition:") {
		t.Fatalf("description should embed the original definition: %q", description)
	}
	if !strings.Contains(description, `"type":"custom"`) || !strings.Contains(description, `"format":`) || !strings.Contains(description, `"syntax":"lark"`) {
		t.Fatalf("description lost custom tool metadata: %q", description)
	}
}

func TestChatResponseToResponsesExtractsReasoningDetails(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_minimax",
		"object": "chat.completion",
		"created": 123,
		"model": "MiniMax-M2.7",
		"choices": [{
			"message": {
				"role": "assistant",
				"reasoning_details": [{"type": "reasoning_text", "text": "Need to inspect the code."}],
				"content": "Done"
			},
			"finish_reason": "stop"
		}]
	}`)
	output := result["output"].([]any)
	jsonEq(t, "output[0].type", output[0].(map[string]any)["type"], "reasoning")
	jsonEq(t, "reasoning text", output[0].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"], "Need to inspect the code.")
	jsonEq(t, "message text", output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"], "Done")
}

func TestResponsesToChatNormalizesCodexInternalRoles(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Follow project instructions."}]},
			{"type": "message", "role": "latest_reminder", "content": "Keep the reply brief."},
			{"type": "message", "role": "unknown_codex_role", "content": "Fallback content."}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "developer role", list[0].(map[string]any)["role"], "system")
	jsonEq(t, "developer content", list[0].(map[string]any)["content"], "Follow project instructions.")
	jsonEq(t, "latest_reminder role", list[1].(map[string]any)["role"], "user")
	jsonEq(t, "unknown role", list[2].(map[string]any)["role"], "user")
}

func TestResponsesToChatMergesMidStreamSystemIntoHead(t *testing.T) {
	result := convertRequest(t, `{
		"model": "MiniMax-M2.7",
		"instructions": "You are Codex.",
		"input": [
			{"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Permissions block"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "AGENTS.md"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "你好"}]},
			{"type": "message", "role": "developer", "content": [{"type": "input_text", "text": "Collaboration Mode: Default"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "你好"}]},
			{"type": "message", "role": "user", "content": [{"type": "input_text", "text": "你好"}]}
		]
	}`)
	list := messages(t, result)
	for index, raw := range list {
		role, _ := raw.(map[string]any)["role"].(string)
		if index == 0 {
			if role != "system" {
				t.Fatalf("first message must be system, got %q", role)
			}
			continue
		}
		if role == "system" {
			t.Fatalf("no system role allowed past index 0 (got at %d)", index)
		}
	}
	head, _ := list[0].(map[string]any)["content"].(string)
	for _, want := range []string{"You are Codex.", "Permissions block", "Collaboration Mode: Default"} {
		if !strings.Contains(head, want) {
			t.Fatalf("head system message missing %q: %q", want, head)
		}
	}
}

func TestCollapseSystemMessagesPreservesNonSystemOrder(t *testing.T) {
	out := collapseSystemMessagesToHead([]any{
		map[string]any{"role": "system", "content": "S1"},
		map[string]any{"role": "user", "content": "U1"},
		map[string]any{"role": "assistant", "content": "A1"},
		map[string]any{"role": "system", "content": "S2"},
		map[string]any{"role": "user", "content": "U2"},
	})
	if len(out) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(out))
	}
	jsonEq(t, "head", out[0], map[string]any{"role": "system", "content": "S1\n\nS2"})
	jsonEq(t, "user order kept", out[1].(map[string]any)["content"], "U1")
	jsonEq(t, "assistant kept", out[2].(map[string]any)["content"], "A1")
	jsonEq(t, "trailing user kept", out[3].(map[string]any)["content"], "U2")
}

func TestResponsesToChatPassesReasoningContentBackToAssistantMessage(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "Need to inspect the repo."}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "I will check the files."}]},
			{"type": "message", "role": "user", "content": "Continue"}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "assistant content", list[0].(map[string]any)["content"], "I will check the files.")
	jsonEq(t, "reasoning", list[0].(map[string]any)["reasoning_content"], "Need to inspect the repo.")
	if _, exists := list[1].(map[string]any)["reasoning_content"]; exists {
		t.Fatalf("user message must not carry reasoning_content")
	}
}

func TestResponsesToChatAttachesTrailingReasoningToPreviousAssistant(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "message", "role": "assistant", "content": "I checked the files."},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "The answer came from README."}]},
			{"type": "message", "role": "user", "content": "Continue"}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "content", list[0].(map[string]any)["content"], "I checked the files.")
	jsonEq(t, "reasoning", list[0].(map[string]any)["reasoning_content"], "The answer came from README.")
	if _, exists := list[1].(map[string]any)["reasoning_content"]; exists {
		t.Fatalf("user message must not carry reasoning_content")
	}
}

func TestResponsesToChatKeepsEmbeddedAssistantReasoning(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [{
			"type": "message",
			"role": "assistant",
			"reasoning_content": "I need to preserve thinking history.",
			"content": "Done."
		}]
	}`)
	list := messages(t, result)
	jsonEq(t, "content", list[0].(map[string]any)["content"], "Done.")
	jsonEq(t, "reasoning", list[0].(map[string]any)["reasoning_content"], "I need to preserve thinking history.")
}

func TestResponsesToChatPreservesTrailingReasoningAfterEmbeddedReasoning(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "message", "role": "assistant", "reasoning_content": "Embedded thought.", "content": "Done."},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "Trailing thought."}]},
			{"type": "message", "role": "user", "content": "Continue"}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "merged reasoning", list[0].(map[string]any)["reasoning_content"], "Embedded thought.\n\nTrailing thought.")
}

func TestResponsesToChatAttachesReasoningToToolCallMessage(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "reasoning", "summary": "Need to read a file."},
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\":\"README.md\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Readme content"}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "reasoning", list[0].(map[string]any)["reasoning_content"], "Need to read a file.")
	toolCalls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "call id", toolCalls[0].(map[string]any)["id"], "call_1")
	if list[1].(map[string]any)["role"] != "tool" {
		t.Fatalf("second message should be a tool result")
	}
}

func TestResponsesToChatRecoversReasoningFromFunctionCallItem(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\":\"README.md\"}", "reasoning_content": "Need to read a file."},
			{"type": "function_call_output", "call_id": "call_1", "output": "Readme content"}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "reasoning", list[0].(map[string]any)["reasoning_content"], "Need to read a file.")
}

func TestResponsesToChatInjectsPlaceholderReasoningForBareToolCall(t *testing.T) {
	result := convertRequest(t, `{
		"model": "kimi-k2-thinking",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\":\"README.md\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Readme content"}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "placeholder reasoning", list[0].(map[string]any)["reasoning_content"], "tool call")
}

func TestResponsesToChatAttachesTrailingReasoningToToolCallMessage(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\":\"README.md\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Readme content"},
			{"type": "reasoning", "summary": "Need to read a file."}
		]
	}`)
	list := messages(t, result)
	jsonEq(t, "reasoning", list[0].(map[string]any)["reasoning_content"], "Need to read a file.")
	if list[1].(map[string]any)["role"] != "tool" {
		t.Fatalf("tool message order changed")
	}
}

func TestResponsesToChatAttachesReasoningForwardToFollowingAssistant(t *testing.T) {
	result := convertRequest(t, `{
		"model": "kimi-k2-thinking",
		"input": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "first thought"}]},
			{"type": "message", "role": "assistant", "content": "First answer."},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "second thought"}]},
			{"type": "message", "role": "assistant", "content": "Second answer."},
			{"role": "user", "content": "Continue"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "assistant", "user")
	jsonEq(t, "first content", list[0].(map[string]any)["content"], "First answer.")
	jsonEq(t, "first reasoning", list[0].(map[string]any)["reasoning_content"], "first thought")
	jsonEq(t, "second content", list[1].(map[string]any)["content"], "Second answer.")
	jsonEq(t, "second reasoning", list[1].(map[string]any)["reasoning_content"], "second thought")
}

func TestResponsesToChatKeepsReasoningOnFinalAnswerAfterToolCall(t *testing.T) {
	result := convertRequest(t, `{
		"model": "kimi-k2-thinking",
		"input": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "need to read a file"}]},
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\":\"README.md\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Readme content"},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "now I can answer"}]},
			{"type": "message", "role": "assistant", "content": "The file says hello."},
			{"role": "user", "content": "Continue"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "tool", "assistant", "user")
	toolCalls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "call id", toolCalls[0].(map[string]any)["id"], "call_1")
	jsonEq(t, "call reasoning", list[0].(map[string]any)["reasoning_content"], "need to read a file")
	jsonEq(t, "final content", list[2].(map[string]any)["content"], "The file says hello.")
	jsonEq(t, "final reasoning", list[2].(map[string]any)["reasoning_content"], "now I can answer")
}

func TestResponsesToChatKeepsMultipleToolCallsAdjacentToOutputs(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "read_file", "arguments": "{\"path\":\"README.md\"}"},
			{"type": "function_call", "call_id": "call_2", "name": "list_files", "arguments": "{\"path\":\"src\"}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Readme content"},
			{"type": "function_call_output", "call_id": "call_2", "output": ["main.rs", "lib.rs"]},
			{"role": "user", "content": "Continue"}
		]
	}`)
	list := messages(t, result)
	if len(list) != 4 {
		t.Fatalf("expected 4 messages, got %d", len(list))
	}
	rolesEq(t, result, "assistant", "tool", "tool", "user")
	toolCalls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "first call", toolCalls[0].(map[string]any)["id"], "call_1")
	jsonEq(t, "second call", toolCalls[1].(map[string]any)["id"], "call_2")
	jsonEq(t, "structured output stays canonical JSON", list[2].(map[string]any)["content"], `["main.rs","lib.rs"]`)
}

func TestResponsesToChatCanonicalizesJSONStringToolPayloads(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{ \"b\": 2, \"a\": 1 }"},
			{"type": "function_call_output", "call_id": "call_1", "output": "{ \"z\": true, \"a\": [2, 1] }"}
		]
	}`)
	list := messages(t, result)
	toolCalls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "arguments", toolCalls[0].(map[string]any)["function"].(map[string]any)["arguments"], `{"a":1,"b":2}`)
	jsonEq(t, "tool output", list[1].(map[string]any)["content"], `{"a":[2,1],"z":true}`)
}

func TestResponsesToChatPreservesPlainTextToolOutput(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "not json"},
			{"type": "function_call_output", "call_id": "call_1", "output": "plain text result"}
		]
	}`)
	list := messages(t, result)
	toolCalls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "arguments preserved verbatim", toolCalls[0].(map[string]any)["function"].(map[string]any)["arguments"], "not json")
	jsonEq(t, "tool output", list[1].(map[string]any)["content"], "plain text result")
}

func TestResponsesToChatKeepsNoMediaToolOutputBytesStable(t *testing.T) {
	cases := []struct{ output, want string }{
		{`"plain text"`, "plain text"},
		{`"{ \"z\": true, \"a\": [2, 1] }"`, `{"a":[2,1],"z":true}`},
		{`["main.rs", "lib.rs"]`, `["main.rs","lib.rs"]`},
		{`{"z": true, "a": [2, 1]}`, `{"a":[2,1],"z":true}`},
		{`[]`, `[]`},
	}
	for index, testCase := range cases {
		callID := fmt.Sprintf("call_stable_%d", index)
		result := convertRequest(t, fmt.Sprintf(`{
			"model": "gpt-5.4",
			"input": [
				{"type": "function_call", "call_id": %q, "name": "lookup", "arguments": "{}"},
				{"type": "function_call_output", "call_id": %q, "output": %s}
			]
		}`, callID, callID, testCase.output))
		list := messages(t, result)
		rolesEq(t, result, "assistant", "tool")
		jsonEq(t, "tool output", list[1].(map[string]any)["content"], testCase.want)
	}
	// A missing output field serializes as empty content.
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "function_call", "call_id": "call_missing", "name": "lookup", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_missing"}
		]
	}`)
	jsonEq(t, "missing output", messages(t, result)[1].(map[string]any)["content"], "")
}

func TestResponsesToChatPreservesLegacyUnknownItemBatchBoundary(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "function_call", "call_id": "call_1", "name": "lookup", "arguments": "{}"},
			{"type": "future_metadata", "value": 1},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "second batch reasoning"}]},
			{"type": "function_call", "call_id": "call_2", "name": "lookup", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_1", "output": "first result"},
			{"type": "function_call_output", "call_id": "call_2", "output": "second result"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "assistant", "tool", "tool")
	firstCalls := list[0].(map[string]any)["tool_calls"].([]any)
	secondCalls := list[1].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "first batch", firstCalls[0].(map[string]any)["id"], "call_1")
	jsonEq(t, "first batch placeholder", list[0].(map[string]any)["reasoning_content"], "tool call")
	jsonEq(t, "second batch", secondCalls[0].(map[string]any)["id"], "call_2")
	jsonEq(t, "second batch reasoning", list[1].(map[string]any)["reasoning_content"], "second batch reasoning")
}

func TestResponsesToChatCoalescesAdjacentCommentaryWithToolCalls(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "need to update the file"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Part 1 written. Appending sections 4-5."}]},
			{"type": "function_call", "call_id": "call_1", "name": "write_file", "arguments": "{}", "reasoning_content": "need to update the file"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Success"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "tool")
	jsonEq(t, "commentary", list[0].(map[string]any)["content"], "Part 1 written. Appending sections 4-5.")
	calls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "call id", calls[0].(map[string]any)["id"], "call_1")
	jsonEq(t, "reasoning deduped", list[0].(map[string]any)["reasoning_content"], "need to update the file")
	jsonEq(t, "tool_call_id", list[1].(map[string]any)["tool_call_id"], "call_1")
}

func TestResponsesToChatDeduplicatesRepeatedCallReasoningSegments(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "need to update the file"}]},
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Part 1 written."}]},
			{"type": "function_call", "call_id": "call_1", "name": "write_file", "arguments": "{}", "reasoning_content": "need to update the file"},
			{"type": "function_call", "call_id": "call_2", "name": "write_file", "arguments": "{}", "reasoning_content": "second section planning"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Success 1"},
			{"type": "function_call_output", "call_id": "call_2", "output": "Success 2"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "tool", "tool")
	jsonEq(t, "merged reasoning", list[0].(map[string]any)["reasoning_content"],
		"need to update the file\n\nsecond section planning")
}

func TestResponsesToChatKeepsUserBoundaryBeforeToolCallTurn(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Done for now."}]},
			{"type": "message", "role": "user", "content": "Continue with the next file."},
			{"type": "function_call", "call_id": "call_next", "name": "read_file", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_next", "output": "Next result"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "user", "assistant", "tool")
	if _, exists := list[0].(map[string]any)["tool_calls"]; exists {
		t.Fatalf("first assistant message must not carry tool calls")
	}
	if content, exists := list[2].(map[string]any)["content"]; !exists || content != nil {
		t.Fatalf("tool-call assistant message must have null content, got %v", content)
	}
}

func TestResponsesToChatCoalescesWhenReasoningSitsBetweenCommentaryAndCall(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Part 1 written. Appending sections 4-5."}]},
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "need to update the file"}]},
			{"type": "function_call", "call_id": "call_1", "name": "write_file", "arguments": "{}", "reasoning_content": "need to update the file"},
			{"type": "function_call_output", "call_id": "call_1", "output": "Success"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "tool")
	jsonEq(t, "commentary", list[0].(map[string]any)["content"], "Part 1 written. Appending sections 4-5.")
	jsonEq(t, "reasoning", list[0].(map[string]any)["reasoning_content"], "need to update the file")
}

func TestResponsesToChatBackfillsReasoningPlaceholderForCoalescedCall(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"input": [
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Running the build now."}]},
			{"type": "function_call", "call_id": "call_build", "name": "build", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_build", "output": "Build succeeded"}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "tool")
	jsonEq(t, "commentary", list[0].(map[string]any)["content"], "Running the build now.")
	jsonEq(t, "placeholder", list[0].(map[string]any)["reasoning_content"], "tool call")
}

func TestResponsesToChatCoalescesCustomToolCallWithCommentary(t *testing.T) {
	result := convertRequest(t, `{
		"model": "kimi-k3",
		"tools": [{"type": "custom", "name": "apply_patch", "description": "Apply a patch to files."}],
		"input": [
			{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Applying the next patch."}]},
			{"type": "custom_tool_call", "id": "ctc_1", "call_id": "call_patch", "name": "apply_patch", "input": "*** Begin Patch\n*** End Patch"},
			{"type": "custom_tool_call_output", "call_id": "call_patch", "output": {"text": "Success"}}
		]
	}`)
	list := messages(t, result)
	rolesEq(t, result, "assistant", "tool")
	jsonEq(t, "commentary", list[0].(map[string]any)["content"], "Applying the next patch.")
	calls := list[0].(map[string]any)["tool_calls"].([]any)
	jsonEq(t, "call id", calls[0].(map[string]any)["id"], "call_patch")
	jsonEq(t, "tool_call_id", list[1].(map[string]any)["tool_call_id"], "call_patch")
}

func TestResponsesToChatMultiRoundHistoryHasNoTextOnlyAssistantTurns(t *testing.T) {
	var input []string
	input = append(input, `{"type": "message", "role": "user", "content": "Fix the failing build."}`)
	for index := 1; index <= 3; index++ {
		input = append(input, fmt.Sprintf(`{"type": "reasoning", "summary": [{"type": "summary_text", "text": "round %d reasoning"}]}`, index))
		input = append(input, fmt.Sprintf(`{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Step %d: checking the logs."}]}`, index))
		input = append(input, fmt.Sprintf(`{"type": "function_call", "call_id": "call_round_%d", "name": "read_logs", "arguments": "{}", "reasoning_content": "round %d reasoning"}`, index, index))
		input = append(input, fmt.Sprintf(`{"type": "function_call_output", "call_id": "call_round_%d", "output": "round %d result"}`, index, index))
	}
	input = append(input, `{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Build fixed."}]}`)
	result := convertRequest(t, fmt.Sprintf(`{"model": "kimi-k2-thinking", "input": [%s]}`, strings.Join(input, ",")))

	list := messages(t, result)
	rolesEq(t, result, "user", "assistant", "tool", "assistant", "tool", "assistant", "tool", "assistant")
	for index := 0; index+1 < len(list); index++ {
		message := list[index].(map[string]any)
		if message["role"] != "assistant" {
			continue
		}
		if calls, ok := message["tool_calls"].([]any); !ok || len(calls) == 0 {
			t.Fatalf("assistant at index %d must carry tool calls", index)
		}
	}
	jsonEq(t, "step content", list[1].(map[string]any)["content"], "Step 1: checking the logs.")
	jsonEq(t, "step reasoning", list[1].(map[string]any)["reasoning_content"], "round 1 reasoning")
	jsonEq(t, "final content", list[7].(map[string]any)["content"], "Build fixed.")
	if _, exists := list[7].(map[string]any)["tool_calls"]; exists {
		t.Fatalf("final assistant message must not carry tool calls")
	}
}

func TestResponsesToChatConversionIsDeterministic(t *testing.T) {
	literal := `{
		"model": "kimi-k3",
		"input": [
			{"type": "function_call", "call_id": "call_repeat", "name": "lookup", "arguments": "{}"},
			{"type": "function_call_output", "call_id": "call_repeat", "output": {"content": [{"type": "input_text", "text": "answer"}, {"type": "text", "text": "more"}]}}
		]
	}`
	first, err := ResponsesToChat([]byte(literal))
	if err != nil {
		t.Fatal(err)
	}
	second, err := ResponsesToChat([]byte(literal))
	if err != nil {
		t.Fatal(err)
	}
	if string(first) != string(second) {
		t.Fatalf("conversion must be deterministic:\n%s\n%s", first, second)
	}
}

func TestResponsesToChatRejectsMissingModelOrInput(t *testing.T) {
	if _, err := ResponsesToChat([]byte(`{"input": "hi"}`)); err == nil {
		t.Fatalf("missing model must error")
	}
	if _, err := ResponsesToChat([]byte(`{"model": "m"}`)); err == nil {
		t.Fatalf("missing input must error")
	}
}

// ---------------------------------------------------------------------------
// Tool choice / tools guard (cc-switch issue #3557)
// ---------------------------------------------------------------------------

func TestResponsesToChatDropsToolChoiceWhenNoTools(t *testing.T) {
	result := convertRequest(t, `{"model": "qwen3-7-max", "tool_choice": "auto", "input": "hi"}`)
	if _, exists := result["tool_choice"]; exists {
		t.Fatalf("tool_choice should be dropped when tools is absent")
	}
	if _, exists := result["tools"]; exists {
		t.Fatalf("tools should be absent")
	}
}

func TestResponsesToChatDropsToolChoiceWhenToolsEmptyArray(t *testing.T) {
	result := convertRequest(t, `{"model": "gpt-5.4", "tools": [], "tool_choice": "auto", "input": "hi"}`)
	if _, exists := result["tool_choice"]; exists {
		t.Fatalf("tool_choice should be dropped when tools is empty")
	}
	if _, exists := result["tools"]; exists {
		t.Fatalf("tools should be absent when input tools was empty")
	}
}

func TestResponsesToChatDropsParallelToolCallsWhenNoTools(t *testing.T) {
	result := convertRequest(t, `{"model": "gpt-5.4", "tool_choice": "auto", "parallel_tool_calls": true, "input": "hi"}`)
	if _, exists := result["tool_choice"]; exists {
		t.Fatalf("tool_choice should be dropped")
	}
	if _, exists := result["parallel_tool_calls"]; exists {
		t.Fatalf("parallel_tool_calls should be dropped")
	}
}

func TestResponsesToChatDropsToolChoiceWhenAllToolsFiltered(t *testing.T) {
	result := convertRequest(t, `{"model": "gpt-5.4", "tools": [{"type": "function"}], "tool_choice": "auto", "input": "hi"}`)
	if _, exists := result["tool_choice"]; exists {
		t.Fatalf("tool_choice should be dropped when all tools filtered")
	}
	if _, exists := result["tools"]; exists {
		t.Fatalf("tools should be absent when all filtered")
	}
}

func TestResponsesToChatKeepsToolChoiceWhenToolsPresent(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tools": [{"type": "function", "name": "get_weather", "description": "Get weather", "parameters": {"type": "object"}}],
		"tool_choice": "auto",
		"parallel_tool_calls": true,
		"input": "hi"
	}`)
	if result["tool_choice"] != "auto" {
		t.Fatalf("tool_choice should be kept, got %v", result["tool_choice"])
	}
	if result["parallel_tool_calls"] != true {
		t.Fatalf("parallel_tool_calls should be kept")
	}
	tools := result["tools"].([]any)
	jsonEq(t, "tool name", tools[0].(map[string]any)["function"].(map[string]any)["name"], "get_weather")
}

func TestResponsesToChatKeepsToolChoiceFunctionWhenToolsPresent(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tools": [{"type": "function", "name": "get_weather", "description": "Get weather", "parameters": {"type": "object"}}],
		"tool_choice": {"type": "function", "name": "get_weather"},
		"input": "hi"
	}`)
	toolChoice := result["tool_choice"].(map[string]any)
	jsonEq(t, "type", toolChoice["type"], "function")
	jsonEq(t, "name", toolChoice["function"].(map[string]any)["name"], "get_weather")
}

func TestResponsesToChatNoToolChoiceNoToolsStaysClean(t *testing.T) {
	result := convertRequest(t, `{"model": "gpt-5.4", "input": "hi"}`)
	for _, key := range []string{"tool_choice", "tools", "parallel_tool_calls"} {
		if _, exists := result[key]; exists {
			t.Fatalf("%s should be absent", key)
		}
	}
}

func TestResponsesToChatToolChoiceNoneDroppedWhenNoTools(t *testing.T) {
	result := convertRequest(t, `{"model": "gpt-5.4", "tool_choice": "none", "input": "hi"}`)
	if _, exists := result["tool_choice"]; exists {
		t.Fatalf("tool_choice none should be dropped when no tools")
	}
}

func TestResponsesToChatToolSearchOutputProvidesToolsKeepsToolChoice(t *testing.T) {
	result := convertRequest(t, `{
		"model": "gpt-5.4",
		"tool_choice": "auto",
		"input": [{
			"type": "tool_search_output",
			"call_id": "call_ts_1",
			"status": "completed",
			"execution": "client",
			"tools": [{
				"type": "function",
				"name": "search_docs",
				"description": "Search documentation.",
				"parameters": {"type": "object", "properties": {"query": {"type": "string"}}}
			}]
		}]
	}`)
	if result["tool_choice"] != "auto" {
		t.Fatalf("tool_choice should be kept when tool_search_output provides tools")
	}
	tools := result["tools"].([]any)
	jsonEq(t, "loaded tool", tools[0].(map[string]any)["function"].(map[string]any)["name"], "search_docs")
}

// ---------------------------------------------------------------------------
// Usage mapping
// ---------------------------------------------------------------------------

func TestChatUsageToResponsesIncludesRequiredInputTokenDetails(t *testing.T) {
	usage := chatUsageToResponsesUsage(parse(t, `{
		"prompt_tokens": 13,
		"completion_tokens": 245,
		"total_tokens": 258,
		"prompt_tokens_details": {"cached_tokens": 0}
	}`))
	jsonEq(t, "input_tokens_details", usage["input_tokens_details"], map[string]any{"cached_tokens": 0})

	fallback := chatUsageToResponsesUsage(nil)
	jsonEq(t, "fallback details", fallback["input_tokens_details"], map[string]any{"cached_tokens": 0})
}

func TestChatUsageToResponsesResolvesCacheReadPrecedence(t *testing.T) {
	direct := chatUsageToResponsesUsage(parse(t, `{
		"prompt_tokens": 100,
		"completion_tokens": 5,
		"prompt_tokens_details": {"cached_tokens": 0},
		"cache_read_input_tokens": 40
	}`))
	jsonEq(t, "direct cache read", direct["input_tokens_details"].(map[string]any)["cached_tokens"], float64(40))
	jsonEq(t, "echoed cache read", direct["cache_read_input_tokens"], float64(40))

	directZero := chatUsageToResponsesUsage(parse(t, `{
		"prompt_tokens_details": {"cached_tokens": 12},
		"cache_read_input_tokens": 0
	}`))
	jsonEq(t, "explicit zero wins", directZero["input_tokens_details"].(map[string]any)["cached_tokens"], float64(0))
	jsonEq(t, "echoed zero", directZero["cache_read_input_tokens"], float64(0))

	invalidDirect := chatUsageToResponsesUsage(parse(t, `{
		"prompt_tokens_details": {"cached_tokens": 12},
		"cache_read_input_tokens": "invalid"
	}`))
	jsonEq(t, "fallback to details", invalidDirect["input_tokens_details"].(map[string]any)["cached_tokens"], float64(12))
	if _, exists := invalidDirect["cache_read_input_tokens"]; exists {
		t.Fatalf("invalid direct cache read must not be echoed")
	}

	invalidPromptDetails := chatUsageToResponsesUsage(parse(t, `{
		"prompt_tokens_details": {"cached_tokens": "invalid"},
		"input_tokens_details": {"cached_tokens": 7}
	}`))
	jsonEq(t, "fallback to input details", invalidPromptDetails["input_tokens_details"].(map[string]any)["cached_tokens"], float64(7))
}

func TestChatUsageToResponsesMapsDeepseekCacheHitTokens(t *testing.T) {
	usage := chatUsageToResponsesUsage(parse(t, `{
		"prompt_tokens": 1000,
		"completion_tokens": 100,
		"total_tokens": 1100,
		"prompt_cache_hit_tokens": 600,
		"prompt_cache_miss_tokens": 400
	}`))
	jsonEq(t, "deepseek cache hit", usage["input_tokens_details"].(map[string]any)["cached_tokens"], float64(600))
}

// ---------------------------------------------------------------------------
// Response conversion
// ---------------------------------------------------------------------------

func TestChatResponseToResponsesMapsTextToolCallsAndUsage(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_1",
		"object": "chat.completion",
		"created": 123,
		"model": "gpt-5.4",
		"choices": [{
			"message": {
				"role": "assistant",
				"reasoning_content": "I should check the weather before answering.",
				"content": "Let me check.",
				"tool_calls": [{
					"id": "call_1",
					"type": "function",
					"function": {"name": "get_weather", "arguments": "{\"city\":\"Tokyo\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 5,
			"total_tokens": 15,
			"prompt_tokens_details": {"cached_tokens": 3, "cache_write_tokens": 2}
		}
	}`)
	if result["id"] != "resp_chatcmpl_1" {
		t.Fatalf("response id: %v", result["id"])
	}
	jsonEq(t, "status", result["status"], "completed")
	output := result["output"].([]any)
	jsonEq(t, "output[0].type", output[0].(map[string]any)["type"], "reasoning")
	jsonEq(t, "reasoning text", output[0].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"], "I should check the weather before answering.")
	jsonEq(t, "output[1].type", output[1].(map[string]any)["type"], "message")
	jsonEq(t, "message text", output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"], "Let me check.")
	jsonEq(t, "output[2].type", output[2].(map[string]any)["type"], "function_call")
	jsonEq(t, "call id", output[2].(map[string]any)["call_id"], "call_1")
	jsonEq(t, "call reasoning", output[2].(map[string]any)["reasoning_content"], "I should check the weather before answering.")
	usage := result["usage"].(map[string]any)
	jsonEq(t, "input tokens", usage["input_tokens"], float64(10))
	jsonEq(t, "output tokens", usage["output_tokens"], float64(5))
	details := usage["input_tokens_details"].(map[string]any)
	jsonEq(t, "cached tokens", details["cached_tokens"], float64(3))
	jsonEq(t, "cache write tokens", details["cache_write_tokens"], float64(2))
}

func buildToolContext(t *testing.T, requestLiteral string) *ToolContext {
	t.Helper()
	return NewToolContextFromRequestBytes([]byte(requestLiteral))
}

func convertResponseWithContext(t *testing.T, literal string, context *ToolContext) map[string]any {
	t.Helper()
	data, err := ChatResponseToResponsesWithContext([]byte(literal), context)
	if err != nil {
		t.Fatalf("ChatResponseToResponsesWithContext: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("decode converted response: %v", err)
	}
	return out
}

func TestChatResponseToResponsesRestoresLoadedNamespaceToolCall(t *testing.T) {
	context := buildToolContext(t, `{
		"model": "gpt-5.4",
		"tools": [{"type": "tool_search"}],
		"input": [{
			"type": "tool_search_output",
			"call_id": "call_tool_search_1",
			"status": "completed",
			"execution": "client",
			"tools": [{
				"type": "namespace",
				"name": "mcp__codex_apps__gmail",
				"description": "Find and reference emails from your inbox.",
				"tools": [{
					"type": "function",
					"name": "_search_emails",
					"description": "Search Gmail for emails matching a query.",
					"parameters": {"type": "object", "properties": {"query": {"type": "string"}}}
				}]
			}]
		}]
	}`)
	result := convertResponseWithContext(t, `{
		"id": "chatcmpl_gmail",
		"model": "gpt-5.4",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_gmail",
					"type": "function",
					"function": {
						"name": "mcp__codex_apps__gmail___search_emails",
						"arguments": "{\"query\":\"-in:spam\",\"max_results\":5}"
					}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`, context)
	output := result["output"].([]any)
	jsonEq(t, "type", output[0].(map[string]any)["type"], "function_call")
	jsonEq(t, "call id", output[0].(map[string]any)["call_id"], "call_gmail")
	jsonEq(t, "namespace", output[0].(map[string]any)["namespace"], "mcp__codex_apps__gmail")
	jsonEq(t, "name", output[0].(map[string]any)["name"], "_search_emails")
	jsonEq(t, "arguments", output[0].(map[string]any)["arguments"], `{"max_results":5,"query":"-in:spam"}`)
}

func TestChatResponseToResponsesRestoresToolSearchCall(t *testing.T) {
	context := buildToolContext(t, `{"model": "gpt-5.4", "tools": [{"type": "tool_search"}], "input": "Find tools."}`)
	result := convertResponseWithContext(t, `{
		"id": "chatcmpl_tool_search",
		"model": "gpt-5.4",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_tool_search_1",
					"type": "function",
					"function": {"name": "tool_search", "arguments": "{\"query\":\"Gmail search emails\",\"limit\":10}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`, context)
	item := result["output"].([]any)[0].(map[string]any)
	jsonEq(t, "type", item["type"], "tool_search_call")
	jsonEq(t, "call id", item["call_id"], "call_tool_search_1")
	jsonEq(t, "execution", item["execution"], "client")
	jsonEq(t, "query", item["arguments"].(map[string]any)["query"], "Gmail search emails")
	jsonEq(t, "limit", item["arguments"].(map[string]any)["limit"], float64(10))
}

func TestChatResponseToResponsesRestoresCustomToolCall(t *testing.T) {
	context := buildToolContext(t, `{"model": "gpt-5.4", "tools": [{"type": "custom", "name": "apply_patch"}], "input": "Patch it."}`)
	result := convertResponseWithContext(t, `{
		"id": "chatcmpl_custom",
		"model": "gpt-5.4",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{
					"id": "call_patch",
					"type": "function",
					"function": {"name": "apply_patch", "arguments": "{\"input\":\"*** Begin Patch\\n*** End Patch\"}"}
				}]
			},
			"finish_reason": "tool_calls"
		}]
	}`, context)
	item := result["output"].([]any)[0].(map[string]any)
	jsonEq(t, "type", item["type"], "custom_tool_call")
	jsonEq(t, "item id", item["id"], "ctc_call_patch")
	jsonEq(t, "call id", item["call_id"], "call_patch")
	jsonEq(t, "name", item["name"], "apply_patch")
	jsonEq(t, "input", item["input"], "*** Begin Patch\n*** End Patch")
}

func TestChatResponseWithOnlyUnnamedToolCallIsAnError(t *testing.T) {
	_, err := ChatResponseToResponses([]byte(`{
		"id": "chatcmpl_drop",
		"model": "kimi-k3",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "让我继续处理这个文件",
				"tool_calls": [{"id": "call_bad", "type": "function", "function": {"arguments": "{}"}}]
			},
			"finish_reason": "tool_calls"
		}]
	}`))
	if err == nil || !strings.Contains(err.Error(), "without a function name") {
		t.Fatalf("expected dropped tool call error, got %v", err)
	}
}

func TestChatResponseKeepsValidToolCallBesideUnnamedOne(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_mixed",
		"model": "kimi-k3",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [
					{"id": "call_bad", "type": "function", "function": {"arguments": "{}"}},
					{"id": "call_good", "type": "function", "function": {"name": "exec_command", "arguments": "{\"cmd\":\"ls\"}"}}
				]
			},
			"finish_reason": "tool_calls"
		}]
	}`)
	output := result["output"].([]any)
	if len(output) != 1 {
		t.Fatalf("expected one surviving tool call, got %d", len(output))
	}
	jsonEq(t, "name", output[0].(map[string]any)["name"], "exec_command")
	jsonEq(t, "call id", output[0].(map[string]any)["call_id"], "call_good")
	jsonEq(t, "status", result["status"], "completed")
}

func TestChatResponseWithUnnamedLegacyFunctionCallIsAnError(t *testing.T) {
	_, err := ChatResponseToResponses([]byte(`{
		"id": "chatcmpl_legacy",
		"model": "kimi-k3",
		"choices": [{
			"message": {"role": "assistant", "function_call": {"id": "call_legacy", "arguments": "{}"}},
			"finish_reason": "function_call"
		}]
	}`))
	if err == nil {
		t.Fatalf("unnamed legacy function_call must error")
	}
}

func TestChatResponseTruncatedStaysIncompleteInsteadOfError(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_trunc",
		"model": "kimi-k3",
		"choices": [{
			"message": {
				"role": "assistant",
				"content": "我来看看",
				"tool_calls": [{"id": "call_cut", "type": "function", "function": {"arguments": "{\"pa"}}]
			},
			"finish_reason": "length"
		}]
	}`)
	jsonEq(t, "status", result["status"], "incomplete")
	jsonEq(t, "incomplete reason", result["incomplete_details"].(map[string]any)["reason"], "max_output_tokens")
}

func TestChatResponseWhitespaceOnlyToolNameIsAnError(t *testing.T) {
	_, err := ChatResponseToResponses([]byte(`{
		"id": "chatcmpl_ws",
		"model": "kimi-k3",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{"id": "call_ws", "type": "function", "function": {"name": "   ", "arguments": "{}"}}]
			},
			"finish_reason": "tool_calls"
		}]
	}`))
	if err == nil {
		t.Fatalf("whitespace-only tool name must error when nothing survives")
	}
}

func TestChatResponseTextOnlyStillCompletes(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_text",
		"model": "kimi-k3",
		"choices": [{
			"message": {"role": "assistant", "content": "完成了"},
			"finish_reason": "stop"
		}]
	}`)
	jsonEq(t, "status", result["status"], "completed")
}

func TestChatResponseToResponsesCanonicalizesJSONStringToolArguments(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_args",
		"model": "gpt-5.4",
		"choices": [{
			"message": {
				"role": "assistant",
				"tool_calls": [{"id": "call_1", "type": "function", "function": {"name": "lookup", "arguments": "{ \"b\": 2, \"a\": 1 }"}}]
			},
			"finish_reason": "tool_calls"
		}]
	}`)
	item := result["output"].([]any)[0].(map[string]any)
	jsonEq(t, "type", item["type"], "function_call")
	jsonEq(t, "arguments", item["arguments"], `{"a":1,"b":2}`)
}

func TestChatResponseToResponsesSplitsInlineThinkContent(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_think",
		"model": "MiniMax-M2.7",
		"choices": [{
			"message": {"role": "assistant", "content": "<think>\nI should answer with pong.\n</think>\n\npong"},
			"finish_reason": "stop"
		}],
		"usage": {
			"prompt_tokens": 10,
			"completion_tokens": 20,
			"total_tokens": 30,
			"completion_tokens_details": {"reasoning_tokens": 18}
		}
	}`)
	output := result["output"].([]any)
	jsonEq(t, "output[0].type", output[0].(map[string]any)["type"], "reasoning")
	jsonEq(t, "reasoning text", output[0].(map[string]any)["summary"].([]any)[0].(map[string]any)["text"], "I should answer with pong.")
	jsonEq(t, "message text", output[1].(map[string]any)["content"].([]any)[0].(map[string]any)["text"], "pong")
	jsonEq(t, "reasoning tokens", result["usage"].(map[string]any)["output_tokens_details"].(map[string]any)["reasoning_tokens"], float64(18))
}

func TestChatResponseLengthMapsToIncompleteResponse(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_2",
		"model": "gpt-5.4",
		"choices": [{
			"message": {"role": "assistant", "content": "partial"},
			"finish_reason": "length"
		}]
	}`)
	jsonEq(t, "status", result["status"], "incomplete")
	jsonEq(t, "reason", result["incomplete_details"].(map[string]any)["reason"], "max_output_tokens")
}

// ---------------------------------------------------------------------------
// Error normalization
// ---------------------------------------------------------------------------

func decodeNormalizedError(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode normalized error: %v", err)
	}
	errorObject, ok := out["error"].(map[string]any)
	if !ok {
		t.Fatalf("normalized body must carry an error object: %s", body)
	}
	return errorObject
}

func TestChatErrorToResponseErrorNormalizesStandardOpenAIShape(t *testing.T) {
	errorObject := decodeNormalizedError(t, ChatErrorToResponseError([]byte(`{
		"error": {"message": "Invalid API key", "type": "invalid_request_error", "code": "invalid_api_key", "param": "api_key"}
	}`)))
	jsonEq(t, "message", errorObject["message"], "Invalid API key")
	jsonEq(t, "type", errorObject["type"], "invalid_request_error")
	jsonEq(t, "code", errorObject["code"], "invalid_api_key")
	jsonEq(t, "param", errorObject["param"], "api_key")
}

func TestChatErrorToResponseErrorNormalizesMiniMaxBaseResp(t *testing.T) {
	errorObject := decodeNormalizedError(t, ChatErrorToResponseError([]byte(`{
		"base_resp": {"status_code": 2013, "status_msg": "invalid params, chat content has invalid message role: system"}
	}`)))
	jsonEq(t, "message", errorObject["message"], "invalid params, chat content has invalid message role: system")
	jsonEq(t, "code", errorObject["code"], float64(2013))
	jsonEq(t, "type", errorObject["type"], "upstream_error")
}

func TestChatErrorToResponseErrorHandlesPlainTextBody(t *testing.T) {
	errorObject := decodeNormalizedError(t, ChatErrorToResponseError([]byte("Upstream timeout")))
	jsonEq(t, "message", errorObject["message"], "Upstream timeout")
	jsonEq(t, "type", errorObject["type"], "upstream_error")
	if errorObject["code"] != nil || errorObject["param"] != nil {
		t.Fatalf("code/param must be null")
	}
}

func TestChatErrorToResponseErrorHandlesMissingBody(t *testing.T) {
	errorObject := decodeNormalizedError(t, ChatErrorToResponseError(nil))
	jsonEq(t, "message", errorObject["message"], "Upstream returned an empty error response")
	jsonEq(t, "type", errorObject["type"], "upstream_error")
}

func TestChatErrorToResponseErrorFallsBackToDetailField(t *testing.T) {
	errorObject := decodeNormalizedError(t, ChatErrorToResponseError([]byte(`{"detail": "rate limit exceeded"}`)))
	jsonEq(t, "message", errorObject["message"], "rate limit exceeded")
	jsonEq(t, "type", errorObject["type"], "upstream_error")
}

// content_filter truncates the turn just like length does, and the Responses
// API names a reason for it. Mapping only "length" to incomplete reported a
// filtered turn as completed.
func TestChatResponseContentFilterStaysIncomplete(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_filtered",
		"model": "gpt-5.4",
		"choices": [{
			"message": {"role": "assistant", "content": "I cannot help with"},
			"finish_reason": "content_filter"
		}]
	}`)
	jsonEq(t, "status", result["status"], "incomplete")
	jsonEq(t, "incomplete reason", result["incomplete_details"].(map[string]any)["reason"], "content_filter")
}

// The other direction must not regress: a normal stop is still completed and
// carries no incomplete_details.
func TestChatResponseStopHasNoIncompleteDetails(t *testing.T) {
	result := convertResponse(t, `{
		"id": "chatcmpl_stop",
		"model": "gpt-5.4",
		"choices": [{"message": {"role": "assistant", "content": "done"}, "finish_reason": "stop"}]
	}`)
	jsonEq(t, "status", result["status"], "completed")
	if _, present := result["incomplete_details"]; present {
		t.Fatalf("completed turn must not carry incomplete_details: %v", result["incomplete_details"])
	}
}
