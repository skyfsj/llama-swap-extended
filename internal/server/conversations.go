package server

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

const conversationHeader = "X-Llama-Swap-Conversation-ID"
const conversationVersionHeader = "X-Llama-Swap-Conversation-Version"

type conversationRecordKey struct{}
type conversationTurnKey struct{}
type mcpNativeResumeKey struct{}

func protocolObjectID(prefix string) string { return prefix + rand.Text() }
func conversationError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrConversationNotFound) {
		status = http.StatusNotFound
	}
	if errors.Is(err, store.ErrConversationConflict) {
		status = http.StatusConflict
	}
	swaputil.SendResponse(w, r, status, err.Error())
}
func protocolJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func (s *Server) handleConversations(w http.ResponseWriter, r *http.Request) {
	owner := identityIDFromContext(r.Context())
	if r.Method == http.MethodGet {
		items, err := s.store.ListConversations(r.Context(), owner)
		if err != nil {
			conversationError(w, r, err)
			return
		}
		visible := items[:0]
		for _, item := range items {
			if identityFromContext(r.Context()).HasModel(item.Model) {
				visible = append(visible, item)
			}
		}
		protocolJSON(w, 200, map[string]any{"data": visible})
		return
	}
	var input struct {
		Model string `json:"model"`
	}
	if r.Body != nil && r.Body != http.NoBody {
		body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
		if err != nil || len(body) > 65536 || len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &input) != nil {
			conversationError(w, r, errors.New("invalid conversation request"))
			return
		}
	}
	if input.Model != "" && !identityFromContext(r.Context()).HasModel(input.Model) {
		swaputil.SendResponse(w, r, 403, "model is not allowed")
		return
	}
	state, _ := conversation.State{}.Encode()
	item := store.Conversation{ID: protocolObjectID("gconv_"), Owner: owner, Model: input.Model, State: state}
	if err := s.store.CreateConversation(r.Context(), item); err != nil {
		conversationError(w, r, err)
		return
	}
	created, _ := s.store.GetConversation(r.Context(), item.ID, owner)
	protocolJSON(w, 201, created)
}

func (s *Server) conversationForRequest(r *http.Request) (store.Conversation, error) {
	c, err := s.store.GetConversation(r.Context(), r.PathValue("conversation_id"), identityIDFromContext(r.Context()))
	if err == nil && !identityFromContext(r.Context()).HasModel(c.Model) {
		return c, store.ErrConversationNotFound
	}
	return c, err
}
func (s *Server) handleConversation(w http.ResponseWriter, r *http.Request) {
	c, err := s.conversationForRequest(r)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.store.DeleteConversation(r.Context(), c.ID, c.Owner); err != nil {
			conversationError(w, r, err)
			return
		}
		protocolJSON(w, 200, map[string]any{"id": c.ID, "deleted": true})
		return
	}
	state, err := conversation.Decode(c.State)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	lineage := []any{}
	for _, native := range state.Native {
		lineage = append(lineage, map[string]any{"scope": native.Scope, "version": native.Version, "current": native.Version == c.Version})
	}
	protocolJSON(w, 200, map[string]any{"conversation": c, "messages": state.Messages, "summary": state.Summary, "lineage": lineage, "parent": state.Parent, "parent_version": state.ParentVersion})
}

func (s *Server) handleConversationCheckpoint(w http.ResponseWriter, r *http.Request) {
	c, err := s.conversationForRequest(r)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	cp := store.ConversationCheckpoint{ID: protocolObjectID("gcp_"), ConversationID: c.ID, Owner: c.Owner, Version: c.Version}
	if err := s.store.PutConversationCheckpoint(r.Context(), cp); err != nil {
		conversationError(w, r, err)
		return
	}
	stored, err := s.store.GetConversationCheckpoint(r.Context(), cp.ID, c.Owner)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	protocolJSON(w, 201, stored)
}

func (s *Server) handleConversationFork(w http.ResponseWriter, r *http.Request) {
	c, err := s.conversationForRequest(r)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	var input struct {
		CheckpointID string `json:"checkpoint_id"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(body) > 65536 || len(bytes.TrimSpace(body)) > 0 && json.Unmarshal(body, &input) != nil {
		conversationError(w, r, errors.New("invalid branch request"))
		return
	}
	if input.CheckpointID != "" {
		cp, err := s.store.GetConversationCheckpoint(r.Context(), input.CheckpointID, c.Owner)
		if err != nil || cp.ConversationID != c.ID {
			conversationError(w, r, store.ErrConversationNotFound)
			return
		}
		c.State = cp.State
		c.Version = cp.Version
	}
	state, err := conversation.Decode(c.State)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	state.Parent = c.ID
	state.ParentVersion = c.Version
	for key, native := range state.Native {
		if native.Version == c.Version {
			native.Version = 0
		} else {
			native.Version = -1
		}
		state.Native[key] = native
	}
	c.State, err = state.Encode()
	if err != nil {
		conversationError(w, r, err)
		return
	}
	c.ID = protocolObjectID("gconv_")
	c.Version = 0
	if err := s.store.CreateConversation(r.Context(), c); err != nil {
		conversationError(w, r, err)
		return
	}
	created, _ := s.store.GetConversation(r.Context(), c.ID, c.Owner)
	protocolJSON(w, 201, created)
}

func (s *Server) handleConversationCompact(w http.ResponseWriter, r *http.Request) {
	c, err := s.conversationForRequest(r)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	var input struct {
		Summary string `json:"summary"`
		Retain  int    `json:"retain_messages"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(body) > 1<<20 || json.Unmarshal(body, &input) != nil || input.Retain < 0 {
		conversationError(w, r, errors.New("a non-negative retained message count is required"))
		return
	}
	cp := store.ConversationCheckpoint{ID: protocolObjectID("gcp_"), ConversationID: c.ID, Owner: c.Owner, Version: c.Version}
	if err := s.store.PutConversationCheckpoint(r.Context(), cp); err != nil {
		conversationError(w, r, err)
		return
	}
	token := protocolObjectID("lease_")
	if err := s.store.LeaseConversation(r.Context(), c.ID, c.Owner, c.Version, token, time.Now().Add(5*time.Minute)); err != nil {
		conversationError(w, r, err)
		return
	}
	defer s.store.ReleaseConversation(context.WithoutCancel(r.Context()), c.ID, c.Owner, token)
	state, err := conversation.Decode(c.State)
	if err != nil {
		conversationError(w, r, err)
		return
	}
	if strings.TrimSpace(input.Summary) == "" {
		input.Summary, err = s.summarizeConversation(r, c, state)
		if err != nil {
			conversationError(w, r, err)
			return
		}
	}
	state.Summary = input.Summary
	state.Native = nil
	if input.Retain == 0 {
		state.Messages = nil
	} else if input.Retain < len(state.Messages) {
		start := len(state.Messages) - input.Retain
		for start > 0 && state.Messages[start].Role != dialect.RoleUser {
			start--
		}
		state.Messages = state.Messages[start:]
	}
	c.State, err = state.Encode()
	if err == nil {
		err = s.store.CommitConversation(r.Context(), c, token)
	}
	if err != nil {
		conversationError(w, r, err)
		return
	}
	protocolJSON(w, 200, map[string]any{"id": c.ID, "version": c.Version + 1, "checkpoint_id": cp.ID})
}

func (s *Server) summarizeConversation(r *http.Request, c store.Conversation, state conversation.State) (string, error) {
	if c.Model == "" {
		return "", errors.New("a model is required for semantic compaction")
	}
	messages := append([]dialect.Message(nil), state.Messages...)
	for index := range messages {
		messages[index].Parts = conversationSummaryParts(messages[index].Parts)
	}
	messages = append(messages, dialect.Message{Role: dialect.RoleUser, Parts: []dialect.Part{{Kind: dialect.PartText, Text: "Summarize the conversation for continuation. Preserve user requirements, decisions, unresolved questions, tool call identifiers and outcomes, and file references. Do not invent facts. Return only the summary."}}})
	system := "Previous conversation summary:\n" + state.Summary
	request := &dialect.Request{Model: c.Model, Messages: messages, System: []dialect.Part{{Kind: dialect.PartText, Text: system}}}
	body, _, err := dialect.EncodeRequest(request, dialect.OpenAIChat, dialect.ConvertOptions{})
	if err != nil {
		return "", err
	}
	var raw map[string]any
	_ = json.Unmarshal(body, &raw)
	raw["tool_choice"] = "none"
	raw["store"] = false
	body, err = json.Marshal(raw)
	if err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	copy := r.Clone(context.WithValue(ctx, extensionSkipKey{}, true))
	copy.Method = "POST"
	copy.URL.Path = "/v1/chat/completions"
	copy.URL.RawPath = ""
	copy.URL.RawQuery = ""
	copy.Header.Del(conversationHeader)
	copy.Header.Del(conversationVersionHeader)
	copy.Header.Set("Content-Type", "application/json")
	setBackendRequestBody(copy, body)
	writer := newBufferedResponseWriter()
	s.ServeHTTP(writer, copy)
	if writer.status >= 400 || writer.overflow {
		return "", errors.New("semantic compaction generation failed")
	}
	response, _, err := dialect.DecodeResponse(dialect.OpenAIChat, writer.body.Bytes())
	if err != nil {
		return "", err
	}
	var summary strings.Builder
	for _, part := range response.Parts {
		if part.Kind == dialect.PartText {
			summary.WriteString(part.Text)
		}
	}
	if strings.TrimSpace(summary.String()) == "" || summary.Len() > 1<<20 {
		return "", errors.New("semantic compaction returned an invalid summary")
	}
	return summary.String(), nil
}

type conversationTurn struct {
	server   *Server
	record   store.Conversation
	state    conversation.State
	current  *dialect.Request
	format   dialect.Format
	stream   bool
	mu       sync.Mutex
	prepared map[string][]byte
	request  *dialect.Request
	response *dialect.Response
	latest   map[string]conversation.Native
}

func CreateConversationMiddleware(s *Server, cfg config.Config) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			route, ok := dialect.RouteForRequest(r.Method, r.URL.Path)
			if !ok {
				next.ServeHTTP(w, r)
				return
			}
			id := strings.TrimSpace(r.Header.Get(conversationHeader))
			if id == "" {
				if cfg.Extensions.Enabled || cfg.ProtocolRuntime.MCP.Enabled {
					data, _ := swaputil.ReadContext(r.Context())
					turn := &conversationTurn{server: s, format: route.Format, stream: data.Streaming, prepared: map[string][]byte{}, latest: map[string]conversation.Native{}}
					if saved, ok := r.Context().Value(mcpNativeResumeKey{}).(map[string]conversation.Native); ok {
						turn.latest = saved
					}
					r = r.WithContext(swaputil.WithUpstreamHooks(context.WithValue(r.Context(), conversationTurnKey{}, turn), swaputil.UpstreamHooks{Prepare: turn.prepare, Complete: turn.complete}))
				}
				next.ServeHTTP(w, r)
				return
			}
			data, err := swaputil.FetchContext(r, cfg)
			if err != nil {
				swaputil.SendError(w, r, err)
				return
			}
			c, err := s.store.GetConversation(r.Context(), id, identityIDFromContext(r.Context()))
			if err != nil {
				conversationError(w, r, err)
				return
			}
			identity := identityFromContext(r.Context())
			if !identity.HasModel(c.Model) || !identity.HasModel(data.ModelID) {
				swaputil.SendResponse(w, r, 403, "conversation model is not allowed")
				return
			}
			if value := r.Header.Get(conversationVersionHeader); value != "" {
				expected, err := strconv.ParseInt(value, 10, 64)
				if err != nil || expected != c.Version {
					conversationError(w, r, store.ErrConversationConflict)
					return
				}
			}
			body, err := readResponsesRequestBody(r)
			if err != nil {
				swaputil.SendResponse(w, r, 413, err.Error())
				return
			}
			setBackendRequestBody(r, body)
			var raw map[string]json.RawMessage
			if json.Unmarshal(body, &raw) != nil {
				conversationError(w, r, errors.New("invalid conversation JSON"))
				return
			}
			for _, key := range []string{"previous_response_id", "previous_interaction_id", "conversation"} {
				if value := bytes.TrimSpace(raw[key]); len(value) > 0 && !bytes.Equal(value, []byte("null")) {
					conversationError(w, r, fmt.Errorf("%s cannot be combined with %s", key, conversationHeader))
					return
				}
			}
			if bytes.Equal(raw["store"], []byte("false")) {
				conversationError(w, r, errors.New("a persistent gateway conversation cannot be used with store:false"))
				return
			}
			if bytes.Equal(bytes.TrimSpace(raw["background"]), []byte("true")) {
				if _, ok := r.Context().Value(nativeCheckpointKey{}).(*nativeCheckpointCapture); !ok {
					conversationError(w, r, errors.New("use /api/conversations/{id}/generations for background gateway checkpoints"))
					return
				}
			}
			current, _, err := dialect.DecodeRequest(route.Format, body)
			if err != nil {
				conversationError(w, r, err)
				return
			}
			current.Stream = dialect.RequestStreamed(route, body)
			if current.N != nil && *current.N > 1 {
				conversationError(w, r, errors.New("checkpoint conversations require a single response candidate"))
				return
			}
			state, err := conversation.Decode(c.State)
			if err != nil {
				conversationError(w, r, err)
				return
			}
			token := protocolObjectID("lease_")
			if err := s.store.LeaseConversation(r.Context(), c.ID, c.Owner, c.Version, token, time.Now().Add(5*time.Minute)); err != nil {
				conversationError(w, r, err)
				return
			}
			leaseCtx, cancel := context.WithCancel(r.Context())
			leaseDone := make(chan struct{})
			defer func() {
				cancel()
				<-leaseDone
				_ = s.store.ReleaseConversation(context.WithoutCancel(r.Context()), c.ID, c.Owner, token)
			}()
			go func(renewalCtx context.Context) {
				defer close(leaseDone)
				ticker := time.NewTicker(time.Minute)
				defer ticker.Stop()
				for {
					select {
					case <-renewalCtx.Done():
						return
					case <-ticker.C:
						if err := s.store.RenewConversation(renewalCtx, c.ID, c.Owner, token, time.Now().Add(5*time.Minute)); err != nil {
							cancel()
							return
						}
					}
				}
			}(leaseCtx)
			turn := &conversationTurn{server: s, record: c, state: state, current: current, format: route.Format, stream: current.Stream, prepared: map[string][]byte{}, latest: map[string]conversation.Native{}}
			if saved, ok := r.Context().Value(mcpNativeResumeKey{}).(map[string]conversation.Native); ok {
				turn.latest = saved
			}
			leaseCtx = context.WithValue(leaseCtx, conversationRecordKey{}, c)
			leaseCtx = context.WithValue(leaseCtx, conversationTurnKey{}, turn)
			r = r.WithContext(swaputil.WithUpstreamHooks(leaseCtx, swaputil.UpstreamHooks{Prepare: turn.prepare, Complete: turn.complete}))
			w.Header().Set(conversationHeader, c.ID)
			w.Header().Add("Trailer", conversationVersionHeader)
			w.Header().Add("Trailer", "X-Llama-Swap-Conversation-Status")
			writer := newConversationWriter(w, turn.stream, turn.format)
			next.ServeHTTP(writer, r)
			if err := writer.finish(func() error {
				return turn.commit(r.Context(), token, writer.body.Bytes(), writer.status, writer.truncated, data.ModelID)
			}); err != nil {
				if s.proxylog != nil {
					s.proxylog.Warnf("conversation %s did not commit: %v", c.ID, err)
				}
			} else if writer.status < 400 && !writer.failed {
				w.Header().Set(conversationVersionHeader, strconv.FormatInt(c.Version+1, 10))
			}
		})
	}
}

func (t *conversationTurn) prepare(ctx context.Context, scope conversation.Scope, r *http.Request) error {
	if _, ok := ctx.Value(nativeCheckpointKey{}).(*nativeCheckpointCapture); ok && (scope.Format != t.format || scope.Provider == "local") {
		return errors.New("native background checkpoint requires the original provider API surface")
	}
	if r.GetBody == nil {
		return errors.New("conversation request cannot be replayed")
	}
	body, err := r.GetBody()
	if err != nil {
		return err
	}
	actual, err := io.ReadAll(io.LimitReader(body, store.MaxConversationBytes+1))
	body.Close()
	if err != nil || len(actual) > store.MaxConversationBytes {
		return errors.New("conversation request is too large")
	}
	current, _, err := dialect.DecodeRequest(scope.Format, actual)
	if err != nil {
		return err
	}
	current.Stream = t.stream
	t.mu.Lock()
	defer t.mu.Unlock()
	state := t.state
	if t.server != nil {
		var err error
		state, err = t.server.hydrateConversationState(ctx, state)
		if err != nil {
			return err
		}
	}
	if latest, ok := t.latest[scope.Key()]; ok {
		fresh, err := nativeContinuationTail(scope.Format, current, actual, latest.Response)
		if err != nil {
			return err
		}
		actual = fresh
		state.Native = map[string]conversation.Native{}
		for key, native := range t.state.Native {
			state.Native[key] = native
		}
		latest.Version = t.record.Version
		state.Native[scope.Key()] = latest
	} else if len(t.latest) > 0 {
		state.Native = nil
	}
	if len(state.Messages) == 0 && len(state.Native) == 0 && state.Summary == "" {
		t.prepared[scope.Key()] = actual
		t.request = current
		r.Header.Del(conversationHeader)
		r.Header.Del(conversationVersionHeader)
		return nil
	}
	prepared, _, err := state.Prepare(scope, t.record.Version, current, actual)
	if err != nil {
		return err
	}
	replay, _, err := state.Replay(scope, t.record.Version, current, actual)
	if err != nil {
		return err
	}
	t.prepared[scope.Key()] = replay
	t.request = current
	setBackendRequestBody(r, prepared)
	r.Header.Del(conversationHeader)
	r.Header.Del(conversationVersionHeader)
	return nil
}

func nativeContinuationTail(format dialect.Format, current *dialect.Request, actual, nativeResponse []byte) ([]byte, error) {
	response, _, err := dialect.DecodeResponse(format, nativeResponse)
	if err != nil {
		return nil, err
	}
	ids := map[string]bool{}
	names := map[string]bool{}
	for _, part := range response.Parts {
		if part.Kind == dialect.PartToolCall {
			ids[part.ToolCallID] = true
			names[part.ToolName] = true
		}
	}
	last := -1
	for index, message := range current.Messages {
		if message.Role != dialect.RoleAssistant {
			continue
		}
		for _, part := range message.Parts {
			if part.Kind == dialect.PartToolCall && (part.ToolCallID != "" && ids[part.ToolCallID] || part.ToolCallID == "" && names[part.ToolName]) {
				last = index
			}
		}
	}
	if last < 0 {
		return nil, errors.New("native tool continuation no longer matches the completed upstream turn")
	}
	copy := *current
	copy.Messages = current.Messages[last+1:]
	encoded, _, err := dialect.EncodeRequest(&copy, format, dialect.ConvertOptions{})
	if err != nil {
		return nil, err
	}
	var raw, tail map[string]json.RawMessage
	if json.Unmarshal(actual, &raw) != nil || json.Unmarshal(encoded, &tail) != nil {
		return nil, errors.New("invalid native continuation request")
	}
	key := "messages"
	switch format {
	case dialect.Gemini:
		key = "contents"
	case dialect.OpenAIResponses, dialect.GeminiInteractions:
		key = "input"
	}
	raw[key] = tail[key]
	delete(raw, "previous_response_id")
	delete(raw, "previous_interaction_id")
	return json.Marshal(raw)
}

func (t *conversationTurn) complete(ctx context.Context, scope conversation.Scope, input, output []byte, headers http.Header, status int, truncated bool) {
	if status < 200 || status >= 300 || truncated {
		return
	}
	decoded, err := decompressBody(output, headers.Get("Content-Encoding"))
	if err != nil || len(decoded) > store.MaxConversationBytes {
		return
	}
	output = decoded
	response, err := decodeProtocolResponse(scope.Format, output)
	if err != nil || response.Status == "failed" {
		return
	}
	native, err := nativeProtocolResponse(scope.Format, output)
	if err != nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	replay := t.prepared[scope.Key()]
	if len(replay) == 0 {
		return
	}
	t.response = response
	t.latest[scope.Key()] = conversation.Native{Scope: scope, Version: t.record.Version + 1, Request: replay, Response: native}
}

func (t *conversationTurn) commit(ctx context.Context, token string, body []byte, status int, truncated bool, model string) error {
	if truncated {
		return errors.New("conversation response exceeded the checkpoint limit")
	}
	if status < 200 || status >= 300 {
		return errors.New("generation did not succeed")
	}
	response, err := decodeProtocolResponse(t.format, body)
	if err != nil {
		return err
	}
	if response.Status == "failed" {
		if response.Error != nil && response.Error.Message != "" {
			return errors.New(response.Error.Message)
		}
		return errors.New("generation failed before completion")
	}
	if response.Status == "queued" || response.Status == "in_progress" || response.Status == "running" {
		if capture, ok := ctx.Value(nativeCheckpointKey{}).(*nativeCheckpointCapture); ok {
			return t.captureNativeCheckpoint(ctx, capture, model)
		}
		return errors.New("generation is pending and cannot be committed as a checkpoint turn")
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	current := t.current
	if t.request != nil {
		current = t.request
	}
	if t.response != nil {
		response = t.response
	}
	t.state.Append(current, response)
	for _, native := range t.latest {
		t.state.Remember(native.Scope, native.Version, native.Request, native.Response)
	}
	if err := t.server.compactConversationMedia(ctx, &t.state); err != nil {
		return err
	}
	state, err := t.state.Encode()
	if err != nil {
		return err
	}
	t.record.State = state
	t.record.Model = model
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), affinityWriteTimeout)
	defer cancel()
	return t.server.store.CommitConversation(writeCtx, t.record, token)
}

func decodeProtocolResponse(format dialect.Format, body []byte) (*dialect.Response, error) {
	if json.Valid(bytes.TrimSpace(body)) {
		r, _, err := dialect.DecodeResponse(format, body)
		return r, err
	}
	r, _, err := dialect.DecodeStream(format, body)
	return r, err
}

func nativeProtocolResponse(format dialect.Format, body []byte) ([]byte, error) {
	if json.Valid(bytes.TrimSpace(body)) {
		return append([]byte(nil), body...), nil
	}
	return nativeStreamResponse(format, body)
}
