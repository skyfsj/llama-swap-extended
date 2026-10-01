package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/extensions"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// extensionManager resolves the manager for a request. It deliberately imposes
// no key requirement of its own: the route chain already enforces the
// config-admin scope, and requiring a key on top of that would make this one
// management surface stricter than every other one in a keyless deployment,
// where configuration, settings and model control are all open. A deployment
// that adds a key gets the same scope check it gets everywhere else.
func (s *Server) extensionManager(w http.ResponseWriter, r *http.Request) *extensions.Manager {
	manager := s.currentExtensions()
	if manager == nil {
		swaputil.SendResponse(w, r, http.StatusConflict, "extensions are disabled")
		return nil
	}
	return manager
}

// maxExtensionBody is the largest extension payload the API accepts. A tree
// of source files plus its lock file has to fit, so it is well above the 3 MiB
// ceiling the config API uses.
const maxExtensionBody = 8 << 20

func (s *Server) handleAPIExtensions(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	if r.Method == http.MethodGet {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"data": m.List()})
		return
	}
	var d extensions.Definition
	if err := decodeJSONBody(w, r, &d, maxExtensionBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	saved, err := m.Save(r.Context(), d, "")
	if err != nil {
		extensionAPIError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", saved.ETag)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(saved)
}

func (s *Server) handleAPIExtension(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	id := r.PathValue("id")
	switch r.Method {
	case http.MethodGet:
		d, err := m.Get(id)
		if err != nil {
			extensionAPIError(w, r, err)
			return
		}
		w.Header().Set("ETag", d.ETag)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(d)
	case http.MethodPut:
		if r.Header.Get("If-Match") == "" {
			swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required")
			return
		}
		var d extensions.Definition
		if err := decodeJSONBody(w, r, &d, maxExtensionBody); err != nil {
			swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
			return
		}
		if d.Manifest.ID != id {
			swaputil.SendResponse(w, r, http.StatusBadRequest, "extension id cannot change")
			return
		}
		saved, err := m.Save(r.Context(), d, r.Header.Get("If-Match"))
		if err != nil {
			extensionAPIError(w, r, err)
			return
		}
		w.Header().Set("ETag", saved.ETag)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(saved)
	case http.MethodDelete:
		if r.Header.Get("If-Match") == "" {
			swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required")
			return
		}
		if err := m.Delete(id, r.Header.Get("If-Match")); err != nil {
			extensionAPIError(w, r, err)
			return
		}
		s.pruneExistingLogMonitors()
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) handleAPIExtensionReload(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	if err := m.Reload(r.Context()); err != nil {
		extensionAPIError(w, r, err)
		return
	}
	s.pruneExistingLogMonitors()
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": m.List()})
}

func (s *Server) handleAPIExtensionDuplicate(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &payload, 64<<10); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	d, err := m.Get(r.PathValue("id"))
	if err != nil {
		extensionAPIError(w, r, err)
		return
	}
	d.Manifest.ID, d.Manifest.Enabled = payload.ID, false
	d.ETag = ""
	saved, err := m.Save(r.Context(), d, "")
	if err != nil {
		extensionAPIError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(saved)
}

func (s *Server) handleAPIExtensionPresets(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	installed := map[string]bool{}
	for _, item := range m.List() {
		installed[item.Manifest.ID] = true
	}
	entries := make([]map[string]any, 0)
	for _, preset := range extensions.Presets() {
		entries = append(entries, map[string]any{
			"id":          preset.ID,
			"name":        preset.Name,
			"description": preset.Description,
			"category":    preset.Category,
			"tags":        preset.Tags,
			"settings":    preset.Settings,
			"hosts":       preset.Hosts,
			"tools":       preset.Tools,
			"installed":   installed[preset.ID],
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": entries})
}

func (s *Server) handleAPIExtensionPreset(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	preset, ok := extensions.PresetByID(r.PathValue("id"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "preset not found")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(preset)
}

func (s *Server) handleAPIExtensionPresetInstall(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	preset, ok := extensions.PresetByID(r.PathValue("id"))
	if !ok {
		swaputil.SendResponse(w, r, http.StatusNotFound, "preset not found")
		return
	}
	var payload struct {
		ID string `json:"id"`
	}
	if err := decodeJSONBody(w, r, &payload, 64<<10); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	installed, err := m.InstallPreset(r.Context(), preset, payload.ID)
	if err != nil {
		extensionAPIError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("ETag", installed.ETag)
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(installed)
}

func (s *Server) handleAPIExtensionCheck(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	var payload struct {
		Files  map[string]string `json:"files"`
		Path   string            `json:"path"`
		Source string            `json:"source"`
	}
	if err := decodeJSONBody(w, r, &payload, maxExtensionBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	var diagnostics []extensions.CheckDiagnostic
	if len(payload.Files) > 0 {
		diagnostics = extensions.CheckTree(payload.Files)
	} else if payload.Path != "" {
		diagnostics = extensions.CheckFile(payload.Path, payload.Source)
	} else {
		swaputil.SendResponse(w, r, http.StatusBadRequest, "files or path are required")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"diagnostics": diagnostics})
}

// maxExtensionUpload bounds the zip body; the 4 MiB tree limit bounds what it
// may expand to inside the archive.
const maxExtensionUpload = 8 << 20

// handleAPIExtensionExport streams the extension as a zip: a config-stripped
// manifest.yaml plus the whole source tree.
func (s *Server) handleAPIExtensionExport(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	d, err := m.Get(r.PathValue("id"))
	if err != nil {
		extensionAPIError(w, r, err)
		return
	}
	data, err := extensions.BuildExtensionArchive(d)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", d.Manifest.ID+".zip"))
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}

// readExtensionUpload parses the multipart zip body. The first multipart
// reader in the management API keeps the same guard style as everything else:
// a hard byte cap on the whole body.
func readExtensionUpload(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	reader, err := r.MultipartReader()
	if err != nil {
		return nil, errors.New("expected a multipart form with a zip file")
	}
	var data []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			return nil, errors.New("multipart form has no zip file")
		}
		if err != nil {
			return nil, err
		}
		if part.FormName() != "file" {
			continue
		}
		trimmed, err := io.ReadAll(io.LimitReader(part, maxExtensionUpload+1))
		if err != nil {
			return nil, err
		}
		if len(trimmed) > maxExtensionUpload {
			return nil, errors.New("extension archive exceeds 8 MiB")
		}
		data = trimmed
		return data, nil
	}
}

// handleAPIExtensionImportPreview parses an uploaded archive and reports what
// would be imported without writing anything.
func (s *Server) handleAPIExtensionImportPreview(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	data, err := readExtensionUpload(w, r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := extensions.ParseExtensionArchive(data)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	// Whether the id already exists drives the UI's replace/save-as choice.
	conflict := false
	if existing, err := m.Get(parsed.Manifest.ID); err == nil && existing.Manifest.ID == parsed.Manifest.ID {
		conflict = true
	}
	files := make([]string, 0, len(parsed.Files))
	for name := range parsed.Files {
		files = append(files, name)
	}
	sort.Strings(files)
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"manifest": map[string]any{
			"id":          parsed.Manifest.ID,
			"name":        parsed.Manifest.Name,
			"description": parsed.Manifest.Description,
			"enabled":     parsed.Manifest.Enabled,
			"permissions": parsed.Manifest.Permissions,
			"match":       parsed.Manifest.Match,
		},
		"files":       files,
		"diagnostics": parsed.Diagnostics,
		"conflict":    conflict,
	})
}

// handleAPIExtensionImport writes an uploaded archive through the existing
// Manager.Save path. mode=create imports under the archive's id (409 when it
// exists unless ?id= renames it); mode=replace requires If-Match and updates
// in place, failing on an etag conflict without touching the extension.
func (s *Server) handleAPIExtensionImport(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	data, err := readExtensionUpload(w, r)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	parsed, err := extensions.ParseExtensionArchive(data)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	// Imported extensions start disabled: an upload must not silently start
	// running code the operator has not reviewed.
	parsed.Manifest.Enabled = false
	mode := r.URL.Query().Get("mode")
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		parsed.Manifest.ID = id
	}
	etag := ""
	if mode == "replace" {
		etag = r.Header.Get("If-Match")
		if etag == "" {
			swaputil.SendResponse(w, r, http.StatusPreconditionRequired, "If-Match is required for replace")
			return
		}
	}
	saved, err := m.Save(r.Context(), extensions.Definition{Manifest: parsed.Manifest, Files: parsed.Files}, etag)
	if err != nil {
		extensionAPIError(w, r, err)
		return
	}
	w.Header().Set("ETag", saved.ETag)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(saved)
}

// extensionUnderTest resolves the extension a debug run executes against: the
// editor draft when files are attached (live debugging of unsaved code), else
// the stored definition. Draft compilation is in-memory and never touches the
// extension directory.
func (s *Server) extensionUnderTest(r *http.Request, m *extensions.Manager, id string, files map[string]string) (*extensions.Compiled, error) {
	var compiled *extensions.Compiled
	if len(files) > 0 {
		var manifest extensions.Manifest
		if existing, err := m.Get(id); err == nil {
			// Draft inherits the stored manifest (permissions, matching,
			// limits) so what the operator tests is what would deploy; the
			// forced-disabled test mode governs execution anyway.
			manifest = existing.Manifest
		} else {
			// A brand-new extension has no stored manifest yet.
			manifest.ID = id
		}
		draft, err := m.CompileDraft(r.Context(), manifest, files)
		if err != nil {
			return nil, fmt.Errorf("draft failed to compile: %w", err)
		}
		compiled = draft
	} else {
		item, ok := m.Compiled(id)
		if !ok {
			return nil, errors.New("extension not found")
		}
		compiled = item
	}
	// Debug sessions can opt into the Node executor (LLAMA_SWAP_NODE_DEBUG=1);
	// production inference always runs goja.
	if extensions.NodeExecutorEnabled() {
		compiled = compiled.WithRuntime(extensions.RuntimeNode)
	}
	return compiled, nil
}

func (s *Server) handleAPIExtensionTest(w http.ResponseWriter, r *http.Request) {
	m := s.extensionManager(w, r)
	if m == nil {
		return
	}
	var payload struct {
		Context extensions.Context `json:"context"`
		Request map[string]any     `json:"request"`
		// Files carries the editor draft: when present the hook runs against
		// this unsaved source instead of the stored definition, so debugging
		// is live and saving stays a deploy decision.
		Files map[string]string `json:"files,omitempty"`
	}
	if err := decodeJSONBody(w, r, &payload, maxExtensionBody); err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	item, err := s.extensionUnderTest(r, m, r.PathValue("id"), payload.Files)
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	payload.Context.DryRun = true
	if payload.Context.ResolvedModel == "" {
		if model, ok := payload.Request["model"].(string); ok {
			payload.Context.ResolvedModel = model
		}
	}
	// The test mode mirrors the live context surface (locale, model snapshot)
	// so scripts behave the same in both; identity stays synthetic because a
	// test has no authenticated caller.
	payload.Context.Locale = requestLocale(r)
	if cfg := s.currentConfig(); len(cfg.Models) > 0 {
		payload.Context.Models = extensionModelSnapshot(cfg, auth.Identity{})
	}
	r = r.WithContext(extensions.WithHostCaller(r.Context(), dryRunHost{}))
	// A draft run debugs code that is not deployed yet, so the enabled flag is
	// not part of the match decision; matching rules still apply.
	matched := (payload.Files != nil || item.Manifest.Enabled) && item.Manifest.Matches(payload.Context)
	result := map[string]any{"matched": matched, "hooks": item.Hooks, "requestBefore": payload.Request, "requestAfter": payload.Request, "injectedTools": []any{}, "logs": []string{}}
	if matched {
		started := time.Now()
		var logs []string
		before := payload.Request
		after, logs, err := extensionMapHookLogged(r.Context(), item, "onRequest", payload.Context, before)
		if err != nil {
			result["error"] = err.Error()
		} else {
			result["requestAfter"] = after
			testRequest := map[string]any{"tools": after["tools"]}
			if payload.Context.Endpoint == "anthropic.messages" {
				_ = injectAnthropicTools(testRequest, item, map[string]*extensions.Compiled{})
			} else {
				_ = injectExtensionTools(testRequest, item, payload.Context.Endpoint != "responses", map[string]*extensions.Compiled{})
			}
			result["injectedTools"] = testRequest["tools"]
		}
		result["logs"] = logs
		result["durationMs"] = float64(time.Since(started)) / float64(time.Millisecond)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

func extensionAPIError(w http.ResponseWriter, r *http.Request, err error) {
	status := http.StatusBadRequest
	var settingsErr *extensions.SettingsValidationError
	if errors.As(err, &settingsErr) {
		writeExtensionDiagnostics(w, settingsErr.Diagnostics)
		return
	}
	if errors.Is(err, os.ErrNotExist) {
		status = http.StatusNotFound
	}
	if strings.Contains(err.Error(), "etag conflict") {
		status = http.StatusPreconditionFailed
	}
	swaputil.SendResponse(w, r, status, err.Error())
}

// writeExtensionDiagnostics answers with a per-field payload the UI anchors to
// the setting that was rejected, mirroring the settings preview contract.
func writeExtensionDiagnostics(w http.ResponseWriter, diagnostics []extensions.SettingDiagnostic) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnprocessableEntity)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error":       "extension settings are invalid",
		"diagnostics": diagnostics,
	})
}
