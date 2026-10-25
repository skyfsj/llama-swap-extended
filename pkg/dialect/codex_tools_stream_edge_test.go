package dialect_test

import (
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// TestStreamResponsesToolNamedAfterArguments covers an upstream that streams a
// tool call's arguments before its name. The call must still be announced once
// the name arrives; dropping it made the client wait for a call that never
// came.
func TestStreamResponsesToolNamedAfterArguments(t *testing.T) {
	request := codexToolRequest(t)
	converter, err := dialect.NewStreamConverter(dialect.OpenAIChat, dialect.OpenAIResponses, dialect.StreamOptions{
		Model: "m", ID: "resp_1", Request: request,
	})
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	frames := []string{
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"code\":"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_a","function":{"name":"mcp__node_repl__js","arguments":"\"1+1\"}"}}]}}]}`,
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	var stream strings.Builder
	for _, payload := range frames {
		emitted, err := converter.Feed([]byte(payload))
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		for _, frame := range emitted {
			stream.Write(frame.Data)
			stream.WriteString("\n")
		}
	}
	tail, err := converter.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, frame := range tail {
		stream.Write(frame.Data)
		stream.WriteString("\n")
	}
	output := stream.String()
	if !strings.Contains(output, "response.output_item.added") {
		t.Fatalf("a call whose name arrived late was never announced:\n%s", output)
	}
	if !strings.Contains(output, `"namespace":"mcp__node_repl"`) {
		t.Fatalf("late-named call lost its namespace:\n%s", output)
	}
	if !strings.Contains(output, `{\"code\":\"1+1\"}`) {
		t.Fatalf("buffered arguments were not released with the announcement:\n%s", output)
	}
}

// TestStreamResponsesSourceCustomTool checks that a custom_tool_call arriving
// from a Responses upstream is understood, so converting Responses to another
// dialect does not silently drop the call.
func TestStreamResponsesSourceCustomTool(t *testing.T) {
	converter, err := dialect.NewStreamConverter(dialect.OpenAIResponses, dialect.OpenAIChat, dialect.StreamOptions{Model: "m"})
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	frames := []string{
		`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1}}`,
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","namespace":"codex"}}`,
		`{"type":"response.custom_tool_call_input.delta","output_index":0,"item_id":"ctc_1","call_id":"call_1","delta":"*** Begin Patch"}`,
		`{"type":"response.custom_tool_call_input.done","output_index":0,"item_id":"ctc_1","call_id":"call_1","input":"*** Begin Patch"}`,
		`{"type":"response.output_item.done","output_index":0,"item":{"type":"custom_tool_call","id":"ctc_1","call_id":"call_1","name":"apply_patch","input":"*** Begin Patch"}}`,
		`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed"}}`,
	}
	var stream strings.Builder
	for _, payload := range frames {
		emitted, err := converter.Feed([]byte(payload))
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		for _, frame := range emitted {
			stream.Write(frame.Data)
			stream.WriteString("\n")
		}
	}
	tail, err := converter.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, frame := range tail {
		stream.Write(frame.Data)
		stream.WriteString("\n")
	}
	output := stream.String()
	if !strings.Contains(output, "apply_patch") {
		t.Fatalf("custom tool call disappeared when converting away from Responses:\n%s", output)
	}
	// The freeform input is wrapped as one JSON argument, so the fragment is
	// quoted inside the chunk's own JSON string.
	if !strings.Contains(output, `\"input\":\"*** Begin Patch\"`) {
		t.Fatalf("custom tool input not carried into the Chat arguments:\n%s", output)
	}
}
