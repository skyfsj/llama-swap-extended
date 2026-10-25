package server

import (
	"net/http"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// audio.cpp UI passthrough. audiocpp_server ships its own web UI and model
// management, so it is served under /audiocpp/ and bound to the reserved
// config.AudioCppModelID model. Requests do not need to name a model. Routes
// are registered in server.go.

func handleAudioCppRedirect(w http.ResponseWriter, r *http.Request) {
	// Relative Location so the redirect keeps a reverse proxy's path prefix
	// (see redirectRelative).
	location := "audiocpp/"
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	status := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		status = http.StatusMovedPermanently
	}
	redirectRelative(w, location, status)
}

// handleAudioCpp proxies requests under /audiocpp/ to the fixed local
// audio.cpp model.
func (s *Server) handleAudioCpp(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, config.AudioCppModelID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+config.AudioCppModelID)
		return
	}
	if _, ok := cfg.Models[config.AudioCppModelID]; !ok || !s.local.Handles(config.AudioCppModelID) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "local model "+config.AudioCppModelID+" not found")
		return
	}

	// Strip the /audiocpp prefix before forwarding. URL.Path and PathValue are
	// decoded, so retain the matching escaped suffix in RawPath exactly as the
	// generic /upstream handler does.
	remainingPath := "/" + strings.TrimPrefix(r.PathValue("audioCppPath"), "/")
	escapedRemaining := swaputil.EscapedPathSuffix(r.URL.EscapedPath(), "/audiocpp")
	r.URL.Path = remainingPath
	r.URL.RawPath = escapedRemaining

	// Only an explicit request for the audio.cpp root may start the model, for
	// the same reason as /comfyui: a stale browser tab must not reload it.
	if remainingPath != "/" {
		state, ok := s.local.RunningModels()[config.AudioCppModelID]
		if !ok || state != process.StateReady {
			swaputil.SendResponse(w, r, http.StatusConflict,
				"model "+config.AudioCppModelID+" is not loaded; only /audiocpp/ can start it")
			return
		}
	}

	*r = *r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ApiKey:   swaputil.ExtractAPIKey(r),
		Model:    config.AudioCppModelID,
		ModelID:  config.AudioCppModelID,
		Metadata: make(map[string]string),
	}))
	if err := s.prepareResourceLoadContext(r.Context(), config.AudioCppModelID); err != nil {
		swaputil.SendResponse(w, r, http.StatusInsufficientStorage, err.Error())
		return
	}
	defer s.releaseResourceReservation(config.AudioCppModelID)
	s.local.ServeHTTP(w, r)
}

// serveConfiguredVoices answers GET /v1/audio/voices from the model's
// capabilities.voices when it is set, without starting the model or contacting
// the upstream. It reports whether it wrote the response. The upstream route
// is unreliable for this non-OpenAI endpoint, so an operator can pin the list.
func (s *Server) serveConfiguredVoices(w http.ResponseWriter, r *http.Request, cfg config.Config, modelID string) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/v1/audio/voices" {
		return false
	}
	mc, ok := cfg.Models[modelID]
	if !ok || len(mc.Capabilities.Voices) == 0 {
		return false
	}
	writeJSON(w, map[string]any{
		"voices": mc.Capabilities.Voices,
		"model":  modelID,
	})
	return true
}
