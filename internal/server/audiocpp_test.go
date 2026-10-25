package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func TestServer_HandleAudioCpp(t *testing.T) {
	local := newStubRouter([]string{config.AudioCppModelID}, "")
	var gotPath, gotQuery string
	var gotContext swaputil.ReqContextData
	local.serveHTTP = func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.EscapedPath()
		gotQuery = r.URL.RawQuery
		gotContext, _ = swaputil.ReadContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}
	// The pass-through refuses non-root paths until the model is ready.
	local.running = map[string]process.ProcessState{config.AudioCppModelID: process.StateReady}

	s := newTestServerWithConfig(
		config.Config{Models: map[string]config.ModelConfig{config.AudioCppModelID: {}}},
		local,
		newStubRouter(nil, ""),
	)

	t.Run("redirects bare path", func(t *testing.T) {
		for _, tt := range []struct {
			method string
			status int
		}{
			{http.MethodGet, http.StatusMovedPermanently},
			{http.MethodHead, http.StatusMovedPermanently},
			{http.MethodPost, http.StatusPermanentRedirect},
		} {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(tt.method, "/audiocpp?token=value", nil))
			if w.Code != tt.status {
				t.Errorf("%s status=%d want %d", tt.method, w.Code, tt.status)
			}
			if got := w.Header().Get("Location"); got != "audiocpp/?token=value" {
				t.Errorf("%s Location=%q want audiocpp/?token=value", tt.method, got)
			}
		}
	})

	t.Run("strips prefix and binds the fixed model", func(t *testing.T) {
		for _, tt := range []struct {
			method    string
			target    string
			wantPath  string
			wantQuery string
		}{
			{http.MethodGet, "/audiocpp/?token=value", "/", "token=value"},
			{http.MethodGet, "/audiocpp/assets/app.js", "/assets/app.js", ""},
			{http.MethodPost, "/audiocpp/v1/tasks/run", "/v1/tasks/run", ""},
			{http.MethodGet, "/audiocpp/api/models%2Fa.gguf", "/api/models%2Fa.gguf", ""},
		} {
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(tt.method, tt.target, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("%s status=%d want 200 body=%s", tt.target, w.Code, w.Body.String())
			}
			if gotPath != tt.wantPath || gotQuery != tt.wantQuery {
				t.Errorf("%s path=%q query=%q want path=%q query=%q",
					tt.target, gotPath, gotQuery, tt.wantPath, tt.wantQuery)
			}
			if gotContext.Model != config.AudioCppModelID || gotContext.ModelID != config.AudioCppModelID {
				t.Errorf("%s context=%+v want model %s", tt.target, gotContext, config.AudioCppModelID)
			}
		}
	})

	t.Run("non-root paths require a loaded model", func(t *testing.T) {
		stopped := newStubRouter([]string{config.AudioCppModelID}, "")
		noStart := newTestServerWithConfig(
			config.Config{Models: map[string]config.ModelConfig{config.AudioCppModelID: {}}},
			stopped,
			newStubRouter(nil, ""),
		)
		// A root request may start the model; an asset request must not.
		w := httptest.NewRecorder()
		noStart.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/audiocpp/assets/app.js", nil))
		if w.Code != http.StatusConflict {
			t.Fatalf("assets status = %d, want 409", w.Code)
		}
	})
}

func TestServer_HandleAudioCpp_RequiresExactLocalModel(t *testing.T) {
	for _, tt := range []struct {
		name  string
		local *stubRouter
		peer  *stubRouter
		cfg   map[string]config.ModelConfig
	}{
		{"not configured", newStubRouter(nil, ""), newStubRouter(nil, ""), nil},
		{"only on a peer", newStubRouter(nil, ""), newStubRouter([]string{config.AudioCppModelID}, "peer"), nil},
		{"configured but not local", newStubRouter(nil, ""), newStubRouter(nil, ""),
			map[string]config.ModelConfig{config.AudioCppModelID: {}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := newTestServerWithConfig(config.Config{Models: tt.cfg}, tt.local, tt.peer)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/audiocpp/", nil))
			if w.Code != http.StatusNotFound {
				t.Errorf("status=%d want 404", w.Code)
			}
		})
	}
}

func TestServer_ConfiguredVoicesOverrideUpstream(t *testing.T) {
	cfg := config.Config{Models: map[string]config.ModelConfig{
		"tts":      {Capabilities: config.ModelCapConfig{Voices: []string{"voicea", "voiceb", "voicec"}}},
		"tts-none": {},
	}}
	local := newStubRouter([]string{"tts", "tts-none"}, "upstream")
	s := newTestServerWithConfig(cfg, local, newStubRouter(nil, ""))

	t.Run("configured voices are returned", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/audio/voices?model=tts", nil))

		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
		if ct := w.Header().Get("Content-Type"); ct != "application/json" {
			t.Errorf("Content-Type = %q", ct)
		}
		var got struct {
			Voices []string `json:"voices"`
			Model  string   `json:"model"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if len(got.Voices) != 3 || got.Voices[0] != "voicea" || got.Model != "tts" {
			t.Errorf("unexpected body: %s", w.Body.String())
		}
	})

	t.Run("model without voices goes to upstream", func(t *testing.T) {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/audio/voices?model=tts-none", nil))

		if w.Body.String() != "upstream" {
			t.Errorf("body = %q, want upstream", w.Body.String())
		}
	})
}
