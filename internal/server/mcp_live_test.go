package server

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"errors"
	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
)

func TestServer_MCPLiveRoutingAndApproval(t *testing.T) {
	for _, policy := range []string{"never", "always"} {
		t.Run(policy, func(t *testing.T) {
			remote, count := mcpFixture(t)
			cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Approval: policy}}}}}
			s := multiSurfaceServer(t, cfg)
			gateway := httptest.NewServer(s)
			defer gateway.Close()
			defer s.closeMCPRuntime()
			session, err := mcpruntime.Connect(t.Context(), config.MCPServerConfig{Transport: "http", URL: gateway.URL + "/mcp?model=m&live=true"})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			tools, err := session.ListTools(t.Context(), nil)
			if err != nil || len(tools.Tools) != 1 {
				t.Fatalf("tools: %v %v", tools, err)
			}
			resources, err := session.ListResources(t.Context(), nil)
			if err != nil || len(resources.Resources) != 1 {
				t.Fatalf("resources: %v %v", resources, err)
			}
			resource, err := session.ReadResource(t.Context(), &mcp.ReadResourceParams{URI: resources.Resources[0].URI})
			if err != nil || resource.Contents[0].Text != "RESOURCE" {
				t.Fatalf("resource: %v %v", resource, err)
			}
			result, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tools.Tools[0].Name, Arguments: map[string]any{"q": "test"}})
			if policy == "never" {
				if err != nil || count.Load() != 1 || result.Content[0].(*mcp.TextContent).Text != "MCP-RESULT" {
					t.Fatalf("tool: %v %v calls=%d", result, err, count.Load())
				}
				return
			}
			var wire *jsonrpc.Error
			if !errors.As(err, &wire) || count.Load() != 0 {
				t.Fatalf("approval bypassed: %v calls=%d", err, count.Load())
			}
			var detail map[string]any
			if json.Unmarshal(wire.Data, &detail) != nil {
				t.Fatal("missing approval data")
			}
			id := protocolFixtureStr(detail["approval_id"])
			approved := postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
			if approved.Code != 200 || count.Load() != 1 {
				t.Fatalf("approval: %d %s calls=%d", approved.Code, approved.Body.String(), count.Load())
			}
		})
	}
}
