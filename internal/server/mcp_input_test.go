package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func TestServer_MCPInputModelContinuation(t *testing.T) {
	for _, format := range protocolFixtureFormats {
		for _, stream := range []bool{false, true} {
			for _, approval := range []string{"never", "always"} {
				t.Run(fmt.Sprintf("%s/stream=%t/%s", format, stream, approval), func(t *testing.T) {
					var executions atomic.Int32
					remote := mcp.NewServer(&mcp.Implementation{Name: "input", Version: "1"}, nil)
					remote.AddTool(&mcp.Tool{Name: "confirm", InputSchema: map[string]any{"type": "object"}}, func(ctx context.Context, req *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
						executions.Add(1)
						if req.Params.RequestState == "opaque-two" && req.Params.InputResponses["confirm"] != nil {
							return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ACCEPTED"}}}, nil
						}
						state := "opaque-one"
						if req.Params.RequestState == "opaque-one" {
							state = "opaque-two"
						}
						return &mcp.CallToolResult{RequestState: state, InputRequests: mcp.InputRequestMap{"confirm": &mcp.ElicitParams{Mode: "form", Message: "Confirm", RequestedSchema: map[string]any{"type": "object", "required": []string{"ok"}, "properties": map[string]any{"ok": map[string]any{"type": "boolean"}}}}}}, nil
					})
					host := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{Stateless: true}))
					defer host.Close()
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						body, _ := io.ReadAll(r.Body)
						input, _, err := dialect.DecodeRequest(dialect.OpenAIChat, body)
						if err != nil {
							t.Error(err)
							w.WriteHeader(400)
							return
						}
						done := false
						for _, message := range input.Messages {
							if message.Role == dialect.RoleTool {
								done = true
							}
						}
						response := &dialect.Response{ID: "input-model", Model: "m", Status: "completed", Parts: []dialect.Part{{Kind: dialect.PartText, Text: "DONE"}}}
						if !done {
							response.Parts = []dialect.Part{{Kind: dialect.PartToolCall, ToolCallID: "needs-input", ToolName: input.Tools[0].Name, Arguments: json.RawMessage(`{}`)}}
						}
						encoded, _, _ := dialect.EncodeResponse(response, dialect.OpenAIChat)
						if input.Stream {
							events, _, _ := dialect.ConvertResponseToStream(dialect.OpenAIChat, dialect.OpenAIChat, encoded, dialect.StreamOptions{})
							w.Header().Set("Content-Type", "text/event-stream")
							for _, event := range events {
								fmt.Fprintf(w, "data: %s\n\n", event.Data)
							}
						} else {
							w.Header().Set("Content-Type", "application/json")
							w.Write(encoded)
						}
					}))
					defer backend.Close()
					cfg := config.Config{ProtocolRuntime: config.ProtocolRuntimeConfig{MCP: config.MCPRuntimeConfig{Enabled: true, Servers: map[string]config.MCPServerConfig{"input": {Transport: "http", URL: host.URL, Approval: approval}}}}, Providers: config.ProviderDictionaryConfig{"p": {ApiKeys: []config.ProviderKey{{Value: "x"}}, Adapters: []config.ProviderAdapter{{Name: "a", Type: config.ProviderTypeOpenAI, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
					s := multiSurfaceServer(t, cfg)
					pending := postJSON(t, s, protocolFixturePath(format, stream), string(conversationFixtureInput(format, "CALL", stream)))
					var detail map[string]any
					for _, event := range protocolStreamObjects(pending.Body.Bytes()) {
						if candidate := protocolFixtureMap(event["error"]); candidate["approval_id"] != nil {
							detail = candidate
						}
					}
					if detail == nil {
						detail = protocolFixtureMap(protocolFixtureObj(pending.Body.Bytes())["error"])
					}
					id := protocolFixtureStr(detail["approval_id"])
					if id == "" {
						t.Fatalf("missing continuation: %d %s", pending.Code, pending.Body.String())
					}
					if approval == "always" {
						approved := postJSON(t, s, "/api/mcp/approvals/"+id, `{"approve":true}`)
						if approved.Code != 200 {
							t.Fatalf("approve: %s", approved.Body.String())
						}
					}
					invalid := postJSON(t, s, "/api/mcp/approvals/"+id, `{"input_responses":{"confirm":{"action":"accept","content":{"ok":"wrong"}}}}`)
					if invalid.Code != 400 || executions.Load() != 1 {
						t.Fatalf("invalid input: %d calls=%d %s", invalid.Code, executions.Load(), invalid.Body.String())
					}
					for round := 0; round < 2; round++ {
						accepted := postJSON(t, s, "/api/mcp/approvals/"+id, `{"input_responses":{"confirm":{"action":"accept","content":{"ok":true}}}}`)
						if accepted.Code != 200 {
							t.Fatalf("input: %d %s", accepted.Code, accepted.Body.String())
						}
					}
					resumed := postJSON(t, s, "/api/mcp/approvals/"+id+"/resume", `{}`)
					if resumed.Code != 200 || !bytes.Contains(resumed.Body.Bytes(), []byte("DONE")) || executions.Load() != 3 {
						t.Fatalf("resume: %d calls=%d %s", resumed.Code, executions.Load(), resumed.Body.String())
					}
					duplicate := postJSON(t, s, "/api/mcp/approvals/"+id+"/resume", `{}`)
					if duplicate.Code != 409 || executions.Load() != 3 {
						t.Fatalf("duplicate: %d calls=%d", duplicate.Code, executions.Load())
					}
				})
			}
		}
	}
}
