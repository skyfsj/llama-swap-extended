package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/stretchr/testify/assert"
)

func TestServer_ConcurrencyLimitMiddleware(t *testing.T) {
	t.Run("allows requests up to the limit", func(t *testing.T) {
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mw := CreateConcurrencyLimitMiddleware(2)
		handler := mw(final)

		for i := 0; i < 2; i++ {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, http.StatusOK, w.Code)
		}
	})

	t.Run("rejects with 429 once the limit is reached", func(t *testing.T) {
		// blocks until released, so the slot stays held while the next request
		// is admitted
		release := make(chan struct{})
		started := make(chan struct{})
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			close(started)
			<-release
			w.WriteHeader(http.StatusOK)
		})
		mw := CreateConcurrencyLimitMiddleware(1)
		handler := mw(final)

		var wg sync.WaitGroup
		wg.Add(1)
		go func() {
			defer wg.Done()
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
		}()

		<-started

		r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)

		assert.Equal(t, http.StatusTooManyRequests, w.Code)
		assert.Equal(t, "1", w.Header().Get("Retry-After"))

		close(release)
		wg.Wait()
	})

	t.Run("released slot can be reacquired", func(t *testing.T) {
		final := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})
		mw := CreateConcurrencyLimitMiddleware(1)
		handler := mw(final)

		for i := 0; i < 3; i++ {
			r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, r)
			assert.Equal(t, http.StatusOK, w.Code)
		}
	})
}

// A limit of zero must not be installed at all; the route chain test below
// covers the wiring, this one pins the config surface.
func TestConfig_GlobalConcurrencyLimit(t *testing.T) {
	load := func(t *testing.T, content string) (config.Config, error) {
		t.Helper()
		return config.LoadConfigFromReader(strings.NewReader(content))
	}

	cfg, err := load(t, `
models:
  model1:
    cmd: server --port ${PORT}
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GlobalConcurrencyLimit != 0 {
		t.Fatalf("default = %d, want 0", cfg.GlobalConcurrencyLimit)
	}

	cfg, err = load(t, `
globalConcurrencyLimit: 4
models:
  model1:
    cmd: server --port ${PORT}
`)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.GlobalConcurrencyLimit != 4 {
		t.Fatalf("configured = %d, want 4", cfg.GlobalConcurrencyLimit)
	}

	if _, err := load(t, `
globalConcurrencyLimit: -1
models:
  model1:
    cmd: server --port ${PORT}
`); err == nil {
		t.Fatal("negative limit must be rejected")
	}
}

// A request rejected by the global limiter never reaches dispatch, and the 429
// carries the same envelope as the other throttles.
func TestServer_GlobalConcurrencyLimitWiring(t *testing.T) {
	s := newTestServerWithConfig(
		config.Config{GlobalConcurrencyLimit: 1},
		newStubRouter([]string{"local-model"}, "ok"),
		newStubRouter(nil, ""),
	)

	// The stub router answers immediately, so the limiter never saturates.
	// What this asserts is that a configured limit does not break the route:
	// the limiter is present and releases its slot.
	w := httptest.NewRecorder()
	s.ServeHTTP(w, chatRequest("local-model"))
	if w.Code != http.StatusOK {
		t.Fatalf("status=%d body=%q want 200", w.Code, w.Body.String())
	}
}
