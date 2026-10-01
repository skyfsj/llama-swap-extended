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
	"github.com/mostlygeek/llama-swap/internal/swaputil"
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
	items              []*extensions.Compiled
	meta               extensions.Context
	owners             map[string]*extensions.Compiled
	streamID           string
	streamModel        string
	streamCreated      any
	streamCreatedFlag  bool
	streamText         string
	streamUsage        map[string]any
	streamOutputOffset int
}

func extensionEndpoint(path string) string {
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
			if manager == nil || endpoint == "" || r.Method != http.MethodPost {
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
			state := &extensionRequestState{items: manager.Match(meta), meta: meta, owners: map[string]*extensions.Compiled{}}
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
				logRoot := extensionLogRoot(&cfg)
				rotation := newExtensionLogRotation(logRoot, cfg.Extensions)
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
			var request map[string]any
			if err := json.Unmarshal(body, &request); err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension request must be JSON")
				return
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
				request, err = applyExtensionPending(request, pending, endpoint)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
					return
				}
			}
			originalModel := request["model"]
			for _, item := range state.items {
				if hasPending {
					break
				}
				if !hasExtensionHook(item, "onRequest") {
					continue
				}
				request, err = extensionMapHook(r.Context(), item, "onRequest", meta, request)
				if err != nil && !item.Manifest.ContinueOnError {
					swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
					return
				}
			}
			if request["model"] != originalModel {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension cannot change routed model")
				return
			}
			if stream, _ := request["stream"].(bool); stream != data.Streaming {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension cannot change stream mode")
				return
			}
			if endpoint == "anthropic.messages" {
				for _, item := range state.items {
					if err := injectAnthropicTools(request, item, state.owners); err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
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
					next.ServeHTTP(w, roundRequest)
					pendingComplete = true
					return
				}
				pendingComplete = serveExtensionStream(s, w, r, next, cfg, state, request)
				return
			}
			var usage map[string]any
			for round := 0; round < cfg.Extensions.EffectiveMaxToolRounds(); round++ {
				prepared, err := json.Marshal(request)
				if err != nil {
					swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
					return
				}
				roundRequest := r.Clone(context.WithValue(r.Context(), extensionContextKey{}, state))
				setExtensionBody(roundRequest, prepared)
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
				if err := json.Unmarshal(buffer.body.Bytes(), &response); err != nil {
					swaputil.SendResponse(w, r, http.StatusBadGateway, "extension upstream response is invalid JSON")
					return
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
				if len(serverCalls) == 0 {
					for i := len(state.items) - 1; i >= 0; i-- {
						item := state.items[i]
						if !hasExtensionHook(item, "onResponse") {
							continue
						}
						response, err = extensionMapHook(r.Context(), item, "onResponse", meta, response)
						if err != nil && !item.Manifest.ContinueOnError {
							swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
							return
						}
					}
					final, err := json.Marshal(response)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
						return
					}
					copyExtensionResponse(w, buffer, final)
					pendingComplete = true
					return
				}
				if len(clientCalls) != 0 {
					publicCalls, saveErr := s.saveExtensionMixed(r.Context(), state, request, response, serverCalls, clientCalls, endpoint)
					if saveErr != nil {
						swaputil.SendResponse(w, r, http.StatusBadGateway, saveErr.Error())
						return
					}
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
				if round+1 >= cfg.Extensions.EffectiveMaxToolRounds() {
					swaputil.SendResponse(w, r, http.StatusBadGateway, "extension tool round limit reached")
					return
				}
				if err := extensionContinue(r.Context(), state, request, response, serverCalls, endpoint); err != nil {
					swaputil.SendResponse(w, r, http.StatusBadGateway, err.Error())
					return
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
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension request must be JSON")
				return
			}
			chatDialect := state.meta.Endpoint == "chat.completions" || r.Context().Value(responsesChatBackendPathKey{}) != nil
			if state.meta.Endpoint != "anthropic.messages" {
				state.owners = map[string]*extensions.Compiled{}
			}
			forwardModel, forwardStream := request["model"], request["stream"]
			capture := debugCaptureFrom(r.Context())
			for _, item := range state.items {
				if state.meta.Endpoint != "anthropic.messages" {
					toolsBefore := extensionToolNames(request["tools"])
					if err := injectExtensionTools(request, item, chatDialect, state.owners); err != nil {
						swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
						return
					}
					if capture != nil {
						capture.recordInjectedTools(addedExtensionTools(toolsBefore, extensionToolNames(request["tools"])))
					}
				}
				if hasExtensionHook(item, "onBeforeForward") {
					forwardTools, _ := json.Marshal(request["tools"])
					request, err = extensionMapHook(r.Context(), item, "onBeforeForward", state.meta, request)
					if err != nil && !item.Manifest.ContinueOnError {
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
			if request["model"] != forwardModel || request["stream"] != forwardStream {
				swaputil.SendResponse(w, r, http.StatusBadRequest, "extension cannot change routed model or stream mode")
				return
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
				return
			}
			setExtensionBody(r, encoded)
			next.ServeHTTP(w, r)
		})
	}
}

func setExtensionBody(r *http.Request, body []byte) {
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.Header.Set("Content-Length", strconv.Itoa(len(body)))
	r.Header.Del("Transfer-Encoding")
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

func injectExtensionTools(request map[string]any, item *extensions.Compiled, chat bool, owners map[string]*extensions.Compiled) error {
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

func injectAnthropicTools(request map[string]any, item *extensions.Compiled, owners map[string]*extensions.Compiled) error {
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
}

func classifyExtensionCalls(ctx context.Context, state *extensionRequestState, calls []extensionCall) ([]extensionCall, []extensionCall, error) {
	serverCalls := make([]extensionCall, 0, len(calls))
	clientCalls := make([]extensionCall, 0, len(calls))
	for _, call := range calls {
		if owner := state.owners[call.Name]; owner != nil {
			call.Owner = owner
			serverCalls = append(serverCalls, call)
			continue
		}
		intercepted := false
		for _, item := range state.items {
			if !item.Manifest.InterceptClientTools || !hasExtensionHook(item, "onToolCall") {
				continue
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
				if item.Manifest.ContinueOnError {
					continue
				}
				return nil, nil, err
			}
			handled, _ := result["handled"].(bool)
			if !handled {
				continue
			}
			if _, ok := result["content"]; !ok {
				return nil, nil, fmt.Errorf("extension %s intercepted tool %s without content", item.Manifest.ID, call.Name)
			}
			call.Owner = item
			call.Precomputed = result
			serverCalls = append(serverCalls, call)
			intercepted = true
			break
		}
		if !intercepted {
			clientCalls = append(clientCalls, call)
		}
	}
	return serverCalls, clientCalls, nil
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
	if endpoint == "chat.completions" {
		choices, _ := response["choices"].([]any)
		if len(choices) == 0 {
			return result
		}
		choice, _ := choices[0].(map[string]any)
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

func extensionContinue(ctx context.Context, state *extensionRequestState, request, response map[string]any, calls []extensionCall, endpoint string) error {
	var results []any
	for _, call := range calls {
		var args any
		if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
			return fmt.Errorf("tool %s has invalid arguments: %w", call.Name, err)
		}
		input := map[string]any{"id": call.ID, "name": call.Name, "arguments": args}
		output := call.Precomputed
		var err error
		if output == nil {
			output, err = extensionMapHook(ctx, call.Owner, "onToolCall", state.meta, input)
		}
		if err != nil {
			if !call.Owner.Manifest.ContinueOnError {
				return err
			}
			output = map[string]any{"content": "tool execution failed", "is_error": true}
		}
		for _, item := range state.items {
			if !hasExtensionHook(item, "onToolResult") {
				continue
			}
			output, err = extensionMapHook(ctx, item, "onToolResult", state.meta, output)
			if err != nil && !item.Manifest.ContinueOnError {
				return err
			}
		}
		content := output["content"]
		if content == nil {
			content = output
		}
		if endpoint == "chat.completions" {
			results = append(results, map[string]any{"role": "tool", "tool_call_id": call.ID, "content": content})
		} else if endpoint == "anthropic.messages" {
			if _, ok := content.(string); !ok {
				if _, ok := content.([]any); !ok {
					encoded, _ := json.Marshal(content)
					content = string(encoded)
				}
			}
			result := map[string]any{"type": "tool_result", "tool_use_id": call.ID, "content": content}
			if failed, _ := output["is_error"].(bool); failed {
				result["is_error"] = true
			}
			results = append(results, result)
		} else {
			results = append(results, map[string]any{"type": "function_call_output", "call_id": call.ID, "output": content})
		}
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
