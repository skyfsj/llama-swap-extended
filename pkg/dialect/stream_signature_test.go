package dialect

// A Gemini Interactions stream delivers a tool call's thought signature as its
// own step.delta, after the arguments. The canonical accumulator is what
// DecodeStream and the conversation recorder read, so a signature that only
// reached the encoder's per-index map was lost from the transcript: replaying
// that conversation dropped the signature Gemini requires.
import (
	"bytes"
	"strings"
	"testing"
)

func TestInteractionsStreamSignatureReachesAccumulatedToolCall(t *testing.T) {
	stream := strings.Join([]string{
		`event: interaction.created` + "\n" + `data: {"event_type":"interaction.created","interaction":{"id":"i_1","model":"gemini-4-argon","status":"in_progress"}}`,
		`event: step.start` + "\n" + `data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call_a","name":"lookup","arguments":{}}}`,
		`event: step.delta` + "\n" + `data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"q\":\"x\"}"}}`,
		`event: step.delta` + "\n" + `data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"SIG-INTERACTIONS"}}`,
		`event: step.stop` + "\n" + `data: {"event_type":"step.stop","index":0}`,
		`event: interaction.completed` + "\n" + `data: {"event_type":"interaction.completed","interaction":{"id":"i_1","model":"gemini-4-argon","status":"completed","step_count":1}}`,
	}, "\n\n") + "\n\n"

	response, diagnostics, err := DecodeStream(GeminiInteractions, []byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Parts) != 1 || response.Parts[0].Kind != PartToolCall {
		t.Fatalf("parts = %+v", response.Parts)
	}
	if response.Parts[0].ThoughtSignature != "SIG-INTERACTIONS" {
		t.Fatalf("the accumulated tool call lost its thought signature: %+v (diagnostics=%v)",
			response.Parts[0], diagnostics)
	}
}

// A signature that arrives before any text opens the reasoning part; a later
// delta carrying only the signature attaches it to that same part.
func TestGeminiStreamKeepsSignatureAcrossFrames(t *testing.T) {
	body := bytes.NewBufferString(strings.Join([]string{
		`data: {"candidates":[{"content":{"parts":[{"text":"thinking","thought":true}]},"index":0}]}`,
		`data: {"candidates":[{"content":{"parts":[{"text":"","thought":true,"thoughtSignature":"SIG-LATE"}]},"index":0}]}`,
		`data: {"candidates":[{"content":{"parts":[]},"finishReason":"STOP","index":0}]}`,
	}, "\n\n") + "\n\n")

	response, diagnostics, err := DecodeStream(Gemini, body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Parts) == 0 || response.Parts[0].Kind != PartReasoning {
		t.Fatalf("parts = %+v", response.Parts)
	}
	if response.Parts[0].ThoughtSignature != "SIG-LATE" {
		t.Fatalf("a signature that arrived in a later frame was lost: %+v (diagnostics=%v)",
			response.Parts[0], diagnostics)
	}
}

// A thought step before the call shifts the accumulated part index, so an
// implementation that attaches the signature "to whatever part is open" by
// part index files it against the wrong tool call. The signature belongs to
// the call's own step index.
func TestInteractionsStreamSignatureAfterThoughtHitsItsOwnCall(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"interaction.created","interaction":{"id":"i_1","model":"gemini-4-argon","status":"in_progress"}}`,
		`data: {"event_type":"step.start","index":0,"step":{"type":"thought"}}`,
		`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"thinking"}}}`,
		`data: {"event_type":"step.start","index":1,"step":{"type":"function_call","id":"call_b","name":"lookup","arguments":{}}}`,
		`data: {"event_type":"step.delta","index":1,"delta":{"type":"thought_signature","signature":"SIG-CALL-B"}}`,
		`data: {"event_type":"step.stop","index":1}`,
		`data: {"event_type":"interaction.completed","interaction":{"id":"i_1","model":"gemini-4-argon","status":"completed"}}`,
	}, "\n\n") + "\n\n"

	response, diagnostics, err := DecodeStream(GeminiInteractions, []byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	var call *Part
	for i := range response.Parts {
		if response.Parts[i].Kind == PartToolCall {
			call = &response.Parts[i]
		}
	}
	if call == nil {
		t.Fatalf("no tool call accumulated: %+v", response.Parts)
	}
	if call.ToolCallID != "call_b" || call.ThoughtSignature != "SIG-CALL-B" {
		t.Fatalf("the signature landed on the wrong part: %+v (diagnostics=%v)", call, diagnostics)
	}
}

// openToolIndex reports an accumulated *parts* index, but the delta's Index
// field is a *step* index. The two only coincide when the stream's step numbers
// happen to match the parts list; with two calls whose steps are numbered 3 and
// 7 the signature was filed against a nonexistent stream index and dropped.
func TestInteractionsStreamSignatureUsesStepIndexNotPartsIndex(t *testing.T) {
	stream := strings.Join([]string{
		`data: {"event_type":"interaction.created","interaction":{"id":"i_1","model":"gemini-4-argon","status":"in_progress"}}`,
		`data: {"event_type":"step.start","index":3,"step":{"type":"function_call","id":"call_a","name":"first","arguments":{}}}`,
		`data: {"event_type":"step.stop","index":3}`,
		`data: {"event_type":"step.start","index":7,"step":{"type":"function_call","id":"call_b","name":"second","arguments":{}}}`,
		`data: {"event_type":"step.delta","index":7,"delta":{"type":"thought_signature","signature":"SIG-SECOND"}}`,
		`data: {"event_type":"step.stop","index":7}`,
		`data: {"event_type":"interaction.completed","interaction":{"id":"i_1","model":"gemini-4-argon","status":"completed"}}`,
	}, "\n\n") + "\n\n"

	response, diagnostics, err := DecodeStream(GeminiInteractions, []byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	var second *Part
	for i := range response.Parts {
		if response.Parts[i].ToolCallID == "call_b" {
			second = &response.Parts[i]
		}
	}
	if second == nil {
		t.Fatalf("call_b was not accumulated: %+v", response.Parts)
	}
	if second.ThoughtSignature != "SIG-SECOND" {
		t.Fatalf("the signature was dropped (step index vs parts index): %+v (diagnostics=%v)", second, diagnostics)
	}
}
