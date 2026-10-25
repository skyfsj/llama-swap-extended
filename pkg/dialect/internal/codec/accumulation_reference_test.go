package codec

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// chatChunk renders one OpenAI Chat SSE payload with the given delta fields.
func chatChunk(text, reasoning, refusal string) string {
	delta := map[string]any{}
	if text != "" {
		delta["content"] = text
	}
	if reasoning != "" {
		delta["reasoning_content"] = reasoning
	}
	if refusal != "" {
		delta["refusal"] = refusal
	}
	data, _ := marshalJSON(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": delta}}})
	return string(data)
}

func chatToolChunk(index int, id, name, arguments string) string {
	call := map[string]any{"index": index}
	if id != "" {
		call["id"] = id
	}
	if name != "" || arguments != "" {
		function := map[string]any{}
		if name != "" {
			function["name"] = name
		}
		if arguments != "" {
			function["arguments"] = arguments
		}
		call["function"] = function
	}
	data, _ := marshalJSON(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{call}}}}})
	return string(data)
}

func chatFinishChunk(reason string) string {
	data, _ := marshalJSON(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": reason}}})
	return string(data)
}

func chatUsageChunk(input, output int) string {
	data, _ := marshalJSON(map[string]any{"id": "c1", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}}},
		"usage":   map[string]any{"prompt_tokens": input, "completion_tokens": output}})
	return string(data)
}

// accumulate feeds the frames and returns the canonical response plus the
// emitted frames. It is the single entry point every test below uses, so the
// accumulation can only be validated through the real path.
func accumulate(t *testing.T, source, target Format, frames []string) (Response, []string) {
	t.Helper()
	converter, err := NewStreamConverter(source, target, StreamOptions{Model: "m"})
	if err != nil {
		t.Fatalf("NewStreamConverter: %v", err)
	}
	var emitted []string
	for _, frame := range frames {
		out, err := converter.Feed([]byte(frame))
		if err != nil {
			t.Fatalf("Feed(%q): %v", frame, err)
		}
		for _, f := range out {
			emitted = append(emitted, string(f.Data))
		}
	}
	terminal, err := converter.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, f := range terminal {
		emitted = append(emitted, string(f.Data))
	}
	return converter.response, emitted
}

// textFromFrames extracts every text delta the client actually received by
// reading the emitted frames of the target dialect. It is an oracle that is
// independent of the converter's internal accumulation: if the canonical
// response and the emitted frames disagree, one of them is wrong.
func textFromFrames(t *testing.T, target Format, frames []string) string {
	t.Helper()
	var b strings.Builder
	for _, frame := range frames {
		var object map[string]any
		if err := json.Unmarshal([]byte(frame), &object); err != nil {
			continue
		}
		switch target {
		case OpenAIChat:
			for _, raw := range objectArray(object, "choices") {
				choice, _ := raw.(map[string]any)
				delta, _ := choice["delta"].(map[string]any)
				if text, ok := delta["content"].(string); ok {
					b.WriteString(text)
				}
			}
		case ClaudeMessages:
			if object["type"] == "content_block_delta" {
				delta, _ := object["delta"].(map[string]any)
				if delta["type"] == "text_delta" {
					if text, ok := delta["text"].(string); ok {
						b.WriteString(text)
					}
				}
			}
		case Gemini:
			for _, raw := range objectArray(object, "candidates") {
				candidate, _ := raw.(map[string]any)
				content, _ := candidate["content"].(map[string]any)
				for _, rawPart := range arrayAt(content, "parts") {
					part, _ := rawPart.(map[string]any)
					if text, ok := part["text"].(string); ok {
						b.WriteString(text)
					}
				}
			}
		case OpenAIResponses:
			if object["type"] == "response.output_text.delta" {
				if text, ok := object["delta"].(string); ok {
					b.WriteString(text)
				}
			}
		case GeminiInteractions:
			// The Interactions stream nests the text inside a step delta.
			if delta, ok := object["delta"].(map[string]any); ok {
				if text, ok := delta["text"].(string); ok {
					b.WriteString(text)
				}
			}
		}
	}
	return b.String()
}

// objectArray reads a JSON array field, tolerating a missing or non-array
// value so the oracle can read frames it does not fully model.
func objectArray(object map[string]any, key string) []any {
	value, _ := object[key].([]any)
	return value
}

func canonicalText(parts []Part) string {
	var b strings.Builder
	for _, part := range parts {
		if part.Kind == PartText {
			b.WriteString(part.Text)
		}
	}
	return b.String()
}

// TestStreamConverter_AccumulatedTextMatchesEmittedFrames is the core
// invariant of the delta accumulation: the text folded into the canonical
// response must be exactly the text the emitted frames carry. A builder that
// drops, duplicates or misorders a fragment breaks one side of this equality.
func TestStreamConverter_AccumulatedTextMatchesEmittedFrames(t *testing.T) {
	cases := []struct {
		name   string
		frames []string
	}{
		{
			name:   "plain text",
			frames: []string{chatChunk("Hello ", "", ""), chatChunk("world", "", ""), chatChunk("!", "", ""), chatFinishChunk("stop")},
		},
		{
			name:   "empty deltas",
			frames: []string{chatChunk("", "", ""), chatChunk("x", "", ""), chatChunk("", "", ""), chatFinishChunk("stop")},
		},
		{
			name:   "unicode and newlines",
			frames: []string{chatChunk("héllo ", "", ""), chatChunk("世界\n", "", ""), chatChunk("\ttab", "", ""), chatFinishChunk("stop")},
		},
		{
			name:   "many small deltas",
			frames: manyTextFrames(500, 3),
		},
		{
			name:   "text around a tool call",
			frames: []string{chatChunk("before ", "", ""), chatToolChunk(0, "call_1", "lookup", ""), chatToolChunk(0, "", "", `{"q":`), chatChunk("between ", "", ""), chatToolChunk(0, "", "", `"z"}`), chatChunk("after", "", ""), chatFinishChunk("tool_calls")},
		},
	}
	for _, target := range []Format{ClaudeMessages, Gemini, OpenAIResponses, GeminiInteractions} {
		for _, tc := range cases {
			t.Run(fmt.Sprintf("chat->%s/%s", target, tc.name), func(t *testing.T) {
				response, emitted := accumulate(t, OpenAIChat, target, tc.frames)
				want := textFromFrames(t, target, emitted)
				got := canonicalText(response.Parts)
				if got != want {
					t.Fatalf("accumulated text does not match emitted frames\n got: %q\nwant: %q", got, want)
				}
			})
		}
	}
}

func manyTextFrames(count, tokenLen int) []string {
	frames := make([]string, 0, count+1)
	for i := range count {
		frames = append(frames, chatChunk(fmt.Sprintf("%0*d", tokenLen, i%10), "", ""))
	}
	return append(frames, chatFinishChunk("stop"))
}

// TestStreamConverter_TextPartMergingRules pins the merge rule itself: only
// consecutive parts of the same kind merge, and each delta keeps its place.
func TestStreamConverter_TextPartMergingRules(t *testing.T) {
	response, _ := accumulate(t, OpenAIChat, ClaudeMessages, []string{
		chatChunk("a", "", ""),
		chatChunk("", "b", ""),
		chatChunk("c", "", ""),
		chatChunk("", "", "d"),
		chatChunk("e", "", ""),
		chatFinishChunk("stop"),
	})
	want := []struct {
		kind PartKind
		text string
	}{
		{PartText, "a"},
		{PartReasoning, "b"},
		{PartText, "c"},
		{PartRefusal, "d"},
		{PartText, "e"},
	}
	if len(response.Parts) != len(want) {
		t.Fatalf("got %d parts, want %d: %+v", len(response.Parts), len(want), response.Parts)
	}
	for i, expected := range want {
		if response.Parts[i].Kind != expected.kind || response.Parts[i].Text != expected.text {
			t.Fatalf("part %d = %v/%q, want %v/%q", i, response.Parts[i].Kind, response.Parts[i].Text, expected.kind, expected.text)
		}
	}
}

// TestStreamConverter_ToolArgumentsAccumulate pins tool-argument accumulation:
// the deltas of one call concatenate in order, the parsed arguments round-trip,
// and a second call keeps its own buffer.
func TestStreamConverter_ToolArgumentsAccumulate(t *testing.T) {
	response, _ := accumulate(t, OpenAIChat, ClaudeMessages, []string{
		chatToolChunk(0, "call_1", "lookup", ""),
		chatToolChunk(1, "call_2", "search", ""),
		chatToolChunk(0, "", "", `{"q":`),
		chatToolChunk(1, "", "", `{"q":`),
		chatToolChunk(0, "", "", `"one"}`),
		chatToolChunk(1, "", "", `"two"}`),
		chatFinishChunk("tool_calls"),
	})
	if len(response.Parts) != 2 {
		t.Fatalf("got %d parts, want 2: %+v", len(response.Parts), response.Parts)
	}
	for i, expected := range []struct{ id, name, args string }{
		{"call_1", "lookup", `{"q":"one"}`},
		{"call_2", "search", `{"q":"two"}`},
	} {
		part := response.Parts[i]
		if part.Kind != PartToolCall || part.ToolCallID != expected.id || part.ToolName != expected.name {
			t.Fatalf("part %d = %+v, want call %s/%s", i, part, expected.id, expected.name)
		}
		if part.ArgumentsText != expected.args {
			t.Fatalf("part %d arguments = %q, want %q", i, part.ArgumentsText, expected.args)
		}
		if len(part.Arguments) > 0 {
			var decoded map[string]any
			if err := json.Unmarshal(part.Arguments, &decoded); err != nil {
				t.Fatalf("part %d arguments do not parse: %v (%q)", i, err, part.Arguments)
			}
		}
	}
}

// TestStreamConverter_ToolNameAfterArguments covers the Chat decoder's delayed
// announcement: arguments that arrive before the name are buffered and emitted
// with the name, and the accumulated arguments still equal the concatenation.
func TestStreamConverter_ToolNameAfterArguments(t *testing.T) {
	response, _ := accumulate(t, OpenAIChat, ClaudeMessages, []string{
		chatToolChunk(0, "call_9", "", ""),
		chatToolChunk(0, "", "", `{"q":"`),
		chatToolChunk(0, "", "lookup", ""),
		chatToolChunk(0, "", "", `y"}`),
		chatFinishChunk("tool_calls"),
	})
	if len(response.Parts) != 1 {
		t.Fatalf("got %d parts, want 1: %+v", len(response.Parts), response.Parts)
	}
	part := response.Parts[0]
	if part.ToolName != "lookup" || part.ToolCallID != "call_9" {
		t.Fatalf("part = %+v, want name lookup id call_9", part)
	}
	if part.ArgumentsText != `{"q":"y"}` {
		t.Fatalf("arguments = %q, want %q", part.ArgumentsText, `{"q":"y"}`)
	}
}

// TestStreamConverter_LargeAnswerAccumulatesExactly replays a long answer in
// many small deltas, the shape that made the naive concatenation quadratic. It
// asserts the exact concatenation, so any lost or duplicated fragment fails.
func TestStreamConverter_LargeAnswerAccumulatesExactly(t *testing.T) {
	const deltas = 2000
	frames := make([]string, 0, deltas+3)
	var want strings.Builder
	for i := range deltas {
		token := fmt.Sprintf("t%d ", i)
		want.WriteString(token)
		frames = append(frames, chatChunk(token, "", ""))
	}
	frames = append(frames, chatFinishChunk("stop"))
	frames = append(frames, chatToolChunk(0, "call_1", "lookup", ""))
	var wantArgs strings.Builder
	for i := range deltas {
		digit := fmt.Sprintf("%d", i%10)
		wantArgs.WriteString(digit)
		frames = append(frames, chatToolChunk(0, "", "", digit))
	}
	frames = append(frames, chatFinishChunk("tool_calls"))

	for _, target := range []Format{ClaudeMessages, Gemini, OpenAIResponses} {
		t.Run(fmt.Sprintf("chat->%s", target), func(t *testing.T) {
			response, _ := accumulate(t, OpenAIChat, target, frames)
			if len(response.Parts) != 2 {
				t.Fatalf("got %d parts, want 2: %+v", len(response.Parts), response.Parts)
			}
			if response.Parts[0].Text != want.String() {
				t.Fatalf("text len = %d, want %d", len(response.Parts[0].Text), want.Len())
			}
			if response.Parts[1].ArgumentsText != wantArgs.String() {
				t.Fatalf("arguments len = %d, want %d", len(response.Parts[1].ArgumentsText), wantArgs.Len())
			}
		})
	}
}

// TestStreamConverter_TruncatedStreamMaterializes covers the error path: a
// stream that stops without a terminal event still goes through Finish, so the
// partial answer must be materialized exactly like a completed one.
func TestStreamConverter_TruncatedStreamMaterializes(t *testing.T) {
	response, _ := accumulate(t, OpenAIChat, OpenAIResponses, []string{chatChunk("partial ", "", ""), chatChunk("answer", "", "")})
	if response.Status != "failed" {
		t.Fatalf("status = %q, want failed", response.Status)
	}
	if canonicalText(response.Parts) != "partial answer" {
		t.Fatalf("text = %q, want %q", canonicalText(response.Parts), "partial answer")
	}
}

// TestStreamConverter_UsageAndFinishCarry pins the remaining canonical fields
// the accumulation writes, so a refactor cannot silently drop them.
func TestStreamConverter_UsageAndFinishCarry(t *testing.T) {
	response, _ := accumulate(t, OpenAIChat, ClaudeMessages, []string{
		chatChunk("x", "", ""),
		chatUsageChunk(10, 2),
		chatFinishChunk("stop"),
	})
	if response.Usage.InputTokens != 10 || response.Usage.OutputTokens != 2 {
		t.Fatalf("usage = %+v, want input 10 output 2", response.Usage)
	}
	if response.FinishReason != FinishStop {
		t.Fatalf("finish reason = %q, want %q", response.FinishReason, FinishStop)
	}
	if response.ID != "c1" || response.Model != "m" {
		t.Fatalf("identity = %q/%q, want c1/m", response.ID, response.Model)
	}
}

// TestStreamConverter_DecodeStreamMatchesAccumulation checks the public
// non-streaming path: DecodeStream replays a captured stream and returns the
// canonical response, which must equal what the live converter accumulated.
func TestStreamConverter_DecodeStreamMatchesAccumulation(t *testing.T) {
	frames := []string{
		chatChunk("Hello ", "", ""),
		chatChunk("world", "", ""),
		chatToolChunk(0, "call_1", "lookup", ""),
		chatToolChunk(0, "", "", `{"q":`),
		chatToolChunk(0, "", "", `"x"}`),
		chatFinishChunk("tool_calls"),
	}
	var body strings.Builder
	for _, frame := range frames {
		body.WriteString("data: " + frame + "\n\n")
	}
	body.WriteString("data: [DONE]\n\n")

	decoded, _, err := DecodeStream(OpenAIChat, []byte(body.String()))
	if err != nil {
		t.Fatalf("DecodeStream: %v", err)
	}
	if decoded == nil {
		t.Fatal("DecodeStream returned no response")
	}
	live, _ := accumulate(t, OpenAIChat, ClaudeMessages, frames)
	if decoded.ID != live.ID || decoded.Model != live.Model {
		t.Fatalf("identity = %q/%q, want %q/%q", decoded.ID, decoded.Model, live.ID, live.Model)
	}
	if len(decoded.Parts) != len(live.Parts) {
		t.Fatalf("DecodeStream parts = %d, live = %d", len(decoded.Parts), len(live.Parts))
	}
	for i := range decoded.Parts {
		if decoded.Parts[i].Kind != live.Parts[i].Kind || decoded.Parts[i].Text != live.Parts[i].Text {
			t.Fatalf("part %d = %v/%q, want %v/%q", i, decoded.Parts[i].Kind, decoded.Parts[i].Text, live.Parts[i].Kind, live.Parts[i].Text)
		}
		if decoded.Parts[i].ArgumentsText != live.Parts[i].ArgumentsText {
			t.Fatalf("part %d arguments = %q, want %q", i, decoded.Parts[i].ArgumentsText, live.Parts[i].ArgumentsText)
		}
	}
	if decoded.FinishReason != live.FinishReason {
		t.Fatalf("finish reason = %q, want %q", decoded.FinishReason, live.FinishReason)
	}
}
