package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/event"
	"github.com/mostlygeek/llama-swap/internal/perf"
	"github.com/mostlygeek/llama-swap/internal/process"
	"github.com/mostlygeek/llama-swap/internal/router/scheduler"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// apiModel is one entry in the /api/events modelStatus payload.
type apiModel struct {
	Id          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	State       string `json:"state"`
	BackendType string `json:"backendType,omitempty"`
	Unlisted    bool   `json:"unlisted"`
	// Disabled is maintenance mode: the model keeps its configuration but is
	// not servable. It is exposed here so the management UI can still show it
	// and offer to re-enable it, which the /v1/models listing deliberately does
	// not.
	Disabled bool `json:"disabled"`
	// Maintenance means the model is out of service after a failed
	// configuration start, because auto-rollback is disabled. It differs from
	// Disabled: maintenance is entered automatically and exits on its own once
	// a start succeeds and serves a request.
	Maintenance   bool           `json:"maintenance"`
	PeerID        string         `json:"peerID"`
	Aliases       []string       `json:"aliases,omitempty"`
	Capabilities  map[string]any `json:"capabilities,omitempty"`
	ContextLength int            `json:"context_length,omitempty"`
	ConfigStatus  string         `json:"configStatus"`
	AppliedRev    uint64         `json:"appliedRevision"`
	DesiredRev    uint64         `json:"desiredRevision"`
	OldRequests   int            `json:"oldRequests"`
	Waiting       int            `json:"waitingRequests"`
	ConfigError   string         `json:"error,omitempty"`
}

type apiModelLoadConflict struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	State string `json:"state"`
}

// configLifecycleErrorCode is deliberately safe to expose through modelStatus
// SSE. Process startup errors can include command lines, local paths, or
// environment-derived values, so the detailed cause stays in server logs.
const configLifecycleErrorCode = "model_config_update_failed"

const configLifecycleExitStatusPrefix = configLifecycleErrorCode + "_exit_"

func publicConfigLifecycleError(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// An exit status is safe and useful to expose, while the surrounding
	// command/error text may contain paths, credentials, or other arguments.
	const marker = "exit status "
	if index := strings.LastIndex(raw, marker); index >= 0 {
		value := strings.TrimSpace(raw[index+len(marker):])
		if fields := strings.Fields(value); len(fields) > 0 {
			if code, err := strconv.Atoi(fields[0]); err == nil && code >= 0 && code <= 255 {
				return configLifecycleExitStatusPrefix + strconv.Itoa(code)
			}
		}
	}
	return configLifecycleErrorCode
}

type apiProfile struct {
	ID          string            `json:"id"`
	Description string            `json:"description"`
	Pins        map[string]string `json:"pins"`
}

func nullableProfile(name string) any {
	if name == "" {
		return nil
	}
	return name
}

// backendProgressVisible applies the same model authorization rules to the
// long-lived event stream that the point-in-time backend endpoints use. A
// restricted identity must not receive model-less runtime lifecycle events:
// those events are global and can reveal another tenant's model activity.
func backendProgressVisible(cfg config.Config, identity auth.Identity, progress swaputil.BackendProgressEvent) bool {
	if identity.Legacy || len(identity.Models) == 0 {
		return true
	}
	model := strings.TrimSpace(progress.Model)
	return model != "" && modelAllowedForIdentity(cfg, identity, model)
}

func activityEventVisible(cfg config.Config, identity auth.Identity, activity ActivityLogEntry) bool {
	if identity.Legacy || len(identity.Models) == 0 {
		return true
	}
	return modelAllowedForIdentity(cfg, identity, activity.Model)
}

func (s *Server) handleAPIProfiles(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	ids := make([]string, 0, len(cfg.Profiles))
	for id := range cfg.Profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	identity := identityFromContext(r.Context())
	profiles := make([]apiProfile, 0, len(ids))
	for _, id := range ids {
		profile := cfg.Profiles[id]
		pins := make(map[string]string, len(profile.Pins))
		for pin, target := range profile.Pins {
			if modelAllowedForIdentity(cfg, identity, pin) {
				pins[pin] = target
			}
		}
		if !identity.Legacy && len(identity.Models) > 0 && len(pins) == 0 {
			continue
		}
		profiles = append(profiles, apiProfile{
			ID:          id,
			Description: profile.Description,
			Pins:        pins,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"active":   nullableProfile(s.ActiveProfile()),
		"profiles": profiles,
	})
}

func (s *Server) handleAPIActiveProfile(w http.ResponseWriter, r *http.Request) {
	if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
		// Activating a profile changes model routing globally. A model-scoped
		// key cannot express a safe partial activation, so reject it rather
		// than allowing a profile to rewrite an inaccessible model.
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: activating profiles requires an unrestricted key")
		return
	}
	if r.Body == nil || r.Body == http.NoBody {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid profile request")
		return
	}
	var body map[string]json.RawMessage
	// The profile payload contains only a nullable name. Bound it separately
	// from inference bodies so a control-plane caller cannot make the handler
	// retain an arbitrarily large JSON object before routing is changed.
	if err := decodeJSONBody(w, r, &body, maxAPIKeyJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid profile request")
		return
	}
	raw, ok := body["name"]
	if !ok {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "profile name is required")
		return
	}

	var name string
	if string(raw) != "null" {
		if err := json.Unmarshal(raw, &name); err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "profile name must be a string or null")
			return
		}
	}
	if _, err := s.setActiveProfile(name); err != nil {
		swaputil.SendResponse(w, r, http.StatusNotFound, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"active": nullableProfile(s.ActiveProfile())})
}

// modelStatus returns every configured model joined with its current process
// state (defaulting to "stopped"), followed by peer models.
func (s *Server) modelStatus() []apiModel {
	cfg := s.currentConfig()
	running := s.local.RunningModels()
	lifecycle := s.modelLifecycleStatuses()
	reconcile := s.configReconciler.Status()

	ids := make([]string, 0, len(cfg.Models)+len(lifecycle))
	seen := make(map[string]struct{}, len(cfg.Models)+len(lifecycle))
	for id := range cfg.Models {
		ids = append(ids, id)
		seen[id] = struct{}{}
	}
	for id := range lifecycle {
		if _, exists := seen[id]; !exists {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	models := make([]apiModel, 0, len(ids))
	for _, id := range ids {
		mc := cfg.Models[id]
		state := "stopped"
		if st, ok := running[id]; ok {
			state = string(st)
		}
		if s.modelSleeping(id) {
			state = string(process.StateSleeping)
		}
		_, capsMap, _, ctxLen := renderCapabilities(mc.Capabilities)
		modelLifecycle := lifecycle[id]
		configStatus := modelLifecycle.ConfigStatus
		if configStatus == "" {
			configStatus = scheduler.ConfigStatusApplied
		}
		appliedRev := reconcile.ActiveRevision
		desiredRev := reconcile.DesiredRevision
		if modelLifecycle.AppliedRevision > 0 {
			appliedRev = modelLifecycle.AppliedRevision
		}
		if modelLifecycle.DesiredRevision > 0 {
			desiredRev = modelLifecycle.DesiredRevision
		}
		models = append(models, apiModel{
			Id:          id,
			Name:        mc.Name,
			Description: mc.Description,
			State:       state,
			BackendType: strings.TrimSpace(mc.Backend.Type),
			Unlisted:    mc.Unlisted,
			Disabled:    mc.Disabled,
			// The lifecycle flag is only meaningful while the model is still
			// configured as servable; a per-model disabled flag is the
			// operator's own maintenance mode and wins the naming.
			Maintenance:   !mc.Disabled && modelLifecycle.Maintenance,
			Aliases:       mc.Aliases,
			Capabilities:  capsMap,
			ContextLength: ctxLen,
			ConfigStatus:  configStatus,
			AppliedRev:    appliedRev,
			DesiredRev:    desiredRev,
			OldRequests:   modelLifecycle.OldRequests,
			Waiting:       modelLifecycle.WaitingRequests,
			ConfigError:   publicConfigLifecycleError(modelLifecycle.Error),
		})
	}

	for peerID, peer := range cfg.Peers {
		for _, modelID := range peer.Models {
			models = append(models, apiModel{
				Id:           config.PeerModelFQN(peerID, modelID),
				PeerID:       peerID,
				ConfigStatus: scheduler.ConfigStatusApplied,
				AppliedRev:   reconcile.ActiveRevision,
				DesiredRev:   reconcile.DesiredRevision,
			})
		}
	}

	return models
}

// handleAPIUnloadAll stops every running local process.
func (s *Server) handleAPIUnloadAll(w http.ResponseWriter, r *http.Request) {
	if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: unloading all models requires an unrestricted key")
		return
	}
	s.local.Unload(0)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"msg": "ok"})
}

// handleAPIModelLoadConflicts previews the models that the active local
// topology would stop before loading the requested model. The preview is
// advisory: the router makes the authoritative eviction decision again when
// the request is actually admitted.
func (s *Server) handleAPIModelLoadConflicts(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	requested := strings.TrimPrefix(r.PathValue("model"), "/")
	modelID, found := cfg.RealModelName(requested)
	if !found || s.local == nil || !s.local.Handles(modelID) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, modelID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+requested)
		return
	}

	conflicts := make([]apiModelLoadConflict, 0)
	if provider, ok := s.local.(interface{ LoadConflicts(string) []string }); ok {
		running := s.local.RunningModels()
		seen := make(map[string]struct{})
		for _, conflictID := range provider.LoadConflicts(modelID) {
			conflictID = strings.TrimSpace(conflictID)
			if conflictID == "" || conflictID == modelID {
				continue
			}
			if _, duplicate := seen[conflictID]; duplicate {
				continue
			}
			state, active := running[conflictID]
			if !active || !modelAllowedForIdentity(cfg, identityFromContext(r.Context()), conflictID) {
				continue
			}
			name := cfg.Models[conflictID].Name
			if strings.TrimSpace(name) == "" {
				name = conflictID
			}
			seen[conflictID] = struct{}{}
			conflicts = append(conflicts, apiModelLoadConflict{
				ID:    conflictID,
				Name:  name,
				State: string(state),
			})
		}
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":     modelID,
		"conflicts": conflicts,
	})
}

// handleAPIUnloadModel stops a single named local process.
func (s *Server) handleAPIUnloadModel(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	requested := strings.TrimPrefix(r.PathValue("model"), "/")
	realName, found := cfg.RealModelName(requested)
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	// The model-unload scope protects the operation itself, but a managed key
	// may also be restricted to an allowlist of models. Require the same
	// canonical/alias authorization used by inference and backend controls
	// before touching the local router; otherwise a model-scoped key could
	// unload an unrelated model by naming it in the path.
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, realName) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+requested)
		return
	}
	if !s.local.Handles(realName) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "no local server found for requested model")
		return
	}
	s.local.Unload(0, realName)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("OK"))
}

// handleAPIModelRestart confirms a pending process-configuration change. The
// request is deliberately scoped like unload: it can stop and recreate a
// local inference process, but never changes the daemon or a peer endpoint.
func (s *Server) handleAPIModelRestart(w http.ResponseWriter, r *http.Request) {
	s.handleAPIModelRestartRequest(w, r, false)
}

// handleAPIModelForceRestart confirms a pending restart and drops the model's
// currently tracked requests after the router has fenced new admissions.
func (s *Server) handleAPIModelForceRestart(w http.ResponseWriter, r *http.Request) {
	s.handleAPIModelRestartRequest(w, r, true)
}

func (s *Server) handleAPIModelRestartRequest(w http.ResponseWriter, r *http.Request, force bool) {
	cfg := s.currentConfig()
	requested := strings.TrimPrefix(r.PathValue("model"), "/")
	modelID, found := cfg.RealModelName(requested)
	if !found || !s.local.Handles(modelID) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		return
	}
	if identity := identityFromContext(r.Context()); !modelAllowedForIdentity(cfg, identity, modelID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+requested)
		return
	}
	var err error
	if force {
		err = s.ReconcileModelForceRestart(modelID)
	} else {
		err = s.ReconcileModelRestart(modelID)
	}
	if err != nil {
		switch {
		case errors.Is(err, scheduler.ErrRestartNotPending):
			swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
		case errors.Is(err, scheduler.ErrModelNotFound):
			swaputil.SendResponse(w, r, http.StatusNotFound, "model not found")
		default:
			swaputil.SendResponse(w, r, http.StatusServiceUnavailable, err.Error())
		}
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":  modelID,
		"status": "accepted",
	})
}

// handleAPIActivity serves paginated activity table rows.
func (s *Server) handleAPIActivity(w http.ResponseWriter, r *http.Request) {
	query, err := parseActivityQuery(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	configuredOnly, err := parseActivityBool(r, "configured_only")
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if query.KeyID != "" && !keyUsageAllowedForIdentity(identityFromContext(r.Context()), query.KeyID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key cannot read another key's activity")
		return
	}
	models, empty, scopeErr := scopedActivityModels(s.currentConfig(), identityFromContext(r.Context()), query.Models, configuredOnly)
	if scopeErr != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, scopeErr.Error())
		return
	}
	if empty {
		query.Models = nil
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.ActivityPage{Data: []store.ActivityLogEntry{}, Page: query.Page, Limit: query.Limit})
		return
	}
	query.Models = models
	page, err := s.store.ListActivity(r.Context(), query)
	if err != nil {
		// Every other handler in this package returns err.Error(); here the
		// client gets a fixed sentence, so log the cause or a SQLite failure
		// leaves no trace anywhere.
		s.proxylog.Errorf("list activity: %v", err)
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "failed to get activity")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(page)
}

// handleAPIActivityStats serves aggregate activity statistics and histograms.
func (s *Server) handleAPIActivityStats(w http.ResponseWriter, r *http.Request) {
	start, err := parseActivityTime(r, "start")
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	end, err := parseActivityTime(r, "end")
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !start.IsZero() && !end.IsZero() && start.After(end) {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "start must be before end")
		return
	}
	minID, err := parseActivityID(r, "min_id")
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	maxID, err := parseActivityID(r, "max_id")
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if minID > 0 && maxID > 0 && minID > maxID {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "min_id must be <= max_id")
		return
	}
	configuredOnly, err := parseActivityBool(r, "configured_only")
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	keyID := strings.TrimSpace(r.URL.Query().Get("key_id"))
	if keyID != "" && !keyUsageAllowedForIdentity(identityFromContext(r.Context()), keyID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key cannot read another key's activity stats")
		return
	}
	requestedModels := append([]string(nil), r.URL.Query()["model"]...)
	if len(requestedModels) == 0 {
		if model := strings.TrimSpace(r.URL.Query().Get("model")); model != "" {
			requestedModels = []string{model}
		}
	}
	models, empty, scopeErr := scopedActivityModels(s.currentConfig(), identityFromContext(r.Context()), requestedModels, configuredOnly)
	if scopeErr != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, scopeErr.Error())
		return
	}
	if empty {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.ActivityStats{})
		return
	}
	stats, err := s.store.ActivityStats(r.Context(), store.ActivityStatsQuery{
		Models:    models,
		KeyID:     keyID,
		SessionID: strings.TrimSpace(r.URL.Query().Get("session_id")),
		Start:     start,
		End:       end,
		MinID:     minID,
		MaxID:     maxID,
	})
	if err != nil {
		s.proxylog.Errorf("activity stats: %v", err)
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "failed to get activity stats")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(stats)
}

// handleAPIMetricsSpeed serves the activity page's inference-speed views: a
// per-model time series of prefill/decode rates and first-token latency, plus
// the same metrics aggregated by prompt (context) length.
func (s *Server) handleAPIMetricsSpeed(w http.ResponseWriter, r *http.Request) {
	filter, empty, err := s.parseActivityStatsFilter(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if empty {
		// A scoped-but-empty view must not fall back to every model's traffic,
		// so answer with an empty report instead of running the query.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(store.SpeedReport{
			Series:  []store.SpeedSeries{},
			Context: []store.ContextSeries{},
		})
		return
	}
	bucketSeconds, err := parseActivityBucket(r, "bucket")
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	report, err := s.store.ActivitySpeedReport(r.Context(), store.ActivitySpeedQuery{
		ActivityFilter: filter,
		BucketSeconds:  bucketSeconds,
	})
	if err != nil {
		s.proxylog.Errorf("activity speed report: %v", err)
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "failed to get activity speed report")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(report)
}

// parseActivityStatsFilter reads the filter params shared by the stats and
// speed endpoints and resolves them against the caller's identity: key_id and
// model scoping follow the activity table's rules so a key never reads another
// key's or another model's traffic. The empty result means the caller may read
// activity but no row matches the requested scope.
func (s *Server) parseActivityStatsFilter(r *http.Request) (store.ActivityFilter, bool, error) {
	start, err := parseActivityTime(r, "start")
	if err != nil {
		return store.ActivityFilter{}, false, err
	}
	end, err := parseActivityTime(r, "end")
	if err != nil {
		return store.ActivityFilter{}, false, err
	}
	if !start.IsZero() && !end.IsZero() && start.After(end) {
		return store.ActivityFilter{}, false, fmt.Errorf("start must be before end")
	}
	minID, err := parseActivityID(r, "min_id")
	if err != nil {
		return store.ActivityFilter{}, false, err
	}
	maxID, err := parseActivityID(r, "max_id")
	if err != nil {
		return store.ActivityFilter{}, false, err
	}
	if minID > 0 && maxID > 0 && minID > maxID {
		return store.ActivityFilter{}, false, fmt.Errorf("min_id must be <= max_id")
	}
	keyID := strings.TrimSpace(r.URL.Query().Get("key_id"))
	if keyID != "" && !keyUsageAllowedForIdentity(identityFromContext(r.Context()), keyID) {
		return store.ActivityFilter{}, false, fmt.Errorf("forbidden: API key cannot read another key's activity stats")
	}
	configuredOnly, err := parseActivityBool(r, "configured_only")
	if err != nil {
		return store.ActivityFilter{}, false, err
	}
	requestedModels := append([]string(nil), r.URL.Query()["model"]...)
	if len(requestedModels) == 0 {
		if model := strings.TrimSpace(r.URL.Query().Get("model")); model != "" {
			requestedModels = []string{model}
		}
	}
	models, empty, scopeErr := scopedActivityModels(s.currentConfig(), identityFromContext(r.Context()), requestedModels, configuredOnly)
	if scopeErr != nil {
		return store.ActivityFilter{}, false, fmt.Errorf("%s", scopeErr.Error())
	}
	return store.ActivityFilter{
		Models:    models,
		KeyID:     keyID,
		SessionID: strings.TrimSpace(r.URL.Query().Get("session_id")),
		Start:     start,
		End:       end,
		MinID:     minID,
		MaxID:     maxID,
	}, empty, nil
}

// parseActivityBucket reads the optional bucket width in seconds. Zero (the
// default) lets the store pick the width from the requested span; explicit
// values still have to fit the store's own bounds.
func parseActivityBucket(r *http.Request, param string) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(param))
	if raw == "" {
		return 0, nil
	}
	seconds, err := strconv.Atoi(raw)
	if err != nil || seconds < 0 {
		return 0, fmt.Errorf("%s must be a non-negative number of seconds", param)
	}
	return seconds, nil
}

func parseActivityLimit(raw string) (int, error) {
	limit, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("invalid limit")
	}
	if limit > 0 && limit < 1000 {
		return limit, nil
	}
	return 0, fmt.Errorf("limit must be between 1 and 999")
}

// parseActivityTime reads an optional RFC3339 timestamp param, matching the
// ?after= convention used by handleAPIPerformance. A missing param is the zero
// time, which the store treats as unbounded. The UI does not send these; they
// exist for direct API consumers.
func parseActivityTime(r *http.Request, param string) (time.Time, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(param))
	if raw == "" {
		return time.Time{}, nil
	}
	parsed, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("invalid '%s' timestamp, use RFC3339 format", param)
	}
	return parsed, nil
}

// parseActivityID reads an optional row id bound. A missing param is 0, which
// the store treats as unbounded.
func parseActivityID(r *http.Request, param string) (int, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(param))
	if raw == "" {
		return 0, nil
	}
	id, err := strconv.Atoi(raw)
	if err != nil || id < 1 {
		return 0, fmt.Errorf("%s must be >= 1", param)
	}
	return id, nil
}

func parseActivityBool(r *http.Request, param string) (bool, error) {
	raw := strings.TrimSpace(r.URL.Query().Get(param))
	if raw == "" {
		return false, nil
	}
	switch strings.ToLower(raw) {
	case "1", "true", "yes":
		return true, nil
	case "0", "false", "no":
		return false, nil
	default:
		return false, fmt.Errorf("%s must be true or false", param)
	}
}

func parseActivityQuery(r *http.Request) (store.ActivityQuery, error) {
	const defaultLimit = 25
	query := store.ActivityQuery{
		Limit: defaultLimit,
		Page:  1,
	}

	// model repeats to filter on several models at once (?model=a&model=b).
	// A single ?model=x stays a plain exact match.
	for _, raw := range r.URL.Query()["model"] {
		if model := strings.TrimSpace(raw); model != "" {
			query.Models = append(query.Models, model)
		}
	}
	query.KeyID = strings.TrimSpace(r.URL.Query().Get("key_id"))
	query.SessionID = strings.TrimSpace(r.URL.Query().Get("session_id"))

	start, err := parseActivityTime(r, "start")
	if err != nil {
		return store.ActivityQuery{}, err
	}
	end, err := parseActivityTime(r, "end")
	if err != nil {
		return store.ActivityQuery{}, err
	}
	if !start.IsZero() && !end.IsZero() && start.After(end) {
		return store.ActivityQuery{}, fmt.Errorf("start must be before end")
	}
	query.Start, query.End = start, end

	minID, err := parseActivityID(r, "min_id")
	if err != nil {
		return store.ActivityQuery{}, err
	}
	maxID, err := parseActivityID(r, "max_id")
	if err != nil {
		return store.ActivityQuery{}, err
	}
	if minID > 0 && maxID > 0 && minID > maxID {
		return store.ActivityQuery{}, fmt.Errorf("min_id must be <= max_id")
	}
	query.MinID, query.MaxID = minID, maxID

	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		limit, err := parseActivityLimit(raw)
		if err != nil {
			return store.ActivityQuery{}, err
		}
		query.Limit = limit
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("page")); raw != "" {
		page, err := strconv.Atoi(raw)
		if err != nil || page < 1 {
			return store.ActivityQuery{}, fmt.Errorf("page must be >= 1")
		}
		query.Page = page
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("sort")); raw != "" {
		if _, ok := store.ActivitySortColumn(raw); !ok {
			return store.ActivityQuery{}, fmt.Errorf("invalid sort column")
		}
		query.Sort = raw
	}

	if raw := strings.TrimSpace(r.URL.Query().Get("order")); raw != "" {
		switch strings.ToLower(raw) {
		case "asc", "desc":
			query.Order = strings.ToLower(raw)
		default:
			return store.ActivityQuery{}, fmt.Errorf("order must be asc or desc")
		}
	}

	return query, nil
}

// handleAPIPerformance serves the buffered system/GPU stats, optionally
// filtered to samples after the ?after=<RFC3339> timestamp.
func (s *Server) handleAPIPerformance(w http.ResponseWriter, r *http.Request) {
	if s.perf == nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		json.NewEncoder(w).Encode(map[string]bool{"enabled": false})
		return
	}

	sysStats, gpuStats := s.perf.Current()

	if afterStr := r.URL.Query().Get("after"); afterStr != "" {
		after, err := time.Parse(time.RFC3339, afterStr)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid 'after' timestamp, use RFC3339 format")
			return
		}
		filteredSys := make([]perf.SysStat, 0, len(sysStats))
		for _, st := range sysStats {
			if st.Timestamp.After(after) {
				filteredSys = append(filteredSys, st)
			}
		}
		sysStats = filteredSys

		filteredGpu := make([]perf.GpuStat, 0, len(gpuStats))
		for _, g := range gpuStats {
			if g.Timestamp.After(after) {
				filteredGpu = append(filteredGpu, g)
			}
		}
		gpuStats = filteredGpu
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"enabled":   true,
		"sys_stats": sysStats,
		"gpu_stats": gpuStats,
	})
}

// handleAPIVersion serves the build metadata.
func (s *Server) handleAPIVersion(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"version":    s.build.Version,
		"commit":     s.build.Commit,
		"build_date": s.build.Date,
	})
}

// handleAPIHardware serves the hardware snapshot captured at process startup.
func (s *Server) handleAPIHardware(w http.ResponseWriter, r *http.Request) {
	if s.hardware == nil {
		swaputil.SendResponse(w, r, http.StatusServiceUnavailable, "hardware detection unavailable")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(s.hardware); err != nil {
		s.proxylog.Warnf("failed to encode hardware snapshot: %v", err)
	}
}

// handleAPICancelInflight cancels an active model-dispatched request by its
// inflight ID. Normal request cleanup removes the row and emits the update.
func (s *Server) handleAPICancelInflight(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	id := r.PathValue("id")
	if identity := identityFromContext(r.Context()); !identity.Legacy && len(identity.Models) > 0 {
		found := false
		for _, request := range s.inflight.Current().Requests {
			if request.ID != id {
				continue
			}
			found = true
			if !modelAllowedForIdentity(cfg, identity, request.Model) {
				swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+request.Model)
				return
			}
			break
		}
		if !found {
			swaputil.SendResponse(w, r, http.StatusNotFound, "inflight request not found")
			return
		}
	}
	if id == "" || !s.inflight.Cancel(id) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "inflight request not found")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"msg": "ok"})
}

type messageType string

const (
	msgTypeModelStatus     messageType = "modelStatus"
	msgTypeLogData         messageType = "logData"
	msgTypeActivity        messageType = "activity"
	msgTypeInFlight        messageType = "inflight"
	msgTypeUIConfig        messageType = "uiConfig"
	msgTypeProfile         messageType = "profileChanged"
	msgTypeBackendProgress messageType = "backendProgress"
)

// sendDropReportInterval is how often handleAPIEvents reports messages that
// were dropped because the send buffer was full.
const sendDropReportInterval = 5 * time.Second

type messageEnvelope struct {
	Type messageType `json:"type"`
	Data string      `json:"data"`
}

// handleAPIEvents streams server events (model status, log data, metrics,
// in-flight counts) to the client as Server-Sent Events.
func (s *Server) handleAPIEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// prevent nginx from buffering SSE
	w.Header().Set("X-Accel-Buffering", "no")

	flusher, ok := w.(http.Flusher)
	if !ok {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// internal/event already has a 50K event buffer
	// a 1K message buffer should be enough, watch the logs for the warning that the sendBuffer is full
	sendBuffer := make(chan messageEnvelope, 1024)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	identity := identityFromContext(r.Context())

	// Dropped messages are counted and reported at most once per interval.
	// Logging every drop floods the logs because each warning becomes a log
	// event that is sent over this same buffer, which drops again.
	dropped := newSuppressionCounter(sendDropReportInterval)
	cancelled := newSuppressionCounter(sendDropReportInterval)
	reportDropped := func(n int) {
		s.proxylog.Warnf("handleAPIEvents sendBuffer full, %d messages suppressed", n)
	}
	reportCancelled := func(n int) {
		s.proxylog.Warnf("handleAPIEvents send suppressed due to context done, %d messages suppressed", n)
	}
	// runs after the event handlers below are unsubscribed so any remaining
	// counts are reported before the connection goes away
	defer func() {
		if n, ok := dropped.Flush(); ok {
			reportDropped(n)
		}
		if n, ok := cancelled.Flush(); ok {
			reportCancelled(n)
		}
	}()

	send := func(msg messageEnvelope) {
		select {
		case sendBuffer <- msg:
		case <-ctx.Done():
			if n, ok := cancelled.Add(); ok {
				reportCancelled(n)
			}
		default:
			if n, ok := dropped.Add(); ok {
				reportDropped(n)
			}
		}
	}
	sendModels := func() {
		cfg := s.currentConfig()
		models := s.modelStatus()
		if !identity.Legacy && len(identity.Models) > 0 {
			visible := models[:0]
			for _, model := range models {
				if modelAllowedForIdentity(cfg, identity, model.Id) {
					visible = append(visible, model)
				}
			}
			models = visible
		}
		if data, err := json.Marshal(models); err == nil {
			send(messageEnvelope{Type: msgTypeModelStatus, Data: string(data)})
		}
	}
	sendLogData := func(source string, data []byte) {
		// A model-scoped key cannot safely receive the combined proxy/upstream
		// history: log lines do not carry a reliable model boundary and may
		// contain prompts or paths for another backend. Keep the model/inflight
		// event stream useful while suppressing this global side channel.
		if !identity.Legacy && len(identity.Models) > 0 {
			return
		}
		if j, err := json.Marshal(map[string]string{"source": source, "data": string(data)}); err == nil {
			send(messageEnvelope{Type: msgTypeLogData, Data: string(j)})
		}
	}
	sendActivity := func(activity ActivityLogEntry) {
		if !activityEventVisible(s.currentConfig(), identity, activity) {
			// Activity events expose an id that can be used to request the raw
			// capture. Do not leak ids for another model to a model-scoped key,
			// even though the capture endpoint performs its own authorization.
			return
		}
		if j, err := json.Marshal(map[string]int{"id": activity.ID}); err == nil {
			send(messageEnvelope{Type: msgTypeActivity, Data: string(j)})
		}
	}
	sendInFlight := func(update swaputil.InFlightRequestsEvent) {
		cfg := s.currentConfig()
		if !identity.Legacy && len(identity.Models) > 0 {
			if update.Request != nil && !modelAllowedForIdentity(cfg, identity, update.Request.Model) {
				return
			}
			if len(update.Requests) > 0 {
				visible := update.Requests[:0]
				for _, request := range update.Requests {
					if modelAllowedForIdentity(cfg, identity, request.Model) {
						visible = append(visible, request)
					}
				}
				update.Requests = visible
			}
		}
		if update.Operation == inflightOperationSnapshot && update.Requests == nil {
			update.Requests = []swaputil.InflightRequestEntry{}
		}
		if j, err := json.Marshal(update); err == nil {
			send(messageEnvelope{Type: msgTypeInFlight, Data: string(j)})
		}
	}
	sendUIConfig := func() {
		if j, err := json.Marshal(s.currentConfig().UI); err == nil {
			send(messageEnvelope{Type: msgTypeUIConfig, Data: string(j)})
		}
	}
	sendProfile := func() {
		if j, err := json.Marshal(map[string]any{"active": nullableProfile(s.ActiveProfile())}); err == nil {
			send(messageEnvelope{Type: msgTypeProfile, Data: string(j)})
		}
	}
	sendBackendProgress := func(progress swaputil.BackendProgressEvent) {
		normalized, ok := progress.Normalize()
		if !ok {
			return
		}
		progress = normalized
		if !backendProgressVisible(s.currentConfig(), identity, progress) {
			return
		}
		if j, err := json.Marshal(progress); err == nil {
			send(messageEnvelope{Type: msgTypeBackendProgress, Data: string(j)})
		}
	}

	defer event.On(func(e swaputil.ProcessStateChangeEvent) { sendModels() })()
	defer event.On(func(e swaputil.ConfigFileChangedEvent) { sendModels() })()
	defer event.On(func(e swaputil.ModelStatusesChangedEvent) { sendModels() })()
	defer event.On(func(e swaputil.ProfileChangedEvent) {
		sendProfile()
		sendModels()
	})()
	defer s.proxylog.OnLogData(func(data []byte) { sendLogData("proxy", data) })()
	defer s.upstreamlog.OnLogData(func(data []byte) { sendLogData("upstream", data) })()
	defer event.On(func(e ActivityLogEvent) { sendActivity(e.Metrics) })()
	defer event.On(func(e swaputil.InFlightRequestsEvent) { sendInFlight(e) })()
	defer event.On(func(e swaputil.BackendProgressEvent) { sendBackendProgress(e) })()

	// initial payload
	sendLogData("proxy", s.proxylog.GetHistory())
	sendLogData("upstream", s.upstreamlog.GetHistory())
	sendModels()
	sendUIConfig()
	sendProfile()
	sendInFlight(s.inflight.Current())

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdownCtx.Done():
			return
		case msg := <-sendBuffer:
			data, err := json.Marshal(msg)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event:message\ndata:%s\n\n", data)
			flusher.Flush()
		}
	}
}
