package server

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
)

// keyStatus is one credential's runtime state as served by the status API.
// Key material is never included.
type keyStatus struct {
	ID             string    `json:"id"`
	Disabled       bool      `json:"disabled"`
	Unresolved     bool      `json:"unresolvedEnv"`
	Inflight       int       `json:"inflight"`
	MaxConcurrent  int       `json:"maxConcurrent"`
	AuthUntil      time.Time `json:"authCooldownUntil,omitempty"`
	RateLimitUntil time.Time `json:"rateLimitCooldownUntil,omitempty"`
	TransientUntil time.Time `json:"transientCooldownUntil,omitempty"`
	// QuotaUntil is set by quota enforcement when a usage window is full.
	QuotaUntil  time.Time `json:"quotaCooldownUntil,omitempty"`
	LastError   string    `json:"lastError,omitempty"`
	LastErrorAt time.Time `json:"lastErrorAt,omitempty"`
	LastUsed    time.Time `json:"lastUsed,omitempty"`
}

func keyStatusFromCredential(status gateway.CooldownStatus) keyStatus {
	return keyStatus{
		ID:             status.ID,
		Disabled:       status.Disabled,
		Unresolved:     status.Unresolved,
		Inflight:       status.Inflight,
		MaxConcurrent:  status.MaxConcurrent,
		AuthUntil:      status.AuthUntil,
		RateLimitUntil: status.RateLimitUntil,
		TransientUntil: status.TransientUntil,
		QuotaUntil:     status.QuotaUntil,
		LastError:      status.LastError,
		LastErrorAt:    status.LastErrorAt,
		LastUsed:       status.LastUsed,
	}
}

// handleAPIProvidersStatus reports the runtime state of every provider route
// and credential: cooldowns, in-flight counts, last errors. Key values are
// never included.
func (s *Server) handleAPIProvidersStatus(w http.ResponseWriter, r *http.Request) {
	gw := s.currentGateway()
	cfg := s.currentConfig()

	type routeStatus struct {
		ProviderID  string `json:"providerID"`
		Adapter     string `json:"adapter,omitempty"`
		AdapterType string `json:"adapterType"`
		ModelID     string `json:"modelID"`
		// ModelName is the client-facing name (modelPrefix + model ID).
		ModelName     string      `json:"modelName"`
		RemoteName    string      `json:"remoteName"`
		FQN           string      `json:"fqn"`
		Sticky        bool        `json:"sticky"`
		ProviderFirst bool        `json:"providerFirst"`
		MaxRetries    int         `json:"maxRetries"`
		Keys          []keyStatus `json:"keys"`
	}
	type adapterStatus struct {
		Name       string `json:"name,omitempty"`
		Type       string `json:"type"`
		Endpoint   string `json:"endpoint"`
		ModelCount int    `json:"modelCount"`
	}

	out := struct {
		Providers     []map[string]any `json:"providers"`
		Routes        []routeStatus    `json:"routes"`
		StickyEnabled bool             `json:"stickySessionsEnabled"`
		// QuotaPresets describes the built-in usage mappings the UI offers as
		// providers.<id>.quota.preset values: the dropdown needs the label,
		// and the hint under it needs the endpoint and its shape.
		QuotaPresets []config.QuotaPresetSummary `json:"quotaPresets"`
		// Latency reports the observed first-token latency per provider. It
		// backs the skip-slow routing rule, so an operator can see why a
		// provider is being passed over instead of guessing.
		Latency map[string]gateway.ProviderLatency `json:"latency,omitempty"`
	}{
		Providers: []map[string]any{}, Routes: []routeStatus{},
		QuotaPresets: config.QuotaPresetSummaries(),
	}
	if gw != nil {
		out.Latency = gw.Latencies()
	}

	for _, id := range sortedProviderIDs(cfg) {
		p := cfg.Providers[id]
		adapters := p.EffectiveAdapters()
		adapterOut := make([]adapterStatus, 0, len(adapters))
		for _, adapter := range adapters {
			adapterOut = append(adapterOut, adapterStatus{
				Name:       adapter.Name,
				Type:       adapter.Type,
				Endpoint:   adapter.Endpoint,
				ModelCount: len(adapter.Models),
			})
		}
		// type/endpoint stay populated for single-adapter providers so the
		// existing list UI keeps working; multi-adapter providers report
		// "multi" and no single endpoint.
		primaryType, primaryEndpoint := p.Type, p.Endpoint
		if len(adapters) > 1 {
			primaryType, primaryEndpoint = "multi", ""
		}
		// Every configured key, including ones disabled in configuration:
		// route member lists exclude those, but the quota card still shows
		// their subscription and the operator needs to see the key that is
		// switched off. An empty list for providers the gateway never built.
		providerKeys := []keyStatus{}
		if gw != nil {
			for _, cred := range gw.ProviderCredentials(id) {
				providerKeys = append(providerKeys, keyStatusFromCredential(cred.Status()))
			}
		}
		out.Providers = append(out.Providers, map[string]any{
			"id":             id,
			"type":           primaryType,
			"endpoint":       primaryEndpoint,
			"adapters":       adapterOut,
			"template":       p.Template,
			"proxy":          p.Proxy,
			"priority":       p.Priority,
			"weight":         p.Weight,
			"sticky":         p.StickyEnabled(),
			"bareNamePolicy": p.BareNamePolicy,
			"cooldownPolicy": p.Cooldown.Policy,
			"maxRetries":     p.MaxRetriesPerCredential(),
			"disabled":       p.Disabled,
			"modelCount":     len(p.AllModels()),
			"keyCount":       len(p.ApiKeys),
			"keys":           providerKeys,
		})
	}

	if gw != nil {
		for _, route := range gw.AllRoutes() {
			rs := routeStatus{
				ProviderID:    route.ProviderID,
				Adapter:       route.Adapter(),
				AdapterType:   route.Type(),
				ModelID:       route.ModelID,
				ModelName:     route.ExposedName,
				RemoteName:    route.RemoteName,
				FQN:           config.ProviderModelFQN(route.ProviderID, route.ModelID),
				Sticky:        route.Sticky,
				ProviderFirst: route.ProviderFirst,
				MaxRetries:    route.MaxRetries,
				Keys:          []keyStatus{},
			}
			for _, cred := range route.Credentials() {
				rs.Keys = append(rs.Keys, keyStatusFromCredential(cred.Status()))
			}
			out.Routes = append(out.Routes, rs)
		}
	}

	writeJSON(w, out)
}

// handleAPIProvidersClearCooldown resets cooldowns on one credential.
func (s *Server) handleAPIProvidersClearCooldown(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("keyID")
	gw := s.currentGateway()
	if gw == nil {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "no provider gateway active"})
		return
	}
	cleared := false
	for _, route := range gw.AllRoutes() {
		for _, cred := range route.Credentials() {
			if cred.ID == keyID {
				cred.ClearCooldowns()
				cleared = true
			}
		}
	}
	if !cleared {
		w.WriteHeader(http.StatusNotFound)
		writeJSON(w, map[string]string{"error": "credential not found"})
		return
	}
	writeJSON(w, map[string]string{"status": "ok"})
}

// providersProbeRequest carries draft (unsaved) provider values so the UI can
// test connectivity and pull the upstream model list before saving.
type providersProbeRequest struct {
	Type     string `json:"type"`
	Endpoint string `json:"endpoint"`
	Proxy    string `json:"proxy"`
	APIKey   string `json:"apiKey"`
	Timeout  int    `json:"timeoutSeconds,omitempty"`
}

// handleAPIProvidersProbe tests a draft provider: it requests the upstream
// model list (OpenAI /v1/models or Anthropic /v1/models) and returns the
// normalized model IDs.
func (s *Server) handleAPIProvidersProbe(w http.ResponseWriter, r *http.Request) {
	var req providersProbeRequest
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "could not read request body"})
		return
	}
	if err := json.Unmarshal(body, &req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid JSON body"})
		return
	}

	endpoint := strings.TrimRight(strings.TrimSpace(req.Endpoint), "/")
	if endpoint == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "endpoint is required"})
		return
	}
	// Mirror config validation: a base URL written as .../v1 must not produce
	// a doubled /v1/v1/models probe URL.
	endpoint = strings.TrimSuffix(endpoint, "/v1")
	parsed, err := url.Parse(endpoint)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "endpoint must be an http(s) URL"})
		return
	}

	timeout := time.Duration(req.Timeout) * time.Second
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	client := &http.Client{Timeout: timeout}

	// The Gemini API lists models under /v1beta (a /v1 suffix on the endpoint
	// is still stripped above, matching request forwarding).
	probeURL := endpoint + "/v1/models"
	if req.Type == config.ProviderTypeGemini || req.Type == config.ProviderTypeGeminiInteractions {
		probeURL = endpoint + "/v1beta/models"
	}
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, probeURL, nil)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": fmt.Sprintf("invalid probe URL: %v", err)})
		return
	}
	// The probe always builds its own transport, in every proxy case.
	//
	// The process default transport is pinned to HTTP/1.1 so that the network
	// policy decides every connection (net/http hands HTTPS requests to HTTP/2
	// before consulting Transport.Proxy), and an upstream that only speaks
	// HTTP/2 — api.xiaomimimo.com is one — is then unreachable: it answers the
	// HTTP/1.1 request with HTTP/2 frames. A probe is a single explicit request,
	// so it can carry the HTTP/2-capable transport the gateway uses; "" follows
	// the global policy, "direct" bypasses every proxy.
	if proxy := strings.TrimSpace(req.Proxy); proxy != "" && proxy != config.ProxyDirect {
		pu, perr := url.Parse(proxy)
		if perr != nil || (pu.Scheme != "http" && pu.Scheme != "https" && pu.Scheme != "socks5" && pu.Scheme != "socks5h") {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "proxy must be http/https/socks5/socks5h or 'direct'"})
			return
		}
	}
	tr, berr := probeTransportBuilder(req.Proxy)
	if berr != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": berr.Error()})
		return
	}
	client.Transport = tr

	if strings.TrimSpace(req.APIKey) != "" {
		switch req.Type {
		case config.ProviderTypeAnthropic:
			httpReq.Header.Set("x-api-key", req.APIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		case config.ProviderTypeGemini, config.ProviderTypeGeminiInteractions:
			// The Interactions API authenticates exactly like generateContent.
			httpReq.Header.Set("x-goog-api-key", req.APIKey)
		default:
			httpReq.Header.Set("Authorization", "Bearer "+req.APIKey)
		}
	}

	resp, err := client.Do(httpReq)
	if err != nil {
		w.WriteHeader(http.StatusBadGateway)
		writeJSON(w, map[string]string{"error": fmt.Sprintf("probe failed: %v", err)})
		return
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))

	if resp.StatusCode != http.StatusOK {
		w.WriteHeader(resp.StatusCode)
		writeJSON(w, map[string]any{
			"error":    fmt.Sprintf("upstream returned status %d", resp.StatusCode),
			"upstream": strings.TrimSpace(string(respBody[:min(2048, len(respBody))])),
		})
		return
	}

	models := normalizeUpstreamModelList(respBody)
	if models == nil {
		models = []string{}
	}
	writeJSON(w, map[string]any{"models": models})
}

// normalizeUpstreamModelList extracts model IDs from an OpenAI- or
// Anthropic-shaped /v1/models response, or a Gemini /v1beta/models response.
func normalizeUpstreamModelList(body []byte) []string {
	// Gemini: {"models":[{"name":"models/gemini-2.0-flash","supportedGenerationMethods":[...]}]}
	var gemini struct {
		Models []struct {
			Name                       string   `json:"name"`
			SupportedGenerationMethods []string `json:"supportedGenerationMethods"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &gemini); err == nil && len(gemini.Models) > 0 {
		ids := make([]string, 0, len(gemini.Models))
		for _, m := range gemini.Models {
			name := strings.TrimPrefix(strings.TrimSpace(m.Name), "models/")
			if name == "" {
				continue
			}
			// Skip embedding/AQA models: only generateContent models can be
			// routed through the gateway as chat backends.
			if len(m.SupportedGenerationMethods) > 0 && !containsString(m.SupportedGenerationMethods, "generateContent") {
				continue
			}
			ids = append(ids, name)
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			return ids
		}
	}
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &payload); err == nil && len(payload.Data) > 0 {
		ids := make([]string, 0, len(payload.Data))
		for _, m := range payload.Data {
			if strings.TrimSpace(m.ID) != "" {
				ids = append(ids, m.ID)
			}
		}
		sort.Strings(ids)
		return ids
	}
	// Anthropic shape: {"data":[{"id":...}]} already covered; some upstreams
	// return a bare array.
	var bare []struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &bare); err == nil && len(bare) > 0 {
		ids := make([]string, 0, len(bare))
		for _, m := range bare {
			if strings.TrimSpace(m.ID) != "" {
				ids = append(ids, m.ID)
			}
		}
		sort.Strings(ids)
		return ids
	}
	return nil
}

// buildProbeTransport reuses the gateway transport builder for probe requests.
func buildProbeTransport(proxyValue string) (http.RoundTripper, error) {
	return gateway.BuildTransport(proxyValue, 15, 15, 15, 10, 30)
}

// probeTransportBuilder is a test seam: the probe builds its own transport (see
// the proxy handling above), so a test that points it at a local TLS server has
// no other way to trust that server's certificate.
var probeTransportBuilder = buildProbeTransport

// sortedProviderIDs returns provider IDs in deterministic order.
func sortedProviderIDs(cfg config.Config) []string {
	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
