package mcpruntime

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
)

func TestRuntime_BridgeCompletion(t *testing.T) {
	var calls atomic.Int32
	remote := mcp.NewServer(&mcp.Implementation{Name: "completion", Version: "1"}, &mcp.ServerOptions{CompletionHandler: func(ctx context.Context, req *mcp.CompleteRequest) (*mcp.CompleteResult, error) {
		calls.Add(1)
		if req.Params.Argument.Name != "language" || req.Params.Argument.Value != "py" || req.Params.Context.Arguments["project"] != "sample" {
			t.Error("completion argument context changed")
		}
		if req.Params.Ref.Type == "ref/prompt" && req.Params.Ref.Name != "review" || req.Params.Ref.Type == "ref/resource" && req.Params.Ref.URI != "fixture://{language}" {
			t.Error("upstream received a gateway reference")
		}
		return &mcp.CompleteResult{Completion: mcp.CompletionResultDetails{Values: []string{"python", "pytorch"}, Total: 3, HasMore: true}}, nil
	}})
	remote.AddPrompt(&mcp.Prompt{Name: "review"}, func(context.Context, *mcp.GetPromptRequest) (*mcp.GetPromptResult, error) {
		return &mcp.GetPromptResult{}, nil
	})
	remote.AddResourceTemplate(&mcp.ResourceTemplate{Name: "source", URITemplate: "fixture://{language}"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		return &mcp.ReadResourceResult{}, nil
	})
	host := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true}))
	defer host.Close()
	for _, version := range []string{"2025-11-25", "2026-07-28"} {
		t.Run(version, func(t *testing.T) {
			bridge, err := NewBridge(t.Context(), map[string]config.MCPServerConfig{"remote": {Transport: "http", URL: host.URL, Prompts: []string{"review"}, Resources: []string{"fixture://{language}"}}}, true, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer bridge.Close()
			left, right := mcp.NewInMemoryTransports()
			server, err := bridge.Server.Connect(t.Context(), left, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer server.Close()
			client := mcp.NewClient(&mcp.Implementation{Name: "consumer", Version: "1"}, nil)
			session, err := client.Connect(t.Context(), right, &mcp.ClientSessionOptions{ProtocolVersion: version})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			for _, ref := range []*mcp.CompleteReference{{Type: "ref/prompt", Name: BoundName("remote", "review")}, {Type: "ref/resource", URI: "mcp-template://remote/fixture://{language}"}} {
				result, err := session.Complete(t.Context(), &mcp.CompleteParams{Ref: ref, Argument: mcp.CompleteParamsArgument{Name: "language", Value: "py"}, Context: &mcp.CompleteContext{Arguments: map[string]string{"project": "sample"}}})
				if err != nil || len(result.Completion.Values) != 2 || result.Completion.Total != 3 || !result.Completion.HasMore {
					t.Fatalf("completion: %+v %v", result, err)
				}
			}
			before := calls.Load()
			_, err = session.Complete(t.Context(), &mcp.CompleteParams{Ref: &mcp.CompleteReference{Type: "ref/prompt", Name: "forged"}, Argument: mcp.CompleteParamsArgument{Name: "language", Value: "py"}})
			if err == nil || calls.Load() != before {
				t.Fatal("unlisted completion reference reached upstream")
			}
		})
	}
}
