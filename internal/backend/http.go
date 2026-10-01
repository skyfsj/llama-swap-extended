package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

const maxBackendControlResponseBytes int64 = 1 << 20

// HTTPAdapter is the conservative control adapter for an OpenAI-compatible
// llama.cpp/vLLM listener. Only health/discovery, sleep and wake are proxied;
// arbitrary backend admin paths are never constructed from user input.
type HTTPAdapter struct {
	ID      string
	BaseURL string
	Client  *http.Client
	stateMu sync.RWMutex
	// lastReset is local control-plane telemetry. It is deliberately not
	// inferred from an upstream response because llama.cpp/vLLM versions differ
	// in whether they expose reset timestamps.
	lastReset time.Time
}

func NewHTTPAdapter(id, baseURL string) (*HTTPAdapter, error) {
	raw := strings.TrimSpace(baseURL)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.ContainsAny(raw, "\x00\r\n") || !safeBackendURLPath(u) {
		return nil, fmt.Errorf("backend %s: base URL must be an absolute HTTP(S) URL without credentials, query, or fragment", id)
	}
	return &HTTPAdapter{ID: id, BaseURL: strings.TrimRight(u.String(), "/"), Client: &http.Client{Timeout: 5 * time.Second}}, nil
}

func (a *HTTPAdapter) Name() string {
	if a == nil {
		return ""
	}
	return a.ID
}

func (a *HTTPAdapter) Capabilities(ctx context.Context) (CapabilitySet, error) {
	if err := a.ensureAvailable(); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	d := Discover(ctx, a.BaseURL, a.client())
	return MergeDiscovery(nil, d), nil
}

// ensureAvailable keeps a zero-value or typed-nil adapter from turning a
// control-plane probe into a nil-pointer panic. HTTPAdapter instances created
// through NewHTTPAdapter always have a valid URL; the guard is for embedding,
// tests and optional adapters assembled by callers.
func (a *HTTPAdapter) ensureAvailable() error {
	if a == nil || strings.TrimSpace(a.BaseURL) == "" {
		return fmt.Errorf("%w: HTTP adapter is unavailable", ErrUnsupported)
	}
	return nil
}

func (a *HTTPAdapter) TransformRequest(_ context.Context, _ string, req RequestTransform) (RequestTransform, error) {
	return req, nil
}
func (a *HTTPAdapter) TransformResponse(_ context.Context, _ string, body []byte, _ http.Header) ([]byte, error) {
	return body, nil
}

func (a *HTTPAdapter) CacheState(ctx context.Context) (CacheState, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	value, status, err := a.request(ctx, http.MethodGet, "/is_sleeping", "")
	if err != nil {
		return CacheState{}, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		// Older llama.cpp builds expose cache metadata through /props but do not
		// implement /is_sleeping. It is a read-only fallback; a missing or
		// malformed props response remains an explicit unsupported/error result.
		value, status, err = a.request(ctx, http.MethodGet, "/props", "")
		if err != nil {
			return CacheState{}, err
		}
		if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
			return CacheState{}, ErrUnsupported
		}
		if status < 200 || status >= 300 {
			return CacheState{}, fmt.Errorf("backend props returned HTTP %d", status)
		}
		state, recognized, decodeErr := decodeCacheState(value, false)
		if decodeErr != nil {
			return CacheState{}, fmt.Errorf("decode backend props response: %w", decodeErr)
		}
		if !recognized {
			return CacheState{}, ErrUnsupported
		}
		a.applyLastReset(&state)
		return state.Normalize(), nil
	}
	if status < 200 || status >= 300 {
		return CacheState{}, fmt.Errorf("backend is_sleeping returned HTTP %d", status)
	}
	state, _, err := decodeCacheState(value, true)
	if err != nil {
		return CacheState{}, fmt.Errorf("decode backend is_sleeping response: %w", err)
	}
	a.applyLastReset(&state)
	return state.Normalize(), nil
}

// decodeCacheState accepts the small response-shape variations emitted by
// vLLM and llama.cpp. The boolean return reports whether a /props response
// contained a cache-related field; /is_sleeping is considered recognized even
// when it only contains the sleeping flag.
func decodeCacheState(value []byte, requireSleeping bool) (CacheState, bool, error) {
	if strings.EqualFold(strings.TrimSpace(string(value)), "null") {
		return CacheState{}, false, errors.New("backend cache state response must not be null")
	}
	var bare bool
	if err := json.Unmarshal(value, &bare); err == nil {
		if !requireSleeping {
			return CacheState{}, false, nil
		}
		return CacheState{Supported: true, Sleeping: bare}, true, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(value, &payload); err != nil {
		return CacheState{}, false, err
	}
	if payload == nil {
		return CacheState{}, false, errors.New("backend cache state response must be an object")
	}
	state := CacheState{Supported: true}
	recognized := false
	cacheCounterRecognized := false
	sleepingRecognized := false
	if raw, ok := firstRaw(payload, "is_sleeping", "sleeping", "isSleeping"); ok {
		var sleeping bool
		if err := json.Unmarshal(raw, &sleeping); err != nil {
			return CacheState{}, false, err
		}
		state.Sleeping = sleeping
		sleepingRecognized = true
		recognized = true
	} else if requireSleeping {
		// Keep compatibility with the previous behavior for an object without
		// an explicit flag: the endpoint itself is still a supported probe.
		recognized = true
	}
	if raw, ok := firstRaw(payload, "cached_tokens", "cache_tokens", "n_cache_tokens", "num_cached_tokens", "prefix_cache_tokens", "cache_read_input_tokens", "cache_read_tokens"); ok {
		value, err := jsonInt64(raw)
		if err != nil {
			return CacheState{}, false, err
		}
		state.CachedTokens = maxInt64(value, 0)
		cacheCounterRecognized = true
		recognized = true
	}
	if raw, ok := firstRaw(payload, "cache", "cache_state", "cacheState", "cache_status", "cacheStatus"); ok {
		if strings.EqualFold(strings.TrimSpace(string(raw)), "null") {
			return CacheState{}, false, errors.New("backend cache field must not be null")
		}
		var nested map[string]json.RawMessage
		if err := json.Unmarshal(raw, &nested); err != nil {
			return CacheState{}, false, err
		}
		if nested == nil {
			return CacheState{}, false, errors.New("backend cache field must be an object")
		}
		if !sleepingRecognized {
			if sleepingRaw, found := firstRaw(nested, "is_sleeping", "sleeping", "isSleeping"); found {
				var sleeping bool
				if err := json.Unmarshal(sleepingRaw, &sleeping); err != nil {
					return CacheState{}, false, err
				}
				state.Sleeping = sleeping
				sleepingRecognized = true
				recognized = true
			}
		}
		if !cacheCounterRecognized {
			if cached, found := firstRaw(nested, "cached_tokens", "cache_tokens", "n_cache_tokens", "num_cached_tokens", "prefix_cache_tokens", "cache_read_input_tokens", "cache_read_tokens"); found {
				value, err := jsonInt64(cached)
				if err != nil {
					return CacheState{}, false, err
				}
				state.CachedTokens = maxInt64(value, 0)
				cacheCounterRecognized = true
				recognized = true
			}
		}
	}
	if raw, ok := firstRaw(payload, "last_reset", "lastReset", "reset_at", "resetAt"); ok {
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			state.LastReset = strings.TrimSpace(value)
		} else {
			return CacheState{}, false, err
		}
	}
	return state, recognized, nil
}

func firstRaw(values map[string]json.RawMessage, keys ...string) (json.RawMessage, bool) {
	for _, key := range keys {
		if value, ok := values[key]; ok {
			return value, true
		}
	}
	if len(values) == 0 {
		return nil, false
	}
	normalized := make(map[string]json.RawMessage, len(values))
	for key, value := range values {
		normalized[normalizeJSONKey(key)] = value
	}
	for _, key := range keys {
		if value, ok := normalized[normalizeJSONKey(key)]; ok {
			return value, true
		}
	}
	return nil, false
}

func normalizeJSONKey(value string) string {
	var builder strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(value)) {
		if r == '_' || r == '-' || r == ' ' {
			continue
		}
		builder.WriteRune(r)
	}
	return builder.String()
}

func jsonInt64(raw json.RawMessage) (int64, error) {
	var number int64
	if err := json.Unmarshal(raw, &number); err == nil {
		return number, nil
	}
	var textValue string
	if err := json.Unmarshal(raw, &textValue); err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(textValue), 10, 64)
}

func maxInt64(value, floor int64) int64 {
	if value < floor {
		return floor
	}
	return value
}

func (a *HTTPAdapter) applyLastReset(state *CacheState) {
	if a == nil || state == nil || strings.TrimSpace(state.LastReset) != "" {
		return
	}
	a.stateMu.RLock()
	lastReset := a.lastReset
	a.stateMu.RUnlock()
	if !lastReset.IsZero() {
		state.LastReset = lastReset.UTC().Format(time.RFC3339Nano)
	}
}

func (a *HTTPAdapter) ResetCache(ctx context.Context) error {
	if err := a.ensureAvailable(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// vLLM's prefix-cache reset is a fixed, allowlisted development endpoint.
	// Never send it through a remotely addressed adapter: the endpoint can
	// disrupt active inference and must remain reachable only on a backend
	// bound to the local control plane. The inference wrapper also denies this
	// path on its public listener, so a configured adapter should point at the
	// local vLLM admin listener when cache-admin is enabled.
	u, err := url.Parse(a.BaseURL)
	if err != nil || !isLoopbackHost(u.Hostname()) {
		return ErrUnsupported
	}
	body, status, err := a.request(ctx, http.MethodPost, "/reset_prefix_cache", "")
	if err != nil {
		return err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return ErrUnsupported
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("backend reset_prefix_cache returned HTTP %d", status)
	}
	if len(strings.TrimSpace(string(body))) > 0 {
		var result struct {
			Success *bool `json:"success"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			return fmt.Errorf("decode backend reset_prefix_cache response: %w", err)
		}
		if result.Success != nil && !*result.Success {
			return errors.New("backend prefix cache is busy")
		}
	}
	a.stateMu.Lock()
	a.lastReset = time.Now().UTC()
	a.stateMu.Unlock()
	return nil
}

func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(strings.Trim(host, "[]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *HTTPAdapter) Sleep(ctx context.Context, level int) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if level < 1 || level > 2 {
		return fmt.Errorf("sleep level must be 1 or 2")
	}
	if !a.localAdminEndpoint() {
		return ErrUnsupported
	}
	_, status, err := a.request(ctx, http.MethodPost, "/sleep?level="+strconv.Itoa(level), "")
	if err != nil {
		return err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return ErrUnsupported
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("backend sleep returned HTTP %d", status)
	}
	return nil
}

func (a *HTTPAdapter) Wake(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if !a.localAdminEndpoint() {
		return ErrUnsupported
	}
	_, status, err := a.request(ctx, http.MethodPost, "/wake_up", "")
	if err != nil {
		return err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return ErrUnsupported
	}
	if status < 200 || status >= 300 {
		return fmt.Errorf("backend wake returned HTTP %d", status)
	}
	return nil
}

// localAdminEndpoint gates lifecycle mutations to a loopback listener. Sleep
// and wake release or restore model weights and are therefore backend admin
// operations, not ordinary inference proxy calls. A remote URL must never be
// able to turn a llama-swap control-plane request into a cross-host mutation.
func (a *HTTPAdapter) localAdminEndpoint() bool {
	if a == nil {
		return false
	}
	u, err := url.Parse(a.BaseURL)
	return err == nil && isLoopbackHost(u.Hostname())
}

func (a *HTTPAdapter) Progress(ctx context.Context) (Progress, error) {
	return a.progress(ctx)
}

// progress queries the fixed, read-only progress endpoint exposed by managed
// backends. Older llama.cpp/vLLM builds do not expose it, so a 404/405 keeps
// the conservative idle fallback used by the control plane. A successful but
// malformed response is returned as an error rather than silently reporting
// idle and hiding an unhealthy backend.
func (a *HTTPAdapter) progress(ctx context.Context) (Progress, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	value, status, err := a.request(ctx, http.MethodGet, "/progress", "")
	if err != nil {
		return Progress{}, err
	}
	if status == http.StatusNotFound || status == http.StatusMethodNotAllowed {
		return Progress{Phase: "idle", Completed: 1, Total: 1}, nil
	}
	if status < 200 || status >= 300 {
		return Progress{}, fmt.Errorf("backend progress returned HTTP %d", status)
	}
	progress, err := decodeProgress(value)
	if err != nil {
		return Progress{}, fmt.Errorf("decode backend progress response: %w", err)
	}
	return progress.Normalize(), nil
}

func decodeProgress(value []byte) (Progress, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(value, &payload); err != nil {
		return Progress{}, err
	}
	if payload == nil {
		return Progress{}, errors.New("backend progress response must be an object")
	}
	if nested, ok := firstRaw(payload, "progress", "progress_state", "progressState"); ok {
		if strings.EqualFold(strings.TrimSpace(string(nested)), "null") {
			return Progress{}, errors.New("backend progress field must not be null")
		}
		var nestedPayload map[string]json.RawMessage
		if err := json.Unmarshal(nested, &nestedPayload); err != nil {
			return Progress{}, err
		}
		if nestedPayload == nil {
			return Progress{}, errors.New("backend progress field must be an object")
		}
		for key, raw := range nestedPayload {
			if _, exists := payload[key]; !exists {
				payload[key] = raw
			}
		}
	}
	phase := firstString(payload, "phase", "status", "state")
	if phase == "" {
		phase = "idle"
	}
	completed, err := optionalJSONInt64(payload, "completed", "current", "done", "loaded", "current_tokens", "currentTokens")
	if err != nil {
		return Progress{}, err
	}
	total, err := optionalJSONInt64(payload, "total", "size", "target", "total_tokens", "totalTokens")
	if err != nil {
		return Progress{}, err
	}
	if completed < 0 {
		completed = 0
	}
	if total < 0 {
		total = 0
	}
	message := firstString(payload, "message", "detail")
	progressErr := firstString(payload, "error", "err")
	return Progress{Phase: phase, Completed: completed, Total: total, Message: message, Error: progressErr}, nil
}

func firstString(values map[string]json.RawMessage, keys ...string) string {
	for _, key := range keys {
		raw, ok := firstRaw(values, key)
		if !ok {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
			// Keep the original rune sequence until Progress.Normalize. In
			// particular, trimming here would hide leading Unicode/control
			// characters that the control plane must reject rather than silently
			// normalize away.
			return value
		}
	}
	return ""
}

func optionalJSONInt64(values map[string]json.RawMessage, keys ...string) (int64, error) {
	for _, key := range keys {
		if raw, ok := firstRaw(values, key); ok {
			return jsonInt64(raw)
		}
	}
	return 0, nil
}

func (a *HTTPAdapter) request(ctx context.Context, method, path, body string) ([]byte, int, error) {
	if err := a.ensureAvailable(); err != nil {
		return nil, 0, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	req, err := http.NewRequestWithContext(ctx, method, a.BaseURL+path, strings.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := a.client()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	value, readErr := io.ReadAll(io.LimitReader(resp.Body, maxBackendControlResponseBytes+1))
	if readErr == nil && int64(len(value)) > maxBackendControlResponseBytes {
		readErr = fmt.Errorf("backend control response exceeds %d bytes", maxBackendControlResponseBytes)
		value = nil
	}
	return value, resp.StatusCode, readErr
}

// client returns a request-local copy of the configured HTTP client with
// redirects disabled. Backend control endpoints are fixed, manager-owned
// paths; following a redirect could move a loopback sleep/reset request to a
// different host (or downgrade HTTPS) after the loopback allowlist check has
// already passed. A copied client preserves the caller's transport, timeout
// and TLS settings without mutating a client shared by other adapters.
func (a *HTTPAdapter) client() *http.Client {
	if a == nil || a.Client == nil {
		client := *http.DefaultClient
		client.CheckRedirect = rejectBackendRedirect
		return &client
	}
	client := *a.Client
	client.CheckRedirect = rejectBackendRedirect
	return &client
}

func rejectBackendRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}
