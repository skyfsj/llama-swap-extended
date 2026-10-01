package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modeldownload"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type modelDownloadListResponse struct {
	Data   []store.ModelDownloadTask `json:"data"`
	Limit  int                       `json:"limit"`
	Offset int                       `json:"offset"`
}

func (s *Server) modelDownloadManager() *modeldownload.Manager {
	s.modelFilesMu.RLock()
	defer s.modelFilesMu.RUnlock()
	return s.downloads
}

type modelDownloadCredentialStatus struct {
	Configured            bool   `json:"configured"`
	EnvironmentConfigured bool   `json:"environmentConfigured"`
	EnvironmentVariable   string `json:"environmentVariable"`
}

type modelDownloadCredentialsResponse struct {
	ETag        string                        `json:"etag"`
	Writable    bool                          `json:"writable"`
	HuggingFace modelDownloadCredentialStatus `json:"huggingface"`
	ModelScope  modelDownloadCredentialStatus `json:"modelscope"`
}

type modelDownloadCredentialsPatch struct {
	HFToken         *string `json:"hfToken"`
	ModelScopeToken *string `json:"modelScopeToken"`
}

// handleAPIModelDownloadCredentials exposes only credential presence and the
// configured environment variable name. The token itself is never returned
// to the browser; PATCH accepts a replacement or an empty value to remove the
// configured value and fall back to the environment variable.
func (s *Server) handleAPIModelDownloadCredentials(w http.ResponseWriter, r *http.Request) {
	settings := s.currentConfig().ModelFiles.Downloads.Effective()
	var snapshot *config.ConfigSnapshot
	if s.configManager != nil {
		current, err := s.configManager.Snapshot(r.Context())
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		snapshot = &current
	}
	response := buildModelDownloadCredentialsResponse(settings, snapshot)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func (s *Server) handleAPIUpdateModelDownloadCredentials(w http.ResponseWriter, r *http.Request) {
	if s.configManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "config source manager is not configured")
		return
	}
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required")
		return
	}
	var request modelDownloadCredentialsPatch
	if err := decodeJSONBody(w, r, &request, 64<<10); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "a JSON object with hfToken and/or modelScopeToken is required")
		return
	}
	snapshot, err := s.configManager.Snapshot(r.Context())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	operations, err := modelDownloadCredentialPatch(snapshot.Config, request)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if len(operations) == 0 {
		writeModelDownloadCredentialsResponse(w, s, snapshot)
		return
	}
	patch, err := json.Marshal(operations)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	updated, _, err := s.configManager.ApplyPatch(r.Context(), patch, r.Header.Get("If-Match"))
	if err != nil {
		status := http.StatusBadRequest
		if strings.Contains(err.Error(), "etag conflict") {
			status = http.StatusPreconditionFailed
		}
		swaputil.SendResponse(w, r, status, err.Error())
		return
	}
	writeModelDownloadCredentialsResponse(w, s, updated)
}

func writeModelDownloadCredentialsResponse(w http.ResponseWriter, s *Server, snapshot config.ConfigSnapshot) {
	settings := s.currentConfig().ModelFiles.Downloads.Effective()
	response := buildModelDownloadCredentialsResponse(settings, &snapshot)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

func buildModelDownloadCredentialsResponse(settings config.ModelDownloadsConfig, snapshot *config.ConfigSnapshot) modelDownloadCredentialsResponse {
	response := modelDownloadCredentialsResponse{
		HuggingFace: modelDownloadCredentialStatus{
			Configured:            strings.TrimSpace(settings.HFToken) != "",
			EnvironmentConfigured: environmentCredentialConfigured(settings.HFTokenEnv),
			EnvironmentVariable:   strings.TrimSpace(settings.HFTokenEnv),
		},
		ModelScope: modelDownloadCredentialStatus{
			Configured:            strings.TrimSpace(settings.ModelScopeToken) != "",
			EnvironmentConfigured: environmentCredentialConfigured(settings.ModelScopeTokenEnv),
			EnvironmentVariable:   strings.TrimSpace(settings.ModelScopeTokenEnv),
		},
	}
	if snapshot != nil {
		response.ETag = snapshot.ETag
		response.Writable = snapshot.Writable
	}
	return response
}

func environmentCredentialConfigured(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	return strings.TrimSpace(os.Getenv(name)) != ""
}

type modelDownloadCredentialPatchOperation struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}

func modelDownloadCredentialPatch(raw json.RawMessage, request modelDownloadCredentialsPatch) ([]modelDownloadCredentialPatchOperation, error) {
	var root map[string]any
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &root); err != nil {
			return nil, errors.New("current configuration is not a JSON object")
		}
	}
	modelFiles, modelFilesOK := root["modelFiles"].(map[string]any)
	downloads, downloadsOK := modelFiles["downloads"].(map[string]any)
	if modelFilesOK && modelFiles["downloads"] != nil && !downloadsOK {
		return nil, errors.New("modelFiles.downloads must be an object")
	}

	updates := make([]struct {
		field string
		path  string
		value *string
	}, 0, 2)
	if request.HFToken != nil {
		updates = append(updates, struct {
			field string
			path  string
			value *string
		}{field: "hfToken", path: "/modelFiles/downloads/hfToken", value: request.HFToken})
	}
	if request.ModelScopeToken != nil {
		updates = append(updates, struct {
			field string
			path  string
			value *string
		}{field: "modelScopeToken", path: "/modelFiles/downloads/modelScopeToken", value: request.ModelScopeToken})
	}
	if len(updates) == 0 {
		return nil, errors.New("at least one credential field is required")
	}

	if !modelFilesOK {
		downloadsValue := map[string]string{}
		for _, update := range updates {
			if token := strings.TrimSpace(*update.value); token != "" {
				downloadsValue[update.field] = token
			}
		}
		if len(downloadsValue) == 0 {
			return nil, nil
		}
		return []modelDownloadCredentialPatchOperation{{
			Op: "add", Path: "/modelFiles", Value: map[string]any{"downloads": downloadsValue},
		}}, nil
	}
	if !downloadsOK {
		downloadsValue := map[string]string{}
		for _, update := range updates {
			if token := strings.TrimSpace(*update.value); token != "" {
				downloadsValue[update.field] = token
			}
		}
		if len(downloadsValue) == 0 {
			return nil, nil
		}
		return []modelDownloadCredentialPatchOperation{{Op: "add", Path: "/modelFiles/downloads", Value: downloadsValue}}, nil
	}

	operations := make([]modelDownloadCredentialPatchOperation, 0, len(updates))
	for _, update := range updates {
		token := strings.TrimSpace(*update.value)
		if token == "" {
			if _, exists := downloads[update.field]; exists {
				operations = append(operations, modelDownloadCredentialPatchOperation{Op: "remove", Path: update.path})
			}
			continue
		}
		operations = append(operations, modelDownloadCredentialPatchOperation{Op: "add", Path: update.path, Value: token})
	}
	return operations, nil
}

func (s *Server) handleAPIModelDownloads(w http.ResponseWriter, r *http.Request) {
	manager := s.modelDownloadManager()
	if manager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "model download queue is not configured")
		return
	}
	limit, offset, err := parseModelDownloadPagination(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	tasks, err := manager.List(r.Context(), limit, offset)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(modelDownloadListResponse{Data: tasks, Limit: limit, Offset: offset})
}

type createModelDownloadRequest struct {
	Provider string   `json:"provider"`
	RepoID   string   `json:"repo_id"`
	Revision string   `json:"revision"`
	SourceID string   `json:"source_id"`
	Include  []string `json:"include"`
	Exclude  []string `json:"exclude"`
}

func (s *Server) handleAPICreateModelDownload(w http.ResponseWriter, r *http.Request) {
	manager := s.modelDownloadManager()
	if manager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "model download queue is not configured")
		return
	}
	var request createModelDownloadRequest
	if err := decodeJSONBody(w, r, &request, 1<<20); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "a JSON body with provider, repo_id, and optional revision, include, and exclude is required")
		return
	}
	task, duplicate, err := manager.Enqueue(r.Context(), modeldownload.Request{
		Provider: request.Provider,
		RepoID:   request.RepoID,
		Revision: request.Revision,
		SourceID: request.SourceID,
		Include:  request.Include,
		Exclude:  request.Exclude,
	})
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	status := http.StatusAccepted
	if duplicate {
		status = http.StatusOK
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"task": task, "duplicate": duplicate})
}

func (s *Server) handleAPICancelModelDownload(w http.ResponseWriter, r *http.Request) {
	manager := s.modelDownloadManager()
	if manager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "model download queue is not configured")
		return
	}
	if err := manager.Cancel(r.Context(), strings.TrimSpace(r.PathValue("id"))); err != nil {
		sendModelDownloadMutationError(w, r, err)
		return
	}
	s.writeModelDownloadByID(w, r, manager, r.PathValue("id"))
}

func (s *Server) handleAPIRetryModelDownload(w http.ResponseWriter, r *http.Request) {
	manager := s.modelDownloadManager()
	if manager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "model download queue is not configured")
		return
	}
	if err := manager.Retry(r.Context(), strings.TrimSpace(r.PathValue("id"))); err != nil {
		sendModelDownloadMutationError(w, r, err)
		return
	}
	s.writeModelDownloadByID(w, r, manager, r.PathValue("id"))
}

func (s *Server) handleAPIDeleteModelDownload(w http.ResponseWriter, r *http.Request) {
	manager := s.modelDownloadManager()
	if manager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "model download queue is not configured")
		return
	}
	if err := manager.Delete(r.Context(), strings.TrimSpace(r.PathValue("id"))); err != nil {
		sendModelDownloadMutationError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) writeModelDownloadByID(w http.ResponseWriter, r *http.Request, manager *modeldownload.Manager, id string) {
	task, found, err := manager.Get(r.Context(), strings.TrimSpace(id))
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, store.ErrModelDownloadNotFound.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"task": task})
}

func sendModelDownloadMutationError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, store.ErrModelDownloadNotFound) {
		status = http.StatusNotFound
	} else if errors.Is(err, store.ErrModelDownloadNotTerminal) {
		status = http.StatusConflict
	}
	swaputil.SendResponse(w, r, status, err.Error())
}

func parseModelDownloadPagination(r *http.Request) (int, int, error) {
	limit, offset := 100, 0
	for key, target := range map[string]*int{"limit": &limit, "offset": &offset} {
		value := strings.TrimSpace(r.URL.Query().Get(key))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return 0, 0, errors.New(key + " must be an integer")
		}
		*target = parsed
	}
	if limit < 1 || limit > 500 {
		return 0, 0, errors.New("limit must be between 1 and 500")
	}
	if offset < 0 {
		return 0, 0, errors.New("offset must be >= 0")
	}
	return limit, offset, nil
}
