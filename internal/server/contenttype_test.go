package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_ContentTypeMiddleware(t *testing.T) {
	handler := createContentTypeMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("X-Seen-Content-Type", r.Header.Get("Content-Type"))
		w.Header().Set("X-Seen-Body", string(body))
		w.WriteHeader(http.StatusOK)
	}))

	t.Run("json body without a content type is relabelled", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		r.Header.Del("Content-Type")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Seen-Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
		if got := w.Header().Get("X-Seen-Body"); got != `{"model":"m"}` {
			t.Errorf("body = %q, want the original body", got)
		}
	})

	t.Run("curl default form content type with a json body is relabelled", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Seen-Content-Type"); got != "application/json" {
			t.Errorf("Content-Type = %q, want application/json", got)
		}
	})

	t.Run("declared json is not read", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`not json`))
		r.Header.Set("Content-Type", "application/json; charset=utf-8")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want the request untouched", w.Code)
		}
		if got := w.Header().Get("X-Seen-Body"); got != `not json` {
			t.Errorf("body = %q", got)
		}
	})

	t.Run("form-encoded body with model is left alone", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`model=m&prompt=hi`))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d body = %s", w.Code, w.Body.String())
		}
		if got := w.Header().Get("X-Seen-Content-Type"); got != "application/x-www-form-urlencoded" {
			t.Errorf("Content-Type = %q, want it unchanged", got)
		}
	})

	t.Run("multipart is never sniffed", func(t *testing.T) {
		body := "--b\r\nContent-Disposition: form-data; name=\"model\"\r\n\r\nm\r\n--b--\r\n"
		r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(body))
		r.Header.Set("Content-Type", `multipart/form-data; boundary=b`)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want the multipart request untouched", w.Code)
		}
	})

	t.Run("invalid json is rejected with 400", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":`))
		r.Header.Del("Content-Type")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("empty body is rejected with 400", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		r.Header.Del("Content-Type")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", w.Code)
		}
	})

	t.Run("non-body methods pass through", func(t *testing.T) {
		r := httptest.NewRequest(http.MethodGet, "/v1/audio/voices?model=m", nil)
		r.Header.Del("Content-Type")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	})

	t.Run("body under the limit passes", func(t *testing.T) {
		// A 1 MiB body is far below swaputil.MaxRequestBodySize; it must be
		// forwarded with its bytes intact.
		underLimit := `{"model":"m","padding":"` + strings.Repeat("x", 1<<20) + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(underLimit))
		r.Header.Del("Content-Type")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200 for a body under the limit", w.Code)
		}
		if got := w.Header().Get("X-Seen-Body"); got != underLimit {
			t.Errorf("body was altered: got %d bytes, want %d", len(got), len(underLimit))
		}
	})

	t.Run("body over the limit is 413", func(t *testing.T) {
		if testing.Short() {
			t.Skip("allocates a body larger than swaputil.MaxRequestBodySize")
		}
		overLimit := `{"model":"m","padding":"` + strings.Repeat("x", swaputil.MaxRequestBodySize) + `"}`
		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(overLimit))
		r.Header.Del("Content-Type")
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusRequestEntityTooLarge {
			t.Fatalf("status = %d, want 413", w.Code)
		}
	})
}

func TestServer_ContentTypeMiddleware_FormRoute(t *testing.T) {
	handler := createContentTypeMiddleware()(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	r := httptest.NewRequest(http.MethodPost, "/v1/audio/transcriptions", strings.NewReader(`{"model":"m"}`))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	// A JSON body on a multipart endpoint cannot be routed by the form path.
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", w.Code)
	}
}
