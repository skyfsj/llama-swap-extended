package server

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logarchive"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

const (
	defaultIncidentLogDir = "logs"
	incidentLogTailBytes  = 128 << 10

	// incidentSaveWindow rate-limits request-error incidents per error
	// signature (method+path+status). Saving is a synchronous fsync cascade on
	// the request goroutine, so a 4xx flood (path scanning) must not turn each
	// request into disk writes. One file per signature per window preserves
	// the diagnostic value.
	incidentSaveWindow = time.Minute

	// incidentThrottleMaxKeys bounds the signature map: a scanner that never
	// repeats a path must not grow it without end. Exceeding the cap resets
	// the throttle (briefly unthrottled, always bounded).
	incidentThrottleMaxKeys = 4096
)

// requestErrorThrottled reports whether an incident with the same signature
// was already saved inside the window, recording the attempt otherwise.
func (s *Server) requestErrorThrottled(signature string, now time.Time) bool {
	if incidentSaveWindow <= 0 {
		return false
	}
	s.incidentMu.Lock()
	defer s.incidentMu.Unlock()
	if s.incidentThrottle == nil {
		s.incidentThrottle = make(map[string]time.Time)
	}
	if len(s.incidentThrottle) >= incidentThrottleMaxKeys {
		s.incidentThrottle = make(map[string]time.Time)
	}
	if last, ok := s.incidentThrottle[signature]; ok && now.Sub(last) < incidentSaveWindow {
		return true
	}
	s.incidentThrottle[signature] = now
	return false
}

// incidentLogsResponse is deliberately small: the snapshot body is fetched
// only after the operator selects an entry.
type incidentLogsResponse struct {
	Items    []logarchive.Entry `json:"items"`
	MaxFiles int                `json:"maxFiles"`
}

func (s *Server) incidentLogSettings() (string, int) {
	cfg := s.currentConfig()
	storage := cfg.LogStorage.Effective()
	dir := strings.TrimSpace(storage.Path)
	if dir == "" && cfg.Store != nil && strings.TrimSpace(cfg.Store.Path) != "" {
		dir = filepath.Join(filepath.Dir(cfg.Store.Path), defaultIncidentLogDir)
	}
	if dir == "" {
		dir = defaultIncidentLogDir
	}
	return dir, storage.MaxFiles
}

func (s *Server) incidentLogStore() (*logarchive.Store, error) {
	if s == nil {
		return nil, errors.New("server is nil")
	}
	dir, maxFiles := s.incidentLogSettings()
	s.incidentMu.Lock()
	defer s.incidentMu.Unlock()
	if s.incidentArchive != nil && s.incidentArchiveDir == dir {
		if s.incidentArchiveMaxFiles != maxFiles {
			if err := s.incidentArchive.SetMaxFiles(maxFiles); err != nil {
				return nil, err
			}
			s.incidentArchiveMaxFiles = maxFiles
		}
		return s.incidentArchive, nil
	}
	archive, err := logarchive.New(logarchive.Config{Dir: dir, MaxFiles: maxFiles})
	if err != nil {
		return nil, err
	}
	s.incidentArchive = archive
	s.incidentArchiveDir = dir
	s.incidentArchiveMaxFiles = maxFiles
	return archive, nil
}

func (s *Server) recordInferenceCrash(modelID string, processLog []byte, exitErr error) {
	archive, err := s.incidentLogStore()
	if err != nil {
		s.warnIncidentLog("create inference crash archive", err)
		return
	}
	reason := "process exited without an exit error"
	if exitErr != nil {
		reason = exitErr.Error()
	}
	content := fmt.Sprintf(
		"event: %s\ntime: %s\nmodel: %s\nexit: %s\n\nprocess output:\n%s\n",
		logarchive.KindInferenceCrash,
		time.Now().UTC().Format(time.RFC3339Nano),
		incidentHeaderValue(modelID),
		incidentHeaderValue(reason),
		incidentSnapshot(processLog, incidentLogTailBytes),
	)
	if _, err := archive.Save(logarchive.KindInferenceCrash, modelID, []byte(content)); err != nil {
		s.warnIncidentLog("save inference crash log", err)
	}
}

func (s *Server) recordRequestError(request RequestError) {
	// The request logger observes every HTTP route, including the embedded UI
	// and control APIs. Incident archives are for inference diagnostics, so do
	// not let an unavailable static asset or an administrative API failure use
	// up the request-error retention budget.
	if !isLLMAPIRequest(request.Method, request.Path) {
		return
	}

	// Throttle before any I/O: the archive save is a synchronous fsync
	// cascade, and a flood of 4xx requests would otherwise amplify into a
	// disk-write flood on the request goroutines themselves.
	if s.requestErrorThrottled(request.Method+" "+request.Path+" "+strconv.Itoa(request.Status), time.Now()) {
		return
	}
	archive, err := s.incidentLogStore()
	if err != nil {
		s.warnIncidentLog("create request error archive", err)
		return
	}
	content := fmt.Sprintf(
		"event: %s\ntime: %s\nclient: %s\nrequest: %s %s %s\nstatus: %d\nbody_bytes: %d\nuser_agent: %s\nduration: %s\n",
		logarchive.KindRequestError,
		time.Now().UTC().Format(time.RFC3339Nano),
		incidentHeaderValue(request.ClientIP),
		incidentHeaderValue(request.Method),
		incidentHeaderValue(request.Path),
		incidentHeaderValue(request.Proto),
		request.Status,
		request.BodyBytes,
		incidentHeaderValue(request.UserAgent),
		request.Duration,
	)
	// The model and the client-visible error body are what make the archive
	// actionable: without them a 503 is a status code with no subject and no
	// reason. Both are optional — a request that never resolved a model, or a
	// response carrying no body, simply omits the line.
	if model := strings.TrimSpace(request.Model); model != "" {
		content += "model: " + incidentHeaderValue(model) + "\n"
	}
	if detail := strings.TrimSpace(request.Detail); detail != "" {
		content += "\nresponse:\n" + detail + "\n"
	}
	if _, err := archive.Save(logarchive.KindRequestError, request.Path, []byte(content)); err != nil {
		s.warnIncidentLog("save request error log", err)
	}
}

// isLLMAPIRequest reports whether a request belongs to the public inference
// API. The model-dispatched route lists are also used by routing and activity
// tracking, keeping incident archiving aligned with the paths that actually
// reach a model instead of matching broad URL prefixes such as /v1/.
func isLLMAPIRequest(method, path string) bool {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	if isModelDispatchedRequest(method, path) {
		return true
	}

	// These inference endpoints do not put a model in the request body and are
	// consequently registered outside modelPostJSONRoutes/modelGetRoutes.
	if method == http.MethodGet && (path == "/v1/models" || path == "/models") {
		return true
	}
	if strings.HasPrefix(path, "/v1/responses/") || strings.HasPrefix(path, "/v/responses/") {
		return method == http.MethodGet || method == http.MethodPost
	}
	return false
}

func (s *Server) warnIncidentLog(action string, err error) {
	if s != nil && s.proxylog != nil {
		s.proxylog.Warnf("incident log: %s: %v", action, err)
	}
}

func incidentHeaderValue(value string) string {
	value = strings.TrimSpace(value)
	value = strings.ReplaceAll(value, "\r", " ")
	value = strings.ReplaceAll(value, "\n", " ")
	return value
}

// incidentSnapshot renders the captured process output for an incident
// archive. The store already keeps the whole bounded history, so this is a
// last-resort guard for an unexpectedly verbose process rather than the
// normal path; when it does clip, the truncation is stated explicitly so the
// reader never has to guess why the transcript starts mid-stream.
func incidentSnapshot(data []byte, max int) string {
	if len(data) == 0 {
		return "(no process output captured)"
	}
	if max <= 0 || len(data) <= max {
		return string(data)
	}
	return fmt.Sprintf("— earlier output omitted: %d of %d bytes retained —\n%s",
		max, len(data), string(data[len(data)-max:]))
}

func (s *Server) handleAPIIncidentLogs(w http.ResponseWriter, r *http.Request) {
	if isModelScoped(identityFromContext(r.Context())) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: incident logs require an unrestricted key")
		return
	}
	archive, err := s.incidentLogStore()
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	items, err := archive.List()
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	items = s.visibleIncidentLogEntries(archive, items)
	writeJSON(w, incidentLogsResponse{Items: items, MaxFiles: archive.MaxFiles()})
}

// visibleIncidentLogEntries hides request-error archives that were written by
// older versions for non-inference routes. Files are deliberately retained on
// disk so this correction does not delete an operator's history; normal
// retention will eventually rotate them out.
func (s *Server) visibleIncidentLogEntries(archive *logarchive.Store, items []logarchive.Entry) []logarchive.Entry {
	visible := make([]logarchive.Entry, 0, len(items))
	for _, entry := range items {
		if entry.Kind != logarchive.KindRequestError {
			visible = append(visible, entry)
			continue
		}
		data, _, err := archive.Read(entry.Name)
		if err != nil {
			s.warnIncidentLog("read request error archive", err)
			continue
		}
		if isLLMAPIRequestArchive(data) {
			visible = append(visible, entry)
		}
	}
	return visible
}

func isLLMAPIRequestArchive(data []byte) bool {
	for _, line := range strings.Split(string(data), "\n") {
		request, ok := strings.CutPrefix(line, "request: ")
		if !ok {
			continue
		}
		parts := strings.Fields(request)
		return len(parts) >= 2 && isLLMAPIRequest(parts[0], parts[1])
	}
	return false
}

func (s *Server) handleAPIIncidentLog(w http.ResponseWriter, r *http.Request) {
	if isModelScoped(identityFromContext(r.Context())) {
		swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: incident logs require an unrestricted key")
		return
	}
	archive, err := s.incidentLogStore()
	if err != nil {
		swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
		return
	}
	data, entry, err := archive.Read(r.PathValue("name"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			swaputil.SendResponse(w, r, http.StatusNotFound, "incident log not found")
			return
		}
		swaputil.SendResponse(w, r, http.StatusBadRequest, err.Error())
		return
	}
	if entry.Kind == logarchive.KindRequestError && !isLLMAPIRequestArchive(data) {
		swaputil.SendResponse(w, r, http.StatusNotFound, "incident log not found")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("inline; filename=%q", entry.Name))
	_, _ = w.Write(data)
}
