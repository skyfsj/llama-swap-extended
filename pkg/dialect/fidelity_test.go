package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func TestStream_GeminiToolIdentity(t *testing.T) {
	converter, err := dialect.NewStreamConverter(dialect.Gemini, dialect.OpenAIChat, dialect.StreamOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	frames, err := converter.Feed([]byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"id":"provided-id","name":"read","args":{"q":"x"}}}]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, frame := range frames {
		output.Write(frame.Data)
	}
	if !strings.Contains(output.String(), `"id":"provided-id"`) {
		t.Fatalf("call id lost: %s", output.String())
	}
}

func TestStream_GeminiReasoningSignature(t *testing.T) {
	converter, err := dialect.NewStreamConverter(dialect.GeminiInteractions, dialect.Gemini, dialect.StreamOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, input := range []string{
		`{"event_type":"interaction.created","interaction":{"id":"i","model":"m"}}`,
		`{"event_type":"step.start","index":0,"step":{"type":"thought","signature":"signature-value"}}`,
		`{"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"thinking"}}}`,
		`{"event_type":"interaction.completed","interaction":{"status":"completed"}}`,
	} {
		frames, err := converter.Feed([]byte(input))
		if err != nil {
			t.Fatal(err)
		}
		for _, frame := range frames {
			output.Write(frame.Data)
		}
	}
	if !strings.Contains(output.String(), `"thoughtSignature":"signature-value"`) || !strings.Contains(output.String(), "thinking") {
		t.Fatalf("signature lost: %s", output.String())
	}
}

func TestConvert_ResponseStreamLifecycle(t *testing.T) {
	for _, target := range []dialect.Format{dialect.OpenAIChat, dialect.OpenAIResponses, dialect.ClaudeMessages, dialect.Gemini, dialect.GeminiInteractions} {
		t.Run(string(target), func(t *testing.T) {
			frames, _, err := dialect.ConvertResponseToStream(dialect.OpenAIChat, target, []byte(`{"id":"c","model":"m","choices":[{"message":{"content":"answer","tool_calls":[{"id":"call-a","function":{"name":"read","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`), dialect.StreamOptions{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for _, frame := range frames {
				if string(frame.Data) != "[DONE]" && !json.Valid(frame.Data) {
					t.Fatalf("invalid frame: %s", frame.Data)
				}
				output.Write(frame.Data)
			}
			if !strings.Contains(output.String(), "answer") || !strings.Contains(output.String(), "read") {
				t.Fatalf("response content lost: %s", output.String())
			}
		})
	}
}

func TestConvert_ThoughtSignatureDiagnostics(t *testing.T) {
	for _, target := range []dialect.Format{dialect.OpenAIChat, dialect.OpenAIResponses, dialect.ClaudeMessages} {
		t.Run(string(target), func(t *testing.T) {
			request := []byte(`{"contents":[{"role":"model","parts":[{"text":"answer","thoughtSignature":"text-signature"}]}]}`)
			_, diagnostics, err := dialect.ConvertRequest(dialect.Gemini, target, request, dialect.ConvertOptions{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, diagnostic := range diagnostics {
				found = found || diagnostic.Code == "thought_signature_dropped"
			}
			if !found {
				t.Fatal("text signature loss was not diagnosed")
			}
			response := []byte(`{"candidates":[{"content":{"parts":[{"text":"answer","thoughtSignature":"text-signature"}]},"finishReason":"STOP"}]}`)
			_, diagnostics, err = dialect.ConvertResponse(dialect.Gemini, target, response)
			if err != nil {
				t.Fatal(err)
			}
			found = false
			for _, diagnostic := range diagnostics {
				found = found || diagnostic.Code == "thought_signature_dropped"
			}
			if !found {
				t.Fatal("response signature loss was not diagnosed")
			}
		})
	}
}

// TestConvert_GeminiReplayWithoutSignatureGetsPlaceholder pins the fix for a
// replayed tool call arriving from a dialect that cannot carry Google thought
// signatures: Gemini 3 rejects such a turn with "Function call is missing a
// thought_signature in functionCall parts", which reaches the client as an
// opaque ModelProtocolUnsupported error. The placeholder keeps the turn valid.
func TestConvert_GeminiReplayWithoutSignatureGetsPlaceholder(t *testing.T) {
	for _, source := range []dialect.Format{dialect.OpenAIChat, dialect.OpenAIResponses, dialect.ClaudeMessages} {
		t.Run(string(source), func(t *testing.T) {
			var request string
			switch source {
			case dialect.OpenAIChat:
				request = `{"messages":[
					{"role":"user","content":"what time is it"},
					{"role":"assistant","tool_calls":[{"id":"c1","type":"function","function":{"name":"get_time","arguments":"{}"}}]},
					{"role":"tool","tool_call_id":"c1","content":"12:00"}]}`
			case dialect.OpenAIResponses:
				request = `{"input":[
					{"role":"user","content":"what time is it"},
					{"type":"function_call","call_id":"c1","name":"get_time","arguments":"{}"},
					{"type":"function_call_output","call_id":"c1","output":"12:00"}]}`
			case dialect.ClaudeMessages:
				request = `{"messages":[
					{"role":"user","content":"what time is it"},
					{"role":"assistant","content":[{"type":"tool_use","id":"c1","name":"get_time","input":{}}]},
					{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"12:00"}]}]}`
			}
			body, diagnostics, err := dialect.ConvertRequest(source, dialect.Gemini, []byte(request), dialect.ConvertOptions{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			var converted map[string]any
			if err := json.Unmarshal(body, &converted); err != nil {
				t.Fatal(err)
			}
			contents, _ := converted["contents"].([]any)
			foundCall, foundSignature := false, false
			for _, turn := range contents {
				object, _ := turn.(map[string]any)
				parts, _ := object["parts"].([]any)
				for _, entry := range parts {
					part, _ := entry.(map[string]any)
					if part["functionCall"] == nil {
						continue
					}
					foundCall = true
					if part["thoughtSignature"] == "skip_thought_signature_validator" {
						foundSignature = true
					}
				}
			}
			if !foundCall {
				t.Fatalf("replayed function call missing from contents: %s", body)
			}
			if !foundSignature {
				t.Fatalf("missing thought signature was not backfilled: %s", body)
			}
			foundDiagnostic := false
			for _, diagnostic := range diagnostics {
				foundDiagnostic = foundDiagnostic || diagnostic.Code == "thought_signature_placeholder"
			}
			if !foundDiagnostic {
				t.Fatalf("placeholder backfill was not diagnosed: %v", diagnostics)
			}
		})
	}
}

// TestConvert_GeminiReplayPreservesRealSignature guards the placeholder from
// overwriting a signature the conversation still carries.
func TestConvert_GeminiReplayPreservesRealSignature(t *testing.T) {
	request := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"what time is it"}]},
		{"role":"model","parts":[{"functionCall":{"name":"get_time","args":{}},"thoughtSignature":"real-signature"}]},
		{"role":"user","parts":[{"functionResponse":{"name":"get_time","response":{"result":"12:00"}}}]}]}`)
	body, _, err := dialect.ConvertRequest(dialect.Gemini, dialect.Gemini, request, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"thoughtSignature":"real-signature"`) {
		t.Fatalf("real signature was replaced: %s", body)
	}
	if strings.Contains(string(body), "skip_thought_signature_validator") {
		t.Fatalf("placeholder leaked over a real signature: %s", body)
	}
}
