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
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
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

type extensionPendingPayload struct {
	BaseRequest map[string]any    `json:"baseRequest"`
	ClientIDs   map[string]string `json:"clientIds"`
}

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
			if !bytes.Contains(body, []byte("call_ls_")) {
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
			var record store.ExtensionPending
			for id := range ids {
				if !strings.HasPrefix(id, "call_ls_") {
					continue
				}
				found, lookupErr := s.store.LookupExtensionPending(r.Context(), id)
				if lookupErr != nil {
					swaputil.SendResponse(w, r, http.StatusConflict, lookupErr.Error())
					return
				}
				if record.ID != "" && found.ID != record.ID {
					swaputil.SendResponse(w, r, http.StatusConflict, "multiple pending extension turns in one request")
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
				if _, ok := ids[id]; !ok {
					swaputil.SendResponse(w, r, http.StatusConflict, "extension continuation is missing a client tool result")
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

func incomingExtensionResults(request map[string]any, endpoint string) map[string]any {
	result := map[string]any{}
	key := "messages"
	if endpoint == "responses" {
		key = "input"
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
		} else {
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

func applyExtensionPending(request map[string]any, pending extensionPendingContext, endpoint string) (map[string]any, error) {
	results := incomingExtensionResults(request, endpoint)
	base := pending.payload.BaseRequest
	if base == nil {
		return nil, errors.New("extension continuation has no base request")
	}
	if model, _ := request["model"].(string); model != "" {
		if saved, _ := base["model"].(string); saved != model {
			return nil, errors.New("extension continuation model changed")
		}
	}
	stream, _ := request["stream"].(bool)
	base["stream"] = stream
	key := "messages"
	if endpoint == "responses" {
		key = "input"
	}
	items, _ := base[key].([]any)
	publicIDs := make([]string, 0, len(pending.payload.ClientIDs))
	for id := range pending.payload.ClientIDs {
		publicIDs = append(publicIDs, id)
	}
	sort.Strings(publicIDs)
	var anthropicResults []any
	for _, publicID := range publicIDs {
		originalID := pending.payload.ClientIDs[publicID]
		output, ok := results[publicID]
		if !ok {
			return nil, fmt.Errorf("missing tool result %s", publicID)
		}
		if endpoint == "responses" {
			items = append(items, map[string]any{"type": "function_call_output", "call_id": originalID, "output": output})
		} else if endpoint == "anthropic.messages" {
			anthropicResults = append(anthropicResults, map[string]any{"type": "tool_result", "tool_use_id": originalID, "content": output})
		} else {
			items = append(items, map[string]any{"role": "tool", "tool_call_id": originalID, "content": output})
		}
	}
	if endpoint == "anthropic.messages" && len(anthropicResults) > 0 {
		if len(items) > 0 {
			last, _ := items[len(items)-1].(map[string]any)
			if last["role"] == "user" {
				blocks, _ := last["content"].([]any)
				last["content"] = append(blocks, anthropicResults...)
			} else {
				items = append(items, map[string]any{"role": "user", "content": anthropicResults})
			}
		}
	}
	base[key] = items
	delete(base, "previous_response_id")
	return base, nil
}

func (s *Server) saveExtensionMixed(ctx context.Context, state *extensionRequestState, request map[string]any, response map[string]any, serverCalls, clientCalls []extensionCall, endpoint string) ([]extensionCall, error) {
	if s.store == nil {
		return nil, errors.New("extension continuation store is unavailable")
	}
	if err := extensionContinue(ctx, state, request, response, serverCalls, endpoint); err != nil {
		return nil, err
	}
	mapping := map[string]string{}
	ids := make([]string, 0, len(clientCalls))
	publicCalls := make([]extensionCall, len(clientCalls))
	for i, call := range clientCalls {
		publicID, err := newExtensionCallID()
		if err != nil {
			return nil, err
		}
		mapping[publicID] = call.ID
		ids = append(ids, publicID)
		publicCalls[i] = call
		publicCalls[i].OriginalID = call.ID
		publicCalls[i].ID = publicID
	}
	payload, err := json.Marshal(extensionPendingPayload{BaseRequest: request, ClientIDs: mapping})
	if err != nil {
		return nil, err
	}
	if len(payload) > 2<<20 {
		return nil, errors.New("extension continuation exceeds 2 MiB")
	}
	token, err := newExtensionCallID()
	if err != nil {
		return nil, err
	}
	record := store.ExtensionPending{ID: token, IdentityID: identityFromContext(ctx).ID, ModelID: state.meta.ResolvedModel, Profile: state.meta.Profile, Endpoint: endpoint, Payload: payload, CallIDs: ids, ExpiresAt: time.Now().Add(time.Hour)}
	if err := s.store.SaveExtensionPending(ctx, record); err != nil {
		return nil, err
	}
	return publicCalls, nil
}

func exposeExtensionClientCalls(response map[string]any, clientCalls []extensionCall, endpoint string) {
	if endpoint == "anthropic.messages" {
		content, _ := response["content"].([]any)
		visible := make([]any, 0, len(content))
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if block["type"] != "tool_use" {
				visible = append(visible, raw)
				continue
			}
			for _, call := range clientCalls {
				if block["id"] == call.OriginalID {
					block["id"] = call.ID
					visible = append(visible, block)
					break
				}
			}
		}
		response["content"] = visible
		response["stop_reason"] = "tool_use"
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
