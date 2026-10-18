package relaykit

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestRouteForRequest(t *testing.T) {
	tests := []struct {
		path   string
		format Format
		model  string
		stream bool
	}{
		{"/v1/chat/completions", OpenAIChat, "", false},
		{"/v1/responses", OpenAIResponses, "", false},
		{"/v1/messages", ClaudeMessages, "", false},
		{"/v1beta/models/demo:generateContent", Gemini, "demo", false},
		{"/v1beta/models/demo:streamGenerateContent", Gemini, "demo", true},
	}
	for _, test := range tests {
		route, ok := RouteForRequest("POST", test.path)
		if !ok || route.Format != test.format || route.Model != test.model || route.Stream != test.stream {
			t.Errorf("RouteForRequest(%q) = %+v, %t", test.path, route, ok)
		}
	}
	if _, ok := RouteForRequest("POST", "/v1/messages/count_tokens"); ok {
		t.Fatal("count_tokens was classified as a text generation request")
	}
}

func TestHostedSearchIsLeftToExtensions(t *testing.T) {
	body := []byte(`{"model":"m","input":"search this","tools":[{"type":"web_search"}]}`)
	result, err := ConvertRequest(context.Background(), NewMeta("m", false), OpenAIResponses, OpenAIChat, body, "m")
	if err != nil {
		t.Fatalf("ConvertRequest: %v", err)
	}
	encoded, err := Marshal(result.Value)
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(encoded, &request); err != nil {
		t.Fatal(err)
	}
	if _, exists := request["tools"]; exists {
		t.Fatalf("provider-native search tool reached Chat upstream: %v", request["tools"])
	}
	if len(result.Diagnostics) == 0 {
		t.Fatalf("hosted search omission must be reported: %+v", result.Diagnostics)
	}
}

func TestStripHostedSearchTools(t *testing.T) {
	tests := []struct {
		format Format
		body   string
		want   string
	}{
		{OpenAIChat, `{"web_search_options":{},"tools":[{"type":"web_search"},{"type":"function","function":{"name":"lookup"}}]}`, `"name":"lookup"`},
		{OpenAIResponses, `{"tools":[{"type":"web_search"},{"type":"function","name":"lookup"}]}`, `"name":"lookup"`},
		{ClaudeMessages, `{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"lookup"}]}`, `"name":"lookup"`},
		{Gemini, `{"tools":[{"googleSearch":{}},{"functionDeclarations":[{"name":"lookup"}]}]}`, `"name":"lookup"`},
	}
	for _, test := range tests {
		t.Run(string(test.format), func(t *testing.T) {
			got, changed, err := StripHostedSearchTools([]byte(test.body), test.format)
			if err != nil || !changed || !json.Valid(got) || !strings.Contains(string(got), test.want) {
				t.Fatalf("StripHostedSearchTools = %s, %t, %v", got, changed, err)
			}
			if strings.Contains(string(got), "googleSearch") || strings.Contains(string(got), "web_search") {
				t.Fatalf("hosted search still present: %s", got)
			}
		})
	}
}

func TestConvertRequestMatrix(t *testing.T) {
	requests := map[Format][]byte{
		OpenAIChat:      []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		OpenAIResponses: []byte(`{"model":"m","input":"hi","max_output_tokens":128}`),
		ClaudeMessages:  []byte(`{"model":"m","max_tokens":128,"messages":[{"role":"user","content":"hi"}]}`),
		Gemini:          []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`),
	}
	for source, body := range requests {
		for target := range requests {
			if source == target {
				continue
			}
			t.Run(string(source)+"_to_"+string(target), func(t *testing.T) {
				result, err := ConvertRequest(context.Background(), NewMeta("m", false), source, target, body, "m")
				if err != nil {
					t.Fatalf("ConvertRequest: %v", err)
				}
				if result.To != target {
					t.Fatalf("target = %q, want %q", result.To, target)
				}
				encoded, err := Marshal(result.Value)
				if err != nil || !json.Valid(encoded) {
					t.Fatalf("converted request is invalid JSON: %s (%v)", encoded, err)
				}
				// The user's prompt must survive every conversion: a silently
				// empty request is the failure mode this matrix guards against.
				if !strings.Contains(string(encoded), "hi") {
					t.Fatalf("prompt text lost converting %s -> %s: %s", source, target, encoded)
				}
				if target == Gemini {
					// The Gemini dialect carries the model in the URL path, not
					// in the body: assert the path builder keeps it.
					path, perr := TargetPath(Gemini, "m", false)
					if perr != nil || !strings.Contains(path, "/models/m:") {
						t.Fatalf("model lost converting %s -> gemini: path = %q (%v)", source, path, perr)
					}
				} else if !strings.Contains(string(encoded), `"m"`) {
					t.Fatalf("model lost converting %s -> %s: %s", source, target, encoded)
				}
			})
		}
	}
}

func TestConvertResponseMatrix(t *testing.T) {
	responses := map[Format][]byte{
		OpenAIChat:      []byte(`{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`),
		OpenAIResponses: []byte(`{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"item_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hi","annotations":[]}]}]}`),
		ClaudeMessages:  []byte(`{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"hi"}],"stop_reason":"end_turn"}`),
		Gemini:          []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`),
	}
	for source, body := range responses {
		for target := range responses {
			if source == target {
				continue
			}
			t.Run(string(source)+"_to_"+string(target), func(t *testing.T) {
				result, err := ConvertResponse(context.Background(), NewMeta("m", false), source, target, body)
				if err != nil {
					t.Fatalf("ConvertResponse: %v", err)
				}
				if result.To != target {
					t.Fatalf("target = %q, want %q", result.To, target)
				}
				encoded, err := Marshal(result.Value)
				if err != nil || !json.Valid(encoded) {
					t.Fatalf("converted response is invalid JSON: %s (%v)", encoded, err)
				}
				if !strings.Contains(string(encoded), "hi") {
					t.Fatalf("assistant text lost converting %s -> %s: %s", source, target, encoded)
				}
			})
		}
	}
}

func TestStreamResponseMatrix(t *testing.T) {
	chunks := map[Format][]byte{
		OpenAIChat:      []byte(`{"id":"chatcmpl_1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"content":"hi"}}]}`),
		OpenAIResponses: []byte(`{"type":"response.output_text.delta","delta":"hi"}`),
		ClaudeMessages:  []byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`),
		Gemini:          []byte(`{"candidates":[{"content":{"role":"model","parts":[{"text":"hi"}]},"index":0}]}`),
	}
	for source, body := range chunks {
		for target := range chunks {
			if source == target {
				continue
			}
			t.Run(string(source)+"_to_"+string(target), func(t *testing.T) {
				state, err := NewResponseStreamState(source, target, StreamOptions{Model: "m", IncludeUsage: true})
				if err != nil {
					t.Fatalf("NewResponseStreamState: %v", err)
				}
				response, err := DecodeStreamResponse(source, body)
				if err != nil {
					t.Fatalf("DecodeStreamResponse: %v", err)
				}
				results, err := ConvertStreamResponseChunk(context.Background(), NewMeta("m", true), state, response)
				if err != nil {
					t.Fatalf("ConvertStreamResponseChunk: %v", err)
				}
				tail, err := FinalizeStreamResponse(context.Background(), NewMeta("m", true), state)
				if err != nil {
					t.Fatalf("FinalizeStreamResponse: %v", err)
				}
				results = append(results, tail...)
				encodedAll := ""
				for _, result := range results {
					encoded, err := EncodeSSE(target, result.Value)
					if err != nil {
						t.Fatalf("EncodeSSE: %v", err)
					}
					encodedAll += string(encoded)
				}
				// The delta text must reach the client in every dialect pair.
				if !strings.Contains(encodedAll, "hi") {
					t.Fatalf("stream delta lost converting %s -> %s: %s", source, target, encodedAll)
				}
			})
		}
	}
}
