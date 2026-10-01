package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logarchive"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_RecordRequestErrorStoresBodyFreeSnapshot(t *testing.T) {
	s := &Server{
		cfg:      config.Config{LogStorage: config.LogStorageConfig{Path: t.TempDir(), MaxFiles: 2}},
		proxylog: logmon.NewWriter(io.Discard),
	}
	s.recordRequestError(RequestError{
		ClientIP:  "127.0.0.1",
		Method:    http.MethodPost,
		Path:      "/v1/chat/completions",
		Proto:     "HTTP/1.1",
		UserAgent: "test-agent",
		Status:    http.StatusBadGateway,
		BodyBytes: 12,
		Duration:  25 * time.Millisecond,
	})

	entries, err := s.incidentArchive.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != logarchive.KindRequestError {
		t.Fatalf("entries=%+v, want one request-error entry", entries)
	}
	data, _, err := s.incidentArchive.Read(entries[0].Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body := string(data)
	for _, want := range []string{"request-error", "POST /v1/chat/completions HTTP/1.1", "status: 502", "test-agent"} {
		if !strings.Contains(body, want) {
			t.Errorf("snapshot %q missing %q", body, want)
		}
	}
	if strings.Contains(body, "request body") {
		t.Error("request snapshot contains a request body")
	}
}

// TestServer_RecordRequestErrorIncludesModelAndResponseBody covers the two
// diagnostics that make an archived failure actionable: which model the
// request resolved to, and the error the client was actually shown.
func TestServer_RecordRequestErrorIncludesModelAndResponseBody(t *testing.T) {
	s := &Server{
		cfg:      config.Config{LogStorage: config.LogStorageConfig{Path: t.TempDir(), MaxFiles: 5}},
		proxylog: logmon.NewWriter(io.Discard),
	}
	s.recordRequestError(RequestError{
		Method: http.MethodPost,
		Path:   "/v1/chat/completions",
		Status: http.StatusServiceUnavailable,
		Model:  "Qwen/Qwen3.8-27B-FP8",
		Detail: `{"error":{"message":"model Qwen/Qwen3.8-27B-FP8 is in maintenance mode"}}`,
	})

	entries, err := s.incidentArchive.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("entries=%+v, want one", entries)
	}
	data, _, err := s.incidentArchive.Read(entries[0].Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "model: Qwen/Qwen3.8-27B-FP8") {
		t.Errorf("snapshot %q is missing the resolved model", body)
	}
	if !strings.Contains(body, "maintenance mode") {
		t.Errorf("snapshot %q is missing the response body", body)
	}
}

// TestServer_RecordRequestErrorOmitsEmptyDiagnostics pins that a request
// which never resolved a model does not gain an empty placeholder line.
func TestServer_RecordRequestErrorOmitsEmptyDiagnostics(t *testing.T) {
	s := &Server{
		cfg:      config.Config{LogStorage: config.LogStorageConfig{Path: t.TempDir(), MaxFiles: 5}},
		proxylog: logmon.NewWriter(io.Discard),
	}
	s.recordRequestError(RequestError{
		Method: http.MethodGet,
		Path:   "/v1/models",
		Status: http.StatusBadRequest,
	})

	entries, err := s.incidentArchive.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	data, _, err := s.incidentArchive.Read(entries[0].Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	body := string(data)
	if strings.Contains(body, "model:") {
		t.Errorf("snapshot %q should not carry a model line when none resolved", body)
	}
	if strings.Contains(body, "response:") {
		t.Errorf("snapshot %q should not carry a response section when empty", body)
	}
}

func TestServer_RecordRequestErrorOnlyArchivesLLMAPI(t *testing.T) {
	s := &Server{
		cfg:      config.Config{LogStorage: config.LogStorageConfig{Path: t.TempDir(), MaxFiles: 5}},
		proxylog: logmon.NewWriter(io.Discard),
	}
	for _, request := range []RequestError{
		{Method: http.MethodGet, Path: "/ui/assets/ui-assets-badge.js", Status: http.StatusNotFound},
		{Method: http.MethodGet, Path: "/api/settings", Status: http.StatusInternalServerError},
		{Method: http.MethodPost, Path: "/v1/chat/completions", Status: http.StatusBadGateway},
	} {
		s.recordRequestError(request)
	}

	archive, err := s.incidentLogStore()
	if err != nil {
		t.Fatalf("incidentLogStore: %v", err)
	}
	entries, err := archive.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 || entries[0].Kind != logarchive.KindRequestError {
		t.Fatalf("entries=%+v, want only the LLM API request error", entries)
	}
	data, _, err := archive.Read(entries[0].Name)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if !strings.Contains(string(data), "POST /v1/chat/completions") {
		t.Fatalf("snapshot=%q, want LLM API request", data)
	}
}

func TestServer_IncidentLogAPIHidesLegacyNonLLMRequestErrors(t *testing.T) {
	s := &Server{
		cfg:      config.Config{LogStorage: config.LogStorageConfig{Path: t.TempDir(), MaxFiles: 5}},
		proxylog: logmon.NewWriter(io.Discard),
	}
	archive, err := s.incidentLogStore()
	if err != nil {
		t.Fatalf("incidentLogStore: %v", err)
	}
	legacy, err := archive.Save(logarchive.KindRequestError, "/ui/assets/ui-assets-badge.js", []byte("request: GET /ui/assets/ui-assets-badge.js HTTP/1.1\n"))
	if err != nil {
		t.Fatalf("save legacy request error: %v", err)
	}
	llm, err := archive.Save(logarchive.KindRequestError, "/v1/chat/completions", []byte("request: POST /v1/chat/completions HTTP/1.1\n"))
	if err != nil {
		t.Fatalf("save LLM API request error: %v", err)
	}

	w := httptest.NewRecorder()
	s.handleAPIIncidentLogs(w, httptest.NewRequest(http.MethodGet, "/api/logs/incidents", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q", w.Code, w.Body.String())
	}
	var payload struct {
		Items []logarchive.Entry `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].Name != llm.Name {
		t.Fatalf("items=%+v, want only %q and not legacy %q", payload.Items, llm.Name, legacy.Name)
	}
}

func TestServer_IncidentLogAPIListsAndReadsForUnrestrictedIdentity(t *testing.T) {
	dir := t.TempDir()
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.cfg.LogStorage = config.LogStorageConfig{Path: dir, MaxFiles: 5}
	s.recordInferenceCrash("model/a", []byte("cuda crash output\n"), errors.New("exit status 9"))
	s.routes()

	listRequest := httptest.NewRequest(http.MethodGet, "/api/logs/incidents", nil)
	listResponse := httptest.NewRecorder()
	s.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("list status=%d body=%q", listResponse.Code, listResponse.Body.String())
	}
	var payload struct {
		Items []logarchive.Entry `json:"items"`
	}
	if err := json.Unmarshal(listResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(payload.Items) != 1 || payload.Items[0].Kind != logarchive.KindInferenceCrash {
		t.Fatalf("payload=%+v, want one crash entry", payload)
	}

	readRequest := httptest.NewRequest(http.MethodGet, "/api/logs/incidents/"+payload.Items[0].Name, nil)
	readResponse := httptest.NewRecorder()
	s.ServeHTTP(readResponse, readRequest)
	if readResponse.Code != http.StatusOK || !strings.Contains(readResponse.Body.String(), "cuda crash output") {
		t.Fatalf("read status=%d body=%q", readResponse.Code, readResponse.Body.String())
	}

	unsafeRequest := httptest.NewRequest(http.MethodGet, "/api/logs/incidents/%2e%2e%2foutside", nil)
	unsafeResponse := httptest.NewRecorder()
	s.ServeHTTP(unsafeResponse, unsafeRequest)
	if unsafeResponse.Code != http.StatusBadRequest && unsafeResponse.Code != http.StatusNotFound {
		t.Fatalf("unsafe read status=%d body=%q", unsafeResponse.Code, unsafeResponse.Body.String())
	}
	archive, err := s.incidentLogStore()
	if err != nil {
		t.Fatalf("incidentLogStore after unsafe read: %v", err)
	}
	entries, err := archive.List()
	if err != nil {
		t.Fatalf("List after unsafe read: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("non-LLM API request was archived: entries=%d", len(entries))
	}
}

func TestServer_IncidentLogAPIRejectsModelScopedIdentity(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	identity := auth.Identity{ID: "scoped", Scopes: map[string]struct{}{auth.ScopeLogs: {}}, Models: []string{"model"}}
	req := httptest.NewRequest(http.MethodGet, "/api/logs/incidents", nil).WithContext(withIdentity(context.Background(), identity))
	w := httptest.NewRecorder()
	s.handleAPIIncidentLogs(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q, want 403", w.Code, w.Body.String())
	}
}

func TestServer_RequestLogMiddlewareRecordsOnlyErrors(t *testing.T) {
	proxylog := logmon.NewWriter(io.Discard)
	recorded := make(chan RequestError, 1)
	middleware := CreateRequestLogMiddleware(proxylog, func(request RequestError) {
		recorded <- request
	})

	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	middleware(okHandler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/ok", nil))
	select {
	case event := <-recorded:
		t.Fatalf("recorded successful request: %+v", event)
	default:
	}

	errHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	})
	middleware(errHandler).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/failed", nil))
	select {
	case event := <-recorded:
		if event.Status != http.StatusBadGateway || event.Path != "/failed" {
			t.Fatalf("recorded event=%+v", event)
		}
	case <-time.After(time.Second):
		t.Fatal("error request was not recorded")
	}
}

// TestServer_RequestLogMiddlewarePublishesModelOutward covers the plumbing
// that makes the model name reach the archive at all: contexts flow downward
// only, so the logger creates a holder and the inner layers publish into it.
// This is the difference between "model: Qwen/…" in the archive and a 503
// with no subject.
func TestServer_RequestLogMiddlewarePublishesModelOutward(t *testing.T) {
	proxylog := logmon.NewWriter(io.Discard)
	recorded := make(chan RequestError, 1)
	middleware := CreateRequestLogMiddleware(proxylog, func(request RequestError) {
		recorded <- request
	})

	// A handler that resolves the model the way the request-context
	// middleware does: downstream, into a context it received.
	inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		swaputil.PublishRequestModel(r.Context(), "Qwen/Qwen3.8-27B-FP8")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"message":"in maintenance mode"}}`))
	})
	middleware(inner).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))

	select {
	case event := <-recorded:
		if event.Model != "Qwen/Qwen3.8-27B-FP8" {
			t.Errorf("event.Model = %q, want the published model name", event.Model)
		}
		if !strings.Contains(event.Detail, "maintenance mode") {
			t.Errorf("event.Detail = %q, want the response body captured", event.Detail)
		}
	case <-time.After(time.Second):
		t.Fatal("error request was not recorded")
	}

	// A request that never resolves a model must still record, with an empty
	// model rather than a panic or a placeholder.
	inner2 := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	})
	middleware(inner2).ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/nope", nil))
	select {
	case event := <-recorded:
		if event.Model != "" {
			t.Errorf("event.Model = %q, want empty when nothing was published", event.Model)
		}
	case <-time.After(time.Second):
		t.Fatal("error request was not recorded")
	}
}

func TestServer_IncidentLogPathDoesNotCreateOutsideDirectory(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: config.Config{LogStorage: config.LogStorageConfig{Path: dir}}}
	if _, err := s.incidentLogStore(); err != nil {
		t.Fatalf("incidentLogStore: %v", err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatalf("archive directory missing: %v", err)
	}
}

func TestServer_IncidentLogStoreHotUpdatesRetention(t *testing.T) {
	dir := t.TempDir()
	s := &Server{
		cfg:      config.Config{LogStorage: config.LogStorageConfig{Path: dir, MaxFiles: 3}},
		proxylog: logmon.NewWriter(io.Discard),
	}
	for i := 0; i < 3; i++ {
		s.recordRequestError(RequestError{Method: http.MethodPost, Path: "/v1/chat/completions"})
	}

	s.cfg.LogStorage.MaxFiles = 1
	archive, err := s.incidentLogStore()
	if err != nil {
		t.Fatalf("incidentLogStore after reload: %v", err)
	}
	entries, err := archive.List()
	if err != nil {
		t.Fatalf("List after reload: %v", err)
	}
	if len(entries) != 1 || archive.MaxFiles() != 1 {
		t.Fatalf("entries=%d maxFiles=%d, want one retained entry and maxFiles=1", len(entries), archive.MaxFiles())
	}
}
