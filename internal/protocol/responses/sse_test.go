package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

// streamChat collects the converted Responses SSE output for a list of Chat
// SSE chunks, mirroring the CC Switch streaming test helper.
func streamChat(t *testing.T, chunks []string) string {
	t.Helper()
	return streamChatWithContext(t, chunks, nil)
}

func streamChatWithContext(t *testing.T, chunks []string, context *ToolContext) string {
	t.Helper()
	converter := NewSSEConverterWithContext(context)
	var out strings.Builder
	for _, chunk := range chunks {
		data, err := converter.Convert([]byte(chunk))
		if err != nil {
			t.Fatalf("Convert: %v", err)
		}
		out.Write(data)
	}
	tail, err := converter.Flush()
	if err != nil {
		t.Fatalf("Flush: %v", err)
	}
	out.Write(tail)
	return out.String()
}

// sseEvents parses every data: payload of an SSE stream into decoded maps.
func sseEvents(t *testing.T, output string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, block := range strings.Split(output, "\n\n") {
		for _, line := range strings.Split(block, "\n") {
			if data, ok := stripSSEField(line, "data"); ok {
				var event map[string]any
				if json.Unmarshal([]byte(data), &event) != nil {
					t.Fatalf("malformed SSE data payload: %q", data)
				}
				events = append(events, event)
			}
		}
	}
	return events
}

func findEvent(events []map[string]any, eventType string) map[string]any {
	for _, event := range events {
		if event["type"] == eventType {
			return event
		}
	}
	return nil
}

func outputItemsOf(t *testing.T, event map[string]any) []map[string]any {
	t.Helper()
	response, ok := event["response"].(map[string]any)
	if !ok {
		t.Fatalf("event has no response object")
	}
	rawItems, ok := response["output"].([]any)
	if !ok {
		t.Fatalf("response has no output array")
	}
	items := make([]map[string]any, 0, len(rawItems))
	for _, raw := range rawItems {
		item, _ := raw.(map[string]any)
		items = append(items, item)
	}
	return items
}

func TestSSEConvertsTextChatToResponses(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_1","created":123,"model":"gpt-5.4","choices":[{"delta":{"content":"Hel"}}]}` + "\n\n",
		`data: {"id":"chatcmpl_1","created":123,"model":"gpt-5.4","choices":[{"delta":{"content":"lo"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6,"prompt_tokens_details":{"cached_tokens":0},"cache_read_input_tokens":2}}` + "\n\n",
		"data: [DONE]\n\n",
	})
	events := sseEvents(t, output)
	if findEvent(events, "response.created") == nil {
		t.Fatalf("missing response.created in:\n%s", output)
	}
	if !strings.Contains(output, "event: response.output_text.delta") {
		t.Fatalf("missing text delta in:\n%s", output)
	}
	if !strings.Contains(output, `"text":"Hello"`) {
		t.Fatalf("missing concatenated text in:\n%s", output)
	}
	completed := findEvent(events, "response.completed")
	if completed == nil {
		t.Fatalf("missing response.completed in:\n%s", output)
	}
	usage := completed["response"].(map[string]any)["usage"].(map[string]any)
	if usage["input_tokens"] != float64(4) {
		t.Fatalf("usage input tokens: %v", usage["input_tokens"])
	}
	jsonEq(t, "cached tokens", usage["input_tokens_details"].(map[string]any)["cached_tokens"], float64(2))
	jsonEq(t, "echoed cache read", usage["cache_read_input_tokens"], float64(2))
}

func TestSSEConvertsReasoningContentToReasoningEvents(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_reason","created":123,"model":"deepseek-reasoner","choices":[{"delta":{"reasoning_content":"Need context. "}}]}` + "\n\n",
		`data: {"id":"chatcmpl_reason","created":123,"model":"deepseek-reasoner","choices":[{"delta":{"reasoning":"Now answer. "}}]}` + "\n\n",
		`data: {"id":"chatcmpl_reason","created":123,"model":"deepseek-reasoner","choices":[{"delta":{"content":"Done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":3}}}` + "\n\n",
		"data: [DONE]\n\n",
	})
	for _, want := range []string{
		"event: response.reasoning_summary_part.added",
		"event: response.reasoning_summary_text.delta",
		"event: response.reasoning_summary_text.done",
		"Need context. Now answer. ",
		`"type":"reasoning"`,
		`"text":"Done"`,
		`"reasoning_tokens":3`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
	if strings.Index(output, `"type":"reasoning"`) > strings.Index(output, `"type":"message"`) {
		t.Fatalf("reasoning item must precede the message item")
	}
}

func TestSSEConvertsInlineThinkWithoutLeakingTags(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_minimax","created":123,"model":"MiniMax-M2.7","choices":[{"delta":{"role":"assistant","content":"<think>\nNeed"}}]}` + "\n\n",
		`data: {"id":"chatcmpl_minimax","created":123,"model":"MiniMax-M2.7","choices":[{"delta":{"content":" context.</think>\n\npong"},"finish_reason":"stop"}]}` + "\n\n",
		`data: {"id":"chatcmpl_minimax","created":123,"model":"MiniMax-M2.7","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10,"completion_tokens_details":{"reasoning_tokens":3}}}` + "\n\n",
	})
	if !strings.Contains(output, "event: response.reasoning_summary_text.delta") {
		t.Fatalf("missing reasoning delta in:\n%s", output)
	}
	if !strings.Contains(output, "Need context.") || !strings.Contains(output, `"text":"pong"`) {
		t.Fatalf("inline think split failed:\n%s", output)
	}
	if strings.Contains(output, "<think>") || strings.Contains(output, "</think>") {
		t.Fatalf("think tags leaked into the stream:\n%s", output)
	}
}

func TestSSEConvertsToolCallToResponses(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_2","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"get_weather"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_2","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"city\":\"Tokyo\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	for _, want := range []string{
		"event: response.function_call_arguments.delta",
		"event: response.function_call_arguments.done",
		`"type":"function_call"`,
		`"call_id":"call_1"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
}

func TestSSEPreservesToolIdentityAcrossEmptyContinuationDeltas(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_dashscope","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_dashscope","type":"function","function":{"name":"exec_command","arguments":"{"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_dashscope","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":0,"id":"","type":"function","function":{"name":"","arguments":"\"cmd\":\"date\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	events := sseEvents(t, output)
	added := 0
	for _, event := range events {
		if event["type"] == "response.output_item.added" {
			added++
		}
	}
	if added != 1 {
		t.Fatalf("expected one added item, got %d:\n%s", added, output)
	}
	done := findEvent(events, "response.output_item.done")
	completed := findEvent(events, "response.completed")
	for _, item := range []map[string]any{done["item"].(map[string]any), outputItemsOf(t, completed)[0]} {
		if item["type"] != "function_call" || item["name"] != "exec_command" || item["call_id"] != "call_dashscope" {
			t.Fatalf("tool identity lost: %v", item)
		}
		if item["arguments"] != `{"cmd":"date"}` {
			t.Fatalf("arguments: %v", item["arguments"])
		}
	}
	if strings.Contains(output, `"name":""`) || strings.Contains(output, `"call_id":""`) {
		t.Fatalf("empty identity fragments leaked into the stream:\n%s", output)
	}
}

func TestSSEPreservesParallelToolOrderWhenEarlierNameArrivesLate(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_parallel","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_first","type":"function","function":{"name":"","arguments":"{"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_parallel","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_second","type":"function","function":{"name":"second_tool","arguments":"{\"value\":2}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_parallel","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"first_tool","arguments":"\"value\":1}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	events := sseEvents(t, output)
	added := 0
	for _, event := range events {
		if event["type"] == "response.output_item.added" {
			added++
			item := event["item"].(map[string]any)
			if added == 1 && (event["output_index"] != float64(0) || item["name"] != "first_tool") {
				t.Fatalf("first added item wrong: %v %v", event["output_index"], item["name"])
			}
			if added == 2 && (event["output_index"] != float64(1) || item["name"] != "second_tool") {
				t.Fatalf("second added item wrong: %v %v", event["output_index"], item["name"])
			}
		}
	}
	if added != 2 {
		t.Fatalf("expected two added items, got %d:\n%s", added, output)
	}
	items := outputItemsOf(t, findEvent(events, "response.completed"))
	if items[0]["name"] != "first_tool" || items[0]["call_id"] != "call_first" || items[0]["arguments"] != `{"value":1}` {
		t.Fatalf("first item wrong: %v", items[0])
	}
	if items[1]["name"] != "second_tool" || items[1]["call_id"] != "call_second" || items[1]["arguments"] != `{"value":2}` {
		t.Fatalf("second item wrong: %v", items[1])
	}
}

func TestSSEFinalizationKeepsValidCallAfterUnnamedEarlierCall(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_parallel_missing","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_missing","type":"function","function":{"arguments":"{}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_parallel_missing","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_valid","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"date\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	events := sseEvents(t, output)
	items := outputItemsOf(t, findEvent(events, "response.completed"))
	if len(items) != 1 {
		t.Fatalf("expected one surviving item, got %d:\n%s", len(items), output)
	}
	if items[0]["name"] != "exec_command" || items[0]["call_id"] != "call_valid" {
		t.Fatalf("surviving item wrong: %v", items[0])
	}
	if strings.Contains(output, "call_missing") {
		t.Fatalf("dropped call leaked into the stream:\n%s", output)
	}
}

func TestSSEDroppedOnlyToolCallEmitsFailedWithoutCompleted(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_drop","model":"kimi-k3","choices":[{"delta":{"content":"让我继续处理这个文件"}}]}` + "\n\n",
		`data: {"id":"chatcmpl_drop","model":"kimi-k3","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_bad","type":"function","function":{"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, "event: response.failed") {
		t.Fatalf("expected response.failed in:\n%s", output)
	}
	if !strings.Contains(output, "upstream_tool_call_dropped") {
		t.Fatalf("expected dropped tool call error type in:\n%s", output)
	}
	if strings.Contains(output, "event: response.completed") {
		t.Fatalf("failed turn must not also complete:\n%s", output)
	}
	if !strings.Contains(output, "让我继续处理这个文件") {
		t.Fatalf("already-streamed text must survive:\n%s", output)
	}
}

func TestSSETruncatedTurnStaysIncompleteInsteadOfFailed(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_trunc","model":"kimi-k3","choices":[{"delta":{"content":"我来看看"}}]}` + "\n\n",
		`data: {"id":"chatcmpl_trunc","model":"kimi-k3","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_cut","type":"function","function":{"arguments":"{\"pa"}}]},"finish_reason":"length"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, "event: response.completed") {
		t.Fatalf("expected response.completed in:\n%s", output)
	}
	if !strings.Contains(output, `"status":"incomplete"`) {
		t.Fatalf("expected incomplete status in:\n%s", output)
	}
	if strings.Contains(output, "event: response.failed") {
		t.Fatalf("truncated turn must not fail:\n%s", output)
	}
}

func TestSSEWhitespaceOnlyToolNameIsDropped(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_ws","model":"kimi-k3","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_ws","type":"function","function":{"name":"   ","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, "event: response.failed") || !strings.Contains(output, "upstream_tool_call_dropped") {
		t.Fatalf("whitespace name must drop and fail:\n%s", output)
	}
	if strings.Contains(output, "event: response.completed") {
		t.Fatalf("failed turn must not complete:\n%s", output)
	}
}

func TestSSETextOnlyTurnStillCompletes(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_text","model":"kimi-k3","choices":[{"delta":{"content":"完成了"},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	events := sseEvents(t, output)
	if findEvent(events, "response.completed") == nil {
		t.Fatalf("missing response.completed in:\n%s", output)
	}
	if strings.Contains(output, "event: response.failed") {
		t.Fatalf("text-only turn must not fail:\n%s", output)
	}
	for _, eventType := range []string{"response.created", "response.completed"} {
		event := findEvent(events, eventType)
		usage := event["response"].(map[string]any)["usage"].(map[string]any)
		if usage["input_tokens_details"].(map[string]any)["cached_tokens"] != float64(0) {
			t.Fatalf("%s usage cached tokens: %v", eventType, usage)
		}
	}
}

func TestSSEMissingIndexWithDistinctIDsKeepsCallsSeparate(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_noidx","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"a.txt\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_noidx","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"id":"call_b","type":"function","function":{"name":"exec_command","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	items := outputItemsOf(t, findEvent(sseEvents(t, output), "response.completed"))
	if len(items) != 2 {
		t.Fatalf("expected two calls, got %d:\n%s", len(items), output)
	}
	if items[0]["call_id"] != "call_a" || items[0]["name"] != "read_file" || items[0]["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("first call wrong: %v", items[0])
	}
	if items[1]["call_id"] != "call_b" || items[1]["name"] != "exec_command" || items[1]["arguments"] != `{"cmd":"ls"}` {
		t.Fatalf("second call wrong: %v", items[1])
	}
}

func TestSSEMissingIndexArgumentFragmentsStayInOneCall(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_frag","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_frag","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"type":"function","function":{"arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	items := outputItemsOf(t, findEvent(sseEvents(t, output), "response.completed"))
	if len(items) != 1 {
		t.Fatalf("argument fragments must stay in one call, got %d:\n%s", len(items), output)
	}
	if items[0]["call_id"] != "call_a" || items[0]["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("call wrong: %v", items[0])
	}
}

func TestSSEMissingIndexRepeatedSameIDStaysInOneCall(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_rep","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"{\"path\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_rep","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"id":"call_a","type":"function","function":{"name":"read_file","arguments":"\"a.txt\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	items := outputItemsOf(t, findEvent(sseEvents(t, output), "response.completed"))
	if len(items) != 1 || items[0]["arguments"] != `{"path":"a.txt"}` {
		t.Fatalf("repeated id must stay in one call:\n%s", output)
	}
}

func TestSSEFinalizationKeepsNonContiguousToolIndex(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_sparse","model":"deepseek-v4-pro","choices":[{"delta":{"tool_calls":[{"index":2,"id":"call_sparse","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	items := outputItemsOf(t, findEvent(sseEvents(t, output), "response.completed"))
	if len(items) != 1 {
		t.Fatalf("expected one item, got %d:\n%s", len(items), output)
	}
	if items[0]["name"] != "read_file" || items[0]["call_id"] != "call_sparse" || items[0]["arguments"] != `{"path":"README.md"}` {
		t.Fatalf("item wrong: %v", items[0])
	}
}

func TestSSERestoresCustomToolInputStreamEvents(t *testing.T) {
	request := `{"model": "gpt-5.4", "tools": [{"type": "custom", "name": "exec"}]}`
	context := NewToolContextFromRequestBytes([]byte(request))
	output := streamChatWithContext(t, []string{
		`data: {"id":"chatcmpl_custom","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_custom","type":"function","function":{"name":"exec"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_custom","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"input\":"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_custom","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls -la\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}, context)
	if !strings.Contains(output, "event: response.custom_tool_call_input.delta") ||
		!strings.Contains(output, "event: response.custom_tool_call_input.done") {
		t.Fatalf("missing custom tool input events:\n%s", output)
	}
	if strings.Contains(output, "event: response.function_call_arguments.delta") ||
		strings.Contains(output, "event: response.function_call_arguments.done") {
		t.Fatalf("custom tools must not emit function call argument events:\n%s", output)
	}
	for _, want := range []string{`"id":"ctc_call_custom"`, `"type":"custom_tool_call"`, `"name":"exec"`, `"input":"ls -la"`} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
}

func TestSSECanonicalizesStreamedToolCallArgumentsOnDoneEvents(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_args","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_args","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{ \"b\": 2,"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_args","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":" \"a\": 1 }"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, `"arguments":"{\"a\":1,\"b\":2}"`) {
		t.Fatalf("done arguments must be canonicalized:\n%s", output)
	}
}

func TestSSEPreservesReasoningContentOnStreamedToolCallItems(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_tool_reasoning","model":"deepseek-v4-flash","choices":[{"delta":{"reasoning_content":"Need file."}}]}` + "\n\n",
		`data: {"id":"chatcmpl_tool_reasoning","model":"deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_tool_reasoning","model":"deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, "event: response.output_item.done") {
		t.Fatalf("missing output_item.done in:\n%s", output)
	}
	if !strings.Contains(output, `"type":"function_call"`) || !strings.Contains(output, `"reasoning_content":"Need file."`) {
		t.Fatalf("reasoning_content must be attached to the tool call item:\n%s", output)
	}
}

func TestSSEPreservesLateReasoningContentOnStreamedToolCallItems(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_tool_late","model":"deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_tool_late","model":"deepseek-v4-flash","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"path\":\"README.md\"}"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_tool_late","model":"deepseek-v4-flash","choices":[{"delta":{"reasoning_content":"Need file."}}]}` + "\n\n",
		`data: {"id":"chatcmpl_tool_late","model":"deepseek-v4-flash","choices":[{"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, `"reasoning_content":"Need file."`) {
		t.Fatalf("late reasoning_content must be attached to the tool call item:\n%s", output)
	}
}

func TestSSERestoresNamespaceOnStreamedToolCallItems(t *testing.T) {
	request := `{
		"model": "gpt-5.4",
		"input": [{
			"type": "tool_search_output",
			"call_id": "call_tool_search_1",
			"tools": [{
				"type": "namespace",
				"name": "mcp__codex_apps__gmail",
				"tools": [{
					"type": "function",
					"name": "_search_emails",
					"description": "Search Gmail.",
					"parameters": {"type": "object"}
				}]
			}]
		}]
	}`
	context := NewToolContextFromRequestBytes([]byte(request))
	output := streamChatWithContext(t, []string{
		`data: {"id":"chatcmpl_gmail","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_gmail","type":"function","function":{"name":"mcp__codex_apps__gmail___search_emails"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_gmail","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"in:inbox\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}, context)
	for _, want := range []string{
		`"type":"function_call"`,
		`"namespace":"mcp__codex_apps__gmail"`,
		`"name":"_search_emails"`,
		`"arguments":"{\"query\":\"in:inbox\"}"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
}

func TestSSERestoresToolSearchOnStreamedToolCallItems(t *testing.T) {
	request := `{"model": "gpt-5.4", "tools": [{"type": "tool_search"}], "input": "Search for Gmail tools."}`
	context := NewToolContextFromRequestBytes([]byte(request))
	output := streamChatWithContext(t, []string{
		`data: {"id":"chatcmpl_tool_search","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_tool_search_1","type":"function","function":{"name":"tool_search"}}]}}]}` + "\n\n",
		`data: {"id":"chatcmpl_tool_search","model":"gpt-5.4","choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"Gmail search emails\",\"limit\":10}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
		"data: [DONE]\n\n",
	}, context)
	for _, want := range []string{
		`"type":"tool_search_call"`,
		`"execution":"client"`,
		`"call_id":"call_tool_search_1"`,
		`"query":"Gmail search emails"`,
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q in:\n%s", want, output)
		}
	}
}

func TestSSEStreamEndWithOutputWithoutFinishReasonEmitsIncomplete(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_truncated","model":"gpt-5.4","choices":[{"delta":{"content":"partial"}}]}` + "\n\n",
	})
	if !strings.Contains(output, "event: response.completed") {
		t.Fatalf("expected response.completed in:\n%s", output)
	}
	if !strings.Contains(output, `"status":"incomplete"`) ||
		!strings.Contains(output, `"incomplete_details":{"reason":"max_output_tokens"}`) {
		t.Fatalf("expected incomplete details in:\n%s", output)
	}
	if strings.Contains(output, "event: response.failed") {
		t.Fatalf("truncated stream with output must not fail:\n%s", output)
	}
}

func TestSSEStreamEndWithoutOutputOrFinishReasonEmitsFailed(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_truncated","model":"gpt-5.4","choices":[{"delta":{}}]}` + "\n\n",
	})
	if !strings.Contains(output, "event: response.failed") || !strings.Contains(output, "stream_truncated") {
		t.Fatalf("expected stream_truncated failure in:\n%s", output)
	}
	if strings.Contains(output, "event: response.completed") {
		t.Fatalf("empty stream must not complete:\n%s", output)
	}
}

func TestSSEChatErrorEventEmitsFailedWithoutCompleted(t *testing.T) {
	output := streamChat(t, []string{
		"event: error\ndata: {\"error\":{\"message\":\"bad request\",\"type\":\"invalid_request_error\"}}\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, "event: response.failed") {
		t.Fatalf("expected response.failed in:\n%s", output)
	}
	if !strings.Contains(output, "bad request") || !strings.Contains(output, "invalid_request_error") {
		t.Fatalf("error details lost:\n%s", output)
	}
	if strings.Contains(output, "event: response.completed") {
		t.Fatalf("failed stream must not complete:\n%s", output)
	}
}

func TestSSEChatDataOnlyErrorEmitsFailedWithoutCompleted(t *testing.T) {
	output := streamChat(t, []string{
		"data: {\"error\":{\"message\":\"quota exceeded\",\"code\":\"rate_limit_exceeded\"}}\n\n",
		"data: [DONE]\n\n",
	})
	if !strings.Contains(output, "event: response.failed") {
		t.Fatalf("expected response.failed in:\n%s", output)
	}
	if !strings.Contains(output, "quota exceeded") || !strings.Contains(output, "rate_limit_exceeded") {
		t.Fatalf("error details lost:\n%s", output)
	}
}

func TestSSEBuffersSplitEvent(t *testing.T) {
	converter := NewSSEConverter()
	first, err := converter.Convert([]byte(`data: {"id":"chatcmpl_split","model":"m","choices":[{"delta":{"con`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := converter.Convert([]byte(`tent":"hi"},"finish_reason":"stop"}]}` + "\n\n"))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := converter.Flush()
	if err != nil {
		t.Fatal(err)
	}
	output := string(first) + string(second) + string(tail)
	if !strings.Contains(output, `"text":"hi"`) {
		t.Fatalf("split event must be buffered and emitted:\n%s", output)
	}
}

func TestSSEAcceptsMultilineData(t *testing.T) {
	// SSE multi-line data: every line carries its own data: prefix and the
	// payload is the lines joined with newlines (valid JSON whitespace).
	output := streamChat(t, []string{
		"data: {\"id\":\"chatcmpl_ml\",\ndata: \"model\":\"m\",\ndata: \"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n",
	})
	if !strings.Contains(output, `"text":"hi"`) {
		t.Fatalf("multiline data must be joined:\n%s", output)
	}
}

func TestSSEEmptyStreamStillHasLifecycle(t *testing.T) {
	output := streamChat(t, []string{"data: [DONE]\n\n"})
	events := sseEvents(t, output)
	if findEvent(events, "response.created") == nil {
		t.Fatalf("empty stream must still emit response.created:\n%s", output)
	}
	if findEvent(events, "response.completed") == nil {
		t.Fatalf("empty stream must still emit response.completed:\n%s", output)
	}
}

func TestResponseIDFromSSEMatchesConverterID(t *testing.T) {
	input := "data: {\"id\":\"chatcmpl_id\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	if got := ResponseIDFromSSE([]byte(input)); got != "resp_chatcmpl_id" {
		t.Fatalf("ResponseIDFromSSE = %q", got)
	}
}

func TestResponseIDFromSSEWithoutIDIsDeterministic(t *testing.T) {
	input := "data: {\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"x\"}}]}\n\n"
	first := ResponseIDFromSSE([]byte(input))
	second := ResponseIDFromSSE([]byte(input))
	if first == "" || first != second {
		t.Fatalf("synthesized id must be deterministic: %q vs %q", first, second)
	}
}

func TestSSECanonicalResponseCarriesToolsAndUsage(t *testing.T) {
	converter := NewSSEConverter()
	chunks := []string{
		`data: {"id":"chatcmpl_canon","model":"m","choices":[{"delta":{"reasoning_content":"thought"}}]}` + "\n\n",
		`data: {"id":"chatcmpl_canon","model":"m","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"README.md\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}` + "\n\n",
		"data: [DONE]\n\n",
	}
	for _, chunk := range chunks {
		if _, err := converter.Convert([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := converter.Flush(); err != nil {
		t.Fatal(err)
	}
	canonical := converter.CanonicalResponse()
	if canonical == nil {
		t.Fatalf("canonical response must not be nil")
	}
	if canonical["status"] != "completed" {
		t.Fatalf("status: %v", canonical["status"])
	}
	output := canonical["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("expected reasoning + tool call items, got %v", output)
	}
	if output[0].(map[string]any)["type"] != "reasoning" {
		t.Fatalf("first item should be reasoning: %v", output[0])
	}
	toolItem := output[1].(map[string]any)
	if toolItem["type"] != "function_call" || toolItem["reasoning_content"] != "thought" {
		t.Fatalf("tool item lost reasoning: %v", toolItem)
	}
	usage := canonical["usage"].(map[string]any)
	jsonEq(t, "usage input tokens", usage["input_tokens"], 5)
}

// A null error field is not an error. Providers that serialize a response
// struct carrying an error field emit `"error": null` on every chunk, and
// treating the key's presence as a failure ended a healthy stream after its
// first chunk with response.failed and the message "null".
func TestSSEDoesNotFailOnNullErrorField(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_n","model":"m","choices":[{"delta":{"content":"hi"}}],"error":null}` + "\n\n",
		`data: {"id":"chatcmpl_n","model":"m","choices":[{"delta":{"content":" there"},"finish_reason":"stop"}]}` + "\n\n",
	})
	if strings.Contains(output, "event: response.failed") {
		t.Fatalf("null error field must not fail the stream:\n%s", output)
	}
	if !strings.Contains(output, "hi") || !strings.Contains(output, " there") {
		t.Fatalf("content after a null error field was lost:\n%s", output)
	}
}

// A non-null error field must still fail the stream.
func TestSSEFailsOnPresentErrorField(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_e","model":"m","choices":[{"delta":{"content":"hi"}}],"error":{"message":"upstream exploded","type":"server_error"}}` + "\n\n",
	})
	if !strings.Contains(output, "event: response.failed") {
		t.Fatalf("a present error field must fail the stream:\n%s", output)
	}
	if !strings.Contains(output, "upstream exploded") {
		t.Fatalf("error message missing:\n%s", output)
	}
}

// Chat's content_filter finish truncates the turn, and the Responses API has a
// matching incomplete reason for it. Reporting it as completed let a client
// branch on status and treat a cut-off turn as a finished one.
func TestSSEContentFilterReportsIncomplete(t *testing.T) {
	output := streamChat(t, []string{
		`data: {"id":"chatcmpl_cf","model":"m","choices":[{"delta":{"content":"partial"},"finish_reason":"content_filter"}]}` + "\n\n",
	})
	if !strings.Contains(output, `"status":"incomplete"`) {
		t.Fatalf("content_filter must be incomplete:\n%s", output)
	}
	if !strings.Contains(output, `"incomplete_details":{"reason":"content_filter"}`) {
		t.Fatalf("content_filter reason missing:\n%s", output)
	}
}
