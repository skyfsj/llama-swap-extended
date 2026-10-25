package mcpruntime

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestRuntime_BridgeNotificationsAndInput(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			var subscriptions atomic.Int32
			remote := mcp.NewServer(&mcp.Implementation{Name: "remote", Version: "1"}, &mcp.ServerOptions{SubscribeHandler: func(context.Context, *mcp.SubscribeRequest) error { subscriptions.Add(1); return nil }, UnsubscribeHandler: func(context.Context, *mcp.UnsubscribeRequest) error { subscriptions.Add(-1); return nil }})
			remote.AddTool(&mcp.Tool{Name: "input", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				if req.Params.RequestState == "opaque" && req.Params.InputResponses["confirm"] != nil {
					return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "CONFIRMED"}}}, nil
				}
				return &mcp.CallToolResult{RequestState: "opaque", InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{Mode: "form", Message: "Confirm", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}}}}, nil
			})
			remote.AddResource(&mcp.Resource{Name: "data", URI: "fixture://data"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
				return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: "fixture://data", Text: "DATA"}}}, nil
			})
			host := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, nil))
			defer host.Close()
			bridge, err := NewBridge(t.Context(), map[string]config.MCPServerConfig{"remote": {Transport: "http", URL: host.URL}}, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bridge.Close()
			changed := make(chan struct{}, 10)
			updated := make(chan string, 10)
			acknowledged := make(chan struct{}, 10)
			client := mcp.NewClient(&mcp.Implementation{Name: "consumer", Version: "1"}, &mcp.ClientOptions{ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
				return &mcp.ElicitResult{Action: "accept", Content: map[string]any{}}, nil
			}, ToolListChangedHandler: func(context.Context, *mcp.ToolListChangedRequest) {
				select {
				case changed <- struct{}{}:
				default:
				}
			}, ResourceUpdatedHandler: func(ctx context.Context, req *mcp.ResourceUpdatedNotificationRequest) { updated <- req.Params.URI }})
			client.AddReceivingMiddleware(func(next mcp.MethodHandler) mcp.MethodHandler {
				return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
					if method == "notifications/subscriptions/acknowledged" {
						if params, ok := req.GetParams().(*mcp.SubscriptionsAcknowledgedParams); ok && len(params.Notifications.ResourceSubscriptions) > 0 {
							acknowledged <- struct{}{}
						}
					}
					return next(ctx, method, req)
				}
			})
			left, right := mcp.NewInMemoryTransports()
			serverSession, err := bridge.Server.Connect(t.Context(), left, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer serverSession.Close()
			session, err := client.Connect(t.Context(), right, &mcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: BoundName("remote", "input"), Arguments: map[string]any{}})
			if err != nil || result.Content[0].(*mcp.TextContent).Text != "CONFIRMED" {
				t.Fatalf("input: %v %v", result, err)
			}
			remote.AddTool(&mcp.Tool{Name: "added", InputSchema: map[string]any{"type": "object"}}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
				return &mcp.CallToolResult{Content: []mcp.Content{}}, nil
			})
			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			found := false
			for !found {
				select {
				case <-changed:
					tools, err := session.ListTools(t.Context(), nil)
					if err != nil {
						t.Fatal(err)
					}
					for _, tool := range tools.Tools {
						found = found || tool.Name == BoundName("remote", "added")
					}
				case <-deadline.C:
					t.Fatal("live tool inventory did not update")
				}
			}
			uri := bridgeURI("remote", "fixture://data")
			if err := session.Subscribe(t.Context(), &mcp.SubscribeParams{URI: uri}); err != nil {
				t.Fatal(err)
			}
			if version == "2026-07-28" {
				select {
				case <-acknowledged:
				case <-time.After(5 * time.Second):
					t.Fatal("subscription acknowledgement missing")
				}
			}
			if err := remote.ResourceUpdated(t.Context(), &mcp.ResourceUpdatedNotificationParams{URI: "fixture://data"}); err != nil {
				t.Fatal(err)
			}
			select {
			case got := <-updated:
				if got != uri {
					t.Fatalf("resource scope: %s", got)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("resource update missing")
			}
			if err := session.Unsubscribe(t.Context(), &mcp.UnsubscribeParams{URI: uri}); err != nil {
				t.Fatal(err)
			}
			limit := time.After(5 * time.Second)
			tick := time.NewTicker(time.Millisecond)
			defer tick.Stop()
			for subscriptions.Load() != 0 {
				select {
				case <-tick.C:
				case <-limit:
					t.Fatalf("subscriptions leaked: %d", subscriptions.Load())
				}
			}
		})
	}
}

func TestRuntime_BridgeLegacyCallbacks(t *testing.T) {
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) { testBridgeLegacyCallbacks(t, version) })
	}
}

func testBridgeLegacyCallbacks(t *testing.T, version string) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "legacy", Version: "1"}, nil)
	remote.AddTool(&mcp.Tool{Name: "ask", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		//lint:ignore SA1019 Exercise negotiated legacy roots forwarding.
		roots, err := req.Session.ListRoots(ctx, &mcp.ListRootsParams{})
		if err != nil {
			return nil, err
		}
		if len(roots.Roots) != 1 {
			return nil, fmt.Errorf("roots not forwarded")
		}
		//lint:ignore SA1019 Exercise negotiated legacy sampling forwarding.
		sampled, err := req.Session.CreateMessageWithTools(ctx, &mcp.CreateMessageWithToolsParams{MaxTokens: 16, Messages: []*mcp.SamplingMessageV2{{Role: "user", Content: []mcp.Content{&mcp.TextContent{Text: "Sample"}}}}})
		if err != nil {
			return nil, err
		}
		if sampled.Model != "client-model" {
			return nil, fmt.Errorf("sampling not forwarded")
		}
		result, err := req.Session.Elicit(ctx, &mcp.ElicitParams{Mode: "form", Message: "Confirm", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}})
		if err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: result.Action}}}, nil
	})
	remote.AddResource(&mcp.Resource{Name: "ask", URI: "fixture://ask"}, func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		result, err := req.Session.Elicit(ctx, &mcp.ElicitParams{Mode: "form", Message: "Resource", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}})
		if err != nil {
			return nil, err
		}
		return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{URI: req.Params.URI, Text: result.Action}}}, nil
	})
	remote.AddPrompt(&mcp.Prompt{Name: "ask"}, func(ctx context.Context, req *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		result, err := req.Session.Elicit(ctx, &mcp.ElicitParams{Mode: "form", Message: "Prompt", RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}}})
		if err != nil {
			return nil, err
		}
		return &mcp.GetPromptResult{Messages: []*mcp.PromptMessage{{Role: "user", Content: &mcp.TextContent{Text: result.Action}}}}, nil
	})
	host := httptest.NewServer(mcp.NewSSEHandler(func(*http.Request) *mcp.Server { return remote }, nil))
	defer host.Close()
	bridge, err := NewBridge(t.Context(), map[string]config.MCPServerConfig{"legacy": {Transport: "sse", URL: host.URL}}, true, nil)
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
	//lint:ignore SA1019 Exercise explicit legacy sampling without making a provider request.
	client := mcp.NewClient(&mcp.Implementation{Name: "consumer", Version: "1"}, &mcp.ClientOptions{CreateMessageWithToolsHandler: func(context.Context, *mcp.CreateMessageWithToolsRequest) (*mcp.CreateMessageWithToolsResult, error) {
		//lint:ignore SA1019 Provide the fixture result for a negotiated legacy sampling request.
		return &mcp.CreateMessageWithToolsResult{Role: "assistant", Model: "client-model", Content: []mcp.Content{&mcp.TextContent{Text: "SAMPLED"}}}, nil
	}, ElicitationHandler: func(context.Context, *mcp.ElicitRequest) (*mcp.ElicitResult, error) {
		return &mcp.ElicitResult{Action: "decline"}, nil
	}})
	//lint:ignore SA1019 Provide the fixture roots for a negotiated legacy roots request.
	client.AddRoots(&mcp.Root{URI: "file:///allowed", Name: "allowed"})
	cs, err := client.Connect(t.Context(), right, &mcp.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{Name: BoundName("legacy", "ask"), Arguments: map[string]any{}})
	if err != nil || result.Content[0].(*mcp.TextContent).Text != "decline" {
		t.Fatalf("callback: %v %v", result, err)
	}
	resource, err := cs.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: bridgeURI("legacy", "fixture://ask")})
	if err != nil || resource.Contents[0].Text != "decline" {
		t.Fatalf("resource callback: %v %v", resource, err)
	}
	prompt, err := cs.GetPrompt(t.Context(), &mcp.GetPromptParams{Name: BoundName("legacy", "ask")})
	if err != nil || prompt.Messages[0].Content.(*mcp.TextContent).Text != "decline" {
		t.Fatalf("prompt callback: %v %v", prompt, err)
	}
}
