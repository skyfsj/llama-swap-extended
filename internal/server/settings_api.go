package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/settings"
	"github.com/mostlygeek/llama-swap/internal/settings/spec"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const maxSettingsDraftBody = 16 << 20

type settingsCommitRequest struct {
	Draft       settings.Draft `json:"draft"`
	PreviewHash string         `json:"previewHash"`
}

func (s *Server) handleAPISettingsSchema(w http.ResponseWriter, r *http.Request) {
	if s.settingsManager == nil {
		writeJSON(w, spec.Metadata())
		return
	}
	writeJSON(w, s.settingsManager.Schema())
}

func (s *Server) handleAPISettingsOptions(w http.ResponseWriter, r *http.Request) {
	options := map[string][]map[string]string{
		"log-level":       {{"value": "debug", "label": "debug"}, {"value": "info", "label": "info"}, {"value": "warn", "label": "warn"}, {"value": "error", "label": "error"}},
		"log-time-format": {{"value": "", "label": "default"}, {"value": "rfc3339", "label": "rfc3339"}, {"value": "rfc3339nano", "label": "rfc3339nano"}, {"value": "kitchen", "label": "kitchen"}},
		"log-stdout":      {{"value": "proxy", "label": "proxy"}, {"value": "upstream", "label": "upstream"}, {"value": "both", "label": "both"}, {"value": "none", "label": "none"}},
	}
	if s != nil {
		cfg := s.currentConfig()
		models := make([]string, 0, len(cfg.Models))
		for id := range cfg.Models {
			models = append(models, id)
		}
		sort.Strings(models)
		items := make([]map[string]string, 0, len(models))
		for _, id := range models {
			label := id
			if name := strings.TrimSpace(cfg.Models[id].Name); name != "" {
				label = name
			}
			items = append(items, map[string]string{"value": id, "label": label})
		}
		options["models"] = items
		runtimes := make([]string, 0, len(cfg.Runtimes))
		for id := range cfg.Runtimes {
			runtimes = append(runtimes, id)
		}
		sort.Strings(runtimes)
		runtimeItems := make([]map[string]string, 0, len(runtimes))
		for _, id := range runtimes {
			kind := strings.TrimSpace(cfg.Runtimes[id].Kind)
			label := id
			if kind != "" {
				label += "（" + kind + "）"
			}
			runtimeItems = append(runtimeItems, map[string]string{"value": id, "label": label})
		}
		options["runtimes"] = runtimeItems
	}
	values, ok := options[r.PathValue("provider")]
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "unknown settings option provider")
		return
	}
	writeJSON(w, map[string]any{"items": values})
}

func (s *Server) handleAPISettingsPermission(w http.ResponseWriter, r *http.Request) {
	identity := identityFromContext(r.Context())
	modelScoped := isModelScoped(identity)
	writeJSON(w, map[string]any{
		"read":    true,
		"write":   !modelScoped || len(identity.Models) > 0,
		"raw":     !modelScoped,
		"history": !modelScoped,
		"models":  append([]string(nil), identity.Models...),
	})
}

func (s *Server) handleAPISettingsConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.settingsManager == nil {
		data, yamlData, err := publicConfigSnapshot(s.currentConfig())
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		writeJSON(w, settings.Snapshot{Config: data, YAML: yamlData, Writable: false, SchemaVersion: 1})
		return
	}
	snapshot, err := s.settingsManager.Snapshot(r.Context())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	identity := identityFromContext(r.Context())
	if isModelScoped(identity) {
		if err := s.filterSettingsSnapshot(&snapshot, identity); err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
	}
	w.Header().Set("ETag", snapshot.ETag)
	writeJSON(w, snapshot)
}

func (s *Server) handleAPISettingsPreview(w http.ResponseWriter, r *http.Request) {
	if s.settingsManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "settings manager is not configured")
		return
	}
	var draft settings.Draft
	if err := decodeJSONBody(w, r, &draft, maxSettingsDraftBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid settings draft")
		return
	}
	if err := s.authorizeSettingsDraft(identityFromContext(r.Context()), draft); err != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, err.Error())
		return
	}
	preview, err := s.settingsManager.Preview(r.Context(), draft)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if !preview.Valid {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(preview)
		return
	}
	writeJSON(w, preview)
}

func (s *Server) handleAPISettingsCommit(w http.ResponseWriter, r *http.Request) {
	if s.settingsManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "settings manager is not configured")
		return
	}
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required")
		return
	}
	var raw json.RawMessage
	if err := decodeJSONBody(w, r, &raw, maxSettingsDraftBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid settings commit request")
		return
	}
	var request settingsCommitRequest
	if err := json.Unmarshal(raw, &request); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid settings commit request")
		return
	}
	// Accept the compact form {mode, changes, previewHash} as well as the
	// explicit {draft:{...}, previewHash} envelope used by the UI.
	if request.Draft.Mode == "" {
		var direct settings.Draft
		if err := json.Unmarshal(raw, &direct); err == nil {
			request.Draft = direct
		}
	}
	identity := identityFromContext(r.Context())
	if err := s.authorizeSettingsDraft(identity, request.Draft); err != nil {
		swaputil.SendResponse(w, r, http.StatusForbidden, err.Error())
		return
	}
	actorID := strings.TrimSpace(identity.ID)
	if actorID == "" {
		actorID = "anonymous"
	}
	result, err := s.settingsManager.Commit(r.Context(), request.Draft, r.Header.Get("If-Match"), request.PreviewHash, settings.Actor{ID: actorID, Origin: "ui"})
	if err != nil {
		message := err.Error()
		status := http.StatusBadRequest
		if strings.Contains(message, "etag conflict") || strings.Contains(message, "preview hash conflict") {
			status = http.StatusPreconditionFailed
		}
		swaputil.SendResponse(w, r, status, message)
		return
	}
	w.Header().Set("ETag", result.Snapshot.ETag)
	writeJSON(w, result)
}

func (s *Server) handleAPISettingsHistory(w http.ResponseWriter, r *http.Request) {
	if s.settingsManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "settings manager is not configured")
		return
	}
	if isModelScoped(identityFromContext(r.Context())) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: model-scoped keys cannot access global settings history")
		return
	}
	limit := 100
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		parsed, err := strconv.Atoi(value)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid history limit")
			return
		}
		limit = parsed
	}
	history, err := s.settingsManager.History(r.Context(), limit)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	public := make([]settings.Revision, 0, len(history))
	for _, revision := range history {
		revision.Sources = nil
		public = append(public, revision)
	}
	writeJSON(w, map[string]any{"items": public})
}

func (s *Server) handleAPISettingsRevision(w http.ResponseWriter, r *http.Request) {
	if s.settingsManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "settings manager is not configured")
		return
	}
	if isModelScoped(identityFromContext(r.Context())) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: model-scoped keys cannot access global settings history")
		return
	}
	revision, err := s.settingsManager.Revision(r.Context(), r.PathValue("revision"))
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		swaputil.SendResponse(w, r, status, err.Error())
		return
	}
	revision.Sources = nil
	writeJSON(w, revision)
}

func isModelScoped(identity auth.Identity) bool {
	return !identity.Legacy && len(identity.Models) > 0
}

func (s *Server) authorizeSettingsDraft(identity auth.Identity, draft settings.Draft) error {
	if !isModelScoped(identity) {
		return nil
	}
	if draft.Mode != "" && draft.Mode != settings.ModeStructured {
		return fmt.Errorf("forbidden: model-scoped keys may only edit structured model fields")
	}
	for _, change := range draft.Changes {
		path := strings.TrimSpace(change.Path)
		parts := strings.Split(strings.TrimPrefix(path, "/"), "/")
		if len(parts) < 2 || parts[0] != "models" || !modelAllowedForIdentity(s.currentConfig(), identity, unescapeJSONPointer(parts[1])) {
			return fmt.Errorf("forbidden: model-scoped key cannot edit %s", path)
		}
	}
	return nil
}

func unescapeJSONPointer(value string) string {
	value = strings.ReplaceAll(value, "~1", "/")
	return strings.ReplaceAll(value, "~0", "~")
}

func (s *Server) filterSettingsSnapshot(snapshot *settings.Snapshot, identity auth.Identity) error {
	var value map[string]any
	if err := json.Unmarshal(snapshot.Config, &value); err != nil {
		return err
	}
	models, ok := value["models"].(map[string]any)
	filtered := make(map[string]any)
	if !ok {
		value = map[string]any{"models": filtered}
	} else {
		for model := range models {
			if modelAllowedForIdentity(s.currentConfig(), identity, model) {
				filtered[model] = models[model]
			}
		}
		// Model-scoped identities receive only their model records. Global
		// routing, peers, profiles and raw source data are never disclosed.
		value = map[string]any{"models": filtered}
	}
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	snapshot.Config = data
	// A model-scoped view is intentionally not a source editor and must not
	// expose the global merged YAML or source ownership map.
	snapshot.YAML = ""
	snapshot.Sources = nil
	snapshot.Ownership = nil
	snapshot.Writable = false
	return nil
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
