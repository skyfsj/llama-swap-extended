package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func seedAffinity(t *testing.T, s *Server, id, owner string) {
	t.Helper()
	now := time.Now()
	if err := s.store.UpsertResponseAffinity(context.Background(), store.ResponseAffinity{
		ResponseID: id, Model: "m", Backend: "vllm", Status: "completed",
		Response:   []byte(`{"id":"` + id + `","object":"response","status":"completed","model":"m","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"secret"}]}]}`),
		IdentityID: owner,
		ExpiresAt:  now.Add(time.Hour), CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
}

// TestServer_StoredResponseIsScopedToItsKey guards the ownership check: a key
// that may use the model must still not read a response another key stored.
func TestServer_StoredResponseIsScopedToItsKey(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"m"}, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	seedAffinity(t, s, "resp_owned_by_a", "key-a")

	get := func(keyID string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_owned_by_a", nil)
		request.SetPathValue("response_id", "resp_owned_by_a")
		request = request.WithContext(withIdentity(request.Context(), auth.Identity{ID: keyID, Scopes: map[string]struct{}{"inference": {}}}))
		recorder := httptest.NewRecorder()
		s.handleAPIResponseGet(recorder, request)
		return recorder
	}
	if recorder := get("key-a"); recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), "secret") {
		t.Fatalf("owner denied its own response: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := get("key-b"); recorder.Code != http.StatusForbidden {
		t.Fatalf("foreign key read a stored response: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestServer_LegacyStoredResponseStaysModelScoped keeps pre-migration rows
// usable: an empty owner is the documented upgrade path.
func TestServer_LegacyStoredResponseStaysModelScoped(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"m"}, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	seedAffinity(t, s, "resp_legacy", "")
	request := httptest.NewRequest(http.MethodGet, "/v1/responses/resp_legacy", nil)
	request.SetPathValue("response_id", "resp_legacy")
	request = request.WithContext(withIdentity(request.Context(), auth.Identity{ID: "any-key", Scopes: map[string]struct{}{"inference": {}}}))
	recorder := httptest.NewRecorder()
	s.handleAPIResponseGet(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("legacy row became unreadable: %d %s", recorder.Code, recorder.Body.String())
	}
}

// TestServer_ResponseCancelDoesNotRewriteStatus guards the honest-cancel fix: a
// finished response must come back with its real status.
func TestServer_ResponseCancelDoesNotRewriteStatus(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"m"}, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"m": {Backend: config.BackendConfig{Type: "vllm", Protocol: config.AdapterProtocolResponsesToChat}},
	}}
	seedAffinity(t, s, "resp_done", "key-a")

	request := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_done/cancel", nil)
	request.SetPathValue("response_id", "resp_done")
	request = request.WithContext(withIdentity(request.Context(), auth.Identity{ID: "key-a", Scopes: map[string]struct{}{"inference": {}}}))
	recorder := httptest.NewRecorder()
	s.handleAPIResponseCancel(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("cancel status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if strings.Contains(recorder.Body.String(), "cancelled") {
		t.Fatalf("cancel rewrote a completed response: %s", recorder.Body.String())
	}
	stored, found, err := s.store.GetResponseAffinity(context.Background(), "resp_done", time.Now())
	if err != nil || !found {
		t.Fatalf("stored row: %v %v", found, err)
	}
	if stored.Status != "completed" {
		t.Fatalf("stored status = %q, want completed", stored.Status)
	}
}

// TestServer_ResponseCancelUnknownIdSaysWhy documents the in-flight case.
func TestServer_ResponseCancelUnknownIdSaysWhy(t *testing.T) {
	s := newTestServer(newStubRouter([]string{"m"}, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.store.Close() })
	request := httptest.NewRequest(http.MethodPost, "/v1/responses/resp_missing/cancel", nil)
	request.SetPathValue("response_id", "resp_missing")
	recorder := httptest.NewRecorder()
	s.handleAPIResponseCancel(recorder, request)
	if recorder.Code != http.StatusNotFound || !strings.Contains(recorder.Body.String(), "connection") {
		t.Fatalf("unknown id response = %d %s", recorder.Code, recorder.Body.String())
	}
}
