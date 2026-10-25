package server

import (
	"encoding/json"
	"testing"
	"time"
)

func TestGeminiSignaturesInResponse(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[
		{"thoughtSignature":"sig-a","functionCall":{"id":"call_a","name":"read","args":{}}},
		{"functionCall":{"id":"call_no_sig","name":"write","args":{}}},
		{"text":"answer"}
	]}}]}`)
	found, _ := geminiSignaturesInResponse(body, 0)
	if len(found) != 1 || found["call_a"] != "sig-a" {
		t.Fatalf("signatures = %v, want only call_a", found)
	}
	if _, ok := found["call_no_sig"]; ok {
		t.Fatalf("a call without a signature was recorded: %v", found)
	}
}

// Gemini does not require functionCall.id, and the common response omits it.
// The canonical part the client sees then carries the decoder's positional id,
// so the signature has to be filed under that same id — otherwise the cache
// never matches the replay it exists for.
func TestGeminiSignaturesInResponseFilesIdlessCallsUnderPositionalID(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[
		{"functionCall":{"name":"read","args":{}},"thoughtSignature":"sig-first"},
		{"functionCall":{"name":"write","args":{}}},
		{"functionCall":{"name":"delete","args":{}},"thoughtSignature":"sig-third"}
	]}}]}`)
	found, calls := geminiSignaturesInResponse(body, 0)
	if calls != 3 {
		t.Fatalf("call count = %d, want 3", calls)
	}
	if found["call_gemini_1"] != "sig-first" || found["call_gemini_3"] != "sig-third" {
		t.Fatalf("id-less signatures filed under the wrong ids: %v", found)
	}
}

// A stream numbers calls across frames, so the positional id a client is given
// in frame 2 must not collide with the one it was given in frame 1.
func TestGeminiSignaturesInResponseContinuesStreamNumbering(t *testing.T) {
	first := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"a","args":{}},"thoughtSignature":"sig-a"}]}}]}`)
	second := []byte(`{"candidates":[{"content":{"parts":[{"functionCall":{"name":"b","args":{}},"thoughtSignature":"sig-b"}]}}]}`)

	found, calls := geminiSignaturesInResponse(first, 0)
	if found["call_gemini_1"] != "sig-a" || calls != 1 {
		t.Fatalf("first frame = %v (calls=%d)", found, calls)
	}
	found, calls = geminiSignaturesInResponse(second, calls)
	if found["call_gemini_2"] != "sig-b" || calls != 2 {
		t.Fatalf("second frame = %v (calls=%d)", found, calls)
	}
}

// An explicit upstream id wins over the positional one: that is the id the
// client replays.
func TestGeminiSignaturesInResponsePrefersExplicitID(t *testing.T) {
	body := []byte(`{"candidates":[{"content":{"parts":[
		{"functionCall":{"id":"call_x","name":"a","args":{}},"thoughtSignature":"sig-x"}
	]}}]}`)
	found, _ := geminiSignaturesInResponse(body, 0)
	if found["call_x"] != "sig-x" {
		t.Fatalf("signatures = %v", found)
	}
	if _, ok := found["call_gemini_1"]; ok {
		t.Fatalf("an explicit id was also filed under the positional one: %v", found)
	}
}

func TestGeminiSignatureCacheRoundTrip(t *testing.T) {
	cache := newGeminiSignatureCache()
	now := time.Now()
	scope := signatureScope("conversation-1", "gemini-4-argon", "prov/adapter")
	cache.store(scope, map[string]string{"call_a": "sig-a"}, now)

	found := cache.lookup(scope, []string{"call_a", "call_missing"}, now)
	if len(found) != 1 || found["call_a"] != "sig-a" {
		t.Fatalf("lookup = %v, want call_a only", found)
	}
}

func TestGeminiSignatureCacheIsolatesScope(t *testing.T) {
	cache := newGeminiSignatureCache()
	now := time.Now()
	cache.store(signatureScope("conversation-1", "gemini-4-argon", "prov/adapter"), map[string]string{"call_a": "sig-a"}, now)

	// Another conversation, the same conversation against another model and
	// against another provider's surface must all miss: a signature is opaque,
	// model-specific and minted by one upstream.
	if found := cache.lookup(signatureScope("conversation-2", "gemini-4-argon", "prov/adapter"), []string{"call_a"}, now); len(found) != 0 {
		t.Fatalf("signature leaked across conversations: %v", found)
	}
	if found := cache.lookup(signatureScope("conversation-1", "gemini-3-pro", "prov/adapter"), []string{"call_a"}, now); len(found) != 0 {
		t.Fatalf("signature leaked across models: %v", found)
	}
	if found := cache.lookup(signatureScope("conversation-1", "gemini-4-argon", "other/adapter"), []string{"call_a"}, now); len(found) != 0 {
		t.Fatalf("signature leaked across upstream surfaces: %v", found)
	}
}

// A caller that sends no session header still gets its signatures carried: the
// call id is the upstream's own identifier, so the match is the same call even
// without a conversation-scoped key.
func TestGeminiSignatureCacheServesAnonymousCallers(t *testing.T) {
	cache := newGeminiSignatureCache()
	now := time.Now()
	cache.store(signatureScope("", "gemini-4-argon", "prov/adapter"), map[string]string{"call_a": "sig-a"}, now)

	found := cache.lookup(signatureScope("", "gemini-4-argon", "prov/adapter"), []string{"call_a"}, now)
	if len(found) != 1 || found["call_a"] != "sig-a" {
		t.Fatalf("anonymous lookup = %v, want call_a", found)
	}
	// The model still partitions the anonymous scope.
	if found := cache.lookup(signatureScope("", "gemini-3-pro", "prov/adapter"), []string{"call_a"}, now); len(found) != 0 {
		t.Fatalf("anonymous signature leaked across models: %v", found)
	}
}

func TestGeminiSignatureCacheExpires(t *testing.T) {
	cache := newGeminiSignatureCache()
	now := time.Now()
	scope := signatureScope("conversation-1", "gemini-4-argon", "prov/adapter")
	cache.store(scope, map[string]string{"call_a": "sig-a"}, now)

	if found := cache.lookup(scope, []string{"call_a"}, now.Add(geminiSignatureTTL+time.Minute)); len(found) != 0 {
		t.Fatalf("expired signature was returned: %v", found)
	}
}

func TestApplyCachedGeminiSignatures(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"go"}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"read","args":{}}}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_b","name":"write","args":{}},"thoughtSignature":"real"}]}
	]}`)
	updated := applyCachedGeminiSignatures(body, map[string]string{"call_a": "sig-a", "call_b": "should-not-win"})

	var request map[string]any
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatal(err)
	}
	contents := request["contents"].([]any)
	first := contents[1].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if first["thoughtSignature"] != "sig-a" {
		t.Fatalf("cached signature was not restored: %v", first)
	}
	second := contents[2].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if second["thoughtSignature"] != "real" {
		t.Fatalf("a live signature was overwritten: %v", second)
	}
}

func TestApplyCachedGeminiSignaturesLeavesUnknownCallsAlone(t *testing.T) {
	body := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"read","args":{}}}]}]}`)
	updated := applyCachedGeminiSignatures(body, map[string]string{"call_other": "sig"})
	if string(updated) != string(body) {
		t.Fatalf("body changed without a matching signature: %s", updated)
	}
}

// The encoder writes the documented placeholder when a replayed call carries no
// signature, which is the common case for OpenAI/Anthropic clients. A cached
// signature must replace that placeholder, otherwise the cache never takes
// effect on exactly the path it exists for.
func TestApplyCachedGeminiSignaturesReplacesPlaceholder(t *testing.T) {
	body := []byte(`{"contents":[{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"read","args":{}},"thoughtSignature":"skip_thought_signature_validator"}]}]}`)
	updated := applyCachedGeminiSignatures(body, map[string]string{"call_a": "real-signature"})

	var request map[string]any
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatal(err)
	}
	part := request["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if part["thoughtSignature"] != "real-signature" {
		t.Fatalf("placeholder was not replaced by the cached signature: %v", part)
	}
}

// The positional id is what an id-less upstream call is replayed as, so the
// restore path has to match on it too.
func TestApplyCachedGeminiSignaturesRestoresPositionalCall(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"model","parts":[{"functionCall":{"name":"read","args":{},"id":"call_gemini_1"},"thoughtSignature":"skip_thought_signature_validator"}]}
	]}`)
	updated := applyCachedGeminiSignatures(body, map[string]string{"call_gemini_1": "sig-1"})

	var request map[string]any
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatal(err)
	}
	part := request["contents"].([]any)[0].(map[string]any)["parts"].([]any)[0].(map[string]any)
	if part["thoughtSignature"] != "sig-1" {
		t.Fatalf("positional signature was not restored: %v", part)
	}
}

// Interactions carries the signature on the function_call step itself.
func TestApplyCachedGeminiSignaturesOnInteractionsInput(t *testing.T) {
	body := []byte(`{"model":"gemini-4-argon","input":[
		{"type":"user_input","content":[{"type":"text","text":"go"}]},
		{"type":"function_call","id":"call_a","name":"read","arguments":{}},
		{"type":"function_result","call_id":"call_a","result":{"ok":true}}
	]}`)
	updated := applyCachedGeminiSignatures(body, map[string]string{"call_a": "sig-interactions"})

	var request map[string]any
	if err := json.Unmarshal(updated, &request); err != nil {
		t.Fatal(err)
	}
	step := request["input"].([]any)[1].(map[string]any)
	if step["signature"] != "sig-interactions" {
		t.Fatalf("Interactions signature was not restored: %v", step)
	}
}

func TestGeminiInteractionSignatures(t *testing.T) {
	body := []byte(`{"id":"i_1","steps":[
		{"type":"thought","signature":"thought-sig"},
		{"type":"function_call","id":"call_a","name":"read","arguments":{},"signature":"sig-a"},
		{"type":"function_call","id":"call_b","name":"write","arguments":{}}
	]}`)
	found := geminiInteractionSignatures(body)
	if len(found) != 1 || found["call_a"] != "sig-a" {
		t.Fatalf("signatures = %v, want only call_a", found)
	}
}

func TestReplayCallIDs(t *testing.T) {
	body := []byte(`{"contents":[
		{"role":"user","parts":[{"text":"go"}]},
		{"role":"model","parts":[{"functionCall":{"id":"call_a","name":"read","args":{}}}]}
	]}`)
	ids := replayCallIDs(body)
	if len(ids) != 1 || ids[0] != "call_a" {
		t.Fatalf("ids = %v, want [call_a]", ids)
	}
}

func TestReplayCallIDsOnInteractionsInput(t *testing.T) {
	body := []byte(`{"model":"m","input":[
		{"type":"user_input","content":[{"type":"text","text":"go"}]},
		{"type":"function_call","id":"call_a","name":"read","arguments":{}}
	]}`)
	ids := replayCallIDs(body)
	if len(ids) != 1 || ids[0] != "call_a" {
		t.Fatalf("ids = %v, want [call_a]", ids)
	}
}
