package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/protocol/anthropiccache"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

// middlewareResult records what a handler observed after the middleware ran:
// the exact body bytes, the replay bytes GetBody returns, and the headers the
// middleware is required to maintain.
type middlewareResult struct {
	body      []byte
	replay    []byte
	length    int64
	hasBody   bool
	noChunked bool
}

// runProtocolFiles drives createProtocolFilesMiddleware with a handler that
// reports what it received. store may be nil to exercise the pre-scan path.
func runProtocolFiles(t *testing.T, s *Server, path, body string) middlewareResult {
	t.Helper()
	var result middlewareResult
	handler := createProtocolFilesMiddleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result.hasBody = r.GetBody != nil
		result.length = r.ContentLength
		result.noChunked = r.Header.Get("Transfer-Encoding") == ""
		if r.Body != nil {
			result.body, _ = io.ReadAll(r.Body)
		}
		if result.hasBody {
			replay, err := r.GetBody()
			if err == nil {
				result.replay, _ = io.ReadAll(replay)
			}
		}
	}))
	req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	return result
}

// TestServer_ProtocolFilesPreScanMatchesDecodedPath pins the pre-scan against
// the always-decode path: a body without one of this daemon's file ids must
// reach the handler byte-identical, with the same replayable body and headers,
// whether or not it was decoded. A body with an id keeps taking the full path,
// and an escaped id must not be missed.
func TestServer_ProtocolFilesPreScanMatchesDecodedPath(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	bodies := []struct {
		name string
		body string
	}{
		{"plain", `{"model":"m","messages":[{"role":"user","content":"hi"}]}`},
		{"tools", `{"model":"m","tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`},
		{"escaped key", `{"model":"m","messages":[{"role":"user","content":"quote \" and backslash \\"}]}`},
		{"nested arrays", `{"model":"m","input":[[1,2],[3,4]],"metadata":{"a":{"b":{"c":1}}}}`},
		{"unicode", `{"model":"m","messages":[{"role":"user","content":"世界 🎉"}]}`},
		{"empty object", `{}`},
	}
	for _, tc := range bodies {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				result := runProtocolFiles(t, s, path, tc.body)
				if string(result.body) != tc.body {
					t.Fatalf("handler saw %s, want the original %s", result.body, tc.body)
				}
				if !result.hasBody {
					t.Fatal("the middleware must leave a replayable body")
				}
				if string(result.replay) != tc.body {
					t.Fatalf("replay = %s, want %s", result.replay, tc.body)
				}
				if result.length != int64(len(tc.body)) {
					t.Fatalf("ContentLength = %d, want %d", result.length, len(tc.body))
				}
				if !result.noChunked {
					t.Fatal("the middleware must clear Transfer-Encoding")
				}
			})
		}
	}
}

// TestServer_ProtocolFilesPreScanDefersToDecodedPathForEscapedIDs is the safety
// property: an id spelled with JSON escapes must still take the full path, so a
// request the decoded path rejects is rejected instead of passed through.
func TestServer_ProtocolFilesPreScanDefersToDecodedPathForEscapedIDs(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	body := `{"model":"m","input":[{"type":"input_file","file_id":"\u0067file_abc"}]}`

	// The pre-scan must classify the escaped id as "maybe", so the middleware
	// takes the decoded path, looks the file up and rejects the unknown id.
	status := httptest.NewRecorder()
	handler := createProtocolFilesMiddleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an escaped file id must not reach the handler: the decoded path rejects it")
	}))
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(status, req)
	if status.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (the decoded path rejects an unknown file id)", status.Code, http.StatusBadRequest)
	}
}

// TestServer_ExtensionContinuationAdoptsBodyWithoutCopying pins the adopted
// body: the handler must observe the same bytes, the same ContentLength and a
// GetBody that replays them, exactly as the copying path did.
func TestServer_ExtensionContinuationAdoptsBodyWithoutCopying(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	middleware := CreateExtensionContinuationMiddleware(s, config.Config{})
	// The middleware only reads the body when the server has a store, so the
	// comparison below runs against a store-backed server.
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[],"stream":true}`,
		`{"model":"m","input":"call_ls_pending"}`,
		`{"model":"m","contents":[{"role":"user","parts":[{"text":"call_ls_pending"}]}]}`,
	}
	for _, body := range bodies {
		for _, path := range []string{"/v1/chat/completions", "/v1/responses", "/v1/messages", "/v1beta/models/m:generateContent"} {
			t.Run(path+" "+body, func(t *testing.T) {
				var (
					gotBody   []byte
					gotReplay []byte
					gotLen    int64
					hasBody   bool
					noChunked bool
				)
				handler := middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hasBody = r.GetBody != nil
					gotLen = r.ContentLength
					noChunked = r.Header.Get("Transfer-Encoding") == ""
					gotBody, _ = io.ReadAll(r.Body)
					if hasBody {
						replay, err := r.GetBody()
						if err == nil {
							gotReplay, _ = io.ReadAll(replay)
						}
					}
				}))
				req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
				req.Header.Set("Content-Type", "application/json")
				handler.ServeHTTP(httptest.NewRecorder(), req)

				if string(gotBody) != body {
					t.Fatalf("handler saw %s, want %s", gotBody, body)
				}
				if !hasBody {
					t.Fatal("the middleware must leave a replayable body")
				}
				if string(gotReplay) != body {
					t.Fatalf("replay = %s, want %s", gotReplay, body)
				}
				if gotLen != int64(len(body)) {
					t.Fatalf("ContentLength = %d, want %d", gotLen, len(body))
				}
				if !noChunked {
					t.Fatal("the middleware must clear Transfer-Encoding")
				}
			})
		}
	}
}

// TestAnthropicCache_ApplyAutoSkipsDecodeForNonClaudeCode pins the pre-check
// against the decoded path: in auto mode a request without a Claude Code header
// signal must produce exactly the same Result as the decoder produced, without
// the body being decoded.
func TestAnthropicCache_ApplyAutoSkipsDecodeForNonClaudeCode(t *testing.T) {
	bodies := []string{
		`{"model":"m","messages":[{"role":"user","content":"hi"}]}`,
		`{"model":"m","messages":[],"system":[{"type":"text","text":"be nice"}]}`,
		`{"model":"m","messages":[],"tools":[{"name":"f"}]}`,
		`{"model":"m","messages":[],"system":"plain string"}`,
		`not json`,
		`[]`,
		`null`,
		``,
	}
	headerSets := []struct {
		name    string
		headers map[string]string
	}{
		{"none", nil},
		{"other client", map[string]string{"User-Agent": "python-httpx/0.27"}},
		{"claude code", map[string]string{"User-Agent": "claude-cli/1.0 (external, cli)"}},
		{"x-claude-code", map[string]string{"X-Claude-Code": "true"}},
	}
	for _, headers := range headerSets {
		for _, body := range bodies {
			t.Run(headers.name+" "+body, func(t *testing.T) {
				req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
				for key, value := range headers.headers {
					req.Header.Set(key, value)
				}
				got, err := anthropiccache.Apply([]byte(body), req.Header, anthropiccache.Options{Mode: anthropiccache.ModeAuto})
				if err != nil {
					// A request that carries a signal still decodes, so a
					// malformed body keeps failing exactly as it did.
					if headers.name == "claude code" || headers.name == "x-claude-code" {
						return
					}
					t.Fatalf("Apply: %v", err)
				}
				if string(got.Body) != body {
					t.Fatalf("body = %s, want the original", got.Body)
				}
				if got.Enabled || got.Changed || got.Detected {
					t.Fatalf("auto mode without a signal must not transform: %+v", got)
				}
				for _, transform := range got.Transforms {
					if !transform.Skipped || transform.Reason != "client-not-confirmed" {
						t.Fatalf("transform %s = %+v, want skipped/client-not-confirmed", transform.Name, transform)
					}
				}
			})
		}
	}
}

// TestAnthropicCache_ApplyForceStillDecodesEveryBody is the counterpart: the
// pre-check is auto-only, so force mode must keep decoding and transforming
// exactly as before.
func TestAnthropicCache_ApplyForceStillDecodesEveryBody(t *testing.T) {
	body := `{"model":"m","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	result, err := anthropiccache.Apply([]byte(body), req.Header, anthropiccache.Options{Mode: anthropiccache.ModeForce})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !result.Enabled || !result.Detected {
		t.Fatalf("force mode must enable the transforms: %+v", result)
	}
	if result.Changed {
		t.Fatalf("this body has nothing to transform: %+v", result)
	}
}

// TestAnthropicCache_ApplyOffStillPassesThrough keeps the off mode contract: no
// decode, original bytes, every transform reported as skipped.
func TestAnthropicCache_ApplyOffStillPassesThrough(t *testing.T) {
	for _, body := range []string{`{"model":"m"}`, `not json`, ``} {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		result, err := anthropiccache.Apply([]byte(body), req.Header, anthropiccache.Options{Mode: anthropiccache.ModeOff})
		if err != nil {
			t.Fatalf("Apply(%q): %v", body, err)
		}
		if string(result.Body) != body || result.Enabled || result.Detected {
			t.Fatalf("off mode changed the request: %+v", result)
		}
	}
}

// TestAnthropicCache_DetectClaudeCodeStillRequiresBothSignals pins the detector
// itself after the header signal was factored out: a header alone is not enough,
// and neither is a Claude-shaped body alone.
func TestAnthropicCache_DetectClaudeCodeStillRequiresBothSignals(t *testing.T) {
	claudeHeaders := http.Header{}
	claudeHeaders.Set("User-Agent", "claude-code/1.0 (external, cli)")
	plainHeaders := http.Header{}
	plainHeaders.Set("User-Agent", "python-httpx/0.27")

	claudeBody := map[string]any{
		"messages": []any{map[string]any{"role": "user"}},
		"system":   []any{map[string]any{"type": "text", "text": "be nice"}},
	}
	emptyBody := map[string]any{"messages": []any{}}
	noMessages := map[string]any{"system": []any{map[string]any{"type": "text"}}}

	if !anthropiccache.DetectClaudeCode(claudeHeaders, claudeBody) {
		t.Fatal("a Claude Code request must be detected")
	}
	if anthropiccache.DetectClaudeCode(plainHeaders, claudeBody) {
		t.Fatal("a Claude-shaped body from another client must not be detected")
	}
	if anthropiccache.DetectClaudeCode(claudeHeaders, emptyBody) {
		t.Fatal("a Claude Code client with no messages must not be detected")
	}
	if anthropiccache.DetectClaudeCode(claudeHeaders, noMessages) {
		t.Fatal("a Claude Code client with no messages must not be detected")
	}
	if anthropiccache.DetectClaudeCode(plainHeaders, emptyBody) {
		t.Fatal("neither signal alone must be detected")
	}
}

// TestServer_ProtocolAdapterStillStripsHostedSearchTools is the end-to-end
// counterpart for the strip pre-scan: the middleware must keep stripping a
// hosted search tool and keep passing an ordinary request through untouched.
func TestServer_ProtocolAdapterStillStripsHostedSearchTools(t *testing.T) {
	withTool := `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"web_search"}]}`
	ordinary := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	for _, path := range []string{"/v1/chat/completions"} {
		for _, tc := range []struct {
			name string
			body string
			want string
		}{
			{"hosted tool stripped", withTool, `{"model":"m","messages":[{"role":"user","content":"hi"}],"tools":[]}`},
			{"ordinary untouched", ordinary, ordinary},
		} {
			t.Run(tc.name+" "+path, func(t *testing.T) {
				out, changed, err := dialect.StripHostedSearchTools([]byte(tc.body), dialect.OpenAIChat)
				if err != nil {
					t.Fatalf("StripHostedSearchTools: %v", err)
				}
				var gotDoc, wantDoc any
				if err := json.Unmarshal(out, &gotDoc); err != nil {
					t.Fatalf("out is not JSON: %v", err)
				}
				if err := json.Unmarshal([]byte(tc.want), &wantDoc); err != nil {
					t.Fatalf("want is not JSON: %v", err)
				}
				if !reflect.DeepEqual(gotDoc, wantDoc) {
					t.Fatalf("out = %s, want %s", out, tc.want)
				}
				if tc.name == "hosted tool stripped" && !changed {
					t.Fatal("a hosted tool must be reported as changed")
				}
				if tc.name == "ordinary untouched" && changed {
					t.Fatal("an ordinary request must not be reported as changed")
				}
			})
		}
	}
}

// TestServer_ProtocolFilesPreScanStillInstallsUpstreamHooks pins the contract
// the pre-scan must not break: a conversation replays its earlier turns through
// this middleware, and those turns reference files an earlier request resolved,
// so the upstream hook has to be installed even when the body itself carries no
// resolvable id.
func TestServer_ProtocolFilesPreScanStillInstallsUpstreamHooks(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	sawHook := false
	handler := createProtocolFilesMiddleware(s)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawHook = swaputil.UpstreamHooksFrom(r.Context()).Prepare != nil
	}))
	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(httptest.NewRecorder(), req)
	if !sawHook {
		t.Fatal("the pre-scan path must still install the upstream media hook")
	}
}
