package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type extensionPendingKey struct{}

func (s *Server) runExtensionPendingCleanup() {
	if s == nil || s.store == nil {
		return
	}
	ticker := time.NewTicker(10 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-s.shutdownCtx.Done():
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(s.shutdownCtx, 5*time.Second)
			if err := s.store.PruneExtensionPending(ctx, time.Now()); err != nil && s.proxylog != nil {
				s.proxylog.Warnf("extension pending cleanup: %v", err)
			}
			cancel()
		}
	}
}

// Deferred-round records.
//
// A surface that cannot show a server-executed call to the caller hands over only
// the caller's own calls and keeps the rest of the round in a record: the calls
// still to run (or the result an interceptor already produced) plus the mapping
// from the public ids the caller saw back to the model's own ids. The record
// describes one round, never the conversation, so its size does not grow with the
// session.
const (
	pendingCallServer      = "server"
	pendingCallIntercepted = "intercepted"
	pendingCallClient      = "client"
)

type extensionPendingCall struct {
	ExtensionID string         `json:"extensionId,omitempty"`
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Arguments   string         `json:"arguments,omitempty"`
	Kind        string         `json:"kind"`
	Result      map[string]any `json:"result,omitempty"`
	// ArgsHash fingerprints a caller call's arguments. A surface that keys its
	// record by tool name (Gemini) can only pair a replayed turn with the record
	// by name and position, so the hash is what tells "the caller's own call"
	// apart from a same-named call of another conversation.
	ArgsHash string `json:"argsHash,omitempty"`
}

// pendingContextFingerprint hashes the conversation that led up to a round, on
// the surfaces whose record is keyed by tool name (Gemini).
//
// It hashes the *text* of every content turn before the model turn - or before
// `upto` when the fold knows where that turn sits - because the caller never sees
// the daemon's own calls: two conversations can call the same tool with the same
// arguments, and their prompts are what tells them apart. Text only, so a client
// that re-serializes (or drops) a thought signature on the way back is not
// mistaken for a different conversation; the prefix only, so a caller that appends
// a new message to the continuation still matches.
//
// It returns "" when there is no text to fingerprint, which leaves the other
// checks in charge.
func pendingContextFingerprint(endpoint string, request map[string]any, upto int) string {
	if endpoint != "gemini.generateContent" {
		return ""
	}
	contents, _ := request["contents"].([]any)
	if upto < 0 || upto > len(contents) {
		upto = len(contents)
	}
	texts := make([]string, 0, upto)
	for _, raw := range contents[:upto] {
		content, _ := raw.(map[string]any)
		for _, rawPart := range anySlice(content["parts"]) {
			part, _ := rawPart.(map[string]any)
			if text, ok := part["text"].(string); ok && text != "" {
				texts = append(texts, text)
			}
		}
	}
	if len(texts) == 0 {
		return ""
	}
	encoded, err := json.Marshal(texts)
	if err != nil {
		return ""
	}
	return fingerprintArguments(string(encoded))
}

// pendingLookupKey scopes a record key. Chat and Anthropic keys are random
// tokens, but Gemini's caller pairs results by tool name, so the key there is
// the name within the caller's identity: two identities using the same tool
// never share a record, and a stray lookup finds nothing instead of someone
// else's round.
func pendingLookupKey(endpoint, key, identityID string) string {
	if endpoint == "gemini.generateContent" {
		return key + "\x1f" + identityID
	}
	return key
}

// fingerprintArguments hashes a call's arguments in a canonical form, so the
// same input fingerprints the same however the client re-serialized it.
func fingerprintArguments(arguments string) string {
	var decoded any
	if json.Unmarshal([]byte(arguments), &decoded) != nil {
		return ""
	}
	canonical, err := json.Marshal(decoded)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:])
}

type extensionPendingPayload struct {
	Calls     []extensionPendingCall `json:"calls"`
	ClientIDs map[string]string      `json:"clientIds"`
	// ContextHash fingerprints the content turn that prompted the round. Identical
	// caller arguments in two conversations are common (an empty object, a fixed
	// path), so the prompt is what distinguishes "this round's conversation" from
	// another one that happens to call the same tool with the same arguments.
	ContextHash string `json:"contextHash,omitempty"`
}

// extensionPendingContext is the loaded record plus its decoded payload, carried
// on the request by the continuation middleware.
type extensionPendingContext struct {
	record  store.ExtensionPending
	payload extensionPendingPayload
}

func newExtensionCallID() (string, error) {
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return "call_ls_" + hex.EncodeToString(random[:]), nil
}

func CreateExtensionContinuationMiddleware(s *Server, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if extensionEndpoint(r.URL.Path) == "" || s.store == nil {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1))
			if err != nil || len(body) > swaputil.MaxRequestBodySize {
				swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, "extension continuation body is too large")
				return
			}
			setExtensionBody(r, body)
			endpoint := extensionEndpoint(r.URL.Path)
			if !bytes.Contains(body, []byte("call_ls_")) && endpoint != "gemini.generateContent" {
				next.ServeHTTP(w, r)
				return
			}
			var request map[string]any
			if err := json.Unmarshal(body, &request); err != nil {
				next.ServeHTTP(w, r)
				return
			}
			ids := incomingExtensionResults(request, extensionEndpoint(r.URL.Path))
			if len(ids) == 0 {
				next.ServeHTTP(w, r)
				return
			}
			identityID := identityFromContext(r.Context()).ID
			// The record is keyed the way this request addresses it (a public id, or
			// a tool name scoped to the caller's identity on Gemini), so the
			// presence check below compares like with like.
			present := make(map[string]bool, len(ids))
			for id := range ids {
				present[pendingLookupKey(endpoint, id, identityID)] = true
			}
			var record store.ExtensionPending
			for id := range ids {
				if !strings.HasPrefix(id, "call_ls_") && endpoint != "gemini.generateContent" {
					continue
				}
				found, lookupErr := s.store.LookupExtensionPending(r.Context(), pendingLookupKey(endpoint, id, identityID))
				if lookupErr != nil {
					// A client that resends its whole conversation includes the
					// results of calls that already ran; their records were
					// consumed when they did. Those are history, not a
					// continuation this request depends on, and demanding a
					// record for them rejected a perfectly resumable
					// conversation.
					if errors.Is(lookupErr, store.ErrExtensionPendingNotFound) && requestCarriesCall(request, extensionEndpoint(r.URL.Path), id) {
						continue
					}
					// Nothing to resume from and nothing to reconstruct the round
					// with: the caller has to send the conversation (the same
					// answer Anthropic's own API gives a continuation that is
					// missing part of its history, as a 400).
					swaputil.SendResponse(w, r, http.StatusBadRequest, needsReplayResponse)
					return
				}
				if record.ID != "" && found.ID != record.ID {
					swaputil.SendResponse(w, r, http.StatusBadRequest, "multiple pending extension turns in one request")
					return
				}
				record = found
			}
			if record.ID == "" {
				next.ServeHTTP(w, r)
				return
			}
			if record.IdentityID != identityFromContext(r.Context()).ID {
				swaputil.SendResponse(w, r, http.StatusForbidden, "extension continuation identity mismatch")
				return
			}
			if record.Profile != s.ActiveProfile() {
				swaputil.SendResponse(w, r, http.StatusConflict, "extension continuation profile changed")
				return
			}
			if record.Endpoint != extensionEndpoint(r.URL.Path) {
				swaputil.SendResponse(w, r, http.StatusConflict, "extension continuation endpoint mismatch")
				return
			}
			for _, id := range record.CallIDs {
				if !present[id] {
					swaputil.SendResponse(w, r, http.StatusBadRequest, "extension continuation is missing a client tool result: send every tool result of the round")
					return
				}
			}
			var payload extensionPendingPayload
			if err := json.Unmarshal(record.Payload, &payload); err != nil {
				swaputil.SendResponse(w, r, http.StatusInternalServerError, "extension continuation record is invalid")
				return
			}
			ctx := context.WithValue(r.Context(), extensionPendingKey{}, extensionPendingContext{record: record, payload: payload})
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// requestCarriesCall reports whether the request also carries the assistant
// call that a tool result answers.
//
// A bare continuation sends only the results - llama-swap splices them into the
// stored base request, so it needs the pending record. A resent conversation
// carries both sides of every earlier call, which makes a missing record for
// such an id harmless: the call and its result are already in the request.
func requestCarriesCall(request map[string]any, endpoint, callID string) bool {
	if callID == "" {
		return false
	}
	switch endpoint {
	case "gemini.generateContent":
		contents, _ := request["contents"].([]any)
		for _, raw := range contents {
			content, _ := raw.(map[string]any)
			if role, _ := content["role"].(string); role != "model" {
				continue
			}
			for _, rawPart := range anySlice(content["parts"]) {
				part, _ := rawPart.(map[string]any)
				call := part["functionCall"]
				if call == nil {
					call = part["function_call"]
				}
				entry, _ := call.(map[string]any)
				if entry == nil {
					continue
				}
				if id, _ := entry["id"].(string); id == callID {
					return true
				}
				if name, _ := entry["name"].(string); name == callID {
					return true
				}
			}
		}
	case "responses":
		items, _ := request["input"].([]any)
		for _, raw := range items {
			item, _ := raw.(map[string]any)
			switch item["type"] {
			case "function_call", "custom_tool_call":
				if id, _ := item["call_id"].(string); id == callID {
					return true
				}
			}
		}
	case "anthropic.messages":
		messages, _ := request["messages"].([]any)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if message["role"] != "assistant" {
				continue
			}
			blocks, _ := message["content"].([]any)
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				if block["type"] != "tool_use" {
					continue
				}
				if id, _ := block["id"].(string); id == callID {
					return true
				}
			}
		}
	default:
		messages, _ := request["messages"].([]any)
		for _, raw := range messages {
			message, _ := raw.(map[string]any)
			if message["role"] != "assistant" {
				continue
			}
			calls, _ := message["tool_calls"].([]any)
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				if id, _ := call["id"].(string); id == callID {
					return true
				}
			}
		}
	}
	return false
}

func incomingExtensionResults(request map[string]any, endpoint string) map[string]any {
	result := map[string]any{}
	key := "messages"
	if endpoint == "responses" {
		key = "input"
	}
	if endpoint == "gemini.generateContent" {
		key = "contents"
	}
	items, _ := request[key].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if endpoint == "responses" {
			if item["type"] != "function_call_output" {
				continue
			}
			id, _ := item["call_id"].(string)
			if id != "" {
				result[id] = item["output"]
			}
		} else if endpoint == "anthropic.messages" {
			if item["role"] != "user" {
				continue
			}
			blocks, _ := item["content"].([]any)
			for _, rawBlock := range blocks {
				block, _ := rawBlock.(map[string]any)
				if block["type"] != "tool_result" {
					continue
				}
				id, _ := block["tool_use_id"].(string)
				if id != "" {
					result[id] = block["content"]
				}
			}
			continue
		}
		if endpoint == "gemini.generateContent" {
			for _, raw := range anySlice(item["parts"]) {
				part, _ := raw.(map[string]any)
				response := part["functionResponse"]
				if response == nil {
					response = part["function_response"]
				}
				entry, _ := response.(map[string]any)
				if entry == nil {
					continue
				}
				// Gemini pairs by name classically and by id in newer models; the
				// record is keyed by the public id we handed out, so an id wins and
				// the name is carried for the lookup fallback.
				key, _ := entry["id"].(string)
				if key == "" {
					key, _ = entry["name"].(string)
				}
				if key != "" {
					result[key] = entry["response"]
				}
			}
			continue
		}
		{
			if item["role"] != "tool" {
				continue
			}
			id, _ := item["tool_call_id"].(string)
			if id != "" {
				result[id] = item["content"]
			}
		}
	}
	return result
}

// errExtensionContinuationTooLarge reports a mixed round whose record is too
// large to persist. The record holds one round, so this is a pathological turn
// (an intercepted call carrying megabytes), not a long conversation: the caller's
// calls are degraded instead of failing the turn.
var errExtensionContinuationTooLarge = errors.New("extension continuation record is too large to store")

// errContinuationMismatch reports a continuation whose replayed assistant turn
// does not match the recorded round: on a surface that keys by tool name the
// caller's own call IS the key, so a same-named call of another conversation
// must be refused rather than folded into this one.
var errContinuationMismatch = errors.New("extension continuation does not match the recorded round")

// errContinuationNeedsReplay reports a continuation whose request does not carry
// the assistant turn the record belongs to. A surface that hides server calls has
// no way to rebuild the conversation from results alone, so the caller has to
// replay it - the same contract Anthropic's own Messages API has.
var errContinuationNeedsReplay = errors.New("extension continuation requires the conversation to be replayed")

// needsReplayResponse is what the caller is told when it cannot continue a round.
const needsReplayResponse = "extension continuation requires the conversation to be replayed: send the assistant turn with its tool calls, not only the tool results"

// mismatchResponse is what the caller is told when its replayed turn is not the
// recorded round: on a name-keyed surface the pending round for that tool either
// belongs to another conversation or has already been replaced by a newer turn,
// and neither can be resumed from this request.
const mismatchResponse = "extension continuation does not match the pending round for this tool: it belongs to a different turn, or a newer turn replaced it; resend this turn as a new request"

// ReplayNeeded reports whether an error means the caller must replay the
// conversation to continue. Exported for the middleware message.
func replayNeeded(err error) bool { return errors.Is(err, errContinuationNeedsReplay) }

// continuationMismatch reports a replayed turn that belongs to another round.
func continuationMismatch(err error) bool { return errors.Is(err, errContinuationMismatch) }

// deferredTurn locates where the recorded round sits in the replayed request.
// replayed is the Chat/Anthropic shape (public id -> the caller's own call);
// geminiCalls is the name-paired shape, aligned with the record's calls and
// holding the caller's functionCall objects.
type deferredTurn struct {
	index       int
	replayed    map[string]map[string]any
	geminiCalls []map[string]any
}

// deferredTurnIn finds the assistant turn that carries the public ids of the
// record. The newest matching turn wins: an older one is history.
func deferredTurnIn(request map[string]any, payload extensionPendingPayload, endpoint string) (deferredTurn, error) {
	if endpoint == "gemini.generateContent" {
		return geminiDeferredTurnIn(request, payload)
	}
	messages, _ := request["messages"].([]any)
	for index := len(messages) - 1; index >= 0; index-- {
		message, _ := messages[index].(map[string]any)
		if message["role"] != "assistant" {
			continue
		}
		replayed := map[string]map[string]any{}
		for _, raw := range anySlice(message["tool_calls"]) {
			call, _ := raw.(map[string]any)
			id, _ := call["id"].(string)
			if _, ok := payload.ClientIDs[id]; ok {
				replayed[id] = call
			}
		}
		if len(replayed) == len(payload.ClientIDs) {
			return deferredTurn{index: index, replayed: replayed}, nil
		}
	}
	return deferredTurn{}, errContinuationNeedsReplay
}

// anySlice reads a JSON array without panicking on a missing or mistyped value.
func anySlice(value any) []any {
	items, _ := value.([]any)
	return items
}

// executeDeferredCalls runs what the recorded round left to do: the extension's
// own server calls run now (once, not when the round was handed over), and an
// intercepted call reuses the result its interceptor already produced.
func (s *Server) executeDeferredCalls(ctx context.Context, state *extensionRequestState, payload extensionPendingPayload, endpoint string) (map[string]map[string]any, error) {
	outputs := map[string]map[string]any{}
	for _, entry := range payload.Calls {
		switch entry.Kind {
		case pendingCallClient:
			continue
		case pendingCallIntercepted:
			if entry.Result != nil {
				outputs[entry.ID] = entry.Result
				continue
			}
		}
		item := extensionByID(state, entry.ExtensionID)
		if item == nil {
			failure := fmt.Errorf("extension %s is no longer available", entry.ExtensionID)
			state.markToolFailed(entry.Name, failure)
			outputs[entry.ID] = toolFailureResult(entry.Name, failure)
			continue
		}
		var arguments any
		if strings.TrimSpace(entry.Arguments) == "" {
			arguments = map[string]any{}
		} else if err := json.Unmarshal([]byte(entry.Arguments), &arguments); err != nil {
			// The model's mistake is reported to it as the tool result, exactly as
			// the in-request path does, instead of running the tool with no input.
			failure := fmt.Errorf("arguments are not valid JSON: %w", err)
			if item.Manifest.FailOnToolError {
				return nil, failure
			}
			state.markToolFailed(entry.Name, failure)
			outputs[entry.ID] = toolFailureResult(entry.Name, failure)
			continue
		}
		input := map[string]any{"id": entry.ID, "name": entry.Name, "arguments": arguments}
		output, err := extensionMapHook(ctx, item, "onToolCall", state.meta, input)
		if err != nil {
			// The tool failed, not the request: the model is told and answers
			// without it, unless the extension asked for failures to be fatal.
			if item.Manifest.FailOnToolError {
				return nil, err
			}
			state.markToolFailed(entry.Name, err)
			output = toolFailureResult(entry.Name, err)
		}
		if _, ok := output["content"]; !ok {
			state.markToolFailed(entry.Name, errors.New("the extension returned no tool result"))
		}
		output, err = applyToolResultHooks(ctx, state, output)
		if err != nil {
			return nil, err
		}
		outputs[entry.ID] = output
	}
	return outputs, nil
}

// foldDeferredRound completes a recorded round: it runs what the round left to
// do and splices the result into the conversation the caller replayed.
func (s *Server) foldDeferredRound(ctx context.Context, state *extensionRequestState, request map[string]any, pending extensionPendingContext, endpoint string) error {
	turn, err := deferredTurnIn(request, pending.payload, endpoint)
	if err != nil {
		return err
	}
	outputs, err := s.executeDeferredCalls(ctx, state, pending.payload, endpoint)
	if err != nil {
		return err
	}
	if endpoint == "gemini.generateContent" {
		return foldGeminiDeferredTurn(request, pending.payload, turn, outputs)
	}
	return foldChatDeferredTurn(request, pending.payload, turn, outputs)
}

// extensionByID finds the matched extension a recorded call belongs to.
func extensionByID(state *extensionRequestState, id string) *extensions.Compiled {
	if state == nil || id == "" {
		return nil
	}
	for _, item := range state.items {
		if item != nil && item.Manifest.ID == id {
			return item
		}
	}
	return nil
}

// foldChatDeferredTurn rebuilds the replayed assistant turn from the record and
// appends the results of the deferred calls next to the caller's own results.
func foldChatDeferredTurn(request map[string]any, payload extensionPendingPayload, turn deferredTurn, outputs map[string]map[string]any) error {
	messages, _ := request["messages"].([]any)
	if turn.index < 0 || turn.index >= len(messages) {
		return errContinuationNeedsReplay
	}
	message, _ := messages[turn.index].(map[string]any)
	rebuilt := make([]any, 0, len(payload.Calls))
	for _, entry := range payload.Calls {
		if entry.Kind == pendingCallClient {
			call, ok := turn.replayed[entry.ID]
			if !ok {
				return fmt.Errorf("%w: the caller's call %s is missing", errContinuationNeedsReplay, entry.ID)
			}
			if original := payload.ClientIDs[entry.ID]; original != "" {
				call["id"] = original
			}
			rebuilt = append(rebuilt, call)
			continue
		}
		rebuilt = append(rebuilt, map[string]any{"id": entry.ID, "type": "function",
			"function": map[string]any{"name": entry.Name, "arguments": entry.Arguments}})
	}
	message["tool_calls"] = rebuilt
	// The caller's own results reference the public ids we handed out; the model's
	// own ids are what the upstream conversation must show.
	for _, raw := range messages {
		other, _ := raw.(map[string]any)
		if other == nil {
			continue
		}
		id, _ := other["tool_call_id"].(string)
		if original := payload.ClientIDs[id]; original != "" {
			other["tool_call_id"] = original
		}
	}

	results := make([]any, 0, len(payload.Calls))
	for _, entry := range payload.Calls {
		if entry.Kind == pendingCallClient {
			continue
		}
		output, ok := outputs[entry.ID]
		if !ok {
			continue
		}
		results = append(results, toolResultFor("chat.completions", extensionCall{ID: entry.ID, Name: entry.Name}, output))
	}
	if len(results) == 0 {
		return nil
	}
	insertAt := turn.index + 1
	for insertAt < len(messages) {
		next, _ := messages[insertAt].(map[string]any)
		if next["role"] != "tool" {
			break
		}
		insertAt++
	}
	tail := append(append(make([]any, 0, len(results)+len(messages)-insertAt), results...), messages[insertAt:]...)
	request["messages"] = append(messages[:insertAt], tail...)
	return nil
}

// geminiDeferredTurnIn finds the model turn that carries the round's calls and
// pairs them with the record by name and occurrence, verifying each caller call's
// argument fingerprint. Gemini's parts carry no id, so name+position is all a
// client can offer; the fingerprint is what stops a same-named call from another
// conversation being folded into this round.
func geminiDeferredTurnIn(request map[string]any, payload extensionPendingPayload) (deferredTurn, error) {
	contents, _ := request["contents"].([]any)
	if len(contents) == 0 {
		return deferredTurn{}, errContinuationNeedsReplay
	}
	sawMismatch := false
	wanted := map[string]int{}
	for _, entry := range payload.Calls {
		if entry.Kind == pendingCallClient {
			wanted[entry.Name]++
		}
	}
	if len(wanted) == 0 {
		return deferredTurn{}, errContinuationNeedsReplay
	}
	for index := len(contents) - 1; index >= 0; index-- {
		content, _ := contents[index].(map[string]any)
		if role, _ := content["role"].(string); role != "model" {
			continue
		}
		calls := map[string][]map[string]any{}
		for _, rawPart := range anySlice(content["parts"]) {
			part, _ := rawPart.(map[string]any)
			entry := geminiFunctionCall(part)
			if entry == nil {
				continue
			}
			name, _ := entry["name"].(string)
			calls[name] = append(calls[name], entry)
		}
		turn := deferredTurn{index: index, geminiCalls: make([]map[string]any, len(payload.Calls))}
		occurrence := map[string]int{}
		complete := true
		argsMatch := true
		for i, entry := range payload.Calls {
			if entry.Kind != pendingCallClient {
				continue
			}
			position := occurrence[entry.Name]
			occurrence[entry.Name] = position + 1
			matches := calls[entry.Name]
			if position >= len(matches) {
				complete = false
				break
			}
			call := matches[position]
			if entry.ArgsHash != "" {
				arguments, _ := json.Marshal(call["args"])
				if fingerprintArguments(string(arguments)) != entry.ArgsHash {
					argsMatch = false
					break
				}
			}
			turn.geminiCalls[i] = call
		}
		if !complete {
			continue
		}
		if !argsMatch {
			// Arguments disagree with the record: this turn is not the round's, but
			// an older model turn might be.
			sawMismatch = true
			continue
		}
		// Same tool, same arguments, different conversation: the conversation that
		// led here is what tells them apart, because the caller never sees the
		// daemon's own calls.
		if payload.ContextHash != "" {
			if prefix := pendingContextFingerprint("gemini.generateContent", request, index); prefix != "" && prefix != payload.ContextHash {
				sawMismatch = true
				continue
			}
		}
		return turn, nil
	}
	if sawMismatch {
		return deferredTurn{}, errContinuationMismatch
	}
	return deferredTurn{}, errContinuationNeedsReplay
}

// geminiFunctionCall reads a functionCall part, accepting both spellings.
func geminiFunctionCall(part map[string]any) map[string]any {
	if part == nil {
		return nil
	}
	call := part["functionCall"]
	if call == nil {
		call = part["function_call"]
	}
	entry, _ := call.(map[string]any)
	return entry
}

// geminiTurnCarriesResults reports whether a content turn holds functionResponse
// parts.
func geminiTurnCarriesResults(content map[string]any) bool {
	for _, rawPart := range anySlice(content["parts"]) {
		part, _ := rawPart.(map[string]any)
		if part == nil {
			continue
		}
		if part["functionResponse"] != nil || part["function_response"] != nil {
			return true
		}
	}
	return false
}

// foldGeminiDeferredTurn rebuilds the replayed model turn and puts the deferred
// calls' functionResponse parts next to the caller's own.
//
// Gemini pairs a result with its call by name (the id is optional bookkeeping),
// so the parts of one turn are not order-significant; they are kept together in
// the turn the caller used for its own results.
func foldGeminiDeferredTurn(request map[string]any, payload extensionPendingPayload, turn deferredTurn, outputs map[string]map[string]any) error {
	contents, _ := request["contents"].([]any)
	if turn.index < 0 || turn.index >= len(contents) {
		return errContinuationNeedsReplay
	}
	content, _ := contents[turn.index].(map[string]any)
	parts := make([]any, 0, len(payload.Calls))
	for i, entry := range payload.Calls {
		if entry.Kind == pendingCallClient {
			call := turn.geminiCalls[i]
			if call == nil {
				return fmt.Errorf("%w: the caller's call %s is missing", errContinuationNeedsReplay, entry.Name)
			}
			parts = append(parts, map[string]any{"functionCall": call})
			continue
		}
		var arguments any
		if json.Unmarshal([]byte(entry.Arguments), &arguments) != nil {
			arguments = map[string]any{}
		}
		parts = append(parts, map[string]any{"functionCall": map[string]any{"name": entry.Name, "args": arguments}})
	}
	content["parts"] = parts

	responses := make([]any, 0, len(payload.Calls))
	for _, entry := range payload.Calls {
		if entry.Kind == pendingCallClient {
			continue
		}
		output, ok := outputs[entry.ID]
		if !ok {
			continue
		}
		rendered := toolResultFor("gemini.generateContent", extensionCall{ID: entry.ID, Name: entry.Name}, output)
		// Gemini wraps a tool result in a functionResponse part; a bare part is
		// dropped by the dialect layer as unknown.
		responses = append(responses, map[string]any{"functionResponse": map[string]any{
			"name": rendered["name"], "response": rendered["response"],
		}})
	}
	if len(responses) == 0 {
		return nil
	}
	// Our parts join the caller's own results when that turn follows immediately -
	// Gemini pairs by name, so the parts of one turn are not order-significant. A
	// later user turn (a new message the caller appended) is left alone: the
	// results get a turn of their own right after the model turn instead.
	if turn.index+1 < len(contents) {
		if target, ok := contents[turn.index+1].(map[string]any); ok && geminiTurnCarriesResults(target) {
			existing, _ := target["parts"].([]any)
			merged := make([]any, 0, len(existing)+len(responses))
			merged = append(merged, responses...)
			merged = append(merged, existing...)
			target["parts"] = merged
			return nil
		}
	}
	insert := map[string]any{"role": "user", "parts": responses}
	at := turn.index + 1
	tail := append(append(make([]any, 0, len(contents)-at+1), insert), contents[at:]...)
	request["contents"] = append(contents[:at], tail...)
	return nil
}

// demoteUncontinuableClientCalls turns caller-run tool calls that cannot be
// handed over into failed server calls for this round.
//
// The model receives the failure as the tool result and can answer without the
// tool, and markToolFailed keeps the name out of the extension's next tool list.
// This is the same shape as a tool that failed to run: the caller never sees a
// call it has no way to continue (a call_ls_ id it cannot resolve), and the
// request does not die on a capacity limit.
func demoteUncontinuableClientCalls(state *extensionRequestState, calls []extensionCall, cause error) []extensionCall {
	demoted := make([]extensionCall, 0, len(calls))
	for _, call := range calls {
		call.Precomputed = toolFailureResult(call.Name, cause)
		if call.Owner == nil && state != nil && len(state.items) > 0 {
			call.Owner = state.items[0]
		}
		state.markToolFailed(call.Name, cause)
		demoted = append(demoted, call)
	}
	return demoted
}

// extensionCallNames lists the tool names a call set references, for the
// response filters that must drop calls the caller cannot execute.
func extensionCallNames(calls []extensionCall) map[string]bool {
	if len(calls) == 0 {
		return nil
	}
	names := make(map[string]bool, len(calls))
	for _, call := range calls {
		names[call.Name] = true
	}
	return names
}

// formatContinuationSize renders a payload size for the log line and the tool
// result the model receives.
func formatContinuationSize(bytes int) string {
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1<<20))
}

// mergeClassifiedCalls annotates the model's ordered call list with what
// classification worked out for each call (its owning extension, an interceptor's
// result), so a record keeps both the round's order and what each call needs.
func mergeClassifiedCalls(calls, classified []extensionCall) []extensionCall {
	byID := make(map[string]extensionCall, len(classified))
	for _, call := range classified {
		byID[call.ID] = call
	}
	merged := make([]extensionCall, len(calls))
	for i, call := range calls {
		if enriched, ok := byID[call.ID]; ok {
			merged[i] = enriched
			continue
		}
		merged[i] = call
	}
	return merged
}

// saveExtensionMixed records one round for a surface that hides server calls.
//
// Nothing is executed here. The round's calls are written down in the model's own
// order - the caller's calls under the public ids it will reference, the
// extension's own server calls with the extension that owes them, an intercepted
// call with the result its interceptor already produced - and the record is what
// the caller's next request resumes from (see foldDeferredRound). Deferring the
// extension's own calls is what keeps a turn the caller never continues from
// running server work for nothing.
func (s *Server) saveExtensionMixed(ctx context.Context, state *extensionRequestState, calls []extensionCall, clientCalls []extensionCall, endpoint string) ([]extensionCall, error) {
	if s.store == nil {
		return nil, errors.New("extension continuation store is unavailable")
	}
	identityID := identityFromContext(ctx).ID
	mapping := map[string]string{}
	ids := make([]string, 0, len(clientCalls))
	seenKey := map[string]bool{}
	publicCalls := make([]extensionCall, len(clientCalls))
	publicByOriginal := make(map[string]string, len(clientCalls))
	for i, call := range clientCalls {
		// Gemini's function parts carry no call id, so the caller pairs a result
		// with its call by name; the name is scoped to the caller's identity and
		// deduplicated, because two calls of the same tool in one round are
		// ordinary model output and would otherwise collide on the record's key.
		publicID := call.Name
		if endpoint != "gemini.generateContent" {
			minted, err := newExtensionCallID()
			if err != nil {
				return nil, err
			}
			publicID = minted
		}
		publicID = pendingLookupKey(endpoint, publicID, identityID)
		if !seenKey[publicID] {
			seenKey[publicID] = true
			ids = append(ids, publicID)
		}
		mapping[publicID] = call.ID
		publicByOriginal[call.ID] = publicID
		publicCalls[i] = call
		publicCalls[i].OriginalID = call.ID
		publicCalls[i].ID = publicID
	}
	payload := extensionPendingPayload{Calls: make([]extensionPendingCall, 0, len(calls)), ClientIDs: mapping, ContextHash: state.promptFingerprint}
	for _, call := range calls {
		entry := extensionPendingCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments, Kind: pendingCallServer}
		if call.Owner != nil {
			entry.ExtensionID = call.Owner.Manifest.ID
		}
		switch {
		case publicByOriginal[call.ID] != "":
			entry.ID = publicByOriginal[call.ID]
			entry.Kind = pendingCallClient
			// The caller's own arguments are not stored: its replay carries them.
			// The fingerprint is what a name-keyed surface compares against.
			if endpoint == "gemini.generateContent" {
				entry.ArgsHash = fingerprintArguments(call.Arguments)
			}
			entry.Arguments = ""
			entry.ExtensionID = ""
		case call.Precomputed != nil:
			entry.Kind = pendingCallIntercepted
			entry.Result = call.Precomputed
		}
		payload.Calls = append(payload.Calls, entry)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	if limit := state.continuationLimit; limit > 0 && len(encoded) > limit {
		return nil, fmt.Errorf("%w: the round is %s, over the %s limit",
			errExtensionContinuationTooLarge, formatContinuationSize(len(encoded)), formatContinuationSize(limit))
	}
	// A name-keyed surface can hand a round the key an unconsumed round still owns
	// (same identity, same tool). The newest round wins - the store replaces it -
	// but the operator should see why an earlier conversation stopped being
	// resumable, because that conversation can no longer finish its turn.
	if endpoint == "gemini.generateContent" {
		for _, id := range ids {
			if superseded, lookupErr := s.store.LookupExtensionPending(ctx, id); lookupErr == nil && superseded.ID != "" {
				slog.Warn("extension continuation round superseded by a newer round for the same tool",
					"tool", id, "superseded_round", superseded.ID, "model", state.meta.ResolvedModel, "request_id", state.meta.RequestID)
				break
			}
		}
	}
	token, err := newExtensionCallID()
	if err != nil {
		return nil, err
	}
	record := store.ExtensionPending{ID: token, IdentityID: identityFromContext(ctx).ID, ModelID: state.meta.ResolvedModel, Profile: state.meta.Profile, Endpoint: endpoint, Payload: encoded, CallIDs: ids, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.store.SaveExtensionPending(ctx, record); err != nil {
		return nil, err
	}
	return publicCalls, nil
}

// exposeExtensionClientCalls hands the caller-visible calls of a hidden surface
// back to the caller: Gemini as functionCall parts (paired by name), Chat
// Completions as tool_calls under the public ids the record is keyed by. The
// visible surfaces never reach it - they report their server calls as artifacts
// instead (see extensions_artifacts.go).
func exposeExtensionClientCalls(response map[string]any, clientCalls []extensionCall, endpoint string) {
	if endpoint == "gemini.generateContent" {
		candidates, _ := response["candidates"].([]any)
		if len(candidates) == 0 {
			return
		}
		candidate, _ := candidates[0].(map[string]any)
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		byOriginal := map[string]bool{}
		for _, call := range clientCalls {
			byOriginal[call.OriginalID] = true
		}
		visible := make([]any, 0, len(parts))
		for _, raw := range parts {
			part, ok := raw.(map[string]any)
			call := part["functionCall"]
			if call == nil {
				call = part["function_call"]
			}
			entry, _ := call.(map[string]any)
			if !ok || entry == nil {
				visible = append(visible, raw)
				continue
			}
			name, _ := entry["name"].(string)
			keep := false
			for _, candidateCall := range clientCalls {
				if candidateCall.OriginalID != "" && (candidateCall.Name == name || byOriginal[name]) {
					keep = true
					break
				}
			}
			if keep {
				visible = append(visible, map[string]any{"functionCall": map[string]any{"name": name, "args": entry["args"]}})
			}
		}
		content["parts"] = visible
		candidate["finishReason"] = "STOP"
		return
	}
	if endpoint == "chat.completions" {
		choices, _ := response["choices"].([]any)
		if len(choices) == 0 {
			return
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		items := make([]any, 0, len(clientCalls))
		for _, call := range clientCalls {
			items = append(items, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
		}
		message["tool_calls"] = items
		choice["finish_reason"] = "tool_calls"
		return
	}
	output, _ := response["output"].([]any)
	visible := make([]any, 0, len(output))
	for _, raw := range output {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call" {
			visible = append(visible, raw)
			continue
		}
		for _, call := range clientCalls {
			if item["call_id"] == call.OriginalID {
				item["call_id"] = call.ID
				visible = append(visible, item)
				break
			}
		}
	}
	response["output"] = visible
}
