package server

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	d "github.com/mostlygeek/llama-swap/pkg/dialect"
)

var protocolFixtureFormats = []d.Format{d.OpenAIChat, d.OpenAIResponses, d.ClaudeMessages, d.Gemini, d.GeminiInteractions}

const protocolFixtureRead = "mcp__protocolFixture__read"
const protocolFixtureWrite = "mcp__protocolFixture__write"

func protocolFixtureJSON(v any) []byte {
	b, e := json.Marshal(v)
	if e != nil {
		panic(e)
	}
	return b
}
func protocolFixtureObj(b []byte) map[string]any {
	var v map[string]any
	_ = json.Unmarshal(b, &v)
	return v
}
func protocolFixtureMap(v any) map[string]any { m, _ := v.(map[string]any); return m }
func protocolFixtureArr(v any) []any          { a, _ := v.([]any); return a }
func protocolFixtureStr(v any) string         { s, _ := v.(string); return s }
func protocolFixtureInt(v any) int            { f, _ := v.(float64); return int(f) }

func protocolFixtureRequest(f d.Format, stream bool) []byte {
	schema := map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "string"}}, "required": []string{"q"}}
	var x map[string]any
	switch f {
	case d.OpenAIChat:
		x = protocolFixtureObj([]byte(`{"model":"m","messages":[{"role":"system","content":"SYS"},{"role":"user","content":"BEGIN"},{"role":"assistant","content":"checking","tool_calls":[{"id":"a","type":"function","function":{"name":"mcp__protocolFixture__read","arguments":"{\"q\":\"x\"}"}},{"id":"b","type":"function","function":{"name":"mcp__protocolFixture__write","arguments":"{\"q\":\"y\"}"}}]},{"role":"tool","tool_call_id":"b","content":"result-B"},{"role":"tool","tool_call_id":"a","content":"result-A"},{"role":"user","content":"CONTINUE"}],"max_tokens":128}`))
		x["tools"] = []any{map[string]any{"type": "function", "function": map[string]any{"name": protocolFixtureRead, "parameters": schema}}, map[string]any{"type": "function", "function": map[string]any{"name": protocolFixtureWrite, "parameters": schema}}}
	case d.OpenAIResponses:
		x = protocolFixtureObj([]byte(`{"model":"m","instructions":"SYS","input":[{"role":"user","content":"BEGIN"},{"role":"assistant","content":"checking"},{"type":"function_call","call_id":"a","name":"mcp__protocolFixture__read","arguments":"{\"q\":\"x\"}"},{"type":"function_call","call_id":"b","name":"mcp__protocolFixture__write","arguments":"{\"q\":\"y\"}"},{"type":"function_call_output","call_id":"b","output":"result-B"},{"type":"function_call_output","call_id":"a","output":"result-A"},{"role":"user","content":"CONTINUE"}],"max_output_tokens":128}`))
		x["tools"] = []any{map[string]any{"type": "function", "name": protocolFixtureRead, "parameters": schema}, map[string]any{"type": "function", "name": protocolFixtureWrite, "parameters": schema}}
	case d.ClaudeMessages:
		x = protocolFixtureObj([]byte(`{"model":"m","system":"SYS","max_tokens":128,"messages":[{"role":"user","content":"BEGIN"},{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"tool_use","id":"a","name":"mcp__protocolFixture__read","input":{"q":"x"}},{"type":"tool_use","id":"b","name":"mcp__protocolFixture__write","input":{"q":"y"}}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"b","content":"result-B"},{"type":"tool_result","tool_use_id":"a","content":"result-A"}]},{"role":"user","content":"CONTINUE"}]}`))
		x["tools"] = []any{map[string]any{"name": protocolFixtureRead, "input_schema": schema}, map[string]any{"name": protocolFixtureWrite, "input_schema": schema}}
	case d.Gemini:
		x = protocolFixtureObj([]byte(`{"systemInstruction":{"parts":[{"text":"SYS"}]},"contents":[{"role":"user","parts":[{"text":"BEGIN"}]},{"role":"model","parts":[{"text":"checking"},{"functionCall":{"name":"mcp__protocolFixture__read","args":{"q":"x"}}},{"functionCall":{"name":"mcp__protocolFixture__write","args":{"q":"y"}}}]},{"role":"user","parts":[{"functionResponse":{"name":"mcp__protocolFixture__write","response":{"result":"result-B"}}},{"functionResponse":{"name":"mcp__protocolFixture__read","response":{"result":"result-A"}}},{"text":"CONTINUE"}]}],"generationConfig":{"maxOutputTokens":128}}`))
		x["tools"] = []any{map[string]any{"functionDeclarations": []any{map[string]any{"name": protocolFixtureRead, "parameters": schema}, map[string]any{"name": protocolFixtureWrite, "parameters": schema}}}}
	case d.GeminiInteractions:
		x = protocolFixtureObj([]byte(`{"model":"m","system_instruction":"SYS","input":[{"type":"user_input","content":[{"type":"text","text":"BEGIN"}]},{"type":"model_output","content":[{"type":"text","text":"checking"}]},{"type":"function_call","id":"a","name":"mcp__protocolFixture__read","arguments":{"q":"x"}},{"type":"function_call","id":"b","name":"mcp__protocolFixture__write","arguments":{"q":"y"}},{"type":"function_result","call_id":"b","name":"mcp__protocolFixture__write","result":"result-B"},{"type":"function_result","call_id":"a","name":"mcp__protocolFixture__read","result":"result-A"},{"type":"user_input","content":[{"type":"text","text":"CONTINUE"}]}],"generation_config":{"max_output_tokens":128}}`))
		x["tools"] = []any{map[string]any{"type": "function", "name": protocolFixtureRead, "parameters": schema}, map[string]any{"type": "function", "name": protocolFixtureWrite, "parameters": schema}}
	}
	if f != d.Gemini {
		x["stream"] = stream
	}
	return protocolFixtureJSON(x)
}

func protocolFixtureCheckRequest(t *testing.T, f d.Format, b []byte) {
	t.Helper()
	if f == d.OpenAIChat {
		protocolFixtureCheckChatOrder(t, b)
	}
	r, ds, e := d.DecodeRequest(f, b)
	if e != nil {
		t.Fatalf("decode %s: %v body=%s", f, e, b)
	}
	if len(r.Tools) != 2 {
		t.Errorf("tool definitions=%d want 2; body=%s diag=%v", len(r.Tools), b, ds)
	}
	calls := map[string]d.Part{}
	results := map[string]string{}
	texts := ""
	for _, p := range r.System {
		texts += p.Text
	}
	for _, m := range r.Messages {
		for _, p := range m.Parts {
			texts += p.Text
			switch p.Kind {
			case d.PartToolCall:
				calls[p.ToolCallID] = p
			case d.PartToolResult:
				for _, c := range p.Content {
					results[p.ToolCallID] += c.Text
				}
			}
		}
	}
	if len(calls) != 2 || len(results) != 2 {
		t.Errorf("calls=%d results=%d body=%s", len(calls), len(results), b)
	}
	for id, p := range calls {
		want := "result-A"
		q := "x"
		if p.ToolName == protocolFixtureWrite {
			want = "result-B"
			q = "y"
		} else if p.ToolName != protocolFixtureRead {
			t.Errorf("unexpected name %q", p.ToolName)
		}
		if results[id] != want {
			t.Errorf("pairing name=%s id=%s result=%q want=%q", p.ToolName, id, results[id], want)
		}
		if protocolFixtureObj(p.Arguments)["q"] != q {
			t.Errorf("args=%s", p.Arguments)
		}
	}
	for _, want := range []string{"SYS", "BEGIN", "checking", "CONTINUE"} {
		if !strings.Contains(texts, want) {
			t.Errorf("lost text %q body=%s", want, b)
		}
	}
}
func protocolFixtureCheckChatOrder(t *testing.T, b []byte) {
	t.Helper()
	pending := map[string]bool{}
	for i, v := range protocolFixtureArr(protocolFixtureObj(b)["messages"]) {
		m := protocolFixtureMap(v)
		role := protocolFixtureStr(m["role"])
		if role != "tool" && len(pending) > 0 {
			t.Errorf("message %d role=%s arrives before tool results for %v", i, role, pending)
		}
		if role == "assistant" {
			for _, v := range protocolFixtureArr(m["tool_calls"]) {
				pending[protocolFixtureStr(protocolFixtureMap(v)["id"])] = true
			}
		}
		if role == "tool" {
			id := protocolFixtureStr(m["tool_call_id"])
			if !pending[id] {
				t.Errorf("message %d tool output %s precedes its call or duplicates a result", i, id)
			}
			delete(pending, id)
		}
	}
	if len(pending) > 0 {
		t.Errorf("unanswered tool calls=%v", pending)
	}
}

func protocolFixtureResponse(f d.Format) []byte {
	switch f {
	case d.OpenAIChat:
		return []byte(`{"id":"c","model":"m","created":1,"object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"answer","tool_calls":[{"id":"a","type":"function","function":{"name":"mcp__protocolFixture__read","arguments":"{\"q\":\"x\"}"}},{"id":"b","type":"function","function":{"name":"mcp__protocolFixture__write","arguments":"{\"q\":\"y\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)
	case d.OpenAIResponses:
		return []byte(`{"id":"resp_c","model":"m","created_at":1,"object":"response","status":"completed","output":[{"id":"msg_a","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"answer","annotations":[]}]},{"id":"fc_a","type":"function_call","call_id":"a","name":"mcp__protocolFixture__read","arguments":"{\"q\":\"x\"}","status":"completed"},{"id":"fc_b","type":"function_call","call_id":"b","name":"mcp__protocolFixture__write","arguments":"{\"q\":\"y\"}","status":"completed"}],"usage":{"input_tokens":7,"output_tokens":5,"total_tokens":12}}`)
	case d.ClaudeMessages:
		return []byte(`{"id":"c","model":"m","type":"message","role":"assistant","content":[{"type":"text","text":"answer"},{"type":"tool_use","id":"a","name":"mcp__protocolFixture__read","input":{"q":"x"}},{"type":"tool_use","id":"b","name":"mcp__protocolFixture__write","input":{"q":"y"}}],"stop_reason":"tool_use","usage":{"input_tokens":7,"output_tokens":5}}`)
	case d.Gemini:
		return []byte(`{"modelVersion":"m","candidates":[{"index":0,"content":{"role":"model","parts":[{"text":"answer"},{"functionCall":{"name":"mcp__protocolFixture__read","args":{"q":"x"}}},{"functionCall":{"name":"mcp__protocolFixture__write","args":{"q":"y"}}}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":7,"candidatesTokenCount":5,"totalTokenCount":12}}`)
	case d.GeminiInteractions:
		return []byte(`{"id":"c","model":"m","status":"requires_action","created":"2026-10-19T00:00:00Z","steps":[{"type":"model_output","content":[{"type":"text","text":"answer"}]},{"type":"function_call","id":"a","name":"mcp__protocolFixture__read","arguments":{"q":"x"}},{"type":"function_call","id":"b","name":"mcp__protocolFixture__write","arguments":{"q":"y"}}],"usage":{"total_input_tokens":7,"total_output_tokens":5,"total_tokens":12}}`)
	}
	return nil
}
func protocolFixtureCheckResponse(t *testing.T, f d.Format, b []byte) {
	t.Helper()
	r, ds, e := d.DecodeResponse(f, b)
	if e != nil {
		t.Fatalf("decode response: %v", e)
	}
	txt := ""
	calls := map[string]string{}
	for _, p := range r.Parts {
		if p.Kind == d.PartText {
			txt += p.Text
		}
		if p.Kind == d.PartToolCall {
			calls[p.ToolName] = protocolFixtureStr(protocolFixtureObj(p.Arguments)["q"])
		}
	}
	if txt != "answer" || calls[protocolFixtureRead] != "x" || calls[protocolFixtureWrite] != "y" || len(calls) != 2 {
		t.Errorf("response lost semantic content text=%q calls=%v body=%s diag=%v", txt, calls, b, ds)
	}
	if r.Usage.InputTokens != 7 || r.Usage.OutputTokens != 5 || r.Usage.TotalTokens != 12 {
		t.Errorf("usage=%+v", r.Usage)
	}
	if r.FinishReason != d.FinishToolUse {
		t.Errorf("finish=%s body=%s", r.FinishReason, b)
	}
}

func protocolFixtureStream(f d.Format) []d.Frame {
	var out []d.Frame
	add := func(event string, v any) { out = append(out, d.Frame{Event: event, Data: protocolFixtureJSON(v)}) }
	switch f {
	case d.OpenAIChat:
		add("", protocolFixtureObj([]byte(`{"id":"c","model":"m","created":1,"choices":[{"index":0,"delta":{"content":"answer","tool_calls":[{"index":0,"id":"a","type":"function","function":{"name":"mcp__protocolFixture__read","arguments":"{\"q\":"}},{"index":1,"id":"b","type":"function","function":{"name":"mcp__protocolFixture__write","arguments":"{\"q\":"}}]}}]}`)))
		add("", protocolFixtureObj([]byte(`{"id":"c","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"y\"}"}},{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`)))
		add("", protocolFixtureObj([]byte(`{"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":7,"completion_tokens":5,"total_tokens":12}}`)))
	case d.OpenAIResponses:
		add("response.created", map[string]any{"type": "response.created", "response": map[string]any{"id": "resp_c", "model": "m", "created_at": 1}})
		add("response.output_text.delta", map[string]any{"type": "response.output_text.delta", "output_index": 0, "delta": "answer"})
		for i, n := range []string{protocolFixtureRead, protocolFixtureWrite} {
			id := []string{"a", "b"}[i]
			add("response.output_item.added", map[string]any{"type": "response.output_item.added", "output_index": i + 1, "item": map[string]any{"type": "function_call", "id": "fc_" + id, "call_id": id, "name": n, "arguments": ""}})
		}
		for _, i := range []int{1, 0} {
			args := []string{`{"q":"x"}`, `{"q":"y"}`}[i]
			add("response.function_call_arguments.delta", map[string]any{"type": "response.function_call_arguments.delta", "output_index": i + 1, "delta": args})
			add("response.output_item.done", map[string]any{"type": "response.output_item.done", "output_index": i + 1, "item": map[string]any{"type": "function_call"}})
		}
		add("response.completed", map[string]any{"type": "response.completed", "response": protocolFixtureObj(protocolFixtureResponse(f))})
	case d.ClaudeMessages:
		add("message_start", map[string]any{"type": "message_start", "message": map[string]any{"id": "c", "type": "message", "role": "assistant", "content": []any{}, "model": "m", "usage": map[string]any{"input_tokens": 7, "output_tokens": 0}}})
		add("content_block_start", map[string]any{"type": "content_block_start", "index": 0, "content_block": map[string]any{"type": "text", "text": ""}})
		add("content_block_delta", map[string]any{"type": "content_block_delta", "index": 0, "delta": map[string]any{"type": "text_delta", "text": "answer"}})
		add("content_block_stop", map[string]any{"type": "content_block_stop", "index": 0})
		for i, n := range []string{protocolFixtureRead, protocolFixtureWrite} {
			id := []string{"a", "b"}[i]
			add("content_block_start", map[string]any{"type": "content_block_start", "index": i + 1, "content_block": map[string]any{"type": "tool_use", "id": id, "name": n, "input": map[string]any{}}})
			add("content_block_delta", map[string]any{"type": "content_block_delta", "index": i + 1, "delta": map[string]any{"type": "input_json_delta", "partial_json": []string{`{"q":"x"}`, `{"q":"y"}`}[i]}})
			add("content_block_stop", map[string]any{"type": "content_block_stop", "index": i + 1})
		}
		add("message_delta", map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "tool_use"}, "usage": map[string]any{"output_tokens": 5}})
		add("message_stop", map[string]any{"type": "message_stop"})
	case d.Gemini:
		add("", protocolFixtureObj(protocolFixtureResponse(f)))
	case d.GeminiInteractions:
		add("interaction.created", map[string]any{"event_type": "interaction.created", "interaction": map[string]any{"id": "c", "model": "m", "status": "in_progress"}})
		add("step.start", map[string]any{"event_type": "step.start", "index": 0, "step": map[string]any{"type": "model_output", "content": []any{}}})
		add("step.delta", map[string]any{"event_type": "step.delta", "index": 0, "delta": map[string]any{"type": "text", "text": "answer"}})
		add("step.stop", map[string]any{"event_type": "step.stop", "index": 0})
		for i, n := range []string{protocolFixtureRead, protocolFixtureWrite} {
			id := []string{"a", "b"}[i]
			add("step.start", map[string]any{"event_type": "step.start", "index": i + 1, "step": map[string]any{"type": "function_call", "id": id, "name": n, "arguments": map[string]any{}}})
			add("step.delta", map[string]any{"event_type": "step.delta", "index": i + 1, "delta": map[string]any{"type": "arguments_delta", "arguments": []string{`{"q":"x"}`, `{"q":"y"}`}[i]}})
			add("step.stop", map[string]any{"event_type": "step.stop", "index": i + 1})
		}
		add("interaction.completed", map[string]any{"event_type": "interaction.completed", "interaction": protocolFixtureObj(protocolFixtureResponse(f))})
	}
	return out
}

type protocolFixtureCall struct{ name, id, args string }
type protocolFixtureCollected struct {
	text          string
	calls         map[int]*protocolFixtureCall
	terminal      int
	input, output int
	errors        int
	added, done   map[int]bool
}

func protocolFixtureCollect(f d.Format, frames []d.Frame) protocolFixtureCollected {
	r := protocolFixtureCollected{calls: map[int]*protocolFixtureCall{}, added: map[int]bool{}, done: map[int]bool{}}
	call := func(i int) *protocolFixtureCall {
		if r.calls[i] == nil {
			r.calls[i] = &protocolFixtureCall{}
		}
		return r.calls[i]
	}
	for _, frame := range frames {
		if string(frame.Data) == "[DONE]" {
			r.terminal++
			continue
		}
		o := protocolFixtureObj(frame.Data)
		typ := protocolFixtureStr(o["type"])
		switch f {
		case d.OpenAIChat:
			if o["error"] != nil {
				r.errors++
			}
			for _, v := range protocolFixtureArr(o["choices"]) {
				c := protocolFixtureMap(v)
				delta := protocolFixtureMap(c["delta"])
				r.text += protocolFixtureStr(delta["content"])
				for _, v := range protocolFixtureArr(delta["tool_calls"]) {
					c := protocolFixtureMap(v)
					p := call(protocolFixtureInt(c["index"]))
					fun := protocolFixtureMap(c["function"])
					p.name += protocolFixtureStr(fun["name"])
					p.id += protocolFixtureStr(c["id"])
					p.args += protocolFixtureStr(fun["arguments"])
				}
			}
			if u := protocolFixtureMap(o["usage"]); u != nil {
				r.input = protocolFixtureInt(u["prompt_tokens"])
				r.output = protocolFixtureInt(u["completion_tokens"])
			}
		case d.OpenAIResponses:
			switch typ {
			case "response.output_text.delta":
				r.text += protocolFixtureStr(o["delta"])
			case "response.output_item.added":
				i := protocolFixtureInt(o["output_index"])
				r.added[i] = true
				item := protocolFixtureMap(o["item"])
				if item["type"] == "function_call" {
					p := call(i)
					p.name = protocolFixtureStr(item["name"])
					p.id = protocolFixtureStr(item["call_id"])
				}
			case "response.function_call_arguments.delta":
				call(protocolFixtureInt(o["output_index"])).args += protocolFixtureStr(o["delta"])
			case "response.output_item.done":
				r.done[protocolFixtureInt(o["output_index"])] = true
			case "response.completed", "response.incomplete", "response.failed":
				r.terminal++
				if typ == "response.failed" {
					r.errors++
				}
				u := protocolFixtureMap(protocolFixtureMap(o["response"])["usage"])
				r.input = protocolFixtureInt(u["input_tokens"])
				r.output = protocolFixtureInt(u["output_tokens"])
			}
		case d.ClaudeMessages:
			i := protocolFixtureInt(o["index"])
			switch typ {
			case "message_start":
				u := protocolFixtureMap(protocolFixtureMap(o["message"])["usage"])
				r.input = protocolFixtureInt(u["input_tokens"])
			case "content_block_start":
				r.added[i] = true
				b := protocolFixtureMap(o["content_block"])
				if b["type"] == "tool_use" {
					p := call(i)
					p.name = protocolFixtureStr(b["name"])
					p.id = protocolFixtureStr(b["id"])
				}
			case "content_block_delta":
				b := protocolFixtureMap(o["delta"])
				if b["type"] == "text_delta" {
					r.text += protocolFixtureStr(b["text"])
				}
				if b["type"] == "input_json_delta" {
					call(i).args += protocolFixtureStr(b["partial_json"])
				}
			case "content_block_stop":
				r.done[i] = true
			case "message_delta":
				u := protocolFixtureMap(o["usage"])
				r.output = protocolFixtureInt(u["output_tokens"])
				if v, ok := u["input_tokens"]; ok {
					r.input = protocolFixtureInt(v)
				}
			case "message_stop":
				r.terminal++
			case "error":
				r.errors++
			}
		case d.Gemini:
			if o["error"] != nil {
				r.errors++
			}
			for _, v := range protocolFixtureArr(o["candidates"]) {
				c := protocolFixtureMap(v)
				if protocolFixtureStr(c["finishReason"]) != "" {
					r.terminal++
				}
				for _, v := range protocolFixtureArr(protocolFixtureMap(c["content"])["parts"]) {
					p := protocolFixtureMap(v)
					if p["thought"] != true {
						r.text += protocolFixtureStr(p["text"])
					}
					if fc := protocolFixtureMap(p["functionCall"]); fc != nil {
						q := call(len(r.calls))
						q.name = protocolFixtureStr(fc["name"])
						q.args = string(protocolFixtureJSON(fc["args"]))
					}
				}
			}
			if u := protocolFixtureMap(o["usageMetadata"]); u != nil {
				r.input = protocolFixtureInt(u["promptTokenCount"])
				r.output = protocolFixtureInt(u["candidatesTokenCount"])
			}
		case d.GeminiInteractions:
			typ = protocolFixtureStr(o["event_type"])
			i := protocolFixtureInt(o["index"])
			switch typ {
			case "step.start":
				r.added[i] = true
				s := protocolFixtureMap(o["step"])
				if s["type"] == "function_call" {
					p := call(i)
					p.name = protocolFixtureStr(s["name"])
					p.id = protocolFixtureStr(s["id"])
				}
			case "step.delta":
				s := protocolFixtureMap(o["delta"])
				if s["type"] == "text" {
					r.text += protocolFixtureStr(s["text"])
				}
				if s["type"] == "arguments_delta" {
					call(i).args += protocolFixtureStr(s["arguments"])
				}
			case "step.stop":
				r.done[i] = true
			case "interaction.completed":
				r.terminal++
				u := protocolFixtureMap(protocolFixtureMap(o["interaction"])["usage"])
				r.input = protocolFixtureInt(u["total_input_tokens"])
				r.output = protocolFixtureInt(u["total_output_tokens"])
			case "error":
				r.errors++
			}
		}
	}
	return r
}
func protocolFixtureCheckStream(t *testing.T, f d.Format, frames []d.Frame) {
	t.Helper()
	r := protocolFixtureCollect(f, frames)
	calls := map[string]string{}
	for _, p := range r.calls {
		calls[p.name] = protocolFixtureStr(protocolFixtureObj([]byte(p.args))["q"])
	}
	if r.text != "answer" || len(calls) != 2 || calls[protocolFixtureRead] != "x" || calls[protocolFixtureWrite] != "y" {
		t.Errorf("stream semantic mismatch: text=%q calls=%v rawCalls=%v", r.text, calls, protocolFixtureSortedCalls(r))
	}
	if r.terminal != 1 || r.errors != 0 {
		t.Errorf("terminal=%d errors=%d", r.terminal, r.errors)
	}
	if r.input != 7 || r.output != 5 {
		t.Errorf("usage input=%d output=%d", r.input, r.output)
	}
	for i := range r.added {
		if !r.done[i] {
			t.Errorf("announced item/block %d lacks done event", i)
		}
	}
}

func TestServer_ProtocolCoreMatrix(t *testing.T) {
	for _, source := range protocolFixtureFormats {
		for _, target := range protocolFixtureFormats {
			name := string(source) + "__" + string(target)
			t.Run("request/"+name, func(t *testing.T) {
				out, _, e := d.ConvertRequest(source, target, protocolFixtureRequest(source, false), d.ConvertOptions{Model: "m"})
				if e != nil {
					t.Fatal(e)
				}
				protocolFixtureCheckRequest(t, target, out)
			})
			t.Run("response/"+name, func(t *testing.T) {
				out, _, e := d.ConvertResponse(source, target, protocolFixtureResponse(source))
				if e != nil {
					t.Fatal(e)
				}
				protocolFixtureCheckResponse(t, target, out)
			})
			if source != target {
				t.Run("stream/"+name, func(t *testing.T) {
					c, e := d.NewStreamConverter(source, target, d.StreamOptions{Model: "m", IncludeUsage: true})
					if e != nil {
						t.Fatal(e)
					}
					var all []d.Frame
					for _, f := range protocolFixtureStream(source) {
						fs, e := c.Feed(f.Data)
						if e != nil {
							t.Fatal(e)
						}
						all = append(all, fs...)
					}
					fs, e := c.Finish()
					if e != nil {
						t.Fatal(e)
					}
					all = append(all, fs...)
					protocolFixtureCheckStream(t, target, all)
				})
			}
		}
	}
}

func protocolFixtureSurface(f d.Format) string {
	switch f {
	case d.OpenAIChat:
		return config.ProviderTypeOpenAI
	case d.OpenAIResponses:
		return config.ProviderTypeOpenAIResponses
	case d.ClaudeMessages:
		return config.ProviderTypeAnthropic
	case d.Gemini:
		return config.ProviderTypeGemini
	case d.GeminiInteractions:
		return config.ProviderTypeGeminiInteractions
	}
	return ""
}
func protocolFixturePath(f d.Format, stream bool) string {
	p, e := d.TargetPath(f, "m", stream)
	if e != nil {
		panic(e)
	}
	if f == d.Gemini && stream {
		p += "?alt=sse"
	}
	return p
}
func protocolFixtureEncodeSSE(frames []d.Frame) []byte {
	var b bytes.Buffer
	for _, f := range frames {
		if f.Event != "" {
			fmt.Fprintf(&b, "event: %s\n", f.Event)
		}
		fmt.Fprintf(&b, "data: %s\n\n", f.Data)
	}
	if len(frames) > 0 && protocolFixtureObj(frames[0].Data)["choices"] != nil {
		b.WriteString("data: [DONE]\n\n")
	}
	return b.Bytes()
}
func protocolFixtureParseSSE(b []byte) []d.Frame {
	var fs []d.Frame
	for _, event := range bytes.Split(bytes.ReplaceAll(b, []byte("\r\n"), []byte("\n")), []byte("\n\n")) {
		var f d.Frame
		for _, line := range bytes.Split(event, []byte("\n")) {
			if bytes.HasPrefix(line, []byte("event:")) {
				f.Event = strings.TrimSpace(string(line[6:]))
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				f.Data = append(f.Data, bytes.TrimPrefix(line[5:], []byte(" "))...)
			}
		}
		if len(f.Data) > 0 {
			fs = append(fs, f)
		}
	}
	return fs
}
func TestServer_ProtocolHTTPMatrix(t *testing.T) {
	for _, client := range protocolFixtureFormats {
		for _, upstream := range protocolFixtureFormats {
			for _, stream := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s__%s/stream=%t", client, upstream, stream), func(t *testing.T) {
					hits := 0
					backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						hits++
						b, _ := io.ReadAll(r.Body)
						protocolFixtureCheckRequest(t, upstream, b)
						route, ok := d.RouteForRequest(r.Method, r.URL.Path)
						if !ok || route.Format != upstream {
							t.Errorf("upstream path=%s want=%s", r.URL.Path, upstream)
						}
						if stream {
							w.Header().Set("Content-Type", "text/event-stream")
							w.WriteHeader(200)
							w.(http.Flusher).Flush()
							w.Write(protocolFixtureEncodeSSE(protocolFixtureStream(upstream)))
						} else {
							w.Header().Set("Content-Type", "application/json")
							w.Write(protocolFixtureResponse(upstream))
						}
					}))
					defer backend.Close()
					cfg := config.Config{Models: map[string]config.ModelConfig{}, Providers: config.ProviderDictionaryConfig{"protocolFixture": {ApiKeys: []config.ProviderKey{{Value: "test"}}, Adapters: []config.ProviderAdapter{{Name: "protocolFixture", Type: protocolFixtureSurface(upstream), Endpoint: backend.URL, Models: []config.ProviderModel{{Local: "m", Remote: "m"}}}}}}}
					s := multiSurfaceServer(t, cfg)
					w := postJSON(t, s, protocolFixturePath(client, stream), string(protocolFixtureRequest(client, stream)))
					if hits != 1 || w.Code != 200 {
						t.Fatalf("hits=%d status=%d body=%s", hits, w.Code, w.Body.String())
					}
					if stream {
						protocolFixtureCheckStream(t, client, protocolFixtureParseSSE(w.Body.Bytes()))
					} else {
						protocolFixtureCheckResponse(t, client, w.Body.Bytes())
					}
				})
			}
		}
	}
}

func protocolFixtureSortedCalls(r protocolFixtureCollected) []string {
	var out []string
	for _, p := range r.calls {
		out = append(out, p.name+":"+p.args)
	}
	sort.Strings(out)
	return out
}
