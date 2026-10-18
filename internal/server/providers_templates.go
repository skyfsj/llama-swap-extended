package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// handleAPIProviderTemplates lists the built-in provider templates the UI can
// instantiate.
func (s *Server) handleAPIProviderTemplates(w http.ResponseWriter, r *http.Request) {
	templates, err := config.ProviderTemplates()
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	type adapterSummary struct {
		Name string `json:"name,omitempty"`
		Type string `json:"type"`
	}
	out := make([]map[string]any, 0, len(templates))
	for _, template := range templates {
		adapters := make([]adapterSummary, 0, len(template.Adapters))
		for _, adapter := range template.Adapters {
			adapters = append(adapters, adapterSummary{Name: adapter.Name, Type: adapter.Type})
		}
		out = append(out, map[string]any{
			"id":             template.ID,
			"name":           template.Name,
			"description":    template.Description,
			"adapters":       adapters,
			"requiresKey":    template.ModelsAuth != config.TemplateModelsAuthNone,
			"quotaPreset":    quotaPresetOf(template),
			"sessionHeaders": template.Provider.SessionHeaders,
		})
	}
	writeJSON(w, map[string]any{"templates": out})
}

func quotaPresetOf(template config.ProviderTemplate) string {
	if template.Provider.Quota == nil {
		return ""
	}
	return template.Provider.Quota.Preset
}

// handleAPIProviderTemplateApply builds a provider draft from a template and
// fills the model list by pulling the live upstream list, distributing models
// across the template's adapters with their modelMatch patterns.
//
// The pull authenticates with the request's apiKey when given, otherwise with
// the stored credential of providerID (so an existing provider can re-pull
// without the UI ever holding the secret).
func (s *Server) handleAPIProviderTemplateApply(w http.ResponseWriter, r *http.Request) {
	templateID := strings.TrimSpace(r.PathValue("id"))
	template, ok := config.ProviderTemplateByID(templateID)
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, fmt.Sprintf("unknown provider template %q", templateID))
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<16))
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "could not read request body")
		return
	}
	var req struct {
		APIKey     string `json:"apiKey,omitempty"`
		ProviderID string `json:"providerID,omitempty"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid JSON body")
		return
	}

	warnings := make([]string, 0, 2)
	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" && req.ProviderID != "" {
		if stored, ok := s.storedCredential(req.ProviderID); ok {
			apiKey = stored
		} else {
			warnings = append(warnings, fmt.Sprintf("provider %s has no usable credential for the model pull", req.ProviderID))
		}
	}

	models, pullErr := s.pullTemplateModels(r.Context(), template, apiKey)
	if pullErr != nil {
		warnings = append(warnings, pullErr.Error())
	}

	draft := template.ProviderDraft(apiKey, models)
	assigned := map[string]int{}
	adapters, _ := draft["adapters"].([]map[string]any)
	for _, adapter := range adapters {
		label, _ := adapter["name"].(string)
		if label == "" {
			label, _ = adapter["type"].(string)
		}
		if entries, ok := adapter["models"].([]any); ok {
			assigned[label] = len(entries)
		}
	}

	writeJSON(w, map[string]any{
		"provider": draft,
		"pulled":   len(models),
		"assigned": assigned,
		"warnings": warnings,
	})
}

// storedCredential returns the first usable key value of an existing provider.
func (s *Server) storedCredential(providerID string) (string, bool) {
	gw := s.currentGateway()
	if gw == nil {
		return "", false
	}
	for _, route := range gw.AllRoutes() {
		if route.ProviderID != providerID {
			continue
		}
		for _, cred := range route.Credentials() {
			if strings.TrimSpace(cred.Value) != "" && !cred.Disabled {
				return cred.Value, true
			}
		}
	}
	return "", false
}

// pullTemplateModels fetches the template's model list. A failed pull is not
// fatal: the caller returns the template skeleton with a warning so the user
// can retry from the editor.
func (s *Server) pullTemplateModels(ctx context.Context, template config.ProviderTemplate, apiKey string) ([]string, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, template.ModelsEndpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("model list request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if apiKey != "" {
		switch template.ModelsAuth {
		case config.TemplateModelsAuthNone:
		case config.TemplateModelsAuthAPIKey:
			req.Header.Set("x-api-key", apiKey)
		case config.TemplateModelsAuthGoogle:
			req.Header.Set("x-goog-api-key", apiKey)
		default:
			req.Header.Set("Authorization", "Bearer "+apiKey)
		}
	}

	transport, err := gateway.BuildTransport("", 15, 15, 30, 10, 30)
	if err != nil {
		return nil, fmt.Errorf("model list transport: %w", err)
	}
	defer transport.CloseIdleConnections()

	client := &http.Client{Transport: transport}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("model list request failed: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, fmt.Errorf("reading model list: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		snippet := strings.TrimSpace(string(body))
		if len(snippet) > 200 {
			snippet = snippet[:200]
		}
		return nil, fmt.Errorf("model list returned status %d: %s", resp.StatusCode, snippet)
	}

	models := normalizeUpstreamModelList(body)
	sort.Strings(models)
	if len(models) == 0 {
		return nil, fmt.Errorf("model list contained no usable models")
	}
	return models, nil
}
