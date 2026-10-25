package conversation

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func TestConversation_NativeOpaqueAndProviderSwitch(t *testing.T) {
	s, _ := Decode(nil)
	scope := Scope{Provider: "a", Adapter: "responses", Model: "m", Format: dialect.OpenAIResponses}
	first := []byte(`{"model":"m","store":false,"input":[{"role":"user","content":"FIRST"}],"tools":[{"type":"mcp","authorization":"credential-secret"}]}`)
	response := []byte(`{"id":"resp_first","status":"completed","tools":[{"authorization":"credential-secret"}],"output":[{"type":"reasoning","id":"rs","encrypted_content":"opaque-bytes","summary":[]},{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ANSWER"}]},{"type":"compaction","id":"compact","encrypted_content":"compact-bytes"}]}`)
	s.Remember(scope, 1, first, response)
	encoded, _ := s.Encode()
	if strings.Contains(string(encoded), "credential-secret") {
		t.Fatal("native state retained an authorization credential")
	}
	s.Messages = []dialect.Message{{Role: dialect.RoleUser, Parts: []dialect.Part{{Kind: dialect.PartText, Text: "FIRST"}}}, {Role: dialect.RoleAssistant, Parts: []dialect.Part{{Kind: dialect.PartText, Text: "ANSWER"}}}}
	current, _, err := dialect.DecodeRequest(dialect.OpenAIResponses, []byte(`{"model":"m","input":"NEXT"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, _, err := s.Prepare(scope, 1, current, []byte(`{"model":"m","input":"NEXT"}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "opaque-bytes") || !strings.Contains(string(body), "compact-bytes") || !strings.Contains(string(body), "NEXT") {
		t.Fatalf("native replay lost opaque state: %s", body)
	}
	other := scope
	other.Provider = "b"
	body, _, err = s.Prepare(other, 1, current, []byte(`{"model":"m","input":"NEXT"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "opaque-bytes") || !strings.Contains(string(body), "FIRST") {
		t.Fatalf("provider switch leaked opaque state or lost portable history: %s", body)
	}
	body, _, err = s.Prepare(scope, 2, current, []byte(`{"model":"m","input":"AFTER_SWITCH"}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "opaque-bytes") || strings.Contains(string(body), "previous_response_id") {
		t.Fatalf("stale native lineage reused: %s", body)
	}
}

func TestConversation_StatefulAndStatelessReplay(t *testing.T) {
	s, _ := Decode(nil)
	scope := Scope{Provider: "a", Model: "m", Format: dialect.OpenAIResponses}
	s.Remember(scope, 1, []byte(`{"model":"m","input":[{"role":"user","content":"FIRST"}]}`), []byte(`{"id":"resp_first","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ANSWER"}]}]}`))
	actual := []byte(`{"model":"m","input":"NEXT","store":false}`)
	current, _, _ := dialect.DecodeRequest(dialect.OpenAIResponses, actual)
	prepared, _, err := s.Prepare(scope, 1, current, actual)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(prepared), `"previous_response_id":"resp_first"`) {
		t.Fatalf("missing continuation: %s", prepared)
	}
	replay, _, err := s.Replay(scope, 1, current, actual)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(replay), "previous_response_id") || !strings.Contains(string(replay), "FIRST") || !strings.Contains(string(replay), "ANSWER") {
		t.Fatalf("checkpoint is not self contained: %s", replay)
	}
	s.Remember(scope, 2, replay, []byte(`{"id":"resp_next","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ANSWER_NEXT"}]}]}`))
	body, _, err := s.Prepare(scope, 2, current, actual)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	_ = json.Unmarshal(body, &out)
	if out["previous_response_id"] != nil || !strings.Contains(string(body), "ANSWER_NEXT") {
		t.Fatalf("store:false used a server state handle: %s", body)
	}
}
