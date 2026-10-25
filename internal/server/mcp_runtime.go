package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type mcpApprovalRequired struct {
	ID            string
	IDs           []string
	ClientCalls   []mcpResumeCall
	InputRequired bool
}

func (s *Server) mcpContext(ctx context.Context) context.Context {
	s.mcpRuntimeMu.Lock()
	if s.mcpRuntime == nil {
		parent := s.shutdownCtx
		if parent == nil {
			parent = context.Background()
		}
		s.mcpRuntime = mcpruntime.NewManager(parent)
	}
	manager := s.mcpRuntime
	s.mcpRuntimeMu.Unlock()
	return mcpruntime.WithManager(ctx, manager, identityIDFromContext(ctx))
}

func (s *Server) closeMCPRuntime() {
	s.closeConversationBackground()
	s.closeMCPBridges()
	s.mcpRuntimeMu.Lock()
	manager := s.mcpRuntime
	s.mcpRuntimeMu.Unlock()
	if manager != nil {
		manager.Close()
	}
}

func (e *mcpApprovalRequired) Error() string {
	if e.InputRequired {
		return "MCP client input required: " + e.ID
	}
	return "MCP approval required: " + e.ID
}

func (s *Server) callMCP(ctx context.Context, b mcpruntime.Binding, args any, model string) (*mcp.CallToolResult, error) {
	ctx = s.mcpContext(ctx)
	if b.Config.Approval != "never" || b.RequireApproval {
		if s.store == nil {
			return nil, errors.New("MCP approvals require storage")
		}
		b.CredentialVersion = mcpruntime.ScopeKey(ctx, b.Config)
		b.ConfigurationVersion = mcpConfigurationDigest(s.currentConfig())
		binding, err := json.Marshal(b)
		if err != nil {
			return nil, err
		}
		arguments, err := json.Marshal(args)
		if err != nil {
			return nil, err
		}
		a := store.MCPApproval{ID: protocolObjectID("gapproval_"), Owner: identityIDFromContext(ctx), Model: model, Binding: binding, Arguments: arguments, ExpiresAt: time.Now().Add(15 * time.Minute)}
		if err := s.pinMCPBinding(ctx, b, a.ID, a.ExpiresAt); err != nil {
			return nil, err
		}
		if err := s.store.CreateMCPApproval(ctx, a); err != nil {
			mcpruntime.ReleasePin(ctx, a.ID)
			return nil, err
		}
		return nil, &mcpApprovalRequired{ID: a.ID}
	}
	return b.Call(ctx, args)
}

func (s *Server) handleMCPApproval(w http.ResponseWriter, r *http.Request) {
	r = r.WithContext(s.mcpContext(r.Context()))
	a, err := s.store.GetMCPApproval(r.Context(), r.PathValue("approval_id"), identityIDFromContext(r.Context()))
	if err != nil || !identityFromContext(r.Context()).HasModel(a.Model) {
		swaputil.SendResponse(w, r, 404, "MCP approval not found")
		return
	}
	if r.Method == http.MethodGet {
		var binding mcpruntime.Binding
		_ = json.Unmarshal(a.Binding, &binding)
		var args any
		_ = json.Unmarshal(a.Arguments, &args)
		var result any
		if len(a.Result) > 0 {
			_ = json.Unmarshal(a.Result, &result)
		}
		var resume mcpApprovalResume
		_ = json.Unmarshal(a.Resume, &resume)
		protocolJSON(w, 200, map[string]any{"id": a.ID, "model": a.Model, "server": binding.Server, "tool": binding.Tool.Name, "arguments": args, "state": a.State, "expires_at": a.ExpiresAt, "result": result, "client_tool_calls": resume.ClientCalls})
		return
	}
	var input struct {
		Approve        *bool                `json:"approve"`
		InputResponses mcp.InputResponseMap `json:"input_responses"`
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 65537))
	if err != nil || len(body) > 65536 || json.Unmarshal(body, &input) != nil || input.Approve == nil && len(input.InputResponses) == 0 {
		swaputil.SendResponse(w, r, 400, "approve boolean is required")
		return
	}
	var b mcpruntime.Binding
	if json.Unmarshal(a.Binding, &b) != nil {
		swaputil.SendResponse(w, r, 500, "invalid approval record")
		return
	}
	current, ok := s.currentConfig().ProtocolRuntime.MCP.Servers[b.Server]
	oldConfig, _ := json.Marshal(b.Config)
	currentConfig, _ := json.Marshal(current)
	if !ok || string(oldConfig) != string(currentConfig) || !s.currentConfig().ProtocolRuntime.MCP.Enabled {
		swaputil.SendResponse(w, r, 409, "MCP server configuration changed")
		return
	}
	if b.CredentialVersion != mcpruntime.ScopeKey(r.Context(), current) {
		swaputil.SendResponse(w, r, 409, "MCP approval credential version changed")
		return
	}
	if b.ConfigurationVersion != mcpConfigurationDigest(s.currentConfig()) {
		swaputil.SendResponse(w, r, 409, "MCP approval runtime configuration changed")
		return
	}
	var waiting mcp.CallToolResult
	needsInput := a.State == "completed" && json.Unmarshal(a.Result, &waiting) == nil && waiting.NeedsInput()
	if needsInput {
		if err := validateMCPInput(waiting.InputRequests, input.InputResponses); err != nil {
			swaputil.SendResponse(w, r, 400, err.Error())
			return
		}
		b.Continuation = &mcp.CallToolParams{RequestState: waiting.RequestState, InputResponses: input.InputResponses}
		err = s.store.ClaimMCPInput(r.Context(), a.ID, a.Owner, a.Result)
	} else if input.Approve != nil && len(input.InputResponses) == 0 {
		err = s.store.ClaimMCPApproval(r.Context(), a.ID, a.Owner, *input.Approve)
	} else {
		swaputil.SendResponse(w, r, 409, "MCP call is not awaiting input")
		return
	}
	if err != nil {
		swaputil.SendResponse(w, r, 409, "MCP approval already resolved")
		return
	}
	if !needsInput && !*input.Approve {
		mcpruntime.ReleasePin(r.Context(), a.ID)
		protocolJSON(w, 200, map[string]any{"id": a.ID, "state": "denied"})
		return
	}
	var args any
	_ = json.Unmarshal(a.Arguments, &args)
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	result, callErr := s.executeMCPBinding(ctx, b, args)
	if len(a.Resume) == 0 && (callErr != nil || result == nil || !result.NeedsInput()) {
		defer mcpruntime.ReleasePin(r.Context(), a.ID)
	}
	state := "completed"
	var encoded []byte
	if callErr != nil {
		state = "failed"
		encoded, _ = json.Marshal(map[string]any{"error": callErr.Error()})
	} else {
		encoded, err = json.Marshal(result)
		if err != nil {
			state = "failed"
			encoded = []byte(`{"error":"MCP result encoding failed"}`)
		}
	}
	persistCtx, stop := context.WithTimeout(context.WithoutCancel(r.Context()), 5*time.Second)
	defer stop()
	if err := s.store.FinishMCPApproval(persistCtx, a.ID, a.Owner, state, encoded); err != nil {
		swaputil.SendResponse(w, r, 500, "MCP outcome persistence failed; execution must not be repeated")
		return
	}
	var value any
	_ = json.Unmarshal(encoded, &value)
	protocolJSON(w, 200, map[string]any{"id": a.ID, "state": state, "result": value})
}

func (s *Server) mcpExtensions(ctx context.Context, cfg config.Config, meta extensions.Context) ([]*extensions.Compiled, error) {
	if !cfg.ProtocolRuntime.MCP.Enabled {
		return nil, nil
	}
	ctx = s.mcpContext(ctx)
	bindings, err := mcpruntime.Discover(ctx, cfg.ProtocolRuntime.MCP, meta.ResolvedModel)
	if err != nil {
		return nil, err
	}
	items := make([]*extensions.Compiled, 0, len(bindings))
	for _, binding := range bindings {
		b := binding
		if !applyMCPRequestPolicy(ctx, &b) {
			continue
		}
		definition, _ := json.Marshal(map[string]any{"name": b.Name, "description": b.Tool.Description, "parameters": b.Tool.InputSchema})
		item := &extensions.Compiled{NativeApproval: b.Config.Approval != "never" || b.RequireApproval, Manifest: extensions.Manifest{ID: "mcp-" + b.Name, Name: b.Tool.Name, Enabled: true, FailOnToolError: true}, Hooks: []string{"onToolCall"}, Tools: []extensions.Tool{{Type: "function", Function: definition, Execution: "server"}}}
		item.NativeHook = func(ctx context.Context, hook string, meta extensions.Context, input json.RawMessage) (json.RawMessage, []string, error) {
			var call struct {
				Arguments any `json:"arguments"`
			}
			if err := json.Unmarshal(input, &call); err != nil {
				return nil, nil, err
			}
			result, err := s.callMCP(ctx, b, call.Arguments, meta.ResolvedModel)
			if err != nil {
				return nil, nil, err
			}
			if result.NeedsInput() {
				err := s.deferMCPInput(ctx, b, call.Arguments, meta.ResolvedModel, result)
				return nil, nil, err
			}
			encoded, err := json.Marshal(mcpExtensionOutput(result))
			return encoded, nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (s *Server) handleMCPRuntime(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if !cfg.ProtocolRuntime.MCP.Enabled {
		swaputil.SendResponse(w, r, 404, "MCP runtime is disabled")
		return
	}
	r = r.WithContext(s.mcpContext(r.Context()))
	model := r.URL.Query().Get("model")
	if model == "" || !identityFromContext(r.Context()).HasModel(model) {
		swaputil.SendResponse(w, r, 403, "an allowed model is required")
		return
	}
	if r.URL.Query().Get("live") == "true" {
		s.serveMCPLive(w, r, cfg, model)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	r = r.WithContext(ctx)
	server := mcp.NewServer(&mcp.Implementation{Name: "llama-swap-runtime", Version: "1"}, &mcp.ServerOptions{Capabilities: &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{}, Resources: &mcp.ResourceCapabilities{}, Prompts: &mcp.PromptCapabilities{}}})
	inventory := mcpruntime.Inventory{}
	bindings, err := mcpruntime.Discover(r.Context(), cfg.ProtocolRuntime.MCP, model)
	if err != nil {
		swaputil.SendResponse(w, r, 502, err.Error())
		return
	}
	for _, binding := range bindings {
		b := binding
		tool := *b.Tool
		tool.Name = b.Name
		if err := inventory.Add(&tool); err != nil {
			swaputil.SendResponse(w, r, 502, err.Error())
			return
		}
		if err := mcpruntime.RegisterTool(server, &tool, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			var args any
			if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
				return nil, err
			}
			if b.Config.Approval == "never" {
				return b.CallParams(ctx, &mcp.CallToolParams{Name: b.Tool.Name, Arguments: args, Meta: req.Params.Meta, InputResponses: req.Params.InputResponses, RequestState: req.Params.RequestState})
			}
			bound := b
			bound.Continuation = &mcp.CallToolParams{Name: b.Tool.Name, RequestState: req.Params.RequestState, InputResponses: req.Params.InputResponses}
			result, err := s.callMCP(ctx, bound, args, model)
			var approval *mcpApprovalRequired
			if errors.As(err, &approval) {
				data, _ := json.Marshal(map[string]any{"approval_id": approval.ID, "approval_url": "/api/mcp/approvals/" + approval.ID})
				return nil, &jsonrpc.Error{Code: -32001, Message: approval.Error(), Data: data}
			}
			return result, err
		}); err != nil {
			swaputil.SendResponse(w, r, 502, err.Error())
			return
		}
	}
	for id, remote := range cfg.ProtocolRuntime.MCP.Servers {
		if !mcpruntime.Allowed(remote.Models, model) {
			continue
		}
		session, err := mcpruntime.Connect(r.Context(), remote)
		if err != nil {
			swaputil.SendResponse(w, r, 502, err.Error())
			return
		}
		caps := session.InitializeResult().Capabilities
		inventory.Scope = id
		if caps.Resources != nil {
			for resource, err := range session.Resources(r.Context(), nil) {
				if err != nil {
					session.Close()
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if err := inventory.Add(resource); err != nil {
					session.Close()
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if !mcpruntime.Allowed(remote.Resources, resource.URI) {
					continue
				}
				original := resource.URI
				copy := *resource
				copy.URI = "mcp-resource://" + id + "/" + url.PathEscape(original)
				localURI := copy.URI
				server.AddResource(&copy, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
					ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					defer cancel()
					client, err := mcpruntime.Connect(ctx, remote)
					if err != nil {
						return nil, err
					}
					defer client.Close()
					params := *req.Params
					params.URI = original
					result, err := client.ReadResource(ctx, &params)
					if err == nil {
						for _, content := range result.Contents {
							if content == nil {
								return nil, errors.New("MCP resource returned null content")
							}
							if content.URI == original {
								content.URI = localURI
							}
						}
					}
					return result, err
				})
			}
			for template, err := range session.ResourceTemplates(r.Context(), nil) {
				if err != nil {
					session.Close()
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if err := inventory.Add(template); err != nil {
					session.Close()
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if !mcpruntime.Allowed(remote.Resources, template.URITemplate) {
					continue
				}
				prefix := "mcp-template://" + id + "/"
				copy := *template
				copy.URITemplate = prefix + template.URITemplate
				if err := mcpruntime.RegisterResourceTemplate(server, &copy, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
					ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					defer cancel()
					if !strings.HasPrefix(req.Params.URI, prefix) {
						return nil, errors.New("invalid resource template scope")
					}
					original := strings.TrimPrefix(req.Params.URI, prefix)
					client, err := mcpruntime.Connect(ctx, remote)
					if err != nil {
						return nil, err
					}
					defer client.Close()
					params := *req.Params
					params.URI = original
					result, err := client.ReadResource(ctx, &params)
					if err == nil {
						for _, content := range result.Contents {
							if content == nil {
								return nil, errors.New("MCP resource returned null content")
							}
							if content.URI == original {
								content.URI = req.Params.URI
							}
						}
					}
					return result, err
				}); err != nil {
					session.Close()
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
			}
		}
		if caps.Prompts != nil {
			for prompt, err := range session.Prompts(r.Context(), nil) {
				if err != nil {
					session.Close()
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if err := inventory.Add(prompt); err != nil {
					session.Close()
					swaputil.SendResponse(w, r, 502, err.Error())
					return
				}
				if !mcpruntime.Allowed(remote.Prompts, prompt.Name) {
					continue
				}
				original := prompt.Name
				copy := *prompt
				copy.Name = mcpruntime.BoundName(id, original)
				server.AddPrompt(&copy, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
					ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
					defer cancel()
					client, err := mcpruntime.Connect(ctx, remote)
					if err != nil {
						return nil, err
					}
					defer client.Close()
					params := *req.Params
					params.Name = original
					return client.GetPrompt(ctx, &params)
				})
			}
		}
		session.Close()
	}
	mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, JSONResponse: true, PropagateRequestCancellation: true}).ServeHTTP(w, r)
}

func mcpErrorResponse(w http.ResponseWriter, r *http.Request, err error) bool {
	var approval *mcpApprovalRequired
	if !errors.As(err, &approval) {
		return false
	}
	protocolJSON(w, 409, map[string]any{"error": mcpWaitDetails(approval, err)})
	return true
}

func mcpWaitDetails(approval *mcpApprovalRequired, err error) map[string]any {
	kind := "mcp_approval_required"
	if approval.InputRequired {
		kind = "mcp_input_required"
	}
	return map[string]any{"type": kind, "message": err.Error(), "approval_id": approval.ID, "approval_ids": approval.IDs, "client_tool_calls": approval.ClientCalls, "approval_url": fmt.Sprintf("/api/mcp/approvals/%s", approval.ID), "resume_url": fmt.Sprintf("/api/mcp/approvals/%s/resume", approval.ID)}
}

func mcpExtensionOutput(result *mcp.CallToolResult) map[string]any {
	return map[string]any{"content": map[string]any{"content": result.Content, "structuredContent": result.StructuredContent, "isError": result.IsError}, "mcp_result": result, "is_error": result.IsError}
}
