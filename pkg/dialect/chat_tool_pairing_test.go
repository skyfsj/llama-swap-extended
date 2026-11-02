package dialect

import (
	"encoding/json"
	"strings"
	"testing"
)

// Chat pairing rules every OpenAI-compatible server enforces. A Responses
// transcript can violate both, and the upstream answers with a 400 that names
// neither the offending item nor the reason.

func TestChatToolMessageWithoutCallIDDoesNotStayUnpaired(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call_output","name":"deliver","output":"msg"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	assertChatPairingValid(t, messagesOf(t, converted), diagnostics)
	if !hasDiagnostic(diagnostics, "tool_message_unpaired") {
		t.Fatalf("an id-less tool result was replayed unchanged: %s (%v)", converted, diagnostics)
	}
	if !strings.Contains(string(converted), "msg") {
		t.Fatalf("the tool output was lost instead of replayed: %s", converted)
	}
}

// A function_call_output whose owning item was dropped (local_shell_call,
// computer_call) leaves no assistant turn declaring its id.
func TestChatToolMessageForDroppedCallDoesNotStayUnpaired(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"local_shell_call","call_id":"S1","status":"completed","action":{"type":"exec","command":["ls"]}},
		{"type":"function_call_output","call_id":"S1","output":"done"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	assertChatPairingValid(t, messagesOf(t, converted), diagnostics)
	if !hasDiagnostic(diagnostics, "tool_output_unpaired") {
		t.Fatalf("a tool result for a dropped call was replayed unchanged: %s (%v)", converted, diagnostics)
	}
}

// An id-less result that arrives while a call is still unanswered belongs to
// that call, so it keeps its tool-message shape instead of degrading.
func TestChatToolMessageWithoutCallIDMatchesPendingCall(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":12345,"name":"lookup","arguments":"{}"},
		{"type":"function_call_output","output":"found"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := messagesOf(t, converted)
	assertChatPairingValid(t, messages, diagnostics)

	assistant := messages[1]
	calls := arrayOf(assistant["tool_calls"])
	if len(calls) != 1 {
		t.Fatalf("tool call lost: %s", converted)
	}
	callID := calls[0].(map[string]any)["id"]
	if callID == "" {
		t.Fatalf("a call with a non-string id kept an empty id: %s", converted)
	}
	if messages[2]["role"] != "tool" || messages[2]["tool_call_id"] != callID {
		t.Fatalf("the result was not matched to its call: %s", converted)
	}
}

// A result naming a call this conversation never declared is forwarded as the
// client sent it. The encoder deliberately cannot judge that case: it also
// encodes fragments (a lone tool message appended to the next turn), which are
// only valid once stitched into a conversation it never sees.
func TestChatToolMessageForUnknownCallIsForwarded(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"function_call_output","call_id":"ghost","output":"early"},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
	]}`)
	converted, _, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := messagesOf(t, converted)
	if messages[0]["role"] != "tool" || messages[0]["tool_call_id"] != "ghost" {
		t.Fatalf("a declared id was rewritten: %s", converted)
	}
}

// Reasoning merged from two turns of one assistant message keeps a paragraph
// break, matching how a carried-over block is joined.
func TestChatMergedReasoningKeepsParagraphBreak(t *testing.T) {
	body := []byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":null,"reasoning_content":"first","tool_calls":[{"id":"a","type":"function","function":{"name":"f","arguments":"{}"}}]},
		{"role":"assistant","content":null,"reasoning_content":"second","tool_calls":[{"id":"b","type":"function","function":{"name":"g","arguments":"{}"}}]}
	]}`)
	converted, _, err := ConvertRequest(OpenAIChat, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := messagesOf(t, converted)
	if len(messages) != 2 {
		t.Fatalf("assistant turns did not merge: %s", converted)
	}
	if got := messages[1]["reasoning_content"]; got != "first\nsecond" {
		t.Fatalf("reasoning_content = %q, want the blocks kept apart: %s", got, converted)
	}
}

func assertChatPairingValid(t *testing.T, messages []map[string]any, diagnostics []Diagnostic) {
	t.Helper()
	declared := map[string]bool{}
	for index, message := range messages {
		switch message["role"] {
		case "assistant":
			for _, rawCall := range arrayOf(message["tool_calls"]) {
				call, ok := rawCall.(map[string]any)
				if !ok {
					continue
				}
				id, _ := call["id"].(string)
				if id == "" {
					compact, _ := json.Marshal(message)
					t.Fatalf("message[%d] declares a tool call with no id: %s (%v)", index, compact, diagnostics)
				}
				declared[id] = true
			}
		case "tool":
			id, _ := message["tool_call_id"].(string)
			if id == "" || !declared[id] {
				compact, _ := json.Marshal(message)
				t.Fatalf("message[%d] answers no declared tool call: %s (%v)", index, compact, diagnostics)
			}
		}
	}
}
