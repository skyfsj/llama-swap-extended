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

// handleAPIProvidersStatus reports the runtime state of every provider route
// and credential: cooldowns, in-flight counts, last errors. Key values are
// never included.
func (s *Server) handleAPIProvidersStatus(w http.ResponseWriter, r *http.Request) {
	gw := s.currentGateway()
	cfg := s.currentConfig()

	type keyStatus struct {
		ID             string    `json:"id"`
		Disabled       bool      `json:"disabled"`
		Unresolved     bool      `json:"unresolvedEnv"`
		Inflight       int       `json:"inflight"`
		MaxConcurrent  int       `json:"maxConcurrent"`
		AuthUntil      time.Time `json:"authCooldownUntil,omitempty"`
		RateLimitUntil time.Time `json:"rateLimitCooldownUntil,omitempty"`
		TransientUntil time.Time `json:"transientCooldownUntil,omitempty"`
		LastError      string    `json:"lastError,omitempty"`
		LastErrorAt    time.Time `json:"lastErrorAt,omitempty"`
		LastUsed       time.Time `json:"lastUsed,omitempty"`
	}
	type routeStatus struct {
		ProviderID    string      `json:"providerID"`
		ModelID       string      `json:"modelID"`
		RemoteName    string      `json:"remoteName"`
		FQN           string      `json:"fqn"`
		Sticky        bool        `json:"sticky"`
		ProviderFirst bool        `json:"providerFirst"`
		MaxRetries    int         `json:"maxRetries"`
		Keys          []keyStatus `json:"keys"`
	}

	out := struct {
		Providers     []map[string]any `json:"providers"`
		Routes        []routeStatus    `json:"routes"`
		StickyEnabled bool             `json:"stickySessionsEnabled"`
	}{
		Providers: []map[string]any{}, Routes: []routeStatus{},
	}

	for _, id := range sortedProviderIDs(cfg) {
		p := cfg.Providers[id]
		out.Providers = append(out.Providers, map[string]any{
			"id":             id,
			"type":           p.Type,
			"endpoint":       p.Endpoint,
			"proxy":          p.Proxy,
			"priority":       p.Priority,
			"weight":         p.Weight,
			"sticky":         p.StickyEnabled(),
			"bareNamePolicy": p.BareNamePolicy,
			"maxRetries":     p.MaxRetries,
			"disabled":       p.Disabled,
			"modelCount":     len(p.Models),
			"keyCount":       len(p.ApiKeys),
		})
	}

	if gw != nil {
		for _, route := range gw.AllRoutes() {
			rs := routeStatus{
				ProviderID:    route.ProviderID,
				ModelID:       route.ModelID,
				RemoteName:    route.RemoteName,
				FQN:           config.ProviderModelFQN(route.ProviderID, route.ModelID),
				Sticky:        route.Sticky,
				ProviderFirst: route.ProviderFirst,
				MaxRetries:    route.MaxRetries,
				Keys:          []keyStatus{},
			}
			for _, cred := range route.Credentials() {
				st := cred.Status()
				rs.Keys = append(rs.Keys, keyStatus{
					ID:             st.ID,
					Disabled:       st.Disabled,
					Unresolved:     st.Unresolved,
					Inflight:       st.Inflight,
					MaxConcurrent:  st.MaxConcurrent,
					AuthUntil:      st.AuthUntil,
					RateLimitUntil: st.RateLimitUntil,
					TransientUntil: st.TransientUntil,
					LastError:      st.LastError,
					LastErrorAt:    st.LastErrorAt,
					LastUsed:       st.LastUsed,
				})
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

	probeURL := endpoint + "/v1/models"
	httpReq, err := http.NewRequestWithContext(r.Context(), http.MethodGet, probeURL, nil)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": fmt.Sprintf("invalid probe URL: %v", err)})
		return
	}
	if strings.TrimSpace(req.Proxy) == "direct" {
		client.Transport = &http.Transport{Proxy: nil}
	} else if proxy := strings.TrimSpace(req.Proxy); proxy != "" {
		pu, perr := url.Parse(proxy)
		if perr != nil || (pu.Scheme != "http" && pu.Scheme != "https" && pu.Scheme != "socks5" && pu.Scheme != "socks5h") {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "proxy must be http/https/socks5/socks5h or 'direct'"})
			return
		}
		tr, berr := buildProbeTransport(proxy)
		if berr != nil {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": berr.Error()})
			return
		}
		client.Transport = tr
	}

	if strings.TrimSpace(req.APIKey) != "" {
		if req.Type == config.ProviderTypeAnthropic {
			httpReq.Header.Set("x-api-key", req.APIKey)
			httpReq.Header.Set("anthropic-version", "2023-06-01")
		} else {
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
// Anthropic-shaped /v1/models response.
func normalizeUpstreamModelList(body []byte) []string {
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

// sortedProviderIDs returns provider IDs in deterministic order.
func sortedProviderIDs(cfg config.Config) []string {
	ids := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
