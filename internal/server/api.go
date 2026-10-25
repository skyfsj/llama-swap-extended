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
	// ContextWindow mirrors ContextLength for OpenAI-compatible gateways that
	// read the context size from this field name (GROQ convention, followed by
	// e.g. Bifrost).
	ContextWindow int            `json:"context_window,omitempty"`
	Meta          map[string]any `json:"meta,omitempty"`
	Status        map[string]any `json:"status"`
}

// cappedMetadataKeys are top-level /v1/models fields produced by the
// capabilities renderer. If a model's metadata block defines any of these
// keys, the renderer's values win and the metadata keys are dropped.
var cappedMetadataKeys = map[string]struct{}{
	"architecture":         {},
	"capabilities":         {},
	"supported_parameters": {},
	"context_length":       {},
	"context_window":       {},
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
		rec.ContextWindow = rec.ContextLength
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
		// What the config declares, filled in with whatever the last probe of
		// the running upstream learned (unless capabilities.disableAuto is
		// set on the model).
		caps := s.resolveCapabilities(r.Context(), id, mc)
		data = append(data, newRecord(id, mc.Name, mc.Description, mc.Metadata, caps, status, internalMetadata))

		if cfg.IncludeAliasesInList {
			for _, alias := range mc.Aliases {
				if alias := strings.TrimSpace(alias); alias != "" {
					data = append(data, newRecord(
						alias,
						mc.Name,
						mc.Description,
						mc.Metadata,
						caps,
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

	// Provider models follow the peer pattern: FQNs are always listed; the
	// bare name enters the listing when the gateway alone claims it (the
	// dispatch arbitration with local models is the same rule).
	if gw := s.currentGateway(); gw != nil {
		for _, route := range gw.AllRoutes() {
			// The client-facing name is the provider's modelPrefix plus the
			// model ID (the bare ID when no prefix is configured). A provider
			// may still be called with the legacy provider/model form, but the
			// listing shows one stable name per model.
			name := route.ExposedName
			if name == "" {
				name = config.ProviderModelName("", route.ModelID)
			}
			legacy := config.ProviderModelFQN(route.ProviderID, route.ModelID)
			if !modelAllowedForIdentity(cfg, identity, name) && !modelAllowedForIdentity(cfg, identity, legacy) {
				continue
			}
			if _, duplicate := modelIDs[name]; duplicate {
				// Another provider exposes the same client-facing name; the
				// gateway arbitrates, the listing shows it once.
				continue
			}
			modelIDs[name] = struct{}{}
			status := "loaded"
			if route.BestCredential() == nil {
				// Every credential is cooling down or saturated; the model
				// stays listed but reports unavailable.
				status = "unavailable"
			}
			metadata := map[string]any{"type": "provider", "providerID": route.ProviderID}
			if len(route.ModelAliases) > 0 {
				// Same shape as a local model's aliases so the pickers nest
				// them under the model they resolve to.
				metadata["aliases"] = route.ModelAliases
			}
			data = append(data, newRecord(
				name,
				route.ProviderID+": "+route.ModelID,
				"",
				nil,
				route.Capabilities,
				status,
				metadata,
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

	// The global CORS middleware already answered the browser; echoing the
	// Origin here as well would produce two values under one header key.
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

// redirectRelative sends a redirect whose Location header stays relative to
// the URL the client requested. http.Redirect resolves its destination against
// the server-visible request path, which drops the path prefix when llama-swap
// runs behind a prefix-stripping reverse proxy (a browser resolves a relative
// Location against the URL it actually asked for, so the prefix survives).
// Target must not start with "/".
func redirectRelative(w http.ResponseWriter, target string, code int) {
	w.Header().Set("Location", target)
	w.WriteHeader(code)
}

func handleRootRedirect(w http.ResponseWriter, r *http.Request) {
	redirectRelative(w, "ui/", http.StatusFound)
}

// handleUpstreamRedirect sends /upstream to the Models page. The target uses
// the hash router (ui/#/models) so the document stays mounted at <prefix>/ui/:
// a path route (ui/models) would leave the UI's prefix detection in
// ui/src/lib/apiPath.ts unable to recover the proxy prefix, disconnecting the
// page behind a prefix-stripping reverse proxy.
func handleUpstreamRedirect(w http.ResponseWriter, r *http.Request) {
	redirectRelative(w, "ui/#/models", http.StatusFound)
}

func handleComfyUIRedirect(w http.ResponseWriter, r *http.Request) {
	// Relative Location so the redirect keeps a reverse proxy's path prefix
	// (see redirectRelative).
	location := "comfyui/"
	if r.URL.RawQuery != "" {
		location += "?" + r.URL.RawQuery
	}
	status := http.StatusPermanentRedirect
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		status = http.StatusMovedPermanently
	}
	redirectRelative(w, location, status)
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
	if err := s.prepareResourceLoadContext(r.Context(), config.ComfyUIModelID); err != nil {
		swaputil.SendResponse(w, r, http.StatusInsufficientStorage, err.Error())
		return
	}
	defer s.releaseResourceReservation(config.ComfyUIModelID)
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
	// responses resolve. The Location stays relative so the redirect keeps a
	// reverse proxy's path prefix (see redirectRelative). 301 for GET/HEAD,
	// 308 otherwise to preserve the method.
	//
	// The target is built from the last escaped segment of the requested URL,
	// not from searchName: a slash name ("author/model") arriving as two
	// segments would resolve "author/model/" against /upstream/author/ and
	// double the path, and a colon alias ("model1:high") would parse as a URL
	// scheme (RFC 3986 4.2). "./<segment>/" stays path-relative for both, and
	// reusing the escaped segment keeps %2F-encoded names redirecting to the
	// URL the client actually requested.
	if remainingPath == "/" && !strings.HasSuffix(r.URL.Path, "/") {
		escapedPath := r.URL.EscapedPath()
		newPath := "./" + escapedPath[strings.LastIndex(escapedPath, "/")+1:] + "/"
		if r.URL.RawQuery != "" {
			newPath += "?" + r.URL.RawQuery
		}
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			redirectRelative(w, newPath, http.StatusMovedPermanently)
		} else {
			redirectRelative(w, newPath, http.StatusPermanentRedirect)
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
		if err := s.prepareResourceLoadContext(r.Context(), modelID); err != nil {
			swaputil.SendResponse(w, r, http.StatusInsufficientStorage, err.Error())
			return
		}
		defer s.releaseResourceReservation(modelID)
		// The raw passthrough path shares the same maintenance contract as
		// the model-dispatch chain: a thermally parked model fails over
		// instead of being started.
		data := swaputil.ReqContextData{Model: searchName, ModelID: modelID, Metadata: map[string]string{}}
		if !s.forwardMaintenanceModel(w, r, data, cfg) {
			return
		}
		s.local.ServeHTTP(w, r)
	case s.peer.Handles(modelID):
		s.peer.ServeHTTP(w, r)
	default:
		swaputil.SendResponse(w, r, http.StatusNotFound, "no router for model "+modelID)
	}
}
