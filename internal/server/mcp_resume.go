package server

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/conversation"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type mcpApprovalResume struct {
	Endpoint      string                         `json:"endpoint"`
	Model         string                         `json:"model"`
	Request       map[string]any                 `json:"request"`
	Response      map[string]any                 `json:"response"`
	Calls         []mcpResumeCall                `json:"calls"`
	Approvals     []string                       `json:"approvals"`
	Conversation  string                         `json:"conversation,omitempty"`
	Interactions  bool                           `json:"interactions,omitempty"`
	Version       int64                          `json:"conversation_version"`
	Profile       string                         `json:"profile"`
	Configuration string                         `json:"configuration"`
	Stream        bool                           `json:"stream"`
	ClientCalls   []mcpResumeCall                `json:"client_tool_calls,omitempty"`
	Native        map[string]conversation.Native `json:"native,omitempty"`
	Policy        map[string]mcpRequestPolicy    `json:"mcp_policy,omitempty"`
}
type mcpResumeCall struct {
	ID          string         `json:"id"`
	Name        string         `json:"name"`
	Arguments   string         `json:"arguments"`
	Approval    string         `json:"approval,omitempty"`
	Precomputed map[string]any `json:"precomputed,omitempty"`
}

type mcpPendingInputKey struct{}

func mcpConfigurationDigest(cfg config.Config) string {
	encoded, _ := json.Marshal(cfg.ProtocolRuntime.MCP)
	return fmt.Sprintf("%x", sha256.Sum256(encoded))
}

func prepareMCPApprovals(ctx context.Context, state *extensionRequestState, request, response map[string]any, calls []extensionCall, endpoint string) error {
	needs := false
	pending, _ := ctx.Value(mcpPendingInputKey{}).(map[string]string)
	for _, call := range calls {
		if pending[call.ID] != "" || call.Owner != nil && call.Owner.NativeApproval && call.Precomputed == nil {
			needs = true
		}
	}
	if !needs {
		return nil
	}
	resume := mcpApprovalResume{Endpoint: endpoint, Model: state.meta.ResolvedModel, Request: request, Response: response}
	resume.Policy, _ = ctx.Value(mcpPolicyKey{}).(map[string]mcpRequestPolicy)
	serverIDs := map[string]bool{}
	for _, call := range calls {
		serverIDs[call.ID] = true
	}
	for _, call := range extensionCalls(response, endpoint) {
		if !serverIDs[call.ID] {
			resume.ClientCalls = append(resume.ClientCalls, mcpResumeCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
	}
	if approvalRequestHasCredentials(request) {
		return errors.New("gateway approval continuation cannot retain inline MCP credentials; configure MCP credentials through environment references")
	}
	resume.Profile = state.meta.Profile
	resume.Stream = state.meta.Stream
	if record, ok := ctx.Value(conversationRecordKey{}).(store.Conversation); ok {
		resume.Version = record.Version
	}
	if stored, ok := request["store"].(bool); ok && !stored {
		return errors.New("MCP approval continuation cannot retain a store:false request")
	}
	resume.Interactions, _ = ctx.Value(interactionsToolClientKey{}).(bool)
	if host, ok := extensions.HostCallerFrom(ctx).(*extensionHost); ok && host != nil {
		resume.Conversation = host.request.Header.Get(conversationHeader)
	}
	var server *Server
	for _, call := range calls {
		saved := mcpResumeCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments, Precomputed: call.Precomputed}
		if id := pending[call.ID]; id != "" {
			saved.Approval = id
			resume.Approvals = append(resume.Approvals, id)
		} else if call.Owner != nil && call.Owner.NativeApproval && call.Precomputed == nil {
			var args any
			if err := json.Unmarshal([]byte(call.Arguments), &args); err != nil {
				return err
			}
			input, _ := json.Marshal(map[string]any{"id": call.ID, "name": call.Name, "arguments": args})
			_, _, err := call.Owner.Invoke(ctx, "onToolCall", state.meta, input)
			var approval *mcpApprovalRequired
			if !errors.As(err, &approval) {
				if err == nil {
					return errors.New("MCP approval handler executed before consent")
				}
				return err
			}
			saved.Approval = approval.ID
			resume.Approvals = append(resume.Approvals, approval.ID)
		}
		resume.Calls = append(resume.Calls, saved)
	}
	if host, ok := extensions.HostCallerFrom(ctx).(*extensionHost); ok && host != nil {
		server = host.server
	}
	if server == nil {
		return errors.New("MCP continuation host is unavailable")
	}
	resume.Configuration = mcpConfigurationDigest(server.currentConfig())
	if turn, ok := ctx.Value(conversationTurnKey{}).(*conversationTurn); ok {
		sidecar := conversation.State{}
		turn.mu.Lock()
		for _, native := range turn.latest {
			sidecar.Remember(native.Scope, native.Version, native.Request, native.Response)
		}
		turn.mu.Unlock()
		if err := server.compactConversationMedia(ctx, &sidecar); err != nil {
			return err
		}
		resume.Native = sidecar.Native
	}
	body, err := json.Marshal(resume)
	if err != nil {
		return err
	}
	for _, id := range resume.Approvals {
		if err := server.store.SetMCPApprovalResume(ctx, id, identityIDFromContext(ctx), body); err != nil {
			return err
		}
	}
	return &mcpApprovalRequired{ID: resume.Approvals[0], IDs: resume.Approvals, ClientCalls: resume.ClientCalls, InputRequired: len(pending) > 0}
}

func approvalRequestHasCredentials(request map[string]any) bool {
	var inspect func(any) bool
	inspect = func(value any) bool {
		switch value := value.(type) {
		case []any:
			for _, item := range value {
				if inspect(item) {
					return true
				}
			}
		case map[string]any:
			for _, key := range []string{"authorization", "authorization_token", "access_token", "auth_token", "headers"} {
				if raw, exists := value[key]; exists && raw != nil && raw != "" {
					return true
				}
			}
			for _, key := range []string{"server", "mcp_server", "mcp_servers", "remote_mcp"} {
				if inspect(value[key]) {
					return true
				}
			}
		}
		return false
	}
	if inspect(request["mcp_servers"]) {
		return true
	}
	tools, _ := request["tools"].([]any)
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["type"] == "mcp" || tool["type"] == "mcp_server" || tool["type"] == "remote_mcp" {
			if inspect(tool) {
				return true
			}
		}
	}
	return false
}

func (s *Server) handleMCPResume(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(s.mcpContext(r.Context()))
	r = r.WithContext(extensions.WithHostCaller(r.Context(), newExtensionHost(s, r)))
	var supplied struct {
		Results map[string]any `json:"tool_results"`
	}
	input, err := io.ReadAll(io.LimitReader(r.Body, (1<<20)+1))
	if err != nil || len(input) > 1<<20 || len(input) > 0 && json.Unmarshal(input, &supplied) != nil {
		swaputil.SendResponse(w, r, 400, "invalid MCP continuation input")
		return
	}
	owner := identityIDFromContext(r.Context())
	record, err := s.store.GetMCPApproval(r.Context(), r.PathValue("approval_id"), owner)
	if err != nil || !identityFromContext(r.Context()).HasModel(record.Model) {
		swaputil.SendResponse(w, r, 404, "MCP approval not found")
		return
	}
	var resume mcpApprovalResume
	if len(record.Resume) == 0 || json.Unmarshal(record.Resume, &resume) != nil || len(resume.Approvals) == 0 {
		swaputil.SendResponse(w, r, 409, "approval has no model continuation")
		return
	}
	r = r.WithContext(context.WithValue(r.Context(), mcpPolicyKey{}, resume.Policy))
	if resume.Configuration != mcpConfigurationDigest(s.currentConfig()) || resume.Profile != s.ActiveProfile() {
		swaputil.SendResponse(w, r, 409, "MCP continuation configuration or profile changed")
		return
	}
	knownClientIDs := map[string]bool{}
	for _, call := range resume.ClientCalls {
		knownClientIDs[call.ID] = true
		if _, ok := supplied.Results[call.ID]; !ok {
			swaputil.SendResponse(w, r, 409, "client tool results are required before continuation")
			return
		}
	}
	for id := range supplied.Results {
		if !knownClientIDs[id] {
			swaputil.SendResponse(w, r, 400, "unexpected client tool result")
			return
		}
	}
	if resume.Conversation != "" {
		conversation, err := s.store.GetConversation(r.Context(), resume.Conversation, owner)
		if err != nil || conversation.Version != resume.Version {
			swaputil.SendResponse(w, r, 409, "MCP continuation conversation version changed")
			return
		}
	}
	outputs := map[string]map[string]any{}
	for _, id := range resume.Approvals {
		approval, err := s.store.GetMCPApproval(r.Context(), id, owner)
		if err != nil || approval.State == "pending" || approval.State == "running" {
			swaputil.SendResponse(w, r, 409, "all MCP approvals must be resolved before continuation")
			return
		}
		if approval.State == "denied" {
			outputs[id] = map[string]any{"content": "MCP execution denied by caller", "is_error": true}
		} else if approval.State == "completed" {
			var result mcp.CallToolResult
			if json.Unmarshal(approval.Result, &result) != nil {
				swaputil.SendResponse(w, r, 500, "invalid saved MCP result")
				return
			}
			if result.NeedsInput() {
				protocolJSON(w, 409, map[string]any{"error": map[string]any{"type": "mcp_input_required", "approval_id": id, "input_requests": result.InputRequests, "input_url": "/api/mcp/approvals/" + id, "resume_url": "/api/mcp/approvals/" + id + "/resume"}})
				return
			}
			outputs[id] = mcpExtensionOutput(&result)
		} else {
			outputs[id] = map[string]any{"content": string(approval.Result), "is_error": true}
		}
	}
	cfg := s.currentConfig()
	meta := extensions.Context{Endpoint: resume.Endpoint, ResolvedModel: resume.Model}
	items, err := s.mcpExtensions(r.Context(), cfg, meta)
	if err != nil {
		swaputil.SendResponse(w, r, 502, err.Error())
		return
	}
	if manager := s.currentExtensions(); manager != nil && cfg.Extensions.Enabled {
		items = append(items, manager.Match(meta)...)
	}
	owners := map[string]*extensions.Compiled{}
	for _, item := range items {
		for _, tool := range item.Tools {
			var def struct {
				Name string `json:"name"`
			}
			_ = json.Unmarshal(tool.Function, &def)
			owners[def.Name] = item
		}
	}
	calls := []extensionCall{}
	for _, saved := range resume.Calls {
		call := extensionCall{ID: saved.ID, Name: saved.Name, Arguments: saved.Arguments, Owner: owners[saved.Name], Precomputed: saved.Precomputed}
		if saved.Approval != "" {
			call.Precomputed = outputs[saved.Approval]
		}
		if call.Precomputed == nil && call.Owner == nil {
			swaputil.SendResponse(w, r, 409, "continuation tool is no longer available")
			return
		}
		calls = append(calls, call)
	}
	for _, saved := range resume.ClientCalls {
		calls = append(calls, extensionCall{ID: saved.ID, Name: saved.Name, Arguments: saved.Arguments, Precomputed: map[string]any{"content": supplied.Results[saved.ID]}})
	}
	if err := s.store.ClaimMCPResume(r.Context(), resume.Approvals[0], owner); err != nil {
		swaputil.SendResponse(w, r, 409, "MCP continuation already consumed")
		return
	}
	defer func() {
		for _, id := range resume.Approvals {
			mcpruntime.ReleasePin(r.Context(), id)
		}
	}()
	state := &extensionRequestState{meta: meta, items: items, owners: owners}
	if err := extensionContinue(r.Context(), state, resume.Request, resume.Response, calls, resume.Endpoint); err != nil {
		swaputil.SendResponse(w, r, 502, err.Error())
		return
	}
	body, err := json.Marshal(resume.Request)
	if err != nil {
		swaputil.SendResponse(w, r, 500, err.Error())
		return
	}
	path := ""
	switch resume.Endpoint {
	case "responses":
		path = "/v1/responses"
	case "chat.completions":
		path = "/v1/chat/completions"
	case "anthropic.messages":
		path = "/v1/messages"
	case "gemini.generateContent":
		path = fmt.Sprintf("/v1beta/models/%s:generateContent", resume.Model)
		if resume.Stream {
			path = fmt.Sprintf("/v1beta/models/%s:streamGenerateContent", resume.Model)
		}
	default:
		swaputil.SendResponse(w, r, 409, "unsupported continuation endpoint")
		return
	}
	copy := r.Clone(context.WithValue(r.Context(), mcpNativeResumeKey{}, resume.Native))
	copy.Method = "POST"
	copy.URL.Path = path
	copy.URL.RawPath = ""
	copy.URL.RawQuery = ""
	if resume.Endpoint == "gemini.generateContent" && resume.Stream {
		copy.URL.RawQuery = "alt=sse"
	}
	copy.Header.Set("Content-Type", "application/json")
	if resume.Conversation != "" {
		copy.Header.Set(conversationHeader, resume.Conversation)
		copy.Header.Set(conversationVersionHeader, strconv.FormatInt(resume.Version, 10))
	}
	if err := setProtocolRequestBody(copy, body); err != nil {
		swaputil.SendResponse(w, r, 500, err.Error())
		return
	}
	if resume.Interactions {
		stream, _ := resume.Request["stream"].(bool)
		meta := relaykit.NewMeta(resume.Model, stream)
		writer, err := newProtocolResponseWriter(w, copy, nil, resume.Model, "", relaykit.OpenAIResponses, relaykit.GeminiInteractions, body, meta, stream)
		if err != nil {
			swaputil.SendResponse(w, r, 500, err.Error())
			return
		}
		s.ServeHTTP(writer, copy)
		if err := writer.finish(); err != nil {
			if writer.streamWriter != nil {
				writer.streamWriter.emitTerminalStreamError(err)
			} else if !writer.committed() {
				swaputil.SendResponse(w, r, 502, err.Error())
			}
		}
		return
	}
	s.ServeHTTP(w, copy)
}
