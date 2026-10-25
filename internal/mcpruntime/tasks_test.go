package mcpruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

func taskFixture(t *testing.T) (*httptest.Server, *atomic.Int32) {
	t.Helper()
	caps := &mcp.ServerCapabilities{}
	caps.AddExtension(TasksExtension, map[string]any{})
	server := mcp.NewServer(&mcp.Implementation{Name: "tasks", Version: "1"}, &mcp.ServerOptions{Capabilities: caps})
	server.AddTool(&mcp.Tool{Name: "async", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"mode": map[string]any{"type": "string", "x-mcp-header": "Mode"}, "tenant": map[string]any{"type": "object", "properties": map[string]any{"id": map[string]any{"type": "string", "x-mcp-header": "Tenant"}}}}}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return nil, fmt.Errorf("task handler was bypassed")
	})
	var created atomic.Int32
	var mu sync.Mutex
	states := map[string]Task{}
	modes := map[string]string{}
	polls := map[string]int{}
	reply := func(task Task, kind string) *TaskReply {
		raw, _ := json.Marshal(task)
		var fields map[string]any
		_ = json.Unmarshal(raw, &fields)
		fields["resultType"] = kind
		raw, _ = json.Marshal(fields)
		return &TaskReply{Raw: raw}
	}
	server.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method != "tools/call" {
				return next(ctx, method, req)
			}
			if !supportsTasks(req) {
				return nil, &jsonrpc.Error{Code: -32021, Message: "Tasks capability is required"}
			}
			params := req.(*mcp.CallToolRequest).Params
			var args map[string]string
			_ = json.Unmarshal(params.Arguments, &args)
			id := fmt.Sprintf("remote-%d", created.Add(1))
			ttl, interval := int64(60000), int64(1)
			task := Task{TaskID: id, Status: "working", CreatedAt: time.Now().UTC(), LastUpdatedAt: time.Now().UTC(), TTL: &ttl, PollInterval: &interval}
			mu.Lock()
			states[id] = task
			modes[id] = args["mode"]
			mu.Unlock()
			return reply(task, "task"), nil
		}
	})
	for _, method := range []string{"tasks/get", "tasks/update", "tasks/cancel"} {
		err := mcp.AddReceivingCustomMethod(server, method, func(ctx context.Context, peer *mcp.ServerSession, params *TaskParams) (*TaskReply, error) {
			mu.Lock()
			defer mu.Unlock()
			task, ok := states[params.TaskID]
			if !ok {
				return nil, &jsonrpc.Error{Code: -32602, Message: "Unknown task"}
			}
			switch method {
			case "tasks/get":
				if task.Status == "working" {
					polls[task.TaskID]++
					if modes[task.TaskID] == "input" {
						task.Status = "input_required"
						task.InputRequests = mcp.InputRequestMap{"confirm": &mcp.ElicitParams{Mode: "form", Message: "Confirm task", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}}}
					} else if modes[task.TaskID] != "wait" && polls[task.TaskID] > 1 {
						task.Status = "completed"
						task.Result = json.RawMessage(`{"resultType":"complete","content":[{"type":"text","text":"FINISHED"}]}`)
					}
				}
			case "tasks/update":
				if task.Status != "input_required" || params.Responses["confirm"] == nil {
					return nil, &jsonrpc.Error{Code: -32602, Message: "Invalid input"}
				}
				task.Status = "completed"
				task.InputRequests = nil
				task.Result = json.RawMessage(`{"resultType":"complete","content":[{"type":"text","text":"CONFIRMED"}]}`)
			case "tasks/cancel":
				if task.Status != "completed" {
					task.Status = "cancelled"
					task.InputRequests = nil
				}
			}
			task.LastUpdatedAt = time.Now().UTC()
			states[task.TaskID] = task
			if method == "tasks/update" {
				return &TaskReply{Raw: json.RawMessage(`{"resultType":"complete"}`)}, nil
			}
			return reply(task, "complete"), nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	host := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true}))
	t.Cleanup(host.Close)
	return host, &created
}

func taskClient(t *testing.T, endpoint string) *Session {
	t.Helper()
	options := &mcp.ClientOptions{Capabilities: &mcp.ClientCapabilities{}, MultiRoundTrip: &mcp.MultiRoundTripOptions{Disabled: true}}
	options.Capabilities.AddExtension(TasksExtension, map[string]any{})
	session, err := connectSessionOptions(t.Context(), config.MCPServerConfig{Transport: "http", URL: endpoint}, t.Context(), options, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.Close() })
	return session
}

func TestRuntime_TasksProxyAndRecovery(t *testing.T) {
	remote, count := taskFixture(t)
	registry := &memoryTaskRegistry{routes: map[string]memoryTaskRoute{}}
	configs := map[string]config.MCPServerConfig{"tasks": {Transport: "http", URL: remote.URL}}
	bridge, err := NewBridge(t.Context(), configs, true, nil, WithTaskRegistry(registry))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(bridge.Close)
	host := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return bridge.Server }, &mcp.StreamableHTTPOptions{Stateless: true}))
	client := taskClient(t, host.URL)
	params := &mcp.CallToolParams{Name: BoundName("tasks", "async"), Arguments: map[string]any{"tenant": map[string]any{"id": " 中文\n"}}}
	created, err := client.CallToolReply(t.Context(), params)
	if err != nil || !created.isTask() {
		t.Fatalf("create: %v %v", created, err)
	}
	task, err := created.task(false)
	if err != nil || task.TaskID == "remote-1" {
		t.Fatalf("mapping: %+v %v", task, err)
	}
	client.Close()
	host.Close()
	bridge.Close()
	bridge, err = NewBridge(t.Context(), configs, true, nil, WithTaskRegistry(registry))
	if err != nil {
		t.Fatal(err)
	}
	defer bridge.Close()
	host = httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return bridge.Server }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer host.Close()
	client = taskClient(t, host.URL)
	for i := 0; i < 2; i++ {
		result, err := client.Task(t.Context(), "tasks/get", &TaskParams{TaskID: task.TaskID})
		if err != nil {
			t.Fatal(err)
		}
		polled, err := result.task(true)
		if err != nil || polled.TaskID != task.TaskID {
			t.Fatalf("get: %+v %v", polled, err)
		}
		if i == 1 && (polled.Status != "completed" || string(polled.Result) == "") {
			t.Fatalf("completion: %+v", polled)
		}
	}
	if count.Load() != 1 {
		t.Fatal("polling repeated tool execution")
	}
	if _, err := client.Task(t.Context(), "tasks/get", &TaskParams{TaskID: "forged"}); err == nil {
		t.Fatal("unknown task route accepted")
	}
	input, err := client.CallToolReply(t.Context(), &mcp.CallToolParams{Name: params.Name, Arguments: map[string]any{"mode": "input"}})
	if err != nil {
		t.Fatal(err)
	}
	pending, _ := input.task(false)
	polled, err := client.Task(t.Context(), "tasks/get", &TaskParams{TaskID: pending.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	waiting, _ := polled.task(true)
	if waiting.Status != "input_required" || waiting.InputRequests["confirm"] == nil {
		t.Fatalf("input: %+v", waiting)
	}
	if _, err := client.Task(t.Context(), "tasks/update", &TaskParams{TaskID: pending.TaskID, Responses: mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}}}); err != nil {
		t.Fatal(err)
	}
	finished, err := client.Task(t.Context(), "tasks/get", &TaskParams{TaskID: pending.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	final, _ := finished.task(true)
	if final.Status != "completed" {
		t.Fatalf("updated: %+v", final)
	}
	cancelled, err := client.Task(t.Context(), "tasks/cancel", &TaskParams{TaskID: task.TaskID})
	if err != nil {
		t.Fatal(err)
	}
	terminal, _ := cancelled.task(true)
	if terminal.Status != "completed" {
		t.Fatal("cancel changed a completed task")
	}
}

func TestRuntime_TasksSynchronousFallback(t *testing.T) {
	remote, count := taskFixture(t)
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			bridge, err := NewBridge(t.Context(), map[string]config.MCPServerConfig{"tasks": {Transport: "http", URL: remote.URL}}, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bridge.Close()
			left, right := mcp.NewInMemoryTransports()
			ss, err := bridge.Server.Connect(t.Context(), left, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer ss.Close()
			consumer := mcp.NewClient(&mcp.Implementation{Name: "non-task-client", Version: "1"}, &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
			}})
			client, err := consumer.Connect(t.Context(), right, &mcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			before := count.Load()
			result, err := client.CallTool(t.Context(), &mcp.CallToolParams{Name: BoundName("tasks", "async"), Arguments: map[string]any{"mode": "input"}})
			if err != nil || result.Content[0].(*mcp.TextContent).Text != "CONFIRMED" || count.Load() != before+1 {
				t.Fatalf("fallback: %v %v calls=%d", result, err, count.Load()-before)
			}
		})
	}
}
