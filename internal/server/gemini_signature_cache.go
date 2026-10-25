// Gemini thought-signature carry-over.
//
// A Gemini 3 model stamps every function call it emits with an opaque
// thoughtSignature. A replay of that call must return the signature verbatim,
// but the OpenAI and Anthropic dialects have nowhere to put it: the signature
// is dropped on the way out to the client and the client cannot send it back.
// The next turn then fails upstream with
//
//	Function call is missing a thought_signature in functionCall parts
//
// which the upstream reports as an opaque ModelProtocolUnsupported error, so
// the conversation stalls with no actionable message.
//
// The relay sits between the two halves, so it keeps the mapping for the
// conversation and restores it on the way back in. The join key is the
// function call id the client replays, which the gateway forwards unchanged in
// both directions (Gemini functionCall.id, Chat tool_calls.id, Responses
// call_id, Interactions step.id).
//
// Gemini does not require functionCall.id, and a response that omits it is
// extremely common. The canonical part the client sees then carries the
// positional id the decoder mints ("call_gemini_<n>"), so capture numbers calls
// exactly the way the decoder does: by position within the response, counting
// every call whatever its id. Keying on the raw id would drop every signature
// Gemini minted for an id-less call, which is the case the cache exists for.
//
// Signatures are keyed per conversation, model *and* upstream surface:
// Google's signature is opaque, model-specific and minted by one adapter's
// upstream, so replaying it against another model or another provider's
// surface is never correct. An id that cannot be matched is simply left alone;
// the encoder then falls back to the documented placeholder instead of
// attaching a wrong signature, which would silently degrade output (or be
// rejected upstream).

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// geminiSignatureTTL bounds how long a signature stays repliable. It
	// matches the gateway's session-affinity window: a conversation idle for
	// longer has moved on, and signature lifetime is not documented as longer.
	geminiSignatureTTL = time.Hour
	// geminiSignatureLimit caps tracked calls so a busy relay cannot grow this
	// map without bound. The sweep below also drops idle entries.
	geminiSignatureLimit = 65536
)

// geminiSignatureCache maps (conversation, model, surface, call id) to the
// thought signature the upstream returned for that call. Safe for concurrent
// use.
type geminiSignatureCache struct {
	mu      sync.Mutex
	entries map[geminiSignatureKey]geminiSignatureEntry
	order   []geminiSignatureKey
	swept   time.Time
}

type geminiSignatureKey struct {
	scope  string
	callID string
}

type geminiSignatureEntry struct {
	signature string
	expires   time.Time
}

func newGeminiSignatureCache() *geminiSignatureCache {
	return &geminiSignatureCache{entries: map[geminiSignatureKey]geminiSignatureEntry{}}
}

// signatureScope identifies the conversation, model and upstream surface a
// signature belongs to.
//
// An empty conversation falls back to a model-scoped key rather than disabling
// the cache: a call id the upstream minted is still that same call, so a hit
// without a conversation is valid. Callers that do send a session header get
// the tighter per-conversation isolation on top.
//
// The backend label partitions providers and adapters: a signature minted by
// one provider's upstream cannot be replayed to another's.
func signatureScope(conversation, model, backend string) string {
	return conversation + "\x00" + model + "\x00" + backend
}

// signatureConversation returns the client-supplied conversation identity for
// a request, or "" when the caller sent none. These are the same headers the
// gateway uses for session affinity, so a caller that already pins its
// conversation to one upstream also pins its signatures.
func signatureConversation(r *http.Request) string {
	if r == nil {
		return ""
	}
	for _, header := range []string{
		"x-opencode-session",
		"X-Session-Affinity",
		"X-Session-Id",
		"X-Claude-Code-Session-Id",
		"Session-Id",
		"conversation_id",
		"prompt_cache_key",
	} {
		if value := strings.TrimSpace(r.Header.Get(header)); value != "" {
			return value
		}
	}
	return ""
}

// store records the signatures carried by one upstream response.
func (c *geminiSignatureCache) store(scope string, signatures map[string]string, now time.Time) {
	if c == nil || len(signatures) == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sweepLocked(now)
	for callID, signature := range signatures {
		if callID == "" || signature == "" {
			continue
		}
		key := geminiSignatureKey{scope: scope, callID: callID}
		if _, exists := c.entries[key]; !exists {
			c.order = append(c.order, key)
		}
		c.entries[key] = geminiSignatureEntry{signature: signature, expires: now.Add(geminiSignatureTTL)}
	}
	for len(c.order) > geminiSignatureLimit {
		oldest := c.order[0]
		c.order = c.order[1:]
		delete(c.entries, oldest)
	}
}

// lookup returns the signatures known for the given call ids in this
// conversation. Unknown ids are absent from the result.
func (c *geminiSignatureCache) lookup(scope string, callIDs []string, now time.Time) map[string]string {
	if c == nil || len(callIDs) == 0 {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var found map[string]string
	for _, callID := range callIDs {
		entry, ok := c.entries[geminiSignatureKey{scope: scope, callID: callID}]
		if !ok || now.After(entry.expires) {
			continue
		}
		if found == nil {
			found = map[string]string{}
		}
		found[callID] = entry.signature
	}
	return found
}

// sweepLocked drops expired entries. The full scan is throttled so a busy
// relay does not pay for it on every response.
func (c *geminiSignatureCache) sweepLocked(now time.Time) {
	if now.Sub(c.swept) < time.Minute {
		return
	}
	c.swept = now
	kept := c.order[:0]
	for _, key := range c.order {
		entry, ok := c.entries[key]
		if !ok {
			continue
		}
		if now.After(entry.expires) {
			delete(c.entries, key)
			continue
		}
		kept = append(kept, key)
	}
	c.order = kept
}

// geminiPartSignature reads the opaque signature a generateContent part
// carries. The REST API spells it camelCase; snake_case is accepted for
// compatible servers.
func geminiPartSignature(part map[string]any) string {
	if value, _ := part["thoughtSignature"].(string); value != "" {
		return value
	}
	value, _ := part["thought_signature"].(string)
	return value
}

// geminiSignaturesInResponse collects call id → signature pairs from one
// generateContent payload. A whole response body and a single SSE data frame
// share this shape, so the same walk serves both paths.
//
// callSequence is the number of function calls already seen in this response
// (zero for a whole body, the running count for a stream). The returned count
// continues the numbering, so a streamed call keeps the positional id the
// decoder gave the client and a replay can find its signature.
func geminiSignaturesInResponse(body []byte, callSequence int) (map[string]string, int) {
	var decoded struct {
		Candidates []struct {
			Content struct {
				Parts []map[string]any `json:"parts"`
			} `json:"content"`
		} `json:"candidates"`
	}
	if json.Unmarshal(body, &decoded) != nil || len(decoded.Candidates) == 0 {
		return nil, callSequence
	}
	var found map[string]string
	// Only the first candidate reaches the client; numbering any other would
	// shift the ids the decoder mints for it.
	for _, part := range decoded.Candidates[0].Content.Parts {
		call := geminiFunctionCall(part)
		if call == nil {
			continue
		}
		callSequence++
		signature := geminiPartSignature(part)
		if signature == "" {
			continue
		}
		callID, _ := call["id"].(string)
		if callID == "" {
			callID = fmt.Sprintf("call_gemini_%d", callSequence)
		}
		if found == nil {
			found = map[string]string{}
		}
		found[callID] = signature
	}
	return found, callSequence
}

// geminiInteractionSignatures collects call id → signature pairs from one
// Interactions payload (a whole interaction or a single SSE frame). A
// function_call step carries its signature on the step itself.
func geminiInteractionSignatures(body []byte) map[string]string {
	var decoded struct {
		Steps []map[string]any `json:"steps"`
	}
	if json.Unmarshal(body, &decoded) != nil {
		return nil
	}
	var found map[string]string
	for _, step := range decoded.Steps {
		if kind, _ := step["type"].(string); kind != "function_call" {
			continue
		}
		callID, _ := step["id"].(string)
		signature, _ := step["signature"].(string)
		if callID == "" || signature == "" {
			continue
		}
		if found == nil {
			found = map[string]string{}
		}
		found[callID] = signature
	}
	return found
}

// geminiPlaceholderSignature is the value the dialect encoder writes when a
// replayed call has no signature at all. It keeps the turn valid upstream but
// carries no reasoning state, so a cached signature always replaces it.
const geminiPlaceholderSignature = "skip_thought_signature_validator"

// applyCachedGeminiSignatures restores signatures the client could not carry
// into an already-converted Gemini request body (generateContent contents or
// Interactions input steps). Calls already holding a signature (a native
// Gemini client) are left untouched.
func applyCachedGeminiSignatures(body []byte, cached map[string]string) []byte {
	if len(cached) == 0 {
		return body
	}
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return body
	}
	applied := false
	if contents, ok := request["contents"].([]any); ok {
		for _, turn := range contents {
			object, _ := turn.(map[string]any)
			parts, _ := object["parts"].([]any)
			for _, entry := range parts {
				part, _ := entry.(map[string]any)
				call := geminiFunctionCall(part)
				if call == nil {
					continue
				}
				// A signature the conversation still carries is authoritative; the
				// placeholder the encoder writes when it has nothing better is not,
				// so a cached signature replaces it.
				if signature := geminiPartSignature(part); signature != "" && signature != geminiPlaceholderSignature {
					continue
				}
				callID, _ := call["id"].(string)
				signature, ok := cached[callID]
				if !ok || signature == "" {
					continue
				}
				part["thoughtSignature"] = signature
				applied = true
			}
		}
	} else if input, ok := request["input"].([]any); ok {
		for _, entry := range input {
			step, _ := entry.(map[string]any)
			if kind, _ := step["type"].(string); kind != "function_call" {
				continue
			}
			if signature, _ := step["signature"].(string); signature != "" && signature != geminiPlaceholderSignature {
				continue
			}
			callID, _ := step["id"].(string)
			signature, ok := cached[callID]
			if !ok || signature == "" {
				continue
			}
			step["signature"] = signature
			applied = true
		}
	}
	if !applied {
		return body
	}
	updated, err := json.Marshal(request)
	if err != nil {
		return body
	}
	return updated
}

// replayCallIDs lists the function call ids a converted Gemini body replays,
// so the cache is only queried for calls the client actually sent back.
func replayCallIDs(body []byte) []string {
	var request map[string]any
	if json.Unmarshal(body, &request) != nil {
		return nil
	}
	var ids []string
	if contents, ok := request["contents"].([]any); ok {
		for _, turn := range contents {
			object, _ := turn.(map[string]any)
			parts, _ := object["parts"].([]any)
			for _, entry := range parts {
				part, _ := entry.(map[string]any)
				call := geminiFunctionCall(part)
				if call == nil {
					continue
				}
				if callID, _ := call["id"].(string); callID != "" {
					ids = append(ids, callID)
				}
			}
		}
		return ids
	}
	if input, ok := request["input"].([]any); ok {
		for _, entry := range input {
			step, _ := entry.(map[string]any)
			if kind, _ := step["type"].(string); kind != "function_call" {
				continue
			}
			if callID, _ := step["id"].(string); callID != "" {
				ids = append(ids, callID)
			}
		}
	}
	return ids
}
