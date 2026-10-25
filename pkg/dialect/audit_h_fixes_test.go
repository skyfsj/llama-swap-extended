package dialect_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// TestConvertRequest_AllowedToolsKeepsMode pins the allowed_tools contract:
// the mode is the selection policy and the tool list is the permitted set. A
// single permitted tool with mode auto is not a forced call, and the tools
// outside the set must not reach a target that cannot express the policy.
func TestConvertRequest_AllowedToolsKeepsMode(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"tools":[
			{"type":"function","name":"read","parameters":{"type":"object"}},
			{"type":"function","name":"write","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"function","name":"read"}]},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]
	}`)
	request, _, err := dialect.DecodeRequest(dialect.OpenAIResponses, body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if request.ToolChoice == nil || request.ToolChoice.Mode != dialect.ToolChoiceAuto {
		t.Fatalf("mode auto with one allowed tool became %+v", request.ToolChoice)
	}
	if len(request.ToolChoice.AllowedNames) != 1 || request.ToolChoice.AllowedNames[0] != "read" {
		t.Fatalf("allowed set = %v", request.ToolChoice.AllowedNames)
	}

	converted, diagnostics, err := dialect.ConvertRequest(dialect.OpenAIResponses, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out struct {
		Tools      []map[string]any `json:"tools"`
		ToolChoice any              `json:"tool_choice"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ToolChoice != "auto" {
		t.Fatalf("tool_choice = %v, want auto", out.ToolChoice)
	}
	if len(out.Tools) != 1 || out.Tools[0]["function"].(map[string]any)["name"] != "read" {
		t.Fatalf("tools were not narrowed to the permitted set: %v", out.Tools)
	}
	if !hasDiagnostic(diagnostics, "allowed_tools_filtered") {
		t.Fatal("narrowing the tool list must be reported")
	}
}

// TestConvertRequest_AllowedToolsResponsesRoundTrip verifies the same policy
// survives when the target is Responses itself, where no narrowing is needed.
func TestConvertRequest_AllowedToolsResponsesRoundTrip(t *testing.T) {
	body := []byte(`{
		"model":"m",
		"tools":[{"type":"function","name":"read","parameters":{"type":"object"}},{"type":"function","name":"write","parameters":{"type":"object"}}],
		"tool_choice":{"type":"allowed_tools","mode":"required","tools":[{"type":"function","name":"read"},{"type":"function","name":"write"}]},
		"input":"hi"
	}`)
	request, _, err := dialect.DecodeRequest(dialect.OpenAIResponses, body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if request.ToolChoice.Mode != dialect.ToolChoiceRequired || len(request.ToolChoice.AllowedNames) != 2 {
		t.Fatalf("policy = %+v", request.ToolChoice)
	}
	converted, _, err := dialect.EncodeRequest(request, dialect.OpenAIResponses, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	var out struct {
		ToolChoice map[string]any `json:"tool_choice"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.ToolChoice["type"] != "allowed_tools" || out.ToolChoice["mode"] != "required" {
		t.Fatalf("tool_choice = %v", out.ToolChoice)
	}
	if tools, _ := out.ToolChoice["tools"].([]any); len(tools) != 2 {
		t.Fatalf("allowed tools = %v", out.ToolChoice["tools"])
	}
}

// TestConvertResponse_ChatErrorEnvelopeIsFailure covers an OpenAI-compatible
// upstream that answers HTTP 200 with an error object: reporting a successful
// empty answer hides the failure.
func TestConvertResponse_ChatErrorEnvelopeIsFailure(t *testing.T) {
	body := []byte(`{"error":{"message":"model overloaded","type":"server_error","code":"overloaded"}}`)
	if !dialect.ResponseFailed(dialect.OpenAIChat, body) {
		t.Fatal("a Chat error envelope must be reported as a failure")
	}
	converted, _, err := dialect.ConvertResponse(dialect.OpenAIChat, dialect.ClaudeMessages, body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !strings.Contains(string(converted), "model overloaded") {
		t.Fatalf("the error message was lost: %s", converted)
	}
}

// TestStreamChatErrorFrameFailsStream covers a mid-stream error frame.
func TestStreamChatErrorFrameFailsStream(t *testing.T) {
	converter, err := dialect.NewStreamConverter(dialect.OpenAIChat, dialect.OpenAIResponses, dialect.StreamOptions{Model: "m"})
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	output := feedAll(t, converter, []string{
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"partial"}}]}`,
		`{"error":{"message":"upstream exploded","type":"server_error"}}`,
	})
	if !strings.Contains(output, "response.failed") {
		t.Fatalf("a mid-stream error must fail the response:\n%s", output)
	}
	if !strings.Contains(output, "upstream exploded") {
		t.Fatalf("the upstream error message was lost:\n%s", output)
	}
}

// TestStreamTruncatedWithoutFinishFails covers an upstream that stops after
// partial output with no finish_reason and no [DONE].
func TestStreamTruncatedWithoutFinishFails(t *testing.T) {
	converter, err := dialect.NewStreamConverter(dialect.OpenAIChat, dialect.OpenAIResponses, dialect.StreamOptions{Model: "m"})
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	output := feedAll(t, converter, []string{
		`{"id":"c","model":"m","choices":[{"index":0,"delta":{"content":"half an ans"}}]}`,
	})
	if !strings.Contains(output, "response.failed") {
		t.Fatalf("a stream that stopped mid-answer must not report completion:\n%s", output)
	}
	if strings.Contains(output, "response.completed") {
		t.Fatalf("truncated stream reported as completed:\n%s", output)
	}
	if diagnostics := converter.Diagnostics(); !hasDiagnostic(diagnostics, "stream_truncated") {
		t.Fatalf("truncation must be diagnosed: %v", diagnostics)
	}
}

// TestConvertRequest_ResponsesConversationIsRejected covers a stateful
// reference the target cannot resolve: silently starting a fresh conversation
// loses the referenced turns.
func TestConvertRequest_ResponsesConversationIsRejected(t *testing.T) {
	body := []byte(`{"model":"m","conversation":"conv_123","input":"hi"}`)
	_, _, err := dialect.ConvertRequest(dialect.OpenAIResponses, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err == nil {
		t.Fatal("conversation must not be dropped silently")
	}
	if !strings.Contains(err.Error(), "conversation") {
		t.Fatalf("error = %v", err)
	}
}

// TestConvertRequest_ResponsesBackgroundIsDiagnosed covers a background turn
// that the target runs synchronously.
func TestConvertRequest_ResponsesBackgroundIsDiagnosed(t *testing.T) {
	body := []byte(`{"model":"m","background":true,"input":"hi"}`)
	_, diagnostics, err := dialect.ConvertRequest(dialect.OpenAIResponses, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !hasDiagnostic(diagnostics, "background_dropped") {
		t.Fatalf("background was dropped without a diagnostic: %v", diagnostics)
	}
}

// TestConvertRequest_ClaudeThinkingBudgetIsDiagnosed covers an Anthropic
// thinking budget converted into Chat, which has no matching control.
func TestConvertRequest_ClaudeThinkingBudgetIsDiagnosed(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"thinking":{"type":"enabled","budget_tokens":2048},"messages":[{"role":"user","content":"hi"}]}`)
	_, diagnostics, err := dialect.ConvertRequest(dialect.ClaudeMessages, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !hasDiagnostic(diagnostics, "reasoning_budget_dropped") {
		t.Fatalf("thinking budget vanished without a diagnostic: %v", diagnostics)
	}
}

// TestConvertRequest_ClaudeThinkingDisabledIsDiagnosed covers an explicit
// disable the target cannot express.
func TestConvertRequest_ClaudeThinkingDisabledIsDiagnosed(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"thinking":{"type":"disabled"},"messages":[{"role":"user","content":"hi"}]}`)
	_, diagnostics, err := dialect.ConvertRequest(dialect.ClaudeMessages, dialect.OpenAIChat, body, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if !hasDiagnostic(diagnostics, "reasoning_disable_dropped") {
		t.Fatalf("a disabled thinking policy disappeared: %v", diagnostics)
	}
}

// TestConvertRequest_GeminiThinkingLevelIsPreserved covers Gemini's
// model-native thinking level, which is not a token budget.
func TestConvertRequest_GeminiThinkingLevelIsPreserved(t *testing.T) {
	body := []byte(`{
		"contents":[{"role":"user","parts":[{"text":"hi"}]}],
		"generationConfig":{"thinkingConfig":{"thinkingLevel":"low","includeThoughts":true}}
	}`)
	request, _, err := dialect.DecodeRequest(dialect.Gemini, body)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if request.Reasoning == nil || request.Reasoning.Level != "low" {
		t.Fatalf("thinkingLevel = %+v", request.Reasoning)
	}
	converted, _, err := dialect.EncodeRequest(request, dialect.Gemini, dialect.ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(converted), `"thinkingLevel":"low"`) {
		t.Fatalf("thinkingLevel was not carried back: %s", converted)
	}
	if !strings.Contains(string(converted), `"includeThoughts":true`) {
		t.Fatalf("includeThoughts was lost: %s", converted)
	}
}

// TestConvertResponse_GeminiThoughtTokensConverted pins the token accounting
// boundary: Gemini reports visible and thinking tokens separately, while the
// canonical output count includes reasoning.
func TestConvertResponse_GeminiThoughtTokensConverted(t *testing.T) {
	body := []byte(`{
		"candidates":[{"content":{"role":"model","parts":[{"text":"answer"}]},"finishReason":"STOP"}],
		"usageMetadata":{"promptTokenCount":10,"candidatesTokenCount":5,"thoughtsTokenCount":20,"totalTokenCount":35}
	}`)
	converted, _, err := dialect.ConvertResponse(dialect.Gemini, dialect.OpenAIResponses, body)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var out struct {
		Usage struct {
			OutputTokens        int `json:"output_tokens"`
			OutputTokensDetails struct {
				ReasoningTokens int `json:"reasoning_tokens"`
			} `json:"output_tokens_details"`
			TotalTokens int `json:"total_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(converted, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.Usage.OutputTokens != 25 || out.Usage.OutputTokensDetails.ReasoningTokens != 20 {
		t.Fatalf("usage = %+v, want output 25 (5 visible + 20 thinking) with 20 reasoning", out.Usage)
	}
	if out.Usage.OutputTokensDetails.ReasoningTokens > out.Usage.OutputTokens {
		t.Fatalf("reasoning cannot exceed the output total: %+v", out.Usage)
	}
}

// TestConvertResponse_GeminiKeepsThoughtSignature covers the response path:
// a signature that survives the request must survive the response too.
func TestConvertResponse_GeminiKeepsThoughtSignature(t *testing.T) {
	parts := []dialect.Part{
		{Kind: dialect.PartReasoning, Text: "thinking", ThoughtSignature: "sig-123"},
		{Kind: dialect.PartToolCall, ToolCallID: "c1", ToolName: "f", Arguments: json.RawMessage(`{}`), ThoughtSignature: "sig-456"},
	}
	encoded, _, err := dialect.EncodeResponse(&dialect.Response{Parts: parts}, dialect.Gemini)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.Contains(string(encoded), `"thoughtSignature":"sig-123"`) {
		t.Fatalf("reasoning signature lost: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"thoughtSignature":"sig-456"`) {
		t.Fatalf("tool-call signature lost: %s", encoded)
	}
}

func feedAll(t *testing.T, converter *dialect.StreamConverter, frames []string) string {
	t.Helper()
	var output strings.Builder
	for _, payload := range frames {
		emitted, err := converter.Feed([]byte(payload))
		if err != nil {
			t.Fatalf("Feed: %v", err)
		}
		for _, frame := range emitted {
			if frame.Event != "" {
				output.WriteString("event: ")
				output.WriteString(frame.Event)
				output.WriteString("\n")
			}
			output.Write(frame.Data)
			output.WriteString("\n")
		}
	}
	tail, err := converter.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, frame := range tail {
		if frame.Event != "" {
			output.WriteString("event: ")
			output.WriteString(frame.Event)
			output.WriteString("\n")
		}
		output.Write(frame.Data)
		output.WriteString("\n")
	}
	return output.String()
}
