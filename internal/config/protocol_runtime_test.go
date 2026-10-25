package config

import "testing"

func TestConfig_ProtocolRuntime(t *testing.T) {
	for _, transport := range []string{"http", "sse", "stdio"} {
		server := MCPServerConfig{Transport: transport, URL: "https://example.com/mcp", Command: "/usr/local/bin/mcp", Headers: map[string]string{"Authorization": "MCP_AUTHORIZATION"}, Env: []string{"HOME"}}
		if err := (ProtocolRuntimeConfig{MCP: MCPRuntimeConfig{Servers: map[string]MCPServerConfig{"server": server}}}).Validate(); err != nil {
			t.Fatalf("%s: %v", transport, err)
		}
	}
	for _, server := range []MCPServerConfig{{Transport: "stdio", Command: "relative"}, {Transport: "http", URL: "https://user:secret@example.com/mcp"}, {Transport: "http", URL: "file:///tmp/mcp"}, {Transport: "http", URL: "https://example.com", Approval: "read-only"}, {Transport: "http", URL: "https://example.com", Headers: map[string]string{"Authorization": "Bearer inline-secret"}}} {
		if err := (ProtocolRuntimeConfig{MCP: MCPRuntimeConfig{Servers: map[string]MCPServerConfig{"server": server}}}).Validate(); err == nil {
			t.Fatalf("invalid config accepted: %+v", server)
		}
	}
}
