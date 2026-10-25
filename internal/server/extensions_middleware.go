package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	protocolrelaykit "github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
	"strings"
)

type extensionContextKey struct{}
type extensionOriginKey struct{}

type extensionOrigin struct {
	requestedModel string
	profile        string
}

func CreateExtensionOriginMiddleware(s *Server) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if extensionEndpoint(r.URL.Path) == "" {
				next.ServeHTTP(w, r)
				return
			}
			requestID := r.Header.Get("X-Request-ID")
			if !validExtensionRequestID(requestID) {
				var token [16]byte
				if _, err := rand.Read(token[:]); err == nil {
					requestID = hex.EncodeToString(token[:])
					r.Header.Set("X-Request-ID", requestID)
				}
			}
			if requestID != "" {
				w.Header().Set("X-Request-ID", requestID)
			}
			model, _ := swaputil.ExtractModel(r)
			origin := extensionOrigin{requestedModel: model, profile: s.ActiveProfile()}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), extensionOriginKey{}, origin)))
		})
	}
}

func validExtensionRequestID(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, ch := range value {
		if ch != '-' && ch != '_' && ch != '.' && (ch < '0' || ch > '9') && (ch < 'a' || ch > 'z') && (ch < 'A' || ch > 'Z') {
			return false
		}
	}
	return true
}

type extensionRequestState struct {
	toolChoice        *dialect.ToolChoice
	parallelTools     *bool
	unknownToolChoice bool
	items             []*extensions.Compiled
	meta              extensions.Context
	owners            map[string]*extensions.Compiled
	// hostedToolsBefore is the provider-hosted tool count captured before the
	// request hooks ran, so a hook-injected hosted tool can be told apart from
	// a client-sent one when the protocol adapter decides what to strip.
	hostedToolsBefore int
	// failedTools names the tools that could not run during this request (an
	// upstream quota, a broken handler). They are dropped from the tool list in
	// the following rounds: a model that keeps re-calling a broken tool burns
	// every round and ends with no answer, which is exactly what an operator
	// sees as "the chat hangs".
	failedTools map[string]bool
	// withholdTools marks the final round: the tool rounds are spent, so the
	// extensions' server tools are withheld and the model has to answer with
	// what it already has. Without this the turn ends on the model's last tool
	// call - a call the client can neither see nor execute - and the user sees
	// an empty reply. withheld is the concrete set for the current round.
	withholdTools bool
	withheld      map[string]bool
	// injectedServerTools records the server tools the extensions actually put
	// into the request, which is what makes withheld ownership-based.
	injectedServerTools map[string]bool
	// continuationLimit is the largest base request that may be persisted so a
	// round mixing server and caller-run tools can be resumed. Past it the
	// caller's tool calls are degraded instead of failing the turn.
	continuationLimit int
	// serverOutputs records what each server-side call of this request returned,
	// keyed by call id. It is what lets a visible surface render the result
	// artifact next to the call without re-running the tool.
	serverOutputs map[string]map[string]any
	// visibleTurn collects the artifacts of rounds that continued inside this
	// request. They are prepended to the answer so the caller's transcript
	// carries the whole turn (see applyVisibleTurn). visibleItems is the
	// Responses form of the same collection, merged into the final
	// response.completed frame.
	visibleTurn  []any
	visibleItems []visibleArtifact
	// promptFingerprint is the conversation fingerprint of the request as the
	// caller sent it (before any request hook could rewrite the prompt).
	promptFingerprint string
	// suppressAffinity records a caller's store:false: the visible Responses turn
	// must not be re-persisted for previous_response_id then.
	suppressAffinity   bool
	streamID           string
	streamModel        string
	streamCreated      any
	streamCreatedFlag  bool
	streamText         string
	streamUsage        map[string]any
	streamOutputOffset int
}

func extensionEndpoint(path string) string {
	if route, ok := protocolrelaykit.RouteForRequest(http.MethodPost, path); ok {
		switch route.Format {
		case protocolrelaykit.Gemini:
			return "gemini.generateContent"
		case protocolrelaykit.GeminiInteractions:
			// The extension loop has no Steps-based branch (call collection,
			// continuation and streaming all assume the four original
			// dialects), and the Interactions tool shape differs from the Chat
			// one, so an extension declaring this endpoint would be advertised
			// tools it can never execute. No endpoint means no match, which is
			// the honest state until the loop supports it.
			return ""
		}
	}
	switch path {
	case "/v1/chat/completions", "/v/chat/completions":
		return "chat.completions"
	case "/v1/responses", "/v/responses":
		return "responses"
	case "/v1/messages", "/v/messages":
		return "anthropic.messages"
	default:
		return ""
	}
}

func CreateExtensionsMiddleware(s *Server, cfg config.Config, manager *extensions.Manager) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			endpoint := extensionEndpoint(r.URL.Path)
			if (manager == nil && !cfg.ProtocolRuntime.MCP.Enabled) || endpoint == "" || r.Method != http.MethodPost {
				next.ServeHTTP(w, r)
				return
			}
			// A forward's inner request skips extensions entirely so a hook
			// cannot re-enter the chain that produced it.
			if _, skip := r.Context().Value(extensionSkipKey{}).(bool); skip {
				next.ServeHTTP(w, r)
				return
			}
			data, ok := swaputil.ReadContext(r.Context())
			if !ok || data.ModelID == "" {
				next.ServeHTTP(w, r)
				return
			}
			// Every hook on this request can reach the host-call channel; the
			// executor keeps the request so late capabilities (forwarding) can
			// inherit the caller's identity.
			r = withExtensionHost(s, r)
			provider := "legacy"
			if mc, found := cfg.Models[data.ModelID]; found && mc.Backend.Type != "" {
				provider = mc.Backend.Type
			}
			if _, found := cfg.Models[data.ModelID]; !found {
				provider = "peer"
			}
			origin, _ := r.Context().Value(extensionOriginKey{}).(extensionOrigin)
			if origin.requestedModel == "" {
				origin.requestedModel = data.Model
			}
			meta := extensions.Context{RequestID: r.Header.Get("X-Request-ID"), RequestedModel: origin.requestedModel, ResolvedModel: data.ModelID, Profile: origin.profile, Provider: provider, Endpoint: endpoint, Stream: data.Streaming}
			identity := identityFromContext(r.Context())
			meta.Session = extensionSession(r, identity)
			meta.Locale = requestLocale(r)
			meta.Models = extensionModelSnapshot(cfg, identity)
			var matched []*extensions.Compiled
			if manager != nil && cfg.Extensions.Enabled {
				matched = manager.Match(meta)
			}
			mcpItems, mcpErr := s.mcpExtensions(r.Context(), cfg, meta)
			if mcpErr != nil {
				swaputil.SendResponse(w, r, http.StatusBadGateway, mcpErr.Error())
				return
			}
			matched = append(matched, mcpItems...)
			state := &extensionRequestState{items: matched, meta: meta, owners: map[string]*extensions.Compiled{}, continuationLimit: cfg.Extensions.EffectiveMaxContinuationBytes()}
			// A debug chat request runs exactly one extension regardless of its
			// enabled state or match rules, so the editor tests a work-in-progress
			// extension in isolation from everything else that would match.
			if session := debugSessionFrom(r.Context()); session != nil {
				state.items = []*extensions.Compiled{session.item}
			}
			// Live log sink: every matched extension writes its structured log
			// records into its per-extension monitor and rolling store. The
			// sink is installed once per Compiled (at compile time it is nil)
			// and the closure is identical for every request, so concurrent
			// writes race only on the benign field, not on behavior.
			if s.extensionLogs != nil {
				rotation := s.extensionLogs.rotationFor(extensionLogRoot(&cfg), cfg.Extensions)
				for _, item := range state.items {
					if item == nil || item.LogSink != nil {
						continue
					}
					extensionID := item.Manifest.ID
					monitor := s.extensionLogs.monitorFor(extensionID)
					item.SetLogSink(func(record extensions.LogRecord) {
						line := formatExtensionLog(record)
						monitor.Write([]byte(line))
						// Persistence level filtering: records always reach the
						// live broadcast; only levels the operator configured are
						// appended to the rolling store.
						if logLevelPersisted(record.Level) {
							rotation.append(extensionID, line)
						}
					})
				}
			}
			pending, hasPending := r.Context().Value(extensionPendingKey{}).(extensionPendingContext)
			if hasPending && pending.record.ModelID != data.ModelID {
				swaputil.SendResponse(w, r, http.StatusConflict, "extension continuation model changed")
				return
			}
			if len(state.items) == 0 {
				if hasPending {
					swaputil.SendResponse(w, r, http.StatusConflict, "extension is no longer available for continuation")
					return
				}
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1))
			if err != nil || len(body) > swaputil.MaxRequestBodySize {
				swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, "extension request body is too large")
				return
			}
			if protocolTargetFormat(r) == protocolrelaykit.OpenAIChat || state.meta.Endpoint == "chat.completions" {
				body, _, err = protocolrelaykit.StripHostedSearchTools(body, protocolrelaykit.OpenAIChat)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
			}
			var request map[string]any
			if err := json.Unmarshal(body, &request); err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension request must be JSON: "+jsonErrorDetail(body, err))
				return
			}
			format := dialect.OpenAIResponses
			switch endpoint {
			case "chat.completions":
				format = dialect.OpenAIChat
			case "anthropic.messages":
				format = dialect.ClaudeMessages
			case "gemini.generateContent":
				format = dialect.Gemini
			}
			if parsed, notes, err := dialect.DecodeRequest(format, body); err == nil {
				state.toolChoice = parsed.ToolChoice
				state.parallelTools = parsed.ParallelTools
				for _, note := range notes {
					if note.Code == "tool_choice_dropped" {
						state.unknownToolChoice = true
					}
				}
			}
			if serverToolArtifactsVisible(endpoint) {
				// A caller that replayed the previous turn carries the server-tool
				// artifacts we handed it; fold them back into the ordinary call and
				// result pair every dialect below understands.
				downgradeServerArtifacts(request, endpoint)
			}

			pendingComplete := false
			if hasPending {
				if err := s.store.ClaimExtensionPending(r.Context(), pending.record.ID); err != nil {
					swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
					return
				}
				defer func() {
					cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					defer cancel()
					if pendingComplete {
						_ = s.store.DeleteExtensionPending(cleanup, pending.record.ID)
					} else {
						_ = s.store.ReleaseExtensionPending(cleanup, pending.record.ID)
					}
				}()
				if err := s.foldDeferredRound(r.Context(), state, request, pending, endpoint); err != nil {
					if continuationMismatch(err) {
						// The replayed turn is not the recorded round (a same-named
						// call of another conversation on a name-keyed surface).
						swaputil.SendResponse(w, r, http.StatusBadRequest, mismatchResponse)
						return
					}
					if replayNeeded(err) {
						// An incomplete continuation is an invalid request, not a
						// conflict: this is the 400 Anthropic's Messages API
						// answers with when the replayed history is missing part
						// of the turn.
						swaputil.SendResponse(w, r, http.StatusBadRequest, needsReplayResponse)
						return
					}
					swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
					return
				}
			}
			// The record's conversation fingerprint is taken from the request the
			// caller sent: an onRequest hook may rewrite the prompt, and the caller's
			// replay will not carry the rewritten text.
			state.promptFingerprint = pendingContextFingerprint(endpoint, request, -1)
			if endpoint == "responses" {
				// The visible turn is re-persisted for previous_response_id after the
				// extensions rewrote it. Retention needs permission from both ends: the
				// caller's own store:false is a privacy request that a request hook must
				// not override, and a hook that asks for no retention is honored too.
				// Read here, before the hooks, so the caller's flag is the one that
				// survives rewriting.
				if store, ok := request["store"].(bool); ok && !store {
					state.suppressAffinity = true
				} else {
					state.suppressAffinity = false
				}
			}
			originalModel := request["model"]
			// The hosted-tool baseline must be captured before the request hooks:
			// the openai-web-search preset pushes {"type":"web_search"} from
			// onRequest, and a baseline taken after it hides that addition from
			// the marker, so the adapter strip removed the injected tool. It is
			// kept on the request state because the injection block below runs in
			// a later scope.
			state.hostedToolsBefore = hostedToolCount(request["tools"])
			for _, item := range state.items {
				if !hasExtensionHook(item, "onRequest") {
					continue
				}
				request, err = extensionMapHook(r.Context(), item, "onRequest", meta, request)
				if err != nil && !item.Manifest.ContinueOnError {
					// A request hook that cannot run because the conversation
					// does not fit on the worker's protocol line is a capacity
					// limit, not a broken extension: the request continues
					// without this hook rather than failing, and the reason is
					// logged so an operator can see why the extension went
					// quiet on long conversations.
					if errors.Is(err, extensions.ErrHookInputTooLarge) {
						slog.Warn("extension request hook skipped: payload too large",
							"extension", item.Manifest.ID, "request_id", state.meta.RequestID, "error", err)
						continue
					}
					swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
					return
				}
			}
			if request["model"] != originalModel {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension cannot change routed model")
				return
			}
			if endpoint != "gemini.generateContent" {
				if stream, _ := request["stream"].(bool); stream != data.Streaming {
					swaputil.SendResponse(w, r, http.StatusBadRequest, "extension cannot change stream mode")
					return
				}
			}
			if endpoint == "responses" {
				if store, ok := request["store"].(bool); ok && !store {
					state.suppressAffinity = true
				}
			}
			if data.Streaming {
				if !requiresExtensionStreamBridge(state.items) {
					prepared, err := json.Marshal(request)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
					roundRequest := r.Clone(context.WithValue(r.Context(), extensionContextKey{}, state))
					setExtensionBody(roundRequest, prepared)
					roundRequest = markInjectedHostedTools(roundRequest, request, state)
					next.ServeHTTP(w, roundRequest)
					pendingComplete = true
					return
				}
				pendingComplete = serveExtensionStream(s, w, r, next, cfg, state, request)
				return
			}
			var usage map[string]any
			rounds := cfg.Extensions.EffectiveMaxToolRounds()
			for round := 0; round < rounds; round++ {
				// The last round offers no server tools: the rounds are spent, so
				// the model has to answer with what it already has. Ending on a
				// tool call the extension would have run, but no longer can,
				// left the client with a call it could not execute and no text
				// to show.
				state.withholdTools = round+1 >= rounds
				prepared, err := json.Marshal(request)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
				roundRequest := r.Clone(context.WithValue(r.Context(), extensionContextKey{}, state))
				setExtensionBody(roundRequest, prepared)
				roundRequest = markInjectedHostedTools(roundRequest, request, state)
				buffer := newBufferedResponseWriter()
				next.ServeHTTP(buffer, roundRequest)
				if buffer.overflow {
					swaputil.SendResponse(w, r, http.StatusBadGateway, "extension response exceeds size limit")
					return
				}
				status := buffer.status
				if status == 0 {
					status = http.StatusOK
				}
				if status < 200 || status >= 300 {
					notifyExtensionsError(r.Context(), state, fmt.Sprintf("upstream returned HTTP %d", status))
					copyExtensionResponse(w, buffer, nil)
					return
				}
				var response map[string]any
				if err := decodeExtensionRound(buffer, endpoint, &response); err != nil {
					swaputil.SendResponse(w, r, http.StatusBadGateway, "extension upstream response is invalid JSON: "+err.Error())
					return
				}
				// A withheld tool must not survive in the body either: some
				// models name a tool they were not offered, and the client has
				// no way to run it.
				if withheld := state.withheldServerTools(); len(withheld) > 0 {
					dropWithheldCalls(response, endpoint, withheld)
				}
				if current, ok := response["usage"].(map[string]any); ok {
					usage = sumUsage(usage, current)
				}
				if len(usage) > 0 {
					response["usage"] = usage
				}
				calls := extensionCalls(response, endpoint)
				serverCalls, clientCalls, err := classifyExtensionCalls(r.Context(), state, calls)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
					return
				}
				if err := prepareMCPApprovals(r.Context(), state, request, response, serverCalls, endpoint); err != nil {
					if mcpErrorResponse(w, r, err) {
						return
					}
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				// Alternate choices never join the extension loop, and a call there
				// must not reach the caller unexecuted, whichever branch answers.
				stripAlternateChoiceCalls(response, endpoint)
				if len(serverCalls) == 0 {
					for i := len(state.items) - 1; i >= 0; i-- {
						item := state.items[i]
						if !hasExtensionHook(item, "onResponse") {
							continue
						}
						response, err = extensionMapHook(r.Context(), item, "onResponse", meta, response)
						if err != nil && !item.Manifest.ContinueOnError {
							if errors.Is(err, extensions.ErrHookInputTooLarge) {
								slog.Warn("extension response hook skipped: payload too large",
									"extension", item.Manifest.ID, "request_id", state.meta.RequestID, "error", err)
								continue
							}
							swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
							return
						}
					}
					applyVisibleTurn(response, state, endpoint)
					final, err := json.Marshal(response)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
						return
					}
					s.persistVisibleResponse(r.Context(), state, final)
					copyExtensionResponse(w, buffer, final)
					pendingComplete = true
					return
				}
				var demotedNames map[string]bool
				if len(clientCalls) != 0 && serverToolArtifactsVisible(endpoint) {
					// The caller replays the whole turn, so the executed server calls
					// travel back as artifacts instead of a daemon-side record:
					// nothing about this round has to survive the round trip.
					if err := extensionContinue(r.Context(), state, request, response, serverCalls, endpoint); err != nil {
						if mcpErrorResponse(w, r, err) {
							return
						}
						swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
						return
					}
					setVisibleTurn(response, endpoint, state.visibleTurn, visibleTurnBlocks(response, endpoint, state.serverOutputs, serverCalls))
					final, marshalErr := json.Marshal(response)
					if marshalErr != nil {
						swaputil.SendResponse(w, r, http.StatusBadGateway, marshalErr.Error())
						return
					}
					s.persistVisibleResponse(r.Context(), state, final)
					copyExtensionResponse(w, buffer, final)
					pendingComplete = true
					return
				}
				if len(clientCalls) != 0 {
					annotated := mergeClassifiedCalls(calls, append(append([]extensionCall{}, serverCalls...), clientCalls...))
					publicCalls, saveErr := s.saveExtensionMixed(r.Context(), state, annotated, clientCalls, endpoint)
					if saveErr != nil {
						if !errors.Is(saveErr, errExtensionContinuationTooLarge) {
							swaputil.SendResponse(w, r, http.StatusBadGateway, saveErr.Error())
							return
						}
						// The conversation is too large to record how to resume a
						// caller-run tool, so the call cannot be handed over. Report
						// it to the model as a failed tool and let the round loop
						// continue: a capacity limit degrades the answer, it does not
						// fail the request.
						slog.Warn("extension continuation too large; caller tool calls are degraded",
							"model", state.meta.ResolvedModel, "endpoint", endpoint, "request_id", state.meta.RequestID, "error", saveErr.Error())
						serverCalls = append(serverCalls, demoteUncontinuableClientCalls(state, clientCalls, saveErr)...)
						// Neither the demoted calls nor the extension's own calls may
						// reach a caller that cannot run or continue them.
						demotedNames = extensionCallNames(serverCalls)
					} else {
						exposeExtensionClientCalls(response, publicCalls, endpoint)
						final, marshalErr := json.Marshal(response)
						if marshalErr != nil {
							swaputil.SendResponse(w, r, http.StatusBadGateway, marshalErr.Error())
							return
						}
						copyExtensionResponse(w, buffer, final)
						pendingComplete = true
						return
					}
				}
				if round+1 >= rounds {
					// The rounds are spent. Hand the client the model's last
					// response instead of an error: a tool loop that did not
					// converge is a degraded answer, not a failed request.
					slog.Warn("extension tool round limit reached; returning the model's last response",
						"endpoint", state.meta.Endpoint, "rounds", rounds)
					// A call the caller cannot continue must not survive in the
					// body either. The continuation above already carries it, so
					// this only changes what the caller reads.
					dropWithheldCalls(response, endpoint, demotedNames)
					applyVisibleTurn(response, state, endpoint)
					final, err := json.Marshal(response)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
						return
					}
					s.persistVisibleResponse(r.Context(), state, final)
					copyExtensionResponse(w, buffer, final)
					pendingComplete = true
					return
				}
				if err := extensionContinue(r.Context(), state, request, response, serverCalls, endpoint); err != nil {
					if mcpErrorResponse(w, r, err) {
						return
					}
					swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
					return
				}
				if serverToolArtifactsVisible(endpoint) {
					state.visibleTurn = append(state.visibleTurn, visibleTurnBlocks(response, endpoint, state.serverOutputs, serverCalls)...)
				}
			}
		})
	}
}

func requiresExtensionStreamBridge(items []*extensions.Compiled) bool {
	for _, item := range items {
		if item.Manifest.InterceptClientTools && hasExtensionHook(item, "onToolCall") {
			return true
		}
		if hasExtensionHook(item, "onStreamEvent") {
			return true
		}
		for _, tool := range item.Tools {
			if tool.Execution == "server" {
				return true
			}
		}
	}
	return false
}

func notifyExtensionsError(ctx context.Context, state *extensionRequestState, message string) {
	if state == nil {
		return
	}
	encoded, _ := json.Marshal(map[string]any{"hook": "upstream", "error": message})
	for _, item := range state.items {
		if !hasExtensionHook(item, "onError") {
			continue
		}
		_, _, _ = item.Invoke(ctx, "onError", state.meta, encoded)
	}
}

func CreateExtensionsBeforeForwardMiddleware() chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			state, ok := r.Context().Value(extensionContextKey{}).(*extensionRequestState)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			body, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1))
			if err != nil || len(body) > swaputil.MaxRequestBodySize {
				swaputil.SendResponse(w, r, http.StatusRequestEntityTooLarge, "extension request body is too large")
				return
			}
			var request map[string]any
			if err := json.Unmarshal(body, &request); err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension request must be JSON: "+jsonErrorDetail(body, err))
				return
			}
			target := protocolTargetFormat(r)
			chatDialect := target == protocolrelaykit.OpenAIChat || (target == "" && state.meta.Endpoint == "chat.completions")
			claudeDialect := target == protocolrelaykit.ClaudeMessages || (target == "" && state.meta.Endpoint == "anthropic.messages")
			geminiDialect := target == protocolrelaykit.Gemini || (target == "" && state.meta.Endpoint == "gemini.generateContent")
			if state.meta.Endpoint != "anthropic.messages" {
				state.owners = map[string]*extensions.Compiled{}
			}
			// Which server tools this round must not offer is decided before the
			// injection runs, because the decision depends on what earlier rounds
			// injected - see beginRound.
			state.beginRound(extensionToolNames(request["tools"]))
			forwardModel, forwardStream := request["model"], request["stream"]
			capture := debugCaptureFrom(r.Context())
			injectedTools := false
			for _, item := range state.items {
				skip := state.toolSkipList()
				if claudeDialect {
					toolsBefore := extensionToolNames(request["tools"])
					if err := injectAnthropicTools(request, item, state.owners, skip); err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
					injectedTools = injectedTools || len(addedExtensionTools(toolsBefore, extensionToolNames(request["tools"]))) > 0
					if capture != nil {
						capture.recordInjectedTools(addedExtensionTools(toolsBefore, extensionToolNames(request["tools"])))
					}
				} else if geminiDialect {
					toolsBefore := extensionToolNames(request["tools"])
					if err := injectGeminiTools(request, item, state.owners, skip); err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
					injectedTools = injectedTools || len(addedExtensionTools(toolsBefore, extensionToolNames(request["tools"]))) > 0
				} else {
					toolsBefore := extensionToolNames(request["tools"])
					if err := injectExtensionTools(request, item, chatDialect, state.owners, skip); err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
					if capture != nil {
						capture.recordInjectedTools(addedExtensionTools(toolsBefore, extensionToolNames(request["tools"])))
					}
					injectedTools = injectedTools || len(addedExtensionTools(toolsBefore, extensionToolNames(request["tools"]))) > 0
				}
				if hasExtensionHook(item, "onBeforeForward") {
					forwardTools, _ := json.Marshal(request["tools"])
					request, err = extensionMapHook(r.Context(), item, "onBeforeForward", state.meta, request)
					if err != nil && !item.Manifest.ContinueOnError {
						if errors.Is(err, extensions.ErrHookInputTooLarge) {
							slog.Warn("extension forward hook skipped: payload too large",
								"extension", item.Manifest.ID, "request_id", state.meta.RequestID, "error", err)
							continue
						}
						swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
						return
					}
					currentTools, _ := json.Marshal(request["tools"])
					if !bytes.Equal(forwardTools, currentTools) {
						swaputil.SendResponse(w, r, http.StatusBadRequest, "onBeforeForward cannot change tool definitions")
						return
					}
				}
			}
			// Remember what the extensions actually injected. On a later round
			// those names are the ones to withhold: they belong to an extension,
			// so neither the model nor the caller may be left holding them.
			if len(state.owners) > 0 {
				if state.injectedServerTools == nil {
					state.injectedServerTools = map[string]bool{}
				}
				for name := range state.owners {
					state.injectedServerTools[name] = true
				}
			}
			if request["model"] != forwardModel || request["stream"] != forwardStream {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension cannot change routed model or stream mode")
				return
			}
			if hostedToolCount(request["tools"]) > state.hostedToolsBefore {
				injectedTools = true
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			setExtensionBody(r, encoded)
			if injectedTools {
				r = markExtensionServerTools(r)
			}
			next.ServeHTTP(w, r)
		})
	}
}

// hostedToolCount counts provider-hosted search tools in a tool list, so the
// adapter can tell a client-sent hosted tool from an extension-injected one.
func hostedToolCount(raw any) int {
	tools, ok := raw.([]any)
	if !ok {
		return 0
	}
	count := 0
	for _, item := range tools {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		typeName, _ := tool["type"].(string)
		if strings.HasPrefix(typeName, "web_search") || tool["googleSearch"] != nil || tool["google_search"] != nil {
			count++
		}
	}
	return count
}

// markInjectedHostedTools flags a request whose tool list gained a
// provider-hosted tool from an extension hook. The protocol adapter, which runs
// downstream of this middleware, strips client-sent hosted search tools because
// many upstreams reject them; a tool an extension added on purpose must survive
// that strip (the openai-web-search preset depends on it).
func markInjectedHostedTools(r *http.Request, request map[string]any, state *extensionRequestState) *http.Request {
	if hostedToolCount(request["tools"]) > state.hostedToolsBefore {
		return markExtensionServerTools(r)
	}
	return r
}

func setExtensionBody(r *http.Request, body []byte) {
	swaputil.ReplaceRequestBody(r, body)
}

func hasExtensionHook(item *extensions.Compiled, hook string) bool {
	for _, candidate := range item.Hooks {
		if candidate == hook {
			return true
		}
	}
	return false
}

// extensionMapHook runs one extension hook and discards its log output.
func extensionMapHook(ctx context.Context, item *extensions.Compiled, hook string, meta extensions.Context, input map[string]any) (map[string]any, error) {
	result, _, err := extensionMapHookLogged(ctx, item, hook, meta, input)
	return result, err
}

// extensionMapHookLogged runs one hook and also returns the lines the script
// wrote through ctx.log, which the test endpoint reports back to the UI.
// Debug chat requests additionally capture the invocation for the editor.
func extensionMapHookLogged(ctx context.Context, item *extensions.Compiled, hook string, meta extensions.Context, input map[string]any) (map[string]any, []string, error) {
	encoded, err := json.Marshal(input)
	if err != nil {
		return input, nil, err
	}
	// The size check lives in extensions.Compiled.Invoke, which knows how much
	// of the protocol line this extension's own bundle consumes and returns
	// ErrHookInputTooLarge for a payload that does not fit. Checking here too
	// would measure the same limit against the wrong number.
	capture := debugCaptureFrom(ctx)
	started := time.Now()
	output, logs, err := item.Invoke(ctx, hook, meta, encoded)
	if capture != nil {
		var result map[string]any
		if unmarshalErr := json.Unmarshal(output, &result); unmarshalErr == nil {
			capture.recordHook(item.Manifest.ID, hook, time.Since(started), input, result, logs, err)
		} else {
			capture.recordHook(item.Manifest.ID, hook, time.Since(started), input, nil, logs, err)
		}
	}
	for _, line := range logs {
		slog.Debug("extension log", "extension", item.Manifest.ID, "hook", hook, "request_id", meta.RequestID, "message", line)
	}
	if err != nil {
		slog.Warn("extension hook failed", "extension", item.Manifest.ID, "hook", hook, "model", meta.ResolvedModel, "request_id", meta.RequestID, "duration", time.Since(started), "error", err)
		if hook != "onError" && hasExtensionHook(item, "onError") {
			diagnostic, _ := json.Marshal(map[string]any{"hook": hook, "error": err.Error()})
			_, _, _ = item.Invoke(ctx, "onError", meta, diagnostic)
		}
		return input, logs, fmt.Errorf("extension %s %s: %w", item.Manifest.ID, hook, err)
	}
	slog.Debug("extension hook completed", "extension", item.Manifest.ID, "hook", hook, "model", meta.ResolvedModel, "request_id", meta.RequestID, "duration", time.Since(started))
	var result map[string]any
	if err := json.Unmarshal(output, &result); err != nil || result == nil {
		return input, logs, fmt.Errorf("extension %s %s must return an object", item.Manifest.ID, hook)
	}
	return result, logs, nil
}

// markToolFailed records that a tool cannot run for the rest of this request.
func (state *extensionRequestState) markToolFailed(name string, failure error) {
	if state == nil || name == "" {
		return
	}
	if state.failedTools == nil {
		state.failedTools = map[string]bool{}
	}
	if state.failedTools[name] {
		return
	}
	state.failedTools[name] = true
	if failure != nil {
		slog.Warn("extension tool failed for this request; it will not be offered again",
			"tool", name, "error", failure.Error())
	}
}

// decodeExtensionRound turns one buffered upstream response into the response
// object every endpoint has in common.
//
// A round runs without the stream bridge, but that does not mean the upstream
// answered with a JSON document: a passthrough surface can compress the body
// (we forward the client's Accept-Encoding, and a large answer over a CDN is
// routinely gzipped), and some upstreams stream anyway. Both used to reach
// json.Unmarshal raw, which turned a perfectly good answer into
// "extension upstream response is invalid JSON".
func decodeExtensionRound(buffer *bufferedResponseWriter, endpoint string, response *map[string]any) error {
	body := buffer.body.Bytes()
	if decoded, err := decodeBufferedBody(buffer, body); err != nil {
		return err
	} else {
		body = decoded
	}
	if isEventStream(buffer.Header().Get("Content-Type"), body) {
		return assembleExtensionStream(endpoint, body, response)
	}
	if err := json.Unmarshal(body, response); err != nil {
		// Name what actually arrived: the content type and a short preview are
		// what distinguish a mislabelled stream from a truncated body, and the
		// archived error log is the only place an operator can see it.
		return fmt.Errorf("not a JSON object (content-type %q, %d bytes, starts %q)",
			buffer.Header().Get("Content-Type"), len(body), previewBytes(body, 64))
	}
	return nil
}

// previewBytes renders the head of an upstream body for a diagnostic message:
// quotes and control bytes stay escaped, so an error log can never be broken by
// the bytes it describes.
func previewBytes(body []byte, limit int) string {
	if len(body) > limit {
		body = body[:limit]
	}
	return strings.ToValidUTF8(string(body), "?")
}

// decodeBufferedBody unwraps the Content-Encoding the upstream applied. The
// header is left alone: the bytes forwarded to a caller that asked for gzip
// stay compressed, and only the copy we parse is expanded.
func decodeBufferedBody(buffer *bufferedResponseWriter, body []byte) ([]byte, error) {
	encoding := strings.TrimSpace(buffer.Header().Get("Content-Encoding"))
	if encoding == "" || strings.EqualFold(encoding, "identity") {
		return body, nil
	}
	decoded, err := decompressBody(body, encoding)
	if err != nil {
		return nil, fmt.Errorf("could not decode a %s response body: %w", encoding, err)
	}
	return decoded, nil
}

// isEventStream reports whether a buffered body is an SSE transcript. The
// upstream's declared type is the first signal; a body that begins with an SSE
// field is one even when the header was lost or never set, which is how a
// passthrough surface hands back a stream to a caller that asked for JSON.
func isEventStream(contentType string, body []byte) bool {
	if strings.Contains(strings.ToLower(contentType), "text/event-stream") {
		return true
	}
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("data:")) || bytes.HasPrefix(trimmed, []byte("event:"))
}

// assembleExtensionStream replays an SSE transcript through the stream writer
// and returns the response it would have produced, so a mislabelled stream
// follows the same classification path as a JSON round instead of failing the
// request.
func assembleExtensionStream(endpoint string, body []byte, response *map[string]any) error {
	sink := &discardResponseWriter{header: make(http.Header)}
	writer := newExtensionStreamWriter(sink, nil, &extensionRequestState{meta: extensions.Context{Endpoint: endpoint}})
	if _, err := writer.Write(body); err != nil {
		return err
	}
	assembled := writer.ContinueResponse()
	if sink.status >= 400 {
		return fmt.Errorf("the upstream stream reported HTTP %d", sink.status)
	}
	if len(assembled) == 0 {
		return errors.New("the upstream stream carried no response")
	}
	*response = assembled
	if len(writer.usage) > 0 {
		(*response)["usage"] = writer.usage
	}
	return nil
}

// stripAlternateChoiceCalls removes tool calls from every choice but the first.
//
// The extension loop continues one conversation: a call in an alternate choice
// can neither run with its result spliced into the continuation nor be handed to
// the caller with any hope of a coherent next turn, so it is dropped. Leaving it
// in the body was how a server-tool call reached a client that cannot run it.
func stripAlternateChoiceCalls(response map[string]any, endpoint string) {
	if endpoint != "chat.completions" {
		return
	}
	choices, _ := response["choices"].([]any)
	for _, raw := range choices[min(1, len(choices)):] {
		choice, _ := raw.(map[string]any)
		message, _ := choice["message"].(map[string]any)
		if message == nil {
			continue
		}
		if _, ok := message["tool_calls"]; !ok {
			continue
		}
		delete(message, "tool_calls")
		if finish, _ := choice["finish_reason"].(string); finish == "tool_calls" {
			choice["finish_reason"] = "stop"
		}
	}
}

// withheldServerTools names the extension server tools the current round does
// not offer. Only the final round withholds anything.
func (state *extensionRequestState) withheldServerTools() map[string]bool {
	if state == nil || !state.withholdTools {
		return nil
	}
	return state.withheld
}

// beginRound works out which of the extensions' own server tools this round
// must leave out of the tool list.
//
// The set is ownership-based, not name-based: a server tool the extension
// injected earlier in this request is withheld, while a name the caller also
// declared stays with the caller when the conflict policy left it there. That
// distinction matters on the final round, where a withheld call is dropped
// outright - dropping the caller's own tool call would be wrong.
//
// declared is the tool list as the caller sent it, before any injection.
func (state *extensionRequestState) beginRound(declared []string) {
	if state == nil {
		return
	}
	state.withheld = nil
	if !state.withholdTools {
		return
	}
	withheld := map[string]bool{}
	for name := range state.injectedServerTools {
		withheld[name] = true
	}
	if len(withheld) == 0 {
		// Nothing was injected earlier, so this is the request's first round
		// (a one-round tool budget). Withhold what this round would have added;
		// a name the caller declared is left to the conflict policy instead.
		fromCaller := make(map[string]bool, len(declared))
		for _, name := range declared {
			fromCaller[name] = true
		}
		for _, item := range state.items {
			for _, tool := range item.Tools {
				if tool.Execution != "server" {
					continue
				}
				var function struct {
					Name string `json:"name"`
				}
				if json.Unmarshal(tool.Function, &function) != nil || function.Name == "" || fromCaller[function.Name] {
					continue
				}
				withheld[function.Name] = true
			}
		}
	}
	if len(withheld) > 0 {
		state.withheld = withheld
	}
}

// toolSkipList is the set of tool names the round must not offer: the ones that
// already failed, plus every server tool once the rounds are spent.
func (state *extensionRequestState) toolSkipList() map[string]bool {
	if state == nil {
		return nil
	}
	withheld := state.withheldServerTools()
	if len(state.failedTools) == 0 {
		return withheld
	}
	skip := make(map[string]bool, len(state.failedTools)+len(withheld))
	for name := range state.failedTools {
		skip[name] = true
	}
	for name := range withheld {
		skip[name] = true
	}
	return skip
}

// dropWithheldCalls removes calls for withheld server tools from a response
// body. A model can still name a tool it was not offered; the client must not
// receive a call it cannot fulfil, so the artifact is dropped instead of being
// handed over as a client-side call.
func dropWithheldCalls(response map[string]any, endpoint string, withheld map[string]bool) {
	if response == nil || len(withheld) == 0 {
		return
	}
	switch endpoint {
	case "chat.completions":
		// Every choice can carry tool calls; inspecting only choices[0] leaked
		// withheld calls from multi-choice responses.
		choices, _ := response["choices"].([]any)
		for _, rawChoice := range choices {
			choice, ok := rawChoice.(map[string]any)
			if !ok {
				continue
			}
			message, ok := choice["message"].(map[string]any)
			if !ok {
				continue
			}
			calls, _ := message["tool_calls"].([]any)
			kept := make([]any, 0, len(calls))
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				function, _ := call["function"].(map[string]any)
				name, _ := function["name"].(string)
				if withheld[name] {
					continue
				}
				kept = append(kept, raw)
			}
			if len(kept) == len(calls) {
				continue
			}
			if len(kept) == 0 {
				delete(message, "tool_calls")
			} else {
				message["tool_calls"] = kept
			}
			if finish, _ := choice["finish_reason"].(string); finish == "tool_calls" {
				choice["finish_reason"] = "stop"
			}
		}
	case "anthropic.messages":
		content, _ := response["content"].([]any)
		kept := make([]any, 0, len(content))
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if block["type"] == "tool_use" {
				if name, _ := block["name"].(string); withheld[name] {
					continue
				}
			}
			kept = append(kept, raw)
		}
		if len(kept) == len(content) {
			return
		}
		response["content"] = kept
		if stop, _ := response["stop_reason"].(string); stop == "tool_use" {
			response["stop_reason"] = "end_turn"
		}
	default:
		output, _ := response["output"].([]any)
		kept := make([]any, 0, len(output))
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if item["type"] == "function_call" {
				if name, _ := item["name"].(string); withheld[name] {
					continue
				}
			}
			kept = append(kept, raw)
		}
		if len(kept) == len(output) {
			return
		}
		response["output"] = kept
	}
}

func injectExtensionTools(request map[string]any, item *extensions.Compiled, chat bool, owners map[string]*extensions.Compiled, skip map[string]bool) error {
	if len(item.Tools) == 0 {
		return nil
	}
	tools, _ := request["tools"].([]any)
	for _, tool := range item.Tools {
		var function map[string]any
		if err := json.Unmarshal(tool.Function, &function); err != nil {
			return err
		}
		name, _ := function["name"].(string)
		if skip[name] {
			continue
		}
		index := -1
		for i, raw := range tools {
			if toolName(raw, chat) == name {
				index = i
				break
			}
		}
		if index >= 0 {
			switch item.Manifest.ToolConflict {
			case "override":
			case "error":
				return fmt.Errorf("extension %s tool %s conflicts with an existing tool", item.Manifest.ID, name)
			default:
				slog.Warn("extension tool skipped due to name conflict", "extension", item.Manifest.ID, "tool", name)
				continue
			}
		}
		var rendered map[string]any
		if chat {
			rendered = map[string]any{"type": "function", "function": function}
		} else {
			rendered = map[string]any{"type": "function"}
			for key, value := range function {
				rendered[key] = value
			}
		}
		if index >= 0 {
			tools[index] = rendered
		} else {
			tools = append(tools, rendered)
		}
		if tool.Execution == "server" {
			owners[name] = item
		} else {
			delete(owners, name)
		}
	}
	request["tools"] = tools
	return nil
}

func injectAnthropicTools(request map[string]any, item *extensions.Compiled, owners map[string]*extensions.Compiled, skip map[string]bool) error {
	if len(item.Tools) == 0 {
		return nil
	}
	tools, _ := request["tools"].([]any)
	for _, tool := range item.Tools {
		var function map[string]any
		if err := json.Unmarshal(tool.Function, &function); err != nil {
			return err
		}
		name, _ := function["name"].(string)
		if skip[name] {
			continue
		}
		index := -1
		for i, raw := range tools {
			candidate, _ := raw.(map[string]any)
			if candidate["name"] == name {
				index = i
				break
			}
		}
		if index >= 0 {
			switch item.Manifest.ToolConflict {
			case "override":
			case "error":
				return fmt.Errorf("extension %s tool %s conflicts with an existing tool", item.Manifest.ID, name)
			default:
				slog.Warn("extension tool skipped due to name conflict", "extension", item.Manifest.ID, "tool", name)
				continue
			}
		}
		rendered := map[string]any{"name": name, "input_schema": function["parameters"]}
		if rendered["input_schema"] == nil {
			rendered["input_schema"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if description, ok := function["description"].(string); ok {
			rendered["description"] = description
		}
		if strict, ok := function["strict"].(bool); ok {
			rendered["strict"] = strict
		}
		if index >= 0 {
			tools[index] = rendered
		} else {
			tools = append(tools, rendered)
		}
		if tool.Execution == "server" {
			owners[name] = item
		} else {
			delete(owners, name)
		}
	}
	request["tools"] = tools
	return nil
}

func injectGeminiTools(request map[string]any, item *extensions.Compiled, owners map[string]*extensions.Compiled, skip map[string]bool) error {
	if len(item.Tools) == 0 {
		return nil
	}
	tools, _ := request["tools"].([]any)
	for _, tool := range item.Tools {
		var function map[string]any
		if err := json.Unmarshal(tool.Function, &function); err != nil {
			return err
		}
		name, _ := function["name"].(string)
		if skip[name] {
			continue
		}
		containerIndex, declarationIndex := -1, -1
		for ti, rawTool := range tools {
			toolObject, _ := rawTool.(map[string]any)
			declarations, _ := toolObject["functionDeclarations"].([]any)
			for di, rawDeclaration := range declarations {
				declaration, _ := rawDeclaration.(map[string]any)
				if declaration["name"] == name {
					containerIndex, declarationIndex = ti, di
					break
				}
			}
			if declarationIndex >= 0 {
				break
			}
		}
		if declarationIndex >= 0 {
			switch item.Manifest.ToolConflict {
			case "override":
			case "error":
				return fmt.Errorf("extension %s tool %s conflicts with an existing tool", item.Manifest.ID, name)
			default:
				slog.Warn("extension tool skipped due to name conflict", "extension", item.Manifest.ID, "tool", name)
				continue
			}
		}
		declaration := map[string]any{
			"name":       name,
			"parameters": function["parameters"],
		}
		if declaration["parameters"] == nil {
			declaration["parameters"] = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		if description, ok := function["description"].(string); ok {
			declaration["description"] = description
		}
		if declarationIndex >= 0 {
			container, _ := tools[containerIndex].(map[string]any)
			declarations, _ := container["functionDeclarations"].([]any)
			declarations[declarationIndex] = declaration
			container["functionDeclarations"] = declarations
			tools[containerIndex] = container
		} else {
			tools = append(tools, map[string]any{"functionDeclarations": []any{declaration}})
		}
		if tool.Execution == "server" {
			owners[name] = item
		} else {
			delete(owners, name)
		}
	}
	request["tools"] = tools
	return nil
}

func toolName(raw any, chat bool) string {
	tool, ok := raw.(map[string]any)
	if !ok {
		return ""
	}
	if chat {
		function, _ := tool["function"].(map[string]any)
		name, _ := function["name"].(string)
		return name
	}
	name, _ := tool["name"].(string)
	return name
}

// extensionToolNames lists the tool names currently present in a request body.
func extensionToolNames(raw any) []string {
	tools, _ := raw.([]any)
	names := make([]string, 0, len(tools))
	for _, tool := range tools {
		if name := toolName(tool, true); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// addedExtensionTools returns the names present after but not before injection.
func addedExtensionTools(before, after []string) []string {
	known := make(map[string]bool, len(before))
	for _, name := range before {
		known[name] = true
	}
	added := make([]string, 0, len(after))
	for _, name := range after {
		if !known[name] {
			added = append(added, name)
		}
	}
	return added
}

type extensionCall struct {
	ID, OriginalID, Name, Arguments string
	Owner                           *extensions.Compiled
	Precomputed                     map[string]any
	// RawPart is the wire part a call arrived as, when the surface's parts carry
	// more than a name and arguments (Gemini thought signatures). A continuation
	// echoes it back instead of rebuilding the part from name and arguments alone.
	RawPart map[string]any
}

func classifyExtensionCalls(ctx context.Context, state *extensionRequestState, calls []extensionCall) ([]extensionCall, []extensionCall, error) {
	serverCalls := make([]extensionCall, 0, len(calls))
	clientCalls := make([]extensionCall, 0, len(calls))
	withheld := state.withheldServerTools()
	if len(calls) > 1 && (state.parallelTools != nil && !*state.parallelTools || state.toolChoice != nil && state.toolChoice.DisableParallel != nil && *state.toolChoice.DisableParallel) {
		return nil, nil, errors.New("upstream violated the request's parallel tool-call policy")
	}
	for _, call := range calls {
		if state.owners[call.Name] != nil {
			if err := validateExtensionToolChoice(state, call.Name); err != nil {
				return nil, nil, err
			}
		}
		if withheld[call.Name] {
			// The round did not offer this tool, so it cannot be run and the
			// client cannot run it either: drop the call rather than hand over
			// something nobody can serve.
			continue
		}
		if owner := state.owners[call.Name]; owner != nil {
			call.Owner = owner
			serverCalls = append(serverCalls, call)
			continue
		}
		intercepted := false
		var interceptorErr error
		var interceptorOwner *extensions.Compiled
		for _, item := range state.items {
			if !item.Manifest.InterceptClientTools || !hasExtensionHook(item, "onToolCall") {
				continue
			}
			if err := validateExtensionToolChoice(state, call.Name); err != nil {
				return nil, nil, err
			}
			conflict := false
			for _, tool := range item.Tools {
				var function struct {
					Name string `json:"name"`
				}
				_ = json.Unmarshal(tool.Function, &function)
				if function.Name == call.Name && item.Manifest.ToolConflict != "override" {
					conflict = true
					break
				}
			}
			if conflict {
				continue
			}
			var arguments any
			if json.Unmarshal([]byte(call.Arguments), &arguments) != nil {
				continue
			}
			input := map[string]any{"id": call.ID, "name": call.Name, "arguments": arguments}
			result, err := extensionMapHook(ctx, item, "onToolCall", state.meta, input)
			if err != nil {
				if item.Manifest.FailOnToolError {
					return nil, nil, err
				}
				// Remember the first failure and keep probing: a later
				// extension may still own this call. Only when nobody does is
				// the failure reported back to the model.
				if interceptorErr == nil {
					interceptorErr = err
					interceptorOwner = item
				}
				continue
			}
			handled, _ := result["handled"].(bool)
			if !handled {
				continue
			}
			if _, ok := result["content"]; !ok {
				if item.Manifest.FailOnToolError {
					return nil, nil, fmt.Errorf("extension %s intercepted tool %s without content", item.Manifest.ID, call.Name)
				}
				if interceptorErr == nil {
					interceptorErr = fmt.Errorf("extension %s intercepted tool %s without content", item.Manifest.ID, call.Name)
					interceptorOwner = item
				}
				continue
			}
			call.Owner = item
			call.Precomputed = result
			serverCalls = append(serverCalls, call)
			intercepted = true
			break
		}
		if intercepted {
			continue
		}
		if interceptorErr != nil {
			// The tool the model asked for could not run. That is the model's
			// business, not the request's: hand the failure to the model as the
			// tool result so it can retry, fall back, or answer without it.
			owner := interceptorOwner
			if owner == nil {
				owner = state.items[0]
			}
			call.Owner = owner
			state.markToolFailed(call.Name, interceptorErr)
			call.Precomputed = toolFailureResult(call.Name, interceptorErr)
			serverCalls = append(serverCalls, call)
			continue
		}
		clientCalls = append(clientCalls, call)
	}
	return serverCalls, clientCalls, nil
}

// geminiCallID mints the id used to pair a Gemini functionCall with its
// functionResponse. Gemini carries no ids, so position plus a counter is the
// only correlation the dialect offers.
func geminiCallID(index int) string {
	return fmt.Sprintf("call_gemini_%d", index+1)
}

func extensionCalls(response map[string]any, endpoint string) []extensionCall {
	result := []extensionCall{}
	if endpoint == "anthropic.messages" {
		blocks, _ := response["content"].([]any)
		for _, raw := range blocks {
			block, _ := raw.(map[string]any)
			if block["type"] != "tool_use" {
				continue
			}
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			arguments, err := json.Marshal(block["input"])
			if err == nil && id != "" && name != "" {
				result = append(result, extensionCall{ID: id, Name: name, Arguments: string(arguments)})
			}
		}
		return result
	}
	if endpoint == "gemini.generateContent" {
		candidates, _ := response["candidates"].([]any)
		if len(candidates) == 0 {
			return result
		}
		candidate, _ := candidates[0].(map[string]any)
		content, _ := candidate["content"].(map[string]any)
		parts, _ := content["parts"].([]any)
		for index, raw := range parts {
			part, _ := raw.(map[string]any)
			call := part["functionCall"]
			if call == nil {
				call = part["function_call"]
			}
			entry, _ := call.(map[string]any)
			if entry == nil {
				continue
			}
			name, _ := entry["name"].(string)
			arguments, err := json.Marshal(entry["args"])
			if err != nil {
				arguments = []byte("{}")
			}
			if name != "" {
				id, _ := entry["id"].(string)
				if id == "" {
					id = geminiCallID(index)
				}
				result = append(result, extensionCall{ID: id, Name: name, Arguments: string(arguments), RawPart: part})
			}
		}
		return result
	}
	if endpoint == "chat.completions" {
		// Only the first choice joins the extension loop: the continuation carries
		// one assistant turn, so a call in an alternate choice could neither be
		// executed with its result spliced in nor answered. Alternative choices are
		// stripped of their calls instead (stripAlternateChoiceCalls), which is what
		// keeps a server-tool call there from reaching the client unexecuted.
		choices, _ := response["choices"].([]any)
		for _, rawChoice := range choices[:min(1, len(choices))] {
			choice, _ := rawChoice.(map[string]any)
			message, _ := choice["message"].(map[string]any)
			calls, _ := message["tool_calls"].([]any)
			for _, raw := range calls {
				call, _ := raw.(map[string]any)
				function, _ := call["function"].(map[string]any)
				id, _ := call["id"].(string)
				name, _ := function["name"].(string)
				args, _ := function["arguments"].(string)
				if id != "" && name != "" {
					result = append(result, extensionCall{ID: id, Name: name, Arguments: args})
				}
			}
		}
		return result
	}
	items, _ := response["output"].([]any)
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		if item["type"] != "function_call" {
			continue
		}
		id, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		args, _ := item["arguments"].(string)
		if id != "" && name != "" {
			result = append(result, extensionCall{ID: id, Name: name, Arguments: args})
		}
	}
	return result
}

// applyToolResultHooks runs every extension's onToolResult transform. A failing
// transform keeps the untransformed output: a broken transform must not replace
// a usable tool result with a request failure (failOnToolError opts back in).
func applyToolResultHooks(ctx context.Context, state *extensionRequestState, output map[string]any) (map[string]any, error) {
	for _, item := range state.items {
		if !hasExtensionHook(item, "onToolResult") {
			continue
		}
		transformed, err := extensionMapHook(ctx, item, "onToolResult", state.meta, output)
		if err != nil {
			if item.Manifest.FailOnToolError {
				return nil, err
			}
			continue
		}
		output = transformed
	}
	return output, nil
}

// toolResultFor renders one tool result in the endpoint's own shape.
//
// A server tool must return {content: ...}. When it does not — a handler whose
// name check never matched and returned undefined, or one that returned its
// arguments untouched — the result is reported as a failure instead of echoing
// the input back. The echo produced a tool message the engine rejects (a tool
// result whose content is an object), which failed the whole request.
func toolResultFor(endpoint string, call extensionCall, output map[string]any) map[string]any {
	if native, err := mcpNativeToolResult(endpoint, call, output); err == nil && native != nil {
		return native
	} else if err != nil {
		output = toolFailureResult(call.Name, err)
	}
	content, ok := output["content"]
	if !ok || content == nil {
		output = toolFailureResult(call.Name, errors.New("the extension returned no tool result"))
		content = output["content"]
	}
	switch endpoint {
	case "chat.completions":
		// A chat tool message carries the result as text: an engine rejects a
		// tool message whose content is an object, so a handler that returned
		// structured data gets it JSON-encoded here.
		return map[string]any{"role": "tool", "tool_call_id": call.ID, "content": toolResultText(content)}
	case "gemini.generateContent":
		// Gemini pairs a result with its call by function name inside a user
		// turn; the id only exists for the gateway's own bookkeeping.
		return map[string]any{"type": "function_response", "name": call.Name, "response": content}
	case "anthropic.messages":
		if _, ok := content.(string); !ok {
			if _, ok := content.([]any); !ok {
				content = toolResultText(content)
			}
		}
		result := map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": content}
		if failed, _ := output["is_error"].(bool); failed {
			result["is_error"] = true
		}
		return result
	default:
		return map[string]any{"type": "function_call_output", "call_id": call.ID, "output": toolResultText(content)}
	}
}

// toolResultText renders a tool result for a message shape that carries text.
// Strings pass through; anything structured becomes compact JSON, which is what
// a model can read and what the engines accept.
func toolResultText(content any) any {
	if _, ok := content.(string); ok {
		return content
	}
	encoded, err := json.Marshal(content)
	if err != nil {
		return fmt.Sprintf("%v", content)
	}
	return string(encoded)
}

// toolFailureResult renders a failed extension tool call as the tool result the
// model receives. It carries the real error (quota exhausted, upstream down,
// timeout) plus a short note that the tool is unavailable for this turn, which
// is what lets the model answer without it instead of the request dying.
func toolFailureResult(tool string, failure error) map[string]any {
	detail := "unknown error"
	if failure != nil {
		detail = strings.TrimSpace(failure.Error())
		if detail == "" {
			detail = "unknown error"
		}
	}
	return map[string]any{
		"content":  fmt.Sprintf("tool %q failed and is unavailable for this request: %s", tool, detail),
		"is_error": true,
	}
}

func extensionContinue(ctx context.Context, state *extensionRequestState, request, response map[string]any, calls []extensionCall, endpoint string) error {
	if err := prepareMCPApprovals(ctx, state, request, response, calls, endpoint); err != nil {
		return err
	}
	var results []any
	for _, call := range calls {
		var args any
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			// Malformed arguments are the model's mistake, not the request's:
			// report them as the tool result so it can correct the call.
			if call.Owner != nil && call.Owner.Manifest.FailOnToolError {
				return fmt.Errorf("tool %s has invalid arguments: %w", call.Name, err)
			}
			state.markToolFailed(call.Name, err)
			output := toolFailureResult(call.Name, fmt.Errorf("arguments are not valid JSON: %w", err))
			output, err = applyToolResultHooks(ctx, state, output)
			if err != nil {
				return err
			}
			results = append(results, toolResultFor(endpoint, call, output))
			continue
		}
		input := map[string]any{"id": call.ID, "name": call.Name, "arguments": args}
		output := call.Precomputed
		var err error
		if output == nil {
			output, err = extensionMapHook(ctx, call.Owner, "onToolCall", state.meta, input)
		}
		if err != nil {
			var waiting *mcpApprovalRequired
			if errors.As(err, &waiting) && waiting.InputRequired {
				for i := range calls {
					if prior := state.serverOutputs[calls[i].ID]; prior != nil {
						calls[i].Precomputed = prior
					}
				}
				ctx := context.WithValue(ctx, mcpPendingInputKey{}, map[string]string{call.ID: waiting.ID})
				return prepareMCPApprovals(ctx, state, request, response, calls, endpoint)
			}
			if call.Owner.Manifest.FailOnToolError {
				return err
			}
			state.markToolFailed(call.Name, err)
			output = toolFailureResult(call.Name, err)
		}
		output, err = applyToolResultHooks(ctx, state, output)
		if err != nil {
			return err
		}
		if _, err := mcpNativeToolResult(endpoint, call, output); err != nil {
			return err
		}
		if _, ok := output["content"]; !ok {
			state.markToolFailed(call.Name, errors.New("the extension returned no tool result"))
		}
		if state != nil {
			if state.serverOutputs == nil {
				state.serverOutputs = map[string]map[string]any{}
			}
			state.serverOutputs[call.ID] = output
		}
		results = append(results, toolResultFor(endpoint, call, output))
	}
	if endpoint == "chat.completions" {
		choices, _ := response["choices"].([]any)
		if len(choices) == 0 {
			return errors.New("missing chat choice")
		}
		choice, _ := choices[0].(map[string]any)
		message, _ := choice["message"].(map[string]any)
		messages, _ := request["messages"].([]any)
		request["messages"] = append(append(messages, message), results...)
	} else if endpoint == "anthropic.messages" {
		messages, _ := request["messages"].([]any)
		assistant := map[string]any{"role": "assistant", "content": response["content"]}
		request["messages"] = append(messages, assistant, map[string]any{"role": "user", "content": results})
	} else if endpoint == "gemini.generateContent" {
		// The model's own functionCall turn must be echoed back verbatim (with
		// its thought signature), followed by the user turn carrying
		// functionResponse parts in the same order. A streaming round hands us
		// the content object directly; a buffered round hands the whole body.
		contents, _ := request["contents"].([]any)
		modelContent, _ := response["content"].(map[string]any)
		if modelContent == nil {
			if candidates, ok := response["candidates"].([]any); ok && len(candidates) > 0 {
				if candidate, ok := candidates[0].(map[string]any); ok {
					modelContent, _ = candidate["content"].(map[string]any)
				}
			}
		}
		if modelContent != nil {
			contents = append(contents, modelContent)
		}
		parts := make([]any, 0, len(results))
		for _, result := range results {
			entry, _ := result.(map[string]any)
			if native, ok := entry["native"].(map[string]any); ok {
				parts = append(parts, map[string]any{"functionResponse": native})
				continue
			}
			responsePayload := map[string]any{"result": entry["response"]}
			if entry["response"] == nil {
				responsePayload["result"] = ""
			}
			parts = append(parts, map[string]any{
				"functionResponse": map[string]any{
					"name": entry["name"], "response": responsePayload,
				},
			})
		}
		request["contents"] = append(contents, map[string]any{"role": "user", "parts": parts})
	} else {
		input, _ := request["input"].([]any)
		if input == nil {
			if text, ok := request["input"].(string); ok && text != "" {
				input = []any{map[string]any{"role": "user", "content": text}}
			}
		}
		items, _ := response["output"].([]any)
		request["input"] = append(append(input, items...), results...)
		delete(request, "previous_response_id")
	}
	return nil
}

func copyExtensionResponse(w http.ResponseWriter, buffer *bufferedResponseWriter, body []byte) {
	copyHeaders(w.Header(), buffer.Header())
	if body == nil {
		body = buffer.body.Bytes()
	} else {
		clearTransformedBodyHeaders(w.Header())
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	status := buffer.status
	if status == 0 {
		status = http.StatusOK
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}
