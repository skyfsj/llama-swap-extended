package server

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// handleAPIResponseGet resolves a Responses response id through the durable
// affinity table. Chat-adapted responses are served from the stored canonical
// Responses JSON; native backends are dispatched upstream with the model
// restored as a query parameter.
func (s *Server) handleAPIResponseGet(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	affinity, found, err := s.store.GetResponseAffinity(r.Context(), strings.TrimSpace(r.PathValue("response_id")), time.Now())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, "response not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, affinity.Model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+affinity.Model)
		return
	}
	if s.responseUsesChatAdapter(affinity.Model) && len(affinity.Response) > 0 {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(affinity.Response)
		return
	}
	s.handleResponseForward(w, r, affinity.Model)
}

// handleAPIResponseCancel forwards native cancellation to the selected
// backend. For a chat-adapted response there is no backend response id to
// cancel after completion, so the stored response is returned with an
// explicit cancelled status rather than pretending a remote cancellation
// happened.
func (s *Server) handleAPIResponseCancel(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	id := strings.TrimSpace(r.PathValue("response_id"))
	affinity, found, err := s.store.GetResponseAffinity(r.Context(), id, time.Now())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, "response not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, affinity.Model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+affinity.Model)
		return
	}
	if s.responseUsesChatAdapter(affinity.Model) && len(affinity.Response) > 0 {
		var response map[string]any
		if json.Unmarshal(affinity.Response, &response) == nil {
			response["status"] = "cancelled"
			body, marshalErr := json.Marshal(response)
			if marshalErr == nil {
				affinity.Status = "cancelled"
				affinity.Response = body
				affinity.UpdatedAt = time.Now()
				// The client is told "cancelled" below whether or not this lands,
				// so a failed write leaves the stored row saying "completed" with
				// no record of why the two disagree.
				if err := s.store.UpsertResponseAffinity(r.Context(), affinity); err != nil {
					s.proxylog.Errorf("persist cancelled response %s: %v", affinity.ResponseID, err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(affinity.Response)
		return
	}
	s.handleResponseForward(w, r, affinity.Model)
}

// responseUsesChatAdapter is deliberately resolved from the current model
// configuration instead of using the presence of response_json as a proxy for
// protocol mode. Native Responses responses are also persisted for audit and
// restart correlation, but GET/cancel must still reach the upstream native
// endpoint; only the Chat adapter can satisfy those operations from its local
// response chain.
func (s *Server) responseUsesChatAdapter(model string) bool {
	if s == nil {
		return false
	}
	modelConfig, ok := s.currentConfig().Models[model]
	return ok && modelConfig.Backend.EffectiveProtocol() == config.AdapterProtocolResponsesToChat
}

func (s *Server) handleResponseForward(w http.ResponseWriter, r *http.Request, model string) {
	query := r.URL.Query()
	query.Set("model", model)
	r.URL.RawQuery = query.Encode()
	// The regular model dispatcher accepts GET query-model requests only for
	// registered model routes. Calling the selected local/peer handler directly
	// keeps the original response path intact for native backends.
	s.localPeerHandler(w, r)
}
