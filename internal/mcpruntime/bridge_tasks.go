package mcpruntime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type TaskRegistry interface {
	Save(context.Context, string, []byte, time.Time) error
	Load(context.Context, string) ([]byte, error)
}
type BridgeOption func(*Bridge)

func WithTaskRegistry(registry TaskRegistry) BridgeOption {
	return func(b *Bridge) { b.taskRegistry = registry }
}

type taskRoute struct {
	Server    string          `json:"server"`
	RemoteID  string          `json:"remote_id"`
	Scope     string          `json:"scope"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}
type memoryTaskRegistry struct {
	mu     sync.Mutex
	routes map[string]memoryTaskRoute
}
type memoryTaskRoute struct {
	data    []byte
	expires time.Time
}

func (r *memoryTaskRegistry) Save(ctx context.Context, id string, data []byte, expires time.Time) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, route := range r.routes {
		if time.Now().After(route.expires) {
			delete(r.routes, key)
		}
	}
	if len(r.routes) >= 1024 {
		return errors.New("MCP task route capacity exceeded")
	}
	r.routes[id] = memoryTaskRoute{append([]byte(nil), data...), expires}
	return nil
}
func (r *memoryTaskRegistry) Load(ctx context.Context, id string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	route, ok := r.routes[id]
	if !ok || time.Now().After(route.expires) {
		return nil, errors.New("MCP task route not found")
	}
	return append([]byte(nil), route.data...), nil
}

func supportsTasks(req mcp.Request) bool {
	meta := req.GetParams().GetMeta()
	version, _ := meta[mcp.MetaKeyProtocolVersion].(string)
	if version < "2026-07-28" {
		return false
	}
	encoded, _ := json.Marshal(meta[mcp.MetaKeyClientCapabilities])
	var capabilities mcp.ClientCapabilities
	if json.Unmarshal(encoded, &capabilities) != nil {
		return false
	}
	_, ok := capabilities.Extensions[TasksExtension]
	return ok
}

func (b *Bridge) saveTask(ctx context.Context, server string, params *mcp.CallToolParams, task Task) (string, error) {
	if !b.namespace {
		return task.TaskID, nil
	}
	route := taskRoute{Server: server, RemoteID: task.TaskID, Scope: ScopeKey(b.ctx, b.configs[server]), Name: params.Name}
	route.Arguments, _ = json.Marshal(params.Arguments)
	data, err := json.Marshal(route)
	if err != nil {
		return "", err
	}
	expires := time.Now().Add(24 * time.Hour)
	if task.TTL != nil && *task.TTL <= int64(24*time.Hour/time.Millisecond) {
		until := task.CreatedAt.Add(time.Duration(*task.TTL) * time.Millisecond)
		if until.Before(expires) {
			expires = until
		}
	}
	if !expires.After(time.Now()) {
		return "", errors.New("MCP task already expired")
	}
	id := "gtask_" + rand.Text()
	if err := b.taskRegistry.Save(ctx, id, data, expires); err != nil {
		return "", err
	}
	return id, nil
}
func (b *Bridge) loadTask(ctx context.Context, id string) (taskRoute, error) {
	if !b.namespace {
		for server := range b.clients {
			return taskRoute{Server: server, RemoteID: id}, nil
		}
	}
	data, err := b.taskRegistry.Load(ctx, id)
	if err != nil {
		return taskRoute{}, errors.New("MCP task not found")
	}
	var route taskRoute
	if json.Unmarshal(data, &route) != nil {
		return route, errors.New("invalid MCP task route")
	}
	cfg, ok := b.configs[route.Server]
	if !ok || route.Scope != ScopeKey(b.ctx, cfg) {
		return route, errors.New("MCP task configuration or credential scope changed")
	}
	return route, nil
}
func (b *Bridge) taskOperation(ctx context.Context, method string, params *TaskParams) (*TaskReply, error) {
	if params == nil || params.TaskID == "" {
		return nil, errors.New("MCP taskId is required")
	}
	route, err := b.loadTask(ctx, params.TaskID)
	if err != nil {
		return nil, err
	}
	client := b.clients[route.Server]
	if client == nil {
		return nil, errors.New("MCP task server is unavailable")
	}
	copy := *params
	copy.TaskID = route.RemoteID
	if method == "tasks/update" && len(copy.Responses) == 0 {
		return nil, errors.New("MCP task input responses are required")
	}
	if method == "tasks/update" {
		pending, err := client.Task(ctx, "tasks/get", &TaskParams{TaskID: route.RemoteID})
		if err != nil {
			return nil, err
		}
		task, err := pending.task(true)
		if err != nil {
			return nil, err
		}
		if task.TaskID != route.RemoteID || task.Status != "input_required" {
			return nil, errors.New("MCP task is not awaiting input")
		}
		for id, response := range copy.Responses {
			request, ok := task.InputRequests[id]
			if !ok || response == nil {
				return nil, errors.New("MCP task input does not match a pending request")
			}
			if form, ok := request.(*mcp.ElicitParams); ok {
				answer, ok := response.(*mcp.ElicitResult)
				if !ok || answer.Action != "accept" && answer.Action != "decline" && answer.Action != "cancel" {
					return nil, errors.New("invalid MCP task elicitation response")
				}
				if answer.Action == "accept" && form.Mode != "url" && form.RequestedSchema != nil {
					if err := validateToolSchema(form.RequestedSchema, answer.Content); err != nil {
						return nil, err
					}
				}
			}
		}
	}
	reply, err := client.Task(ctx, method, &copy)
	if err != nil {
		return nil, err
	}
	if method == "tasks/update" {
		var ack struct {
			ResultType string `json:"resultType"`
		}
		if json.Unmarshal(reply.Raw, &ack) != nil || ack.ResultType != "complete" {
			return nil, errors.New("invalid MCP task update acknowledgement")
		}
		return reply, nil
	}
	task, err := reply.task(true)
	if err != nil {
		return nil, err
	}
	if task.TaskID != route.RemoteID {
		return nil, errors.New("MCP task response identity mismatch")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(reply.Raw, &fields)
	fields["taskId"], _ = json.Marshal(params.TaskID)
	reply.Raw, err = json.Marshal(fields)
	return reply, err
}

func (b *Bridge) taskTool(ctx context.Context, req *mcp.CallToolRequest, binding Binding) (mcp.Result, error) {
	client := b.clients[binding.Server]
	var args any
	if json.Unmarshal(req.Params.Arguments, &args) != nil {
		return nil, errors.New("invalid MCP tool arguments")
	}
	if err := validateToolSchema(binding.Tool.InputSchema, args); err != nil {
		return nil, err
	}
	params := &mcp.CallToolParams{Name: binding.Tool.Name, Arguments: args, Meta: req.Params.Meta, RequestState: req.Params.RequestState, InputResponses: req.Params.InputResponses}
	return bridgeInvoke(b, ctx, req, "tools/call", binding.Server, req.Params.RequestState, req.Params.InputResponses, func(ctx context.Context) (*TaskReply, error) {
		if b.call != nil {
			result, err := b.call(ctx, binding, params)
			if err != nil {
				return nil, err
			}
			if result != nil {
				raw, err := json.Marshal(result)
				return &TaskReply{Raw: raw}, err
			}
		}
		reply, err := client.CallToolReply(withTaskTool(ctx, binding.Tool), params)
		if err != nil {
			return nil, err
		}
		if reply.isTask() {
			task, err := reply.task(false)
			if err != nil {
				return nil, err
			}
			id, err := b.saveTask(ctx, binding.Server, params, task)
			if err != nil {
				return nil, err
			}
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(reply.Raw, &fields)
			fields["taskId"], _ = json.Marshal(id)
			reply.Raw, err = json.Marshal(fields)
			return reply, err
		}
		var result mcp.CallToolResult
		if err := json.Unmarshal(reply.Raw, &result); err != nil {
			return nil, err
		}
		if !result.NeedsInput() && !result.IsError && binding.Tool.OutputSchema != nil {
			if err := validateToolSchema(binding.Tool.OutputSchema, result.StructuredContent); err != nil {
				return nil, err
			}
		}
		return reply, nil
	})
}
func validateToolSchema(value any, input any) error {
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	var schema jsonschema.Schema
	if err := json.Unmarshal(encoded, &schema); err != nil {
		return err
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return err
	}
	if err := resolved.Validate(input); err != nil {
		return fmt.Errorf("MCP tool schema: %w", err)
	}
	return nil
}

func (b *Bridge) callSynchronous(ctx context.Context, server string, params *mcp.CallToolParams) (*mcp.CallToolResult, error) {
	const prefix = "llama-swap-task:"
	id := ""
	if strings.HasPrefix(params.RequestState, prefix) {
		id = strings.TrimPrefix(params.RequestState, prefix)
		route, err := b.loadTask(ctx, id)
		if err != nil {
			return nil, err
		}
		arguments, _ := json.Marshal(params.Arguments)
		if route.Server != server || b.namespace && (route.Name != params.Name || string(route.Arguments) != string(arguments)) {
			return nil, errors.New("MCP task continuation does not match the tool call")
		}
		if _, err := b.taskOperation(ctx, "tasks/update", &TaskParams{TaskID: id, Responses: params.InputResponses}); err != nil {
			return nil, err
		}
	} else {
		b.mu.Lock()
		var tool *mcp.Tool
		for _, binding := range b.toolBindings {
			if binding.Server == server && binding.Tool.Name == params.Name {
				tool = binding.Tool
				break
			}
		}
		b.mu.Unlock()
		reply, err := b.clients[server].CallToolReply(withTaskTool(ctx, tool), params)
		if err != nil {
			return nil, err
		}
		if !reply.isTask() {
			var result mcp.CallToolResult
			if err := json.Unmarshal(reply.Raw, &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
		task, err := reply.task(false)
		if err != nil {
			return nil, err
		}
		id, err = b.saveTask(ctx, server, params, task)
		if err != nil {
			return nil, err
		}
	}
	for {
		reply, err := b.taskOperation(ctx, "tasks/get", &TaskParams{TaskID: id})
		if err != nil {
			return nil, err
		}
		task, err := reply.task(true)
		if err != nil {
			return nil, err
		}
		switch task.Status {
		case "completed":
			var result mcp.CallToolResult
			if err := json.Unmarshal(task.Result, &result); err != nil {
				return nil, err
			}
			return &result, nil
		case "failed":
			return nil, task.Error
		case "cancelled":
			return nil, errors.New("MCP task was cancelled")
		case "input_required":
			raw, err := json.Marshal(map[string]any{"resultType": "input_required", "inputRequests": task.InputRequests, "requestState": prefix + id})
			if err != nil {
				return nil, err
			}
			var result mcp.CallToolResult
			if err := json.Unmarshal(raw, &result); err != nil {
				return nil, err
			}
			return &result, nil
		}
		delay := time.Second
		if task.PollInterval != nil {
			delay = time.Minute
			if *task.PollInterval <= 60000 {
				delay = time.Duration(*task.PollInterval) * time.Millisecond
			}
			if delay < 50*time.Millisecond {
				delay = 50 * time.Millisecond
			}
		}
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			_, _ = b.taskOperation(cleanup, "tasks/cancel", &TaskParams{TaskID: id})
			cancel()
			return nil, ctx.Err()
		}
	}
}
