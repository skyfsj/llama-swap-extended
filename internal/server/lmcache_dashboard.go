package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mostlygeek/llama-swap/internal/config"
)

const (
	// A dashboard must never be able to consume an unbounded response from a
	// managed process. Metrics are intentionally generous enough for a normal
	// Prometheus exposition while still being a bounded control-plane payload.
	lmcacheDashboardMaxBody    = 1 << 20
	lmcacheDashboardProbeLimit = 5 * time.Second
)

var lmcacheDashboardEndpoints = []string{
	"health",
	"status",
	"adapters",
	"version",
	"lmcacheVersion",
	"commitID",
	"metrics",
	"periodicHealth",
}

// lmcacheDashboardHealth is deliberately normalized instead of returning the
// upstream response verbatim. LMCache's health endpoint has changed shape
// between releases, but a dashboard needs one unambiguous boolean.
type lmcacheDashboardHealth struct {
	Healthy    bool      `json:"healthy"`
	Status     string    `json:"status,omitempty"`
	HTTPStatus int       `json:"httpStatus"`
	CheckedAt  time.Time `json:"checkedAt"`
	Error      string    `json:"error,omitempty"`
}

type lmcacheDashboardStatus struct {
	Healthy             *bool  `json:"healthy,omitempty"`
	EngineType          string `json:"engineType,omitempty"`
	ChunkSize           int    `json:"chunkSize,omitempty"`
	ActiveSessions      int    `json:"activeSessions,omitempty"`
	ActivePrefetchJobs  int    `json:"activePrefetchJobs,omitempty"`
	L1MemoryUsedBytes   int64  `json:"l1MemoryUsedBytes,omitempty"`
	L1MemoryUsedPresent bool   `json:"-"`
}

type lmcacheDashboardMetrics struct {
	Body        string    `json:"body,omitempty"`
	ContentType string    `json:"contentType,omitempty"`
	FetchedAt   time.Time `json:"fetchedAt"`
	Error       string    `json:"error,omitempty"`
}

type lmcacheDashboardPeriodicHealth struct {
	Healthy          *bool             `json:"healthy,omitempty"`
	UnhealthyThreads []json.RawMessage `json:"unhealthyThreads,omitempty"`
	Error            string            `json:"error,omitempty"`
}

type lmcacheDashboardVersions struct {
	Version       string `json:"version,omitempty"`
	LMCache       string `json:"lmcacheVersion,omitempty"`
	CommitID      string `json:"commitId,omitempty"`
	VersionError  string `json:"versionError,omitempty"`
	LMCacheError  string `json:"lmcacheVersionError,omitempty"`
	CommitIDError string `json:"commitIdError,omitempty"`
}

// lmcacheDashboardResponse is a read-only, same-origin projection of the
// safe LMCache management endpoints. It intentionally has no arbitrary path
// or mutation field: the browser cannot turn this route into a generic proxy.
type lmcacheDashboardResponse struct {
	Available      bool                           `json:"available"`
	Reason         string                         `json:"reason,omitempty"`
	CheckedAt      time.Time                      `json:"checkedAt"`
	Health         lmcacheDashboardHealth         `json:"health"`
	Status         *lmcacheDashboardStatus        `json:"status,omitempty"`
	Adapters       json.RawMessage                `json:"adapters,omitempty"`
	Versions       lmcacheDashboardVersions       `json:"versions"`
	Metrics        lmcacheDashboardMetrics        `json:"metrics"`
	PeriodicHealth lmcacheDashboardPeriodicHealth `json:"periodicHealth"`
	Errors         map[string]string              `json:"errors"`
}

type lmcacheDashboardHTTPResponse struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

// lmcacheDashboardURL builds the endpoint from the configured management
// listener. url.URL handles IPv6 literals and escapes the path; no user input
// can choose a host or path through the dashboard request.
func lmcacheDashboardURL(cfg config.Config, path string) (string, error) {
	eff := cfg.LMCache.Server.Effective()
	host := strings.TrimSpace(eff.HTTPHost)
	if host == "" || eff.HTTPPort <= 0 || eff.HTTPPort > 65535 {
		return "", errors.New("lmcache management endpoint is not configured")
	}
	u := &url.URL{
		Scheme: "http",
		Host:   net.JoinHostPort(host, strconv.Itoa(eff.HTTPPort)),
		Path:   "/" + strings.TrimPrefix(path, "/"),
	}
	return u.String(), nil
}

func fetchLMCacheDashboardEndpoint(ctx context.Context, client *http.Client, endpoint string) (lmcacheDashboardHTTPResponse, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return lmcacheDashboardHTTPResponse{}, err
	}
	response, err := client.Do(request)
	if err != nil {
		return lmcacheDashboardHTTPResponse{}, err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, lmcacheDashboardMaxBody+1))
	if err != nil {
		return lmcacheDashboardHTTPResponse{}, err
	}
	if len(body) > lmcacheDashboardMaxBody {
		return lmcacheDashboardHTTPResponse{}, fmt.Errorf("lmcache endpoint response exceeds %d bytes", lmcacheDashboardMaxBody)
	}
	return lmcacheDashboardHTTPResponse{
		StatusCode:  response.StatusCode,
		ContentType: response.Header.Get("Content-Type"),
		Body:        body,
	}, nil
}

type lmcacheHealthPayload struct {
	Status    string `json:"status"`
	Healthy   *bool  `json:"healthy"`
	OK        *bool  `json:"ok"`
	IsHealthy *bool  `json:"is_healthy"`
}

func decodeLMCacheHealth(response lmcacheDashboardHTTPResponse) (lmcacheDashboardHealth, error) {
	result := lmcacheDashboardHealth{
		HTTPStatus: response.StatusCode,
		CheckedAt:  time.Now().UTC(),
	}
	if response.StatusCode != http.StatusOK {
		var payload lmcacheHealthPayload
		if err := json.Unmarshal(response.Body, &payload); err == nil {
			result.Status = strings.TrimSpace(payload.Status)
		}
		return result, fmt.Errorf("lmcache healthcheck returned HTTP %d", response.StatusCode)
	}
	trimmed := strings.TrimSpace(string(response.Body))
	if strings.EqualFold(trimmed, "healthy") || trimmed == "true" {
		result.Healthy = true
		result.Status = "healthy"
		return result, nil
	}
	var payload lmcacheHealthPayload
	if err := json.Unmarshal(response.Body, &payload); err != nil {
		return result, errors.New("lmcache healthcheck response is not a recognized healthy result")
	}
	result.Status = strings.TrimSpace(payload.Status)
	switch {
	case payload.Healthy != nil:
		result.Healthy = *payload.Healthy
	case payload.OK != nil:
		result.Healthy = *payload.OK
	case payload.IsHealthy != nil:
		result.Healthy = *payload.IsHealthy
	case strings.EqualFold(result.Status, "healthy"):
		result.Healthy = true
	case strings.TrimSpace(result.Status) != "":
		return result, fmt.Errorf("lmcache healthcheck returned non-healthy status %q", result.Status)
	default:
		return result, errors.New("lmcache healthcheck response has no explicit healthy boolean or healthy status")
	}
	if !result.Healthy {
		return result, errors.New("lmcache healthcheck reported unhealthy")
	}
	return result, nil
}

func decodeLMCacheStatus(body []byte) (*lmcacheDashboardStatus, error) {
	var payload struct {
		IsHealthy          *bool  `json:"is_healthy"`
		Healthy            *bool  `json:"healthy"`
		EngineType         string `json:"engine_type"`
		ChunkSize          int    `json:"chunk_size"`
		ActiveSessions     int    `json:"active_sessions"`
		ActivePrefetchJobs int    `json:"active_prefetch_jobs"`
		StorageManager     struct {
			L1Manager struct {
				MemoryUsedBytes *int64 `json:"memory_used_bytes"`
			} `json:"l1_manager"`
		} `json:"storage_manager"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	result := &lmcacheDashboardStatus{
		Healthy:            payload.IsHealthy,
		EngineType:         payload.EngineType,
		ChunkSize:          payload.ChunkSize,
		ActiveSessions:     payload.ActiveSessions,
		ActivePrefetchJobs: payload.ActivePrefetchJobs,
	}
	if result.Healthy == nil {
		result.Healthy = payload.Healthy
	}
	if payload.StorageManager.L1Manager.MemoryUsedBytes != nil {
		result.L1MemoryUsedBytes = *payload.StorageManager.L1Manager.MemoryUsedBytes
		result.L1MemoryUsedPresent = true
	}
	return result, nil
}

func decodeLMCacheAdapters(body []byte) (json.RawMessage, error) {
	var list []json.RawMessage
	if err := json.Unmarshal(body, &list); err == nil && list != nil {
		return json.RawMessage(body), nil
	}
	var payload struct {
		Adapters json.RawMessage `json:"adapters"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if len(payload.Adapters) == 0 || string(payload.Adapters) == "null" {
		return nil, errors.New("lmcache adapters response has no adapters list")
	}
	if err := json.Unmarshal(payload.Adapters, &list); err != nil {
		return nil, errors.New("lmcache adapters response is not a list")
	}
	return json.RawMessage(payload.Adapters), nil
}

func decodeLMCachePeriodicHealth(body []byte) (lmcacheDashboardPeriodicHealth, error) {
	var raw struct {
		Healthy          *bool             `json:"healthy"`
		UnhealthyThreads []json.RawMessage `json:"unhealthy_threads"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return lmcacheDashboardPeriodicHealth{}, err
	}
	return lmcacheDashboardPeriodicHealth{Healthy: raw.Healthy, UnhealthyThreads: raw.UnhealthyThreads}, nil
}

func decodeLMCacheVersion(body []byte) string {
	var value string
	if json.Unmarshal(body, &value) == nil && strings.TrimSpace(value) != "" {
		return strings.TrimSpace(value)
	}
	var payload struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(body, &payload) == nil && strings.TrimSpace(payload.Version) != "" {
		return strings.TrimSpace(payload.Version)
	}
	return strings.TrimSpace(string(body))
}

func dashboardEndpointMap(cfg config.Config) (map[string]string, error) {
	paths := map[string]string{
		"health":         "healthcheck",
		"status":         "status",
		"adapters":       "config/adapters",
		"version":        "version",
		"lmcacheVersion": "lmc_version",
		"commitID":       "commit_id",
		"metrics":        "metrics",
		"periodicHealth": "periodic-threads-health",
	}
	result := make(map[string]string, len(paths))
	for _, name := range lmcacheDashboardEndpoints {
		path, ok := paths[name]
		if !ok {
			return nil, fmt.Errorf("lmcache dashboard endpoint %q is not allowlisted", name)
		}
		endpoint, err := lmcacheDashboardURL(cfg, path)
		if err != nil {
			return nil, err
		}
		result[name] = endpoint
	}
	return result, nil
}

// dashboard fetches the LMCache management API only while the supervised
// process is actually running. A stopped process is a normal state, not an
// upstream error, and it must never expose configuration values as if they
// were live telemetry.
func (svc *lmcacheService) dashboard(ctx context.Context, cfg config.Config) lmcacheDashboardResponse {
	now := time.Now().UTC()
	response := lmcacheDashboardResponse{
		CheckedAt: now,
		Errors:    make(map[string]string),
		Health: lmcacheDashboardHealth{
			CheckedAt: now,
		},
	}
	if svc == nil {
		response.Reason = "unavailable"
		response.Errors["service"] = "lmcache service is unavailable"
		return response
	}
	proc := svc.proc.Status()
	if !proc.Running {
		if proc.State == LMCacheStateError {
			response.Reason = "error"
		} else {
			response.Reason = "stopped"
		}
		return response
	}
	if proc.State != LMCacheStateRunning || !proc.Healthy {
		switch proc.State {
		case LMCacheStateStarting:
			response.Reason = "starting"
		case LMCacheStateStopping:
			response.Reason = "stopping"
		case LMCacheStateUpdating:
			response.Reason = "updating"
		case LMCacheStateError:
			response.Reason = "error"
		default:
			response.Reason = "unhealthy"
		}
		return response
	}
	endpoints, err := dashboardEndpointMap(cfg)
	if err != nil {
		response.Reason = "invalid_endpoint"
		response.Errors["service"] = err.Error()
		return response
	}
	if ctx == nil {
		ctx = context.Background()
	}
	client := &http.Client{Timeout: lmcacheDashboardProbeLimit}

	healthCtx, cancelHealth := context.WithTimeout(ctx, lmcacheDashboardProbeLimit)
	healthResponse, healthErr := fetchLMCacheDashboardEndpoint(healthCtx, client, endpoints["health"])
	cancelHealth()
	if healthErr != nil {
		response.Reason = "unhealthy"
		response.Health.Error = healthErr.Error()
		response.Errors["health"] = healthErr.Error()
		return response
	}
	health, parseErr := decodeLMCacheHealth(healthResponse)
	response.Health = health
	if parseErr != nil || !health.Healthy {
		response.Reason = "unhealthy"
		if response.Health.Error == "" && parseErr != nil {
			response.Health.Error = parseErr.Error()
		}
		response.Errors["health"] = response.Health.Error
		if response.Errors["health"] == "" {
			response.Errors["health"] = "lmcache healthcheck reported unhealthy"
		}
		return response
	}

	type result struct {
		name     string
		response lmcacheDashboardHTTPResponse
		err      error
	}
	results := make(chan result, len(endpoints)-1)
	var group sync.WaitGroup
	for _, name := range []string{"status", "adapters", "version", "lmcacheVersion", "commitID", "metrics", "periodicHealth"} {
		name := name
		group.Add(1)
		go func() {
			defer group.Done()
			endpointCtx, cancel := context.WithTimeout(ctx, lmcacheDashboardProbeLimit)
			defer cancel()
			item, itemErr := fetchLMCacheDashboardEndpoint(endpointCtx, client, endpoints[name])
			results <- result{name: name, response: item, err: itemErr}
		}()
	}
	group.Wait()
	close(results)

	response.Available = true
	for item := range results {
		if item.err != nil {
			response.Errors[item.name] = item.err.Error()
			continue
		}
		if item.response.StatusCode < 200 || item.response.StatusCode >= 300 {
			response.Errors[item.name] = fmt.Sprintf("lmcache %s returned HTTP %d", item.name, item.response.StatusCode)
			continue
		}
		switch item.name {
		case "status":
			decoded, decodeErr := decodeLMCacheStatus(item.response.Body)
			if decodeErr != nil {
				response.Errors[item.name] = decodeErr.Error()
			} else {
				response.Status = decoded
			}
		case "adapters":
			adapters, decodeErr := decodeLMCacheAdapters(item.response.Body)
			if decodeErr != nil {
				response.Errors[item.name] = decodeErr.Error()
			} else {
				response.Adapters = adapters
			}
		case "version":
			response.Versions.Version = decodeLMCacheVersion(item.response.Body)
		case "lmcacheVersion":
			response.Versions.LMCache = decodeLMCacheVersion(item.response.Body)
		case "commitID":
			response.Versions.CommitID = decodeLMCacheVersion(item.response.Body)
		case "metrics":
			response.Metrics = lmcacheDashboardMetrics{
				Body:        string(item.response.Body),
				ContentType: item.response.ContentType,
				FetchedAt:   now,
			}
		case "periodicHealth":
			periodic, decodeErr := decodeLMCachePeriodicHealth(item.response.Body)
			if decodeErr != nil {
				response.Errors[item.name] = decodeErr.Error()
			} else {
				response.PeriodicHealth = periodic
			}
		}
	}
	return response
}

func (s *Server) handleAPILMCacheDashboard(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	if s == nil || s.lmcacheMod == nil {
		response := lmcacheDashboardResponse{
			Available: false,
			Reason:    "unavailable",
			CheckedAt: time.Now().UTC(),
			Errors:    map[string]string{"service": "lmcache service is unavailable"},
		}
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	response := s.lmcacheMod.dashboard(r.Context(), s.currentConfig())
	_ = json.NewEncoder(w).Encode(response)
}

// lmcacheServerLogTailLimit bounds one logs response. The supervised server
// appends to a single file, so the tail carries everything an operator needs
// to diagnose a crash or a stuck health check.
const lmcacheServerLogTailLimit = 256 << 10

// lmcacheServerLogServeLimit is how much of the tail one response carries;
// the UI polls this endpoint, so a bounded body keeps each poll cheap.
const lmcacheServerLogServeLimit = 24 << 10

type lmcacheServerLogResponse struct {
	LogPath   string `json:"logPath"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	Output    string `json:"output"`
}

// handleAPILMCacheServerLogs tails the supervised LMCache server's log file.
// The file already exists for post-mortem diagnosis (spec 25); this endpoint
// exposes it to the dashboard's log panel without changing how the server
// writes it.
func (s *Server) handleAPILMCacheServerLogs(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	response := lmcacheServerLogResponse{}
	if s == nil {
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	response.LogPath = lmcacheServerLogPath(s.currentConfig())
	info, err := os.Stat(response.LogPath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			response.LogPath = ""
			response.Output = fmt.Sprintf("stat lmcache server log: %v", err)
		}
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	response.Size = info.Size()
	start := info.Size() - lmcacheServerLogTailLimit
	if start > 0 {
		response.Truncated = true
	} else {
		start = 0
	}
	file, err := os.Open(response.LogPath)
	if err != nil {
		response.LogPath = ""
		response.Output = fmt.Sprintf("open lmcache server log: %v", err)
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	defer file.Close()
	// The tail starts at `start`, not at the beginning of the file; ReadFull
	// without the seek would silently serve the file's head instead.
	if _, err := file.Seek(start, io.SeekStart); err != nil {
		response.LogPath = ""
		response.Output = fmt.Sprintf("seek lmcache server log: %v", err)
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	data := make([]byte, info.Size()-start)
	if _, err := io.ReadFull(file, data); err != nil {
		response.LogPath = ""
		response.Output = fmt.Sprintf("read lmcache server log: %v", err)
		_ = json.NewEncoder(w).Encode(response)
		return
	}
	// Trim the tail first so every response stays bounded, then keep the
	// bytes as-is: escape sequences are rendered as terminal colors by the
	// log panel, not stripped.
	cutFromMiddle := start > 0
	if len(data) > lmcacheServerLogServeLimit {
		data = data[len(data)-lmcacheServerLogServeLimit:]
		cutFromMiddle = true
	}
	response.Output = sanitizeLogTail(string(data), cutFromMiddle)
	_ = json.NewEncoder(w).Encode(response)
}
