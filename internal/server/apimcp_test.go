package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/mostlygeek/llama-swap/internal/config"
)

const mcpDocsFixture = `# Configuration

Every top-level key.

## Capability discovery

Discovery asks the upstream what it supports when the model becomes ready.
Set disableAuto to keep it away from a model.

## Upstream keep-alive

disableKeepAlives opens a new connection per request.
`

// configWithTwoModels is a tiny configuration with two plain models, used to
// exercise the config tools without a database or a running process.
func configWithTwoModels() config.Config {
	return config.Config{Models: map[string]config.ModelConfig{
		"alpha": {Cmd: "llama-server --port ${PORT}"},
		"beta":  {Cmd: "llama-server --port ${PORT}"},
	}}
}

// mcpTestServer returns a server with documentation indexed.
func mcpTestServer(t *testing.T) *Server {
	t.Helper()
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.SetDocumentation(fstest.MapFS{
		"configuration.md": {Data: []byte(mcpDocsFixture)},
	})
	return s
}

// mcpCall posts one JSON-RPC message and returns the decoded response.
func mcpCall(t *testing.T, s *Server, body string, headers map[string]string) mcpResponse {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %q: %v", w.Body.String(), err)
	}
	return resp
}

func TestMCP_InitializeHandshake(t *testing.T) {
	s := mcpTestServer(t)
	resp := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`, nil)
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	if result["protocolVersion"] != "2025-06-18" {
		t.Errorf("protocolVersion = %v, want the client's revision echoed", result["protocolVersion"])
	}
	if result["serverInfo"].(map[string]any)["name"] != "llama-swap" {
		t.Errorf("serverInfo = %v", result["serverInfo"])
	}
}

func TestMCP_InitializeOffersLegacyWhenClientIsUnknown(t *testing.T) {
	s := mcpTestServer(t)
	resp := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`, nil)
	if resp.Error != nil {
		t.Fatalf("initialize errored: %+v", resp.Error)
	}
	if got := resp.Result.(map[string]any)["protocolVersion"]; got != mcpProtocolVersionLegacy {
		t.Errorf("protocolVersion = %v, want %s", got, mcpProtocolVersionLegacy)
	}
}

func TestMCP_DiscoverListsBothRevisions(t *testing.T) {
	s := mcpTestServer(t)
	resp := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"server/discover"}`, map[string]string{
		headerMCPProtocolVersion: mcpProtocolVersionModern,
		headerMCPMethod:          "server/discover",
	})
	if resp.Error != nil {
		t.Fatalf("discover errored: %+v", resp.Error)
	}
	versions := resp.Result.(map[string]any)["supportedVersions"].([]any)
	if len(versions) != 2 {
		t.Fatalf("supportedVersions = %v", versions)
	}
}

func TestMCP_RejectsAnUnknownProtocolRevision(t *testing.T) {
	s := mcpTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	req.Header.Set(headerMCPProtocolVersion, "1999-01-01")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)

	var resp mcpResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error == nil || resp.Error.Code != codeMCPUnsupportedProtocolCode {
		t.Fatalf("error = %+v, want the unsupported-version code", resp.Error)
	}
	if !strings.Contains(string(resp.Error.Data), mcpProtocolVersionModern) {
		t.Errorf("error data %q does not name the supported revision", resp.Error.Data)
	}
}

func TestMCP_RejectsAMismatchedMethodHeader(t *testing.T) {
	s := mcpTestServer(t)
	resp := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, map[string]string{
		headerMCPMethod: "tools/call",
	})
	if resp.Error == nil || resp.Error.Code != codeMCPInvalidRequest {
		t.Fatalf("error = %+v, want invalid request", resp.Error)
	}
}

func TestMCP_ToolsListAdvertisesDocsAndConfigTools(t *testing.T) {
	s := mcpTestServer(t)
	resp := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if resp.Error != nil {
		t.Fatalf("tools/list errored: %+v", resp.Error)
	}
	result := resp.Result.(map[string]any)
	names := map[string]bool{}
	for _, tool := range result["tools"].([]any) {
		names[tool.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"docs__list", "docs__search", "docs__get", "config__get", "sys__overview"} {
		if !names[want] {
			t.Errorf("tool %q is missing from %v", want, names)
		}
	}
	if result["ttlMs"] == nil {
		t.Error("the listing must carry ttlMs: a stateless endpoint cannot push list changes")
	}
}

// mcpToolText runs one tool and returns its text content.
func mcpToolText(t *testing.T, s *Server, name, args string) (string, bool) {
	t.Helper()
	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"` + name + `","arguments":` + args + `}}`
	resp := mcpCall(t, s, body, nil)
	if resp.Error != nil {
		t.Fatalf("%s errored: %+v", name, resp.Error)
	}
	result := resp.Result.(map[string]any)
	content := result["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("%s returned no content", name)
	}
	isError, _ := result["isError"].(bool)
	return content[0].(map[string]any)["text"].(string), isError
}

func TestMCP_DocsSearchThenGet(t *testing.T) {
	s := mcpTestServer(t)

	text, isErr := mcpToolText(t, s, "docs__search", `{"query":"capability discovery"}`)
	if isErr {
		t.Fatalf("search failed: %s", text)
	}
	if !strings.Contains(text, "guides/configuration#capability-discovery") {
		t.Fatalf("search text = %q", text)
	}

	text, isErr = mcpToolText(t, s, "docs__get", `{"id":"guides/configuration#capability-discovery"}`)
	if isErr {
		t.Fatalf("get failed: %s", text)
	}
	if !strings.Contains(text, "disableAuto") {
		t.Errorf("get text = %q", text)
	}
	if !strings.Contains(text, "source: guides/configuration.md") {
		t.Errorf("get text does not cite its source: %q", text)
	}
}

func TestMCP_DocsGetRejectsAnUnknownID(t *testing.T) {
	s := mcpTestServer(t)
	text, isErr := mcpToolText(t, s, "docs__get", `{"id":"guides/nope"}`)
	if !isErr {
		t.Fatalf("an unknown id must be reported as an error, got %q", text)
	}
	if !strings.Contains(text, "docs__list") {
		t.Errorf("the error should say how to recover: %q", text)
	}
}

func TestMCP_ConfigGetSelectsWithJQ(t *testing.T) {
	s := newTestServerWithConfig(
		configWithTwoModels(),
		newStubRouter(nil, ""),
		newStubRouter(nil, ""),
	)
	s.SetDocumentation(fstest.MapFS{"configuration.md": {Data: []byte(mcpDocsFixture)}})

	text, isErr := mcpToolText(t, s, "config__get", `{"query":".models | keys"}`)
	if isErr {
		t.Fatalf("config__get failed: %s", text)
	}
	if !strings.Contains(text, "alpha") || !strings.Contains(text, "beta") {
		t.Errorf("query result = %q, want both model ids", text)
	}
	if strings.Contains(text, "secret") {
		t.Errorf("query result leaked credentials: %q", text)
	}
}

func TestMCP_ConfigGetRejectsABadQuery(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.SetDocumentation(fstest.MapFS{"configuration.md": {Data: []byte(mcpDocsFixture)}})

	text, isErr := mcpToolText(t, s, "config__get", `{"query":".models | keys["}`)
	if !isErr {
		t.Fatalf("an invalid jq expression must be reported as an error, got %q", text)
	}
}

func TestMCP_UnknownToolIsAnError(t *testing.T) {
	s := mcpTestServer(t)
	text, isErr := mcpToolText(t, s, "docs__nope", `{}`)
	if !isErr || !strings.Contains(text, "unknown tool") {
		t.Fatalf("text = %q isError = %v", text, isErr)
	}
}

func TestMCP_NotificationsAreAcceptedWithoutABody(t *testing.T) {
	s := mcpTestServer(t)
	req := httptest.NewRequest(http.MethodPost, "/api/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","method":"notifications/initialized"}`))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", w.Code)
	}
	if w.Body.Len() != 0 {
		t.Errorf("a notification must not get a body, got %q", w.Body.String())
	}
}

// A JSON null id is a legal request id, so it must be answered like any other
// request; only a missing id makes a message a notification.
func TestMCP_NullIDIsARequestNotANotification(t *testing.T) {
	s := mcpTestServer(t)
	resp := mcpCall(t, s, `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, nil)
	if resp.Error != nil {
		t.Fatalf("a null id was treated as a notification: %+v", resp.Error)
	}
	if resp.Result == nil {
		t.Fatal("a null id must still get a result")
	}
	if string(resp.ID) != "null" {
		t.Errorf("id = %s, want it echoed as null", resp.ID)
	}
}

func TestMCP_BatchingIsRejected(t *testing.T) {
	s := mcpTestServer(t)
	resp := mcpCall(t, s, `[{"jsonrpc":"2.0","id":1,"method":"tools/list"}]`, nil)
	if resp.Error == nil || !strings.Contains(resp.Error.Message, "batching") {
		t.Fatalf("error = %+v, want a batching refusal", resp.Error)
	}
}

func TestMCP_GETIsMethodNotAllowed(t *testing.T) {
	s := mcpTestServer(t)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/mcp", nil))
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405", w.Code)
	}
	if got := w.Header().Get("Allow"); got != "POST" {
		t.Errorf("Allow = %q, want POST", got)
	}
}

func TestMCP_WithoutDocumentationAnswersUnavailable(t *testing.T) {
	// A Server that never had documentation installed must answer a clear
	// error rather than panicking on a nil provider.
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	resp := mcpCall(t, s, `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`, nil)
	if resp.Error == nil {
		t.Fatal("expected an error from a server without documentation")
	}
	if !strings.Contains(resp.Error.Message, "not available") {
		t.Errorf("message = %q", resp.Error.Message)
	}
}

func TestMCP_RequiresInferenceScope(t *testing.T) {
	// The endpoint reads the redacted configuration, so it is not anonymous:
	// it sits behind the same scope as the inference API.
	cfg := configWithTwoModels()
	cfg.RequiredAPIKeys = []string{"secret"}
	s := newTestServerWithConfig(cfg, newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.SetDocumentation(fstest.MapFS{"configuration.md": {Data: []byte(mcpDocsFixture)}})

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/mcp",
		strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)))
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401 without a key", w.Code)
	}
}
