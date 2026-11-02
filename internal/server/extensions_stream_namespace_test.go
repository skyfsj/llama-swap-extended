package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// TestServer_ExtensionStreamWriterKeepsToolNamespace guards the namespace of a
// namespaced Responses tool call. Codex dispatches by (namespace, name), so a
// call rebuilt as a bare name has no handler at all: the client answered
// "unsupported call: js" for the built-in browser tools (cua.js,
// codex-app.open_in_codex) whenever extensions were enabled, because the stream
// bridge accumulated the item and re-emitted only name and arguments. The
// non-streaming path never rebuilt the item, which is why the same call worked
// there and every streamed one failed.
func TestServer_ExtensionStreamWriterKeepsToolNamespace(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	state := &extensionRequestState{meta: extensions.Context{Endpoint: "responses"}}
	w := newExtensionStreamWriter(recorder, request, state)

	w.WriteHeader(http.StatusOK)
	events := []string{
		`{"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","id":"fc_0","call_id":"call_1","name":"js","namespace":"mcp__cua_repl","arguments":"","status":"in_progress"}}`,
		`{"type":"response.function_call_arguments.done","output_index":0,"item_id":"fc_0","call_id":"call_1","name":"js","arguments":"{\"code\":\"1+1\"}"}`,
	}
	for _, event := range events {
		if _, err := w.Write([]byte("event: response.event\ndata: " + event + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}

	calls := w.Calls()
	if len(calls) != 1 {
		t.Fatalf("accumulated %d calls, want 1", len(calls))
	}
	if calls[0].Namespace != "mcp__cua_repl" {
		t.Fatalf("the accumulated call lost its namespace: %+v", calls[0])
	}
	if err := w.FinishClientCalls(calls); err != nil {
		t.Fatal(err)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, `"name":"js","namespace":"mcp__cua_repl"`) {
		t.Fatalf("the re-emitted call must carry its namespace:\n%s", body)
	}
}
