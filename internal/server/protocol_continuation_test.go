package server

import (
	"context"
	"fmt"
	"github.com/mostlygeek/llama-swap/internal/config"
	protocolrelaykit "github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/store"
	d "github.com/mostlygeek/llama-swap/pkg/dialect"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestServer_ProtocolToolPolicyMatrix(t *testing.T) {
	for _, source := range protocolFixtureFormats {
		for _, target := range protocolFixtureFormats {
			for _, mode := range []d.ToolChoiceMode{d.ToolChoiceAuto, d.ToolChoiceNone, d.ToolChoiceRequired, d.ToolChoiceTool} {
				t.Run(fmt.Sprintf("%s__%s/%s", source, target, mode), func(t *testing.T) {
					request, _, e := d.DecodeRequest(source, protocolFixtureRequest(source, false))
					if e != nil {
						t.Fatal(e)
					}
					request.ToolChoice = &d.ToolChoice{Mode: mode}
					if mode == d.ToolChoiceTool {
						request.ToolChoice.Name = protocolFixtureRead
					}
					native, _, e := d.EncodeRequest(request, source, d.ConvertOptions{Model: "m"})
					if e != nil {
						t.Fatal(e)
					}
					out, _, e := d.ConvertRequest(source, target, native, d.ConvertOptions{Model: "m"})
					if e != nil {
						t.Fatal(e)
					}
					r, _, e := d.DecodeRequest(target, out)
					if e != nil {
						t.Fatal(e)
					}
					if r.ToolChoice == nil || r.ToolChoice.Mode != mode {
						t.Errorf("policy=%+v want=%s body=%s", r.ToolChoice, mode, out)
					} else if mode == d.ToolChoiceTool && r.ToolChoice.Name != protocolFixtureRead {
						t.Errorf("forced tool=%+v", r.ToolChoice)
					}
				})
			}
		}
	}
}
func TestServer_ProtocolPreviousResponseToolOrder(t *testing.T) {
	for _, target := range []d.Format{d.OpenAIChat, d.ClaudeMessages, d.Gemini, d.GeminiInteractions} {
		t.Run(string(target), func(t *testing.T) {
			st, e := store.New("")
			if e != nil {
				t.Fatal(e)
			}
			defer st.Close()
			now := time.Now()
			resp := protocolFixtureResponse(d.OpenAIResponses)
			e = st.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{ResponseID: "resp_c", Model: "m", Status: "completed", Response: resp, ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now})
			if e != nil {
				t.Fatal(e)
			}
			body := []byte(`{"model":"m","previous_response_id":"resp_c","input":[{"type":"function_call_output","call_id":"a","output":"result-A"},{"type":"function_call_output","call_id":"b","output":"result-B"},{"role":"user","content":"CONTINUE"}]}`)
			out, e := convertProtocolRequest(context.Background(), protocolrelaykit.NewMeta("m", false), d.OpenAIResponses, target, body, "m", st)
			if e != nil {
				t.Fatal(e)
			}
			if target != d.OpenAIChat {
				out, _, e = d.ConvertRequest(target, d.OpenAIChat, out, d.ConvertOptions{Model: "m"})
				if e != nil {
					t.Fatal(e)
				}
			}
			protocolFixtureCheckChatOrder(t, out)
		})
	}
}
func TestServer_ProtocolPreviousResponseHistory(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	var bodies [][]byte
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"id":"c%d","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ANSWER_%d"},"finish_reason":"stop"}]}`, len(bodies), len(bodies))
	}
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m": {}}}
	s.routes()
	defer s.store.Close()
	previous := ""
	for i := 1; i <= 3; i++ {
		body := map[string]any{"model": "m", "store": true, "input": fmt.Sprintf("USER_%d", i)}
		if previous != "" {
			body["previous_response_id"] = previous
		}
		w := postJSON(t, s, "/v1/responses", string(protocolFixtureJSON(body)))
		if w.Code != 200 {
			t.Fatalf("turn%d status=%d body=%s", i, w.Code, w.Body.String())
		}
		previous = protocolFixtureStr(protocolFixtureObj(w.Body.Bytes())["id"])
	}
	if len(bodies) != 3 {
		t.Fatalf("rounds=%d", len(bodies))
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(string(bodies[2]), fmt.Sprintf("USER_%d", i)) {
			t.Errorf("third turn lost USER_%d history: %s", i, bodies[2])
		}
	}
	messages := protocolFixtureArr(protocolFixtureObj(bodies[1])["messages"])
	if len(messages) > 0 && protocolFixtureMap(messages[len(messages)-1])["content"] != "USER_2" {
		t.Errorf("current user should follow the previous assistant response: %s", bodies[1])
	}
}
func TestServer_ProtocolConversionFailover(t *testing.T) {
	hits := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		w.Write(protocolFixtureResponse(d.Gemini))
	}))
	defer backend.Close()
	cfg := multiSurfaceConfig(backend.URL, backend.URL)
	p := cfg.Providers["ocg"]
	p.Adapters[1].Type = config.ProviderTypeGemini
	cfg.Providers["ocg"] = p
	s := multiSurfaceServer(t, cfg)
	w := postJSON(t, s, "/v1/chat/completions", `{"model":"claude-haiku-5-5","messages":[{"role":"user","content":"hi"}],"n":2}`)
	if w.Code == 400 && hits == 0 {
		t.Errorf("first incompatible surface aborted before trying a compatible Gemini candidate: status=%d hits=%d body=%s", w.Code, hits, w.Body.String())
	}
}
func TestServer_ProtocolInteractionsTiming(t *testing.T) {
	for _, paced := range []bool{false, true} {
		t.Run(fmt.Sprintf("paced=%t", paced), func(t *testing.T) {
			backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				if !paced {
					w.Write(protocolFixtureEncodeSSE(protocolFixtureStream(d.GeminiInteractions)))
					return
				}
				for _, frame := range protocolFixtureStream(d.GeminiInteractions) {
					w.Write(protocolFixtureEncodeSSE([]d.Frame{frame}))
					w.(http.Flusher).Flush()
					time.Sleep(3 * time.Millisecond)
				}
			}))
			defer backend.Close()
			cfg := config.Config{Models: map[string]config.ModelConfig{}, Providers: config.ProviderDictionaryConfig{"protocolFixture": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "protocolFixture", Type: config.ProviderTypeGeminiInteractions, Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
			s := multiSurfaceServer(t, cfg)
			w := postJSON(t, s, "/v1/responses", `{"model":"m","input":"hi","stream":true}`)
			r := protocolFixtureCollect(d.OpenAIResponses, protocolFixtureParseSSE(w.Body.Bytes()))
			if r.text != "answer" {
				t.Errorf("valid Interactions stream lost depending on arrival timing paced=%t status=%d text=%q body=%s", paced, w.Code, r.text, w.Body.String())
			}
		})
	}
}
func TestServer_ProtocolGeminiParallelSameNameIDs(t *testing.T) {
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]},{"role":"model","parts":[{"functionCall":{"id":"a","name":"read","args":{"q":"x"}}},{"functionCall":{"id":"b","name":"read","args":{"q":"y"}}}]},{"role":"user","parts":[{"functionResponse":{"id":"b","name":"read","response":{"result":"result-B"}}},{"functionResponse":{"id":"a","name":"read","response":{"result":"result-A"}}}]}]}`)
	for _, target := range protocolFixtureFormats {
		if target == d.Gemini {
			continue
		}
		t.Run(string(target), func(t *testing.T) {
			out, _, e := d.ConvertRequest(d.Gemini, target, body, d.ConvertOptions{Model: "m"})
			if e != nil {
				t.Fatal(e)
			}
			r, _, e := d.DecodeRequest(target, out)
			if e != nil {
				t.Fatal(e)
			}
			calls := map[string]string{}
			results := map[string]string{}
			for _, m := range r.Messages {
				for _, p := range m.Parts {
					if p.Kind == d.PartToolCall {
						calls[p.ToolCallID] = protocolFixtureStr(protocolFixtureObj(p.Arguments)["q"])
					}
					if p.Kind == d.PartToolResult {
						for _, c := range p.Content {
							results[p.ToolCallID] += c.Text
						}
					}
				}
			}
			for id, q := range calls {
				want := "result-A"
				if q == "y" {
					want = "result-B"
				}
				if results[id] != want {
					t.Errorf("Gemini explicit ids ignored: args.q=%s matched result=%s, want=%s", q, results[id], want)
				}
			}
		})
	}
}

func TestServer_ProtocolStreamErrorMatrix(t *testing.T) {
	errors := map[d.Format]string{
		d.OpenAIChat:         `{"error":{"type":"server_error","code":"protocolFixture_error","message":"BOOM"}}`,
		d.OpenAIResponses:    `{"type":"response.failed","response":{"id":"r","status":"failed","error":{"code":"protocolFixture_error","message":"BOOM"}}}`,
		d.ClaudeMessages:     `{"type":"error","error":{"type":"api_error","message":"BOOM"}}`,
		d.Gemini:             `{"error":{"code":503,"status":"UNAVAILABLE","message":"BOOM"}}`,
		d.GeminiInteractions: `{"event_type":"error","error":{"code":"protocolFixture_error","message":"BOOM"}}`,
	}
	for source, body := range errors {
		for _, target := range protocolFixtureFormats {
			if source == target {
				continue
			}
			t.Run(string(source)+"__"+string(target), func(t *testing.T) {
				c, e := d.NewStreamConverter(source, target, d.StreamOptions{Model: "m"})
				if e != nil {
					t.Fatal(e)
				}
				fs, e := c.Feed([]byte(body))
				if e != nil {
					t.Fatal(e)
				}
				tail, e := c.Finish()
				if e != nil {
					t.Fatal(e)
				}
				all := protocolFixtureEncodeSSE(append(fs, tail...))
				if !strings.Contains(string(all), "BOOM") {
					t.Errorf("lost error: %s", all)
				}
				if strings.Contains(string(all), `"status":"completed"`) || strings.Contains(string(all), `"finishReason":"STOP"`) {
					t.Errorf("error converted into success: %s", all)
				}
			})
		}
	}
}

func TestServer_ProtocolRefusalTruncation(t *testing.T) {
	sources := map[d.Format][]string{d.OpenAIChat: {`{"id":"c","choices":[{"index":0,"delta":{"refusal":"cannot"}}]}`}, d.OpenAIResponses: {`{"type":"response.created","response":{"id":"resp_c","model":"m"}}`, `{"type":"response.refusal.delta","delta":"cannot"}`}}
	for source, frames := range sources {
		for _, target := range protocolFixtureFormats {
			if source == target {
				continue
			}
			t.Run(string(source)+"__"+string(target), func(t *testing.T) {
				c, _ := d.NewStreamConverter(source, target, d.StreamOptions{Model: "m"})
				for _, f := range frames {
					c.Feed([]byte(f))
				}
				fs, e := c.Finish()
				if e != nil {
					return
				}
				if len(c.Diagnostics()) == 0 {
					t.Errorf("refusal-only stream with missing terminal event was not diagnosed: %s", protocolFixtureEncodeSSE(fs))
				}
			})
		}
	}
}

func TestServer_ProtocolRichToolResultMatrix(t *testing.T) {
	for _, target := range protocolFixtureFormats {
		t.Run(string(target), func(t *testing.T) {
			r, _, e := d.DecodeRequest(d.ClaudeMessages, protocolFixtureRequest(d.ClaudeMessages, false))
			if e != nil {
				t.Fatal(e)
			}
			for i := range r.Messages {
				for j := range r.Messages[i].Parts {
					p := &r.Messages[i].Parts[j]
					if p.Kind == d.PartToolResult {
						p.Content = append(p.Content, d.Part{Kind: d.PartImage, MediaType: "image/png", Base64: "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+/l3sAAAAASUVORK5CYII="})
					}
				}
			}
			out, ds, e := d.EncodeRequest(r, target, d.ConvertOptions{Model: "m"})
			if e != nil {
				return
			}
			if target == d.OpenAIChat {
				for _, v := range protocolFixtureArr(protocolFixtureObj(out)["messages"]) {
					m := protocolFixtureMap(v)
					if m["role"] == "tool" {
						for _, part := range protocolFixtureArr(m["content"]) {
							if protocolFixtureMap(part)["type"] != "text" {
								t.Errorf("Chat tool messages carry a non-text block forbidden by the SDK schema: %s", protocolFixtureJSON(part))
							}
						}
					}
				}
			}
			if !strings.Contains(string(out), "iVBORw0") && len(ds) == 0 {
				t.Errorf("tool result image disappeared without diagnostic: %s", out)
			}
		})
	}
}

func TestServer_ProtocolForeignMCPArtifacts(t *testing.T) {
	for _, status := range []string{"completed", "in_progress", "failed"} {
		t.Run(status, func(t *testing.T) {
			request := protocolFixtureObj([]byte(fmt.Sprintf(`{"model":"m","tools":[{"type":"mcp","server_label":"external","server_url":"https://mcp.example.invalid/mcp","require_approval":"always"}],"input":[{"type":"mcp_call","id":"mcp1","server_label":"external","name":"read","arguments":"{}","status":%q,"approval_request_id":"approval1"},{"type":"mcp_approval_response","approval_request_id":"approval1","approve":false}]}`, status)))
			before := string(protocolFixtureJSON(request))
			changed := responsesFoldServerArtifacts(request)
			if changed || string(protocolFixtureJSON(request)) != before {
				t.Errorf("foreign native MCP history was converted into a local call/result, status=%s: %s", status, protocolFixtureJSON(request))
			}
		})
	}
}

func TestServer_ProtocolMultiCandidate(t *testing.T) {
	for _, source := range []d.Format{d.OpenAIChat, d.Gemini} {
		target := d.Gemini
		if source == d.Gemini {
			target = d.OpenAIChat
		}
		t.Run(string(source)+"__"+string(target), func(t *testing.T) {
			req := protocolFixtureObj(protocolFixtureRequest(source, false))
			if source == d.OpenAIChat {
				req["n"] = 2
			} else {
				protocolFixtureMap(req["generationConfig"])["candidateCount"] = 2
			}
			_, _, e := d.ConvertRequest(source, target, protocolFixtureJSON(req), d.ConvertOptions{Model: "m"})
			if e != nil {
				t.Logf("explicit rejection: %v", e)
				return
			}
			resp := protocolFixtureObj(protocolFixtureResponse(source))
			key := "choices"
			if source == d.Gemini {
				key = "candidates"
			}
			items := protocolFixtureArr(resp[key])
			second := protocolFixtureObj(protocolFixtureJSON(items[0]))
			second["index"] = 1
			resp[key] = append(items, second)
			out, _, e := d.ConvertResponse(source, target, protocolFixtureJSON(resp))
			if e != nil {
				t.Fatal(e)
			}
			key = "candidates"
			if target == d.OpenAIChat {
				key = "choices"
			}
			if len(protocolFixtureArr(protocolFixtureObj(out)[key])) != 2 {
				t.Errorf("n=2 was accepted but second candidate was discarded: %s", out)
			}
		})
	}
}
