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
	"time"

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
	// CC Switch configures each application with the base URL it appends its own
	// request path to: Claude Code posts to {base}/v1/messages and the Gemini CLI
	// to {base}/v1beta/models/..., while Codex posts to {base}/responses and the
	// AI-SDK clients to {base}/chat/completions.
	for name, link := range map[string]string{"codex": browserLinks.Codex, "claude": browserLinks.Claude} {
		parsed, err := url.Parse(link)
		if err != nil {
			t.Fatalf("parse %s deeplink: %v", name, err)
		}
		wantEndpoint := "http://console.example:9292/v1"
		wantFormat := "openai_responses"
		if name == "claude" {
			wantEndpoint = "http://console.example:9292"
			wantFormat = "anthropic"
		}
		if got := parsed.Query().Get("endpoint"); got != wantEndpoint {
			t.Fatalf("%s endpoint=%q, want %q", name, got, wantEndpoint)
		}
		if got := parsed.Query().Get("apiFormat"); got != wantFormat {
			t.Fatalf("%s apiFormat=%q, want %q", name, got, wantFormat)
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
		App       string `json:"app"`
		Link      string `json:"link"`
		Endpoint  string `json:"endpoint"`
		ApiFormat string `json:"apiFormat"`
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
	if payload.ApiFormat != "anthropic" || payload.Endpoint != "http://example.com" {
		t.Fatalf("resolved import fields = %+v, want anthropic at the bare origin", payload)
	}
	if got := link.Query().Get("endpoint"); got != payload.Endpoint {
		t.Fatalf("link endpoint=%q, response endpoint=%q", got, payload.Endpoint)
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

	// A management key may create a client key even when it only carries the
	// inference scope (the UI's "auto key" flow). The generated credential is
	// nested under it, so it cannot exceed the caller's own access.
	allowedCreate := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"client","model":"allowed","createKey":true}`))
	allowedCreate.Header.Set("Content-Type", "application/json")
	allowedCreate.Header.Set("Authorization", "Bearer selected-secret")
	allowedResponse := httptest.NewRecorder()
	s.ServeHTTP(allowedResponse, allowedCreate)
	if allowedResponse.Code != http.StatusOK {
		t.Fatalf("management-key auto-create status=%d body=%s, want 200", allowedResponse.Code, allowedResponse.Body.String())
	}

	// An access key is a model credential, not a control-plane credential: it
	// must never mint another key.
	accessSalt, accessDigest, err := auth.GenerateSalted("access-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "access", Name: "access", KeyHash: accessDigest, KeySalt: accessSalt,
		KeySecret: "access-secret", Kind: auth.KeyKindAccess, ParentID: "admin",
		Scopes: []string{auth.ScopeInference}, Models: []string{"allowed"},
	}); err != nil {
		t.Fatal(err)
	}
	s.refreshKeyCache()
	denied := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"escalated","model":"allowed","createKey":true}`))
	denied.Header.Set("Content-Type", "application/json")
	denied.Header.Set("Authorization", "Bearer access-secret")
	deniedResponse := httptest.NewRecorder()
	s.ServeHTTP(deniedResponse, denied)
	if deniedResponse.Code != http.StatusForbidden {
		t.Fatalf("access-key auto-create status=%d body=%s, want 403", deniedResponse.Code, deniedResponse.Body.String())
	}

	// A keys-admin key may create one, and the new credential is nested under
	// the caller and inherits its constraints.
	createRequest := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"allowed","model":"allowed","createKey":true}`))
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("Authorization", "Bearer admin-secret")
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
	if record.Kind != auth.KeyKindAccess || record.ParentID != "admin" {
		t.Fatalf("auto-created key kind=%q parent=%q, want an access key under admin", record.Kind, record.ParentID)
	}
}

// TestServer_CCSwitchAutoCreatedKeyInheritsCallerRestrictions is the
// regression guard for credential-laundering: an access key that survived the
// scope check must still produce a child no stronger than itself.
func TestServer_CCSwitchAutoCreatedKeyInheritsCallerRestrictions(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}}}
	s.routes()

	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)
	salt, digest, err := auth.GenerateSalted("boss-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "boss", Name: "boss", KeyHash: digest, KeySalt: salt, KeySecret: "boss-secret",
		Kind: auth.KeyKindManagement, AllowManagementLogin: true,
		Scopes: []string{auth.ScopeInference, auth.ScopeKeysAdmin},
		Models: []string{"allowed"}, AllowedIPs: []string{"203.0.113.0/24"},
		MaxConcurrency: 4, ExpiresAt: &expires,
	}); err != nil {
		t.Fatal(err)
	}
	s.refreshKeyCache()

	request := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"child","model":"allowed","createKey":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer boss-secret")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("auto-create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(created.Link)
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := s.store.FindAPIKeyBySecret(t.Context(), link.Query().Get("apiKey"))
	if err != nil || !found {
		t.Fatalf("find child found=%v err=%v", found, err)
	}
	if record.Kind != auth.KeyKindAccess || record.ParentID != "boss" {
		t.Fatalf("child kind=%q parent=%q", record.Kind, record.ParentID)
	}
	if len(record.AllowedIPs) != 1 || record.AllowedIPs[0] != "203.0.113.0/24" {
		t.Fatalf("child allowedIps=%v, want the parent's", record.AllowedIPs)
	}
	if record.MaxConcurrency != 4 {
		t.Fatalf("child maxConcurrency=%d, want the parent's 4", record.MaxConcurrency)
	}
	if record.ExpiresAt == nil || !record.ExpiresAt.Equal(expires) {
		t.Fatalf("child expiresAt=%v, want the parent's %v", record.ExpiresAt, expires)
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

// TestServer_AuthSessionGetIsThrottled pins the oracle: GET reports whether a
// credential is valid, so it must share the failed-login budget with POST.
// Otherwise guessing simply moves to the read-only form.
func TestServer_AuthSessionGetIsThrottled(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{
		RequiredAPIKeys: []string{"correct-key"},
		Security: config.SecurityConfig{AuthRateLimit: config.AuthRateLimitConfig{
			MaxFailures: 3, Window: time.Minute, BlockFor: time.Minute,
		}},
	}
	s.routes()

	get := func(bearer string) int {
		request := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
		request.RemoteAddr = "198.51.100.4:5000"
		if bearer != "" {
			request.Header.Set("Authorization", "Bearer "+bearer)
		}
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, request)
		return recorder.Code
	}

	for attempt := 1; attempt <= 3; attempt++ {
		if code := get("wrong-key"); code != http.StatusOK {
			t.Fatalf("guess %d status=%d, want the oracle to answer 200 while under the limit", attempt, code)
		}
	}
	if code := get("wrong-key"); code != http.StatusTooManyRequests {
		t.Fatalf("guess after the budget status=%d, want 429", code)
	}

	// Anonymous polling by the login screen must never be penalised.
	other := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
	other.RemoteAddr = "203.0.113.9:5000"
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, other)
	if recorder.Code != http.StatusOK {
		t.Fatalf("anonymous poll status=%d, want 200", recorder.Code)
	}
}

// TestServer_AuthSessionGetDoesNotPenaliseNonLoginCredential guards the
// distinction between "this credential is invalid" and "this credential may
// not sign in". Counting the latter as a failed login would let a few page
// loads of a perfectly valid access key block that IP from inference.
func TestServer_AuthSessionGetDoesNotPenaliseNonLoginCredential(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{
		RequiredAPIKeys: []string{"admin-key"},
		Security: config.SecurityConfig{AuthRateLimit: config.AuthRateLimitConfig{
			MaxFailures: 3, Window: time.Minute, BlockFor: time.Minute,
		}},
	}
	s.routes()

	salt, digest, err := auth.GenerateSalted("api-only-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "api-only", Name: "api-only", KeyHash: digest, KeySalt: salt,
		KeySecret: "api-only-secret", Kind: auth.KeyKindManagement,
		Scopes: []string{auth.ScopeInference}, AllowManagementLogin: false,
	}); err != nil {
		t.Fatal(err)
	}
	s.refreshKeyCache()

	// Poll the session endpoint as a client with a valid but non-login key
	// more times than the failure budget allows.
	for attempt := 1; attempt <= 5; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
		request.RemoteAddr = "198.51.100.4:5000"
		request.Header.Set("Authorization", "Bearer api-only-secret")
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("poll %d status=%d, want 200", attempt, recorder.Code)
		}
		var payload struct {
			Authenticated bool `json:"authenticated"`
		}
		if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.Authenticated {
			t.Fatalf("poll %d reported a non-login key as authenticated", attempt)
		}
	}

	// The same caller must still be able to use the credential it holds.
	inference := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	inference.RemoteAddr = "198.51.100.4:5000"
	inference.Header.Set("Authorization", "Bearer api-only-secret")
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, inference)
	if recorder.Code == http.StatusTooManyRequests {
		t.Fatalf("inference was throttled after session polls: status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}

// TestServer_CCSwitchAutoCreatedKeyInheritsGroup pins the usage-group
// inheritance, which is easy to drop silently.
func TestServer_CCSwitchAutoCreatedKeyInheritsGroup(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}}}
	s.routes()

	salt, digest, err := auth.GenerateSalted("grouped-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "grouped", Name: "grouped", KeyHash: digest, KeySalt: salt,
		KeySecret: "grouped-secret", Kind: auth.KeyKindManagement,
		Scopes: []string{auth.ScopeInference}, AllowManagementLogin: true, Group: "team-a",
	}); err != nil {
		t.Fatal(err)
	}
	s.refreshKeyCache()

	request := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"child","model":"allowed","createKey":true}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer grouped-secret")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("auto-create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Link string `json:"link"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(created.Link)
	if err != nil {
		t.Fatal(err)
	}
	record, found, err := s.store.FindAPIKeyBySecret(t.Context(), link.Query().Get("apiKey"))
	if err != nil || !found {
		t.Fatalf("find child found=%v err=%v", found, err)
	}
	if record.Group != "team-a" {
		t.Fatalf("child group=%q, want the parent's team-a", record.Group)
	}
}

// TestServer_DeeplinkKeylessBootstrapCanSignIn pins the keyless path: once the
// generated row is written the deployment is no longer keyless, so the row must
// be able to reach the control plane or the panel is locked out.
func TestServer_DeeplinkKeylessBootstrapCanSignIn(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}}}
	s.routes()

	request := httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", strings.NewReader(`{"app":"codex","name":"bootstrap","model":"allowed","createKey":true}`))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	s.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("keyless auto-create status=%d body=%s", response.Code, response.Body.String())
	}
	var created struct {
		Link  string `json:"link"`
		KeyID string `json:"keyId"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	link, err := url.Parse(created.Link)
	if err != nil {
		t.Fatal(err)
	}
	secret := link.Query().Get("apiKey")
	if secret == "" {
		t.Fatalf("created link carries no key: %s", created.Link)
	}
	record, found, err := s.store.FindAPIKeyBySecret(t.Context(), secret)
	if err != nil || !found {
		t.Fatalf("find bootstrap key found=%v err=%v", found, err)
	}
	if record.Kind != auth.KeyKindManagement || !record.AllowManagementLogin {
		t.Fatalf("bootstrap key kind=%q allowLogin=%v, want a management key that can sign in", record.Kind, record.AllowManagementLogin)
	}
	hasKeysAdmin := false
	for _, scope := range record.Scopes {
		if scope == auth.ScopeKeysAdmin {
			hasKeysAdmin = true
		}
	}
	if !hasKeysAdmin {
		t.Fatalf("bootstrap scopes=%v, want keys-admin so the panel can be repaired", record.Scopes)
	}

	// The generated secret must actually work for a control-plane request.
	s.refreshKeyCache()
	keysRequest := httptest.NewRequest(http.MethodGet, "/api/keys", nil)
	keysRequest.Header.Set("Authorization", "Bearer "+secret)
	keysResponse := httptest.NewRecorder()
	s.ServeHTTP(keysResponse, keysRequest)
	if keysResponse.Code != http.StatusOK {
		t.Fatalf("bootstrap key cannot read /api/keys: status=%d body=%s", keysResponse.Code, keysResponse.Body.String())
	}
}

// TestServer_DeeplinkAnonymousIDIsNotKeylessTarget is the regression guard for
// a sentinel collision: only the legacy keyless identity may take the bootstrap
// path. A stored credential whose id happens to be "anonymous" is a real key
// and must be nested like any other, or a restricted key could mint a full
// management key from here.
func TestServer_DeeplinkAnonymousIDIsNotKeylessTarget(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"allowed": {}}}
	s.routes()

	// A non-legacy identity carrying the sentinel id must not be treated as
	// the keyless state. With no matching store row the helper fails closed,
	// which is the safe outcome; the important part is that it does not fall
	// through to the bootstrap branch.
	record, err := s.generatedDeeplinkKeyRecord(
		httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", nil),
		auth.Identity{ID: "anonymous", Scopes: map[string]struct{}{auth.ScopeInference: {}}},
		"child", "child", []string{"allowed"}, "digest", []byte("salt"), "secret",
	)
	if err == nil {
		t.Fatalf("non-legacy anonymous identity took the bootstrap path: %+v", record)
	}

	// The legacy keyless identity is the one that may bootstrap.
	bootstrap, err := s.generatedDeeplinkKeyRecord(
		httptest.NewRequest(http.MethodPost, "/api/cc-switch/deeplink", nil),
		auth.Identity{ID: "anonymous", Legacy: true},
		"boot", "boot", []string{"allowed"}, "digest", []byte("salt"), "secret",
	)
	if err != nil {
		t.Fatalf("legacy keyless identity rejected: %v", err)
	}
	if bootstrap.Kind == auth.KeyKindAccess {
		t.Fatal("legacy keyless identity was nested instead of bootstrapped")
	}
	if !bootstrap.AllowManagementLogin {
		t.Fatal("bootstrap key cannot sign in to the panel")
	}
}

// TestServer_AuthSessionKeylessIgnoresStaleCookie keeps a keyless deployment
// from locking itself out. With no key configured nothing can authenticate, so
// charging those rejections to the caller would let a stale cookie from an
// earlier keyed deployment block that IP's inference traffic.
func TestServer_AuthSessionKeylessIgnoresStaleCookie(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{
		Security: config.SecurityConfig{AuthRateLimit: config.AuthRateLimitConfig{
			MaxFailures: 2, Window: time.Minute, BlockFor: time.Minute,
		}},
	}
	s.routes()

	for attempt := 1; attempt <= 6; attempt++ {
		request := httptest.NewRequest(http.MethodGet, "/api/auth/session", nil)
		request.RemoteAddr = "198.51.100.4:5000"
		request.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "stale-key"})
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusOK {
			t.Fatalf("poll %d status=%d, want 200 on a keyless deployment", attempt, recorder.Code)
		}
	}

	// The same caller must not be throttled.
	health := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	health.RemoteAddr = "198.51.100.4:5000"
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, health)
	if recorder.Code == http.StatusTooManyRequests {
		t.Fatalf("keyless caller throttled by a stale cookie: %s", recorder.Body.String())
	}
}

// TestServer_SeedStartupAPIKeysRemapsReservedID keeps an existing deployment
// from failing to start. A configuration that already declared the reserved id
// must be remapped to the derived id rather than rejected, and the remap must
// be stable across reloads.
func TestServer_SeedStartupAPIKeysRemapsReservedID(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{StartupAPIKeys: []config.StartupAPIKey{{
		ID: "anonymous", Key: "legacy-reserved-secret",
		Scopes: []string{auth.ScopeInference},
	}}}
	s.routes()

	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatalf("seeding a config with a reserved id failed: %v", err)
	}
	record, found, err := s.store.FindAPIKeyBySecret(t.Context(), "legacy-reserved-secret")
	if err != nil || !found {
		t.Fatalf("seeded key found=%v err=%v", found, err)
	}
	if record.ID == "anonymous" {
		t.Fatal("reserved id was stored verbatim")
	}
	derived := record.ID

	// A reload must resolve to the same row instead of creating a second one.
	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatalf("second seed failed: %v", err)
	}
	keys, err := s.store.ListAPIKeys(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, key := range keys {
		if key.ID == derived {
			count++
		}
	}
	if count != 1 {
		var ids []string
		for _, key := range keys {
			ids = append(ids, key.ID)
		}
		t.Fatalf("derived row appears %d times (ids=%v), want exactly one stable row", count, ids)
	}
	if derived == "" || derived == "anonymous" {
		t.Fatalf("derived id = %q", derived)
	}
}

// TestServer_AuthSessionKeylessPostDoesNotThrottle is the POST half of the
// keyless rule: a submission that cannot possibly succeed must not consume the
// shared failure budget and block inference for that address.
func TestServer_AuthSessionKeylessPostDoesNotThrottle(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{
		Security: config.SecurityConfig{AuthRateLimit: config.AuthRateLimitConfig{
			MaxFailures: 2, Window: time.Minute, BlockFor: time.Minute,
		}},
	}
	s.routes()

	for attempt := 1; attempt <= 5; attempt++ {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/session", strings.NewReader(`{"key":"bogus"}`))
		request.Header.Set("Content-Type", "application/json")
		request.RemoteAddr = "198.51.100.4:5000"
		recorder := httptest.NewRecorder()
		s.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("post %d status=%d, want 401 on a keyless deployment", attempt, recorder.Code)
		}
	}

	health := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	health.RemoteAddr = "198.51.100.4:5000"
	recorder := httptest.NewRecorder()
	s.ServeHTTP(recorder, health)
	if recorder.Code == http.StatusTooManyRequests {
		t.Fatalf("keyless caller throttled after failed logins: %s", recorder.Body.String())
	}
}

// TestServer_LegacyAnonymousRowUpgradeStaysOperable is the upgrade guard for a
// row written by an older build that declared id: anonymous. The row must not
// stop startup and must remain addressable so it can be revoked.
func TestServer_LegacyAnonymousRowUpgradeStaysOperable(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })
	s.cfg = config.Config{
		RequiredAPIKeys: []string{"legacy-secret"},
		Models:          map[string]config.ModelConfig{"allowed": {}},
	}
	s.routes()

	// Simulate the historical row: reserved id, config source, no remap.
	salt, digest, err := auth.GenerateSalted("legacy-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "anonymous", Name: "anonymous", KeyHash: digest, KeySalt: salt,
		KeySecret: "legacy-secret", Kind: auth.KeyKindManagement,
		Scopes: auth.ManagementScopes(), AllowManagementLogin: true,
		Source: store.KeySourceConfig,
	}); err != nil {
		t.Fatalf("historical row could not be stored: %v", err)
	}

	// Lookup, secret resolution and revocation must all still work.
	if _, found, err := s.store.FindAPIKeyByID(t.Context(), "anonymous"); err != nil || !found {
		t.Fatalf("lookup found=%v err=%v", found, err)
	}
	if _, found, err := s.store.GetAPIKeySecret(t.Context(), "anonymous"); err != nil || !found {
		t.Fatalf("secret lookup found=%v err=%v", found, err)
	}
	if err := s.store.RevokeAPIKeyCascade(t.Context(), "anonymous", time.Now()); err != nil {
		t.Fatalf("revoking the historical row failed: %v", err)
	}

	// Seeding a config that declares the reserved id must not fail startup.
	s.cfg.StartupAPIKeys = []config.StartupAPIKey{{
		ID: "anonymous", Key: "legacy-secret", Scopes: []string{auth.ScopeInference},
	}}
	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatalf("seeding after the historical row failed: %v", err)
	}
}

// TestServer_UpgradeFromReservedIDKeepsRecoveryKey walks the full upgrade: a
// historical config-owned row with the reserved id is replaced by the remapped
// row, and the stale one is cleaned up without tripping the lockout guard.
func TestServer_UpgradeFromReservedIDKeepsRecoveryKey(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	// Historical state: one config-owned row using the reserved id and a
	// full-management scope set, as an older build would have written it.
	salt, digest, err := auth.GenerateSalted("legacy-secret")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.store.UpsertAPIKey(t.Context(), store.APIKeyRecord{
		ID: "anonymous", Name: "legacy", KeyHash: digest, KeySalt: salt,
		KeySecret: "legacy-secret", Kind: auth.KeyKindManagement,
		Scopes: auth.ManagementScopes(), AllowManagementLogin: true,
		Source: store.KeySourceConfig,
	}); err != nil {
		t.Fatal(err)
	}

	s.cfg = config.Config{StartupAPIKeys: []config.StartupAPIKey{{
		ID: "anonymous", Key: "legacy-secret", Scopes: auth.ManagementScopes(),
	}}}
	s.routes()
	if err := s.seedStartupAPIKeys(s.cfg); err != nil {
		t.Fatalf("upgrade seeding failed: %v", err)
	}

	keys, err := s.store.ListAPIKeys(t.Context(), true)
	if err != nil {
		t.Fatal(err)
	}
	var remapped, reserved *store.APIKeyRecord
	for index := range keys {
		switch keys[index].ID {
		case "anonymous":
			reserved = &keys[index]
		default:
			if keys[index].Source == store.KeySourceConfig {
				remapped = &keys[index]
			}
		}
	}
	if remapped == nil {
		t.Fatalf("no remapped config row was written: %+v", keys)
	}
	if reserved != nil && reserved.RevokedAt == nil {
		t.Fatalf("stale reserved-id row was left active: %+v", reserved)
	}
	if !remapped.AllowManagementLogin {
		t.Fatal("remapped row cannot sign in; the deployment would be locked out")
	}
}

// TestServer_SeedStartupAPIKeysRejectsDuplicateIDs keeps two configured
// entries from silently collapsing into one row: an Upsert on the same id
// replaces the credential, so the second one would win without any warning.
func TestServer_SeedStartupAPIKeysRejectsDuplicateIDs(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	t.Cleanup(func() { _ = s.Shutdown(0) })

	cases := []struct {
		name string
		keys []config.StartupAPIKey
	}{
		{
			name: "explicit duplicate id",
			keys: []config.StartupAPIKey{
				{ID: "shared", Key: "first"},
				{ID: "shared", Key: "second"},
			},
		},
		{
			name: "remapped id collides with an explicit entry",
			keys: []config.StartupAPIKey{
				{ID: "anonymous", Key: "first"},
				// The remap of the entry above is startup-<hash(first)>; a
				// second entry may declare that same derived id explicitly.
				{ID: "startup-" + auth.Hash("first")[:16], Key: "other"},
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s.cfg = config.Config{StartupAPIKeys: c.keys}
			if err := s.seedStartupAPIKeys(s.cfg); err == nil {
				t.Fatal("duplicate final key id was accepted")
			} else if !strings.Contains(err.Error(), "declared more than once") {
				t.Fatalf("error = %v, want a duplicate-id explanation", err)
			}
		})
	}
}
