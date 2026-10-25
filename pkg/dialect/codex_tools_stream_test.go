package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// TestStreamResponsesToolLifecycle pins the standard Responses tool-call
// lifecycle. A client that waits for function_call_arguments.delta and for an
// output_item.added that is still in_progress never sees a streamed call when
// everything is deferred to the terminal frame.
func TestStreamResponsesToolLifecycle(t *testing.T) {
	request := codexToolRequest(t)
	converter, err := dialect.NewStreamConverter(dialect.OpenAIChat, dialect.OpenAIResponses, dialect.StreamOptions{
		Model: "m", ID: "resp_1", Request: request,
	})
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	frames := []string{
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"mcp__cua_repl__js","arguments":"{\"code\":"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"1+1\"}"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var output strings.Builder
	for _, payload := range frames {
		emitted, err := converter.Feed([]byte(payload))
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		for _, frame := range emitted {
			output.WriteString("event: " + frame.Event + "\ndata: " + string(frame.Data) + "\n\n")
		}
	}
	tail, err := converter.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, frame := range tail {
		output.WriteString("event: " + frame.Event + "\ndata: " + string(frame.Data) + "\n\n")
	}
	stream := output.String()

	addedIndex := strings.Index(stream, "response.output_item.added")
	deltaIndex := strings.Index(stream, "response.function_call_arguments.delta")
	doneIndex := strings.Index(stream, "response.function_call_arguments.done")
	itemDoneIndex := strings.Index(stream, "response.output_item.done")
	if addedIndex < 0 || deltaIndex < 0 || doneIndex < 0 || itemDoneIndex < 0 {
		t.Fatalf("missing tool lifecycle events in stream:\n%s", stream)
	}
	if !(addedIndex < deltaIndex && deltaIndex < doneIndex && doneIndex < itemDoneIndex) {
		t.Fatalf("tool events out of order (added=%d delta=%d done=%d itemDone=%d):\n%s",
			addedIndex, deltaIndex, doneIndex, itemDoneIndex, stream)
	}

	var addedItem, doneItem map[string]any
	for _, line := range strings.Split(stream, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		switch event["type"] {
		case "response.output_item.added":
			addedItem, _ = event["item"].(map[string]any)
		case "response.output_item.done":
			doneItem, _ = event["item"].(map[string]any)
		}
	}
	if addedItem == nil || doneItem == nil {
		t.Fatalf("tool item events missing:\n%s", stream)
	}
	if addedItem["status"] != "in_progress" {
		t.Fatalf("output_item.added status = %v, want in_progress", addedItem["status"])
	}
	if addedItem["namespace"] != "mcp__cua_repl" || addedItem["name"] != "js" {
		t.Fatalf("streamed call advertised as %v/%v, want namespace mcp__cua_repl name js", addedItem["namespace"], addedItem["name"])
	}
	if doneItem["namespace"] != "mcp__cua_repl" || doneItem["name"] != "js" {
		t.Fatalf("completed call reported as %v/%v", doneItem["namespace"], doneItem["name"])
	}
	if doneItem["arguments"] != `{"code":"1+1"}` {
		t.Fatalf("completed arguments = %v", doneItem["arguments"])
	}
}

// TestStreamResponsesCustomToolLifecycle verifies a custom tool streams its
// freeform input through the custom_tool_call_input events rather than
// pretending to be a JSON function call.
func TestStreamResponsesCustomToolLifecycle(t *testing.T) {
	request := codexToolRequest(t)
	converter, err := dialect.NewStreamConverter(dialect.OpenAIChat, dialect.OpenAIResponses, dialect.StreamOptions{
		Model: "m", ID: "resp_1", Request: request,
	})
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	frames := []string{
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_c","function":{"name":"apply_patch","arguments":"{\"input\":\"*** Begin Patch\"}"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var output strings.Builder
	for _, payload := range frames {
		emitted, err := converter.Feed([]byte(payload))
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		for _, frame := range emitted {
			output.WriteString(string(frame.Data))
			output.WriteString("\n")
		}
	}
	tail, err := converter.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, frame := range tail {
		output.WriteString(string(frame.Data))
		output.WriteString("\n")
	}
	stream := output.String()
	if !strings.Contains(stream, "response.custom_tool_call_input.delta") {
		t.Fatalf("custom tool input was not streamed:\n%s", stream)
	}
	if strings.Contains(stream, "response.function_call_arguments.delta") {
		t.Fatalf("a custom tool must not stream JSON function arguments:\n%s", stream)
	}
	if !strings.Contains(stream, `"type":"custom_tool_call"`) {
		t.Fatalf("custom tool item not restored as custom_tool_call:\n%s", stream)
	}
}
