package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

// The fold has to leave the caller's own turn untouched and place the artifact's
// result next to the caller's results - exactly once. A duplicated result reached
// the upstream in the Claude Code end-to-end run, so the shapes that run used are
// pinned here.
func TestServer_ExtensionsDowngradeClaudeServerArtifacts(t *testing.T) {
	raw := `{"model":"m","messages":[
		{"role":"user","content":[{"type":"text","text":"say hi"}]},
		{"role":"assistant","content":[
			{"type":"text","text":"checking"},
			{"type":"server_tool_use","id":"srvtoolu_s1","name":"lookup","input":{"q":"x"}},
			{"type":"mcp_tool_result","tool_use_id":"srvtoolu_s1","is_error":false,"content":[{"type":"text","text":"found:x"}]},
			{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"/etc/hosts"}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"hosts"}]},
		{"role":"system","content":[{"type":"text","text":"<total_tokens>7</total_tokens>"}]}
	]}`
	var request map[string]any
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	if !downgradeClaudeServerArtifacts(request) {
		t.Fatal("no artifact was folded")
	}
	messages, _ := request["messages"].([]any)
	if len(messages) != 4 {
		t.Fatalf("message count changed: %d", len(messages))
	}
	assistant, _ := messages[1].(map[string]any)
	blocks, _ := assistant["content"].([]any)
	var kinds []string
	results := 0
	for _, rawBlock := range blocks {
		block, _ := rawBlock.(map[string]any)
		kind, _ := block["type"].(string)
		kinds = append(kinds, kind)
		if kind == "tool_use" && block["id"] != "srvtoolu_s1" && block["id"] != "c1" {
			t.Fatalf("unexpected call id %v", block["id"])
		}
		if kind == "mcp_tool_result" {
			results++
		}
	}
	if results != 0 {
		t.Fatalf("the artifact result stayed in the assistant turn: %v", kinds)
	}
	if len(blocks) != 3 || kinds[0] != "text" || kinds[1] != "tool_use" || kinds[2] != "tool_use" {
		t.Fatalf("assistant turn = %v", kinds)
	}
	user, _ := messages[2].(map[string]any)
	userBlocks, _ := user["content"].([]any)
	if len(userBlocks) != 2 {
		t.Fatalf("caller results = %v", userBlocks)
	}
	seen := map[string]int{}
	for _, rawBlock := range userBlocks {
		block, _ := rawBlock.(map[string]any)
		seen[block["type"].(string)]++
	}
	if seen["tool_result"] != 2 {
		t.Fatalf("the artifact result was not paired exactly once: %v", userBlocks)
	}
	for _, rawBlock := range userBlocks {
		block, _ := rawBlock.(map[string]any)
		if block["tool_use_id"] == "srvtoolu_s1" {
			if !containsText(block["content"], "found:x") {
				t.Fatalf("artifact result content = %v", block["content"])
			}
			return
		}
	}
	t.Fatalf("the folded result is missing: %v", userBlocks)
}

func containsText(content any, needle string) bool {
	encoded, _ := json.Marshal(content)
	return json.Valid(encoded) && len(encoded) > 0 && jsonContains(encoded, needle)
}

func jsonContains(haystack []byte, needle string) bool {
	var value any
	if json.Unmarshal(haystack, &value) != nil {
		return false
	}
	text, _ := value.(string)
	if text == needle {
		return true
	}
	switch typed := value.(type) {
	case string:
		return typed == needle
	case []any:
		for _, item := range typed {
			raw, _ := json.Marshal(item)
			if jsonContains(raw, needle) {
				return true
			}
		}
	case map[string]any:
		for _, item := range typed {
			raw, _ := json.Marshal(item)
			if jsonContains(raw, needle) {
				return true
			}
		}
	}
	return false
}

// The exact shape Claude Code replays (verified against its raw request body in
// the end-to-end run) must fold into one call and one result - a duplicated
// result reached the upstream there.
func TestServer_ExtensionsAnthropicReplayedArtifactsFoldOnce(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "json"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			local := newStubRouter([]string{"m"}, "")
			var upstream map[string]any
			local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &upstream)
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"b","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
			}
			s := extensionTestServer(t, local)
			streaming := ""
			if stream {
				streaming = `"stream":true,`
			}
			body := `{"model":"m",` + streaming + `"max_tokens":100,"messages":[
				{"role":"user","content":"say hi"},
				{"role":"assistant","content":[{"type":"text","text":"checking"},{"type":"server_tool_use","id":"srvtoolu_s1","name":"lookup","input":{"q":"x"}},{"type":"mcp_tool_result","tool_use_id":"srvtoolu_s1","content":[{"type":"text","text":"found:x"}]},{"type":"tool_use","id":"c1","name":"Read","input":{"file_path":"/etc/hosts"}}]},
				{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"hosts"}]}]}`
			r := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d %.200s", w.Code, w.Body.String())
			}
			encoded, _ := json.Marshal(upstream["messages"])
			folded := strings.Count(string(encoded), `"tool_call_id":"srvtoolu_s1"`)
			if folded != 1 {
				t.Fatalf("the folded result appears %d times: %s", folded, encoded)
			}
			if !strings.Contains(string(encoded), "found:x") || !strings.Contains(string(encoded), "hosts") {
				t.Fatalf("results missing: %s", encoded)
			}
		})
	}
}

// A replayed mcp_call item folds into the ordinary call/result pair, in place,
// without touching the caller's own items.
func TestServer_ExtensionsResponsesArtifactsFold(t *testing.T) {
	raw := `{"model":"m","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"mcp_call","id":"mcp_s1","server_label":"llama-swap","name":"lookup","arguments":"{\"q\":\"x\"}","output":"found:x","status":"completed"},
		{"type":"function_call","call_id":"c1","name":"client_tool","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"client-result"}]}`
	var request map[string]any
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	if !responsesFoldServerArtifacts(request) {
		t.Fatal("no artifact was folded")
	}
	items, _ := request["input"].([]any)
	if len(items) != 5 {
		t.Fatalf("input items = %v", items)
	}
	folded, _ := items[1].(map[string]any)
	if folded["type"] != "function_call" || folded["call_id"] != "mcp_s1" || folded["name"] != "lookup" || folded["arguments"] != `{"q":"x"}` {
		t.Fatalf("folded call = %v", folded)
	}
	output, _ := items[2].(map[string]any)
	if output["type"] != "function_call_output" || output["call_id"] != "mcp_s1" || output["output"] != "found:x" {
		t.Fatalf("folded output = %v", output)
	}
	callerCall, _ := items[3].(map[string]any)
	callerOutput, _ := items[4].(map[string]any)
	if callerCall["call_id"] != "c1" || callerOutput["call_id"] != "c1" {
		t.Fatalf("the caller's own items moved: %v", items[3:])
	}
}

// A client that continues with previous_response_id gets the stored turn back,
// mcp_call artifacts included, as a coherent call/result chain.
func TestServer_ExtensionsResponsesAffinityReplaysArtifacts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "control.db")
	st, err := store.New(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	now := time.Now()
	if err := st.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: "resp-artifact", Model: "m", Backend: "vllm", Status: "completed",
		Response: []byte(`{"id":"resp-artifact","model":"m","status":"completed","output":[
			{"type":"mcp_call","id":"mcp_s1","server_label":"llama-swap","name":"lookup","arguments":"{\"q\":\"x\"}","output":"found:x","status":"completed"},
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking"}]}]}`),
		ExpiresAt: now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	chatBody, err := addPreviousResponseChainForModel(
		context.Background(),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"continue"}]}`),
		[]byte(`{"model":"m","input":[{"type":"function_call_output","call_id":"mcp_s1","output":"found:x"}],"previous_response_id":"resp-artifact"}`),
		"m", st,
	)
	if err != nil {
		t.Fatalf("previous_response_id with an artifact: %v", err)
	}
	var chat map[string]any
	if err := json.Unmarshal(chatBody, &chat); err != nil {
		t.Fatal(err)
	}
	messages, _ := chat["messages"].([]any)
	sawCall, sawResult := false, false
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		for _, rawCall := range asSlice(message["tool_calls"]) {
			call, _ := rawCall.(map[string]any)
			if call["id"] == "mcp_s1" {
				sawCall = true
			}
		}
		if message["role"] == "tool" && message["tool_call_id"] == "mcp_s1" {
			sawResult = true
		}
	}
	if !sawCall || !sawResult {
		t.Fatalf("stored artifact did not replay as a call/result pair: %s", chatBody)
	}
}

// The streaming Responses surface reports the same artifacts: mcp_call item
// events, plus the item inside the final response.completed frame so a client
// that only reads that frame still sees the server work.
func TestServer_ExtensionsResponsesStreamMixedResume(t *testing.T) {
	local := newStubRouter([]string{"m"}, "")
	rounds := 0
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		rounds++
		if rounds == 1 {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"s1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{\\\"q\\\":\\\"x\\\"}\"}},{\"index\":1,\"id\":\"c1\",\"type\":\"function\",\"function\":{\"name\":\"client_tool\",\"arguments\":\"{}\"}}]}}]}\n\n")
			_, _ = io.WriteString(w, "data: {\"id\":\"a\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"b","object":"chat.completion","created":1,"model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`)
	}
	s := extensionTestServer(t, local)
	model := s.cfg.Models["m"]
	model.Backend = config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolNative}
	s.cfg.Models["m"] = model
	s.routes()
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"m","input":"hi","stream":true,"tools":[{"type":"function","name":"client_tool","parameters":{"type":"object"}}]}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	content := w.Body.String()
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d %.300s", w.Code, content)
	}
	if strings.Contains(content, "call_ls_") {
		t.Fatalf("a visible surface must not rewrite caller ids: %s", content)
	}
	if !strings.Contains(content, `"type":"mcp_call"`) || !strings.Contains(content, "found:x") {
		t.Fatalf("the server call is not visible in the stream: %s", content)
	}
	if !strings.Contains(content, `"name":"lookup"`) || !strings.Contains(content, `"name":"client_tool"`) {
		t.Fatalf("the round's calls are missing from the stream: %s", content)
	}
	// The completed frame lists the artifact item as well.
	completed := ""
	for _, line := range strings.Split(content, "\n") {
		if strings.Contains(line, "response.completed") && strings.HasPrefix(line, "data: ") {
			completed = line
		}
	}
	if completed == "" || !strings.Contains(completed, `"mcp_call"`) {
		t.Fatalf("response.completed does not carry the artifact: %s", completed)
	}
}

// The Gemini fold rebuilds the replayed model turn and puts the deferred call's
// own functionResponse next to the caller's, all paired by name.
func TestServer_ExtensionsGeminiDeferredFold(t *testing.T) {
	payload := extensionPendingPayload{
		Calls: []extensionPendingCall{
			{ExtensionID: "gcal", ID: "call_gemini_1", Name: "lookup", Arguments: `{"q":"x"}`, Kind: pendingCallServer},
			{ID: "client_tool", Name: "client_tool", Kind: pendingCallClient},
		},
		ClientIDs: map[string]string{"client_tool": "call_gemini_2"},
	}
	var request map[string]any
	raw := `{"contents":[
		{"role":"user","parts":[{"text":"hi"}]},
		{"role":"model","parts":[{"functionCall":{"name":"client_tool","args":{}}}]},
		{"role":"user","parts":[{"functionResponse":{"name":"client_tool","response":{"content":"client-result"}}}]}]}`
	if err := json.Unmarshal([]byte(raw), &request); err != nil {
		t.Fatal(err)
	}
	turn, err := geminiDeferredTurnIn(request, payload)
	if err != nil {
		t.Fatalf("deferred turn: %v", err)
	}
	outputs := map[string]map[string]any{"call_gemini_1": {"content": "found:x"}}
	if err := foldGeminiDeferredTurn(request, payload, turn, outputs); err != nil {
		t.Fatalf("fold: %v", err)
	}
	encoded, _ := json.Marshal(request["contents"])
	body := string(encoded)
	// Gemini pairs by name, so the parts carry names (not the gateway's ids).
	for _, want := range []string{"lookup", "client_tool", "found:x", "client-result"} {
		if !strings.Contains(body, want) {
			t.Fatalf("folded conversation is missing %q: %s", want, body)
		}
	}
	var contents []any
	_ = json.Unmarshal(encoded, &contents)
	if len(contents) != 3 {
		t.Fatalf("contents = %v", contents)
	}
	model, _ := contents[1].(map[string]any)
	parts, _ := model["parts"].([]any)
	if len(parts) != 2 {
		t.Fatalf("the model turn must carry both calls: %v", parts)
	}
	user, _ := contents[2].(map[string]any)
	userParts, _ := user["parts"].([]any)
	if len(userParts) != 2 {
		t.Fatalf("the result turn must carry both results: %v", userParts)
	}
	// Gemini wraps a result in a functionResponse part; a bare part is dropped by
	// the dialect layer as unknown.
	wrapped := false
	for _, rawPart := range userParts {
		part, _ := rawPart.(map[string]any)
		response, _ := part["functionResponse"].(map[string]any)
		if response == nil {
			continue
		}
		if response["name"] == "lookup" {
			text, _ := json.Marshal(response["response"])
			if !strings.Contains(string(text), "found:x") {
				t.Fatalf("lookup result = %s", text)
			}
			wrapped = true
		}
	}
	if !wrapped {
		t.Fatalf("the deferred result is not a functionResponse part: %v", userParts)
	}
}
