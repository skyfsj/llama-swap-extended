package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func TestServer_MCPClientPolicy(t *testing.T) {
	cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Servers: map[string]config.MCPServerConfig{"known": {Transport: "http", URL: "https://example.com/mcp", Approval: "never"}}}}}
	for _, test := range []struct {
		name, body        string
		allowed, approval bool
		readOnly          bool
	}{
		{"require_approval", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp","require_approval":"always","allowed_tools":["echo"]}]}`, true, true, false},
		{"default_approval", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp"}]}`, true, true, false},
		{"never", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp","require_approval":"never"}]}`, true, false, false},
		{"empty_allowlist", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp","allowed_tools":[]}]}`, false, false, false},
		{"restricted", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp","allowed_tools":["other"]}]}`, false, false, false},
		{"readonly", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp","allowed_tools":{"read_only":true}}]}`, true, true, true},
		{"not_readonly", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp","allowed_tools":{"read_only":true}}]}`, false, false, false},
		{"foreign_credentials", `{"tools":[{"type":"mcp","server_url":"https://example.com/mcp","authorization":"another-account"}]}`, false, false, false},
		{"different_endpoint", `{"tools":[{"type":"mcp","server_label":"known","server_url":"https://other.example.com/mcp","require_approval":"always"}]}`, true, false, false},
		{"claude_disabled", `{"mcp_servers":[{"type":"url","url":"https://example.com/mcp","tool_configuration":{"enabled":false}}]}`, false, false, false},
		{"claude_allowed", `{"mcp_servers":[{"type":"url","url":"https://example.com/mcp","tool_configuration":{"allowed_tools":["echo"]}}]}`, true, false, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			var input map[string]json.RawMessage
			_ = json.Unmarshal([]byte(test.body), &input)
			ctx := withMCPRequestPolicy(context.Background(), cfg, input)
			binding := mcpruntime.Binding{Server: "known", Config: cfg.ProtocolRuntime.MCP.Servers["known"], Tool: &mcp.Tool{Name: "echo", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: test.readOnly}}}
			allowed := applyMCPRequestPolicy(ctx, &binding)
			if allowed != test.allowed || allowed && binding.RequireApproval != test.approval {
				t.Fatalf("allowed=%t approval=%t", allowed, binding.RequireApproval)
			}
		})
	}
}

func TestServer_MCPToolChoiceNoneBlocksExecution(t *testing.T) {
	remote, count := mcpFixture(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		response := &dialect.Response{ID: "violation", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartToolCall, ToolCallID: "forbidden", ToolName: mcpruntime.BoundName("fixture", "echo"), Arguments: json.RawMessage(`{"q":"x"}`)}}}
		encoded, _, _ := dialect.EncodeResponse(response, dialect.OpenAIChat)
		w.Header().Set("Content-Type", "application/json")
		w.Write(encoded)
	}))
	defer backend.Close()
	cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Approval: "never"}}}}, Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
	for _, format := range protocolFixtureFormats {
		t.Run(string(format), func(t *testing.T) {
			s := multiSurfaceServer(t, cfg)
			request := &dialect.Request{Model: "m", Messages: []dialect.Message{{Role: dialect.RoleUser, Parts: []dialect.Part{{Kind: dialect.PartText, Text: "NO TOOLS"}}}}, ToolChoice: &dialect.ToolChoice{Mode: dialect.ToolChoiceNone}}
			body, _, err := dialect.EncodeRequest(request, format, dialect.ConvertOptions{})
			if err != nil {
				t.Fatal(err)
			}
			w := postJSON(t, s, protocolFixturePath(format, false), string(body))
			if w.Code < 400 {
				t.Fatalf("forbidden tool accepted: %d %s", w.Code, w.Body.String())
			}
			if count.Load() != 0 {
				t.Fatal("tool_choice:none executed an MCP tool")
			}
		})
	}
}

func TestServer_MCPNativeApprovalPolicySurvivesResume(t *testing.T) {
	remote, count := mcpFixture(t)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		response := &dialect.Response{ID: "policy", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartToolCall, ToolCallID: "policy_call", ToolName: mcpruntime.BoundName("fixture", "echo"), Arguments: json.RawMessage(`{"q":"x"}`)}}}
		encoded, _, _ := dialect.EncodeResponse(response, dialect.OpenAIChat)
		w.Header().Set("Content-Type", "application/json")
		w.Write(encoded)
	}))
	defer backend.Close()
	cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Approval: "never"}}}}, Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
	s := multiSurfaceServer(t, cfg)
	input := map[string]any{"model": "m", "input": "CALL", "tools": []any{map[string]any{"type": "mcp_server", "server_name": "fixture", "url": remote.URL, "require_approval": "always", "allowed_tools": []string{"echo"}}}}
	pending := postJSON(t, s, "/v1beta/interactions", string(protocolFixtureJSON(input)))
	id := protocolFixtureStr(protocolFixtureMap(protocolFixtureObj(pending.Body.Bytes())["error"])["approval_id"])
	if pending.Code != 409 || id == "" || count.Load() != 0 {
		t.Fatalf("request approval was weakened: %d %s calls=%d", pending.Code, pending.Body.String(), count.Load())
	}
	approved := postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
	if approved.Code != 200 || count.Load() != 1 {
		t.Fatalf("approve=%d %s calls=%d", approved.Code, approved.Body.String(), count.Load())
	}
	again := postJSON(t, s, "/api/mcp/approvals/"+id+"/resume", `{}`)
	next := protocolFixtureStr(protocolFixtureMap(protocolFixtureObj(again.Body.Bytes())["error"])["approval_id"])
	if again.Code != 409 || next == "" || next == id || count.Load() != 1 {
		t.Fatalf("continuation approval was weakened: %d %s calls=%d", again.Code, again.Body.String(), count.Load())
	}
}
