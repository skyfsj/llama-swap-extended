package dialect

// Anthropic verifies the signature of every thinking block a request replays
// and rejects the whole turn (HTTP 400) when one is missing. Reasoning that
// came from another dialect has no Anthropic signature — Gemini signs with its
// own opaque value, Chat has no signature at all — so such a block must not be
// replayed. Dropping it costs the summary; sending it fails the turn.
import (
	"strings"
	"testing"
)

func TestClaudeReplayDropsUnsignedThinkingBlock(t *testing.T) {
	chat := []byte(`{"model":"m","messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":"answer","reasoning_content":"summary"},
		{"role":"user","content":"again"}
	]}`)
	converted, diagnostics, err := ConvertRequest(OpenAIChat, ClaudeMessages, chat, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converted), `"type":"thinking"`) {
		t.Fatalf("an unsigned thinking block was replayed: %s", converted)
	}
	if !hasDiagnostic(diagnostics, "thinking_block_dropped") {
		t.Fatalf("the dropped reasoning block was not diagnosed: %v", diagnostics)
	}
	if !strings.Contains(string(converted), `"text":"answer"`) {
		t.Fatalf("the visible answer was lost too: %s", converted)
	}
}

// A Gemini thought part carries Google's own signature, which Anthropic cannot
// verify either; it lives in ThoughtSignature, and copying it into Signature
// would be silently wrong (Anthropic rejects a signature it did not mint).
func TestClaudeReplayDropsGeminiThoughtSignature(t *testing.T) {
	gemini := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"text":"summary","thought":true,"thoughtSignature":"SIG-GOOGLE"},{"text":"answer"}]},
		{"role":"user","parts":[{"text":"again"}]}
	]}`)
	converted, diagnostics, err := ConvertRequest(Gemini, ClaudeMessages, gemini, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(converted), "SIG-GOOGLE") {
		t.Fatalf("Google's signature was replayed as an Anthropic signature: %s", converted)
	}
	if !hasDiagnostic(diagnostics, "thinking_block_dropped") {
		t.Fatalf("diagnostics = %v", diagnostics)
	}
}

// A native Anthropic turn keeps its signed thinking block: that is the one case
// where the signature is real and required.
func TestClaudeReplayKeepsSignedThinkingBlock(t *testing.T) {
	body := []byte(`{"model":"m","max_tokens":64,"messages":[
		{"role":"user","content":"hi"},
		{"role":"assistant","content":[{"type":"thinking","thinking":"summary","signature":"SIG-CLAUDE"},{"type":"text","text":"answer"}]},
		{"role":"user","content":"again"}
	]}`)
	converted, diagnostics, err := ConvertRequest(ClaudeMessages, ClaudeMessages, body, ConvertOptions{Model: "m"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(converted), `"signature":"SIG-CLAUDE"`) {
		t.Fatalf("a signed thinking block lost its signature: %s (%v)", converted, diagnostics)
	}
	if hasDiagnostic(diagnostics, "thinking_block_dropped") {
		t.Fatalf("a signed block was reported as dropped: %v", diagnostics)
	}
}

func hasDiagnostic(diagnostics []Diagnostic, code string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Code == code {
			return true
		}
	}
	return false
}
