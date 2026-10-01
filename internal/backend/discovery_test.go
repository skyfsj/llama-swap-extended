package backend

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBackend_DiscoverAndExplicitPrecedence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"version":"0.8","backend":"vllm"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	d := Discover(context.Background(), srv.URL, nil)
	if d.Version != "0.8" || d.Type != "vllm" || len(d.Models) != 1 || !d.Capabilities["responses"] {
		t.Fatalf("discovery=%+v", d)
	}
	merged := MergeDiscovery(CapabilitySet{"responses": false}, d)
	if merged["responses"] {
		t.Fatal("explicit capability must win")
	}
}

func TestBackend_DiscoverRejectsCredentials(t *testing.T) {
	d := Discover(context.Background(), "http://user:password@example.test", nil)
	if d.Error == "" {
		t.Fatal("expected URL validation error")
	}
}

func TestBackend_DiscoverDoesNotFollowRedirects(t *testing.T) {
	called := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[]}`))
	}))
	defer target.Close()
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/version" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"version":"redirect-test"}`))
			return
		}
		http.Redirect(w, r, target.URL+"/v1/models", http.StatusFound)
	}))
	defer source.Close()

	d := Discover(context.Background(), source.URL, nil)
	if d.Error == "" || !strings.Contains(d.Error, "HTTP 302") {
		t.Fatalf("expected redirect error, got %+v", d)
	}
	if called {
		t.Fatal("discovery followed a redirect to another host")
	}
}

func TestBackend_DiscoverNilContextStillProbes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"version":"test"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[]}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	if got := Discover(testNilContext(), srv.URL, nil); got.Error != "" || got.Version != "test" {
		t.Fatalf("nil-context discovery = %+v", got)
	}
}

func TestBackend_DiscoverReadsOptionalSafeServerInfo(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"version":"0.8.5","backend":"vllm"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"data":[{"id":"model-a"}]}`))
		case "/server_info":
			if r.URL.Query().Get("config_format") != "json" {
				t.Fatalf("server_info query = %q", r.URL.RawQuery)
			}
			_, _ = w.Write([]byte(`{
				"vllm_config":{"dtype":"float16","max_model_len":8192,"capabilities":{"responses":false,"chat":true}},
				"vllm_env":{"VLLM_TARGET_DEVICE":"cuda","VLLM_API_KEY":"must-not-leak"},
				"system_env":{"SECRET_TOKEN":"must-not-leak"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discovered := Discover(context.Background(), server.URL, nil)
	if discovered.Error != "" || discovered.Version != "0.8.5" || discovered.Type != "vllm" {
		t.Fatalf("discovery=%+v", discovered)
	}
	if discovered.ServerInfo["dtype"] != "float16" || discovered.ServerInfo["max_model_len"] != "8192" || discovered.ServerInfo["target_device"] != "cuda" {
		t.Fatalf("safe server info=%#v", discovered.ServerInfo)
	}
	if _, leaked := discovered.ServerInfo["api_key"]; leaked {
		t.Fatal("secret-like environment value leaked into server info")
	}
	if _, leaked := discovered.ServerInfo["secret_token"]; leaked {
		t.Fatal("system environment value leaked into server info")
	}
	if discovered.Capabilities["responses"] {
		t.Fatal("server_info explicit false capability was ignored")
	}
}

func TestBackend_DiscoverServerInfoFailureIsFailOpen(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"version":"test"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"models":["model-a"]}`))
		case "/server_info":
			w.WriteHeader(http.StatusUnauthorized)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discovered := Discover(context.Background(), server.URL, nil)
	if discovered.Error != "" || len(discovered.Models) != 1 || discovered.Models[0] != "model-a" {
		t.Fatalf("optional server_info failure was not fail-open: %+v", discovered)
	}
}

func TestBackend_SafeServerInfoNormalizesKeysAndTypedCapabilities(t *testing.T) {
	info, capabilities := safeServerInfo(map[string]any{
		"maxModelLen":  int64(4096),
		"capabilities": map[string]bool{"responses": false, "rerank": true},
	}, map[string]bool{"chat": true})
	if info["max_model_len"] != "4096" {
		t.Fatalf("normalized server info=%#v", info)
	}
	if capabilities["responses"] || !capabilities["rerank"] || !capabilities["chat"] {
		t.Fatalf("typed capabilities=%#v", capabilities)
	}
}

func TestBackend_DiscoverNormalizesCamelCaseEnvelopes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/version":
			_, _ = w.Write([]byte(`{"buildVersion":"0.9.0","backend":"vllm"}`))
		case "/v1/models":
			_, _ = w.Write([]byte(`{"Models":["camel-model"]}`))
		case "/server_info":
			_, _ = w.Write([]byte(`{
				"vllmConfig":{"maxModelLen":16384,"capabilities":["realtime"]},
				"vllmEnv":{"VLLM_TARGET_DEVICE":"rocm"}
			}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	discovered := Discover(context.Background(), server.URL, nil)
	if discovered.Error != "" || discovered.Version != "0.9.0" || discovered.Type != "vllm" {
		t.Fatalf("discovery=%+v", discovered)
	}
	if len(discovered.Models) != 1 || discovered.Models[0] != "camel-model" {
		t.Fatalf("models=%#v", discovered.Models)
	}
	if discovered.ServerInfo["max_model_len"] != "16384" || discovered.ServerInfo["target_device"] != "rocm" || !discovered.Capabilities["realtime"] {
		t.Fatalf("normalized server info/capabilities: info=%#v capabilities=%#v", discovered.ServerInfo, discovered.Capabilities)
	}
}

func TestBackend_ModelIDsAcceptsTypedSlices(t *testing.T) {
	if got := modelIDs(map[string]any{
		"data": []map[string]any{{"id": "typed-data"}},
	}); len(got) != 1 || got[0] != "typed-data" {
		t.Fatalf("typed data model ids = %#v", got)
	}
	got := modelIDs(map[string]any{"data": []string{"typed-string"}})
	if len(got) != 1 || got[0] != "typed-string" {
		t.Fatalf("typed string data model ids = %#v", got)
	}
	got = modelIDs(map[string]any{
		"models": []string{"typed-model", ""},
	})
	if len(got) != 1 || got[0] != "typed-model" {
		t.Fatalf("typed models model ids = %#v", got)
	}
	got = modelIDs(map[string]any{
		"models": []map[string]any{{"id": "object-model"}},
	})
	if len(got) != 1 || got[0] != "object-model" {
		t.Fatalf("typed object model ids = %#v", got)
	}
}

func TestBackend_DiscoverySanitizesRemoteStrings(t *testing.T) {
	if got := stringField(map[string]any{"version": " 0.9 "}, "version"); got != "0.9" {
		t.Fatalf("trimmed version = %q", got)
	}
	for _, value := range []string{"model\u200b", "model\u202e", "model\u00a0", "model\n", strings.Repeat("x", maxDiscoveryString+1)} {
		if got := stringField(map[string]any{"id": value}, "id"); got != "" {
			t.Errorf("unsafe discovery string %q returned %q", value, got)
		}
	}
	if got := modelIDs(map[string]any{"data": []string{"ok", "bad\u200b", strings.Repeat("x", maxDiscoveryString+1)}}); len(got) != 1 || got[0] != "ok" {
		t.Fatalf("sanitized model ids = %#v", got)
	}
}

func TestBackend_DiscoveryCapabilityDeclarationsAreBoundedAndSanitized(t *testing.T) {
	declared := make(map[string]any, maxDiscoveredCaps+3)
	for i := 0; i < maxDiscoveredCaps+3; i++ {
		declared[fmt.Sprintf("cap-%04d", i)] = true
	}
	declared["bad\u200bcap"] = true
	capabilities := mergeDeclaredCapabilities(nil, declared)
	if len(capabilities) != maxDiscoveredCaps {
		t.Fatalf("capability count = %d, want <= %d", len(capabilities), maxDiscoveredCaps)
	}
	if _, ok := capabilities["bad\u200bcap"]; ok {
		t.Fatal("invisible capability key was retained")
	}
}

func TestBackend_ProbeJSONRejectsOversizedResponse(t *testing.T) {
	payload := fmt.Sprintf(`{"data":[{"id":"%s"}]}`, strings.Repeat("x", maxDiscoveryBody))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, payload)
	}))
	defer server.Close()
	if _, err := probeJSON(context.Background(), server.Client(), server.URL); err == nil || !strings.Contains(err.Error(), "too large") {
		t.Fatalf("oversized discovery response error = %v", err)
	}
}

func testNilContext() context.Context { return nil }
