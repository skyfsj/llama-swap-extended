package dialect

import (
	"encoding/json"
	"strings"
	"testing"
)

// Anthropic rejects a tool_result whose tool_use_id no tool_use declares. The
// orphan reaches here from a Responses function_call_output whose owning item
// could not be represented — the decoder is the only place that knows the call
// was dropped, so it replays the output as user content for every target.
func TestClaudeToolResultForDroppedCallDoesNotStayUnpaired(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"local_shell_call","call_id":"S1","status":"completed","action":{"type":"exec","command":["ls"]}},
		{"type":"function_call_output","call_id":"S1","output":"early"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := claudeMessagesOf(t, converted)
	assertClaudePairingValid(t, messages, diagnostics)
	if !hasDiagnostic(diagnostics, "tool_output_unpaired") {
		t.Fatalf("an unpaired tool result was replayed unchanged: %s (%v)", converted, diagnostics)
	}
	if !strings.Contains(string(converted), "early") {
		t.Fatalf("the tool output was lost instead of replayed: %s", converted)
	}
}

// A properly paired transcript must be left exactly as it was.
func TestClaudeToolResultWithToolUseIsKept(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"function_call","call_id":"c1","name":"lookup","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"found"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	messages := claudeMessagesOf(t, converted)
	assertClaudePairingValid(t, messages, diagnostics)
	if hasDiagnostic(diagnostics, "tool_result_unpaired") {
		t.Fatalf("a paired tool result was reported unpaired: %s (%v)", converted, diagnostics)
	}
	if !strings.Contains(string(converted), `"tool_use_id":"c1"`) {
		t.Fatalf("the paired result lost its tool_use_id: %s", converted)
	}
}

func claudeMessagesOf(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var parsed struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("decode converted request: %v (%s)", err, body)
	}
	return parsed.Messages
}

func assertClaudePairingValid(t *testing.T, messages []map[string]any, diagnostics []Diagnostic) {
	t.Helper()
	declared := map[string]bool{}
	for index, message := range messages {
		blocks, _ := message["content"].([]any)
		for _, rawBlock := range blocks {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				continue
			}
			switch block["type"] {
			case "tool_use":
				if id, _ := block["id"].(string); id != "" {
					declared[id] = true
				}
			case "tool_result":
				id, _ := block["tool_use_id"].(string)
				if id == "" || !declared[id] {
					compact, _ := json.Marshal(message)
					t.Fatalf("messages[%d] has a tool_result no tool_use declares: %s (%v)",
						index, compact, diagnostics)
				}
			}
		}
	}
}
