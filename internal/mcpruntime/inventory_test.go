package mcpruntime

import (
	"context"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestRuntime_InventoryValidation(t *testing.T) {
	for _, value := range []any{nil, (*mcp.Tool)(nil), &mcp.Tool{}, &mcp.Tool{Name: strings.Repeat("x", 129)}, &mcp.Resource{}, &mcp.ResourceTemplate{}, &mcp.Prompt{}, &mcp.Tool{Name: "large", Description: strings.Repeat("x", 1<<20)}} {
		inventory := Inventory{}
		if err := inventory.Add(value); err == nil {
			t.Fatalf("invalid inventory entry accepted: %T", value)
		}
	}
	inventory := Inventory{}
	tool := &mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}
	if err := inventory.Add(tool); err != nil {
		t.Fatal(err)
	}
	if err := inventory.Add(tool); err == nil {
		t.Fatal("duplicate tool identity accepted")
	}
	inventory.Scope = "other"
	if err := inventory.Add(tool); err != nil {
		t.Fatal("namespaced identity was rejected")
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "validation", Version: "1"}, nil)
	if err := RegisterTool(server, &mcp.Tool{Name: "invalid"}, func(context.Context, *mcp.CallToolRequest) (*mcp.CallToolResult, error) { return nil, nil }); err == nil {
		t.Fatal("missing tool schema was accepted")
	}
	if err := RegisterResourceTemplate(server, &mcp.ResourceTemplate{Name: "invalid", URITemplate: "fixture://{broken"}, func(context.Context, *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) { return nil, nil }); err == nil {
		t.Fatal("invalid resource template was accepted")
	}
}
