package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestServer_CreateAccessKeyRequiresManagementParent(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	// Without any management key there is nothing to nest under.
	orphan := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"kind":"access","name":"child"}`))
	orphan.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.handleAPICreateKey(response, orphan)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("orphan access key status=%d body=%s", response.Code, response.Body.String())
	}

	manager := createTestKey(t, s, `{"kind":"management","name":"manager","allowManagementLogin":true}`)
	if manager.Kind != auth.KeyKindManagement {
		t.Fatalf("management key record = %#v", manager)
	}

	// A revoked parent cannot gain new children.
	if err := s.store.RevokeAPIKeyCascade(t.Context(), manager.ID, time.Now()); err != nil {
		t.Fatal(err)
	}
	revokedParent := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"kind":"access","name":"child","parentId":"`+manager.ID+`"}`))
	revokedParent.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	s.handleAPICreateKey(response, revokedParent)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("access key under revoked parent status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServer_CreateAccessKeyAppliesRestrictionsAndParentModels(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	manager := createTestKey(t, s, `{"kind":"management","name":"manager","allowManagementLogin":true,"models":["gpt-4","claude"]}`)
	child := createTestKey(t, s, `{"kind":"access","name":"client","parentId":"`+manager.ID+`","models":["gpt-4"],"allowedIps":["203.0.113.0/24","10.0.0.1"],"maxConcurrency":4,"group":"team-a"}`)
	if child.Kind != auth.KeyKindAccess || child.ParentID != manager.ID {
		t.Fatalf("access key record = %#v", child)
	}
	if len(child.AllowedIPs) != 2 || child.MaxConcurrency != 4 || child.Group != "team-a" {
		t.Fatalf("access key restrictions = %#v", child)
	}
	if child.AllowManagementLogin {
		t.Fatal("access key was allowed to sign in to the management panel")
	}
	if len(child.Models) != 1 || child.Models[0] != "gpt-4" {
		t.Fatalf("access key models = %#v", child.Models)
	}

	// A model outside the parent's allowlist is rejected rather than silently
	// narrowed, so the operator sees the conflict instead of a guess.
	widened := httptest.NewRequest(http.MethodPatch, "/api/keys/"+child.ID, strings.NewReader(`{"models":["other-model"]}`))
	widened.Header.Set("Content-Type", "application/json")
	widened.SetPathValue("id", child.ID)
	response := httptest.NewRecorder()
	s.handleAPIUpdateKey(response, widened)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("widen access key status=%d body=%s", response.Code, response.Body.String())
	}
	if !strings.Contains(response.Body.String(), "must overlap") {
		t.Fatalf("widen body = %s", response.Body.String())
	}
	// A model the parent also allows is accepted.
	overlapping := httptest.NewRequest(http.MethodPatch, "/api/keys/"+child.ID, strings.NewReader(`{"models":["claude"]}`))
	overlapping.Header.Set("Content-Type", "application/json")
	overlapping.SetPathValue("id", child.ID)
	response = httptest.NewRecorder()
	s.handleAPIUpdateKey(response, overlapping)
	if response.Code != http.StatusOK {
		t.Fatalf("allowed model update status=%d body=%s", response.Code, response.Body.String())
	}
	var narrowed store.APIKeyRecord
	if err := json.Unmarshal(response.Body.Bytes(), &narrowed); err != nil {
		t.Fatal(err)
	}
	if len(narrowed.Models) != 1 || narrowed.Models[0] != "claude" {
		t.Fatalf("updated access key models = %#v", narrowed.Models)
	}

	// Access-key restrictions stay editable.
	updated := httptest.NewRequest(http.MethodPatch, "/api/keys/"+child.ID, strings.NewReader(`{"allowedIps":["198.51.100.0/24"],"maxConcurrency":9,"group":"team-b"}`))
	updated.Header.Set("Content-Type", "application/json")
	updated.SetPathValue("id", child.ID)
	response = httptest.NewRecorder()
	s.handleAPIUpdateKey(response, updated)
	if response.Code != http.StatusOK {
		t.Fatalf("update restrictions status=%d body=%s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &narrowed); err != nil {
		t.Fatal(err)
	}
	if len(narrowed.AllowedIPs) != 1 || narrowed.AllowedIPs[0] != "198.51.100.0/24" || narrowed.MaxConcurrency != 9 || narrowed.Group != "team-b" {
		t.Fatalf("updated restrictions = %#v", narrowed)
	}
	// Scopes cannot be smuggled in through an access key.
	scopeEscalation := httptest.NewRequest(http.MethodPatch, "/api/keys/"+child.ID, strings.NewReader(`{"scopes":["keys-admin"]}`))
	scopeEscalation.Header.Set("Content-Type", "application/json")
	scopeEscalation.SetPathValue("id", child.ID)
	response = httptest.NewRecorder()
	s.handleAPIUpdateKey(response, scopeEscalation)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("access key scope escalation status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServer_RevokeManagementKeyCascadesThroughAPI(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	// A second permanent management key, so revoking the first does not trip
	// the recovery-key guard.
	createTestKey(t, s, `{"kind":"management","name":"backup","allowManagementLogin":true}`)
	manager := createTestKey(t, s, `{"kind":"management","name":"manager","allowManagementLogin":true}`)
	child := createTestKey(t, s, `{"kind":"access","name":"client","parentId":"`+manager.ID+`"}`)

	// A rotation on the child keeps the same identity fields.
	rotate := httptest.NewRequest(http.MethodPost, "/api/keys/"+child.ID+"/rotate", nil)
	rotate.SetPathValue("id", child.ID)
	response := httptest.NewRecorder()
	s.handleAPIRotateKey(response, rotate)
	if response.Code != http.StatusOK {
		t.Fatalf("rotate access key status=%d body=%s", response.Code, response.Body.String())
	}
	var rotated struct {
		Key    string             `json:"key"`
		Record store.APIKeyRecord `json:"record"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Record.Kind != auth.KeyKindAccess || rotated.Record.ParentID != manager.ID {
		t.Fatalf("rotated access key = %#v", rotated.Record)
	}

	revoke := httptest.NewRequest(http.MethodDelete, "/api/keys/"+manager.ID, nil)
	revoke.SetPathValue("id", manager.ID)
	response = httptest.NewRecorder()
	s.handleAPIRevokeKey(response, revoke)
	if response.Code != http.StatusNoContent {
		t.Fatalf("revoke management key status=%d body=%s", response.Code, response.Body.String())
	}
	keys, err := s.store.ListAPIKeys(t.Context(), false)
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keys {
		if key.ID == manager.ID || key.ParentID == manager.ID {
			t.Fatalf("key %q is still active after the parent was revoked", key.ID)
		}
	}
}

func createTestKey(t *testing.T, s *Server, body string) store.APIKeyRecord {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.handleAPICreateKey(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("create key status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Key    string             `json:"key"`
		Record store.APIKeyRecord `json:"record"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Key == "" {
		t.Fatal("created key did not return a secret")
	}
	return payload.Record
}
