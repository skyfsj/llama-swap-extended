package dialect

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestInteractionsRouteRecognition(t *testing.T) {
	for _, path := range []string{"/v1beta/interactions", "/v1/interactions", "/v/interactions"} {
		route, ok := RouteForRequest("POST", path)
		if !ok || route.Format != GeminiInteractions {
			t.Fatalf("RouteForRequest(%q) = %+v, %t", path, route, ok)
		}
	}
	target, err := TargetPath(GeminiInteractions, "m", false)
	if err != nil || target != "/v1beta/interactions" {
		t.Fatalf("TargetPath = %q, %v", target, err)
	}
}

func TestInteractionsRequestDecodeStringAndContent(t *testing.T) {
	// A string input is the SDK's simplest call shape.
	body := []byte(`{"model":"gemini-3.6-flash","input":"hello","system_instruction":"be brief","generation_config":{"temperature":0.4,"max_output_tokens":64,"thinking_level":"high"}}`)
	request, diagnostics, err := DecodeRequest(GeminiInteractions, body)
	if err != nil {
		t.Fatalf("decode: %v (%v)", err, diagnostics)
	}
	if request.Model != "gemini-3.6-flash" || len(request.Messages) != 1 {
		t.Fatalf("request = %+v", request)
	}
	if request.Messages[0].Parts[0].Text != "hello" || request.Messages[0].Role != RoleUser {
		t.Fatalf("message = %+v", request.Messages[0])
	}
	if len(request.System) != 1 || request.System[0].Text != "be brief" {
		t.Fatalf("system = %+v", request.System)
	}
	if request.MaxTokens == nil || *request.MaxTokens != 64 || request.Temperature == nil {
		t.Fatalf("generation config lost: %+v", request)
	}
	if request.Reasoning == nil || request.Reasoning.Effort != "high" {
		t.Fatalf("thinking_level lost: %+v", request.Reasoning)
	}
}

func TestInteractionsRequestDecodeSteps(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"user_input","content":[{"type":"text","text":"what is 2+2?"}]},
		{"type":"thought","summary":[{"type":"text","text":"think"}],"signature":"SIG-1"},
		{"type":"function_call","id":"call_1","name":"calc","arguments":{"expr":"2+2"}},
		{"type":"function_result","call_id":"call_1","name":"calc","result":"4"},
		{"type":"user_input","content":[{"type":"text","text":"thanks"}]}
	],"tools":[{"function":{"name":"calc","description":"compute","parameters":{"type":"object"}}}]}`)
	request, diagnostics, err := DecodeRequest(GeminiInteractions, body)
	if err != nil {
		t.Fatalf("decode: %v (%v)", err, diagnostics)
	}
	if len(request.Messages) != 5 {
		t.Fatalf("messages = %d: %+v", len(request.Messages), request.Messages)
	}
	if request.Messages[0].Role != RoleUser || request.Messages[1].Role != RoleAssistant {
		t.Fatalf("roles = %+v", request.Messages)
	}
	thought := request.Messages[1].Parts[0]
	if thought.Kind != PartReasoning || thought.Text != "think" || thought.ThoughtSignature != "SIG-1" {
		t.Fatalf("thought step = %+v", thought)
	}
	call := request.Messages[2].Parts[0]
	if call.Kind != PartToolCall || call.ToolCallID != "call_1" || call.ToolName != "calc" {
		t.Fatalf("function_call step = %+v", call)
	}
	if string(call.Arguments) != `{"expr":"2+2"}` {
		t.Fatalf("arguments = %s", call.Arguments)
	}
	result := request.Messages[3].Parts[0]
	if result.Kind != PartToolResult || result.ToolCallID != "call_1" || result.Content[0].Text != "4" {
		t.Fatalf("function_result step = %+v", result)
	}
	if len(request.Tools) != 1 || request.Tools[0].Name != "calc" {
		t.Fatalf("tools = %+v", request.Tools)
	}
}

func TestInteractionsToChatConversion(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","system_instruction":"brief","generation_config":{"max_output_tokens":32,"temperature":0.1},"response_mime_type":"application/json"}`)
	converted, diagnostics, err := ConvertRequest(GeminiInteractions, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v (%v)", err, diagnostics)
	}
	object := decodeMap(t, converted)
	if object["model"] != "m" {
		t.Fatalf("model = %v", object["model"])
	}
	messages := object["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %v", messages)
	}
	if messages[0].(map[string]any)["role"] != "system" || messages[0].(map[string]any)["content"] != "brief" {
		t.Fatalf("system lost: %v", messages[0])
	}
	format, ok := object["response_format"].(map[string]any)
	if !ok || format["type"] != "json_object" {
		t.Fatalf("response_mime_type lost: %v", object["response_format"])
	}
	if object["max_tokens"] != float64(32) {
		t.Fatalf("max_output_tokens lost: %v", object["max_tokens"])
	}
}

func TestInteractionsStatefulFieldsAreReported(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","previous_interaction_id":"v1_abc","cached_content":"caches/1","store":true,"background":true}`)
	// Same dialect keeps them; a different dialect must diagnose the loss.
	same, _, err := ConvertRequest(GeminiInteractions, GeminiInteractions, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	text := string(same)
	for _, must := range []string{"v1_abc", "caches/1"} {
		if !strings.Contains(text, must) {
			t.Fatalf("stateful field %q lost on a same-dialect pass: %s", must, text)
		}
	}
	converted, diagnostics, err := ConvertRequest(GeminiInteractions, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	codes := map[string]bool{}
	for _, diagnostic := range diagnostics {
		codes[diagnostic.Code] = true
	}
	if !codes["field_dropped"] {
		t.Fatalf("stateful fields were dropped silently: %s (%v)", converted, diagnostics)
	}
}

func TestInteractionsResponseDecode(t *testing.T) {
	body := []byte(`{"id":"v1_abc","object":"interaction","status":"completed","model":"gemini-3.6-flash","created":"2026-10-17T12:00:00Z","updated":"2026-10-17T12:00:01Z",
		"steps":[{"type":"thought","summary":[{"type":"text","text":"thinking"}],"signature":"SIG-9"},{"type":"model_output","content":[{"type":"text","text":"Paris"}]}],
		"usage":{"total_input_tokens":7,"total_output_tokens":20,"total_thought_tokens":22,"total_tokens":49,"total_cached_tokens":3}}`)
	response, diagnostics, err := DecodeResponse(GeminiInteractions, body)
	if err != nil {
		t.Fatalf("decode: %v (%v)", err, diagnostics)
	}
	if response.ID != "v1_abc" || response.Model != "gemini-3.6-flash" || response.Status != "completed" {
		t.Fatalf("response = %+v", response)
	}
	if len(response.Parts) != 2 {
		t.Fatalf("parts = %+v", response.Parts)
	}
	if response.Parts[0].Kind != PartReasoning || response.Parts[0].ThoughtSignature != "SIG-9" {
		t.Fatalf("thought = %+v", response.Parts[0])
	}
	if response.Parts[1].Kind != PartText || response.Parts[1].Text != "Paris" {
		t.Fatalf("text = %+v", response.Parts[1])
	}
	if response.Usage.InputTokens != 7 || response.Usage.OutputTokens != 20 || response.Usage.ReasoningTokens != 22 || response.Usage.CachedTokens != 3 {
		t.Fatalf("usage = %+v", response.Usage)
	}
	if response.Usage.TotalTokens != 49 {
		t.Fatalf("total = %d", response.Usage.TotalTokens)
	}
	if response.Created == 0 {
		t.Fatal("created timestamp lost")
	}
}

func TestInteractionsResponseWithToolCallToChat(t *testing.T) {
	body := []byte(`{"id":"v1_x","object":"interaction","status":"completed","model":"m",
		"steps":[{"type":"function_call","id":"call_9","name":"calc","arguments":{"expr":"1+1"}}],
		"usage":{"total_input_tokens":3,"total_output_tokens":5,"total_tokens":8}}`)
	converted, diagnostics, err := ConvertResponse(GeminiInteractions, OpenAIChat, body)
	if err != nil {
		t.Fatalf("convert: %v (%v)", err, diagnostics)
	}
	object := decodeMap(t, converted)
	choice := object["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %v", choice["finish_reason"])
	}
	message := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool calls = %v", message)
	}
	call := calls[0].(map[string]any)
	function := call["function"].(map[string]any)
	if call["id"] != "call_9" || function["name"] != "calc" || function["arguments"] != `{"expr":"1+1"}` {
		t.Fatalf("call = %v", call)
	}
}

func TestChatToInteractionsRequest(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"system","content":"brief"},{"role":"user","content":"hi"}],"max_tokens":16,"temperature":0.5}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, GeminiInteractions, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v (%v)", err, diagnostics)
	}
	object := decodeMap(t, converted)
	if object["system_instruction"] != "brief" {
		t.Fatalf("system_instruction = %v", object["system_instruction"])
	}
	input := object["input"].([]any)
	if len(input) != 1 {
		t.Fatalf("input = %v", input)
	}
	step := input[0].(map[string]any)
	if step["type"] != "user_input" {
		t.Fatalf("step = %v", step)
	}
	config := object["generation_config"].(map[string]any)
	if config["max_output_tokens"] != float64(16) || config["temperature"] != 0.5 {
		t.Fatalf("generation_config = %v", config)
	}
}

func TestInteractionsStreamDecodeTextAndUsage(t *testing.T) {
	frames := []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_s","model":"m","status":"in_progress","created":"2026-10-17T12:00:00Z"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"model_output","content":[]}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"po"}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"ng"}}`,
		`{"event_type":"step.stop","index":0,"usage":{"total_input_tokens":4,"total_output_tokens":2,"total_tokens":6}}`,
		`{"event_type":"interaction.completed","interaction":{"id":"v1_s","status":"completed","usage":{"total_input_tokens":4,"total_output_tokens":2,"total_tokens":6}}}`,
	}
	for _, target := range []Format{OpenAIChat, ClaudeMessages, OpenAIResponses, Gemini} {
		t.Run(string(target), func(t *testing.T) {
			output, diagnostics := runStream(t, GeminiInteractions, target, frames, StreamOptions{IncludeUsage: true})
			if !strings.Contains(output, "po") || !strings.Contains(output, "ng") {
				t.Fatalf("text lost: %s (%v)", output, diagnostics)
			}
			assertTerminalFrame(t, target, output)
		})
	}
}

func TestInteractionsStreamEncodeFromChat(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"po"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ng"},"finish_reason":"stop"}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
	}
	output, diagnostics := runStream(t, OpenAIChat, GeminiInteractions, frames, StreamOptions{IncludeUsage: true})
	for _, must := range []string{"interaction.created", "step.start", "step.delta", "step.stop", "interaction.completed"} {
		if !strings.Contains(output, must) {
			t.Fatalf("missing %s: %s (%v)", must, output, diagnostics)
		}
	}
	if !strings.Contains(output, "po") || !strings.Contains(output, "ng") {
		t.Fatalf("text lost: %s", output)
	}
	if !strings.Contains(output, `"total_input_tokens":4`) {
		t.Fatalf("usage lost: %s", output)
	}
}

func TestInteractionsStreamCarriesThoughtSignature(t *testing.T) {
	frames := []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_s","model":"m","status":"in_progress"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call_1","name":"calc","arguments":{}}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"SIG-S"}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"arguments","arguments":"{\"a\":"}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"arguments","arguments":"1}"}}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"interaction.completed","interaction":{"id":"v1_s","status":"completed"}}`,
	}
	// To Gemini: the signature must survive.
	gemini, _ := runStream(t, GeminiInteractions, Gemini, frames, StreamOptions{})
	if !strings.Contains(gemini, "SIG-S") {
		t.Fatalf("signature lost toward Gemini: %s", gemini)
	}
	// To Chat: the signature cannot be carried, so it must be diagnosed.
	_, diagnostics := runStream(t, GeminiInteractions, OpenAIChat, frames, StreamOptions{})
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "thought_signature_dropped" {
			found = true
		}
	}
	if !found {
		t.Fatalf("signature loss not diagnosed: %v", diagnostics)
	}
}

func TestInteractionsStreamToolCallToChat(t *testing.T) {
	frames := []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_s","model":"m","status":"in_progress"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call_1","name":"calc","arguments":{}}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"arguments","arguments":"{\"a\":1}"}}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"interaction.completed","interaction":{"id":"v1_s","status":"completed"}}`,
	}
	chat, diagnostics := runStream(t, GeminiInteractions, OpenAIChat, frames, StreamOptions{})
	if !strings.Contains(chat, `"tool_calls"`) || !strings.Contains(chat, `"calc"`) {
		t.Fatalf("tool call lost: %s (%v)", chat, diagnostics)
	}
	if !strings.Contains(chat, `"finish_reason":"tool_calls"`) {
		t.Fatalf("finish reason not tool_calls: %s", chat)
	}
}

func TestInteractionsStreamErrorSurfaces(t *testing.T) {
	frames := []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_s","model":"m","status":"in_progress"}}`,
		`{"event_type":"error","error":{"code":500,"message":"boom"}}`,
	}
	chat, _ := runStream(t, GeminiInteractions, OpenAIChat, frames, StreamOptions{})
	if !strings.Contains(chat, "boom") {
		t.Fatalf("error swallowed: %s", chat)
	}
}

func TestInteractionsJSONShapeIsSDKParseable(t *testing.T) {
	// The SDK's pydantic models are strict: required fields must exist and
	// types must match. This asserts the wire shape the SDK expects.
	converted, _, err := ConvertRequest(OpenAIChat, GeminiInteractions,
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"max_tokens":8}`),
		ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(converted, &object); err != nil {
		t.Fatal(err)
	}
	if _, ok := object["model"].(string); !ok {
		t.Fatalf("model must be a string: %s", converted)
	}
	if _, ok := object["input"].([]any); !ok {
		t.Fatalf("input must be an array of steps: %s", converted)
	}
}

// ---- Round-2 review findings ----

func TestInteractionsContentArrayInputIsNotSteps(t *testing.T) {
	// A documented Content[] input array: every Content carries a `type`, which
	// used to be misread as a Step and dropped entirely.
	body := []byte(`{"model":"m","input":[{"type":"text","text":"hello"},{"type":"image","data":"QUJD","mime_type":"image/png"}]}`)
	request, diagnostics, err := DecodeRequest(GeminiInteractions, body)
	if err != nil {
		t.Fatalf("decode: %v (%v)", err, diagnostics)
	}
	if len(request.Messages) != 1 {
		t.Fatalf("messages = %+v", request.Messages)
	}
	parts := request.Messages[0].Parts
	if len(parts) != 2 || parts[0].Text != "hello" || parts[1].Kind != PartImage {
		t.Fatalf("parts = %+v", parts)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "unknown_step_dropped" {
			t.Fatalf("content was treated as a step: %v", diagnostics)
		}
	}
}

func TestInteractionsToolResultFromClaudeUserTurn(t *testing.T) {
	// Claude and Gemini put tool results inside user turns; an Interactions
	// transcript needs a function_result step for each.
	claude := []byte(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"tool_use","id":"t1","name":"lookup","input":{"q":"x"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"found"},{"type":"text","text":"continue"}]}
	]}`)
	converted, diagnostics, err := ConvertRequest(ClaudeMessages, GeminiInteractions, claude, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v (%v)", err, diagnostics)
	}
	text := string(converted)
	if !strings.Contains(text, `"function_result"`) || !strings.Contains(text, "found") {
		t.Fatalf("tool result lost: %s (%v)", text, diagnostics)
	}
	if !strings.Contains(text, `"call_id":"t1"`) {
		t.Fatalf("tool result is not paired: %s", text)
	}
	// The result must precede the remaining user text.
	if strings.Index(text, "function_result") > strings.Index(text, "continue") {
		t.Fatalf("result must precede the user text: %s", text)
	}
}

func TestInteractionsRequiresActionMapsToToolUse(t *testing.T) {
	body := []byte(`{"id":"v1_x","object":"interaction","status":"requires_action","model":"m",
		"steps":[{"type":"function_call","id":"call_1","name":"lookup","arguments":{"q":"x"}}]}`)
	converted, _, err := ConvertResponse(GeminiInteractions, OpenAIChat, body)
	if err != nil {
		t.Fatal(err)
	}
	choice := decodeMap(t, converted)["choices"].([]any)[0].(map[string]any)
	if choice["finish_reason"] != "tool_calls" {
		t.Fatalf("requires_action must report tool_calls, got %v", choice["finish_reason"])
	}
}

func TestInteractionsNonSuccessStatusesAreNotSuccesses(t *testing.T) {
	for _, status := range []string{"cancelled", "queued", "in_progress", "budget_exceeded"} {
		body := []byte(`{"id":"v1_x","object":"interaction","status":"` + status + `","model":"m","steps":[]}`)
		response, _, err := DecodeResponse(GeminiInteractions, body)
		if err != nil {
			t.Fatalf("%s: %v", status, err)
		}
		if response.Status == "completed" || response.Error == nil {
			t.Fatalf("%s must be reported as an error, not a completion: %+v", status, response)
		}
	}
}

func TestInteractionsStatefulFieldsDiagnosedOnEveryTarget(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","previous_interaction_id":"v1_abc","cached_content":"caches/1","background":true}`)
	for _, target := range []Format{OpenAIChat, ClaudeMessages, OpenAIResponses, Gemini} {
		converted, diagnostics, err := ConvertRequest(GeminiInteractions, target, body, ConvertOptions{Model: "m"})
		if err != nil {
			t.Fatalf("%s: %v", target, err)
		}
		codes := map[string]bool{}
		for _, diagnostic := range diagnostics {
			codes[diagnostic.Code] = true
		}
		if !codes["field_dropped"] && !codes["cached_content_dropped"] {
			t.Fatalf("%s dropped the stateful fields silently: %s (%v)", target, converted, diagnostics)
		}
	}
}

func TestInteractionsOrphanFunctionResultIsDropped(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"functionResponse":{"name":"ghost","response":{"result":"x"}}}]}]}`)
	converted, diagnostics, err := ConvertRequest(Gemini, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converted), `"tool_use_id":""`) {
		t.Fatalf("orphan result kept an empty id: %s", converted)
	}
	if strings.Contains(string(converted), "tool_result") {
		t.Fatalf("an unpaired tool_result must not reach Anthropic: %s", converted)
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

func TestInteractionsStreamArgumentsDeltaDiscriminator(t *testing.T) {
	// The SDK's union names the streamed tool arguments "arguments_delta".
	frames := []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_s","model":"m","status":"in_progress"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call_1","name":"calc","arguments":{}}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"a\":1}"}}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"interaction.completed","interaction":{"id":"v1_s","status":"requires_action"}}`,
	}
	chat, diagnostics := runStream(t, GeminiInteractions, OpenAIChat, frames, StreamOptions{})
	if !strings.Contains(chat, `"{\"a\":1}"`) {
		t.Fatalf("arguments lost: %s (%v)", chat, diagnostics)
	}
	if !strings.Contains(chat, `"finish_reason":"tool_calls"`) {
		t.Fatalf("requires_action must finish as tool_calls: %s", chat)
	}
	// The encoder must emit the SDK's discriminator, not the step field name.
	back, _ := runStream(t, OpenAIChat, GeminiInteractions, []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"calc","arguments":"{}"}}]}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}, StreamOptions{})
	if strings.Contains(back, `"type":"arguments"`) || !strings.Contains(back, "arguments_delta") {
		t.Fatalf("encoder used the wrong delta discriminator: %s", back)
	}
}

func TestInteractionsStreamLengthStatusIsIncomplete(t *testing.T) {
	frames := []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"length"}]}`,
	}
	output, _ := runStream(t, OpenAIChat, GeminiInteractions, frames, StreamOptions{})
	if !strings.Contains(output, `"status":"incomplete"`) {
		t.Fatalf("a length stop must not report completed: %s", output)
	}
}

// ---- Round-3 review findings ----

func TestInteractionsStreamFailureIsNotASuccess(t *testing.T) {
	frames := []string{
		`{"event_type":"interaction.created","interaction":{"id":"v1_s","model":"m","status":"in_progress"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"model_output","content":[]}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"partial"}}`,
		`{"event_type":"step.stop","index":0}`,
		`{"event_type":"interaction.completed","interaction":{"id":"v1_s","status":"cancelled","errors":[{"message":"cancelled by user"}]}}`,
	}
	chat, _ := runStream(t, GeminiInteractions, OpenAIChat, frames, StreamOptions{})
	if strings.Contains(chat, `"finish_reason":"stop"`) {
		t.Fatalf("a cancelled interaction finished as a normal stop: %s", chat)
	}
	if !strings.Contains(chat, "cancelled") {
		t.Fatalf("the cancellation was not surfaced: %s", chat)
	}
	// An error terminal frame must not be followed by a completed status.
	errorFrames := []string{
		`{"type":"error","error":{"type":"overloaded_error","message":"boom"}}`,
	}
	out, _ := runStream(t, ClaudeMessages, GeminiInteractions, errorFrames, StreamOptions{})
	if !strings.Contains(out, `"status":"failed"`) {
		t.Fatalf("an error stream ended as completed: %s", out)
	}
}

func TestInteractionsStreamNonSuccessStatusesAreErrors(t *testing.T) {
	for _, status := range []string{"cancelled", "failed", "queued", "in_progress", "budget_exceeded", "requires_action"} {
		frames := []string{
			`{"event_type":"interaction.created","interaction":{"id":"v1_s","model":"m","status":"in_progress"}}`,
			`{"event_type":"interaction.completed","interaction":{"id":"v1_s","status":"` + status + `"}}`,
		}
		chat, _ := runStream(t, GeminiInteractions, OpenAIChat, frames, StreamOptions{})
		if strings.Contains(chat, `"finish_reason":"stop"`) {
			t.Fatalf("stream status %q finished as a normal stop: %s", status, chat)
		}
		if !strings.Contains(chat, `"error"`) {
			t.Fatalf("stream status %q produced no error frame: %s", status, chat)
		}
		// A second target proves the error is not an artifact of the Chat
		// encoding: a Responses client must see a failure too.
		responses, _ := runStream(t, GeminiInteractions, OpenAIResponses, frames, StreamOptions{})
		if strings.Contains(responses, `"status":"completed"`) {
			t.Fatalf("stream status %q ended as completed: %s", status, responses)
		}
		if !strings.Contains(responses, "response.failed") {
			t.Fatalf("stream status %q produced no terminal failure: %s", status, responses)
		}
	}
}

func TestInteractionsToolChoiceModePrecedence(t *testing.T) {
	// The mode decides first; a single allowed name only narrows "any".
	none := []byte(`{"model":"m","input":"hi","generation_config":{"tool_choice":{"allowed_tools":{"mode":"none","tools":["lookup"]}}},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	converted, _, err := ConvertRequest(GeminiInteractions, OpenAIChat, none, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if choice := decodeMap(t, converted)["tool_choice"]; choice != "none" {
		t.Fatalf("mode none with one allowed name became %v", choice)
	}
	validated := []byte(`{"model":"m","input":"hi","generation_config":{"tool_choice":{"allowed_tools":{"mode":"validated","tools":["lookup"]}}},"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`)
	converted, diagnostics, err := ConvertRequest(GeminiInteractions, OpenAIChat, validated, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "tool_choice_approximated" {
			found = true
		}
	}
	if !found {
		t.Fatalf("validated mode was not diagnosed: %s (%v)", converted, diagnostics)
	}
}

func TestInteractionsBareListFunctionResultDecodes(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"user_input","content":[{"type":"text","text":"hi"}]},
		{"type":"function_call","id":"c1","name":"f","arguments":{}},
		{"type":"function_result","call_id":"c1","result":[{"type":"text","text":"4"}]}
	]}`)
	converted, diagnostics, err := ConvertRequest(GeminiInteractions, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v (%v)", err, diagnostics)
	}
	text := string(converted)
	if !strings.Contains(text, `"role":"tool"`) || !strings.Contains(text, `"content":"4"`) {
		t.Fatalf("bare-list result was not rendered as tool content: %s", text)
	}
}

func TestGeminiOrphanDoesNotReserveAnID(t *testing.T) {
	// An orphan result is dropped; it must not consume an id that a later real
	// call would then be paired against.
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"orphan"}}}]},
		{"role":"model","parts":[{"functionCall":{"name":"f","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"f","response":{"result":"real"}}}]}
	]}`)
	converted, _, err := ConvertRequest(Gemini, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(converted, &request); err != nil {
		t.Fatal(err)
	}
	var declared, resultID string
	for _, raw := range request["messages"].([]any) {
		message, _ := raw.(map[string]any)
		content, _ := message["content"].([]any)
		for _, rawBlock := range content {
			block, _ := rawBlock.(map[string]any)
			switch block["type"] {
			case "tool_use":
				declared, _ = block["id"].(string)
			case "tool_result":
				resultID, _ = block["tool_use_id"].(string)
			}
		}
	}
	if declared == "" || declared != resultID {
		t.Fatalf("pairing broken: declared=%q result=%q (%s)", declared, resultID, converted)
	}
}

func TestGeminiStreamErrorCodeHasNoNil(t *testing.T) {
	frames := []string{
		`{"error":{"message":"boom"}}`,
	}
	chat, _ := runStream(t, Gemini, OpenAIChat, frames, StreamOptions{})
	if strings.Contains(chat, "<nil>") {
		t.Fatalf("error code rendered as <nil>: %s", chat)
	}
}
