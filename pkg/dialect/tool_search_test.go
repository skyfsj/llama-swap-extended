package dialect

import (
	"encoding/json"
	"strings"
	"testing"
)

// The client-executed deferred-tool protocol. Codex declares a
// `{"type":"tool_search"}` tool, the model asks for tools by description, the
// client searches locally, and the next request carries the discovered
// definitions back inside a tool_search_output item. Every hop has to survive
// the conversion or the discovered tools stay unreachable.

const toolSearchDeclaration = `{"type":"tool_search","execution":"client","description":"search deferred tools","parameters":{"type":"object","properties":{"query":{"type":"string"},"limit":{"type":"number"}},"required":["query"]}}`

func TestResponsesToolSearchToolReachesChat(t *testing.T) {
	body := []byte(`{"model":"m","tools":[` + toolSearchDeclaration + `],"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"find a calendar tool"}]}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "hosted_tool_dropped" {
			t.Fatalf("the search tool was still reported dropped: %v", diagnostics)
		}
	}
	tools := arrayOf(decodeMap(t, converted)["tools"])
	if len(tools) != 1 {
		t.Fatalf("tools = %d, want the search tool: %s", len(tools), converted)
	}
	function := tools[0].(map[string]any)["function"].(map[string]any)
	if function["name"] != "tool_search" {
		t.Fatalf("search tool missing from the Chat tools list: %s", converted)
	}
	if !strings.Contains(string(converted), `"query"`) {
		t.Fatalf("the search parameters were not carried: %s", converted)
	}
}

// A model that calls the search function must reach codex as a
// tool_search_call, which codex dispatches itself. A function_call would be
// looked up in a registry that has no such tool.
func TestChatToolSearchCallBecomesResponsesItem(t *testing.T) {
	request := []byte(`{"model":"m","tools":[` + toolSearchDeclaration + `],"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"find a tool"}]}]}`)
	parsed, _, err := DecodeRequest(OpenAIResponses, request)
	if err != nil {
		t.Fatal(err)
	}
	response := []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,
		"message":{"role":"assistant","content":null,"tool_calls":[{"id":"ts1","type":"function",
		"function":{"name":"tool_search","arguments":"{\"query\":\"calendar\",\"limit\":8}"}}]},
		"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	converted, _, err := ConvertResponseWithRequest(OpenAIChat, OpenAIResponses, response, parsed)
	if err != nil {
		t.Fatal(err)
	}
	output := arrayOf(decodeMap(t, converted)["output"])
	if len(output) != 1 {
		t.Fatalf("output = %d items: %s", len(output), converted)
	}
	item := output[0].(map[string]any)
	if item["type"] != "tool_search_call" {
		t.Fatalf("a search call was rendered as %v: %s", item["type"], converted)
	}
	if item["call_id"] != "ts1" {
		t.Fatalf("call_id lost: %s", converted)
	}
	if item["execution"] != "client" {
		t.Fatalf("execution must name the client: %s", converted)
	}
	arguments, ok := item["arguments"].(map[string]any)
	if !ok {
		t.Fatalf("arguments must be an object, got %T: %s", item["arguments"], converted)
	}
	if arguments["query"] != "calendar" {
		t.Fatalf("arguments lost: %s", converted)
	}
	// The declaration travels back as its own type, not a plain function.
	tools := arrayOf(decodeMap(t, converted)["tools"])
	if len(tools) == 0 || tools[0].(map[string]any)["type"] != "tool_search" {
		t.Fatalf("the search tool declaration was flattened on the way back: %s", converted)
	}
}

// The tools the client discovered arrive as declarations inside the output
// item. Without them joining the callable set the model can never call one.
func TestResponsesToolSearchOutputDeclaresDiscoveredTools(t *testing.T) {
	body := []byte(`{"model":"m","tools":[` + toolSearchDeclaration + `],"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"find a calendar tool"}]},
		{"type":"tool_search_call","call_id":"ts1","execution":"client","arguments":{"query":"calendar"}},
		{"type":"tool_search_output","call_id":"ts1","status":"completed","execution":"client","tools":[
			{"type":"namespace","name":"mcp__cal","description":"calendar","tools":[
				{"type":"function","name":"create_event","parameters":{"type":"object","properties":{}}}]}]},
		{"type":"function_call","call_id":"cc1","name":"create_event","namespace":"mcp__cal","arguments":"{}"},
		{"type":"function_call_output","call_id":"cc1","output":"ok"}]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIResponses, OpenAIChat, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"name":"mcp__cal__create_event"`) {
		t.Fatalf("the discovered tool was not declared to the target: %s (%v)", converted, diagnostics)
	}
	messages := messagesOf(t, converted)
	assertChatPairingValid(t, messages, diagnostics)

	// The search call and its result pair up so the turn reads the same way.
	if messages[1]["role"] != "assistant" || messages[2]["role"] != "tool" {
		t.Fatalf("the search call and its result were not replayed as a pair: %s", converted)
	}
	if messages[2]["tool_call_id"] != "ts1" {
		t.Fatalf("the search result answers the wrong call: %s", converted)
	}
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == "input_item_dropped" {
			t.Fatalf("a search item was dropped: %v", diagnostics)
		}
	}
}

// codex types a search call's arguments as an object. A truncated argument
// must not reach it as a string, and an empty query silently discovers
// nothing, so the substitution is reported.
func TestChatToolSearchCallWithMalformedArgumentsIsDiagnosed(t *testing.T) {
	request := []byte(`{"model":"m","tools":[` + toolSearchDeclaration + `],"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"find a tool"}]}]}`)
	parsed, _, err := DecodeRequest(OpenAIResponses, request)
	if err != nil {
		t.Fatal(err)
	}
	response := []byte(`{"id":"c1","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,
		"message":{"role":"assistant","content":null,"tool_calls":[{"id":"ts1","type":"function",
		"function":{"name":"tool_search","arguments":"{\"query\":\"cal"}}]},
		"finish_reason":"tool_calls"}]}`)
	converted, diagnostics, err := ConvertResponseWithRequest(OpenAIChat, OpenAIResponses, response, parsed)
	if err != nil {
		t.Fatal(err)
	}
	item := arrayOf(decodeMap(t, converted)["output"])[0].(map[string]any)
	if _, ok := item["arguments"].(map[string]any); !ok {
		t.Fatalf("a truncated argument reached codex as %T instead of an object: %s", item["arguments"], converted)
	}
	// The decoder already reports the unparseable upstream arguments; the
	// search encoder must not turn that silence into an unremarked empty query.
	if !hasDiagnostic(diagnostics, "invalid_tool_arguments") && !hasDiagnostic(diagnostics, "tool_search_arguments_invalid") {
		t.Fatalf("the empty query was silent: %v", diagnostics)
	}
}

// A discovered tool call must come back named so codex can resolve it, and a
// search call must not be streamed as a function call.
func TestChatToolSearchStreamRendersSearchItem(t *testing.T) {
	request := []byte(`{"model":"m","tools":[` + toolSearchDeclaration + `],"stream":true,"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"find a tool"}]}]}`)
	parsed, _, err := DecodeRequest(OpenAIResponses, request)
	if err != nil {
		t.Fatal(err)
	}
	frames := []string{
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"ts1","type":"function","function":{"name":"tool_search","arguments":""}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"query\":\"calendar\"}"}}]},"finish_reason":null}]}`,
		`{"id":"c1","object":"chat.completion.chunk","created":1,"model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
	}
	encoder, err := NewStreamConverter(OpenAIChat, OpenAIResponses, StreamOptions{Model: "m", ID: "resp_1", Request: parsed})
	if err != nil {
		t.Fatal(err)
	}
	var encoded strings.Builder
	for _, frame := range frames {
		emitted, err := encoder.Feed([]byte(frame))
		if err != nil {
			t.Fatalf("frame: %v", err)
		}
		for _, out := range emitted {
			encoded.Write(out.Data)
			encoded.WriteString("\n")
		}
	}
	tail, err := encoder.Finish()
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	for _, out := range tail {
		encoded.Write(out.Data)
		encoded.WriteString("\n")
	}
	output := encoded.String()
	if !strings.Contains(output, `"tool_search_call"`) {
		t.Fatalf("a streamed search call was not rendered as a search item: %s", output)
	}
	if strings.Contains(output, `"type":"function_call"`) {
		t.Fatalf("a search call leaked as a plain function call: %s", output)
	}

	var done map[string]any
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimPrefix(line, "data: ")
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(line), &event) != nil {
			continue
		}
		if event["type"] == "response.output_item.done" {
			done, _ = event["item"].(map[string]any)
		}
	}
	if done == nil {
		t.Fatalf("no output_item.done emitted: %s", output)
	}
	arguments, ok := done["arguments"].(map[string]any)
	if !ok || arguments["query"] != "calendar" {
		t.Fatalf("streamed arguments were not carried as an object: %v", done["arguments"])
	}
}
