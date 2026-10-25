package server

import (
	"fmt"
	"github.com/mostlygeek/llama-swap/internal/config"
	d "github.com/mostlygeek/llama-swap/pkg/dialect"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestServer_ProtocolMCPNamespaceRestore(t *testing.T) {
	body := []byte(`{"model":"m","input":"hi","tools":[{"type":"namespace","name":"mcp__protocolFixture","tools":[{"type":"function","name":"read","parameters":{"type":"object"}},{"type":"function","name":"write","parameters":{"type":"object"}}]}]}`)
	request, _, e := d.DecodeRequest(d.OpenAIResponses, body)
	if e != nil {
		t.Fatal(e)
	}
	for _, upstream := range protocolFixtureFormats {
		if upstream == d.OpenAIResponses {
			continue
		}
		t.Run(string(upstream), func(t *testing.T) {
			converted, _, e := d.ConvertRequest(d.OpenAIResponses, upstream, body, d.ConvertOptions{Model: "m"})
			if e != nil {
				t.Fatal(e)
			}
			r, _, e := d.DecodeRequest(upstream, converted)
			if e != nil {
				t.Fatal(e)
			}
			if len(r.Tools) != 2 || r.Tools[0].Name != protocolFixtureRead || r.Tools[1].Name != protocolFixtureWrite {
				t.Fatalf("namespaced tools lost: %s", converted)
			}
			out, _, e := d.ConvertResponseWithRequest(upstream, d.OpenAIResponses, protocolFixtureResponse(upstream), request)
			if e != nil {
				t.Fatal(e)
			}
			count := 0
			for _, v := range protocolFixtureArr(protocolFixtureObj(out)["output"]) {
				item := protocolFixtureMap(v)
				if item["type"] == "function_call" {
					count++
					if item["namespace"] != "mcp__protocolFixture" || (item["name"] != "read" && item["name"] != "write") {
						t.Errorf("namespace restore=%v", item)
					}
				}
			}
			if count != 2 {
				t.Errorf("calls=%d", count)
			}
		})
	}
}

func TestServer_ProtocolCustomDefaultNamespace(t *testing.T) {
	for _, namespace := range []string{"functions", "mcp__protocolFixture"} {
		for _, upstream := range protocolFixtureFormats {
			if upstream == d.OpenAIResponses {
				continue
			}
			t.Run(namespace+"/"+string(upstream), func(t *testing.T) {
				body := []byte(fmt.Sprintf(`{"model":"m","input":"hi","tools":[{"type":"namespace","name":%q,"tools":[{"type":"custom","name":"exec","format":{"type":"text"}}]}]}`, namespace))
				request, _, e := d.DecodeRequest(d.OpenAIResponses, body)
				if e != nil {
					t.Fatal(e)
				}
				name := "exec"
				if namespace != "functions" {
					name = namespace + "__exec"
				}
				b := []byte(fmt.Sprintf(`{"id":"c","model":"m","choices":[{"index":0,"message":{"tool_calls":[{"id":"call_x","type":"function","function":{"name":%q,"arguments":"{\"input\":\"ls\"}"}}]},"finish_reason":"tool_calls"}]}`, name))
				if upstream != d.OpenAIChat {
					b, _, e = d.ConvertResponse(d.OpenAIChat, upstream, b)
					if e != nil {
						t.Fatal(e)
					}
				}
				out, _, e := d.ConvertResponseWithRequest(upstream, d.OpenAIResponses, b, request)
				if e != nil {
					t.Fatal(e)
				}
				items := protocolFixtureArr(protocolFixtureObj(out)["output"])
				if len(items) != 1 {
					t.Fatalf("output=%s", out)
				}
				item := protocolFixtureMap(items[0])
				if item["type"] != "custom_tool_call" || item["input"] != "ls" {
					t.Errorf("custom namespace %s lost kind/input: %s", namespace, out)
				}
			})
		}
	}
}

func TestServer_ProtocolDuplicateBinding(t *testing.T) {
	for _, tools := range []string{
		`[{"type":"function","name":"exec"},{"type":"custom","name":"exec"}]`,
		`[{"type":"function","name":"mcp__protocolFixture__read"},{"type":"namespace","name":"mcp__protocolFixture","tools":[{"type":"function","name":"read"}]}]`,
	} {
		t.Run(tools, func(t *testing.T) {
			body := []byte(`{"model":"m","input":"hi","tools":` + tools + `}`)
			r, _, e := d.DecodeRequest(d.OpenAIResponses, body)
			if e != nil {
				t.Fatal(e)
			}
			if len(r.Tools) != 1 {
				t.Fatalf("tools=%v", r.Tools)
			}
			name := r.Tools[0].Name
			b := []byte(fmt.Sprintf(`{"id":"c","choices":[{"message":{"tool_calls":[{"id":"a","function":{"name":%q,"arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`, name))
			out, _, e := d.ConvertResponseWithRequest(d.OpenAIChat, d.OpenAIResponses, b, r)
			if e != nil {
				t.Fatal(e)
			}
			item := protocolFixtureMap(protocolFixtureArr(protocolFixtureObj(out)["output"])[0])
			if item["type"] != "function_call" || item["namespace"] != nil || item["name"] != name {
				t.Errorf("a dropped duplicate changed the surviving binding: %s", out)
			}
		})
	}
}

func protocolFixtureHostedRequest(f d.Format) []byte {
	switch f {
	case d.OpenAIResponses:
		return []byte(`{"model":"m","input":[{"role":"user","content":"hi"},{"type":"mcp_list_tools","id":"list1","server_label":"protocolFixture","tools":[{"name":"read","input_schema":{"type":"object"}}]},{"type":"mcp_approval_request","id":"apr1","server_label":"protocolFixture","name":"read","arguments":"{}"},{"type":"mcp_approval_response","approval_request_id":"apr1","approve":false}],"tools":[{"type":"mcp","server_label":"protocolFixture","server_url":"https://mcp.example.invalid/mcp","require_approval":"always"}]}`)
	case d.ClaudeMessages:
		return []byte(`{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"mcp_servers":[{"type":"url","name":"protocolFixture","url":"https://mcp.example.invalid/mcp"}],"tools":[{"type":"mcp_toolset","mcp_server_name":"protocolFixture"}]}`)
	case d.GeminiInteractions:
		return []byte(`{"model":"m","input":"hi","tools":[{"type":"mcp_server","name":"protocolFixture","url":"https://mcp.example.invalid/mcp"}]}`)
	}
	return nil
}
func TestServer_ProtocolHostedMCPRequestMatrix(t *testing.T) {
	for _, source := range []d.Format{d.OpenAIResponses, d.ClaudeMessages, d.GeminiInteractions} {
		for _, target := range protocolFixtureFormats {
			t.Run(string(source)+"__"+string(target), func(t *testing.T) {
				if source == target {
					w := httptest.NewRecorder()
					called := false
					h := CreateProtocolAdapterMiddleware(config.Config{Models: map[string]config.ModelConfig{}}, nil, func(string) (ProviderDialect, bool) {
						return ProviderDialect{Type: protocolFixtureSurface(target), Label: "protocolFixture"}, true
					})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						called = true
						b, _ := io.ReadAll(r.Body)
						if string(b) != string(protocolFixtureHostedRequest(source)) {
							t.Errorf("native MCP modified: %s", b)
						}
						w.Write(protocolFixtureResponse(source))
					}))
					r := httptest.NewRequest("POST", protocolFixturePath(source, false), strings.NewReader(string(protocolFixtureHostedRequest(source))))
					r.Header.Set("Content-Type", "application/json")
					h.ServeHTTP(w, r)
					if !called || w.Code != 200 {
						t.Errorf("native pass status=%d", w.Code)
					}
					return
				}
				out, ds, e := d.ConvertRequest(source, target, protocolFixtureHostedRequest(source), d.ConvertOptions{Model: "m"})
				if e != nil {
					t.Logf("explicit rejection: %v", e)
					return
				}
				if len(ds) == 0 {
					t.Errorf("hosted MCP silently lost: %s", out)
				}
				r, _, _ := d.DecodeRequest(target, out)
				if r != nil && len(r.Tools) != 0 {
					t.Errorf("hosted MCP falsely became an executable function: %+v", r.Tools)
				}
			})
		}
	}
}

func protocolFixtureHostedResponse(f d.Format) []byte {
	switch f {
	case d.OpenAIResponses:
		return []byte(`{"id":"resp_m","model":"m","status":"completed","output":[{"type":"mcp_list_tools","id":"list1","server_label":"protocolFixture","tools":[{"name":"read","input_schema":{"type":"object"}}]},{"type":"mcp_approval_request","id":"apr1","server_label":"protocolFixture","name":"read","arguments":"{}"},{"type":"mcp_call","id":"mcp1","server_label":"protocolFixture","name":"read","arguments":"{}","output":"MCP_RESULT","status":"completed"}]}`)
	case d.ClaudeMessages:
		return []byte(`{"id":"m","model":"m","type":"message","role":"assistant","content":[{"type":"mcp_tool_use","id":"mcp1","server_name":"protocolFixture","name":"read","input":{}},{"type":"mcp_tool_result","tool_use_id":"mcp1","content":[{"type":"text","text":"MCP_RESULT"}],"is_error":false}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	case d.GeminiInteractions:
		return []byte(`{"id":"m","model":"m","status":"completed","steps":[{"type":"mcp_server_tool_call","id":"mcp1","server_name":"protocolFixture","name":"read","arguments":{}},{"type":"mcp_server_tool_result","call_id":"mcp1","result":{"text":"MCP_RESULT"}}]}`)
	}
	return nil
}
func protocolFixtureHostedStream(f d.Format) []d.Frame {
	var frames []d.Frame
	add := func(v any) { frames = append(frames, d.Frame{Data: protocolFixtureJSON(v)}) }
	switch f {
	case d.OpenAIResponses:
		add(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_m", "model": "m"}})
		for i, item := range protocolFixtureArr(protocolFixtureObj(protocolFixtureHostedResponse(f))["output"]) {
			add(map[string]any{"type": "response.output_item.added", "output_index": i, "item": item})
			add(map[string]any{"type": "response.output_item.done", "output_index": i, "item": item})
		}
		add(map[string]any{"type": "response.mcp_call_arguments.delta", "output_index": 2, "delta": "{}"})
		add(map[string]any{"type": "response.mcp_call.completed", "output_index": 2})
		add(map[string]any{"type": "response.completed", "response": protocolFixtureObj(protocolFixtureHostedResponse(f))})
	case d.ClaudeMessages:
		add(map[string]any{"type": "message_start", "message": map[string]any{"id": "m", "model": "m", "content": []any{}}})
		for i, item := range protocolFixtureArr(protocolFixtureObj(protocolFixtureHostedResponse(f))["content"]) {
			add(map[string]any{"type": "content_block_start", "index": i, "content_block": item})
			add(map[string]any{"type": "content_block_stop", "index": i})
		}
		add(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}})
		add(map[string]any{"type": "message_stop"})
	case d.GeminiInteractions:
		add(map[string]any{"event_type": "interaction.created", "interaction": map[string]any{"id": "m", "model": "m"}})
		for i, item := range protocolFixtureArr(protocolFixtureObj(protocolFixtureHostedResponse(f))["steps"]) {
			add(map[string]any{"event_type": "step.start", "index": i, "step": item})
			add(map[string]any{"event_type": "step.stop", "index": i})
		}
		add(map[string]any{"event_type": "interaction.completed", "interaction": protocolFixtureObj(protocolFixtureHostedResponse(f))})
	}
	return frames
}
func TestServer_ProtocolHostedMCPResponseStreamMatrix(t *testing.T) {
	for _, source := range []d.Format{d.OpenAIResponses, d.ClaudeMessages, d.GeminiInteractions} {
		for _, target := range protocolFixtureFormats {
			if source == target {
				continue
			}
			t.Run("whole/"+string(source)+"__"+string(target), func(t *testing.T) {
				_, ds, e := d.ConvertResponse(source, target, protocolFixtureHostedResponse(source))
				if e == nil && len(ds) == 0 {
					t.Error("hosted MCP response disappeared without rejection or diagnostic")
				}
			})
			t.Run("stream/"+string(source)+"__"+string(target), func(t *testing.T) {
				c, e := d.NewStreamConverter(source, target, d.StreamOptions{Model: "m"})
				if e != nil {
					t.Fatal(e)
				}
				for _, f := range protocolFixtureHostedStream(source) {
					if _, e := c.Feed(f.Data); e != nil {
						t.Logf("rejected: %v", e)
						return
					}
				}
				_, e = c.Finish()
				if e == nil && len(c.Diagnostics()) == 0 {
					t.Error("hosted MCP stream disappeared without rejection or diagnostic")
				}
			})
		}
	}
}

func TestServer_ProtocolEmptyToolResultMatrix(t *testing.T) {
	for _, source := range protocolFixtureFormats {
		t.Run(string(source), func(t *testing.T) {
			r, _, e := d.DecodeRequest(source, protocolFixtureRequest(source, false))
			if e != nil {
				t.Fatal(e)
			}
			for i := range r.Messages {
				for j := range r.Messages[i].Parts {
					if r.Messages[i].Parts[j].Kind == d.PartToolResult {
						r.Messages[i].Parts[j].Content = nil
					}
				}
			}
			out, _, e := d.EncodeRequest(r, d.OpenAIChat, d.ConvertOptions{Model: "m"})
			if e != nil {
				t.Fatal(e)
			}
			for _, v := range protocolFixtureArr(protocolFixtureObj(out)["messages"]) {
				m := protocolFixtureMap(v)
				if m["role"] == "tool" && m["content"] == nil {
					t.Errorf("empty tool result became null content: %s", out)
					return
				}
			}
		})
	}
}
func TestServer_ProtocolExtensionsForeignMCPHTTP(t *testing.T) {
	var captured []byte
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		captured, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.Write(protocolFixtureResponse(d.OpenAIResponses))
	}))
	defer backend.Close()
	cfg := config.Config{Models: map[string]config.ModelConfig{}, Providers: config.ProviderDictionaryConfig{"protocolFixture": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "protocolFixture", Type: config.ProviderTypeOpenAIResponses, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
	s := multiSurfaceServer(t, cfg)
	template := extensionTestServer(t, newStubRouter([]string{"local-model"}, ""))
	s.extensions = template.extensions
	s.cfg.Extensions = template.cfg.Extensions
	s.routes()
	body := `{"model":"m","input":[{"type":"mcp_call","id":"mcp_remote","server_label":"external","name":"read","arguments":"{}","output":"REMOTE_RESULT","status":"completed"},{"role":"user","content":"continue"}],"tools":[{"type":"mcp","server_label":"external","server_url":"https://mcp.example.invalid/mcp","require_approval":"always"}]}`
	w := postJSON(t, s, "/v1/responses", body)
	if w.Code != 200 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	for _, v := range protocolFixtureArr(protocolFixtureObj(captured)["input"]) {
		item := protocolFixtureMap(v)
		if item["type"] == "mcp_call" && item["server_label"] == "external" {
			return
		}
	}
	t.Errorf("enabling an unrelated extension rewrote native remote MCP history: %s", captured)
}
func TestServer_ProtocolNativeMCPApproval(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			response := protocolFixtureObj([]byte(`{"id":"resp_approval","object":"response","model":"m","status":"completed","output":[{"type":"mcp_approval_request","id":"apr1","server_label":"protocolFixture","name":"read","arguments":"{}"}]}`))
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !stream {
					w.Header().Set("Content-Type", "application/json")
					w.Write(protocolFixtureJSON(response))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				frames := []d.Frame{{Event: "response.created", Data: protocolFixtureJSON(map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_approval", "status": "in_progress", "output": []any{}}})}, {Event: "response.output_item.added", Data: protocolFixtureJSON(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": protocolFixtureArr(response["output"])[0]})}, {Event: "response.output_item.done", Data: protocolFixtureJSON(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": protocolFixtureArr(response["output"])[0]})}, {Event: "response.completed", Data: protocolFixtureJSON(map[string]any{"type": "response.completed", "response": response})}}
				w.Write(protocolFixtureEncodeSSE(frames))
			}))
			defer backend.Close()
			cfg := config.Config{Models: map[string]config.ModelConfig{}, Providers: config.ProviderDictionaryConfig{"protocolFixture": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "protocolFixture", Type: config.ProviderTypeOpenAIResponses, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			request := protocolFixtureObj(protocolFixtureHostedRequest(d.OpenAIResponses))
			request["input"] = "hi"
			request["stream"] = stream
			w := postJSON(t, s, "/v1/responses", string(protocolFixtureJSON(request)))
			if w.Code != 200 {
				t.Errorf("approval status=%d body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), `"type":"mcp_approval_request"`) {
				t.Errorf("native MCP approval request was suppressed by the gateway: %s", w.Body.String())
			}
			if !stream && len(protocolFixtureArr(protocolFixtureObj(w.Body.Bytes())["output"])) != 1 {
				t.Errorf("native approval response envelope was replaced: %s", w.Body.String())
			}
		})
	}
}
func protocolFixtureFinalStream(f d.Format) []d.Frame {
	var fs []d.Frame
	add := func(event string, v any) { fs = append(fs, d.Frame{Event: event, Data: protocolFixtureJSON(v)}) }
	switch f {
	case d.OpenAIChat:
		add("", protocolFixtureObj([]byte(`{"id":"final","model":"m","choices":[{"index":0,"delta":{"content":"DONE"},"finish_reason":"stop"}]}`)))
	case d.OpenAIResponses:
		add("response.created", protocolFixtureObj([]byte(`{"type":"response.created","response":{"id":"resp_final","model":"m"}}`)))
		add("response.output_text.delta", protocolFixtureObj([]byte(`{"type":"response.output_text.delta","output_index":0,"delta":"DONE"}`)))
		add("response.completed", protocolFixtureObj([]byte(`{"type":"response.completed","response":{"id":"resp_final","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"DONE"}]}]}}`)))
	case d.ClaudeMessages:
		add("message_start", protocolFixtureObj([]byte(`{"type":"message_start","message":{"id":"final","model":"m","type":"message","role":"assistant","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`)))
		add("content_block_start", protocolFixtureObj([]byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)))
		add("content_block_delta", protocolFixtureObj([]byte(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"DONE"}}`)))
		add("content_block_stop", protocolFixtureObj([]byte(`{"type":"content_block_stop","index":0}`)))
		add("message_delta", protocolFixtureObj([]byte(`{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)))
		add("message_stop", protocolFixtureObj([]byte(`{"type":"message_stop"}`)))
	case d.Gemini:
		add("", protocolFixtureObj([]byte(`{"modelVersion":"m","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"DONE"}]},"finishReason":"STOP"}]}`)))
	case d.GeminiInteractions:
		add("interaction.created", protocolFixtureObj([]byte(`{"event_type":"interaction.created","interaction":{"id":"final","model":"m","status":"in_progress"}}`)))
		add("step.start", protocolFixtureObj([]byte(`{"event_type":"step.start","index":0,"step":{"type":"model_output","content":[]}}`)))
		add("step.delta", protocolFixtureObj([]byte(`{"event_type":"step.delta","index":0,"delta":{"type":"text","text":"DONE"}}`)))
		add("step.stop", protocolFixtureObj([]byte(`{"event_type":"step.stop","index":0}`)))
		add("interaction.completed", protocolFixtureObj([]byte(`{"event_type":"interaction.completed","interaction":{"id":"final","model":"m","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"DONE"}]}]}}`)))
	}
	return fs
}
func protocolFixtureFinalResponse(f d.Format) []byte {
	o := protocolFixtureObj(protocolFixtureResponse(f))
	switch f {
	case d.OpenAIChat:
		o["choices"] = []any{map[string]any{"index": 0, "message": map[string]any{"role": "assistant", "content": "DONE"}, "finish_reason": "stop"}}
	case d.OpenAIResponses:
		o["output"] = []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "DONE"}}}}
	case d.ClaudeMessages:
		o["content"] = []any{map[string]any{"type": "text", "text": "DONE"}}
		o["stop_reason"] = "end_turn"
	case d.Gemini:
		o["candidates"] = []any{map[string]any{"index": 0, "content": map[string]any{"role": "model", "parts": []any{map[string]any{"text": "DONE"}}}, "finishReason": "STOP"}}
	case d.GeminiInteractions:
		o["status"] = "completed"
		o["steps"] = []any{map[string]any{"type": "model_output", "content": []any{map[string]any{"type": "text", "text": "DONE"}}}}
	}
	return protocolFixtureJSON(o)
}
func TestServer_ProtocolExtensionExecutionMatrix(t *testing.T) {
	for _, client := range protocolFixtureFormats {
		if client == d.GeminiInteractions {
			continue
		}
		for _, upstream := range protocolFixtureFormats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s__%s/stream=%t", client, upstream, stream), func(t *testing.T) {
					rounds := 0
					seenResults := false
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						rounds++
						b, _ := io.ReadAll(r.Body)
						if rounds == 2 {
							req, _, e := d.DecodeRequest(upstream, b)
							if e != nil {
								t.Error(e)
							}
							results := ""
							if req != nil {
								for _, m := range req.Messages {
									for _, p := range m.Parts {
										if p.Kind == d.PartToolResult {
											for _, c := range p.Content {
												results += c.Text
											}
										}
									}
								}
							}
							seenResults = strings.Contains(results, "found:x") && strings.Contains(results, "found:y")
							if !seenResults {
								t.Errorf("continuation lost executed tool results: %s", b)
							}
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(200)
							w.(http.Flusher).Flush()
							fs := protocolFixtureFinalStream(upstream)
							if rounds == 1 {
								fs = protocolFixtureStream(upstream)
							}
							encoded := string(protocolFixtureEncodeSSE(fs))
							encoded = strings.ReplaceAll(strings.ReplaceAll(encoded, protocolFixtureRead, "lookup"), protocolFixtureWrite, "lookup")
							io.WriteString(w, encoded)
							return
						}
						w.Header().Set("Content-Type", "application/json")
						out := protocolFixtureFinalResponse(upstream)
						if rounds == 1 {
							out = protocolFixtureResponse(upstream)
						}
						io.WriteString(w, strings.ReplaceAll(strings.ReplaceAll(string(out), protocolFixtureRead, "lookup"), protocolFixtureWrite, "lookup"))
					}))
					defer backend.Close()
					cfg := config.Config{Models: map[string]config.ModelConfig{}, Providers: config.ProviderDictionaryConfig{"protocolFixture": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "protocolFixture", Type: protocolFixtureSurface(upstream), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
					s := multiSurfaceServer(t, cfg)
					template := extensionTestServer(t, newStubRouter([]string{"local-model"}, ""))
					s.extensions = template.extensions
					s.cfg.Extensions = template.cfg.Extensions
					s.routes()
					req := protocolFixtureObj(protocolFixtureRequest(client, stream))
					switch client {
					case d.OpenAIChat:
						req["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
					case d.OpenAIResponses:
						req["input"] = "hi"
					case d.ClaudeMessages:
						req["messages"] = []any{map[string]any{"role": "user", "content": "hi"}}
					case d.Gemini:
						req["contents"] = []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}}
					}
					delete(req, "tools")
					w := postJSON(t, s, protocolFixturePath(client, stream), string(protocolFixtureJSON(req)))
					if w.Code != 200 || rounds != 2 || !seenResults || !strings.Contains(w.Body.String(), "DONE") {
						t.Errorf("extension execution failed: status=%d upstreamRounds=%d resultsReplayed=%t body=%s", w.Code, rounds, seenResults, w.Body.String())
					}
				})
			}
		}
	}
}
