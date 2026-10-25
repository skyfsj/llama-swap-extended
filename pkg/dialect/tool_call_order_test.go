package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// An upstream may stream arguments, and even the call id, before the frame
// that names the call. Every target must still deliver one dispatchable call
// carrying the name and the complete argument text, and no delta may precede
// the call it belongs to.
func TestStreamChat_ToolCallNamedAfterItsArguments(t *testing.T) {
	for _, target := range []dialect.Format{
		dialect.OpenAIResponses,
		dialect.ClaudeMessages,
		dialect.Gemini,
		dialect.GeminiInteractions,
	} {
		t.Run(string(target), func(t *testing.T) {
			converter, err := dialect.NewStreamConverter(dialect.OpenAIChat, target, dialect.StreamOptions{Model: "m"})
			if err != nil {
				t.Fatal(err)
			}
			var output strings.Builder
			for _, frame := range []string{
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"arguments":"{\"q\":"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"read","arguments":"\"x\"}"}}]}}]}`,
				`{"id":"c1","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			} {
				emitted, err := converter.Feed([]byte(frame))
				if err != nil {
					t.Fatalf("stream failed on a call named after its arguments: %v", err)
				}
				for _, item := range emitted {
					output.Write(item.Data)
				}
			}
			tail, err := converter.Finish()
			if err != nil {
				t.Fatal(err)
			}
			for _, item := range tail {
				output.Write(item.Data)
			}
			text := output.String()
			if !strings.Contains(text, "read") {
				t.Fatalf("the call name never reached the client: %s", text)
			}
			// The arguments arrive either as an escaped JSON string or, for
			// Gemini, as the decoded object; both carry the same two members.
			escaped := strings.Contains(text, `\"q\":\"x\"`)
			decoded := strings.Contains(text, `"q":"x"`)
			if !escaped && !decoded {
				t.Fatalf("the complete argument text never reached the client: %s", text)
			}
			if strings.Contains(text, `"name":""`) {
				t.Fatalf("the call was announced without its name: %s", text)
			}
		})
	}
}

// A Responses stream that dies mid-turn leaves items the client never finished
// receiving; their terminal status must not read as completed output.
func TestStreamResponses_TruncatedItemsAreNotCompleted(t *testing.T) {
	converter, err := dialect.NewStreamConverter(dialect.OpenAIChat, dialect.OpenAIResponses, dialect.StreamOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	var output strings.Builder
	for _, frame := range []string{
		`{"id":"c1","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"id":"c1","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"read","arguments":"{\"q\":"}}]}}]}`,
	} {
		emitted, err := converter.Feed([]byte(frame))
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range emitted {
			output.Write(item.Data)
		}
	}
	tail, _ := converter.Finish()
	for _, item := range tail {
		output.Write(item.Data)
	}
	text := output.String()
	if !strings.Contains(text, `"code":"stream_truncated"`) {
		t.Fatalf("a truncated stream was not reported: %s", text)
	}
	if strings.Contains(text, `"status":"completed"`) {
		t.Fatalf("a truncated turn reported completed items: %s", text)
	}
	if !strings.Contains(text, `"status":"incomplete"`) {
		t.Fatalf("truncated items are not marked incomplete: %s", text)
	}
}

// Every announced Responses item must be closed exactly once, and a done must
// never appear for an item that was never added.
func TestStreamResponses_ItemLifecycleIsBalanced(t *testing.T) {
	converter, err := dialect.NewStreamConverter(dialect.Gemini, dialect.OpenAIResponses, dialect.StreamOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	// The Gemini frame names the call, so the item is announced with its name.
	frames, err := converter.Feed([]byte(`{"candidates":[{"content":{"parts":[{"text":"answer"},{"functionCall":{"id":"call_a","name":"read","args":{"q":"x"}}}]},"finishReason":"STOP"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	tail, err := converter.Finish()
	if err != nil {
		t.Fatal(err)
	}
	added, done := 0, 0
	for _, item := range append(frames, tail...) {
		var event map[string]any
		if json.Unmarshal(item.Data, &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.output_item.added":
			added++
		case "response.output_item.done":
			done++
		}
	}
	if added == 0 || added != done {
		t.Fatalf("unbalanced item lifecycle: added=%d done=%d", added, done)
	}
}

// A Responses request declares tools in the top-level array and again through
// additional_tools items inside the input. Flattening both into one list must
// not put the same name there twice, and the first declaration keeps its kind.
func TestDecodeResponses_DuplicateToolAcrossContainers(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"tools":[{"type":"function","name":"apply_patch","parameters":{"type":"object"}}],
		"input":[
			{"type":"additional_tools","tools":[{"type":"custom","name":"apply_patch","format":{"type":"text"}}]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}
		]
	}`)
	request, diagnostics, err := dialect.DecodeRequest(dialect.OpenAIResponses, body)
	if err != nil {
		t.Fatal(err)
	}
	if len(request.Tools) != 1 {
		t.Fatalf("the same name reached the target tool list twice: %+v", request.Tools)
	}
	if request.Tools[0].Kind != "function" {
		t.Fatalf("the dropped declaration changed the surviving kind: %q", request.Tools[0].Kind)
	}
	dropped := false
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "duplicate_tool_dropped" {
			dropped = true
		}
	}
	if !dropped {
		t.Fatalf("the duplicate was dropped without a diagnostic: %+v", diagnostics)
	}
}

// A namespace description travels as the paragraph every member description
// shares, because a flattened member has no field of its own for it. Converting
// the list back must split that paragraph out again rather than fold it in a
// second time.
func TestConvertRequest_NamespaceDescriptionRoundTripIsStable(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"tools":[{"type":"namespace","name":"ns","description":"NAMESPACE","tools":[
			{"type":"function","name":"a","description":"A","parameters":{"type":"object"}},
			{"type":"function","name":"b","description":"B","parameters":{"type":"object"}}
		]}],
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)
	current := body
	for hop := 1; hop <= 3; hop++ {
		canonical, _, err := dialect.DecodeRequest(dialect.OpenAIResponses, current)
		if err != nil {
			t.Fatal(err)
		}
		encoded, _, err := dialect.EncodeRequest(canonical, dialect.OpenAIResponses, dialect.ConvertOptions{Model: "m"})
		if err != nil {
			t.Fatal(err)
		}
		var out struct {
			Tools []map[string]any `json:"tools"`
		}
		if err := json.Unmarshal(encoded, &out); err != nil {
			t.Fatal(err)
		}
		namespace, _ := out.Tools[0]["description"].(string)
		if namespace != "NAMESPACE" {
			t.Fatalf("hop %d namespace description = %q", hop, namespace)
		}
		var members []string
		for _, raw := range out.Tools[0]["tools"].([]any) {
			description, _ := raw.(map[string]any)["description"].(string)
			members = append(members, description)
		}
		if strings.Join(members, "|") != "A|B" {
			t.Fatalf("hop %d member descriptions = %q", hop, members)
		}
		current = encoded
	}
}
