package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/modelmanager"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

func (s *Server) handleAPIModelFiles(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	s.modelFilesMu.RLock()
	manager := s.modelFiles
	s.modelFilesMu.RUnlock()
	if manager == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "model file manager is not configured")
		return
	}
	opts, err := parseModelFileListOptions(r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	identity := identityFromContext(r.Context())
	listOpts := opts
	// A model-scoped runtime-read key must not learn paths or source counts
	// belonging exclusively to another model. Fetch the complete bounded page
	// once and paginate after authorization so filtering does not make the
	// caller's requested offset/total refer to hidden entries.
	if !identity.Legacy && len(identity.Models) > 0 {
		listOpts.Limit = modelmanager.MaxListLimit
		listOpts.Offset = 0
	}
	catalog, err := manager.List(r.Context(), listOpts)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	s.annotateModelFileUsage(&catalog)
	if !identity.Legacy && len(identity.Models) > 0 {
		filterModelFileCatalog(&catalog, opts, cfg, identity)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(catalog)
}

// filterModelFileCatalog removes files that are registered exclusively by
// models outside a scoped API key's allowlist and redacts denied model IDs
// from shared-file metadata. Unregistered files remain visible because they
// do not identify a model owner and are useful as candidate weights for an
// authorized model. The catalog is paginated only after filtering so Total,
// Offset, and per-source file counts describe the visible view rather than
// the unfiltered filesystem scan.
func filterModelFileCatalog(catalog *modelmanager.Catalog, opts modelmanager.ListOptions, cfg config.Config, identity auth.Identity) {
	if catalog == nil || identity.Legacy || len(identity.Models) == 0 {
		return
	}
	visible := make([]modelmanager.File, 0, len(catalog.Data))
	sourceCounts := make(map[string]int, len(catalog.Sources))
	for _, file := range catalog.Data {
		registered := make([]string, 0, len(file.Registered))
		for _, model := range file.Registered {
			if modelAllowedForIdentity(cfg, identity, model) {
				registered = append(registered, model)
			}
		}
		// A file shared with an authorized model is visible, but only the
		// authorized registrations are returned. A file used exclusively by
		// denied models is omitted instead of being exposed as an anonymous path.
		if len(file.Registered) > 0 && len(registered) == 0 {
			continue
		}
		inUse := make([]string, 0, len(file.InUse))
		for _, model := range file.InUse {
			if modelAllowedForIdentity(cfg, identity, model) {
				inUse = append(inUse, model)
			}
		}
		file.Registered = registered
		file.InUse = inUse
		visible = append(visible, file)
		sourceCounts[file.SourceID]++
	}
	for i := range catalog.Sources {
		catalog.Sources[i].FileCount = sourceCounts[catalog.Sources[i].ID]
	}
	catalog.Total = len(visible)
	catalog.Limit = opts.Limit
	start := opts.Offset
	if start > len(visible) {
		start = len(visible)
	}
	end := start + opts.Limit
	if end > len(visible) {
		end = len(visible)
	}
	catalog.Offset = start
	catalog.Data = append([]modelmanager.File(nil), visible[start:end]...)
}

func parseModelFileListOptions(r *http.Request) (modelmanager.ListOptions, error) {
	opts := modelmanager.ListOptions{
		SourceID: strings.TrimSpace(r.URL.Query().Get("source")),
		Query:    strings.TrimSpace(r.URL.Query().Get("query")),
		Limit:    modelmanager.DefaultListLimit,
	}
	for name, destination := range map[string]*int{
		"limit":  &opts.Limit,
		"offset": &opts.Offset,
	} {
		value := strings.TrimSpace(r.URL.Query().Get(name))
		if value == "" {
			continue
		}
		parsed, err := strconv.Atoi(value)
		if err != nil {
			return modelmanager.ListOptions{}, fmt.Errorf("%s must be an integer", name)
		}
		*destination = parsed
	}
	if opts.Limit < 1 || opts.Limit > modelmanager.MaxListLimit {
		return modelmanager.ListOptions{}, errors.New("limit must be between 1 and 1000")
	}
	if opts.Offset < 0 {
		return modelmanager.ListOptions{}, errors.New("offset must be >= 0")
	}
	return opts, nil
}

type deleteModelFileRequest struct {
	SourceID string `json:"source_id"`
	Path     string `json:"path"`
	Confirm  bool   `json:"confirm"`
}

func (s *Server) handleAPIDeleteModelFile(w http.ResponseWriter, r *http.Request) {
	cfg := s.currentConfig()
	if r.Body == nil || r.Body == http.NoBody {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "a JSON body with source_id, path, and confirm is required")
		return
	}
	var req deleteModelFileRequest
	// Only three short fields are accepted here. Keep this destructive control
	// endpoint bounded even when a caller sends a chunked body without a
	// Content-Length header.
	if err := decodeJSONBody(w, r, &req, maxAPIKeyJSONBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "a JSON body with source_id, path, and confirm is required")
		return
	}
	if !req.Confirm {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "confirm must be true to delete a model file")
		return
	}
	if strings.TrimSpace(req.SourceID) == "" || strings.TrimSpace(req.Path) == "" {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "source_id and path are required")
		return
	}

	// Keep the usage check and unlink in one critical section. A model which is
	// already registered is always rejected, even when it is currently stopped;
	// this prevents the next request from starting against a deleted path.
	s.modelFilesMu.Lock()
	defer s.modelFilesMu.Unlock()
	if s.modelFiles == nil {
		swaputil.SendResponse(w, r, http.StatusNotImplemented, "model file manager is not configured")
		return
	}
	registered := modelmanager.RegisteredModels(cfg, req.Path)
	inUse := s.modelsUsingFile(registered)
	// A model-scoped config-admin key may manage files used by its own models,
	// but must not learn or mutate a file referenced by another model. Check
	// authorization before returning the registration conflict so the response
	// cannot disclose denied model IDs through the conflict payload.
	identity := identityFromContext(r.Context())
	if !identity.Legacy && len(identity.Models) > 0 {
		for _, model := range registered {
			if !modelAllowedForIdentity(cfg, identity, model) {
				swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
				return
			}
		}
	}
	if len(registered) > 0 || len(inUse) > 0 {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error":      "model file is registered or currently in use",
			"registered": registered,
			"in_use":     inUse,
		})
		return
	}

	file, err := s.modelFiles.Delete(r.Context(), strings.TrimSpace(req.SourceID), r.PathValue("id"), req.Path)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, modelmanager.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			status = http.StatusNotFound
		}
		swaputil.SendResponse(w, r, status, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"deleted": file})
}

func (s *Server) annotateModelFileUsage(catalog *modelmanager.Catalog) {
	if catalog == nil {
		return
	}
	cfg := s.currentConfig()
	for i := range catalog.Data {
		registered := modelmanager.RegisteredModels(cfg, catalog.Data[i].Path)
		inUse := s.modelsUsingFile(registered)
		catalog.Data[i].Registered = registered
		catalog.Data[i].InUse = inUse
	}
}

func (s *Server) modelsUsingFile(registered []string) []string {
	if s.local == nil || len(registered) == 0 {
		return nil
	}
	running := s.local.RunningModels()
	inUse := make([]string, 0, len(registered))
	for _, modelID := range registered {
		if _, ok := running[modelID]; ok {
			inUse = append(inUse, modelID)
		}
	}
	return inUse
}
