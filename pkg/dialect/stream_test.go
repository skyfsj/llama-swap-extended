package dialect

import (
	"encoding/json"
	"strings"
	"testing"
)

// streamFixtures feeds one upstream frame at a time through a converter.
type streamFixture struct {
	name   string
	source Format
	frames []string
}

var textStreams = []streamFixture{
	{
		name:   "chat",
		source: OpenAIChat,
		frames: []string{
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"po"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"ng"},"finish_reason":"stop"}]}`,
		},
	},
	{
		name:   "claude",
		source: ClaudeMessages,
		frames: []string{
			`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":7,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ng"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		},
	},
	{
		name:   "gemini",
		source: Gemini,
		frames: []string{
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"po"}]},"index":0}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"ng"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":2,"totalTokenCount":9}}`,
		},
	},
	{
		name:   "responses",
		source: OpenAIResponses,
		frames: []string{
			`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1,"status":"in_progress"}}`,
			`{"type":"response.output_text.delta","delta":"po"}`,
			`{"type":"response.output_text.delta","delta":"ng"}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`,
		},
	},
}

func runStream(t *testing.T, source, target Format, frames []string, options StreamOptions) (string, []Diagnostic) {
	t.Helper()
	if options.Model == "" {
		options.Model = "m"
	}
	converter, err := NewStreamConverter(source, target, options)
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	var builder strings.Builder
	for _, payload := range frames {
		emitted, err := converter.Feed([]byte(payload))
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		for _, frame := range emitted {
			builder.WriteString(renderFrame(frame))
		}
	}
	tail, err := converter.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, frame := range tail {
		builder.WriteString(renderFrame(frame))
	}
	return builder.String(), converter.Diagnostics()
}

func renderFrame(frame Frame) string {
	if frame.Event != "" {
		return "event: " + frame.Event + "\ndata: " + string(frame.Data) + "\n\n"
	}
	return "data: " + string(frame.Data) + "\n\n"
}

func TestStreamMatrixPreservesText(t *testing.T) {
	for _, fixture := range textStreams {
		for _, target := range allFormats {
			if fixture.source == target {
				continue
			}
			t.Run(fixture.name+"_to_"+string(target), func(t *testing.T) {
				output, diagnostics := runStream(t, fixture.source, target, fixture.frames, StreamOptions{})
				if !strings.Contains(output, "po") || !strings.Contains(output, "ng") {
					t.Fatalf("deltas lost: %s (%v)", output, diagnostics)
				}
				if fixture.source == target {
					t.Fatalf("unreachable")
				}
				assertTerminalFrame(t, target, output)
			})
		}
	}
}

// assertTerminalFrame verifies the client's dialect ends the stream correctly.
func assertTerminalFrame(t *testing.T, target Format, output string) {
	t.Helper()
	switch target {
	case OpenAIChat:
		if !strings.Contains(output, "[DONE]") {
			t.Fatalf("chat stream has no [DONE]: %s", output)
		}
	case ClaudeMessages:
		if !strings.Contains(output, `"type":"message_stop"`) {
			t.Fatalf("claude stream has no message_stop: %s", output)
		}
	case OpenAIResponses:
		if !strings.Contains(output, `"type":"response.completed"`) && !strings.Contains(output, `"type":"response.incomplete"`) {
			t.Fatalf("responses stream has no terminal event: %s", output)
		}
	case Gemini:
		if !strings.Contains(output, `"finishReason"`) {
			t.Fatalf("gemini stream has no finishReason: %s", output)
		}
	}
}

func TestStreamChatToolCallDeltasAccumulate(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"lookup","arguments":""}}]}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]},"finish_reason":"tool_calls"}]}`,
	}
	// Claude client: tool_use block + input_json_delta fragments.
	claude, diagnostics := runStream(t, OpenAIChat, ClaudeMessages, frames, StreamOptions{})
	for _, want := range []string{`"type":"tool_use"`, `"id":"call_1"`, `"name":"lookup"`, `"partial_json"`, `"tool_use"`} {
		if !strings.Contains(claude, want) {
			t.Fatalf("claude stream lacks %s: %s (%v)", want, claude, diagnostics)
		}
	}
	if !strings.Contains(claude, `"stop_reason":"tool_use"`) {
		t.Fatalf("claude stop reason is not tool_use: %s", claude)
	}
	// Gemini client: one functionCall with fully assembled args.
	gemini, diagnostics := runStream(t, OpenAIChat, Gemini, frames, StreamOptions{})
	if !strings.Contains(gemini, `"functionCall"`) || !strings.Contains(gemini, `"q"`) {
		t.Fatalf("gemini stream lost the tool call: %s (%v)", gemini, diagnostics)
	}
	var call struct {
		Candidates []struct {
			Content struct {
				Parts []struct {
					FunctionCall struct {
						Name string         `json:"name"`
						Args map[string]any `json:"args"`
					} `json:"functionCall"`
				} `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	for _, line := range strings.Split(gemini, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if payload == "" {
			continue
		}
		var probe map[string]any
		if json.Unmarshal([]byte(payload), &probe) != nil {
			continue
		}
		if _, ok := probe["candidates"]; !ok {
			continue
		}
		if json.Unmarshal([]byte(payload), &call) == nil && len(call.Candidates) > 0 {
			parts := call.Candidates[0].Content.Parts
			if len(parts) > 0 && parts[0].FunctionCall.Name != "" {
				if parts[0].FunctionCall.Args["q"] != "x" {
					t.Fatalf("gemini tool args = %v", parts[0].FunctionCall.Args)
				}
			}
		}
	}
}

func TestStreamMidStreamErrorSurfaces(t *testing.T) {
	// Anthropic error events mid-stream must not look like a successful stop.
	frames := []string{
		`{"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[],"usage":{"input_tokens":7,"output_tokens":0}}}`,
		`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"po"}}`,
		`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`,
	}
	for _, target := range []Format{OpenAIChat, OpenAIResponses, Gemini} {
		t.Run(string(target), func(t *testing.T) {
			output, _ := runStream(t, ClaudeMessages, target, frames, StreamOptions{})
			if !strings.Contains(output, "overloaded") {
				t.Fatalf("error message was swallowed: %s", output)
			}
			switch target {
			case OpenAIChat:
				if !strings.Contains(output, `"error"`) {
					t.Fatalf("chat client got no error frame: %s", output)
				}
			case OpenAIResponses:
				if !strings.Contains(output, "response.failed") {
					t.Fatalf("responses client got no terminal error: %s", output)
				}
			case Gemini:
				if !strings.Contains(output, `"error"`) {
					t.Fatalf("gemini client got no error frame: %s", output)
				}
			}
		})
	}
}

func TestStreamUsageReachesClient(t *testing.T) {
	// Chat upstream with include_usage: usage arrives after the finish chunk.
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":9,"completion_tokens":3,"total_tokens":12}}`,
	}
	claude, _ := runStream(t, OpenAIChat, ClaudeMessages, frames, StreamOptions{})
	if !strings.Contains(claude, `"output_tokens":3`) {
		t.Fatalf("claude usage lost: %s", claude)
	}
	responses, _ := runStream(t, OpenAIChat, OpenAIResponses, frames, StreamOptions{})
	if !strings.Contains(responses, `"input_tokens":9`) || !strings.Contains(responses, `"output_tokens":3`) {
		t.Fatalf("responses usage lost: %s", responses)
	}
	gemini, _ := runStream(t, OpenAIChat, Gemini, frames, StreamOptions{})
	if !strings.Contains(gemini, `"promptTokenCount":9`) {
		t.Fatalf("gemini usage lost: %s", gemini)
	}
	chat, _ := runStream(t, Gemini, OpenAIChat, textStreams[2].frames, StreamOptions{IncludeUsage: true})
	if !strings.Contains(chat, `"usage"`) || !strings.Contains(chat, `"prompt_tokens"`) {
		t.Fatalf("chat usage frame lost: %s", chat)
	}
}

func TestStreamMessageStartCarriesEstimatedInput(t *testing.T) {
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	}
	output, _ := runStream(t, OpenAIChat, ClaudeMessages, frames, StreamOptions{EstimateInputTokens: 42})
	if !strings.Contains(output, `"input_tokens":42`) {
		t.Fatalf("message_start input estimate missing: %s", output)
	}
}

func TestStreamResponseIDsAreClientDialect(t *testing.T) {
	chat, _ := runStream(t, Gemini, OpenAIChat, textStreams[2].frames, StreamOptions{Model: "m"})
	var chunk map[string]any
	for _, line := range strings.Split(chat, "\n") {
		payload := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if json.Unmarshal([]byte(payload), &chunk) == nil && chunk["object"] == "chat.completion.chunk" {
			if chunk["id"] == nil || chunk["id"] == "" {
				t.Fatalf("chat chunk has no id: %s", payload)
			}
			break
		}
	}
	claude, _ := runStream(t, OpenAIResponses, ClaudeMessages, textStreams[3].frames, StreamOptions{})
	if !strings.Contains(claude, `"type":"message_start"`) || !strings.Contains(claude, `"id":"resp_1"`) {
		t.Fatalf("claude stream id lost: %s", claude)
	}
}

func TestStreamSameDialectRejected(t *testing.T) {
	if _, err := NewStreamConverter(OpenAIChat, OpenAIChat, StreamOptions{}); err == nil {
		t.Fatal("same-dialect stream conversion must be rejected")
	}
}
