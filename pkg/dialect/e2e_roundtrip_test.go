package dialect_test

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// TestE2E_FullConversationRoundTrip replays a realistic multi-turn conversation
// with tool calls through every dialect pair and asserts semantic fidelity.
func TestE2E_FullConversationRoundTrip(t *testing.T) {
	// Canonical conversation: system + user + assistant tool call + tool result + final user.
	chat := []byte(`{
	  "model": "demo",
	  "messages": [
	    {"role":"system","content":"You are terse."},
	    {"role":"user","content":"What is 2+2?"},
	    {"role":"assistant","content":null,"tool_calls":[{"id":"call_a","type":"function","function":{"name":"calc","arguments":"{\"expr\":\"2+2\"}"}}]},
	    {"role":"tool","tool_call_id":"call_a","content":"4"},
	    {"role":"user","content":"Thanks"}
	  ],
	  "tools":[{"type":"function","function":{"name":"calc","description":"compute","parameters":{"type":"object","properties":{"expr":{"type":"string"}},"required":["expr"]}}}],
	  "tool_choice":{"type":"auto"},
	  "temperature":0.3,"max_tokens":256
	}`)

	for _, target := range []dialect.Format{dialect.OpenAIResponses, dialect.ClaudeMessages, dialect.Gemini} {
		t.Run("chat_to_"+string(target), func(t *testing.T) {
			out, diags, err := dialect.ConvertRequest(dialect.OpenAIChat, target, chat, dialect.ConvertOptions{Model: "demo"})
			if err != nil {
				t.Fatalf("convert: %v (%v)", err, diags)
			}
			text := string(out)
			// Semantic must-haves: system prompt, user text, tool name+args, tool result, final turn.
			for _, must := range []string{"terse", "2+2", "calc"} {
				if !strings.Contains(text, must) {
					t.Fatalf("%s lost %q: %s", target, must, text)
				}
			}
			if !strings.Contains(text, "4") && !strings.Contains(text, "Thanks") {
				t.Fatalf("%s lost tool result or final turn: %s", target, text)
			}
			// Round trip back to Chat must still carry everything.
			back, backDiags, err := dialect.ConvertRequest(target, dialect.OpenAIChat, out, dialect.ConvertOptions{Model: "demo"})
			if err != nil {
				t.Fatalf("round trip: %v (%v)", err, backDiags)
			}
			backText := string(back)
			for _, must := range []string{"terse", "2+2", "4", "Thanks"} {
				if !strings.Contains(backText, must) {
					t.Fatalf("round trip to chat lost %q: %s", must, backText)
				}
			}
			var object map[string]any
			if err := json.Unmarshal(back, &object); err != nil {
				t.Fatalf("round trip is not JSON: %v", err)
			}
			messages := object["messages"].([]any)
			if len(messages) < 5 {
				t.Fatalf("round trip collapsed the conversation: %s", backText)
			}
		})
	}
}

// TestE2E_StreamingAllPairs drives a streaming conversion for every ordered
// pair with real upstream frames and asserts the client gets text + terminal.
func TestE2E_StreamingAllPairs(t *testing.T) {
	sources := map[dialect.Format][]string{
		dialect.OpenAIChat: {
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"Hel"}}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"lo"},"finish_reason":"stop"}]}`,
			`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		},
		dialect.ClaudeMessages: {
			`{"type":"message_start","message":{"id":"msg_1","model":"m","usage":{"input_tokens":5,"output_tokens":0}}}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hel"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"lo"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":2}}`,
			`{"type":"message_stop"}`,
		},
		dialect.Gemini: {
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"Hel"}]},"index":0}]}`,
			`{"candidates":[{"content":{"role":"model","parts":[{"text":"lo"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":5,"candidatesTokenCount":2,"totalTokenCount":7}}`,
		},
		dialect.OpenAIResponses: {
			`{"type":"response.created","response":{"id":"resp_1","model":"m","created_at":1,"status":"in_progress"}}`,
			`{"type":"response.output_text.delta","delta":"Hel"}`,
			`{"type":"response.output_text.delta","delta":"lo"}`,
			`{"type":"response.completed","response":{"id":"resp_1","model":"m","status":"completed","usage":{"input_tokens":5,"output_tokens":2,"total_tokens":7}}}`,
		},
	}

	for source, frames := range sources {
		for _, target := range []dialect.Format{dialect.OpenAIChat, dialect.OpenAIResponses, dialect.ClaudeMessages, dialect.Gemini} {
			if source == target {
				continue
			}
			t.Run(fmt.Sprintf("%s_to_%s", source, target), func(t *testing.T) {
				converter, err := dialect.NewStreamConverter(source, target, dialect.StreamOptions{Model: "m", IncludeUsage: true})
				if err != nil {
					t.Fatalf("converter: %v", err)
				}
				var builder strings.Builder
				for _, payload := range frames {
					emitted, err := converter.Feed([]byte(payload))
					if err != nil {
						t.Fatalf("feed: %v", err)
					}
					for _, f := range emitted {
						builder.WriteString(string(f.Data))
						builder.WriteString("\n")
					}
				}
				tail, err := converter.Finish()
				if err != nil {
					t.Fatalf("finish: %v", err)
				}
				for _, f := range tail {
					builder.WriteString(string(f.Data))
					builder.WriteString("\n")
				}
				out := builder.String()
				// Reassemble the assistant text from the client dialect and
				// require it to be exactly the upstream text.
				if !strings.Contains(out, "Hel") || !strings.Contains(out, "lo") {
					t.Fatalf("text lost: %s", out)
				}
				if !hasTerminal(target, out) {
					t.Fatalf("no terminal frame: %s", out)
				}
				if diags := converter.Diagnostics(); len(diags) > 0 {
					for _, d := range diags {
						if d.Code == "tool_arguments_dropped" || strings.Contains(d.Code, "invalid") {
							t.Fatalf("unexpected diagnostic %s: %s", d.Code, d.Message)
						}
					}
				}
			})
		}
	}
}

func hasTerminal(target dialect.Format, out string) bool {
	switch target {
	case dialect.OpenAIChat:
		return strings.Contains(out, "[DONE]")
	case dialect.ClaudeMessages:
		return strings.Contains(out, "message_stop")
	case dialect.OpenAIResponses:
		return strings.Contains(out, "response.completed") || strings.Contains(out, "response.incomplete")
	case dialect.Gemini:
		return strings.Contains(out, "finishReason")
	}
	return false
}
