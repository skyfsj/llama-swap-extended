package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/extensions"
)

// TestServer_ExtensionStreamWriterFramesCommittedFailure guards the failure a
// handler reports after the response started: the body it writes is a protocol
// error document, not an SSE frame. Appending it verbatim left the client
// parsing a malformed event and treating the turn as a clean end, so a client
// such as ZCode closed the round's pending tool call with
// fault.runtime.toolLifecycleIncomplete instead of showing the error.
func TestServer_ExtensionStreamWriterFramesCommittedFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	state := &extensionRequestState{meta: extensions.Context{Endpoint: "responses"}}
	w := newExtensionStreamWriter(recorder, request, state)

	w.WriteHeader(http.StatusOK)
	if err := w.emit("response.created", map[string]any{"type": "response.created"}); err != nil {
		t.Fatal(err)
	}
	w.WriteHeader(http.StatusBadRequest)
	if _, err := w.Write([]byte(`{"error":{"code":"bad_request","message":"continuation failed","type":"invalid_request_error"}}`)); err != nil {
		t.Fatal(err)
	}
	if err := w.Finish(); err != nil {
		t.Fatal(err)
	}

	body := recorder.Body.String()
	if !strings.Contains(body, "event: error") {
		t.Fatalf("the failure was not framed as an SSE error event: %s", body)
	}
	if !strings.Contains(body, "continuation failed") {
		t.Fatalf("the failure message was lost: %s", body)
	}
	if strings.Contains(body, `"code":"bad_request"`) {
		t.Fatalf("the raw error document leaked into the stream: %s", body)
	}
}
