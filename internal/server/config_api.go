package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
	"gopkg.in/yaml.v3"
)

const maxConfigPatchJSONBody = 8 << 20
const maxConfigYAMLBody = 8 << 20
const maxModelConfigDeleteJSONBody = 64 << 10

type configYAMLRequest struct {
	YAML string `json:"yaml"`
}

type deleteModelConfigRequest struct {
	DeleteModelFile bool `json:"delete_model_file"`
}

type deleteModelConfigResponse struct {
	config.ConfigSnapshot
	Deleted           string   `json:"deleted"`
	DeletedModelFiles []string `json:"deleted_model_files,omitempty"`
	// Pruned lists the routing blocks the cascade adjusted for the removed
	// model's dangling references, so the UI can say what changed on the
	// operator's behalf instead of leaving them to find it.
	Pruned []string `json:"pruned,omitempty"`
}

func (s *Server) handleAPIConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if s.configManager == nil {
		// A server embedded without source paths still gets a useful, read-only
		// snapshot. It is explicitly marked non-writable instead of pretending
		// an in-memory mutation was persisted.
		data, yamlData, err := publicConfigSnapshot(cfg)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"config":          json.RawMessage(data),
			"yaml":            yamlData,
			"writable":        false,
			"etag":            "",
			"ownership":       map[string]string{},
			"restartRequired": len(s.daemonRestartPathsSnapshot()) > 0,
			"restartPaths":    s.daemonRestartPathsSnapshot(),
		})
		return
	}
	snapshot, err := s.configManager.Snapshot(r.Context())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	writeConfigSnapshot(w, s.decorateConfigSnapshot(snapshot))
}

func (s *Server) handleAPIConfigValidate(w http.ResponseWriter, r *http.Request) {
	if s.configManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "config source manager is not configured")
		return
	}
	var raw json.RawMessage
	if err := decodeJSONBody(w, r, &raw, maxConfigPatchJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid RFC 6902 patch")
		return
	}
	result, err := s.configManager.ValidatePatch(r.Context(), raw)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (s *Server) handleAPIConfigPatch(w http.ResponseWriter, r *http.Request) {
	if s.configManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "config source manager is not configured")
		return
	}
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required")
		return
	}
	var raw json.RawMessage
	if err := decodeJSONBody(w, r, &raw, maxConfigPatchJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid RFC 6902 patch")
		return
	}
	snapshot, validation, err := s.configManager.ApplyPatch(r.Context(), raw, r.Header.Get("If-Match"))
	if err != nil {
		if strings.Contains(err.Error(), "etag conflict") {
			swaputil.SendResponse(w, r, http.StatusPreconditionFailed, err.Error())
		} else {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		}
		return
	}
	if !validation.Valid {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(validation)
		return
	}
	writeConfigSnapshot(w, s.decorateConfigSnapshot(snapshot))
}

// handleAPIDeleteModelConfig removes one configured local model. The optional
// model-file cleanup is deliberately part of the same config-admin operation:
// the server resolves the files from the current model definition and applies
// the same shared/in-use checks as the standalone model-file endpoint before
// changing either resource.
func (s *Server) handleAPIDeleteModelConfig(w http.ResponseWriter, r *http.Request) {
	if s.configManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "config source manager is not configured")
		return
	}
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required")
		return
	}

	var request deleteModelConfigRequest
	if r.Body != nil && r.Body != http.NoBody {
		if err := decodeJSONBody(w, r, &request, maxModelConfigDeleteJSONBody); err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid model configuration delete request")
			return
		}
	}

	cfg := s.currentConfig()
	requestedID := strings.TrimPrefix(strings.TrimSpace(r.PathValue("model")), "/")
	modelID, found := cfg.RealModelName(requestedID)
	if !found {
		swaputil.SendResponse(w, r, http.StatusNotFound, "model configuration not found")
		return
	}
	if !modelAllowedForIdentity(cfg, identityFromContext(r.Context()), modelID) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+requestedID)
		return
	}

	var modelFiles []modelmanager.File
	if request.DeleteModelFile {
		var err error
		modelFiles, err = s.associatedModelFiles(r.Context(), cfg, modelID)
		if err != nil {
			swaputil.SendResponse(w, r, http.StatusConflict, err.Error())
			return
		}
		if len(modelFiles) == 0 {
			writeModelConfigDeleteConflict(w, r, "no managed model file is associated with this model", nil, nil, nil)
			return
		}
		registered, inUse := modelFileDeleteConflicts(modelID, modelFiles)
		if len(registered) > 0 || len(inUse) > 0 {
			writeModelConfigDeleteConflict(w, r, "associated model files are shared or currently in use", modelFiles, registered, inUse)
			return
		}
	}

	patch, err := json.Marshal([]config.PatchOp{{
		Op:   "remove",
		Path: "/models/" + jsonPointerToken(modelID),
	}})
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	snapshot, validation, err := s.configManager.ApplyPatch(r.Context(), patch, r.Header.Get("If-Match"))
	if err != nil {
		if strings.Contains(err.Error(), "etag conflict") {
			swaputil.SendResponse(w, r, http.StatusPreconditionFailed, err.Error())
		} else {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		}
		return
	}
	if !validation.Valid {
		if len(validation.Issues) > 0 {
			swaputil.SendResponse(w, r, http.StatusBadRequest, validation.Issues[0].Message)
		} else {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid model configuration delete request")
		}
		return
	}

	deletedFiles := make([]string, 0, len(modelFiles))
	if request.DeleteModelFile {
		if s.modelIsRunning(modelID) {
			writeModelConfigDeleteConflict(w, r, "model configuration was deleted, but its file cleanup was skipped because the model is now in use", modelFiles, nil, []string{modelID})
			return
		}
		s.modelFilesMu.Lock()
		manager := s.modelFiles
		if manager == nil {
			s.modelFilesMu.Unlock()
			writeModelConfigDeleteConflict(w, r, "model configuration was deleted, but the model file manager is unavailable", modelFiles, nil, nil)
			return
		}
		for _, file := range modelFiles {
			if _, err := manager.Delete(r.Context(), file.SourceID, file.ID, file.Path); err != nil {
				s.modelFilesMu.Unlock()
				writeModelConfigDeleteConflict(w, r, fmt.Sprintf("model configuration was deleted, but model file cleanup failed: %v", err), modelFiles, nil, nil)
				return
			}
			deletedFiles = append(deletedFiles, file.Path)
		}
		s.modelFilesMu.Unlock()
	}

	writeModelConfigDeleteResponse(w, s.decorateConfigSnapshot(snapshot), modelID, deletedFiles, validation.Pruned)
}

func jsonPointerToken(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}

func (s *Server) associatedModelFiles(ctx context.Context, cfg config.Config, modelID string) ([]modelmanager.File, error) {
	s.modelFilesMu.RLock()
	manager := s.modelFiles
	if manager == nil {
		s.modelFilesMu.RUnlock()
		return nil, errors.New("model file manager is not configured")
	}
	catalog, err := manager.List(ctx, modelmanager.ListOptions{Limit: modelmanager.MaxListLimit})
	s.modelFilesMu.RUnlock()
	if err != nil {
		return nil, fmt.Errorf("could not inspect associated model files: %w", err)
	}
	if catalog.Truncated || catalog.Total > len(catalog.Data) {
		return nil, errors.New("model file catalog is incomplete; refresh the catalog and try again")
	}

	files := make([]modelmanager.File, 0)
	seen := make(map[string]struct{}, len(catalog.Data))
	for _, file := range catalog.Data {
		registered := modelmanager.RegisteredModels(cfg, file.Path)
		if !containsString(registered, modelID) {
			continue
		}
		if _, exists := seen[file.Path]; exists {
			continue
		}
		seen[file.Path] = struct{}{}
		file.Registered = registered
		file.InUse = s.modelsUsingFile(registered)
		files = append(files, file)
	}
	return files, nil
}

func modelFileDeleteConflicts(modelID string, files []modelmanager.File) ([]string, []string) {
	registeredSet := make(map[string]struct{})
	inUseSet := make(map[string]struct{})
	for _, file := range files {
		for _, other := range file.Registered {
			if other != modelID {
				registeredSet[other] = struct{}{}
			}
		}
		for _, model := range file.InUse {
			inUseSet[model] = struct{}{}
		}
	}
	registered := sortedStringSet(registeredSet)
	inUse := sortedStringSet(inUseSet)
	return registered, inUse
}

func sortedStringSet(values map[string]struct{}) []string {
	result := make([]string, 0, len(values))
	for value := range values {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func (s *Server) modelIsRunning(modelID string) bool {
	if s == nil || s.local == nil {
		return false
	}
	_, running := s.local.RunningModels()[modelID]
	return running
}

func writeModelConfigDeleteConflict(w http.ResponseWriter, r *http.Request, message string, files []modelmanager.File, registered, inUse []string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	paths := make([]string, 0, len(files))
	for _, file := range files {
		paths = append(paths, file.Path)
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       message,
		"model_files": paths,
		"registered":  registered,
		"in_use":      inUse,
	})
}

func writeModelConfigDeleteResponse(w http.ResponseWriter, snapshot config.ConfigSnapshot, modelID string, deletedFiles []string, pruned []string) {
	w.Header().Set("ETag", snapshot.ETag)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(deleteModelConfigResponse{
		ConfigSnapshot:    snapshot,
		Deleted:           modelID,
		DeletedModelFiles: deletedFiles,
		Pruned:            pruned,
	})
}

func (s *Server) handleAPIConfigYAMLValidate(w http.ResponseWriter, r *http.Request) {
	if s.configManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "config source manager is not configured")
		return
	}
	var request configYAMLRequest
	if err := decodeJSONBody(w, r, &request, maxConfigYAMLBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid YAML editor request")
		return
	}
	result, err := s.configManager.ValidateYAML(r.Context(), []byte(request.YAML))
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func (s *Server) handleAPIConfigYAML(w http.ResponseWriter, r *http.Request) {
	if s.configManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "config source manager is not configured")
		return
	}
	if strings.TrimSpace(r.Header.Get("If-Match")) == "" {
		swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required")
		return
	}
	var request configYAMLRequest
	if err := decodeJSONBody(w, r, &request, maxConfigYAMLBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "invalid YAML editor request")
		return
	}
	snapshot, _, err := s.configManager.ApplyYAML(r.Context(), []byte(request.YAML), r.Header.Get("If-Match"))
	if err != nil {
		if strings.Contains(err.Error(), "etag conflict") {
			swaputil.SendResponse(w, r, http.StatusPreconditionFailed, err.Error())
		} else {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeConfigSnapshot(w, s.decorateConfigSnapshot(snapshot))
}

func (s *Server) handleAPIConfigRollback(w http.ResponseWriter, r *http.Request) {
	if s.configManager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "config source manager is not configured")
		return
	}
	snapshot, err := s.configManager.Rollback(r.Context())
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	writeConfigSnapshot(w, s.decorateConfigSnapshot(snapshot))
}

func (s *Server) daemonRestartPathsSnapshot() []string {
	s.reconcileMu.Lock()
	defer s.reconcileMu.Unlock()
	return append([]string(nil), s.daemonRestartPaths...)
}

func (s *Server) decorateConfigSnapshot(snapshot config.ConfigSnapshot) config.ConfigSnapshot {
	paths := s.daemonRestartPathsSnapshot()
	snapshot.RestartPaths = paths
	snapshot.RestartRequired = len(paths) > 0
	return snapshot
}

func writeConfigSnapshot(w http.ResponseWriter, snapshot config.ConfigSnapshot) {
	w.Header().Set("ETag", snapshot.ETag)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(snapshot)
}

// publicConfigSnapshot serializes an effective in-memory configuration for a
// read-only control-plane response. The regular ConfigManager snapshot starts
// from source YAML and can preserve ${env.*} placeholders; an embedded server
// has no source document, so it must redact values after resolution instead of
// accidentally returning API keys, tokens, or environment-backed secrets.
func publicConfigSnapshot(cfg config.Config) ([]byte, string, error) {
	data, err := json.Marshal(cfg)
	if err != nil {
		return nil, "", err
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, "", err
	}
	redactConfigJSON(value, "")
	redactedJSON, err := json.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	redactedYAML, err := yaml.Marshal(value)
	if err != nil {
		return nil, "", err
	}
	return redactedJSON, string(redactedYAML), nil
}

func redactConfigJSON(value any, parent string) {
	switch current := value.(type) {
	case map[string]any:
		// MacroList is represented as [{"Name": ..., "Value": ...}] by the
		// default JSON encoder. A sensitive macro name makes its value secret
		// even though the generic field name is only "Value".
		var macroName string
		for key, child := range current {
			if strings.EqualFold(key, "name") {
				if name, ok := child.(string); ok {
					macroName = name
				}
			}
		}
		for key, child := range current {
			lower := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
			if isSensitiveConfigKey(lower, parent) || (strings.EqualFold(key, "value") && sensitiveConfigName(macroName)) {
				current[key] = "[REDACTED]"
				continue
			}
			if strings.EqualFold(key, "env") {
				redactConfigEnv(child)
				continue
			}
			redactConfigJSON(child, key)
		}
	case []any:
		for _, child := range current {
			redactConfigJSON(child, parent)
		}
	}
}

func redactConfigEnv(value any) {
	switch current := value.(type) {
	case []any:
		for i, child := range current {
			if text, ok := child.(string); ok {
				current[i] = redactConfigEnvString(text)
				continue
			}
			redactConfigEnv(child)
		}
	case map[string]any:
		for key, child := range current {
			if text, ok := child.(string); ok {
				current[key] = redactConfigEnvString(text)
				continue
			}
			redactConfigEnv(child)
		}
	}
}

func redactConfigEnvString(value string) string {
	if index := strings.IndexByte(value, '='); index > 0 {
		if !config.IsSensitiveEnvironmentName(value[:index]) {
			return value
		}
		return value[:index] + "=[REDACTED]"
	}
	if config.IsSafeAnonymousEnvironmentValue(value) {
		return value
	}
	return "[REDACTED]"
}

func isSensitiveConfigKey(key, parent string) bool {
	if strings.EqualFold(parent, "apikeys") && (key == "key" || key == "value" || key == "secret") {
		return true
	}
	return sensitiveConfigName(key)
}

func sensitiveConfigName(name string) bool {
	lower := strings.ToLower(strings.ReplaceAll(name, "-", "_"))
	return lower == "apikey" || lower == "api_key" || lower == "authorization" || lower == "auth_token" || lower == "password" || lower == "secret" || lower == "token" || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "token")
}
