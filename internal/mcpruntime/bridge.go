package mcpruntime

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

type BridgeCall func(context.Context, Binding, *mcp.CallToolParams) (*mcp.CallToolResult, error)

type Bridge struct {
	Server        *mcp.Server
	ctx           context.Context
	cancel        context.CancelFunc
	gate          chan struct{}
	mu            sync.Mutex
	peer          *mcp.ServerSession
	actor         *bridgeActor
	actors        map[string]*bridgeActor
	subscribers   map[string]map[*mcp.ServerSession]bool
	clients       map[string]*Session
	configs       map[string]config.MCPServerConfig
	refresh       chan struct{}
	names         map[string]bool
	resources     map[string]bool
	templates     map[string]bool
	prompts       map[string]bool
	promptOrigins map[string]bridgeReference
	toolBindings  map[string]Binding
	taskRegistry  TaskRegistry
	failure       error
	call          BridgeCall
	namespace     bool
	done          chan struct{}
}

func NewBridge(ctx context.Context, configs map[string]config.MCPServerConfig, namespace bool, call BridgeCall, settings ...BridgeOption) (*Bridge, error) {
	ctx, cancel := context.WithCancel(ctx)
	b := &Bridge{ctx: ctx, cancel: cancel, gate: make(chan struct{}, 1), clients: map[string]*Session{}, configs: configs, refresh: make(chan struct{}, 1), names: map[string]bool{}, resources: map[string]bool{}, templates: map[string]bool{}, prompts: map[string]bool{}, call: call, done: make(chan struct{})}
	b.namespace = namespace
	b.actors = map[string]*bridgeActor{}
	b.subscribers = map[string]map[*mcp.ServerSession]bool{}
	b.promptOrigins = map[string]bridgeReference{}
	b.toolBindings = map[string]Binding{}
	b.taskRegistry = &memoryTaskRegistry{routes: map[string]memoryTaskRoute{}}
	for _, setting := range settings {
		setting(b)
	}
	capabilities := &mcp.ServerCapabilities{Tools: &mcp.ToolCapabilities{ListChanged: true}, Resources: &mcp.ResourceCapabilities{ListChanged: true}, Prompts: &mcp.PromptCapabilities{ListChanged: true}}
	_ = json.Unmarshal([]byte(`{"tools":{"listChanged":true},"resources":{"listChanged":true},"prompts":{"listChanged":true},"logging":{}}`), capabilities)
	capabilities.AddExtension(TasksExtension, map[string]any{})
	b.Server = mcp.NewServer(&mcp.Implementation{Name: "llama-swap-runtime", Version: "1"}, &mcp.ServerOptions{Capabilities: capabilities, SubscribeHandler: b.subscribe, UnsubscribeHandler: b.unsubscribe, CompletionHandler: b.complete})
	for _, method := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
		if err := mcp.AddReceivingCustomMethod(b.Server, method, func(ctx context.Context, peer *mcp.ServerSession, params *TaskParams) (*TaskReply, error) {
			req := &mcp.ServerRequest[*TaskParams]{Session: peer, Params: params}
			if !supportsTasks(req) {
				return nil, errors.New("MCP tasks extension capability is required")
			}
			var result *TaskReply
			err := b.withPeer(ctx, req.Session, func() error { var err error; result, err = b.taskOperation(ctx, method, req.Params); return err })
			return result, err
		}); err != nil {
			cancel()
			return nil, err
		}
	}
	for id, cfg := range configs {
		options := b.clientOptions(id)
		startup, stop := context.WithTimeout(ctx, 30*time.Second)
		session, err := connectSessionOptions(startup, cfg, ctx, options, b.receive)
		stop()
		if err != nil {
			cancel()
			for _, client := range b.clients {
				client.Close()
			}
			return nil, fmt.Errorf("MCP %s: %w", id, err)
		}
		b.clients[id] = session
	}
	if err := b.reload(ctx); err != nil {
		cancel()
		for _, client := range b.clients {
			client.Close()
		}
		return nil, err
	}
	b.Server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			b.mu.Lock()
			err := b.failure
			b.mu.Unlock()
			if err != nil && strings.HasSuffix(method, "/list") {
				return nil, err
			}
			if method == "tools/call" && supportsTasks(req) {
				call, ok := req.(*mcp.CallToolRequest)
				if !ok || call.Params == nil {
					return nil, errors.New("invalid MCP tool call")
				}
				b.mu.Lock()
				binding, ok := b.toolBindings[call.Params.Name]
				b.mu.Unlock()
				if !ok {
					return nil, errors.New("MCP tool is not allowed")
				}
				return b.taskTool(ctx, call, binding)
			}
			return next(ctx, method, req)
		}
	})
	go func() {
		defer close(b.done)
		for {
			select {
			case <-ctx.Done():
				return
			case <-b.refresh:
				err := b.reload(ctx)
				b.mu.Lock()
				b.failure = err
				b.mu.Unlock()
			}
		}
	}()
	return b, nil
}

func (b *Bridge) Close() {
	b.cancel()
	for _, client := range b.clients {
		client.Close()
	}
	<-b.done
}
func (b *Bridge) signal() {
	select {
	case b.refresh <- struct{}{}:
	default:
	}
}
func (b *Bridge) active() (*mcp.ServerSession, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.peer == nil {
		return nil, errors.New("MCP callback has no originating client request")
	}
	return b.peer, nil
}
func (b *Bridge) withPeer(ctx context.Context, peer *mcp.ServerSession, fn func() error) error {
	select {
	case b.gate <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	case <-b.ctx.Done():
		return b.ctx.Err()
	}
	b.mu.Lock()
	b.peer = peer
	b.actor, _ = ctx.Value(bridgeActorKey{}).(*bridgeActor)
	b.mu.Unlock()
	defer func() { b.mu.Lock(); b.peer = nil; b.actor = nil; b.mu.Unlock(); <-b.gate }()
	return fn()
}

func (b *Bridge) receive(next mcp.MethodHandler) mcp.MethodHandler {
	return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
		if method == "roots/list" {
			//lint:ignore SA1019 Translate a negotiated legacy roots callback into explicit client input.
			if response, handled, err := b.input(ctx, &mcp.ListRootsParams{}); handled {
				if err != nil {
					return nil, err
				}
				//lint:ignore SA1019 Validate the result of a negotiated legacy roots callback.
				result, ok := response.(*mcp.ListRootsResult)
				if !ok {
					return nil, errors.New("invalid roots response")
				}
				return result, nil
			}
			peer, err := b.active()
			if err != nil {
				return nil, err
			}
			//lint:ignore SA1019 Forward the negotiated legacy roots request to its originating client.
			return peer.ListRoots(ctx, &mcp.ListRootsParams{})
		}
		return next(ctx, method, req)
	}
}

func (b *Bridge) clientOptions(id string) *mcp.ClientOptions {
	options := &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}}
	_ = json.Unmarshal([]byte(`{"roots":{"listChanged":false},"sampling":{"tools":{}},"elicitation":{"form":{},"url":{}}}`), options.Capabilities)
	options.Capabilities.AddExtension(TasksExtension, map[string]any{})
	//lint:ignore SA1019 Forward legacy sampling without making an autonomous model request.
	options.CreateMessageWithToolsHandler = func(ctx context.Context, req *mcp.CreateMessageWithToolsRequest) (*mcp.CreateMessageWithToolsResult, error) {
		if response, handled, err := b.input(ctx, req.Params); handled {
			if err != nil {
				return nil, err
			}
			encoded, _ := json.Marshal(response)
			//lint:ignore SA1019 Translate an explicitly supplied legacy sampling result.
			var result mcp.CreateMessageWithToolsResult
			if err := json.Unmarshal(encoded, &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
		peer, err := b.active()
		if err != nil {
			return nil, err
		}
		//lint:ignore SA1019 Forward the negotiated legacy sampling request to its originating client.
		return peer.CreateMessageWithTools(ctx, req.Params)
	}
	options.ElicitationHandler = func(ctx context.Context, req *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		if response, handled, err := b.input(ctx, req.Params); handled {
			if err != nil {
				return nil, err
			}
			result, ok := response.(*mcp.ElicitResult)
			if !ok {
				return nil, errors.New("invalid elicitation response")
			}
			return result, nil
		}
		peer, err := b.active()
		if err != nil {
			return nil, err
		}
		return peer.Elicit(ctx, req.Params)
	}
	options.ToolListChangedHandler = func(context.Context, *mcp.ToolListChangedRequest) { b.signal() }
	options.ResourceListChangedHandler = func(context.Context, *mcp.ResourceListChangedRequest) { b.signal() }
	options.PromptListChangedHandler = func(context.Context, *mcp.PromptListChangedRequest) { b.signal() }
	options.ResourceUpdatedHandler = func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) {
		copy := *req.Params
		copy.URI = b.uri(id, copy.URI)
		_ = b.Server.ResourceUpdated(ctx, &copy)
	}
	options.ProgressNotificationHandler = func(ctx context.Context, req *mcp.ProgressNotificationClientRequest) {
		if peer, err := b.active(); err == nil {
			_ = peer.NotifyProgress(ctx, req.Params)
		}
	}
	options.ElicitationCompleteHandler = func(ctx context.Context, req *mcp.ElicitationCompleteNotificationRequest) {
		for peer := range b.Server.Sessions() {
			_ = peer.NotifyElicitationComplete(ctx, req.Params)
		}
	}
	//lint:ignore SA1019 Forward logging only for legacy peers during the protocol deprecation window.
	options.LoggingMessageHandler = func(ctx context.Context, req *mcp.LoggingMessageRequest) {
		for peer := range b.Server.Sessions() {
			//lint:ignore SA1019 Legacy logging is filtered by the downstream session's negotiated level.
			_ = peer.Log(ctx, req.Params)
		}
	}
	return options
}
func bridgeURI(id, uri string) string { return "mcp-resource://" + id + "/" + url.PathEscape(uri) }
func (b *Bridge) name(id, name string) string {
	if !b.namespace {
		return name
	}
	return BoundName(id, name)
}
func (b *Bridge) uri(id, uri string) string {
	if !b.namespace {
		return uri
	}
	return bridgeURI(id, uri)
}
func (b *Bridge) Call(ctx context.Context, id string, params *mcp.CallToolParams) (result *mcp.CallToolResult, err error) {
	client := b.clients[id]
	if client == nil {
		return nil, errors.New("MCP session is unavailable")
	}
	err = b.withPeer(ctx, nil, func() error { var err error; result, err = b.callSynchronous(ctx, id, params); return err })
	return
}
func (b *Bridge) resourceScope(uri string) (string, string, error) {
	if !b.namespace {
		for id := range b.clients {
			return id, uri, nil
		}
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", "", err
	}
	if u.Scheme == "mcp-template" {
		if b.clients[u.Host] == nil {
			return "", "", errors.New("unknown MCP resource scope")
		}
		return u.Host, strings.TrimPrefix(uri, "mcp-template://"+u.Host+"/"), nil
	}
	if u.Scheme != "mcp-resource" || b.clients[u.Host] == nil {
		return "", "", errors.New("unknown MCP resource scope")
	}
	original, err := url.PathUnescape(strings.TrimPrefix(u.EscapedPath(), "/"))
	return u.Host, original, err
}
func (b *Bridge) subscribe(ctx context.Context, req *mcp.SubscribeRequest) error {
	return b.subscription(ctx, req.Session, req.Params.URI, true)
}
func (b *Bridge) unsubscribe(ctx context.Context, req *mcp.UnsubscribeRequest) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	return b.subscription(ctx, req.Session, req.Params.URI, false)
}
func (b *Bridge) subscription(ctx context.Context, peer *mcp.ServerSession, uri string, subscribe bool) error {
	id, original, err := b.resourceScope(uri)
	if err != nil {
		return err
	}
	cfg := b.configs[id]
	if !Allowed(cfg.Resources, original) {
		return errors.New("MCP resource subscription is not allowed")
	}
	client := b.clients[id]
	if client.InitializeResult().Capabilities.Resources == nil || !client.InitializeResult().Capabilities.Resources.Subscribe {
		return errors.New("MCP server does not support resource subscriptions")
	}
	return b.withPeer(ctx, peer, func() error {
		if subscribe {
			if b.subscribers[uri][peer] {
				return nil
			}
			if len(b.subscribers[uri]) == 0 {
				if err := client.Subscribe(ctx, &mcp.SubscribeParams{URI: original}); err != nil {
					return err
				}
				b.subscribers[uri] = map[*mcp.ServerSession]bool{}
			}
			b.subscribers[uri][peer] = true
			return nil
		}
		if !b.subscribers[uri][peer] {
			return nil
		}
		if len(b.subscribers[uri]) == 1 {
			if err := client.Unsubscribe(ctx, &mcp.UnsubscribeParams{URI: original}); err != nil {
				return err
			}
		}
		delete(b.subscribers[uri], peer)
		if len(b.subscribers[uri]) == 0 {
			delete(b.subscribers, uri)
		}
		return nil
	})
}

func (b *Bridge) reload(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	return b.withPeer(ctx, nil, func() error {
		inventory := Inventory{}
		names, resources, templates, prompts := map[string]bool{}, map[string]bool{}, map[string]bool{}, map[string]bool{}
		toolBindings := map[string]Binding{}
		promptOrigins := map[string]bridgeReference{}
		for id, client := range b.clients {
			cfg := b.configs[id]
			caps := client.InitializeResult().Capabilities
			inventory.Scope = id
			if caps.Tools != nil {
				for tool, err := range client.Tools(ctx, nil) {
					if err != nil {
						return err
					}
					if err := inventory.Add(tool); err != nil {
						return err
					}
					if !Allowed(cfg.Tools, tool.Name) {
						continue
					}
					binding := Binding{Server: id, Config: cfg, Tool: tool, Name: b.name(id, tool.Name)}
					toolBindings[binding.Name] = binding
					copy := *tool
					copy.Name = binding.Name
					names[copy.Name] = true
					if err := RegisterTool(b.Server, &copy, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						return bridgeInvoke(b, ctx, req, "tools/call", id, req.Params.RequestState, req.Params.InputResponses, func(ctx context.Context) (*mcp.CallToolResult, error) {
							var result *mcp.CallToolResult
							var args any
							if err := json.Unmarshal(req.Params.Arguments, &args); err != nil {
								return nil, err
							}
							params := &mcp.CallToolParams{Name: binding.Tool.Name, Arguments: args, Meta: req.Params.Meta, RequestState: req.Params.RequestState, InputResponses: req.Params.InputResponses}
							var err error
							if b.call != nil {
								result, err = b.call(ctx, binding, params)
							}
							if result == nil && err == nil {
								result, err = b.callSynchronous(ctx, id, params)
							}
							return result, err
						})
					}); err != nil {
						return err
					}
				}
			}
			read := func(original string, req *mcp.ReadResourceRequest, ctx context.Context) (*mcp.ReadResourceResult, error) {
				return bridgeInvoke(b, ctx, req, "resources/read", id, req.Params.RequestState, req.Params.InputResponses, func(ctx context.Context) (*mcp.ReadResourceResult, error) {
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
				})
			}
			if caps.Resources != nil {
				for resource, err := range client.Resources(ctx, nil) {
					if err != nil {
						return err
					}
					if err := inventory.Add(resource); err != nil {
						return err
					}
					if !Allowed(cfg.Resources, resource.URI) {
						continue
					}
					copy := *resource
					original := resource.URI
					copy.URI = b.uri(id, original)
					resources[copy.URI] = true
					b.Server.AddResource(&copy, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
						return read(original, req, ctx)
					})
				}
				for template, err := range client.ResourceTemplates(ctx, nil) {
					if err != nil {
						return err
					}
					if err := inventory.Add(template); err != nil {
						return err
					}
					if !Allowed(cfg.Resources, template.URITemplate) {
						continue
					}
					copy := *template
					prefix := ""
					if b.namespace {
						prefix = "mcp-template://" + id + "/"
					}
					copy.URITemplate = prefix + copy.URITemplate
					templates[copy.URITemplate] = true
					if err := RegisterResourceTemplate(b.Server, &copy, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
						return read(strings.TrimPrefix(req.Params.URI, prefix), req, ctx)
					}); err != nil {
						return err
					}
				}
			}
			if caps.Prompts != nil {
				for prompt, err := range client.Prompts(ctx, nil) {
					if err != nil {
						return err
					}
					if err := inventory.Add(prompt); err != nil {
						return err
					}
					if !Allowed(cfg.Prompts, prompt.Name) {
						continue
					}
					copy := *prompt
					original := prompt.Name
					copy.Name = b.name(id, original)
					prompts[copy.Name] = true
					promptOrigins[copy.Name] = bridgeReference{server: id, original: original}
					b.Server.AddPrompt(&copy, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
						return bridgeInvoke(b, ctx, req, "prompts/get", id, req.Params.RequestState, req.Params.InputResponses, func(ctx context.Context) (*mcp.GetPromptResult, error) {
							params := *req.Params
							params.Name = original
							return client.GetPrompt(ctx, &params)
						})
					})
				}
			}
		}
		for name := range b.names {
			if !names[name] {
				b.Server.RemoveTools(name)
			}
		}
		for uri := range b.resources {
			if !resources[uri] {
				b.Server.RemoveResources(uri)
			}
		}
		for uri := range b.templates {
			if !templates[uri] {
				b.Server.RemoveResourceTemplates(uri)
			}
		}
		for name := range b.prompts {
			if !prompts[name] {
				b.Server.RemovePrompts(name)
			}
		}
		b.names, b.resources, b.templates, b.prompts = names, resources, templates, prompts
		b.promptOrigins = promptOrigins
		b.mu.Lock()
		b.toolBindings = toolBindings
		b.mu.Unlock()
		return nil
	})
}
