package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"
)

// QuotaConfig describes how to read a subscription's usage windows from an
// upstream endpoint so the UI can display them per credential. It is provider
// level: every credential (API key) of the provider is queried with its own
// secret because each key represents one subscription.
//
// A config may name a built-in preset, spell the mapping out inline, or both
// (inline fields override the preset). Presets live in quotapresets/*.yaml and
// make a new plan a data change instead of a code change.
type QuotaConfig struct {
	// Preset names a built-in quota mapping (see QuotaPresetIDs).
	Preset string `yaml:"preset" json:"preset,omitempty"`

	// Endpoint is the absolute URL polled for usage data. Required unless a
	// preset supplies it.
	Endpoint string `yaml:"endpoint" json:"endpoint,omitempty"`
	// Method defaults to GET.
	Method string `yaml:"method" json:"method,omitempty"`
	// Auth selects how the credential value is presented to the endpoint.
	Auth QuotaAuthConfig `yaml:"auth" json:"auth,omitempty"`
	// Headers are extra request headers for the quota request.
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`
	// TimeoutSeconds bounds one refresh attempt (default 15).
	TimeoutSeconds int `yaml:"timeoutSeconds" json:"timeoutSeconds,omitempty"`
	// RefreshInterval enables background refreshes; 0 keeps refresh manual.
	RefreshInterval string `yaml:"refreshInterval" json:"refreshInterval,omitempty"`

	// Windows are the usage windows shown, in order.
	Windows []QuotaWindow `yaml:"windows" json:"windows,omitempty"`
	// Extras are additional scalar rows (credit balance, counters, ...). They
	// may carry the whole mapping on their own: a credit balance has no
	// percentage window, so a quota with extras and no windows is valid.
	Extras []QuotaExtra `yaml:"extras" json:"extras,omitempty"`

	// Enforce optionally ties the quota to scheduling; disabled by default.
	Enforce QuotaEnforceConfig `yaml:"enforce" json:"enforce,omitempty"`
}

// QuotaAuthConfig selects how the credential is sent.
type QuotaAuthConfig struct {
	// Style is "bearer" (default), "header" or "none".
	Style string `yaml:"style" json:"style,omitempty"`
	// Header is the header name used by style "header".
	Header string `yaml:"header" json:"header,omitempty"`
}

// QuotaWindow maps one usage window out of the quota response.
type QuotaWindow struct {
	// ID identifies the window ("rolling", "weekly", "monthly", ...). Known
	// ids get a localized label in the UI; others fall back to Label.
	ID string `yaml:"id" json:"id"`
	// Label is the fallback display name.
	Label string `yaml:"label" json:"label,omitempty"`
	// Path is a gjson path to the window object (e.g. "usage.rolling").
	Path string `yaml:"path" json:"path"`
	// Percent is the field (relative to Path) holding the percentage; it
	// defaults to "percent".
	Percent string `yaml:"percent" json:"percent,omitempty"`
	// PercentMode says whether Percent is the used share (default) or the
	// remaining share.
	PercentMode string `yaml:"percentMode" json:"percentMode,omitempty"`
	// Status is the field holding a status string (optional).
	Status string `yaml:"status" json:"status,omitempty"`
	// ResetsAt is the field holding the window reset timestamp (optional).
	ResetsAt string `yaml:"resetsAt" json:"resetsAt,omitempty"`
}

// QuotaExtra maps one scalar value shown next to the windows.
type QuotaExtra struct {
	ID    string `yaml:"id" json:"id"`
	Label string `yaml:"label" json:"label,omitempty"`
	Path  string `yaml:"path" json:"path"`
}

// QuotaEnforceConfig optionally cool down a credential whose window is full.
type QuotaEnforceConfig struct {
	// Enabled turns the behaviour on. Default false: quota is observability.
	Enabled bool `yaml:"enabled" json:"enabled,omitempty"`
	// Threshold is the used percentage at or above which the credential is
	// cooled down (default 100).
	Threshold int `yaml:"threshold" json:"threshold,omitempty"`
	// FallbackCooldown applies when a full window carries no reset time
	// (default 5m).
	FallbackCooldown string `yaml:"fallbackCooldown" json:"fallbackCooldown,omitempty"`
}

// Quota settings defaults.
const (
	QuotaDefaultPercentMode = "used"
	QuotaDefaultThreshold   = 100
	// QuotaDefaultFallbackCooldown bounds a park when the full window carries
	// no reset timestamp.
	QuotaDefaultFallbackCooldown = 5 * time.Minute
	QuotaDefaultTimeoutSeconds   = 15
	// QuotaDefaultEnforceRefreshInterval is the background refresh cadence
	// applied when enforcement is enabled without an explicit interval.
	QuotaDefaultEnforceRefreshInterval = "5m"
)

// QuotaAuthStyle values.
const (
	QuotaAuthBearer = "bearer"
	QuotaAuthHeader = "header"
	QuotaAuthNone   = "none"
)

// normalize validates the quota block and merges the referenced preset.
// It returns nil (clearing the block) when nothing is configured.
func (q *QuotaConfig) normalize() error {
	if q == nil {
		return nil
	}
	if q.Preset != "" {
		preset, ok := QuotaPresetByID(q.Preset)
		if !ok {
			return fmt.Errorf("providers: quota.preset %q is unknown (known presets: %s)",
				q.Preset, strings.Join(QuotaPresetIDs(), ", "))
		}
		q.mergeFrom(preset)
	}

	q.Method = strings.ToUpper(strings.TrimSpace(q.Method))
	if q.Method == "" {
		q.Method = "GET"
	}
	if q.Method != "GET" {
		return fmt.Errorf("providers: quota.method must be GET, got %q", q.Method)
	}

	endpoint := strings.TrimSpace(q.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("providers: quota.endpoint is required")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("providers: quota.endpoint is not a valid URL (%s): %w", endpoint, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("providers: quota.endpoint must use http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("providers: quota.endpoint is missing a host")
	}
	q.Endpoint = endpoint

	switch q.Auth.Style {
	case "":
		q.Auth.Style = QuotaAuthBearer
	case QuotaAuthBearer, QuotaAuthNone:
		q.Auth.Header = ""
	case QuotaAuthHeader:
		if strings.TrimSpace(q.Auth.Header) == "" {
			return fmt.Errorf("providers: quota.auth.header is required when auth.style is %q", QuotaAuthHeader)
		}
	default:
		return fmt.Errorf("providers: quota.auth.style must be %q, %q or %q, got %q",
			QuotaAuthBearer, QuotaAuthHeader, QuotaAuthNone, q.Auth.Style)
	}

	if q.TimeoutSeconds < 0 {
		return fmt.Errorf("providers: quota.timeoutSeconds must be >= 0")
	}
	if q.TimeoutSeconds == 0 {
		q.TimeoutSeconds = QuotaDefaultTimeoutSeconds
	}

	if q.RefreshInterval == "" && q.Enforce.Enabled {
		// Enforcement is only useful with fresh data; without an explicit
		// interval a background refresh every 5 minutes keeps it working.
		q.RefreshInterval = QuotaDefaultEnforceRefreshInterval
	}
	if q.RefreshInterval != "" {
		interval, err := time.ParseDuration(q.RefreshInterval)
		if err != nil {
			return fmt.Errorf("providers: quota.refreshInterval is not a duration (%s): %w", q.RefreshInterval, err)
		}
		if interval < 0 {
			return fmt.Errorf("providers: quota.refreshInterval must be >= 0")
		}
		if interval > 0 && interval < 30*time.Second {
			// The refresh hits the upstream once per credential; keep a floor
			// so a typo cannot hammer the subscription.
			return fmt.Errorf("providers: quota.refreshInterval must be at least 30s")
		}
	}

	headers := make(map[string]string, len(q.Headers))
	if err := normalizeQuotaHeaders(headers, q.Headers); err != nil {
		return err
	}
	q.Headers = headers

	if len(q.Windows) == 0 && len(q.Extras) == 0 {
		// A balance-style quota has no windows at all, so the requirement is
		// "something to show" rather than "at least one window".
		return fmt.Errorf("providers: quota needs at least one window or extra")
	}
	seenWindows := make(map[string]struct{}, len(q.Windows))
	for i := range q.Windows {
		window := &q.Windows[i]
		window.ID = strings.TrimSpace(window.ID)
		window.Path = strings.TrimSpace(window.Path)
		if window.ID == "" {
			return fmt.Errorf("providers: quota.windows[%d] needs an id", i)
		}
		if window.Path == "" {
			return fmt.Errorf("providers: quota.windows[%d] needs a path", i)
		}
		if _, duplicate := seenWindows[window.ID]; duplicate {
			return fmt.Errorf("providers: duplicate quota window %q", window.ID)
		}
		seenWindows[window.ID] = struct{}{}

		if window.Percent == "" {
			window.Percent = "percent"
		}
		switch window.PercentMode {
		case "":
			window.PercentMode = QuotaDefaultPercentMode
		case QuotaDefaultPercentMode, "remaining":
		default:
			return fmt.Errorf("providers: quota.windows[%d].percentMode must be \"used\" or \"remaining\", got %q",
				i, window.PercentMode)
		}
		if window.Label == "" {
			window.Label = window.ID
		}
	}

	seenExtras := make(map[string]struct{}, len(q.Extras))
	for i := range q.Extras {
		extra := &q.Extras[i]
		extra.ID = strings.TrimSpace(extra.ID)
		extra.Path = strings.TrimSpace(extra.Path)
		if extra.ID == "" || extra.Path == "" {
			return fmt.Errorf("providers: quota.extras[%d] needs an id and a path", i)
		}
		if _, duplicate := seenExtras[extra.ID]; duplicate {
			return fmt.Errorf("providers: duplicate quota extra %q", extra.ID)
		}
		seenExtras[extra.ID] = struct{}{}
		if extra.Label == "" {
			extra.Label = extra.ID
		}
	}

	if q.Enforce.Threshold < 0 || q.Enforce.Threshold > 100 {
		return fmt.Errorf("providers: quota.enforce.threshold must be between 0 and 100")
	}
	// Defaults are only materialized when the section is in use, so a plain
	// "quota: {preset: x}" config does not grow an empty enforce block.
	if q.Enforce.Enabled || q.Enforce.Threshold != 0 || q.Enforce.FallbackCooldown != "" {
		if q.Enforce.Threshold == 0 {
			q.Enforce.Threshold = QuotaDefaultThreshold
		}
		if q.Enforce.FallbackCooldown != "" {
			duration, err := time.ParseDuration(q.Enforce.FallbackCooldown)
			if err != nil {
				return fmt.Errorf("providers: quota.enforce.fallbackCooldown is not a duration (%s): %w", q.Enforce.FallbackCooldown, err)
			}
			if duration <= 0 {
				return fmt.Errorf("providers: quota.enforce.fallbackCooldown must be > 0")
			}
		} else {
			q.Enforce.FallbackCooldown = QuotaDefaultFallbackCooldown.String()
		}
	}

	return nil
}

// mergeFrom fills unset fields from a preset; inline values win.
func (q *QuotaConfig) mergeFrom(preset QuotaPreset) {
	if q.Endpoint == "" {
		q.Endpoint = preset.Endpoint
	}
	if q.Method == "" {
		q.Method = preset.Method
	}
	if q.Auth.Style == "" {
		q.Auth = preset.Auth
	}
	if len(q.Headers) == 0 {
		q.Headers = preset.Headers
	}
	if q.TimeoutSeconds == 0 {
		q.TimeoutSeconds = preset.TimeoutSeconds
	}
	if len(q.Windows) == 0 {
		q.Windows = preset.Windows
	}
	if len(q.Extras) == 0 {
		q.Extras = preset.Extras
	}
}

// EffectiveEnforce returns the enforce settings with defaults applied.
func (q *QuotaConfig) EffectiveThreshold() int {
	if q == nil || q.Enforce.Threshold == 0 {
		return QuotaDefaultThreshold
	}
	return q.Enforce.Threshold
}

// FallbackCooldownDuration parses the configured fallback cooldown.
func (q *QuotaConfig) FallbackCooldownDuration() time.Duration {
	if q == nil {
		return QuotaDefaultFallbackCooldown
	}
	if d, err := time.ParseDuration(q.Enforce.FallbackCooldown); err == nil && d > 0 {
		return d
	}
	return QuotaDefaultFallbackCooldown
}

// RefreshIntervalDuration parses the configured refresh interval (0 = manual).
func (q *QuotaConfig) RefreshIntervalDuration() time.Duration {
	if q == nil {
		return 0
	}
	d, err := time.ParseDuration(q.RefreshInterval)
	if err != nil || d < 30*time.Second {
		return 0
	}
	return d
}

// Timeout parses the configured request timeout.
func (q *QuotaConfig) Timeout() time.Duration {
	if q == nil || q.TimeoutSeconds <= 0 {
		return QuotaDefaultTimeoutSeconds * time.Second
	}
	return time.Duration(q.TimeoutSeconds) * time.Second
}

// normalizeQuotaHeaders canonicalizes quota request header names.
func normalizeQuotaHeaders(dst, headers map[string]string) error {
	for name, value := range headers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !validHeaderName(name) {
			return fmt.Errorf("providers: quota.headers entry %q is not a valid HTTP header name", name)
		}
		dst[canonicalHeaderName(name)] = value
	}
	return nil
}

// QuotaPreset is a named quota mapping shipped with the server.
type QuotaPreset struct {
	ID             string            `yaml:"id"`
	Name           string            `yaml:"name"`
	Endpoint       string            `yaml:"endpoint"`
	Method         string            `yaml:"method"`
	Auth           QuotaAuthConfig   `yaml:"auth"`
	Headers        map[string]string `yaml:"headers"`
	TimeoutSeconds int               `yaml:"timeoutSeconds"`
	Windows        []QuotaWindow     `yaml:"windows"`
	Extras         []QuotaExtra      `yaml:"extras"`
}

// QuotaPresetByID resolves a built-in preset.
func QuotaPresetByID(id string) (QuotaPreset, bool) {
	presets, err := QuotaPresets()
	if err != nil {
		return QuotaPreset{}, false
	}
	for _, preset := range presets {
		if preset.ID == id {
			return preset, true
		}
	}
	return QuotaPreset{}, false
}

// QuotaPresetIDs lists built-in preset ids in sorted order.
func QuotaPresetIDs() []string {
	presets, err := QuotaPresets()
	if err != nil {
		return nil
	}
	ids := make([]string, 0, len(presets))
	for _, preset := range presets {
		ids = append(ids, preset.ID)
	}
	sort.Strings(ids)
	return ids
}
