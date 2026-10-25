package dialect

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
	"time"
)

// ---- Critical / High regressions found by the independent review ----

func TestGeminiStreamRequestKeepsStreamForClaudeTarget(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	stream := true
	converted, _, err := ConvertRequest(Gemini, ClaudeMessages, body, ConvertOptions{Model: "m", Stream: &stream})
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, converted)
	if object["stream"] != true {
		t.Fatalf("stream flag lost: %s", converted)
	}
}

func TestAnthropicToolResultReachesResponses(t *testing.T) {
	// A Claude transcript: assistant tool_use, then a user turn carrying the
	// tool_result. Responses needs function_call_output, not a dropped part.
	body := []byte(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"lookup","input":{"q":"x"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"found"},{"type":"text","text":"now use it"}]}
	]}`)
	converted, diagnostics, err := ConvertRequest(ClaudeMessages, OpenAIResponses, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"type":"function_call_output"`) ||
		!strings.Contains(string(converted), `"call_id":"call_1"`) {
		t.Fatalf("tool result dropped: %s (%v)", converted, diagnostics)
	}
	if !strings.Contains(string(converted), "found") || !strings.Contains(string(converted), "now use it") {
		t.Fatalf("content lost: %s", converted)
	}
}

func TestGeminiFunctionResponseReachesResponses(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"found"}}}]}
	]}`)
	converted, diagnostics, err := ConvertRequest(Gemini, OpenAIResponses, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"type":"function_call_output"`) || !strings.Contains(string(converted), "found") {
		t.Fatalf("function response dropped: %s (%v)", converted, diagnostics)
	}
}

func TestStreamParallelToolCallsDoNotEmitAfterClose(t *testing.T) {
	frames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"b","arguments":"{\"y\":"}}]}}]}`,
		// Interleaved deltas: index 0 arrives after block 1 started.
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	output, diagnostics := runStream(t, OpenAIChat, ClaudeMessages, frames, StreamOptions{})
	if !strings.Contains(output, `"partial_json"`) {
		t.Fatalf("no argument fragments: %s (%v)", output, diagnostics)
	}
	// Every content_block_delta index must have seen its content_block_start
	// and must not follow that index's content_block_stop.
	open := map[string]bool{}
	closed := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if payload == "" || !strings.HasPrefix(payload, "{") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		index := ""
		if value, ok := event["index"].(float64); ok {
			index = strings.TrimSuffix(strings.TrimRight(strings.TrimRight(formatFloat(value), "0"), "."), ".")
		}
		switch event["type"] {
		case "content_block_start":
			open[index] = true
		case "content_block_delta":
			if index != "" && closed[index] {
				t.Fatalf("delta for closed block %s: %s", index, output)
			}
			if index != "" && !open[index] {
				t.Fatalf("delta for unopened block %s: %s", index, output)
			}
		case "content_block_stop":
			closed[index] = true
			open[index] = false
		}
	}
}

func formatFloat(value float64) string {
	encoded, _ := json.Marshal(value)
	return string(encoded)
}

func TestStreamClaudeUsageSeedsChatInputTokens(t *testing.T) {
	frames := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":10,"cache_read_input_tokens":4,"output_tokens":0}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
	}
	chat, diagnostics := runStream(t, ClaudeMessages, OpenAIChat, frames, StreamOptions{IncludeUsage: true})
	if !strings.Contains(chat, `"prompt_tokens":14`) {
		t.Fatalf("input usage lost: %s (%v)", chat, diagnostics)
	}
	if !strings.Contains(chat, `"total_tokens":17`) {
		t.Fatalf("total not recomputed from input+output: %s", chat)
	}
	responses, _ := runStream(t, ClaudeMessages, OpenAIResponses, frames, StreamOptions{})
	if !strings.Contains(responses, `"input_tokens":14`) || !strings.Contains(responses, `"total_tokens":17`) {
		t.Fatalf("responses usage inconsistent: %s", responses)
	}
}

func TestStreamGeminiSingleFinishFrameKeepsReason(t *testing.T) {
	frames := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"MAX_TOKENS","index":0}]}`,
	}
	chat, _ := runStream(t, Gemini, OpenAIChat, frames, StreamOptions{})
	if got := strings.Count(chat, `"finish_reason":"length"`); got != 1 {
		t.Fatalf("finish chunks = %d, want 1: %s", got, chat)
	}
	if strings.Contains(chat, `"finish_reason":"stop"`) {
		t.Fatalf("a second finish frame overwrote the reason: %s", chat)
	}
}

func TestStreamGeminiToolCallHasNonEmptyID(t *testing.T) {
	frames := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}}}]},"finishReason":"STOP","index":0}]}`,
	}
	for _, target := range []Format{OpenAIChat, OpenAIResponses, ClaudeMessages} {
		output, _ := runStream(t, Gemini, target, frames, StreamOptions{})
		if strings.Contains(output, `"id":""`) || strings.Contains(output, `"call_id":""`) {
			t.Fatalf("%s: empty tool id: %s", target, output)
		}
	}
}

func TestStreamGeminiSameFrameToolCallFinishesAsToolUse(t *testing.T) {
	frames := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}}}]},"finishReason":"STOP","index":0}]}`,
	}
	chat, _ := runStream(t, Gemini, OpenAIChat, frames, StreamOptions{})
	if !strings.Contains(chat, `"finish_reason":"tool_calls"`) {
		t.Fatalf("a tool call in the finish frame must map to tool_calls: %s", chat)
	}
}

func TestStreamResponsesOutputIndexesAreConsistent(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1,"status":"in_progress"}}`,
		`{"type":"response.reasoning_summary_text.delta","delta":"think"}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
	}
	output, _ := runStream(t, OpenAIResponses, ClaudeMessages, frames, StreamOptions{})
	// Claude is the target here; the interesting assertion is that the source
	// decoder does not lose either item.
	if !strings.Contains(output, "think") || !strings.Contains(output, "hi") {
		t.Fatalf("reasoning or text lost: %s", output)
	}

	// Responses→Gemini keeps both, and the Responses target must produce
	// consistent output_index values across added/delta/done.
	claudeFrames := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"think"}}`,
		`{"type":"content_block_stop","index":0}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"content_block_stop","index":1}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`,
	}
	responses, _ := runStream(t, ClaudeMessages, OpenAIResponses, claudeFrames, StreamOptions{})
	// Every item's announced index must match its final position in
	// response.completed.output, including items added with `item` (not
	// `item_id`) in response.output_item.added.
	added := map[string]string{}
	var completed []any
	for _, line := range strings.Split(responses, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		if event["type"] == "response.completed" {
			if response, ok := event["response"].(map[string]any); ok {
				completed, _ = response["output"].([]any)
			}
			continue
		}
		if event["type"] == "response.output_item.added" {
			if item, ok := event["item"].(map[string]any); ok {
				if id, _ := item["id"].(string); id != "" {
					if index, ok := event["output_index"].(float64); ok {
						added[id] = formatFloat(index)
					}
				}
			}
			continue
		}
		itemID, _ := event["item_id"].(string)
		index, hasIndex := event["output_index"]
		if itemID == "" || !hasIndex {
			continue
		}
		key := formatFloat(index.(float64))
		if previous, seen := added[itemID]; seen && previous != key {
			t.Fatalf("item %s changed output_index from %s to %s: %s", itemID, previous, key, responses)
		}
		added[itemID] = key
	}
	if len(completed) == 0 {
		t.Fatalf("no response.completed output: %s", responses)
	}
	for position, raw := range completed {
		item, _ := raw.(map[string]any)
		id, _ := item["id"].(string)
		want := formatFloat(float64(position))
		if got, seen := added[id]; !seen || got != want {
			t.Fatalf("item %s at output[%d] announced index %q: %s", id, position, got, responses)
		}
	}
}

func TestResponsesAssistantItemsKeepReasoningFirst(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"},{"role":"assistant","content":[{"type":"text","text":"answer"}],"reasoning_content":"think"}]}`)
	converted, _, err := ConvertRequest(OpenAIChat, OpenAIResponses, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// Compare only the assistant run: the leading user message is neither.
	var input []map[string]any
	raw := decodeMap(t, converted)["input"].([]any)
	for _, item := range raw {
		input = append(input, item.(map[string]any))
	}
	reasoningAt, assistantAt := -1, -1
	for index, item := range input {
		switch item["type"] {
		case "reasoning":
			if reasoningAt < 0 {
				reasoningAt = index
			}
		case "message":
			if item["role"] == "assistant" && assistantAt < 0 {
				assistantAt = index
			}
		}
	}
	if reasoningAt < 0 || assistantAt < 0 || reasoningAt > assistantAt {
		t.Fatalf("reasoning must precede the assistant message: %s", converted)
	}
}

func TestResponsesJSONSchemaGetsName(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","text":{"format":{"type":"json_schema","schema":{"type":"object"}}}}`)
	converted, _, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	format := decodeMap(t, converted)["response_format"].(map[string]any)
	schema := format["json_schema"].(map[string]any)
	if schema["name"] == nil || schema["name"] == "" {
		t.Fatalf("json_schema name missing: %s", converted)
	}
}

func TestResponsesFileURLIsRead(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"message","role":"user","content":[{"type":"input_file","file_url":"https://example.com/a.pdf"},{"type":"input_text","text":"summarize"}]}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), "summarize") {
		t.Fatalf("conversation emptied: %s (%v)", converted, diagnostics)
	}
	if !strings.Contains(string(converted), "example.com/a.pdf") {
		t.Fatalf("file_url lost: %s", converted)
	}
}

func TestNonBase64DataURLIsNotTreatedAsBase64(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:text/plain,hello"}}]}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converted), `"data":"hello"`) {
		t.Fatalf("plain data URL was emitted as base64: %s", converted)
	}
	if strings.Contains(string(converted), "data:text/plain,hello") {
		t.Fatalf("unsupported data URL was forwarded as a URL source: %s", converted)
	}
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "non_base64_data_url" {
			found = true
		}
	}
	if !found {
		t.Fatalf("non-base64 data URL must be diagnosed: %v", diagnostics)
	}
}

func TestGeminiPDFIsADocument(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"inlineData":{"mimeType":"application/pdf","data":"JVBERi0="}},{"text":"summarize"}]}]}`)
	converted, _, err := ConvertRequest(Gemini, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"type":"document"`) {
		t.Fatalf("PDF became a non-document block: %s", converted)
	}
	if strings.Contains(string(converted), `"type":"image"`) {
		t.Fatalf("PDF emitted as image: %s", converted)
	}
}

func TestFailedResponsesResponseSurfacesAsError(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","status":"failed","model":"m","error":{"code":"server_error","message":"boom"},"output":[]}`)
	converted, _, err := ConvertResponse(OpenAIResponses, ClaudeMessages, body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"type":"error"`) || !strings.Contains(string(converted), "boom") {
		t.Fatalf("failed response became a success: %s", converted)
	}
}

func TestLegacyFunctionCallKeepsID(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"assistant","function_call":{"name":"lookup","arguments":"{\"q\":\"x\"}"}},{"role":"function","name":"lookup","content":"found"}]}`)
	converted, _, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(converted)
	if strings.Contains(text, `"id":""`) || strings.Contains(text, `"tool_use_id":""`) {
		t.Fatalf("legacy call ids are empty: %s", text)
	}
	// The tool_use and tool_result must share the synthesized id.
	var request map[string]any
	if err := json.Unmarshal(converted, &request); err != nil {
		t.Fatal(err)
	}
	var useID, resultID string
	for _, raw := range request["messages"].([]any) {
		message := raw.(map[string]any)
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			switch block["type"] {
			case "tool_use":
				useID, _ = block["id"].(string)
			case "tool_result":
				resultID, _ = block["tool_use_id"].(string)
			}
		}
	}
	if useID == "" || useID != resultID {
		t.Fatalf("legacy call not joined: useID=%q resultID=%q (%s)", useID, resultID, text)
	}
}

func TestGeminiStripKeepsFunctionDeclarationsBesideSearch(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"googleSearch":{},"functionDeclarations":[{"name":"lookup"}]}]}`)
	cleaned, changed, err := StripHostedSearchTools(body, Gemini)
	if err != nil || !changed {
		t.Fatalf("strip = %s %t %v", cleaned, changed, err)
	}
	if !strings.Contains(string(cleaned), `"lookup"`) {
		t.Fatalf("function declaration deleted with the hosted tool: %s", cleaned)
	}
	if strings.Contains(string(cleaned), "googleSearch") {
		t.Fatalf("hosted search survived: %s", cleaned)
	}
}

func TestStreamExtraChoicesAreDiagnosed(t *testing.T) {
	frames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"A"}},{"index":1,"delta":{"content":"B"}}]}`,
	}
	output, diagnostics := runStream(t, OpenAIChat, ClaudeMessages, frames, StreamOptions{})
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "extra_choice_dropped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("second choice was merged silently: %s (%v)", output, diagnostics)
	}
	if strings.Contains(output, "B") {
		t.Fatalf("second choice leaked into a single-choice dialect: %s", output)
	}
}

// ---- Round-2 findings ----

func TestStreamGeminiTargetSeedsInputUsage(t *testing.T) {
	frames := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":10,"cache_read_input_tokens":4,"output_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":3}}`,
	}
	output, diagnostics := runStream(t, ClaudeMessages, Gemini, frames, StreamOptions{})
	if !strings.Contains(output, `"promptTokenCount":14`) {
		t.Fatalf("gemini lost the input usage: %s (%v)", output, diagnostics)
	}
	if !strings.Contains(output, `"totalTokenCount":17`) {
		t.Fatalf("gemini total is inconsistent: %s", output)
	}
}

func TestStreamToolCallThenTextClosesBlocksInOrder(t *testing.T) {
	// A Chat stream with a tool call and no explicit end must not leave the
	// tool block open when the text begins, and stops must follow starts.
	frames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"after"},"finish_reason":"stop"}]}`,
	}
	output, _ := runStream(t, OpenAIChat, ClaudeMessages, frames, StreamOptions{})
	var openBlocks []string
	for _, line := range strings.Split(output, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(payload, "{") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		index, _ := event["index"].(float64)
		key := formatFloat(index)
		switch event["type"] {
		case "content_block_start":
			openBlocks = append(openBlocks, key)
		case "content_block_stop":
			if len(openBlocks) == 0 || openBlocks[len(openBlocks)-1] != key {
				t.Fatalf("block %s stopped out of order (open=%v): %s", key, openBlocks, output)
			}
			openBlocks = openBlocks[:len(openBlocks)-1]
		}
	}
	if len(openBlocks) != 0 {
		t.Fatalf("blocks left open: %v\n%s", openBlocks, output)
	}
}

func TestStreamChatTargetEmitsOneRoleChunk(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1,"status":"in_progress"}}`,
		`{"type":"response.in_progress","response":{"id":"resp_1","model":"m","status":"in_progress"}}`,
		`{"type":"response.output_text.delta","delta":"hi"}`,
	}
	chat, _ := runStream(t, OpenAIResponses, OpenAIChat, frames, StreamOptions{})
	if got := strings.Count(chat, `"role":"assistant"`); got != 1 {
		t.Fatalf("role chunks = %d, want 1: %s", got, chat)
	}
}

func TestResponsesMidConversationSystemStaysInPlace(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":"one"},
		{"type":"message","role":"developer","content":"mid instruction"},
		{"type":"message","role":"user","content":"two"}
	]}`)
	converted, _, err := ConvertRequest(OpenAIResponses, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, converted)
	if object["system"] != nil {
		t.Fatalf("mid-conversation developer turn was hoisted to system: %s", converted)
	}
	messages := object["messages"].([]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %d, want 3: %s", len(messages), converted)
	}
}

// A Chat target may carry a system message only at the front: chat templates
// such as Qwen's raise "System message must be at the beginning" for any later
// one, which made every Responses request whose transcript ended with a system
// turn fail upstream. The trailing turn must survive as user text instead of
// being emitted as a second system message or dropped.
func TestChatTargetDowngradesMidConversationSystem(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"system","content":"SYS"},
		{"type":"message","role":"user","content":"one"},
		{"type":"message","role":"developer","content":"mid instruction"},
		{"type":"message","role":"user","content":"two"},
		{"type":"message","role":"system","content":"trailing reminder"}
	]}`)
	converted, _, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := decodeMap(t, converted)["messages"].([]any)
	if len(messages) != 5 {
		t.Fatalf("messages = %d, want 5: %s", len(messages), converted)
	}
	roles := make([]string, 0, len(messages))
	for _, raw := range messages {
		roles = append(roles, raw.(map[string]any)["role"].(string))
	}
	if roles[0] != "system" {
		t.Fatalf("leading system turn lost its role: %v", roles)
	}
	for index, role := range roles[1:] {
		if role == "system" || role == "developer" {
			t.Fatalf("message %d is %q, but only the first may be a system turn: %v", index+1, role, roles)
		}
	}
	if text := messages[4].(map[string]any)["content"]; text != "trailing reminder" {
		t.Fatalf("trailing system content = %v, want the text preserved as user content", text)
	}
}

// The same downgrade applies when the source is already Chat: a client that
// sends a system turn in the middle must not round-trip it back out as system.
func TestChatToChatKeepsOnlyLeadingSystem(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"system","content":"SYS"},
		{"role":"user","content":"one"},
		{"role":"system","content":"mid"},
		{"role":"user","content":"two"}
	]}`)
	converted, _, err := ConvertRequest(OpenAIChat, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := decodeMap(t, converted)["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("messages = %d, want 4: %s", len(messages), converted)
	}
	if role := messages[2].(map[string]any)["role"]; role != "user" {
		t.Fatalf("mid-conversation system turn became %q, want user: %s", role, converted)
	}
	if role := messages[0].(map[string]any)["role"]; role != "system" {
		t.Fatalf("leading system turn became %q, want system: %s", role, converted)
	}
}

func TestDisableParallelReachesResponsesAndGeminiDiagnoses(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"auto","disable_parallel_tool_use":true}}`)
	responses, _, err := ConvertRequest(ClaudeMessages, OpenAIResponses, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if decodeMap(t, responses)["parallel_tool_calls"] != false {
		t.Fatalf("responses lost disable_parallel_tool_use: %s", responses)
	}
	gemini, diagnostics, err := ConvertRequest(ClaudeMessages, Gemini, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "parallel_tool_calls_ignored" {
			found = true
		}
	}
	if !found {
		t.Fatalf("gemini ignored disable_parallel_tool_use silently: %s (%v)", gemini, diagnostics)
	}
}

func TestResponsesFailureConvertsToErrorEnvelope(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","status":"failed","model":"m","error":{"code":"server_error","message":"boom"},"output":[]}`)
	for _, target := range []Format{OpenAIChat, ClaudeMessages, Gemini} {
		converted, _, err := ConvertResponse(OpenAIResponses, target, body)
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		if !strings.Contains(string(converted), "boom") {
			t.Fatalf("%s did not surface the failure: %s", target, converted)
		}
	}
	if !ResponseFailed(OpenAIResponses, body) {
		t.Fatal("ResponseFailed must report a failed upstream response")
	}
}

func TestGeminiRequestConversionIsLinear(t *testing.T) {
	// A long conversation must not rebuild the tool-name map per message.
	build := func(count int) []byte {
		messages := make([]any, 0, count)
		for i := 0; i < count; i++ {
			messages = append(messages, map[string]any{"role": "user", "content": "hi"})
		}
		body, _ := json.Marshal(map[string]any{"model": "m", "messages": messages})
		return body
	}
	small := build(20)
	large := build(400)
	options := ConvertOptions{Model: "m"}

	// One conversion takes tens of microseconds, so a single wall-clock sample
	// measures the scheduler as much as the converter. Each size is timed over
	// enough iterations to dominate a pause, and the fastest of several rounds
	// is kept so one interfering round cannot decide the ratio.
	perConversion := func(body []byte, iterations int) time.Duration {
		best := time.Duration(math.MaxInt64)
		for round := 0; round < 9; round++ {
			started := time.Now()
			for i := 0; i < iterations; i++ {
				if _, _, err := ConvertRequest(OpenAIChat, Gemini, body, options); err != nil {
					t.Fatal(err)
				}
			}
			if elapsed := time.Since(started); elapsed < best {
				best = elapsed
			}
		}
		return best / time.Duration(iterations)
	}
	smallDuration := perConversion(small, 2000)
	largeDuration := perConversion(large, 100)
	// 20x the messages must stay well under 20^2/2 = 200x the work; a
	// quadratic rebuild lands near that bound, linear stays near 20x.
	if largeDuration > smallDuration*60 {
		t.Fatalf("Gemini request conversion scales superlinearly: 20 msgs=%s, 400 msgs=%s", smallDuration, largeDuration)
	}
}

// ---- Round-3 findings ----

func TestResponsesToolBeforeTextKeepsUniqueIndexes(t *testing.T) {
	// A tool call that starts before the assistant text must not make the
	// announcement indices collide, and each item must sit at its index.
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1,"status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_0","type":"function_call","call_id":"c0","name":"a","arguments":""}}`,
		`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"arguments":"{}"}`,
		`{"type":"response.output_text.delta","delta":"after"}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}`,
	}
	// Chat is the target, so the value under test is the source decoder's
	// canonical ordering; then convert the same shape to Responses instead.
	_, _ = runStream(t, OpenAIResponses, OpenAIChat, frames, StreamOptions{})

	responsesFrames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"after"},"finish_reason":"tool_calls"}]}`,
	}
	output, _ := runStream(t, OpenAIChat, OpenAIResponses, responsesFrames, StreamOptions{})
	announced := map[string]string{}
	var completed []any
	for _, line := range strings.Split(output, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.completed":
			if response, ok := event["response"].(map[string]any); ok {
				completed, _ = response["output"].([]any)
			}
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			id, _ := item["id"].(string)
			if index, ok := event["output_index"].(float64); ok && id != "" {
				announced[id] = formatFloat(index)
			}
		}
	}
	seen := map[string]string{}
	for id, index := range announced {
		if other, clash := seen[index]; clash {
			t.Fatalf("items %s and %s both announced output_index %s: %s", id, other, index, output)
		}
		seen[index] = id
	}
	if len(completed) == 0 {
		t.Fatalf("no completed output: %s", output)
	}
	for position, raw := range completed {
		item, _ := raw.(map[string]any)
		id, _ := item["id"].(string)
		if got, ok := announced[id]; !ok || got != formatFloat(float64(position)) {
			t.Fatalf("item %s at output[%d] announced %q: %s", id, position, got, output)
		}
	}
}

func TestResponsesReasoningDoneCarriesReasoningItem(t *testing.T) {
	frames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"reasoning_content":"think"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"answer"},"finish_reason":"stop"}]}`,
	}
	output, _ := runStream(t, OpenAIChat, OpenAIResponses, frames, StreamOptions{})
	for _, line := range strings.Split(output, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		if event["type"] != "response.output_item.done" {
			continue
		}
		index, _ := event["output_index"].(float64)
		item, _ := event["item"].(map[string]any)
		if item == nil {
			continue
		}
		itemType, _ := item["type"].(string)
		// The item announced at this index must be the one closed here.
		for _, addLine := range strings.Split(output, "\n") {
			addPayload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(addLine), "data:"))
			var added map[string]any
			if json.Unmarshal([]byte(addPayload), &added) != nil || added["type"] != "response.output_item.added" {
				continue
			}
			addedIndex, _ := added["output_index"].(float64)
			if addedIndex != index {
				continue
			}
			addedItem, _ := added["item"].(map[string]any)
			if addedType, _ := addedItem["type"].(string); addedType != itemType {
				t.Fatalf("index %v announced %s but closed %s: %s", index, addedType, itemType, output)
			}
		}
	}
}

func TestStreamTruncatedParallelToolsCloseEveryBlock(t *testing.T) {
	// Two tool calls open, only the second ends, the stream then stops without
	// a completed response: both blocks must still be closed.
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1,"status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_0","type":"function_call","call_id":"c0","name":"a","arguments":""}}`,
		`{"type":"response.output_item.added","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"b","arguments":""}}`,
		`{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_1","type":"function_call","call_id":"c1","name":"b","arguments":"{}"}}`,
	}
	output, _ := runStream(t, OpenAIResponses, ClaudeMessages, frames, StreamOptions{})
	starts := strings.Count(output, `"type":"content_block_start"`)
	stops := strings.Count(output, `"type":"content_block_stop"`)
	if starts != stops {
		t.Fatalf("blocks started=%d stopped=%d: %s", starts, stops, output)
	}
}

func TestResponsesCompletedWithEmptyErrorIsNotAFailure(t *testing.T) {
	body := []byte(`{"id":"resp_1","object":"response","status":"completed","model":"m","error":{},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}]}`)
	if ResponseFailed(OpenAIResponses, body) {
		t.Fatal("an empty error object on a completed response is not a failure")
	}
	converted, _, err := ConvertResponse(OpenAIResponses, OpenAIChat, body)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converted), `"error"`) || !strings.Contains(string(converted), "hi") {
		t.Fatalf("completed response was turned into an error: %s", converted)
	}
}

func TestOrphanLegacyFunctionResultIsDropped(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"function","name":"ghost","content":"orphan"}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converted), `"tool_use_id":""`) {
		t.Fatalf("orphan result carried an empty tool_use_id: %s", converted)
	}
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "orphan_tool_result" {
			found = true
		}
	}
	if !found {
		t.Fatalf("orphan result was dropped silently: %v", diagnostics)
	}
}

func TestUppercaseBase64DataURLIsAccepted(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;BASE64,QUJD"}}]}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"data":"QUJD"`) {
		t.Fatalf("uppercase BASE64 data URL was not decoded: %s (%v)", converted, diagnostics)
	}
}

// ---- Round-4 findings ----

func TestResponsesToolTextToolKeepsUniqueIndexes(t *testing.T) {
	// tool -> text -> tool: the second tool's allocated index is not
	// contiguous with the first, so any base+position formula collides.
	frames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"between"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"b","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	output, _ := runStream(t, OpenAIChat, OpenAIResponses, frames, StreamOptions{})
	announced := map[string]string{}
	var completed []any
	for _, line := range strings.Split(output, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.completed":
			if response, ok := event["response"].(map[string]any); ok {
				completed, _ = response["output"].([]any)
			}
		case "response.output_item.added":
			item, _ := event["item"].(map[string]any)
			id, _ := item["id"].(string)
			if index, ok := event["output_index"].(float64); ok && id != "" {
				announced[id] = formatFloat(index)
			}
		}
	}
	byIndex := map[string]string{}
	for id, index := range announced {
		if other, clash := byIndex[index]; clash {
			t.Fatalf("items %s and %s both announced index %s: %s", id, other, index, output)
		}
		byIndex[index] = id
	}
	if len(completed) == 0 {
		t.Fatalf("no completed output: %s", output)
	}
	for position, raw := range completed {
		item, _ := raw.(map[string]any)
		id, _ := item["id"].(string)
		if got, ok := announced[id]; !ok || got != formatFloat(float64(position)) {
			t.Fatalf("item %s at output[%d] announced %q: %s", id, position, got, output)
		}
	}
}

func TestStreamToolDeltaWithoutStartAllocatesIndex(t *testing.T) {
	frames := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":1}"}}`,
		`{"type":"content_block_start","index":1,"content_block":{"type":"text","text":""}}`,
		`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"after"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
	}
	output, _ := runStream(t, ClaudeMessages, OpenAIResponses, frames, StreamOptions{})
	announced := map[string]string{}
	for _, line := range strings.Split(output, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		var event map[string]any
		if json.Unmarshal([]byte(payload), &event) != nil || event["type"] != "response.output_item.added" {
			continue
		}
		item, _ := event["item"].(map[string]any)
		id, _ := item["id"].(string)
		if index, ok := event["output_index"].(float64); ok && id != "" {
			announced[id] = formatFloat(index)
		}
	}
	seen := map[string]string{}
	for id, index := range announced {
		if other, clash := seen[index]; clash {
			t.Fatalf("items %s and %s share index %s: %s", id, other, index, output)
		}
		seen[index] = id
	}
}

func TestStreamRepeatedToolStartDoesNotOrphanBlock(t *testing.T) {
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1,"status":"in_progress"}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_0","type":"function_call","call_id":"c0","name":"a","arguments":""}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_0","type":"function_call","call_id":"c0","name":"a","arguments":""}}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","output":[]}}`,
	}
	output, _ := runStream(t, OpenAIResponses, ClaudeMessages, frames, StreamOptions{})
	starts := strings.Count(output, `"type":"content_block_start"`)
	stops := strings.Count(output, `"type":"content_block_stop"`)
	if starts != stops {
		t.Fatalf("repeated start orphaned a block: starts=%d stops=%d: %s", starts, stops, output)
	}
}

func TestStreamChatRoleChunkComesFirstInEveryShape(t *testing.T) {
	cases := map[string][]string{
		"content": {
			`{"type":"response.output_text.delta","delta":"hi"}`,
		},
		"usage-only": {
			`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","usage":{"input_tokens":1,"output_tokens":1}}}`,
		},
		"args-only": {
			`{"type":"response.output_item.added","output_index":0,"item":{"id":"fc_0","type":"function_call","call_id":"c0","name":"a","arguments":""}}`,
			`{"type":"response.function_call_arguments.delta","output_index":0,"delta":"{}"}`,
		},
	}
	for name, frames := range cases {
		t.Run(name, func(t *testing.T) {
			output, _ := runStream(t, OpenAIResponses, OpenAIChat, frames, StreamOptions{})
			firstChunk := ""
			for _, line := range strings.Split(output, "\n") {
				payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
				if payload != "" && payload != "[DONE]" {
					firstChunk = payload
					break
				}
			}
			if !strings.Contains(firstChunk, `"role":"assistant"`) {
				t.Fatalf("first chunk is not the role chunk: %s", output)
			}
		})
	}
}

func TestStreamDroppedToolArgumentsAreDiagnosed(t *testing.T) {
	// A Chat stream where text starts before tool arguments arrive: the tool
	// block is closed, so the later fragment cannot be delivered.
	frames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_0","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"text"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
	}
	_, diagnostics := runStream(t, OpenAIChat, ClaudeMessages, frames, StreamOptions{})
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "tool_arguments_dropped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("dropped argument fragment was silent: %v", diagnostics)
	}
}

func TestStreamToolDeltaBeforeStartUsesOneItem(t *testing.T) {
	frames := []string{
		`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":1}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"a\":"}}`,
		`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"call_0","name":"a"}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
		`{"type":"message_delta","delta":{"stop_reason":"tool_use"}}`,
	}
	converter, err := NewStreamConverter(ClaudeMessages, OpenAIResponses, StreamOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var items []map[string]any
	collect := func(emitted []Frame) {
		for _, itemFrame := range emitted {
			if itemFrame.Event != "response.output_item.added" {
				continue
			}
			var event map[string]any
			if json.Unmarshal(itemFrame.Data, &event) == nil {
				if item, ok := event["item"].(map[string]any); ok {
					items = append(items, item)
				}
			}
		}
	}
	for _, payload := range frames {
		emitted, err := converter.Feed([]byte(payload))
		if err != nil {
			t.Fatal(err)
		}
		collect(emitted)
	}
	// The Responses target announces tool items at Finish.
	tail, err := converter.Finish()
	if err != nil {
		t.Fatal(err)
	}
	collect(tail)
	if len(items) != 1 {
		t.Fatalf("tool call produced %d items, want 1: %v", len(items), items)
	}
	if items[0]["call_id"] != "call_0" || items[0]["name"] != "a" {
		t.Fatalf("late start did not adopt the item: %v", items[0])
	}
}

// ---- Thought signature preservation (Gemini 3 requirement) ----

func TestGeminiThoughtSignaturePreservedInRequests(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{"q":"x"}},"thoughtSignature":"SIG-ABC"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"lookup","response":{"result":"found"}}}]}
	]}`)
	converted, diagnostics, err := ConvertRequest(Gemini, Gemini, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), "SIG-ABC") {
		t.Fatalf("signature lost on a same-dialect pass: %s (%v)", converted, diagnostics)
	}

	// Cross-dialect: the target cannot carry it, so it must be diagnosed, not
	// silently dropped.
	for _, target := range []Format{OpenAIChat, ClaudeMessages, OpenAIResponses} {
		out, diags, err := ConvertRequest(Gemini, target, body, ConvertOptions{Model: "m"})
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		found := false
		for _, d := range diags {
			if d.Code == "thought_signature_dropped" {
				found = true
			}
		}
		if !found {
			t.Fatalf("%s: signature dropped without a diagnostic: %s (%v)", target, out, diags)
		}
	}
}

func TestGeminiThoughtSignatureSurvivesRoundTripThroughConcepts(t *testing.T) {
	// A Gemini -> Chat -> Gemini round trip cannot keep the signature (Chat has
	// no field for it), so the diagnostic is the contract; a same-dialect
	// conversion must keep it byte-for-byte.
	body := []byte(`{"contents":[{"role":"model","parts":[{"text":"thinking","thought":true,"thoughtSignature":"SIG-XYZ"}]}]}`)
	converted, _, err := ConvertRequest(Gemini, Gemini, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), "SIG-XYZ") || !strings.Contains(string(converted), `"thought":true`) {
		t.Fatalf("thinking signature or flag lost: %s", converted)
	}
}

func TestGeminiStreamKeepsThoughtSignature(t *testing.T) {
	frames := []string{
		`{"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"lookup","args":{}},"thoughtSignature":"SIG-STREAM"}]},"finishReason":"STOP","index":0}]}`,
	}
	// Gemini -> Gemini streaming is passthrough, so exercise the decoder plus
	// the Gemini encoder through a Chat-free path: feed the frame and check the
	// canonical state kept the signature.
	converter, err := NewStreamConverter(Gemini, OpenAIChat, StreamOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := converter.Feed([]byte(frames[0])); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, d := range converter.Diagnostics() {
		if d.Code == "thought_signature_dropped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("streaming loss not diagnosed: %v", converter.Diagnostics())
	}
}
