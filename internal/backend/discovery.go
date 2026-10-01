package backend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode"
)

const (
	maxDiscoveryBody    = 4 << 20
	maxDiscoveryString  = 256
	maxDiscoveredModels = 4096
	maxDiscoveredCaps   = 256
)

// Discovery is the small, safe subset of backend metadata that can be
// observed without calling inference or management endpoints.
type Discovery struct {
	Type         string          `json:"type,omitempty"`
	Version      string          `json:"version,omitempty"`
	Models       []string        `json:"models,omitempty"`
	Capabilities map[string]bool `json:"capabilities,omitempty"`
	// ServerInfo contains a deliberately small, secret-free projection of the
	// optional vLLM /server_info endpoint. The endpoint is development-only in
	// many vLLM releases and includes full environment/config objects, so never
	// expose its raw payload through the control plane.
	ServerInfo map[string]string `json:"serverInfo,omitempty"`
	Source     string            `json:"source,omitempty"`
	Error      string            `json:"error,omitempty"`
}

// Discover probes the required /version and /v1/models endpoints, then makes a
// best-effort read-only request to /server_info. A failing probe is returned as
// metadata rather than an error so discovery never blocks an otherwise usable
// backend (explicit configuration always remains authoritative).
func Discover(ctx context.Context, baseURL string, client *http.Client) Discovery {
	if ctx == nil {
		ctx = context.Background()
	}
	result := Discovery{Capabilities: make(map[string]bool), Source: "discovery"}
	base, err := parseBaseURL(baseURL)
	if err != nil {
		result.Error = err.Error()
		return result
	}
	if client == nil {
		client = &http.Client{Timeout: 3 * time.Second}
	}
	// Discovery is used for read-only metadata, but its URL can still point at
	// a loopback control plane. Never follow a redirect here: doing so could
	// silently move a probe to another host or downgrade HTTPS. Copy the client
	// so the caller's transport, timeout and cookie policy remain intact.
	clientCopy := *client
	clientCopy.CheckRedirect = rejectDiscoveryRedirect
	client = &clientCopy
	versionURL := strings.TrimRight(base.String(), "/") + "/version"
	if version, versionErr := probeJSON(ctx, client, versionURL); versionErr == nil {
		result.Version = stringField(version, "version")
		if result.Version == "" {
			result.Version = stringField(version, "build_version")
		}
		result.Type = stringField(version, "backend")
	}
	modelsURL := strings.TrimRight(base.String(), "/") + "/v1/models"
	models, modelsErr := probeJSON(ctx, client, modelsURL)
	if modelsErr != nil {
		if result.Error == "" {
			result.Error = modelsErr.Error()
		}
		return result
	}
	result.Models = modelIDs(models)
	for _, capability := range []string{"chat", "completions", "embeddings", "responses", "transcriptions", "translations"} {
		result.Capabilities[capability] = true
	}
	// vLLM exposes /server_info only when its development server mode is
	// enabled. Probe it as an optional, read-only endpoint after the required
	// version/models probes; a missing or unauthorized endpoint must never make
	// an otherwise usable backend disappear. Request JSON explicitly so a
	// future vLLM version does not return a giant human-readable config string.
	serverInfoURL := strings.TrimRight(base.String(), "/") + "/server_info?config_format=json"
	if info, infoErr := probeJSON(ctx, client, serverInfoURL); infoErr == nil {
		result.ServerInfo, result.Capabilities = safeServerInfo(info, result.Capabilities)
	}
	if result.Type == "" {
		result.Type = "openai-compatible"
	}
	return result
}

func rejectDiscoveryRedirect(_ *http.Request, _ []*http.Request) error {
	return http.ErrUseLastResponse
}

func parseBaseURL(raw string) (*url.URL, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || strings.ContainsAny(raw, "\x00\r\n") || !safeBackendURLPath(u) {
		return nil, errors.New("backend discovery URL must be an absolute HTTP(S) URL")
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("backend discovery URL must not contain credentials, query, or fragment")
	}
	return u, nil
}

// safeBackendURLPath rejects escaped controls and format characters as well
// as their literal forms. url.Parse intentionally leaves percent-encoded
// bytes in Path, so checking only the raw input would let a base URL containing
// an encoded newline (or zero-width marker) reach the HTTP transport.
func safeBackendURLPath(value *url.URL) bool {
	if value == nil {
		return false
	}
	decoded := value.EscapedPath()
	for i := 0; i < 3; i++ {
		unescaped, err := url.PathUnescape(decoded)
		if err != nil {
			return false
		}
		if unescaped == decoded {
			break
		}
		decoded = unescaped
	}
	for _, r := range decoded {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) || unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func probeJSON(ctx context.Context, client *http.Client, endpoint string) (map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("discovery endpoint returned HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxDiscoveryBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxDiscoveryBody {
		return nil, errors.New("discovery response is too large")
	}
	var value map[string]any
	if err := json.Unmarshal(body, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func stringField(value map[string]any, key string) string {
	if value == nil {
		return ""
	}
	if text, ok := value[key].(string); ok {
		if text, ok := safeDiscoveryString(text); ok {
			return text
		}
		return ""
	}
	normalizedKey := normalizeJSONKey(key)
	for candidate, raw := range value {
		if normalizeJSONKey(candidate) != normalizedKey {
			continue
		}
		text, _ := raw.(string)
		if text, ok := safeDiscoveryString(text); ok {
			return text
		}
		return ""
	}
	return ""
}

func mapField(value map[string]any, key string) (map[string]any, bool) {
	if value == nil {
		return nil, false
	}
	if nested, ok := value[key].(map[string]any); ok {
		return nested, true
	}
	normalizedKey := normalizeJSONKey(key)
	for candidate, raw := range value {
		if normalizeJSONKey(candidate) != normalizedKey {
			continue
		}
		nested, ok := raw.(map[string]any)
		return nested, ok
	}
	return nil, false
}

func anyField(value map[string]any, key string) (any, bool) {
	if value == nil {
		return nil, false
	}
	if raw, ok := value[key]; ok {
		return raw, true
	}
	normalizedKey := normalizeJSONKey(key)
	for candidate, raw := range value {
		if normalizeJSONKey(candidate) == normalizedKey {
			return raw, true
		}
	}
	return nil, false
}

func modelIDs(value map[string]any) []string {
	var data []any
	if raw, ok := anyField(value, "data"); ok {
		data, _ = discoverySlice(raw)
	}
	if len(data) == 0 {
		// A few OpenAI-compatible servers return a plain `models` array rather
		// than the canonical `{data:[{id:...}]}` envelope. Accept only strings
		// here; arbitrary objects are intentionally ignored.
		if raw, ok := anyField(value, "models"); ok {
			models, ok := discoverySlice(raw)
			if !ok {
				return nil
			}
			ids := make([]string, 0, min(len(models), maxDiscoveredModels))
			for _, raw := range models {
				if len(ids) >= maxDiscoveredModels {
					break
				}
				if id, ok := raw.(string); ok {
					if id, valid := safeDiscoveryString(id); valid {
						ids = append(ids, id)
					}
					continue
				}
				if item, ok := raw.(map[string]any); ok {
					if id := stringField(item, "id"); id != "" {
						ids = append(ids, id)
					}
				}
			}
			return ids
		}
	}
	ids := make([]string, 0, min(len(data), maxDiscoveredModels))
	for _, raw := range data {
		if len(ids) >= maxDiscoveredModels {
			break
		}
		if id, ok := raw.(string); ok {
			if id, valid := safeDiscoveryString(id); valid {
				ids = append(ids, id)
			}
			continue
		}
		item, _ := raw.(map[string]any)
		if id := stringField(item, "id"); id != "" {
			ids = append(ids, id)
		}
	}
	return ids
}

// discoverySlice accepts the JSON-decoder shape as well as the typed slices
// commonly passed to the value-level discovery helpers by embedders. Keeping
// this conversion explicit avoids reflection while making model discovery
// independent of whether the caller decoded through encoding/json first.
func discoverySlice(value any) ([]any, bool) {
	switch values := value.(type) {
	case []any:
		return values, true
	case []map[string]any:
		out := make([]any, len(values))
		for index, value := range values {
			out[index] = value
		}
		return out, true
	case []string:
		out := make([]any, len(values))
		for index, value := range values {
			out[index] = value
		}
		return out, true
	default:
		return nil, false
	}
}

// safeServerInfo extracts only scalar operational fields that are useful for
// capability/runtime diagnostics. vLLM's /server_info response contains
// complete environment maps (and may include credentials in future versions),
// so this allowlist is intentionally narrow and values are converted to
// bounded strings. Capabilities may be declared either as a bool map or as a
// string array; explicit false values are retained for precedence handling.
func safeServerInfo(value map[string]any, capabilities map[string]bool) (map[string]string, map[string]bool) {
	if value == nil {
		return nil, capabilities
	}
	allowed := make(map[string]string)
	for _, canonical := range []string{
		"backend", "version", "build_version", "model", "served_model_name", "max_model_len", "dtype",
		"quantization", "device", "platform", "tensor_parallel_size", "pipeline_parallel_size",
		"gpu_memory_utilization", "enable_prefix_caching", "vllm_target_device", "vllm_version", "target_device",
	} {
		allowed[normalizeJSONKey(canonical)] = canonical
	}
	info := make(map[string]string)
	copyScalars := func(source map[string]any) {
		for key, raw := range source {
			canonical, ok := allowed[normalizeJSONKey(key)]
			if !ok {
				continue
			}
			if scalar, ok := safeScalarString(raw); ok {
				info[canonical] = scalar
			}
		}
	}
	copyScalars(value)
	// The JSON form nests the useful config under vllm_config. Only inspect
	// object values; the default text form is ignored rather than retained.
	if config, ok := mapField(value, "vllm_config"); ok {
		copyScalars(config)
	}
	if env, ok := mapField(value, "vllm_env"); ok {
		for key, raw := range env {
			key = strings.ToLower(strings.TrimSpace(key))
			if !strings.HasPrefix(key, "vllm_") {
				continue
			}
			canonical, ok := allowed[normalizeJSONKey(strings.TrimPrefix(key, "vllm_"))]
			if !ok {
				continue
			}
			if scalar, ok := safeScalarString(raw); ok {
				info[canonical] = scalar
			}
		}
	}
	if declared, ok := anyField(value, "capabilities"); ok {
		capabilities = mergeDeclaredCapabilities(capabilities, declared)
	}
	if config, ok := mapField(value, "vllm_config"); ok {
		if declared, exists := anyField(config, "capabilities"); exists {
			capabilities = mergeDeclaredCapabilities(capabilities, declared)
		}
	}
	if len(info) == 0 {
		info = nil
	}
	return info, capabilities
}

func mergeDeclaredCapabilities(capabilities map[string]bool, value any) map[string]bool {
	if capabilities == nil {
		capabilities = make(map[string]bool)
	}
	add := func(raw string, enabled bool) {
		key, ok := safeDiscoveryString(raw)
		if !ok {
			return
		}
		key = strings.ToLower(key)
		if _, exists := capabilities[key]; !exists && len(capabilities) >= maxDiscoveredCaps {
			return
		}
		capabilities[key] = enabled
	}
	switch declared := value.(type) {
	case map[string]any:
		for key, raw := range declared {
			if enabled, ok := raw.(bool); ok {
				add(key, enabled)
			}
		}
	case map[string]bool:
		for key, enabled := range declared {
			add(key, enabled)
		}
	case []any:
		for _, raw := range declared {
			if key, ok := raw.(string); ok {
				add(key, true)
			}
		}
	case []string:
		for _, key := range declared {
			add(key, true)
		}
	}
	return capabilities
}

func safeScalarString(value any) (string, bool) {
	var text string
	switch value := value.(type) {
	case string:
		text = value
	case bool:
		text = strconv.FormatBool(value)
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return "", false
		}
		text = strconv.FormatFloat(value, 'g', -1, 64)
	case float32:
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return "", false
		}
		text = strconv.FormatFloat(float64(value), 'g', -1, 32)
	case int:
		text = strconv.Itoa(value)
	case int8:
		text = strconv.FormatInt(int64(value), 10)
	case int16:
		text = strconv.FormatInt(int64(value), 10)
	case int32:
		text = strconv.FormatInt(int64(value), 10)
	case int64:
		text = strconv.FormatInt(value, 10)
	case uint:
		text = strconv.FormatUint(uint64(value), 10)
	case uint8:
		text = strconv.FormatUint(uint64(value), 10)
	case uint16:
		text = strconv.FormatUint(uint64(value), 10)
	case uint32:
		text = strconv.FormatUint(uint64(value), 10)
	case uint64:
		text = strconv.FormatUint(value, 10)
	case json.Number:
		text = string(value)
	default:
		return "", false
	}
	return safeDiscoveryString(text)
}

// safeDiscoveryString keeps remote backend metadata useful without allowing
// control/format characters or unbounded strings into the control plane. A
// normal ASCII space remains valid for display names and model identifiers;
// other Unicode whitespace is rejected because it can make two IDs look the
// same while routing to different backends.
func safeDiscoveryString(value string) (string, bool) {
	for _, r := range value {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return "", false
		}
		if unicode.IsSpace(r) && r != ' ' {
			return "", false
		}
	}
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxDiscoveryString {
		return "", false
	}
	return value, true
}

// MergeDiscovery applies discovered values only where explicit configuration
// is absent. It is intentionally pure so callers can cache or display the
// result without mutating model configuration.
func MergeDiscovery(explicit CapabilitySet, discovered Discovery) CapabilitySet {
	out := make(CapabilitySet, len(explicit)+len(discovered.Capabilities))
	for key, value := range discovered.Capabilities {
		out[key] = value
	}
	for key, value := range explicit {
		out[key] = value
	}
	return out
}
