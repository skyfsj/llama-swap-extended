package server

import (
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"github.com/mostlygeek/llama-swap/pkg/dialect"
)

func createCompatibilityMiddleware() chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mode := r.Header.Get("X-Llama-Swap-Compatibility")
			if mode == "" {
				mode = "portable"
			}
			if mode != "portable" && mode != "strict" && mode != "native" {
				swaputil.SendResponse(w, r, 400, "compatibility must be portable, strict or native")
				return
			}
			var mu sync.Mutex
			codes := map[string]bool{}
			w.Header().Add("Trailer", "X-Llama-Swap-Conversion-Diagnostics")
			ctx := relaykit.WithCompatibility(r.Context(), mode, func(diagnostics []dialect.Diagnostic) {
				mu.Lock()
				defer mu.Unlock()
				for _, d := range diagnostics {
					codes[d.Code] = true
				}
				sorted := []string{}
				for code := range codes {
					sorted = append(sorted, code)
				}
				sort.Strings(sorted)
				w.Header().Set("X-Llama-Swap-Conversion-Diagnostics", strings.Join(sorted, ","))
			})
			r = r.WithContext(ctx)
			r.Header.Del("X-Llama-Swap-Compatibility")
			next.ServeHTTP(w, r)
		})
	}
}

type protocolExecutionPlan struct {
	Provider           string                `json:"provider"`
	Adapter            string                `json:"adapter"`
	Target             dialect.Format        `json:"target"`
	Classification     string                `json:"classification"`
	Diagnostics        []dialect.Diagnostic  `json:"diagnostics,omitempty"`
	Error              string                `json:"error,omitempty"`
	Capabilities       config.ModelCapConfig `json:"declared_or_discovered_capabilities"`
	CapabilityEvidence string                `json:"capability_evidence"`
}

func (s *Server) handleProtocolPlan(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, swaputil.MaxRequestBodySize+1))
	if err != nil || len(body) > swaputil.MaxRequestBodySize {
		swaputil.SendResponse(w, r, 413, "plan request is too large")
		return
	}
	var input struct {
		Source  dialect.Format  `json:"source"`
		Model   string          `json:"model"`
		Request json.RawMessage `json:"request"`
	}
	if json.Unmarshal(body, &input) != nil || input.Model == "" || len(input.Request) == 0 {
		swaputil.SendResponse(w, r, 400, "source, model and request are required")
		return
	}
	if !identityFromContext(r.Context()).HasModel(input.Model) {
		swaputil.SendResponse(w, r, 403, "model is not allowed")
		return
	}
	if _, _, err := dialect.DecodeRequest(input.Source, input.Request); err != nil {
		swaputil.SendResponse(w, r, 400, err.Error())
		return
	}
	plans := []protocolExecutionPlan{}
	cfg := s.currentConfig()
	if model, ok := cfg.Models[input.Model]; ok {
		plans = append(plans, protocolExecutionPlan{Provider: "local", Target: dialect.OpenAIChat, Capabilities: s.resolveCapabilities(r.Context(), input.Model, model), CapabilityEvidence: "configured_and_cached_discovery"})
	}
	if gateway := s.currentGateway(); gateway != nil {
		for _, route := range gateway.PoolRoutes(input.Model, false, "") {
			plans = append(plans, protocolExecutionPlan{Provider: route.ProviderID, Adapter: route.AdapterName, Target: surfaceFormatForType(route.Type()), Capabilities: route.Capabilities, CapabilityEvidence: "configured"})
		}
	}
	for i := range plans {
		plan := &plans[i]
		if input.Source == plan.Target {
			plan.Classification = "native"
			continue
		}
		_, diagnostics, err := dialect.ConvertRequest(input.Source, plan.Target, input.Request, dialect.ConvertOptions{Model: input.Model})
		plan.Diagnostics = diagnostics
		plan.Classification = "compatible"
		if err != nil {
			plan.Classification = "rejected"
			plan.Error = err.Error()
		} else if len(diagnostics) > 0 {
			plan.Classification = "degraded"
		}
	}
	protocolJSON(w, 200, map[string]any{"model": input.Model, "source": input.Source, "plans": plans, "runtime": map[string]any{"mcp": cfg.ProtocolRuntime.MCP.Enabled, "mcp_primitives": []string{"tools", "resources", "resource_templates", "prompts"}, "files": "gateway_owned", "conversations": "checkpoint_and_native_lineage", "mcp_live_endpoint": "/mcp?model={model}&live=true", "mcp_input_continuation": "client_input_and_model_resume", "background_checkpoints": "/api/conversations/{id}/generations", "background_execution": []string{"gateway", "native"}, "native_background_formats": []string{string(dialect.OpenAIResponses), string(dialect.GeminiInteractions)}, "unsupported": []string{"cross_provider_opaque_state_replay"}}, "unknown_capabilities_require_provider_validation": true})
}
