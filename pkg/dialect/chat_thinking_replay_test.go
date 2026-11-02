package dialect

import (
	"encoding/json"
	"strings"
	"testing"
)

// Two shapes a converted transcript produced in production, both rejected by
// the upstream with a 400 that names neither the offending item nor the fix.

// A Responses tool output can be an image — a screenshot tool returns exactly
// that. Chat keeps a tool message to text, and dropping the image left the
// model with an empty result, so the turn lost what the tool produced.
func TestResponsesToolOutputImageReachesChat(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"screenshot it"}]},
		{"type":"function_call","call_id":"c1","name":"screenshot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"input_image","image_url":"data:image/jpeg;base64,/9j/4AAQSkZJRgABAQ=="}]}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), "data:image/jpeg;base64,/9j/4AAQSkZJRgABAQ==") {
		t.Fatalf("the tool-result image was dropped: %s (%v)", converted, diagnostics)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "tool_result_part_dropped" {
			t.Fatalf("dropping the image was still reported: %v", diagnostics)
		}
	}
	messages := messagesOf(t, converted)
	assertChatPairingValid(t, messages, diagnostics)
	// The image cannot ride in the tool message, so it follows as user content
	// and the tool message itself stays valid.
	if messages[1]["role"] != "assistant" || messages[2]["role"] != "tool" {
		t.Fatalf("the call/result pair was disturbed: %s", converted)
	}
	if messages[3]["role"] != "user" {
		t.Fatalf("the image did not follow as user content: %s", converted)
	}
}

// Text and image in one tool output must both survive.
func TestResponsesToolOutputKeepsTextAndImage(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"go"}]},
		{"type":"function_call","call_id":"c1","name":"shot","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":[
			{"type":"input_text","text":"captured"},
			{"type":"input_image","image_url":"data:image/png;base64,iVBORw0KGgo="}]}]}`)
	converted, _, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"captured", "iVBORw0KGgo="} {
		if !strings.Contains(string(converted), want) {
			t.Fatalf("%q was lost from the tool output: %s", want, converted)
		}
	}
}

// DeepSeek in thinking mode rejects a replayed assistant turn that carries
// tool_calls but no reasoning_content ("The `reasoning_content` in the thinking
// mode must be passed back to the API"). A client that never received the
// reasoning has nothing to send back, so the field must be present and empty.
func TestChatReplayedToolCallCarriesReasoningField(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{\"q\":\"x\"}"},
		{"type":"function_call_output","call_id":"c1","output":"found"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for index, message := range messagesOf(t, converted) {
		if message["role"] != "assistant" || len(arrayOf(message["tool_calls"])) == 0 {
			continue
		}
		value, present := message["reasoning_content"]
		if !present {
			compact, _ := json.Marshal(message)
			t.Fatalf("message[%d] replays a tool call with no reasoning_content: %s (%v)",
				index, compact, diagnostics)
		}
		if _, ok := value.(string); !ok {
			t.Fatalf("message[%d] reasoning_content must be a string: %v", index, value)
		}
	}
	if !hasDiagnostic(diagnostics, "tool_call_reasoning_filled") {
		t.Fatalf("the substitution was silent: %v", diagnostics)
	}
}

// Real reasoning must never be overwritten by the filler.
func TestChatReplayedToolCallKeepsItsReasoning(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"real thought"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"found"}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := messagesOf(t, converted)
	found := false
	for _, message := range messages {
		if message["role"] != "assistant" || len(arrayOf(message["tool_calls"])) == 0 {
			continue
		}
		found = true
		if message["reasoning_content"] != "real thought" {
			t.Fatalf("the real reasoning was not preserved: %s", converted)
		}
	}
	if !found {
		t.Fatalf("the tool call turn is missing: %s", converted)
	}
	if hasDiagnostic(diagnostics, "tool_call_reasoning_filled") {
		t.Fatalf("a turn that carries reasoning was reported as filled: %v", diagnostics)
	}
}

// A plain assistant message is not subject to the rule.
func TestChatPlainAssistantMessageGetsNoReasoningField(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]}]}`)
	converted, _, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converted), "reasoning_content") {
		t.Fatalf("a message without tool calls was given reasoning_content: %s", converted)
	}
}
