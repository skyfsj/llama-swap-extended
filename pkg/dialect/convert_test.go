package dialect

import (
	"encoding/json"
	"strings"
	"testing"
)

var allFormats = []Format{OpenAIChat, OpenAIResponses, ClaudeMessages, Gemini}

func decodeMap(t *testing.T, body []byte) map[string]any {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatalf("invalid JSON %s: %v", body, err)
	}
	return object
}

func TestRouteForRequest(t *testing.T) {
	cases := []struct {
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
	for _, test := range cases {
		route, ok := RouteForRequest("POST", test.path)
		if !ok || route.Format != test.format || route.Model != test.model || route.Stream != test.stream {
			t.Errorf("RouteForRequest(%q) = %+v, %t", test.path, route, ok)
		}
	}
	if _, ok := RouteForRequest("POST", "/v1/messages/count_tokens"); ok {
		t.Fatal("count_tokens must not be a generation route")
	}
	if _, ok := RouteForRequest("GET", "/v1/chat/completions"); ok {
		t.Fatal("GET must not be a generation route")
	}
}

func TestConvertRequestMatrixPreservesPrompt(t *testing.T) {
	requests := map[Format]string{
		OpenAIChat:      `{"model":"m","messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}]}`,
		OpenAIResponses: `{"model":"m","instructions":"be brief","input":"hi","max_output_tokens":64}`,
		ClaudeMessages:  `{"model":"m","max_tokens":64,"system":"be brief","messages":[{"role":"user","content":"hi"}]}`,
		Gemini:          `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"systemInstruction":{"parts":[{"text":"be brief"}]}}`,
	}
	for source, body := range requests {
		for _, target := range allFormats {
			if source == target {
				continue
			}
			t.Run(string(source)+"_to_"+string(target), func(t *testing.T) {
				converted, diagnostics, err := ConvertRequest(source, target, []byte(body), ConvertOptions{Model: "m"})
				if err != nil {
					t.Fatalf("ConvertRequest: %v (%v)", err, diagnostics)
				}
				text := string(converted)
				if !strings.Contains(text, "hi") {
					t.Fatalf("prompt lost: %s", text)
				}
				if target != Gemini && !strings.Contains(text, `"m"`) {
					t.Fatalf("model lost: %s", text)
				}
				var object map[string]any
				if err := json.Unmarshal(converted, &object); err != nil {
					t.Fatalf("invalid JSON: %s", converted)
				}
			})
		}
	}
}

func TestClaudeToolChoiceForwardedToChat(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"tool","name":"lookup","disable_parallel_tool_use":true}}`)
	converted, _, err := ConvertRequest(ClaudeMessages, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, converted)
	choice, ok := object["tool_choice"].(map[string]any)
	if !ok {
		t.Fatalf("tool_choice dropped: %s", converted)
	}
	function, _ := choice["function"].(map[string]any)
	if function["name"] != "lookup" {
		t.Fatalf("tool_choice name = %v", choice)
	}
}

func TestClaudeToolChoiceAnyBecomesRequired(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"tools":[{"name":"lookup","input_schema":{"type":"object"}}],"tool_choice":{"type":"any"}}`)
	converted, _, err := ConvertRequest(ClaudeMessages, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, converted)
	if object["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %v, want required (%s)", object["tool_choice"], converted)
	}
}

func TestChatToolChoiceForwardedToClaudeAndGemini(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}],"tool_choice":{"type":"function","function":{"name":"lookup"}}}`)
	claude, _, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	choice := decodeMap(t, claude)["tool_choice"].(map[string]any)
	if choice["type"] != "tool" || choice["name"] != "lookup" {
		t.Fatalf("claude tool_choice = %v", choice)
	}
	gemini, _, err := ConvertRequest(OpenAIChat, Gemini, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	config := decodeMap(t, gemini)["toolConfig"].(map[string]any)
	calling := config["functionCallingConfig"].(map[string]any)
	if calling["mode"] != "ANY" {
		t.Fatalf("gemini toolConfig = %v", config)
	}
	names, _ := calling["allowedFunctionNames"].([]any)
	if len(names) != 1 || names[0] != "lookup" {
		t.Fatalf("allowedFunctionNames = %v", calling)
	}
}

func TestGeminiResponseFormatForwardedToChat(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"responseMimeType":"application/json","responseSchema":{"type":"object","properties":{"name":{"type":"string"}}}}}`)
	converted, diagnostics, err := ConvertRequest(Gemini, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, converted)
	format, ok := object["response_format"].(map[string]any)
	if !ok {
		t.Fatalf("response_format dropped: %s (%v)", converted, diagnostics)
	}
	if format["type"] != "json_schema" {
		t.Fatalf("response_format = %v", format)
	}
	schema := format["json_schema"].(map[string]any)
	if schema["schema"] == nil {
		t.Fatalf("schema lost: %v", schema)
	}
}

func TestGeminiToolConfigForwardedToChat(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"tools":[{"functionDeclarations":[{"name":"lookup","parameters":{"type":"object"}}]}],"toolConfig":{"functionCallingConfig":{"mode":"ANY","allowedFunctionNames":["lookup"]}}}`)
	converted, _, err := ConvertRequest(Gemini, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	choice := decodeMap(t, converted)["tool_choice"].(map[string]any)
	function, _ := choice["function"].(map[string]any)
	if function["name"] != "lookup" {
		t.Fatalf("tool_choice = %v (%s)", choice, converted)
	}
}

func TestChatResponseFormatForwardedToClaude(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"response_format":{"type":"json_schema","json_schema":{"name":"person","schema":{"type":"object","properties":{"name":{"type":"string"}}}}}}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, converted)
	output, ok := object["output_config"].(map[string]any)
	if !ok {
		t.Fatalf("output_config dropped: %s (%v)", converted, diagnostics)
	}
	format := output["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil {
		t.Fatalf("output_config.format = %v", format)
	}
}

func TestResponsesReasoningItemsDoNotBecomeEmptyMessages(t *testing.T) {
	body := []byte(`{"model":"m","input":[{"type":"reasoning","summary":[{"type":"summary_text","text":"thought"}]},{"type":"message","role":"user","content":"hi"}],"max_output_tokens":64}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, converted)
	messages := object["messages"].([]any)
	var userMessages int
	for _, raw := range messages {
		message := raw.(map[string]any)
		if message["role"] == "user" {
			userMessages++
			if message["content"] == "" || message["content"] == nil {
				t.Fatalf("empty user message was synthesized: %v (%v)", messages, diagnostics)
			}
		}
	}
	if userMessages != 1 {
		t.Fatalf("user messages = %d: %v", userMessages, messages)
	}
}

func TestChatStopNonStringDoesNotPanic(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stop":[1,"END"]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("conversion failed instead of reporting a diagnostic: %v", err)
	}
	var found bool
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "invalid_stop_entry" {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing diagnostic: %v", diagnostics)
	}
	object := decodeMap(t, converted)
	sequences := object["stop_sequences"].([]any)
	if len(sequences) != 1 || sequences[0] != "END" {
		t.Fatalf("stop_sequences = %v", sequences)
	}
}

func TestChatNToSingleChoiceDialectsIsAnError(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"n":2}`)
	for _, target := range []Format{ClaudeMessages, OpenAIResponses} {
		if _, _, err := ConvertRequest(OpenAIChat, target, body, ConvertOptions{Model: "m"}); err == nil {
			t.Errorf("n=2 to %s must be rejected", target)
		}
	}
}

func TestChatStreamRequestsUsageFromUpstream(t *testing.T) {
	body := []byte(`{"model":"m","messages":[{"role":"user","content":"hi"}],"stream":true}`)
	for _, source := range []Format{ClaudeMessages, Gemini, OpenAIResponses} {
		sourceBody := body
		if source == ClaudeMessages {
			sourceBody = []byte(`{"model":"m","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`)
		}
		if source == Gemini {
			sourceBody = []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
		}
		if source == OpenAIResponses {
			sourceBody = []byte(`{"model":"m","input":"hi","stream":true}`)
		}
		converted, _, err := ConvertRequest(source, OpenAIChat, sourceBody, ConvertOptions{Model: "m", Stream: boolPtrTrue()})
		if err != nil {
			t.Fatalf("%s: %v", source, err)
		}
		options, ok := decodeMap(t, converted)["stream_options"].(map[string]any)
		if !ok || options["include_usage"] != true {
			t.Fatalf("%s: stream_options = %v (%s)", source, options, converted)
		}
	}
}

func boolPtrTrue() *bool {
	value := true
	return &value
}

func TestConvertResponseMatrixPreservesText(t *testing.T) {
	responses := map[Format]string{
		OpenAIChat:      `{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"pong"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`,
		OpenAIResponses: `{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"m","output":[{"id":"item_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"pong","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`,
		ClaudeMessages:  `{"id":"msg_1","type":"message","role":"assistant","model":"m","content":[{"type":"text","text":"pong"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`,
		Gemini:          `{"candidates":[{"content":{"role":"model","parts":[{"text":"pong"}]},"finishReason":"STOP","index":0}],"usageMetadata":{"promptTokenCount":1,"candidatesTokenCount":1,"totalTokenCount":2}}`,
	}
	for source, body := range responses {
		for _, target := range allFormats {
			if source == target {
				continue
			}
			t.Run(string(source)+"_to_"+string(target), func(t *testing.T) {
				converted, diagnostics, err := ConvertResponse(source, target, []byte(body))
				if err != nil {
					t.Fatalf("ConvertResponse: %v (%v)", err, diagnostics)
				}
				if !strings.Contains(string(converted), "pong") {
					t.Fatalf("assistant text lost: %s", converted)
				}
				var object map[string]any
				if err := json.Unmarshal(converted, &object); err != nil {
					t.Fatalf("invalid JSON: %s", converted)
				}
			})
		}
	}
}

func TestConvertResponseToolCallsSurvive(t *testing.T) {
	chat := []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]},"finish_reason":"tool_calls"}]}`)
	claude, _, err := ConvertResponse(OpenAIChat, ClaudeMessages, chat)
	if err != nil {
		t.Fatal(err)
	}
	object := decodeMap(t, claude)
	blocks := object["content"].([]any)
	var toolUse map[string]any
	for _, raw := range blocks {
		block := raw.(map[string]any)
		if block["type"] == "tool_use" {
			toolUse = block
		}
	}
	if toolUse == nil || toolUse["name"] != "lookup" || toolUse["id"] != "call_1" {
		t.Fatalf("tool_use lost: %s", claude)
	}
	if object["stop_reason"] != "tool_use" {
		t.Fatalf("stop_reason = %v", object["stop_reason"])
	}
	// Round trip back to Chat.
	back, _, err := ConvertResponse(ClaudeMessages, OpenAIChat, claude)
	if err != nil {
		t.Fatal(err)
	}
	backObject := decodeMap(t, back)
	choice := backObject["choices"].([]any)[0].(map[string]any)
	message := choice["message"].(map[string]any)
	calls, _ := message["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool call lost on the way back: %s", back)
	}
}

func TestErrorEnvelopeTaxonomy(t *testing.T) {
	body := []byte(`{"error":{"type":"invalid_request_error","message":"bad input"}}`)
	cases := map[Format]func(map[string]any){
		ClaudeMessages: func(object map[string]any) {
			if object["type"] != "error" {
				t.Fatalf("claude envelope = %v", object)
			}
			errorObject := object["error"].(map[string]any)
			if errorObject["type"] != "invalid_request_error" {
				t.Fatalf("claude error type = %v", errorObject)
			}
		},
		Gemini: func(object map[string]any) {
			errorObject := object["error"].(map[string]any)
			if errorObject["status"] != "INVALID_ARGUMENT" {
				t.Fatalf("gemini status = %v", errorObject)
			}
		},
		OpenAIChat: func(object map[string]any) {
			errorObject := object["error"].(map[string]any)
			if errorObject["type"] != "invalid_request_error" {
				t.Fatalf("openai type = %v", errorObject)
			}
		},
	}
	for format, check := range cases {
		check(decodeMap(t, ErrorEnvelope(body, format, 400)))
	}
	// A 500 must not present itself as a client error.
	checkServer := decodeMap(t, ErrorEnvelope([]byte(`boom`), ClaudeMessages, 500))
	if checkServer["error"].(map[string]any)["type"] != "api_error" {
		t.Fatalf("500 claude = %v", checkServer)
	}
}

func TestStripHostedSearchTools(t *testing.T) {
	cases := []struct {
		format Format
		body   string
		want   string
	}{
		{OpenAIChat, `{"web_search_options":{},"tools":[{"type":"web_search"},{"type":"function","function":{"name":"lookup"}}]}`, `"name":"lookup"`},
		{OpenAIResponses, `{"tools":[{"type":"web_search"},{"type":"function","name":"lookup"}]}`, `"name":"lookup"`},
		{ClaudeMessages, `{"tools":[{"type":"web_search_20250305","name":"web_search"},{"name":"lookup"}]}`, `"name":"lookup"`},
		{Gemini, `{"tools":[{"googleSearch":{}},{"functionDeclarations":[{"name":"lookup"}]}]}`, `"name":"lookup"`},
	}
	for _, test := range cases {
		got, changed, err := StripHostedSearchTools([]byte(test.body), test.format)
		if err != nil || !changed || !strings.Contains(string(got), test.want) {
			t.Fatalf("%s: %s, %t, %v", test.format, got, changed, err)
		}
		if strings.Contains(string(got), "web_search") || strings.Contains(string(got), "googleSearch") {
			t.Fatalf("%s: hosted search survived: %s", test.format, got)
		}
	}
}
