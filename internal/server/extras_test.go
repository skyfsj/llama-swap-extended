package server

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"time"
)

func TestServer_DecompressBody(t *testing.T) {
	plain := []byte("hello world")

	var gz bytes.Buffer
	gw := gzip.NewWriter(&gz)
	gw.Write(plain)
	gw.Close()

	var fl bytes.Buffer
	fw, _ := flate.NewWriter(&fl, flate.DefaultCompression)
	fw.Write(plain)
	fw.Close()

	cases := []struct {
		name     string
		body     []byte
		encoding string
	}{
		{"plain", plain, ""},
		{"gzip", gz.Bytes(), "gzip"},
		{"deflate", fl.Bytes(), "deflate"},
		{"unknown passthrough", plain, "br"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, err := decompressBody(c.body, c.encoding)
			if err != nil {
				t.Fatalf("decompressBody: %v", err)
			}
			if !bytes.Equal(got, plain) {
				t.Errorf("got %q, want %q", got, plain)
			}
		})
	}
}

func TestServer_DecompressBodyRejectsExpansionBeyondRequestLimit(t *testing.T) {
	plain := bytes.Repeat([]byte{'x'}, swaputil.MaxRequestBodySize+1)
	var compressed bytes.Buffer
	writer := gzip.NewWriter(&compressed)
	if _, err := writer.Write(plain); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := decompressBody(compressed.Bytes(), "gzip"); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized decompressed body error = %v", err)
	}
}

func TestServer_FilterAcceptEncoding(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"gzip, deflate, br", "gzip, deflate"},
		{"br, zstd", ""},
		{"gzip;q=1.0", "gzip;q=1.0"},
	}
	for _, c := range cases {
		if got := filterAcceptEncoding(c.in); got != c.want {
			t.Errorf("filterAcceptEncoding(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestServer_BodyCopier_Flush(t *testing.T) {
	bc := newBodyCopier(httptest.NewRecorder())
	bc.Write([]byte("data"))
	bc.Flush()
	if bc.Status() != http.StatusOK {
		t.Errorf("status = %d, want 200", bc.Status())
	}
}

// hijackRecorder is an httptest.ResponseRecorder that also implements
// http.Hijacker, returning a pipe so Hijack forwarding can be exercised.
type hijackRecorder struct {
	*httptest.ResponseRecorder
	conn net.Conn
}

func (h *hijackRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return h.conn, bufio.NewReadWriter(bufio.NewReader(h.conn), bufio.NewWriter(h.conn)), nil
}

func TestServer_BodyCopier_Hijack(t *testing.T) {
	t.Run("forwards to underlying hijacker", func(t *testing.T) {
		client, server := net.Pipe()
		defer client.Close()
		defer server.Close()

		bc := newBodyCopier(&hijackRecorder{httptest.NewRecorder(), server})
		conn, _, err := bc.Hijack()
		if err != nil {
			t.Fatalf("Hijack: %v", err)
		}
		if conn != server {
			t.Errorf("Hijack returned unexpected conn")
		}
	})

	t.Run("errors when underlying writer is not a hijacker", func(t *testing.T) {
		bc := newBodyCopier(httptest.NewRecorder())
		if _, _, err := bc.Hijack(); err == nil {
			t.Error("expected error hijacking a non-Hijacker ResponseWriter")
		}
	})
}

func TestServer_BodyCopier_SkipsBufferingOnUpgrade(t *testing.T) {
	rec := httptest.NewRecorder()
	bc := newBodyCopier(rec)
	bc.WriteHeader(http.StatusSwitchingProtocols)
	bc.Write([]byte("websocket frame bytes"))

	if bc.body.Len() != 0 {
		t.Errorf("upgrade body buffered = %q, want empty", bc.body.Bytes())
	}
	if got := rec.Body.String(); got != "websocket frame bytes" {
		t.Errorf("client body = %q, want %q", got, "websocket frame bytes")
	}
}

func TestServer_HeaderMapAndRedact(t *testing.T) {
	h := http.Header{
		"Content-Type":  {"application/json"},
		"Authorization": {"Bearer secret"},
		"X-Api-Key":     {"key123"},
	}
	m := headerMap(h)
	if m["Content-Type"] != "application/json" {
		t.Errorf("Content-Type = %q", m["Content-Type"])
	}

	redactHeaders(m)
	if m["Authorization"] != "[REDACTED]" || m["X-Api-Key"] != "[REDACTED]" {
		t.Errorf("sensitive headers not redacted: %v", m)
	}
	if m["Content-Type"] != "application/json" {
		t.Error("non-sensitive header should not be redacted")
	}
}

func TestServer_StripVersionPrefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/v/v1/chat", nil)
	stripVersionPrefix(r)
	if r.URL.Path != "/v1/chat" {
		t.Errorf("path = %q, want /v1/chat", r.URL.Path)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/v1/chat", nil)
	stripVersionPrefix(r2)
	if r2.URL.Path != "/v1/chat" {
		t.Errorf("path = %q, want unchanged", r2.URL.Path)
	}
}

func TestServer_StripAudioAPIPrefix(t *testing.T) {
	r := httptest.NewRequest(http.MethodGet, "/audioapi/v1/tasks/run", nil)
	stripAudioAPIPrefix(r)
	if r.URL.Path != "/v1/tasks/run" {
		t.Errorf("path = %q, want /v1/tasks/run", r.URL.Path)
	}

	r2 := httptest.NewRequest(http.MethodGet, "/v1/tasks/run", nil)
	stripAudioAPIPrefix(r2)
	if r2.URL.Path != "/v1/tasks/run" {
		t.Errorf("path = %q, want unchanged", r2.URL.Path)
	}
}

func TestServer_CloseStreams(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	s.CloseStreams()
	select {
	case <-s.shutdownCtx.Done():
	default:
		t.Error("CloseStreams did not cancel shutdown context")
	}
	s.CloseStreams() // idempotent
}

func TestServer_HandleUIAndFavicon(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))

	for _, path := range []string{"/ui/", "/favicon.ico"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		// Tests build without the `embed_ui` tag, so uiFS is empty and these
		// resolve to 404 — the handlers still execute end to end.
		if w.Code != http.StatusOK && w.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d", path, w.Code)
		}
	}
}

func TestServer_HandleAPIUnloadAll(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))

	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/unload", nil))

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if local.unloadCalls.Load() != 1 {
		t.Errorf("unloadCalls = %d, want 1", local.unloadCalls.Load())
	}
	if len(local.unloadModels) != 0 {
		t.Errorf("unloadModels = %v, want empty for unload all", local.unloadModels)
	}
	if local.unloadTimeout != 0 {
		t.Errorf("unloadTimeout = %v, want 0 (use configured timeouts)", local.unloadTimeout)
	}
}

func TestServer_HandleAPIUnloadModel(t *testing.T) {
	local := newStubRouter([]string{"m1"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{"m1": {}}}

	t.Run("known model", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/unload/m1", nil))
		if w.Code != http.StatusOK {
			t.Errorf("status = %d, want 200", w.Code)
		}
		if len(local.unloadModels) != 1 || local.unloadModels[0] != "m1" {
			t.Errorf("unloadModels = %v, want [m1]", local.unloadModels)
		}
		if local.unloadTimeout != 0 {
			t.Errorf("unloadTimeout = %v, want 0 (use configured timeouts)", local.unloadTimeout)
		}
	})

	t.Run("unknown model 404", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/api/models/unload/nope", nil))
		if w.Code != http.StatusNotFound {
			t.Errorf("status = %d, want 404", w.Code)
		}
	})
}

func TestServer_HandleAPIUnloadModelEnforcesModelRestriction(t *testing.T) {
	local := newStubRouter([]string{"allowed", "denied"}, "")
	s := newTestServer(local, newStubRouter(nil, ""))
	s.cfg = config.Config{Models: map[string]config.ModelConfig{
		"allowed": {},
		"denied":  {},
	}}
	if err := s.store.UpsertAPIKey(context.Background(), store.APIKeyRecord{
		ID: "scoped-unload", KeyHash: store.KeyFingerprint("unload-secret"),
		Scopes: []string{auth.ScopeModelUnload}, Models: []string{"allowed"},
	}); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/models/unload/denied", nil)
	req.Header.Set("Authorization", "Bearer unload-secret")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status=%d body=%q, want 403", w.Code, w.Body.String())
	}
	if local.unloadCalls.Load() != 0 {
		t.Fatalf("unloadCalls=%d, want 0 for denied model", local.unloadCalls.Load())
	}

	req = httptest.NewRequest(http.MethodPost, "/api/models/unload/allowed", nil)
	req.Header.Set("Authorization", "Bearer unload-secret")
	w = httptest.NewRecorder()
	s.ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("allowed status=%d body=%q, want 200", w.Code, w.Body.String())
	}
	if len(local.unloadModels) != 1 || local.unloadModels[0] != "allowed" {
		t.Fatalf("unloadModels=%v, want [allowed]", local.unloadModels)
	}
}

func TestServer_CaptureEndpointRemoved(t *testing.T) {
	s := newTestServer(newStubRouter(nil, ""), newStubRouter(nil, ""))
	w := httptest.NewRecorder()
	s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/captures/42", nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", w.Code)
	}
}

// The request-error incident throttle: one save per (method, path, status)
// signature inside the window, then suppressed saves, then a fresh window
// after the cap reset boundary. This keeps a 4xx flood from turning every
// request into a synchronous fsync cascade.
func TestServer_RequestErrorThrottle(t *testing.T) {
	s := &Server{}
	signature := "GET /models 404"
	now := time.Now()

	if s.requestErrorThrottled(signature, now) {
		t.Fatal("first occurrence must not be throttled")
	}
	if !s.requestErrorThrottled(signature, now.Add(time.Second)) {
		t.Fatal("second occurrence within the window must be throttled")
	}
	// A different signature is throttled independently.
	if s.requestErrorThrottled("GET /other 404", now.Add(time.Second)) {
		t.Fatal("a different signature must not inherit the throttle")
	}
	// After the window the same signature is saved again.
	if s.requestErrorThrottled(signature, now.Add(2*incidentSaveWindow)) {
		t.Fatal("occurrence after the window must not be throttled")
	}
}
