package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// modelRecord is one entry in the OpenAI-compatible /v1/models listing.
type modelRecord struct {
	ID                  string         `json:"id"`
	Object              string         `json:"object"`
	Created             int64          `json:"created"`
	OwnedBy             string         `json:"owned_by"`
	Name                string         `json:"name,omitempty"`
	Description         string         `json:"description,omitempty"`
	Architecture        map[string]any `json:"architecture,omitempty"`
	Capabilities        map[string]any `json:"capabilities,omitempty"`
	SupportedParameters []string       `json:"supported_parameters,omitempty"`
	ContextLength       int            `json:"context_length,omitempty"`
	Meta                map[string]any `json:"meta,omitempty"`
	Status              map[string]any `json:"status"`
}

// cappedMetadataKeys are top-level /v1/models fields produced by the
// capabilities renderer. If a model's metadata block defines any of these
// keys, the renderer's values win and the metadata keys are dropped.
var cappedMetadataKeys = map[string]struct{}{
	"architecture":         {},
	"capabilities":         {},
	"supported_parameters": {},
	"context_length":       {},
}

// renderCapabilities converts a model's capabilities config into additional
// /v1/models fields. Returns zero values when caps.Empty() is true.
func renderCapabilities(caps config.ModelCapConfig) (arch map[string]any, capsMap map[string]any, params []string, ctxLen int) {
	if caps.Empty() {
		return
	}

	hasIn := len(caps.In) > 0
	hasOut := len(caps.Out) > 0

	if hasIn || hasOut {
		arch = make(map[string]any)
	}
	if hasIn {
		arch["input_modalities"] = caps.In
	}
	if hasOut {
		arch["output_modalities"] = caps.Out
	}
	if hasIn && hasOut {
		arch["modality"] = strings.Join(caps.In, "+") + "->" + strings.Join(caps.Out, "+")
	}

	// Build capabilities map only if there's something to put in it.
	if hasIn || hasOut || caps.Tools || caps.Reranker || caps.Translation {
		capsMap = make(map[string]any)
	}

	if hasIn {
		if contains(caps.In, "image") {
			capsMap["vision"] = true
		}
	}
	if hasIn && hasOut {
		if contains(caps.In, "audio") && contains(caps.Out, "text") {
			capsMap["audio_transcriptions"] = true
		}
		if contains(caps.In, "text") && contains(caps.Out, "audio") {
			capsMap["audio_speech"] = true
		}
		if contains(caps.In, "text") && contains(caps.Out, "image") {
			capsMap["image_generation"] = true
		}
		if contains(caps.In, "image") && contains(caps.Out, "image") {
			capsMap["image_to_image"] = true
		}
	}

	if caps.Tools {
		capsMap["function_calling"] = true
		params = []string{"tools", "tool_choice"}
	}

	if caps.Reranker {
		capsMap["reranker"] = true
	}

	if caps.Translation {
		capsMap["translation"] = true
	}

	if caps.Context > 0 {
		ctxLen = caps.Context
	}

	return
}

// contains reports whether s is present in ss.
func contains(ss []string, s string) bool {
	for _, v := range ss {
		if v == s {
			return true
		}
	}
	return false
}

// filterCappedMetadata returns metadata with renderer-owned keys removed.
func filterCappedMetadata(md map[string]any) map[string]any {
	if len(md) == 0 {
		return nil
	}
	filtered := make(map[string]any, len(md))
	for k, v := range md {
		if _, capped := cappedMetadataKeys[k]; !capped {
			filtered[k] = v
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}

// handleListModels serves the OpenAI-compatible model listing: local models
// (with optional aliases) plus peer models.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	created := time.Now().Unix()
	data := make([]modelRecord, 0, len(cfg.Models)+len(cfg.Selectors))
	running := s.local.RunningModels()
	modelIDs := make(map[string]struct{})
	identity := identityFromContext(r.Context())

	modelStatus := func(id string) string {
		if s.modelSleeping(id) {
			return "sleeping"
		}
		// A process that is merely starting (or stopping) is not serving
		// traffic yet: reporting it as "loaded" makes the UI show a ready
		// model while its endpoint still refuses connections.
		switch running[id] {
		case process.StateReady:
			return "loaded"
		case process.StateStarting:
			return "starting"
		case process.StateStopping:
			return "stopping"
		default:
			return "unloaded"
		}
	}

	newRecord := func(
		id, name, description string,
		metadata map[string]any,
		caps config.ModelCapConfig,
		status string,
		internalMetadata map[string]any,
	) modelRecord {
		rec := modelRecord{
			ID:          id,
			Object:      "model",
			Created:     created,
			OwnedBy:     "llama-swap",
			Name:        strings.TrimSpace(name),
			Description: strings.TrimSpace(description),
			Status:      map[string]any{"value": status},
		}
		rec.Architecture, rec.Capabilities, rec.SupportedParameters, rec.ContextLength = renderCapabilities(caps)
		if !caps.Empty() {
			metadata = filterCappedMetadata(metadata)
		}
		llamaSwapMetadata := make(map[string]any, len(metadata)+len(internalMetadata))
		for key, value := range metadata {
			llamaSwapMetadata[key] = value
		}
		for key, value := range internalMetadata {
			llamaSwapMetadata[key] = value
		}
		if len(llamaSwapMetadata) > 0 || rec.ContextLength > 0 {
			rec.Meta = make(map[string]any)
			if len(llamaSwapMetadata) > 0 {
				rec.Meta["llamaswap"] = llamaSwapMetadata
			}
			if rec.ContextLength > 0 {
				rec.Meta["n_ctx"] = rec.ContextLength
			}
		}
		return rec
	}

	for id, mc := range cfg.Models {
		// Model-scoped keys must not learn the existence, aliases, metadata, or
		// lifecycle state of a model outside their allowlist. Check the
		// canonical id once; aliases are emitted only for a visible canonical
		// model and therefore inherit the same authorization decision.
		if !modelAllowedForIdentity(cfg, identity, id) {
			continue
		}
		// Maintenance mode: the model keeps its configuration but is not
		// servable, so it must not enter the listing or the allowlist a key or
		// alias would otherwise resolve to.
		if mc.Disabled {
			continue
		}
		modelIDs[id] = struct{}{}
		for _, alias := range mc.Aliases {
			modelIDs[alias] = struct{}{}
		}

		if mc.Unlisted {
			continue
		}
		status := modelStatus(id)
		internalMetadata := map[string]any{"type": "model"}
		if len(mc.Aliases) > 0 {
			internalMetadata["aliases"] = mc.Aliases
		}
		data = append(data, newRecord(id, mc.Name, mc.Description, mc.Metadata, mc.Capabilities, status, internalMetadata))

		if cfg.IncludeAliasesInList {
			for _, alias := range mc.Aliases {
				if alias := strings.TrimSpace(alias); alias != "" {
					data = append(data, newRecord(
						alias,
						mc.Name,
						mc.Description,
						mc.Metadata,
						mc.Capabilities,
						status,
						map[string]any{"type": "alias", "modelID": id},
					))
				}
			}
		}
	}

	for peerID, peer := range cfg.Peers {
		for _, modelID := range peer.Models {
			fqn := config.PeerModelFQN(peerID, modelID)
			if !modelAllowedForIdentity(cfg, identity, fqn) {
				continue
			}
			modelIDs[fqn] = struct{}{}
			if resolvedPeer, resolvedModel, found := cfg.ResolvePeerModel(modelID); found &&
				resolvedPeer == peerID && resolvedModel == modelID {
				modelIDs[modelID] = struct{}{}
			}
			data = append(data, newRecord(
				fqn,
				peerID+": "+modelID,
				"",
				nil,
				config.ModelCapConfig{},
				"unloaded",
				map[string]any{"type": "peer", "peerID": peerID},
			))
		}
	}

	for selectorID, selector := range cfg.Selectors {
		if !modelAllowedForIdentity(cfg, identity, selectorID) {
			continue
		}
		modelIDs[selectorID] = struct{}{}
		if selector.Unlisted {
			continue
		}
		status := "unloaded"
		visibleTargets := selector.Targets
		if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
			visibleTargets = make([]string, 0, len(selector.Targets))
			for _, target := range selector.Targets {
				if modelAllowedForIdentity(cfg, identity, target) {
					visibleTargets = append(visibleTargets, target)
				}
			}
		}
		for _, target := range visibleTargets {
			modelID, local := cfg.RealModelName(target)
			if local {
				state := running[modelID]
				if state == process.StateReady || state == process.StateStarting {
					status = "loaded"
				}
			}
			if selector.Strategy == config.SelectorStrategyPin || status == "loaded" {
				break
			}
		}
		internalMetadata := map[string]any{
			"type":     "selector",
			"strategy": selector.Strategy,
			"targets":  visibleTargets,
		}
		if selector.Strategy == config.SelectorStrategySpillover {
			internalMetadata["spillover"] = selector.Settings.Spillover
		}
		data = append(data, newRecord(
			selectorID,
			selector.Name,
			selector.Description,
			selector.Metadata,
			config.ModelCapConfig{},
			status,
			internalMetadata,
		))
	}

	if profile, ok := cfg.Profiles[s.ActiveProfile()]; ok {
		for pin, target := range profile.Pins {
			if !modelAllowedForIdentity(cfg, identity, pin) {
				continue
			}
			if target == "" {
				continue
			}
			if _, shadowsModel := modelIDs[pin]; shadowsModel {
				continue
			}
			data = append(data, newRecord(
				pin,
				"",
				"",
				nil,
				config.ModelCapConfig{},
				"unloaded",
				map[string]any{"type": "profile"},
			))
		}
	}

	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })

	// Echo the Origin so browser clients can read the listing.
	if origin := r.Header.Get("Origin"); origin != "" {
		w.Header().Set("Access-Control-Allow-Origin", origin)
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"object": "list",
		"data":   data,
	})
}

// runningModel is one entry in the /running listing.
type runningModel struct {
	Model       string `json:"model"`
	State       string `json:"state"`
	Cmd         string `json:"cmd"`
	Proxy       string `json:"proxy"`
	TTL         int    `json:"ttl"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

// handleUnload stops every running local process. Peer models are remote and
// unaffected.
func (s *Server) handleUnload(w http.ResponseWriter, r *http.Request) {
	if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: unloading all models requires an unrestricted key")
		return
	}
	s.local.Unload(0)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleRunning lists local processes that are not stopped, joining each model
// ID against its config for the cmd/proxy/ttl/name/description metadata.
func (s *Server) handleRunning(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	states := s.local.RunningModels()
	list := make([]runningModel, 0, len(states))
	identity := identityFromContext(r.Context())
	for id, state := range states {
		if !modelAllowedForIdentity(cfg, identity, id) {
			continue
		}
		mc := cfg.Models[id]
		list = append(list, runningModel{
			Model:       id,
			State:       string(state),
			Cmd:         mc.Cmd,
			Proxy:       mc.Proxy,
			TTL:         mc.UnloadAfter,
			Name:        mc.Name,
			Description: mc.Description,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Model < list[j].Model })

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"running": list})
}

// discardResponseWriter satisfies http.ResponseWriter for preload requests,
// dropping the body while capturing the status code.
type discardResponseWriter struct {
	header http.Header
	status int
}

func (d *discardResponseWriter) Header() http.Header {
	if d.header == nil {
		d.header = make(http.Header)
	}
	return d.header
}

func (d *discardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func (d *discardResponseWriter) WriteHeader(status int) { d.status = status }

// startPreload fires a background GET / at every model named in
// Hooks.OnStartup.Preload so they are warm before the first real request.
// Preload names are already resolved to real model IDs by config loading.
func (s *Server) startPreload() {
	models := s.currentConfig().Hooks.OnStartup.Preload
	if len(models) == 0 {
		return
	}
	go func() {
		for _, modelID := range models {
			// Keep each preload admission scoped to one model. The reservation is
			// released before the next preload starts, while the planner continues
			// accounting for a process that remains loaded after ServeHTTP returns.
			func() {
				if !s.local.Handles(modelID) {
					s.proxylog.Warnf("preload: model %s is not a local model, skipping", modelID)
					return
				}
				// Maintenance mode: the preload request would otherwise start a
				// process the operator has taken out of service. The load path
				// itself is gated by the request-context middleware; this covers
				// the startup hook that never passes through it.
				if model, exists := s.currentConfig().Models[modelID]; exists && model.Disabled {
					s.proxylog.Infof("preload: model %s is disabled (maintenance mode), skipping", modelID)
					event.Emit(swaputil.ModelPreloadedEvent{ModelName: modelID, Success: false})
					return
				}
				s.proxylog.Infof("preloading model: %s", modelID)

				if err := s.prepareResourceLoadContext(s.shutdownCtx, modelID); err != nil {
					s.proxylog.Errorf("failed to admit preload model %s: %v", modelID, err)
					event.Emit(swaputil.ModelPreloadedEvent{ModelName: modelID, Success: false})
					return
				}
				defer s.releaseResourceReservation(modelID)

				req, err := http.NewRequestWithContext(s.shutdownCtx, http.MethodGet, "/", nil)
				if err != nil {
					s.proxylog.Errorf("failed to create preload request for model %s: %v", modelID, err)
					event.Emit(swaputil.ModelPreloadedEvent{ModelName: modelID, Success: false})
					return
				}
				req = req.WithContext(swaputil.SetContext(req.Context(), swaputil.ReqContextData{Model: modelID, ModelID: modelID, Metadata: make(map[string]string)}))

				dw := &discardResponseWriter{status: http.StatusOK}
				s.local.ServeHTTP(dw, req)

				success := dw.status < http.StatusBadRequest
				if !success {
					s.proxylog.Errorf("failed to preload model %s: status %d", modelID, dw.status)
				}
				event.Emit(swaputil.ModelPreloadedEvent{ModelName: modelID, Success: success})
			}()
		}
	}()
}

// handleMetrics serves Prometheus-format performance metrics. Returns 503 when
// performance monitoring is disabled.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	if s.perf == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("# performance monitor not available\n"))
		return
	}
	s.perf.MetricsHandler().ServeHTTP(w, r)
	// The perf monitor owns host/GPU gauges. Append the independent control
	// plane families after it so existing scrapers keep their output while
	// runtime/cache telemetry is exposed from the same endpoint.
	s.writeControlMetrics(w)
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

func handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui", http.StatusFound)
}

func handleUpstreamRedirect(w http.ResponseWriter, r *http.Request) {
	http.Redirect(w, r, "/ui/models", http.StatusFound)
}

func handleComfyUIRedirect(w http.ResponseWriter, r *http.Request) {
	location := "/comfyui/"
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	status := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		status = http.StatusMovedPermanently
	}
	http.Redirect(w, r, location, status)
}

// handleComfyUI proxies requests under /comfyui/ to the fixed local
// ComfyUI model. Its compatibility settings are applied while loading config.
func (s *Server) handleComfyUI(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, config.ComfyUIModelID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+config.ComfyUIModelID)
		return
	}
	if _, ok := cfg.Models[config.ComfyUIModelID]; !ok || !s.local.Handles(config.ComfyUIModelID) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "local model "+config.ComfyUIModelID+" not found")
		return
	}

	// Strip the /comfyui prefix before forwarding. URL.Path and PathValue are
	// decoded, so retain the matching escaped suffix in RawPath exactly as the
	// generic /upstream handler does.
	remainingPath := "/" + strings.TrimPrefix(r.PathValue("comfyPath"), "/")
	escapedRemaining := swaputil.EscapedPathSuffix(r.URL.EscapedPath(), "/comfyui")
	r.URL.Path = remainingPath
	r.URL.RawPath = escapedRemaining

	// Only an explicit request for the ComfyUI root may start the model. Once
	// it is unloaded, stale browser requests for assets, APIs, or websockets
	// must not cause it to be loaded again.
	if remainingPath != "/" {
		state, ok := s.local.RunningModels()[config.ComfyUIModelID]
		if !ok || state != process.StateReady {
			swaputil.SendResponse(w, r, http.StatusConflict,
				"model "+config.ComfyUIModelID+" is not loaded; only /comfyui/ can start it")
			return
		}
	}

	*r = *r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{
		ApiKey:   swaputil.ExtractAPIKey(r),
		Model:    config.ComfyUIModelID,
		ModelID:  config.ComfyUIModelID,
		Metadata: make(map[string]string),
	}))
	s.local.ServeHTTP(w, r)
}

// handleUpstream proxies ANY request under /upstream/<model>/<path> directly to
// the model's process, bypassing model dispatch by body/query inspection.
func (s *Server) handleUpstream(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	upstreamPath := r.PathValue("upstreamPath")

	searchName, modelID, remainingPath, found := swaputil.FindModelInPath(cfg, "/"+upstreamPath)
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, modelID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+modelID)
		return
	}

	// Redirect /upstream/model to /upstream/model/ so relative URLs in upstream
	// responses resolve. 301 for GET/HEAD, 308 otherwise to preserve the method.
	if remainingPath == "/" && !strings.HasSuffix(r.URL.Path, "/") {
		newPath := "/upstream/" + searchName + "/"
		if r.URL.RawQuery != "" {
			newPath += "?" + r.URL.RawQuery
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			http.Redirect(w, r, newPath, http.StatusMovedPermanently)
		} else {
			http.Redirect(w, r, newPath, http.StatusPermanentRedirect)
		}
		return
	}

	// Strip the /upstream/<model> prefix before forwarding. URL.Path is decoded,
	// so retain the matching escaped suffix in RawPath for the reverse proxy.
	escapedRemaining := swaputil.EscapedPathSuffix(r.URL.EscapedPath(), "/upstream/"+searchName)
	r.URL.Path = remainingPath
	r.URL.RawPath = escapedRemaining
	// Pin the resolved model so the router skips body/query extraction.
	*r = *r.WithContext(swaputil.SetContext(r.Context(), swaputil.ReqContextData{Model: searchName, ModelID: modelID, Metadata: make(map[string]string)}))
	// This route bypasses the request-context middleware, so publish the
	// resolved model for the outermost request logger here instead: a failed
	// /upstream/ request is archived the same way any other is.
	swaputil.PublishRequestModel(r.Context(), modelID)

	// If the path matches an upstream.ignorePaths entry and the model is
	// not already loaded, refuse the request without triggering a swap. The
	// server was not able to process the response because the model was not
	// already loaded.
	for _, re := range cfg.Upstream.IgnorePaths {
		if !re.MatchString(remainingPath) {
			continue
		}
		if s.local.Handles(modelID) {
			state, ok := s.local.RunningModels()[modelID]
			if !ok || state != process.StateReady {
				swaputil.SendResponse(w, r, http.StatusConflict,
					fmt.Sprintf("model %s is not loaded; path matches upstream.ignorePaths", modelID))
				return
			}
		}
		// Either the model is already loaded (no swap would be triggered)
		// or this is a peer model (peer proxying never swaps). Fall through
		// to normal dispatch.
		break
	}

	switch {
	case s.local.Handles(modelID):
		s.local.ServeHTTP(w, r)
	case s.peer.Handles(modelID):
		s.peer.ServeHTTP(w, r)
	default:
		swaputil.SendResponse(w, r, http.StatusNotFound, "no router for model "+modelID)
	}
}
