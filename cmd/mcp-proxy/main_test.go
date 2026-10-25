package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestProxy_StdioHelper(t *testing.T) {
	endpoint := os.Getenv("MCP_PROXY_HELPER_URL")
	if endpoint == "" {
		return
	}
	if err := run(context.Background(), endpoint, ""); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

func TestProxy_StdioBridge(t *testing.T) {
	server := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "TOOL"}}}, nil
	})
	server.AddResource(&mcp.Resource{URI: "fixture://data", Name: "data"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "fixture://data", Text: "RESOURCE"}}}, nil
	})
	server.AddResourceTemplate(&mcp.ResourceTemplate{URITemplate: "fixture://item/{id}", Name: "items"}, func(ctx context.Context, request *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: request.Params.URI, Text: "TEMPLATE"}}}, nil
	})
	server.AddPrompt(&mcp.Prompt{Name: "greet"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "PROMPT"}}}}, nil
	})
	remote := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer remote.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			command := exec.CommandContext(t.Context(), exe, "-test.run=TestProxy_StdioHelper")
			command.Env = append(os.Environ(), "MCP_PROXY_HELPER_URL="+remote.URL)
			client := mcp.NewClient(&mcp.Implementation{Name: "stdio-client", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), &mcp.CommandTransport{Command: command}, &mcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			caps := session.InitializeResult().Capabilities
			encodedCaps, _ := json.Marshal(caps)
			var capFields map[string]any
			_ = json.Unmarshal(encodedCaps, &capFields)
			if !caps.Tools.ListChanged || !caps.Resources.ListChanged || !caps.Resources.Subscribe || !caps.Prompts.ListChanged || capFields["logging"] == nil {
				t.Fatal("proxy did not advertise live notifications")
			}
			tools, err := session.ListTools(t.Context(), nil)
			if err != nil || len(tools.Tools) != 1 {
				t.Fatalf("tools=%v %v", tools, err)
			}
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: "echo", Arguments: map[string]any{}})
			if err != nil || result.Content[0].(*mcp.TextContent).Text != "TOOL" {
				t.Fatalf("call=%v %v", result, err)
			}
			resources, err := session.ListResources(t.Context(), nil)
			if err != nil || len(resources.Resources) != 1 {
				t.Fatalf("resources=%v %v", resources, err)
			}
			resource, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fixture://data"})
			if err != nil || resource.Contents[0].Text != "RESOURCE" {
				t.Fatalf("resource=%v %v", resource, err)
			}
			templates, err := session.ListResourceTemplates(t.Context(), nil)
			if err != nil || len(templates.ResourceTemplates) != 1 {
				t.Fatalf("templates=%v %v", templates, err)
			}
			template, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: "fixture://item/1"})
			if err != nil || template.Contents[0].Text != "TEMPLATE" {
				t.Fatalf("template=%v %v", template, err)
			}
			prompts, err := session.ListPrompts(t.Context(), nil)
			if err != nil || len(prompts.Prompts) != 1 {
				t.Fatalf("prompts=%v %v", prompts, err)
			}
			prompt, err := session.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: "greet"})
			if err != nil || len(prompt.Messages) != 1 {
				t.Fatalf("prompt=%v %v", prompt, err)
			}
		})
	}
}
