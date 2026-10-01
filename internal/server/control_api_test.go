package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
)

func TestOptionalTimeDistinguishesOmittedAndNull(t *testing.T) {
	var omitted updateKeyRequest
	if err := json.Unmarshal([]byte(`{}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if omitted.ExpiresAt.Set {
		t.Fatal("omitted expiresAt should not be marked as set")
	}
	var cleared updateKeyRequest
	if err := json.Unmarshal([]byte(`{"expiresAt":null}`), &cleared); err != nil {
		t.Fatal(err)
	}
	if !cleared.ExpiresAt.Set || cleared.ExpiresAt.Value != nil {
		t.Fatalf("null expiry = %+v", cleared.ExpiresAt)
	}
}

func TestServer_APIKeyManagementLoginCanBeToggled(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	managerRequest := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"manager","allowManagementLogin":true}`))
	managerRequest.Header.Set("Content-Type", "application/json")
	managerResponse := httptest.NewRecorder()
	s.handleAPICreateKey(managerResponse, managerRequest)
	if managerResponse.Code != http.StatusOK {
		t.Fatalf("create management key status=%d body=%s", managerResponse.Code, managerResponse.Body.String())
	}

	createRequest := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"api-only","scopes":["inference"],"allowManagementLogin":false,"expiresAt":null}`))
	createRequest.Header.Set("Content-Type", "application/json")
	createResponse := httptest.NewRecorder()
	s.handleAPICreateKey(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created struct {
		Key    string             `json:"key"`
		Record store.APIKeyRecord `json:"record"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.Key == "" || created.Record.ID == "" || created.Record.AllowManagementLogin {
		t.Fatalf("created key=%+v", created)
	}

	login := func(secret string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"key":"`+secret+`"}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		s.handleAPIAuthSession(response, request)
		return response
	}
	if response := login(created.Key); response.Code != http.StatusForbidden {
		t.Fatalf("disabled management login status=%d body=%s", response.Code, response.Body.String())
	}

	updateRequest := httptest.NewRequest(http.MethodPatch, "/api/keys/"+created.Record.ID, strings.NewReader(`{"allowManagementLogin":true}`))
	updateRequest.Header.Set("Content-Type", "application/json")
	updateRequest.SetPathValue("id", created.Record.ID)
	updateResponse := httptest.NewRecorder()
	s.handleAPIUpdateKey(updateResponse, updateRequest)
	if updateResponse.Code != http.StatusOK {
		t.Fatalf("update status=%d body=%s", updateResponse.Code, updateResponse.Body.String())
	}
	var updated store.APIKeyRecord
	if err := json.Unmarshal(updateResponse.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update response: %v", err)
	}
	if !updated.AllowManagementLogin {
		t.Fatalf("updated key=%+v", updated)
	}
	if response := login(created.Key); response.Code != http.StatusOK {
		t.Fatalf("enabled management login status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServer_APIKeyManagementRecoveryCannotBeRemoved(t *testing.T) {
	createManager := func(t *testing.T) (*Server, string) {
		t.Helper()
		s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
		t.Cleanup(func() { _ = s.Shutdown(0) })
		request := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"manager","allowManagementLogin":true}`))
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		s.handleAPICreateKey(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("create status=%d body=%s", response.Code, response.Body.String())
		}
		var payload struct {
			Record store.APIKeyRecord `json:"record"`
		}
		if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return s, payload.Record.ID
	}

	for name, body := range map[string]string{
		"management login": `{"allowManagementLogin":false}`,
		"keys-admin scope": `{"scopes":["inference"]}`,
		"permanent expiry": `{"expiresAt":"2030-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s, id := createManager(t)
			request := httptest.NewRequest(http.MethodPatch, "/api/keys/"+id, strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			request.SetPathValue("id", id)
			response := httptest.NewRecorder()
			s.handleAPIUpdateKey(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("update status=%d body=%s", response.Code, response.Body.String())
			}
		})
	}

	t.Run("revoke", func(t *testing.T) {
		s, id := createManager(t)
		request := httptest.NewRequest(http.MethodDelete, "/api/keys/"+id, nil)
		request.SetPathValue("id", id)
		response := httptest.NewRecorder()
		s.handleAPIRevokeKey(response, request)
		if response.Code != http.StatusConflict {
			t.Fatalf("revoke final management key status=%d body=%s", response.Code, response.Body.String())
		}
	})
}

func TestServer_APIKeyManagementRecoveryIsRequiredAtBootstrap(t *testing.T) {
	for name, body := range map[string]string{
		"inference only":   `{"name":"inference-only","scopes":["inference"],"allowManagementLogin":false}`,
		"expiring manager": `{"name":"expiring-manager","allowManagementLogin":true,"expiresAt":"2030-01-01T00:00:00Z"}`,
	} {
		t.Run(name, func(t *testing.T) {
			s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
			t.Cleanup(func() { _ = s.Shutdown(0) })
			request := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(body))
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			s.handleAPICreateKey(response, request)
			if response.Code != http.StatusConflict {
				t.Fatalf("bootstrap status=%d body=%s", response.Code, response.Body.String())
			}
			keys, err := s.store.ListAPIKeys(context.Background(), true)
			if err != nil {
				t.Fatal(err)
			}
			if len(keys) != 0 {
				t.Fatalf("bootstrap failure created keys: %+v", keys)
			}
		})
	}
}

func TestServer_APIKeyUsagePathFilter(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	for _, entry := range []store.ActivityLogEntry{
		{Model: "m", KeyID: "key-a", Tokens: store.TokenMetrics{InputTokens: 3, OutputTokens: 2}},
		{Model: "m", KeyID: "key-b", Tokens: store.TokenMetrics{InputTokens: 100, OutputTokens: 100}},
	} {
		if _, err := s.store.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodGet, "/api/keys/key-a/usage", nil)
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var summary store.UsageSummary
	if err := json.Unmarshal(w.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 1 || summary.InputTokens != 3 || summary.OutputTokens != 2 {
		t.Fatalf("summary=%+v", summary)
	}
}

func TestServer_APIKeyUsageCannotReadAnotherManagedKey(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	auditSalt, auditDigest, err := auth.GenerateSalted("audit-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "audit-key", KeyHash: auditDigest, KeySalt: auditSalt,
		Scopes: []string{auth.ScopeAuditRead},
	}); err != nil {
		t.Fatal(err)
	}
	adminSalt, adminDigest, err := auth.GenerateSalted("keys-admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "keys-admin", KeyHash: adminDigest, KeySalt: adminSalt,
		Scopes: []string{auth.ScopeAuditRead, auth.ScopeKeysAdmin},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.InsertActivity(context.Background(), store.ActivityLogEntry{
		Model: "m", KeyID: "other-key", Tokens: store.TokenMetrics{InputTokens: 7},
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertAuditConversation(context.Background(), store.AuditConversation{ID: "other-conversation", Model: "m", KeyID: "other-key"}); err != nil {
		t.Fatal(err)
	}
	request := func(secret, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer "+secret)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if response := request("audit-secret", "/api/keys/other-key/usage"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-key usage status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("audit-secret", "/api/usage?key_id=other-key"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-key query usage status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("audit-secret", "/api/audit/conversations?key_id=other-key"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-key audit status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("audit-secret", "/api/metrics/activity?key_id=other-key"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-key activity status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("audit-secret", "/api/metrics/stats?key_id=other-key"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-key activity stats status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("audit-secret", "/api/audit/conversations/other-conversation"); response.Code != http.StatusForbidden {
		t.Fatalf("cross-key audit detail status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("keys-admin-secret", "/api/keys/other-key/usage"); response.Code != http.StatusOK {
		t.Fatalf("keys-admin usage status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("keys-admin-secret", "/api/audit/conversations?key_id=other-key"); response.Code != http.StatusOK {
		t.Fatalf("keys-admin audit status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("keys-admin-secret", "/api/metrics/activity?key_id=other-key"); response.Code != http.StatusOK {
		t.Fatalf("keys-admin activity status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("keys-admin-secret", "/api/metrics/stats?key_id=other-key"); response.Code != http.StatusOK {
		t.Fatalf("keys-admin activity stats status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request("keys-admin-secret", "/api/audit/conversations/other-conversation"); response.Code != http.StatusOK {
		t.Fatalf("keys-admin audit detail status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServer_RevokeMissingAPIKeyReturnsNotFound(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodDelete, "/api/keys/missing", nil)
	s.ServeHTTP(w, r)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%q, want 404", w.Code, w.Body.String())
	}
}

func TestServer_APIKeyNameValidation(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	for _, name := range []string{"line\nbreak", strings.Repeat("x", auth.MaxAPIKeyNameLength+1)} {
		body, err := json.Marshal(map[string]string{"name": name})
		if err != nil {
			t.Fatal(err)
		}
		r := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(string(body)))
		r.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("name %q status=%d body=%s, want 400", name, w.Code, w.Body.String())
		}
	}
}

func TestServer_SecretBearingResponsesDisableCaching(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	create := httptest.NewRequest(http.MethodPost, "/api/keys", strings.NewReader(`{"name":"one-time","scopes":["keys-admin","config-admin","inference"]}`))
	create.Header.Set("Content-Type", "application/json")
	createResponse := httptest.NewRecorder()
	s.ServeHTTP(createResponse, create)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	if got := createResponse.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("create Cache-Control=%q, want no-store", got)
	}
	var created struct {
		Key    string `json:"key"`
		Record struct {
			ID string `json:"id"`
		} `json:"record"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if created.Record.ID == "" {
		t.Fatal("create response did not include key record id")
	}
	if created.Key == "" {
		t.Fatal("create response did not include one-time key")
	}

	rotate := httptest.NewRequest(http.MethodPost, "/api/keys/"+created.Record.ID+"/rotate", nil)
	rotate.Header.Set("Authorization", "Bearer "+created.Key)
	rotateResponse := httptest.NewRecorder()
	s.ServeHTTP(rotateResponse, rotate)
	if rotateResponse.Code != http.StatusOK {
		t.Fatalf("rotate status=%d body=%s", rotateResponse.Code, rotateResponse.Body.String())
	}
	if got := rotateResponse.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("rotate Cache-Control=%q, want no-store", got)
	}
	var rotated struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(rotateResponse.Body.Bytes(), &rotated); err != nil {
		t.Fatal(err)
	}
	if rotated.Key == "" {
		t.Fatal("rotate response did not include one-time key")
	}

	deeplinkBody := `{"endpoint":"http://127.0.0.1:8080/v1","name":"local","includeKey":true,"key":"one-time-secret"}`
	deeplink := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(deeplinkBody))
	deeplink.Header.Set("Content-Type", "application/json")
	deeplink.Header.Set("Authorization", "Bearer "+rotated.Key)
	deeplinkResponse := httptest.NewRecorder()
	s.ServeHTTP(deeplinkResponse, deeplink)
	if deeplinkResponse.Code != http.StatusOK {
		t.Fatalf("deeplink status=%d body=%s", deeplinkResponse.Code, deeplinkResponse.Body.String())
	}
	if got := deeplinkResponse.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("deeplink Cache-Control=%q, want no-store", got)
	}
	if got := deeplinkResponse.Header().Get("Referrer-Policy"); got != "no-referrer" {
		t.Fatalf("deeplink Referrer-Policy=%q, want no-referrer", got)
	}

	// The browser action does not read the HttpOnly session key or ask the
	// operator to enter the endpoint again. The control API should derive both
	// values from the request and only include the current key when requested.
	browserDeeplink := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"name":"browser","includeKey":true}`))
	browserDeeplink.Host = "console.example:9292"
	browserDeeplink.Header.Set("Content-Type", "application/json")
	browserDeeplink.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: url.QueryEscape(rotated.Key)})
	browserResponse := httptest.NewRecorder()
	s.ServeHTTP(browserResponse, browserDeeplink)
	if browserResponse.Code != http.StatusOK {
		t.Fatalf("browser deeplink status=%d body=%s", browserResponse.Code, browserResponse.Body.String())
	}
	var browserLinks struct {
		Codex  string `json:"codex"`
		Claude string `json:"claude"`
	}
	if err := json.Unmarshal(browserResponse.Body.Bytes(), &browserLinks); err != nil {
		t.Fatalf("decode browser deeplink: %v", err)
	}
	for name, link := range map[string]string{"codex": browserLinks.Codex, "claude": browserLinks.Claude} {
		parsed, err := url.Parse(link)
		if err != nil {
			t.Fatalf("parse %s deeplink: %v", name, err)
		}
		if got, want := parsed.Query().Get("endpoint"), "http://console.example:9292/v1"; got != want {
			t.Fatalf("%s endpoint=%q, want %q", name, got, want)
		}
		if got, want := parsed.Query().Get("apiKey"), rotated.Key; got != want {
			t.Fatalf("%s apiKey=%q, want current session key", name, got)
		}
	}

	// llama-swap owns model selection and lifecycle locally. The CC Switch
	// import therefore accepts exactly one manager endpoint; it must not turn
	// this control API into a comma-separated upstream proxy configuration.
	for _, endpoint := range []string{
		"http://127.0.0.1:8080/v1,http://127.0.0.1:8081/v1",
		"http://127.0.0.1:8080/v1?api_key=leak",
	} {
		body := `{"endpoint":"` + endpoint + `","name":"local"}`
		request := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Authorization", "Bearer "+rotated.Key)
		response := httptest.NewRecorder()
		s.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("unsafe deeplink endpoint %q status=%d body=%s, want 400", endpoint, response.Code, response.Body.String())
		}
	}
}

func TestServer_CCSwitchDeepLinkUsesInferenceScope(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	secret := "inference-only-secret"
	salt, digest, err := auth.GenerateSalted(secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID:                   "inference-only",
		Name:                 "Inference only",
		KeyHash:              digest,
		KeySalt:              salt,
		Scopes:               []string{auth.ScopeInference},
		AllowManagementLogin: true,
	}); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"endpoint":"http://127.0.0.1:8080/v1","name":"local"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+secret)
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("inference-only deeplink status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestServer_CCSwitchDeepLinkUsesDefaultKeyWithoutAuthentication(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	request := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"local","includeKey":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("default-key deeplink status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode default-key deeplink: %v", err)
	}
	link, err := url.Parse(payload.Link)
	if err != nil {
		t.Fatalf("parse default-key deeplink: %v", err)
	}
	if got := link.Query().Get("apiKey"); got != defaultCCSwitchAPIKey {
		t.Fatalf("default-key apiKey=%q, want %q", got, defaultCCSwitchAPIKey)
	}
}

func TestServer_CCSwitchDeepLinkUsesRetainedSelectedKey(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}}}
	s.routes()

	insertKey := func(id, secret string, scopes []string, models []string) {
		t.Helper()
		salt, digest, err := auth.GenerateSalted(secret)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
			ID: id, Name: id, KeyHash: digest, KeySalt: salt, KeySecret: secret,
			Scopes: scopes, Models: models, AllowManagementLogin: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	insertKey("admin", "admin-secret", []string{auth.ScopeInference, auth.ScopeKeysAdmin}, nil)
	insertKey("selected", "selected-secret", []string{auth.ScopeInference}, []string{"allowed"})

	request := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"claude","name":"My Claude","model":"allowed","haikuModel":"allowed","includeKey":true,"keyId":"selected"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("selected key deeplink status=%d body=%s", response.Code, response.Body.String())
	}
	var payload struct {
		App  string `json:"app"`
		Link string `json:"link"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(payload.Link)
	if err != nil {
		t.Fatal(err)
	}
	if payload.App != "claude" || link.Query().Get("apiKey") != "selected-secret" || link.Query().Get("model") != "allowed" || link.Query().Get("haikuModel") != "allowed" {
		t.Fatalf("selected key deeplink payload=%+v query=%v", payload, link.Query())
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	listRequest.Header.Set("Authorization", "Bearer admin-secret")
	listResponse := httptest.NewRecorder()
	s.ServeHTTP(listResponse, listRequest)
	if listResponse.Code != http.StatusOK {
		t.Fatalf("key list status=%d body=%s", listResponse.Code, listResponse.Body.String())
	}
	if strings.Contains(listResponse.Body.String(), "selected-secret") || strings.Contains(listResponse.Body.String(), "admin-secret") {
		t.Fatalf("retained key leaked through key list: %s", listResponse.Body.String())
	}

	deniedRequest := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"claude","name":"My Claude","model":"allowed","opusModel":"denied","includeKey":true,"keyId":"selected"}`))
	deniedRequest.Header.Set("Content-Type", "application/json")
	deniedRequest.Header.Set("Authorization", "Bearer admin-secret")
	deniedResponse := httptest.NewRecorder()
	s.ServeHTTP(deniedResponse, deniedRequest)
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("out-of-scope Claude override status=%d body=%s, want 403", deniedResponse.Code, deniedResponse.Body.String())
	}
}

func TestServer_CCSwitchOptionsAndAutoCreatedKey(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}, "denied": {}}}
	s.routes()

	insertKey := func(id, secret string, scopes, models []string) {
		t.Helper()
		salt, digest, err := auth.GenerateSalted(secret)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
			ID: id, Name: id, KeyHash: digest, KeySalt: salt, KeySecret: secret,
			Scopes: scopes, Models: models, AllowManagementLogin: true,
		}); err != nil {
			t.Fatal(err)
		}
	}
	insertKey("admin", "admin-secret", []string{auth.ScopeInference, auth.ScopeKeysAdmin}, nil)
	insertKey("selected", "selected-secret", []string{auth.ScopeInference}, []string{"allowed"})

	optionsRequest := httptest.NewRequest(http.MethodGet, "/api/cc-switch/options", nil)
	optionsRequest.Header.Set("Authorization", "Bearer admin-secret")
	optionsResponse := httptest.NewRecorder()
	s.ServeHTTP(optionsResponse, optionsRequest)
	if optionsResponse.Code != http.StatusOK {
		t.Fatalf("options status=%d body=%s", optionsResponse.Code, optionsResponse.Body.String())
	}
	if strings.Contains(optionsResponse.Body.String(), "admin-secret") || strings.Contains(optionsResponse.Body.String(), "selected-secret") {
		t.Fatalf("options leaked a retained secret: %s", optionsResponse.Body.String())
	}
	var options struct {
		Data []ccSwitchKeyOption `json:"data"`
	}
	if err := json.Unmarshal(optionsResponse.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if len(options.Data) != 2 {
		t.Fatalf("options=%+v, want both selectable inference keys", options.Data)
	}

	restrictedOptionsRequest := httptest.NewRequest(http.MethodGet, "/api/cc-switch/options", nil)
	restrictedOptionsRequest.Header.Set("Authorization", "Bearer selected-secret")
	restrictedOptionsResponse := httptest.NewRecorder()
	s.ServeHTTP(restrictedOptionsResponse, restrictedOptionsRequest)
	if restrictedOptionsResponse.Code != http.StatusOK {
		t.Fatalf("restricted options status=%d body=%s", restrictedOptionsResponse.Code, restrictedOptionsResponse.Body.String())
	}
	options = struct {
		Data []ccSwitchKeyOption `json:"data"`
	}{}
	if err := json.Unmarshal(restrictedOptionsResponse.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if len(options.Data) != 1 || options.Data[0].ID != "selected" {
		t.Fatalf("restricted options=%+v, want current key only", options.Data)
	}

	createRequest := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"allowed","model":"allowed","createKey":true}`))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Authorization", "Bearer selected-secret")
	createResponse := httptest.NewRecorder()
	s.ServeHTTP(createResponse, createRequest)
	if createResponse.Code != http.StatusOK {
		t.Fatalf("auto-create status=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
	var created struct {
		Link  string `json:"link"`
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal(createResponse.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(created.Link)
	if err != nil {
		t.Fatal(err)
	}
	secret := link.Query().Get("apiKey")
	if created.KeyID == "" || secret == "" {
		t.Fatalf("created payload=%+v query=%v", created, link.Query())
	}
	record, found, err := s.store.FindAPIKeyBySecret(t.Context(), secret)
	if err != nil || !found {
		t.Fatalf("find auto-created key found=%v err=%v", found, err)
	}
	if record.ID != created.KeyID || record.Name != "allowed" || record.AllowManagementLogin || len(record.Scopes) != 1 || record.Scopes[0] != auth.ScopeInference || len(record.Models) != 1 || record.Models[0] != "allowed" {
		t.Fatalf("auto-created key=%+v", record)
	}
}

func TestServer_SeedsLegacyAPIKeysForCCSwitch(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	secret := "legacy-configured-secret"
	s.cfg = config.Config{
		RequiredAPIKeys: []string{secret},
		Models:          map[string]config.ModelConfig{"allowed": {}},
	}
	s.routes()
	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatal(err)
	}

	id := store.KeyFingerprint(secret)
	retained, found, err := s.store.GetAPIKeySecret(t.Context(), id)
	if err != nil || !found || retained != secret {
		t.Fatalf("legacy retained secret=%q found=%v err=%v", retained, found, err)
	}

	optionsRequest := httptest.NewRequest(http.MethodGet, "/api/cc-switch/options", nil)
	optionsRequest.Header.Set("Authorization", "Bearer "+secret)
	optionsResponse := httptest.NewRecorder()
	s.ServeHTTP(optionsResponse, optionsRequest)
	if optionsResponse.Code != http.StatusOK {
		t.Fatalf("options status=%d body=%s", optionsResponse.Code, optionsResponse.Body.String())
	}
	if strings.Contains(optionsResponse.Body.String(), secret) {
		t.Fatalf("legacy secret leaked in options: %s", optionsResponse.Body.String())
	}
	var options struct {
		Data []ccSwitchKeyOption `json:"data"`
	}
	if err := json.Unmarshal(optionsResponse.Body.Bytes(), &options); err != nil {
		t.Fatal(err)
	}
	if len(options.Data) != 1 || options.Data[0].ID != id {
		t.Fatalf("legacy options=%+v, want persisted key %q", options.Data, id)
	}

	deeplinkRequest := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"legacy","model":"allowed","keyId":"`+id+`"}`))
	deeplinkRequest.Header.Set("Content-Type", "application/json")
	deeplinkRequest.Header.Set("Authorization", "Bearer "+secret)
	deeplinkResponse := httptest.NewRecorder()
	s.ServeHTTP(deeplinkResponse, deeplinkRequest)
	if deeplinkResponse.Code != http.StatusOK {
		t.Fatalf("deeplink status=%d body=%s", deeplinkResponse.Code, deeplinkResponse.Body.String())
	}
	var payload struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(deeplinkResponse.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(payload.Link)
	if err != nil {
		t.Fatal(err)
	}
	if link.Query().Get("apiKey") != secret {
		t.Fatalf("legacy deeplink key=%q, want retained secret", link.Query().Get("apiKey"))
	}
}

func TestServer_ModelScopedControlReadsCannotLeakOtherModels(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"allowed": {},
		"denied":  {},
	}}
	s.routes()
	salt, digest, err := auth.GenerateSalted("scoped-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{ID: "scoped", KeyHash: digest, KeySalt: salt, Scopes: []string{auth.ScopeAuditRead}, Models: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []store.ActivityLogEntry{
		{Model: "allowed", Tokens: store.TokenMetrics{InputTokens: 3, OutputTokens: 2}},
		{Model: "denied", Tokens: store.TokenMetrics{InputTokens: 100, OutputTokens: 100}},
	} {
		if _, err := s.store.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	for _, conversation := range []store.AuditConversation{
		{ID: "allowed-audit", Model: "allowed"},
		{ID: "denied-audit", Model: "denied"},
	} {
		if err := s.store.InsertAuditConversation(context.Background(), conversation); err != nil {
			t.Fatal(err)
		}
	}
	authenticated := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer scoped-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	usage := authenticated("/api/usage")
	if usage.Code != http.StatusOK {
		t.Fatalf("usage status=%d body=%s", usage.Code, usage.Body.String())
	}
	var summary store.UsageSummary
	if err := json.Unmarshal(usage.Body.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	if summary.Requests != 1 || summary.InputTokens != 3 || len(summary.ByModel) != 1 || summary.ByModel[0].Model != "allowed" {
		t.Fatalf("scoped usage leaked rows: %+v", summary)
	}
	audit := authenticated("/api/audit/conversations")
	if audit.Code != http.StatusOK {
		t.Fatalf("audit status=%d body=%s", audit.Code, audit.Body.String())
	}
	var payload struct {
		Data       []store.AuditConversation `json:"data"`
		Page       int                       `json:"page"`
		Limit      int                       `json:"limit"`
		Total      int                       `json:"total"`
		TotalPages int                       `json:"total_pages"`
	}
	if err := json.Unmarshal(audit.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 1 || payload.Data[0].Model != "allowed" || payload.Page != 1 || payload.Limit != 25 || payload.Total != 1 || payload.TotalPages != 1 {
		t.Fatalf("scoped audit leaked rows: %+v", payload.Data)
	}
	conversation := authenticated("/api/audit/conversations/allowed-audit")
	if conversation.Code != http.StatusOK {
		t.Fatalf("single audit status=%d body=%s", conversation.Code, conversation.Body.String())
	}
	var single store.AuditConversation
	if err := json.Unmarshal(conversation.Body.Bytes(), &single); err != nil {
		t.Fatal(err)
	}
	if single.ID != "allowed-audit" || single.Model != "allowed" {
		t.Fatalf("single audit = %+v", single)
	}
	if deniedConversation := authenticated("/api/audit/conversations/denied-audit"); deniedConversation.Code != http.StatusForbidden {
		t.Fatalf("denied single audit status=%d body=%s", deniedConversation.Code, deniedConversation.Body.String())
	}
	forbidden := authenticated("/api/usage?model=denied")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden model status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
}

func TestServer_ModelScopedActivityReadsCannotLeakOtherModels(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"allowed": {Aliases: []string{"friendly"}},
		"denied":  {},
	}}
	s.routes()
	salt, digest, err := auth.GenerateSalted("activity-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "activity", KeyHash: digest, KeySalt: salt,
		Scopes: []string{auth.ScopeAuditRead}, Models: []string{"allowed"},
	}); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []store.ActivityLogEntry{
		{Model: "allowed", Tokens: store.TokenMetrics{InputTokens: 3, OutputTokens: 2}},
		{Model: "denied", Tokens: store.TokenMetrics{InputTokens: 100, OutputTokens: 100}},
	} {
		if _, err := s.store.InsertActivity(context.Background(), entry); err != nil {
			t.Fatal(err)
		}
	}
	request := func(path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodGet, path, nil)
		r.Header.Set("Authorization", "Bearer activity-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	page := request("/api/metrics/activity")
	if page.Code != http.StatusOK {
		t.Fatalf("activity status=%d body=%s", page.Code, page.Body.String())
	}
	var activity store.ActivityPage
	if err := json.Unmarshal(page.Body.Bytes(), &activity); err != nil {
		t.Fatal(err)
	}
	if activity.Total != 1 || len(activity.Data) != 1 || activity.Data[0].Model != "allowed" {
		t.Fatalf("scoped activity leaked rows: %+v", activity)
	}
	stats := request("/api/metrics/stats")
	if stats.Code != http.StatusOK {
		t.Fatalf("stats status=%d body=%s", stats.Code, stats.Body.String())
	}
	var activityStats store.ActivityStats
	if err := json.Unmarshal(stats.Body.Bytes(), &activityStats); err != nil {
		t.Fatal(err)
	}
	if activityStats.TotalRequests != 1 || activityStats.TotalInputTokens != 3 || activityStats.TotalOutputTokens != 2 {
		t.Fatalf("scoped stats leaked rows: %+v", activityStats)
	}
	if forbidden := request("/api/metrics/activity?model=denied"); forbidden.Code != http.StatusForbidden {
		t.Fatalf("forbidden activity model status=%d body=%s", forbidden.Code, forbidden.Body.String())
	}
	if alias := request("/api/metrics/activity?model=friendly"); alias.Code != http.StatusOK {
		t.Fatalf("allowed alias status=%d body=%s", alias.Code, alias.Body.String())
	}
}

func TestServer_AuditConversationUsesActivityID(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"model": {}}}
	s.routes()
	activity, err := s.store.InsertActivity(context.Background(), store.ActivityLogEntry{
		Model:          "model",
		ReqPath:        "/v1/chat/completions",
		RespStatusCode: http.StatusOK,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.InsertAuditConversation(context.Background(), store.AuditConversation{
		ID:             "internal-only",
		ActivityID:     activity.ID,
		Model:          activity.Model,
		ReqPath:        activity.ReqPath,
		ResponseStatus: activity.RespStatusCode,
	}); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "/api/audit/conversations/"+strconv.Itoa(activity.ID), nil)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	var conversation store.AuditConversation
	if err := json.Unmarshal(w.Body.Bytes(), &conversation); err != nil {
		t.Fatal(err)
	}
	if conversation.ActivityID != activity.ID || conversation.ID != "internal-only" {
		t.Fatalf("conversation=%+v", conversation)
	}
}

// TestServer_AuditConversationBodyIsStreamedSeparately pins the split that makes
// this feature usable: the detail view publishes a blob reference and the
// logical size, and the body is streamed on demand. Answering with every turn
// inline is what made a conversation with attachments slow to open — tens of
// megabytes per response, plus the third that base64 adds on top.
func TestServer_AuditConversationBodyIsStreamedSeparately(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	// A file-backed store externalizes bodies; the in-memory default keeps them
	// inline and has nothing to stream.
	fileStore, err := store.New(filepath.Join(t.TempDir(), "db.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = fileStore.Close() })
	s.store = fileStore
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"model": {}}}

	activity, err := fileStore.InsertActivity(context.Background(), store.ActivityLogEntry{
		Model: "model", ReqPath: "/v1/responses", RespStatusCode: http.StatusOK,
	})
	if err != nil {
		t.Fatal(err)
	}
	reqBody := bytes.Repeat([]byte(`{"input":"hello"}`), 256)
	respBody := []byte(`{"id":"resp_1","status":"completed"}`)
	if err := fileStore.InsertAuditConversation(context.Background(), store.AuditConversation{
		ID: "conv-body", ActivityID: activity.ID, Model: activity.Model, ReqPath: activity.ReqPath,
		ResponseStatus: activity.RespStatusCode, RequestBody: reqBody, ResponseBody: respBody,
	}); err != nil {
		t.Fatal(err)
	}

	detailPath := "/api/audit/conversations/" + strconv.Itoa(activity.ID)
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, detailPath, nil))
	if w.Code != http.StatusOK {
		t.Fatalf("detail status=%d body=%s", w.Code, w.Body.String())
	}
	if w.Body.Len() >= len(reqBody) {
		t.Fatalf("detail response is %d bytes, want it to exclude the %d-byte body", w.Body.Len(), len(reqBody))
	}
	var conversation store.AuditConversation
	if err := json.Unmarshal(w.Body.Bytes(), &conversation); err != nil {
		t.Fatal(err)
	}
	if conversation.RequestBodyRef == "" || conversation.RequestBodyBytes != int64(len(reqBody)) {
		t.Fatalf("detail requestBodyRef=%q requestBodyBytes=%d, want a reference and %d",
			conversation.RequestBodyRef, conversation.RequestBodyBytes, len(reqBody))
	}
	if len(conversation.RequestBody) != 0 {
		t.Fatalf("detail carried %d inline body bytes, want the reference only", len(conversation.RequestBody))
	}

	for _, tc := range []struct {
		which string
		want  []byte
	}{
		{"request", reqBody},
		{"response", respBody},
	} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, detailPath+"/body/"+tc.which, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s body status=%d body=%s", tc.which, w.Code, w.Body.String())
		}
		if !bytes.Equal(w.Body.Bytes(), tc.want) {
			t.Fatalf("%s body streamed %d bytes, want %d", tc.which, w.Body.Len(), len(tc.want))
		}
	}

	// An unknown selector is a 404, never a silent empty body.
	w = httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, detailPath+"/body/headers", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("unknown selector status=%d, want 404", w.Code)
	}
}

func TestServer_ModelScopedAuditDestructiveActionsAreBounded(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}, "denied": {}}}
	s.routes()
	salt, digest, err := auth.GenerateSalted("admin-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{ID: "admin", KeyHash: digest, KeySalt: salt, Scopes: []string{auth.ScopeAuditAdmin}, Models: []string{"allowed"}}); err != nil {
		t.Fatal(err)
	}
	for _, conversation := range []store.AuditConversation{{ID: "allowed", Model: "allowed"}, {ID: "denied", Model: "denied"}} {
		if err := s.store.InsertAuditConversation(context.Background(), conversation); err != nil {
			t.Fatal(err)
		}
	}
	request := func(method, path string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, path, nil)
		r.Header.Set("Authorization", "Bearer admin-secret")
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		return w
	}
	if response := request(http.MethodDelete, "/api/audit/conversations/denied"); response.Code != http.StatusForbidden {
		t.Fatalf("denied delete status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(http.MethodPost, "/api/audit/clear"); response.Code != http.StatusForbidden {
		t.Fatalf("scoped clear status=%d body=%s", response.Code, response.Body.String())
	}
	if response := request(http.MethodDelete, "/api/audit/conversations/allowed"); response.Code != http.StatusNoContent {
		t.Fatalf("allowed delete status=%d body=%s", response.Code, response.Body.String())
	}
}
