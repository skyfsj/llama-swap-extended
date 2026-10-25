package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func mcpFixture(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var calls atomic.Int64
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	server.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []string{"q"}}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		calls.Add(1)
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "MCP-RESULT"}}, StructuredContent: map[string]any{"value": "STRUCTURED"}}, nil
	})
	server.AddResource(&mcp.Resource{URI: "fixture://data", Name: "data"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "fixture://data", Text: "RESOURCE"}}}, nil
	})
	server.AddPrompt(&mcp.Prompt{Name: "greet"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: "PROMPT"}}}}, nil
	})
	httpServer := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{Stateless: true, PropagateRequestCancellation: true}))
	t.Cleanup(httpServer.Close)
	return httpServer, &calls
}

func TestServer_MCPRuntimeProtocolMatrix(t *testing.T) {
	remote, count := mcpFixture(t)
	for _, client := range protocolFixtureFormats {
		for _, upstream := range protocolFixtureFormats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s__%s/stream=%t", client, upstream, stream), func(t *testing.T) {
					before := count.Load()
					rounds := 0
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						parsed, _, err := dialect.DecodeRequest(upstream, body)
						if err != nil {
							t.Error(err)
							return
						}
						rounds++
						hasResult := false
						for _, message := range parsed.Messages {
							for _, part := range message.Parts {
								if part.Kind == dialect.PartToolResult {
									hasResult = true
									if !strings.Contains(string(body), "MCP-RESULT") {
										t.Errorf("tool result lost: %s", body)
									}
								}
							}
						}
						response := &dialect.Response{ID: fmt.Sprintf("native_%d", rounds), Model: "m", Status: "completed", FinishReason: "stop", Parts: []dialect.Part{{Kind: dialect.PartText, Text: "DONE"}}}
						if !hasResult {
							if len(parsed.Tools) != 1 {
								t.Errorf("tools=%v", parsed.Tools)
								w.WriteHeader(400)
								return
							}
							response.FinishReason = "tool_calls"
							response.Parts = []dialect.Part{{Kind: dialect.PartToolCall, ToolCallID: "mcp-call", ToolName: parsed.Tools[0].Name, Arguments: json.RawMessage(`{"q":"hello"}`)}}
						}
						encoded, _, err := dialect.EncodeResponse(response, upstream)
						if err != nil {
							t.Error(err)
							return
						}
						if stream {
							events, _, err := dialect.ConvertResponseToStream(upstream, upstream, encoded, dialect.StreamOptions{})
							if err != nil {
								t.Error(err)
								return
							}
							w.Header().Set("Content-Type", "text/event-stream")
							for _, event := range events {
								if event.Event != "" {
									fmt.Fprintf(w, "event: %s\n", event.Event)
								}
								fmt.Fprintf(w, "data: %s\n\n", event.Data)
							}
						} else {
							w.Header().Set("Content-Type", "application/json")
							w.Write(encoded)
						}
					}))
					defer backend.Close()
					cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Approval: "never"}}}}, Providers: config.ProviderDictionaryConfig{"provider": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "adapter", Type: protocolFixtureSurface(upstream), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
					s := multiSurfaceServer(t, cfg)
					w := postJSON(t, s, protocolFixturePath(client, stream), string(conversationFixtureInput(client, "CALL", stream)))
					if w.Code != 200 || !strings.Contains(w.Body.String(), "DONE") {
						t.Fatalf("response=%d %s", w.Code, w.Body.String())
					}
					if count.Load() != before+1 || rounds != 2 {
						t.Fatalf("tool executions=%d upstream rounds=%d", count.Load()-before, rounds)
					}
				})
			}
		}
	}
}

func TestServer_MCPRuntimeServerAndApproval(t *testing.T) {
	remote, count := mcpFixture(t)
	cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL}}}}}
	s := multiSurfaceServer(t, cfg)
	gateway := httptest.NewServer(s)
	defer gateway.Close()
	ctx := t.Context()
	session, err := mcpruntime.Connect(ctx, config.MCPServerConfig{Transport: "http", URL: gateway.URL + "/mcp?model=m"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	tools, err := session.ListTools(ctx, nil)
	if err != nil || len(tools.Tools) != 1 {
		t.Fatalf("tools=%v err=%v", tools, err)
	}
	resources, err := session.ListResources(ctx, nil)
	if err != nil || len(resources.Resources) != 1 {
		t.Fatalf("resources=%v err=%v", resources, err)
	}
	resource, err := session.ReadResource(ctx, &mcp.ReadResourceParams{URI: resources.Resources[0].URI})
	if err != nil || resource.Contents[0].Text != "RESOURCE" {
		t.Fatalf("resource=%v err=%v", resource, err)
	}
	prompts, err := session.ListPrompts(ctx, nil)
	if err != nil || len(prompts.Prompts) != 1 {
		t.Fatalf("prompts=%v err=%v", prompts, err)
	}
	prompt, err := session.GetPrompt(ctx, &mcp.GetPromptParams{Name: prompts.Prompts[0].Name})
	if err != nil || len(prompt.Messages) != 1 {
		t.Fatalf("prompt=%v err=%v", prompt, err)
	}
	_, err = session.CallTool(ctx, &mcp.CallToolParams{Name: tools.Tools[0].Name, Arguments: map[string]any{"q": "hello"}})
	if err == nil || !strings.Contains(err.Error(), "gapproval_") {
		t.Fatalf("missing approval: %v", err)
	}
	id := strings.TrimSpace(err.Error()[strings.Index(err.Error(), "gapproval_"):])
	if index := strings.IndexAny(id, " \"\n"); index >= 0 {
		id = id[:index]
	}
	if count.Load() != 0 {
		t.Fatal("unapproved tool executed")
	}
	w := postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte("STRUCTURED")) {
		t.Fatalf("approval %s: %d %s", id, w.Code, w.Body.String())
	}
	if count.Load() != 1 {
		t.Fatal("approved tool not executed")
	}
	w = postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
	if w.Code != 409 || count.Load() != 1 {
		t.Fatalf("repeated approval: %d count=%d", w.Code, count.Load())
	}
}

func TestServer_MCPApprovalModelResume(t *testing.T) {
	remote, count := mcpFixture(t)
	for _, client := range protocolFixtureFormats {
		t.Run(string(client), func(t *testing.T) {
			before := count.Load()
			round := 0
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				round++
				body, _ := io.ReadAll(r.Body)
				parsed, _, err := dialect.DecodeRequest(dialect.OpenAIChat, body)
				if err != nil {
					t.Error(err)
				}
				hasResult := false
				for _, message := range parsed.Messages {
					for _, part := range message.Parts {
						if part.Kind == dialect.PartToolResult {
							hasResult = true
						}
					}
				}
				response := &dialect.Response{ID: "r", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartText, Text: "DONE"}}}
				if !hasResult {
					response.Parts = []dialect.Part{{Kind: dialect.PartToolCall, ToolName: parsed.Tools[0].Name, ToolCallID: "call_1", Arguments: json.RawMessage(`{"q":"x"}`)}}
				}
				encoded, _, _ := dialect.EncodeResponse(response, dialect.OpenAIChat)
				w.Header().Set("Content-Type", "application/json")
				w.Write(encoded)
			}))
			defer backend.Close()
			cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL}}}}, Providers: config.ProviderDictionaryConfig{"provider": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "adapter", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			w := postJSON(t, s, protocolFixturePath(client, false), string(conversationFixtureInput(client, "CALL", false)))
			if w.Code != 409 {
				t.Fatalf("approval response=%d %s", w.Code, w.Body.String())
			}
			body := protocolFixtureObj(w.Body.Bytes())
			detail := protocolFixtureMap(body["error"])
			id := protocolFixtureStr(detail["approval_id"])
			if id == "" {
				t.Fatalf("approval identity lost: %s", w.Body.String())
			}
			if count.Load() != before {
				t.Fatal("unapproved execution")
			}
			approved := postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
			if approved.Code != 200 {
				t.Fatalf("approve=%d %s", approved.Code, approved.Body.String())
			}
			resumed := postJSON(t, s, "/api/mcp/approvals/"+id+"/resume", `{}`)
			if resumed.Code != 200 || !strings.Contains(resumed.Body.String(), "DONE") {
				t.Fatalf("resume=%d %s", resumed.Code, resumed.Body.String())
			}
			if count.Load() != before+1 || round != 2 {
				t.Fatalf("executions=%d rounds=%d", count.Load()-before, round)
			}
			repeat := postJSON(t, s, "/api/mcp/approvals/"+id+"/resume", `{}`)
			if repeat.Code != 409 {
				t.Fatalf("repeated resume=%d %s", repeat.Code, repeat.Body.String())
			}
		})
	}
}

func TestServer_MCPResourcesPromptsContext(t *testing.T) {
	remote, _ := mcpFixture(t)
	for _, format := range protocolFixtureFormats {
		t.Run(string(format), func(t *testing.T) {
			var received []byte
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received, _ = io.ReadAll(r.Body)
				w.Header().Set("Content-Type", "application/json")
				w.Write(protocolFixtureFinalResponse(format))
			}))
			defer backend.Close()
			cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Approval: "never"}}}}, Providers: config.ProviderDictionaryConfig{"provider": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "adapter", Type: protocolFixtureSurface(format), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			input := protocolFixtureObj(conversationFixtureInput(format, "CURRENT", false))
			input["gateway_context"] = map[string]any{"resources": []any{map[string]any{"server": "fixture", "uri": "fixture://data"}}, "prompts": []any{map[string]any{"server": "fixture", "name": "greet"}}}
			w := postJSON(t, s, protocolFixturePath(format, false), string(protocolFixtureJSON(input)))
			if w.Code != 200 {
				t.Fatalf("context=%d %s", w.Code, w.Body.String())
			}
			for _, text := range []string{"RESOURCE", "PROMPT", "CURRENT"} {
				if !bytes.Contains(received, []byte(text)) {
					t.Errorf("%s missing from %s", text, received)
				}
			}
			if bytes.Contains(received, []byte("gateway_context")) {
				t.Errorf("gateway field forwarded: %s", received)
			}
		})
	}
}

func TestServer_MCPNativeReasoningContinuation(t *testing.T) {
	remote, _ := mcpFixture(t)
	for _, format := range []dialect.Format{dialect.ClaudeMessages, dialect.Gemini} {
		t.Run(string(format), func(t *testing.T) {
			round := 0
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				round++
				body, _ := io.ReadAll(r.Body)
				parsed, _, err := dialect.DecodeRequest(format, body)
				if err != nil {
					t.Error(err)
				}
				response := &dialect.Response{ID: "native", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartText, Text: "DONE"}}}
				if round == 1 {
					response.Parts = []dialect.Part{{Kind: dialect.PartReasoning, Text: "plan", Signature: "OPAQUE", ThoughtSignature: "OPAQUE"}, {Kind: dialect.PartToolCall, ToolCallID: "call_native", ToolName: parsed.Tools[0].Name, Arguments: json.RawMessage(`{"q":"x"}`)}}
				} else {
					if !bytes.Contains(body, []byte("OPAQUE")) {
						t.Errorf("native reasoning signature lost: %s", body)
					}
					if bytes.Count(body, []byte("FIRST")) != 1 {
						t.Errorf("duplicate native input: %s", body)
					}
				}
				encoded, _, _ := dialect.EncodeResponse(response, format)
				w.Header().Set("Content-Type", "application/json")
				w.Write(encoded)
			}))
			defer backend.Close()
			cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Approval: "never"}}}}, Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: protocolFixtureSurface(format), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			w := postJSON(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"FIRST"}]}`)
			if w.Code != 200 || round != 2 {
				t.Fatalf("response=%d rounds=%d %s", w.Code, round, w.Body.String())
			}
		})
	}
}
