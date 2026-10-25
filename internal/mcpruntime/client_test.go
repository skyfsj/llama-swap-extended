package mcpruntime

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

func runtimeFixtureServer(options ...*mcp.ServerOptions) *mcp.Server {
	var option *mcp.ServerOptions
	if len(options) > 0 {
		option = options[0]
	}
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, option)
	s.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"text": map[string]any{"type": "string"}}, "required": []string{"text"}}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var args struct {
			Text string `json:"text"`
		}
		_ = json.Unmarshal(r.Params.Arguments, &args)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: args.Text}}, StructuredContent: map[string]any{"echo": args.Text}}, nil
	})
	s.AddTool(&mcp.Tool{Name: "wait", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	s.AddResource(&mcp.Resource{URI: "test://data", Name: "data", MIMEType: "text/plain"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "test://data", Text: "resource"}}}, nil
	})
	s.AddPrompt(&mcp.Prompt{Name: "greet"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "prompt"}}}}, nil
	})
	return s
}

func TestRuntime_StdioHelper(t *testing.T) {
	if os.Getenv("LLAMA_MCP_HELPER") != "1" {
		return
	}
	server := runtimeFixtureServer()
	if os.Getenv("LLAMA_MCP_STATEFUL") == "1" {
		server = mcp.NewServer(&mcp.Implementation{Name: "stateful", Version: "1"}, nil)
		name := "ticket_" + rand.Text()
		server.AddTool(&mcp.Tool{Name: name, InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: name}}}, nil
		})
	}
	if err := server.Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}
func TestRuntime_Transports(t *testing.T) {
	for _, transport := range []string{"http", "http-legacy", "sse", "stdio"} {
		t.Run(transport, func(t *testing.T) {
			cfg := config.MCPServerConfig{Transport: transport, Approval: "never"}
			if transport == "http-legacy" {
				cfg.Transport = "http"
			}
			var count atomic.Int64
			if transport == "stdio" {
				t.Setenv("LLAMA_MCP_HELPER", "1")
				exe, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cfg.Command = exe
				cfg.Args = []string{"-test.run=TestRuntime_StdioHelper"}
				cfg.Env = []string{"LLAMA_MCP_HELPER"}
			} else {
				server := runtimeFixtureServer()
				if transport == "http-legacy" {
					server = runtimeFixtureServer(&mcp.ServerOptions{SupportedProtocolVersions: []string{"2025-11-25"}})
				}
				var handler http.Handler
				if transport == "http" || transport == "http-legacy" {
					handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: transport == "http", PropagateRequestCancellation: true})
				} else {
					handler = mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return server }, nil)
				}
				httpServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					count.Add(1)
					if r.Header.Get("Authorization") != "Bearer fixture" {
						w.WriteHeader(401)
						return
					}
					handler.ServeHTTP(w, r)
				}))
				defer httpServer.Close()
				t.Setenv("LLAMA_MCP_TEST_TOKEN", "Bearer fixture")
				cfg.URL = httpServer.URL
				cfg.Headers = map[string]string{"Authorization": "LLAMA_MCP_TEST_TOKEN"}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			session, err := Connect(ctx, cfg)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(ctx, nil)
			if err != nil || len(tools.Tools) != 2 {
				t.Fatalf("tools=%v err=%v", tools, err)
			}
			result, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{"text": "hello"}})
			if err != nil || result.Content[0].(*mcp.TextContent).Text != "hello" {
				t.Fatalf("call=%v err=%v", result, err)
			}
			resources, err := session.ListResources(ctx, nil)
			if err != nil || len(resources.Resources) != 1 {
				t.Fatalf("resources=%v err=%v", resources, err)
			}
			resource, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: "test://data"})
			if err != nil || resource.Contents[0].Text != "resource" {
				t.Fatalf("resource=%v err=%v", resource, err)
			}
			prompts, err := session.ListPrompts(ctx, nil)
			if err != nil || len(prompts.Prompts) != 1 {
				t.Fatalf("prompts=%v err=%v", prompts, err)
			}
			prompt, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: "greet"})
			if err != nil || len(prompt.Messages) != 1 {
				t.Fatalf("prompt=%v err=%v", prompt, err)
			}
			short, stop := context.WithTimeout(ctx, 25*time.Millisecond)
			defer stop()
			_, err = session.CallTool(short, &mcp.CallToolParams{Name: "wait", Arguments: map[string]any{}})
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("cancellation: %v", err)
			}
		})
	}
}
func TestRuntime_NameBinding(t *testing.T) {
	a := BoundName("server", "a.b")
	b := BoundName("server", "a/b")
	if a == b || len(a) > 64 {
		t.Fatalf("unsafe name binding: %s %s", a, b)
	}
}

func TestRuntime_InputRequiredContinuation(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "input", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "input", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		if request.Params.RequestState == "opaque-state" && request.Params.InputResponses["confirm"] != nil {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ACCEPTED"}}}, nil
		}
		return &mcp.CallToolResult{InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{Mode: "form", Message: "Confirm input", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}}}, RequestState: "opaque-state"}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer httpServer.Close()
	session, err := Connect(t.Context(), config.MCPServerConfig{Transport: "http", URL: httpServer.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "input", Arguments: map[string]any{}})
	if err != nil || !result.NeedsInput() || result.RequestState != "opaque-state" {
		t.Fatalf("input required=%v err=%v", result, err)
	}
	result, err = session.CallTool(t.Context(), &mcp.CallToolParams{Name: "input", Arguments: map[string]any{}, RequestState: result.RequestState, InputResponses: mcp.InputResponseMap{"confirm": &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}}})
	if err != nil || result.NeedsInput() || result.Content[0].(*mcp.TextContent).Text != "ACCEPTED" {
		t.Fatalf("continuation=%v err=%v", result, err)
	}
}
