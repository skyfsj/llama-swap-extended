package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/mcpruntime"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func TestServer_MCPStreamApprovalContinuation(t *testing.T) {
	remote, _ := mcpFixture(t)
	for _, format := range protocolFixtureFormats {
		t.Run(string(format), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				parsed, _, _ := dialect.DecodeRequest(dialect.OpenAIChat, body)
				hasResult := false
				for _, message := range parsed.Messages {
					if message.Role == dialect.RoleTool {
						hasResult = true
					}
				}
				response := &dialect.Response{ID: "stream", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartText, Text: "DONE"}}}
				if !hasResult {
					response.Parts = []dialect.Part{{Kind: dialect.PartToolCall, ToolCallID: "call_stream", ToolName: parsed.Tools[0].Name, Arguments: json.RawMessage(`{"q":"x"}`)}}
				}
				encoded, _, _ := dialect.EncodeResponse(response, dialect.OpenAIChat)
				events, _, _ := dialect.ConvertResponseToStream(dialect.OpenAIChat, dialect.OpenAIChat, encoded, dialect.StreamOptions{})
				w.Header().Set("Content-Type", "text/event-stream")
				for _, event := range events {
					fmt.Fprintf(w, "data: %s\n\n", event.Data)
				}
			}))
			defer backend.Close()
			cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL}}}}, Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			w := postJSON(t, s, protocolFixturePath(format, true), string(conversationFixtureInput(format, "CALL", true)))
			var detail map[string]any
			for _, event := range protocolStreamObjects(w.Body.Bytes()) {
				candidate, _ := event["error"].(map[string]any)
				if candidate["approval_id"] != nil {
					detail = candidate
				}
			}
			if detail == nil {
				detail = protocolFixtureMap(protocolFixtureObj(w.Body.Bytes())["error"])
			}
			id := protocolFixtureStr(detail["approval_id"])
			if id == "" {
				t.Fatalf("stream approval metadata missing: %d %s", w.Code, w.Body.String())
			}
			approved := postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
			if approved.Code != 200 {
				t.Fatalf("approve=%d %s", approved.Code, approved.Body.String())
			}
			resumed := postJSON(t, s, "/api/mcp/approvals/"+id+"/resume", `{}`)
			if resumed.Code != 200 || !strings.Contains(resumed.Header().Get("Content-Type"), "text/event-stream") || !bytes.Contains(resumed.Body.Bytes(), []byte("DONE")) {
				t.Fatalf("resume=%d %s %s", resumed.Code, resumed.Header().Get("Content-Type"), resumed.Body.String())
			}
		})
	}
}

func TestServer_MCPParallelAndClientApproval(t *testing.T) {
	for _, format := range protocolFixtureFormats {
		t.Run(string(format), func(t *testing.T) { testMCPParallelAndClientApproval(t, format) })
	}
}

func testMCPParallelAndClientApproval(t *testing.T, format dialect.Format) {
	remote, count := mcpFixture(t)
	before := count.Load()
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		parsed, _, _ := dialect.DecodeRequest(dialect.OpenAIChat, body)
		results := 0
		for _, message := range parsed.Messages {
			if message.Role == dialect.RoleTool {
				results++
			}
		}
		response := &dialect.Response{ID: "parallel", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartText, Text: "DONE"}}}
		if results == 0 {
			tool := mcpruntime.BoundName("fixture", "echo")
			response.Parts = []dialect.Part{{Kind: dialect.PartToolCall, ToolCallID: "a", ToolName: tool, Arguments: json.RawMessage(`{"q":"a"}`)}, {Kind: dialect.PartToolCall, ToolCallID: "b", ToolName: tool, Arguments: json.RawMessage(`{"q":"b"}`)}, {Kind: dialect.PartToolCall, ToolCallID: "c", ToolName: "client_tool", Arguments: json.RawMessage(`{}`)}}
		} else if results != 3 {
			t.Errorf("tool results=%d body=%s", results, body)
		}
		encoded, _, _ := dialect.EncodeResponse(response, dialect.OpenAIChat)
		w.Header().Set("Content-Type", "application/json")
		w.Write(encoded)
	}))
	defer backend.Close()
	cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL}}}}, Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
	s := multiSurfaceServer(t, cfg)
	request, _, err := dialect.ConvertRequest(dialect.OpenAIResponses, format, []byte(`{"model":"m","input":"CALL","tools":[{"type":"function","name":"client_tool","parameters":{"type":"object"}}]}`), dialect.ConvertOptions{})
	if err != nil {
		t.Fatal(err)
	}
	w := postJSON(t, s, protocolFixturePath(format, false), string(request))
	detail := protocolFixtureMap(protocolFixtureObj(w.Body.Bytes())["error"])
	ids := protocolFixtureArr(detail["approval_ids"])
	if w.Code != 409 || len(ids) != 2 || len(protocolFixtureArr(detail["client_tool_calls"])) != 1 {
		t.Fatalf("parallel approvals=%d %s", w.Code, w.Body.String())
	}
	first := protocolFixtureStr(ids[0])
	w = postJSON(t, s, "/api/mcp/approvals/"+first, `{"approve":true}`)
	if w.Code != 200 {
		t.Fatalf("approve=%d %s", w.Code, w.Body.String())
	}
	blocked := postJSON(t, s, "/api/mcp/approvals/"+first+"/resume", `{"tool_results":{"c":"CLIENT"}}`)
	if blocked.Code != 409 {
		t.Fatalf("unresolved parallel approval resumed: %d", blocked.Code)
	}
	w = postJSON(t, s, "/api/mcp/approvals/"+protocolFixtureStr(ids[1]), `{"approve":true}`)
	if w.Code != 200 {
		t.Fatalf("approve=%d %s", w.Code, w.Body.String())
	}
	missing := postJSON(t, s, "/api/mcp/approvals/"+first+"/resume", `{}`)
	if missing.Code != 409 {
		t.Fatalf("missing client result accepted: %d", missing.Code)
	}
	done := postJSON(t, s, "/api/mcp/approvals/"+first+"/resume", `{"tool_results":{"c":"CLIENT"}}`)
	if done.Code != 200 || !strings.Contains(done.Body.String(), "DONE") || count.Load() != before+2 {
		t.Fatalf("resume=%d count=%d %s", done.Code, count.Load()-before, done.Body.String())
	}
}

func TestServer_MCPApprovalNativeSidecar(t *testing.T) {
	remote, _ := mcpFixture(t)
	t.Setenv("MCP_FIXTURE_AUTH", "Bearer SECRET_VALUE")
	round := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		round++
		body, _ := io.ReadAll(r.Body)
		parsed, _, _ := dialect.DecodeRequest(dialect.ClaudeMessages, body)
		response := &dialect.Response{ID: "native", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartText, Text: "DONE"}}}
		if round == 1 {
			response.Parts = []dialect.Part{{Kind: dialect.PartReasoning, Text: "plan", Signature: "OPAQUE_APPROVAL"}, {Kind: dialect.PartToolCall, ToolCallID: "call_signed", ToolName: parsed.Tools[0].Name, Arguments: json.RawMessage(`{"q":"x"}`)}}
		} else if !bytes.Contains(body, []byte("OPAQUE_APPROVAL")) {
			t.Errorf("approval lost native state: %s", body)
		}
		encoded, _, _ := dialect.EncodeResponse(response, dialect.ClaudeMessages)
		w.Header().Set("Content-Type", "application/json")
		w.Write(encoded)
	}))
	defer backend.Close()
	cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"fixture": {Transport: "http", URL: remote.URL, Headers: map[string]string{"Authorization": "MCP_FIXTURE_AUTH"}}}}}, Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeAnthropic, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
	s := multiSurfaceServer(t, cfg)
	pending := postJSON(t, s, "/v1/chat/completions", `{"model":"m","messages":[{"role":"user","content":"FIRST"}]}`)
	detail := protocolFixtureMap(protocolFixtureObj(pending.Body.Bytes())["error"])
	id := protocolFixtureStr(detail["approval_id"])
	if pending.Code != 409 || id == "" {
		t.Fatalf("pending=%d %s", pending.Code, pending.Body.String())
	}
	saved, err := s.store.GetMCPApproval(t.Context(), id, "anonymous")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(saved.Resume, []byte("OPAQUE_APPROVAL")) || bytes.Contains(saved.Resume, []byte("SECRET_VALUE")) {
		t.Fatalf("invalid native sidecar")
	}
	approved := postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
	if approved.Code != 200 {
		t.Fatalf("approve=%d %s", approved.Code, approved.Body.String())
	}
	resumed := postJSON(t, s, "/api/mcp/approvals/"+id+"/resume", `{}`)
	if resumed.Code != 200 || round != 2 {
		t.Fatalf("resume=%d rounds=%d %s", resumed.Code, round, resumed.Body.String())
	}
}
