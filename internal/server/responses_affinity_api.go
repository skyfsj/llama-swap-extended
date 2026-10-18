package server

import (
	"net/http"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/gateway"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// responseAffinityAllowed answers whether the caller may read or mutate a
// stored response. Rows written before identity tracking (empty IdentityID)
// stay model-scoped so an upgrade does not strand them; every new row is
// readable only by the credential that created it.
func responseAffinityAllowed(r *http.Request, affinity store.ResponseAffinity) bool {
	owner := strings.TrimSpace(affinity.IdentityID)
	if owner == "" {
		return true
	}
	return owner == identityIDFromContext(r.Context())
}

// handleAPIResponseGet resolves a Responses response id through the durable
// affinity table. Responses results are served from the stored canonical JSON;
// the selected Chat backend is used only when no local result is available.
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
	if !responseAffinityAllowed(r, affinity) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: this response belongs to another API key")
		return
	}
	if s.responseUsesChatAdapter(affinity.Model) && len(affinity.Response) > 0 {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(affinity.Response)
		return
	}
	s.handleResponseForward(w, r, affinity.Model)
}

// handleAPIResponseCancel answers the Responses cancel endpoint.
//
// A request reaches storage only after it finished, so by the time a cancel call
// can resolve an id there is nothing left to stop. The previous implementation
// rewrote a completed response to status "cancelled", which made the API claim
// it had stopped a request it had already delivered. Reporting the response as
// it really is (OpenAI returns a finished response unchanged) is the honest
// behaviour; cancelling a request that is still in flight is done by closing
// the connection, which the inflight middleware turns into a context cancel.
func (s *Server) handleAPIResponseCancel(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	id := strings.TrimSpace(r.PathValue("response_id"))
	affinity, found, err := s.store.GetResponseAffinity(r.Context(), id, time.Now())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		// Either the id is unknown, or the request is still running and has
		// nothing stored yet. Both mean: nothing to cancel here.
		swaputil.SendResponse(w, r, http.StatusNotFound, "response not found (a request still in flight is cancelled by closing its connection)")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, affinity.Model) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+affinity.Model)
		return
	}
	if !responseAffinityAllowed(r, affinity) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: this response belongs to another API key")
		return
	}
	if s.responseUsesChatAdapter(affinity.Model) && len(affinity.Response) > 0 {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(affinity.Response)
		return
	}
	s.handleResponseForward(w, r, affinity.Model)
}

// interactionsPassthrough forwards an Interactions retrieval/cancel/delete to a
// provider that speaks the dialect. A proxy owns no Interactions store, so the
// only correct answer is the upstream's own.
func (s *Server) interactionsPassthrough(w http.ResponseWriter, r *http.Request) {
	gw := s.currentGateway()
	if gw == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable,
			"interactions state is not stored locally; configure a gemini-interactions provider to serve this endpoint")
		return
	}
	route := gw.FirstRouteOfType(config.ProviderTypeGeminiInteractions)
	if route == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented,
			"interactions state is not stored locally; configure a gemini-interactions provider to serve this endpoint")
		return
	}
	s.proxylog.Debugf("dispatch: forwarding %s %s to provider %s", r.Method, r.URL.Path, route.ProviderID)
	gw.ServeHTTP(w, r, route, gateway.Session{})
}

// responseUsesChatAdapter keeps response lookup behavior tied to the selected
// model configuration rather than inferring protocol mode from stored data.
func (s *Server) responseUsesChatAdapter(model string) bool {
	if s == nil {
		return false
	}
	modelConfig, ok := s.currentConfig().Models[model]
	return ok && modelConfig.Backend.AdaptsResponses()
}

func (s *Server) handleResponseForward(w http.ResponseWriter, r *http.Request, model string) {
	query := r.URL.Query()
	query.Set("model", model)
	r.URL.RawQuery = query.Encode()
	// The regular model dispatcher accepts GET query-model requests only for
	// registered model routes. Calling the selected local/peer handler directly
	// keeps the original Responses path intact when no local result is stored.
	s.localPeerHandler(w, r)
}
