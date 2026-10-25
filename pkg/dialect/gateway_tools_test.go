package dialect

import (
	"bytes"
	"testing"
)

func TestStream_GatewayToolExecutionInteractions(t *testing.T) {
	item := `{"type":"mcp_call","server_label":"llama-swap","status":"completed","id":"executed","name":"search","arguments":"{\"q\":\"x\"}","output":"MCP_RESULT"}`
	body := []byte(`{"id":"response","model":"m","status":"completed","output":[` + item + `,{"type":"message","role":"assistant","content":[{"type":"output_text","text":"DONE"}]}]}`)
	check := func(t *testing.T, response *Response) {
		t.Helper()
		call, result := false, false
		for _, part := range response.Parts {
			if part.Kind == PartToolCall && part.ToolCallID == "executed" && bytes.Contains(part.Arguments, []byte(`"x"`)) {
				call = true
			}
			if part.Kind == PartToolResult && part.ToolCallID == "executed" && len(part.Content) > 0 && part.Content[0].Text == "MCP_RESULT" {
				result = true
			}
		}
		if !call || !result {
			t.Fatalf("executed call/result lost: %+v", response.Parts)
		}
	}
	converted, diagnostics, err := ConvertResponse(OpenAIResponses, GeminiInteractions, body)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range diagnostics {
		if d.Code == "output_item_dropped" {
			t.Fatalf("gateway artifact dropped: %v", diagnostics)
		}
	}
	parsed, _, err := DecodeResponse(GeminiInteractions, converted)
	if err != nil {
		t.Fatal(err)
	}
	check(t, parsed)
	for _, whole := range []bool{false, true} {
		var frames []Frame
		if whole {
			frames, _, err = ConvertResponseToStream(OpenAIResponses, GeminiInteractions, body, StreamOptions{})
		} else {
			converter, e := NewStreamConverter(OpenAIResponses, GeminiInteractions, StreamOptions{})
			if e != nil {
				t.Fatal(e)
			}
			for _, event := range []string{`{"type":"response.created","response":{"id":"r","model":"m"}}`, `{"type":"response.output_item.added","output_index":0,"item":` + item + `}`, `{"type":"response.output_item.done","output_index":0,"item":` + item + `}`, `{"type":"response.output_text.delta","output_index":1,"delta":"DONE"}`, `{"type":"response.completed","response":{"id":"r","model":"m","status":"completed"}}`} {
				next, e := converter.Feed([]byte(event))
				if e != nil {
					t.Fatal(e)
				}
				frames = append(frames, next...)
			}
			tail, e := converter.Finish()
			if e != nil {
				t.Fatal(e)
			}
			frames = append(frames, tail...)
		}
		if err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		for _, frame := range frames {
			wire.WriteString("data: ")
			wire.Write(frame.Data)
			wire.WriteString("\n\n")
		}
		parsed, _, e := DecodeStream(GeminiInteractions, wire.Bytes())
		if e != nil {
			t.Fatal(e)
		}
		check(t, parsed)
	}
}
