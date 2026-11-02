package dialect

import (
	"encoding/json"
	"testing"
)

// A Responses transcript records the reasoning item before the assistant
// message and function calls it explains. Encoding that to Chat used to emit a
// standalone assistant message with content null, no tool_calls and only
// reasoning_content, which OpenAI-compatible servers reject with
// "Invalid assistant message: content or tool_calls must be set".
func TestResponsesReasoningAttachesToFollowingAssistantTurn(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"answer"}]},
		{"type":"function_call","call_id":"call_1","name":"lookup","arguments":"{}"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := messagesOf(t, converted)
	assertNoEmptyAssistant(t, messages, diagnostics)

	if len(messages) != 2 {
		t.Fatalf("messages = %d, want user + one assistant: %s", len(messages), converted)
	}
	assistant := messages[1]
	if assistant["reasoning_content"] != "thought" {
		t.Fatalf("reasoning did not follow into the assistant turn: %s", converted)
	}
	if assistant["content"] != "answer" {
		t.Fatalf("assistant content lost: %s", converted)
	}
	if len(arrayOf(assistant["tool_calls"])) != 1 {
		t.Fatalf("tool call separated from its reasoning: %s", converted)
	}
}

// Trailing reasoning with no assistant turn after it cannot be attached; it is
// dropped with a diagnostic instead of becoming an unanswerable 400.
func TestResponsesTrailingReasoningDoesNotEmitEmptyAssistant(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	assertNoEmptyAssistant(t, messagesOf(t, converted), diagnostics)
	if !hasDiagnostic(diagnostics, "reasoning_only_message_dropped") {
		t.Fatalf("dropping trailing reasoning was silent: %v", diagnostics)
	}
}

// The same shape reaches Chat from Claude thinking blocks once a target keeps
// them, so the guard belongs to the encoder rather than to one decoder.
func TestChatEncoderDropsAssistantWithoutContentOrCalls(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null},
		{"role":"assistant","content":"answer"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	assertNoEmptyAssistant(t, messagesOf(t, converted), diagnostics)
	if !hasDiagnostic(diagnostics, "empty_assistant_message_dropped") {
		t.Fatalf("dropping an empty assistant turn was silent: %v", diagnostics)
	}
}

func messagesOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode converted request: %v (%s)", err, body)
	}
	return parsed.Messages
}

func assertNoEmptyAssistant(t *testing.T, messages []map[string]any, diagnostics []Diagnostic) {
	t.Helper()
	for index, message := range messages {
		if message["role"] != "assistant" {
			continue
		}
		if emptyChatContent(message["content"]) && len(arrayOf(message["tool_calls"])) == 0 {
			compact, _ := json.Marshal(message)
			t.Fatalf("message[%d] is an assistant turn without content or tool_calls: %s (%v)",
				index, compact, diagnostics)
		}
	}
}

func emptyChatContent(content any) bool {
	switch value := content.(type) {
	case nil:
		return true
	case string:
		return value == ""
	case []any:
		return len(value) == 0
	}
	return false
}

func arrayOf(value any) []any {
	array, _ := value.([]any)
	return array
}
