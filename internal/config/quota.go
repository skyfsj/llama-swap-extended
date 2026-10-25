package config

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/dop251/goja"
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
	// preset supplies it. It may start with the {base} placeholder, which is
	// replaced by the provider's own endpoint at load time: a self-hosted
	// aggregator (new-api, sub2api, ...) lives on an operator-chosen host, so
	// its usage path can only be expressed relative to the provider's base.
	// The template is what gets serialized back, so editing the provider's
	// endpoint keeps the usage mapping correct.
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
	// DivideBy scales every numeric extra and window amount by this value.
	// Aggregators report quota in internal units (new-api defaults to
	// 500000 units per USD); 0 keeps the raw value. A per-extra value wins.
	DivideBy float64 `yaml:"divideBy" json:"divideBy,omitempty"`

	// Enforce optionally ties the quota to scheduling; disabled by default.
	Enforce QuotaEnforceConfig `yaml:"enforce" json:"enforce,omitempty"`

	// Script is a JavaScript expression that produces the usage snapshot
	// itself. Two shapes are accepted (see docs/quota-scripts.md):
	//
	//	({ request: {url, method, headers, body}, extractor: (response) => ... })
	//	async (ctx) => ({ windows: [...], extras: [...] })
	//
	// The first mirrors CC Switch's custom usage script, so an existing script
	// can be pasted verbatim; {{baseUrl}} and {{apiKey}} are substituted in the
	// request fields. When set, the script replaces endpoint/windows/extras:
	// it decides what to call and how to read the answer. DivideBy and enforce
	// still apply to its result.
	Script string `yaml:"script" json:"script,omitempty"`
}

// HasScript reports whether the block is driven by a script instead of a
// declarative mapping.
func (q *QuotaConfig) HasScript() bool {
	return q != nil && strings.TrimSpace(q.Script) != ""
}

// QuotaBasePlaceholder starts an endpoint that is relative to the provider's
// own base URL. Self-hosted aggregators live on an operator-chosen host, so a
// preset can only name the path and let the provider supply the origin.
const QuotaBasePlaceholder = "{base}"

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
	// defaults to "percent". When Limit is set it instead names the consumed
	// amount, and the share is computed from used/limit.
	Percent string `yaml:"percent" json:"percent,omitempty"`
	// PercentMode says whether Percent is the used share (default) or the
	// remaining share. It is ignored for amount windows (see Limit).
	PercentMode string `yaml:"percentMode" json:"percentMode,omitempty"`
	// Limit is the field (relative to Path) holding the window's total
	// allowance. Aggregators report absolute counters rather than a
	// percentage, so with Limit set Percent names the used amount and the
	// used share becomes used/limit*100.
	Limit string `yaml:"limit" json:"limit,omitempty"`
	// Status is the field holding a status string (optional).
	Status string `yaml:"status" json:"status,omitempty"`
	// ResetsAt is the field holding the window reset timestamp (optional).
	ResetsAt string `yaml:"resetsAt" json:"resetsAt,omitempty"`
	// Optional marks a window that some responses legitimately omit: a relay
	// reports either a subscription window or a wallet balance, never both.
	// An absent optional window is dropped instead of flagged as a broken
	// mapping, and it does not count toward the "no window matched" check.
	Optional bool `yaml:"optional" json:"optional,omitempty"`
}

// QuotaExtra maps one scalar value shown next to the windows.
type QuotaExtra struct {
	ID    string `yaml:"id" json:"id"`
	Label string `yaml:"label" json:"label,omitempty"`
	Path  string `yaml:"path" json:"path"`
	// DivideBy scales a numeric value before display (0 keeps it as is). It
	// exists because aggregators count quota in internal units: new-api
	// defaults to 500000 units per USD. A suffix belongs in Label ("余额 USD").
	DivideBy float64 `yaml:"divideBy" json:"divideBy,omitempty"`
	// Suffix is appended to the rendered value ("%", " USD", ...).
	Suffix string `yaml:"suffix" json:"suffix,omitempty"`
	// Format converts the value before display. "unix"/"unixMs" turn a
	// timestamp into RFC3339; a timestamp of zero means "never" and is
	// omitted entirely.
	Format string `yaml:"format" json:"format,omitempty"`
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

	script := q.HasScript()
	if script {
		// Only the syntax is checked here: running a user script during config
		// load could hang or perform I/O. Shape errors surface on the quota
		// card (or through the editor's test endpoint), where the operator is
		// already looking.
		q.Script = strings.TrimSpace(q.Script)
		if _, err := goja.Parse("quota-script.js", q.Script); err != nil {
			return fmt.Errorf("providers: quota.script does not parse: %w", err)
		}
	} else {
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
		// A "{base}/path" endpoint is resolved against the provider's own URL at
		// request time; validate the path shape and the assembled URL with a
		// placeholder origin so a malformed mapping still fails at load.
		validationURL := endpoint
		if strings.HasPrefix(endpoint, QuotaBasePlaceholder) {
			rest := strings.TrimPrefix(endpoint, QuotaBasePlaceholder)
			if !strings.HasPrefix(rest, "/") {
				return fmt.Errorf("providers: quota.endpoint must use the form %s/path, got %q", QuotaBasePlaceholder, endpoint)
			}
			validationURL = "http://placeholder" + rest
		}
		parsed, err := url.Parse(validationURL)
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

	if q.DivideBy < 0 {
		return fmt.Errorf("providers: quota.divideBy must be >= 0")
	}

	if !script {
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

			window.Percent = strings.TrimSpace(window.Percent)
			window.Limit = strings.TrimSpace(window.Limit)
			if window.Limit != "" {
				// With limit set, percent names the used counter; the "percent"
				// default would read a field an absolute-counter response does not
				// have.
				if window.Percent == "" {
					return fmt.Errorf("providers: quota.windows[%d] needs percent (the used amount field) when limit is set", i)
				}
				// An amount window computes used/limit, so "remaining" has nothing
				// to invert and would silently do nothing.
				if window.PercentMode == "remaining" {
					return fmt.Errorf("providers: quota.windows[%d] cannot combine limit with percentMode \"remaining\"", i)
				}
				window.PercentMode = QuotaDefaultPercentMode
			} else {
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
			if extra.DivideBy < 0 {
				return fmt.Errorf("providers: quota.extras[%d].divideBy must be >= 0", i)
			}
			switch extra.Format {
			case "", "unix", "unixMs":
			default:
				return fmt.Errorf("providers: quota.extras[%d].format must be \"unix\" or \"unixMs\", got %q", i, extra.Format)
			}
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
	if q.DivideBy == 0 {
		q.DivideBy = preset.DivideBy
	}
	if q.Script == "" {
		q.Script = preset.Script
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

// EndpointFor resolves the configured endpoint against a provider base URL.
// An absolute endpoint is returned unchanged; a "{base}/path" template takes
// the provider's origin and path prefix, so a self-hosted aggregator needs no
// duplicated host in its quota mapping.
func (q *QuotaConfig) EndpointFor(providerBase string) string {
	return ResolveQuotaEndpoint(q.Endpoint, providerBase)
}

// ResolveQuotaEndpoint substitutes {base} in a quota endpoint template.
func ResolveQuotaEndpoint(endpoint, providerBase string) string {
	if !strings.HasPrefix(endpoint, QuotaBasePlaceholder) {
		return endpoint
	}
	base := strings.TrimRight(strings.TrimSpace(providerBase), "/")
	return base + strings.TrimPrefix(endpoint, QuotaBasePlaceholder)
}

// ScaledValue divides a numeric quota value by the configured scale.
func (q *QuotaConfig) ScaledValue(value any) any {
	if q == nil || q.DivideBy <= 0 {
		return value
	}
	return ScaleQuotaValue(value, q.DivideBy)
}

// ScaleQuotaValue divides a numeric value, rounding the result so a unit
// conversion does not surface float noise (0.30000000000000004) in the UI.
func ScaleQuotaValue(value any, divideBy float64) any {
	if divideBy <= 0 {
		return value
	}
	number, ok := value.(float64)
	if !ok {
		return value
	}
	scaled := number / divideBy
	return math.Round(scaled*1e6) / 1e6
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
	ID string `yaml:"id"`
	// Name is the human label the UI offers in its preset dropdown.
	Name string `yaml:"name"`
	// Description explains what the mapping shows and what the operator must
	// supply (a {base} endpoint, a special header, ...).
	Description string `yaml:"description"`
	// Category groups presets in the dropdown ("官方余额", "自建中转", ...).
	Category       string            `yaml:"category"`
	Endpoint       string            `yaml:"endpoint"`
	Method         string            `yaml:"method"`
	Auth           QuotaAuthConfig   `yaml:"auth"`
	Headers        map[string]string `yaml:"headers"`
	TimeoutSeconds int               `yaml:"timeoutSeconds"`
	DivideBy       float64           `yaml:"divideBy"`
	Windows        []QuotaWindow     `yaml:"windows"`
	Extras         []QuotaExtra      `yaml:"extras"`
	// Script drives the snapshot instead of the declarative mapping. An
	// official provider whose balance call is not expressible as a static
	// path (or any provider the operator wants to support) ships as a script
	// preset; the operator can copy it into providers.<id>.quota.script and
	// adapt it.
	Script string `yaml:"script"`
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
