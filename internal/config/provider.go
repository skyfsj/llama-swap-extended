package config

import (
	"fmt"
	"net/textproto"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"golang.org/x/net/http/httpguts"
)

// ProviderDictionaryConfig maps provider IDs to their upstream definitions.
type ProviderDictionaryConfig map[string]ProviderConfig

// Provider types: one API surface each. The type decides both the auth header
// style and the request dialect, so a client speaking another dialect is
// converted before forwarding (see ProviderAdapter.Protocol to override).
const (
	// ProviderTypeOpenAI is the OpenAI Completions API (/v1/chat/completions).
	ProviderTypeOpenAI = "openai"
	// ProviderTypeOpenAIResponses is the OpenAI Responses API (/v1/responses).
	ProviderTypeOpenAIResponses = "openai-responses"
	// ProviderTypeAnthropic is the Anthropic Messages API (/v1/messages).
	ProviderTypeAnthropic = "anthropic"
	// ProviderTypeGemini is the Google generateContent API (/v1beta/models/...).
	ProviderTypeGemini = "gemini"
	// ProviderTypeGeminiInteractions is Google's stateful Interactions API
	// (/v1beta/interactions): Steps in, an Interaction with a steps[] transcript
	// and modality-broken-down usage out.
	ProviderTypeGeminiInteractions = "gemini-interactions"
)

// ProxyDirect is a sentinel proxy value meaning "bypass all proxies, including
// environment proxies, and dial directly". Useful when a global proxy exists
// but a specific provider must not use it (CLIProxyAPI semantics).
const ProxyDirect = "direct"

// ProviderKey is one upstream credential. The scalar form (plain string) is
// normalized into {Value: ...}. Secrets never serialize to JSON.
type ProviderKey struct {
	Value    string `yaml:"value" json:"-"`
	ValueEnv string `yaml:"valueEnv" json:"valueEnv,omitempty"`
	Weight   int    `yaml:"weight" json:"weight,omitempty"`
	// Priority: higher wins. Keys sharing the highest available priority are
	// used first; lower tiers are only reached when every key in the higher
	// tier is cooling down or saturated.
	Priority      int    `yaml:"priority" json:"priority,omitempty"`
	MaxConcurrent int    `yaml:"maxConcurrent" json:"maxConcurrent,omitempty"`
	Proxy         string `yaml:"proxy" json:"proxy,omitempty"`
	Disabled      bool   `yaml:"disabled" json:"disabled,omitempty"`
}

// ProviderModel maps a local model ID exposed by the gateway to an upstream
// model name. The scalar form (plain string) maps the model to itself.
type ProviderModel struct {
	Local  string `yaml:"local" json:"local"`
	Remote string `yaml:"remote" json:"remote"`
	// Aliases are extra client-facing names for the same upstream model. They
	// are routable (and accepted in access-key allowlists) exactly like Local
	// but are not listed separately in /v1/models.
	Aliases []string `yaml:"aliases" json:"aliases,omitempty"`
	// Proxy is the outbound proxy for this model only (http/https/socks5/
	// socks5h URL, or "direct"). It is the most specific level: empty inherits
	// the adapter, then the credential, then the provider, then the global
	// network proxy.
	Proxy string `yaml:"proxy" json:"proxy,omitempty"`
	// Capabilities declares what the upstream model accepts and produces (the
	// same block a local model uses). The playground uses it to file the model
	// under chat, image, speech or translation tooling; an empty declaration
	// reads as a plain chat model.
	Capabilities ModelCapConfig `yaml:"capabilities,omitempty" json:"capabilities,omitempty"`
	// Pricing pins the pricing-catalog entry used to estimate this model's
	// cost. Without it the estimate resolves the catalog by the provider name
	// (see ProviderConfig.PricingProvider) and the upstream model name.
	Pricing PricingRef `yaml:"pricing" json:"pricing,omitempty"`
}

// Names returns the names the model entry declares for clients: the local name
// first, then its aliases. The upstream name is not part of it — whether it is
// callable too is a provider-level decision, see ProviderConfig.ModelNames.
func (m ProviderModel) Names() []string {
	names := make([]string, 0, 1+len(m.Aliases))
	names = append(names, m.Local)
	names = append(names, m.Aliases...)
	return names
}

// ModelNames lists every name a client may use for one model entry: the local
// name, its aliases, and — unless the provider turns it off — the upstream name.
//
// Exposing the upstream name keeps the vendor's own name callable when a model
// is renamed for clients ("downstream may use an alias or the upstream name"),
// and an entry that sets no local name takes the upstream name anyway. A
// provider that would rather publish only its own names sets
// exposeUpstreamName: false.
func (p ProviderConfig) ModelNames(m ProviderModel) []string {
	return append(m.Names(), p.UpstreamNames(m)...)
}

// UpstreamNames lists the upstream name when it is callable and not already
// declared by the entry (an equal local name, or an alias). The gateway
// registers these in a final pass, so an already-claimed name must not be
// returned here: re-registering it would list the same route twice in the
// bare-name pool and let a request fail over to the provider it just left.
func (p ProviderConfig) UpstreamNames(m ProviderModel) []string {
	if !p.ExposesUpstreamName() {
		return nil
	}
	remote := strings.TrimSpace(m.Remote)
	if remote == "" {
		return nil
	}
	for _, name := range m.Names() {
		if strings.TrimSpace(name) == remote {
			return nil
		}
	}
	return []string{remote}
}

// ExposesUpstreamName reports whether the upstream (remote) name is callable in
// addition to the local name and the aliases. Unset means yes.
func (p ProviderConfig) ExposesUpstreamName() bool {
	return p.ExposeUpstreamName == nil || *p.ExposeUpstreamName
}

// ProviderAdapter is one upstream API surface of a provider: a dialect, a base
// endpoint, and the models served there. A provider without an explicit
// adapters list has a single implicit adapter built from its top-level
// type/endpoint/models.
type ProviderAdapter struct {
	// Name is an optional label for the UI and for error messages.
	Name string `yaml:"name" json:"name,omitempty"`
	// Type is the upstream dialect: openai, anthropic or gemini.
	Type string `yaml:"type" json:"type"`
	// Endpoint is the base URL for this adapter.
	Endpoint string `yaml:"endpoint" json:"endpoint"`
	// Proxy is the outbound proxy for this adapter (http/https/socks5/socks5h
	// URL, or "direct"). It overrides the credential and provider level;
	// empty inherits them, and a model-level proxy overrides this.
	Proxy string `yaml:"proxy" json:"proxy,omitempty"`
	// PathMap rewrites literal request path prefixes before the path is
	// appended to the endpoint; the longest matching prefix wins. It exists
	// for upstreams that expose a dialect under a different path shape.
	PathMap map[string]string `yaml:"pathMap" json:"pathMap,omitempty"`
	// Models are the local -> upstream mappings served by this adapter.
	Models []ProviderModel `yaml:"models" json:"models"`
	// ApiKeys optionally overrides the provider credentials for this adapter.
	ApiKeys []ProviderKey `yaml:"apiKeys" json:"-"`
	// ModelMatch lists glob patterns used to route models pulled from the
	// upstream model list into this adapter (templates).
	ModelMatch []string `yaml:"modelMatch" json:"modelMatch,omitempty"`
	// Default marks the catch-all adapter for pulled models that match no
	// other adapter's ModelMatch patterns. At most one adapter may set it.
	Default bool `yaml:"default" json:"default,omitempty"`
}

// ProviderConfig defines one upstream provider entry.
type ProviderConfig struct {
	// Type/Endpoint/Models are the single-adapter shorthand. When Adapters is
	// set they must be empty and the adapters own the endpoints and models.
	Type     string `yaml:"type" json:"type,omitempty"`
	Endpoint string `yaml:"endpoint" json:"endpoint,omitempty"`
	// Proxy is the provider-level outbound proxy (http/https/socks5/socks5h
	// URL, or "direct"). Key-level proxy overrides it; empty inherits
	// environment proxies.
	Proxy string `yaml:"proxy" json:"proxy,omitempty"`

	// Priority and Weight are defaults inherited by every key that does not
	// set its own.
	Priority int `yaml:"priority" json:"priority,omitempty"`
	Weight   int `yaml:"weight" json:"weight,omitempty"`

	// Sticky enables session affinity for this provider (default true).
	Sticky *bool `yaml:"sticky" json:"sticky,omitempty"`
	// ExposeUpstreamName keeps the upstream model name callable next to the
	// local name and its aliases. Unset (the default) exposes it; false
	// publishes only the names this provider declares.
	ExposeUpstreamName *bool `yaml:"exposeUpstreamName" json:"exposeUpstreamName,omitempty"`
	// BareNamePolicy controls arbitration for the bare (unqualified) model
	// name when a local model shares it: "local-first" (default) keeps local
	// precedence, "provider-first" lets this provider win.
	BareNamePolicy string `yaml:"bareNamePolicy" json:"bareNamePolicy,omitempty"`
	// MaxRetries is how many times one credential may be retried before it is
	// benched and the request continues on the next credential of the chain.
	// A pointer so an explicit 0 ("try each credential once") is distinct from
	// an omitted value ("use the default").
	MaxRetries *int `yaml:"maxRetries" json:"maxRetries,omitempty"`

	Disabled bool `yaml:"disabled" json:"disabled,omitempty"`

	// ModelPrefix is prepended to every model name this provider exposes to
	// clients (routing and /v1/models). Default "" exposes the bare model ID.
	// The legacy provider/model form keeps working as a routing alias.
	ModelPrefix string `yaml:"modelPrefix" json:"modelPrefix,omitempty"`

	// PricingProvider is the pricing-catalog provider id used to price this
	// provider's models when a model entry does not pin its own pricing ref:
	// the estimate looks up (PricingProvider, the upstream model name). Empty
	// falls back to the provider's own id, lowercased — a provider named
	// "DeepSeek" then prices against models.dev's "deepseek" entries without
	// any per-model configuration.
	PricingProvider string `yaml:"pricingProvider" json:"pricingProvider,omitempty"`

	// SessionHeaders are request headers set to the session id on upstream
	// requests: the caller's value when it sent one of the headers listed in
	// ui.activity.session_id, otherwise a value derived from the conversation
	// prefix. Empty means no injection.
	SessionHeaders []string `yaml:"sessionHeaders" json:"sessionHeaders,omitempty"`

	// Headers are static request headers sent upstream. Credential-derived
	// headers (Authorization, x-api-key, x-goog-api-key) and session headers
	// are applied afterwards and win.
	Headers map[string]string `yaml:"headers" json:"headers,omitempty"`

	// Adapters are the provider's upstream API surfaces. Empty means the
	// provider has one implicit adapter from Type/Endpoint/Models.
	Adapters []ProviderAdapter `yaml:"adapters" json:"adapters,omitempty"`
	// Cooldown optionally overrides the gateway-wide cooldown policy for this
	// provider: inherit (default), enabled (optionally with its own durations)
	// or disabled (never bench this provider's credentials).
	Cooldown ProviderCooldownConfig `yaml:"cooldown" json:"cooldown,omitempty"`

	// Quota describes how to read this subscription's usage windows for the
	// UI (and, when enforce is enabled, for credential scheduling).
	Quota *QuotaConfig `yaml:"quota" json:"quota,omitempty"`
	// Template records the provider template this entry was created from. It
	// is provenance only and does not affect routing.
	Template string `yaml:"template" json:"template,omitempty"`

	ApiKeys []ProviderKey   `yaml:"apiKeys" json:"-"`
	Models  []ProviderModel `yaml:"models" json:"models"`

	Timeouts TimeoutsConfig `yaml:"timeouts" json:"timeouts"`
}

// EffectiveAdapters returns the provider's adapters: the configured list, or a
// single implicit adapter built from the top-level type/endpoint/models.
func (p ProviderConfig) EffectiveAdapters() []ProviderAdapter {
	if len(p.Adapters) > 0 {
		return p.Adapters
	}
	return []ProviderAdapter{{
		Type:     p.Type,
		Endpoint: p.Endpoint,
		Models:   p.Models,
	}}
}

// AllModels returns every model the provider serves, flattened across its
// adapters in configuration order.
func (p ProviderConfig) AllModels() []ProviderModel {
	models := make([]ProviderModel, 0, len(p.Models))
	for _, adapter := range p.EffectiveAdapters() {
		models = append(models, adapter.Models...)
	}
	return models
}

// StickyEnabled reports whether session affinity is on for this provider.
func (p ProviderConfig) StickyEnabled() bool {
	return p.Sticky == nil || *p.Sticky
}

// MaxRetriesPerCredential reports how many times one credential may be retried
// before it is benched.
func (p ProviderConfig) MaxRetriesPerCredential() int {
	if p.MaxRetries == nil {
		return 2
	}
	return *p.MaxRetries
}

// intPointer returns a pointer to a copy of value, for optional scalar fields.
func intPointer(value int) *int { return &value }

// ResolveProviderKeyProxy returns the effective proxy for a key: key-level
// overrides provider-level; empty inherits environment proxies.
func (p ProviderConfig) ResolveProviderKeyProxy(key ProviderKey) string {
	if key.Proxy != "" {
		return key.Proxy
	}
	return p.Proxy
}

// Provider cooldown policies.
const (
	// ProviderCooldownInherit uses the gateway-wide cooldown settings.
	ProviderCooldownInherit = "inherit"
	// ProviderCooldownEnabled keeps cooldowns on (optionally with its own
	// durations).
	ProviderCooldownEnabled = "enabled"
	// ProviderCooldownDisabled never benches this provider's credentials: a
	// failure is recorded for observability and the request still fails over,
	// but the credential stays schedulable.
	ProviderCooldownDisabled = "disabled"
)

// ProviderCooldownConfig is the per-provider cooldown policy. The scalar YAML
// form sets just the policy ("inherit", "enabled" or "disabled"); the object
// form also overrides individual durations.
type ProviderCooldownConfig struct {
	Policy string `yaml:"policy" json:"policy,omitempty"`
	// Durations override the gateway-wide values for this provider; empty
	// inherits. Ignored when Policy is disabled.
	Auth      string `yaml:"auth" json:"auth,omitempty"`
	AuthProbe string `yaml:"authProbe" json:"authProbe,omitempty"`
	RateLimit string `yaml:"rateLimit" json:"rateLimit,omitempty"`
	Transient string `yaml:"transient" json:"transient,omitempty"`
}

// UnmarshalYAML accepts the scalar ("disabled") and object forms.
func (c *ProviderCooldownConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar string
	if err := unmarshal(&scalar); err == nil {
		c.Policy = scalar
		return nil
	}
	type rawCooldown ProviderCooldownConfig
	var raw rawCooldown
	if err := unmarshal(&raw); err != nil {
		return fmt.Errorf("provider cooldown must be a policy string or an object: %w", err)
	}
	*c = ProviderCooldownConfig(raw)
	return nil
}

// Disabled reports whether cooldowns are turned off for this provider.
func (c ProviderCooldownConfig) Disabled() bool {
	return strings.EqualFold(strings.TrimSpace(c.Policy), ProviderCooldownDisabled)
}

func (c *ProviderCooldownConfig) normalize() error {
	switch policy := strings.ToLower(strings.TrimSpace(c.Policy)); policy {
	case "":
		c.Policy = ProviderCooldownInherit
	case ProviderCooldownInherit, ProviderCooldownEnabled, ProviderCooldownDisabled:
		c.Policy = policy
	default:
		return fmt.Errorf("providers: cooldown policy must be %q, %q or %q, got %q",
			ProviderCooldownInherit, ProviderCooldownEnabled, ProviderCooldownDisabled, c.Policy)
	}
	return GatewayCooldownConfig{
		Auth: c.Auth, AuthProbe: c.AuthProbe, RateLimit: c.RateLimit, Transient: c.Transient,
	}.Validate()
}

// GatewayConfig holds gateway-wide settings.
type GatewayConfig struct {
	// StickyTTL is the idle expiry for session affinity entries. Hits slide
	// the expiry forward.
	StickyTTL string `yaml:"stickyTTL" json:"stickyTTL"`
	// Cooldown durations per error class.
	Cooldown GatewayCooldownConfig `yaml:"cooldown" json:"cooldown"`
	// Failover decides which upstream responses make the gateway continue on
	// another credential (or another provider serving the same bare model).
	Failover GatewayFailoverConfig `yaml:"failover" json:"failover"`
	// SkipSlow drops providers whose first-token latency is known to be much
	// worse than the alternatives serving the same model.
	SkipSlow GatewaySkipSlowConfig `yaml:"skipSlow" json:"skipSlow,omitempty"`
	// EmptyResponse decides whether a success response that carries no model
	// output is retried on the next credential/provider instead of being
	// handed to the caller.
	EmptyResponse GatewayEmptyResponseConfig `yaml:"emptyResponse" json:"emptyResponse,omitempty"`
}

// GatewayEmptyResponseConfig bounds the "model answered nothing" retry.
//
// A provider occasionally answers 200 and then produces no token at all: a
// lifecycle-only stream, an empty choices array, a truncated tool call. Nothing
// has reached the client at that point, so the request can continue on another
// credential (or provider) instead of leaving the caller with an empty success
// that breaks whatever runs downstream.
type GatewayEmptyResponseConfig struct {
	// Retry turns the behaviour on (default true).
	Retry *bool `yaml:"retry" json:"retry,omitempty"`
	// Hold bounds how long a streaming response may be inspected before its
	// first content. Content usually arrives well before this; the bound keeps
	// a client from waiting on lifecycle frames that never become an answer.
	// After the hold the response is committed and streamed as before.
	// Default 15s; 0 disables stream inspection (whole-body responses are
	// still checked).
	Hold string `yaml:"hold" json:"hold,omitempty"`
}

// DefaultEmptyResponseHold is how long a stream may stay content-free before
// the gateway commits it anyway.
const DefaultEmptyResponseHold = 15 * time.Second

// RetryEnabled reports whether empty responses are retried (default true).
func (c GatewayEmptyResponseConfig) RetryEnabled() bool {
	return c.Retry == nil || *c.Retry
}

// EffectiveHold applies the default to an unset duration.
func (c GatewayEmptyResponseConfig) EffectiveHold() time.Duration {
	if strings.TrimSpace(c.Hold) == "" {
		return DefaultEmptyResponseHold
	}
	// Validate has already rejected unparsable values on the load path;
	// returning the default here keeps programmatically built configs sane.
	d, err := time.ParseDuration(c.Hold)
	if err != nil || d < 0 {
		return DefaultEmptyResponseHold
	}
	return d
}

// Validate rejects a hold that is not a duration.
func (c GatewayEmptyResponseConfig) Validate() error {
	if strings.TrimSpace(c.Hold) == "" {
		return nil
	}
	d, err := time.ParseDuration(c.Hold)
	if err != nil {
		return fmt.Errorf("gateway.emptyResponse.hold: %q is not a duration", c.Hold)
	}
	if d < 0 {
		return fmt.Errorf("gateway.emptyResponse.hold must be >= 0")
	}
	return nil
}

// GatewaySkipSlowConfig makes the gateway prefer the provider that answers
// fastest, instead of only reacting after a slow one has already been tried.
//
// Every observation is a real first-token latency measured on this deployment,
// so the rule adapts to the workload instead of encoding numbers that only fit
// one machine. The default is the relative rule alone: an absolute ceiling
// would have to be guessed, and a wrong guess silently removes capacity.
type GatewaySkipSlowConfig struct {
	// Enabled turns the behaviour on (default true). False keeps every
	// provider a candidate regardless of its observed latency.
	Enabled *bool `yaml:"enabled" json:"enabled,omitempty"`
	// MaxFirstTokenMs skips a provider whose observed first-token latency
	// exceeds this many milliseconds. 0 (default) disables the absolute rule,
	// which is the safer default: the right ceiling depends on the model.
	MaxFirstTokenMs int `yaml:"maxFirstTokenMs" json:"maxFirstTokenMs,omitempty"`
	// RelativeFactor skips a provider whose observed first-token latency is
	// more than this multiple of the fastest candidate's. Default 3.
	//
	// A factor, not a difference: 2s vs 6s and 5s vs 15s are the same kind of
	// gap in practice, and a fixed difference would not express that.
	RelativeFactor float64 `yaml:"relativeFactor" json:"relativeFactor,omitempty"`
	// MinSamples is how many measurements a provider needs before its latency
	// is trusted (default 3). A cold start or one slow request must not be
	// enough to drop a provider, and the gateway itself resets on reload.
	MinSamples int `yaml:"minSamples" json:"minSamples,omitempty"`
	// ProbeInterval bounds how long a skipped provider stays out of the pool
	// before it is sampled again (default 5m, "0s" disables probing).
	//
	// Skipping is self-reinforcing: a provider that is never picked never
	// produces a new measurement, so its smoothed latency freezes at the value
	// that got it skipped while the faster siblings keep refreshing theirs. A
	// provider that recovered would therefore stay excluded forever. Probing
	// admits it back on a timer so recovery is observable at all.
	ProbeInterval string `yaml:"probeInterval" json:"probeInterval,omitempty"`
}

// Skip-slow defaults.
const (
	GatewaySkipSlowDefaultFactor     = 3.0
	GatewaySkipSlowDefaultMinSamples = 3
	GatewaySkipSlowDefaultProbe      = 5 * time.Minute
)

// EnabledValue reports whether the behaviour is on. The default is on, and a
// zero-value config (no skipSlow block at all) is therefore enabled with the
// relative rule — an operator gets the tuning without having to write it.
func (c GatewaySkipSlowConfig) EnabledValue() bool {
	return c.Enabled == nil || *c.Enabled
}

// EffectiveRelativeFactor applies the default.
func (c GatewaySkipSlowConfig) EffectiveRelativeFactor() float64 {
	if c.RelativeFactor == 0 {
		return GatewaySkipSlowDefaultFactor
	}
	return c.RelativeFactor
}

// EffectiveMinSamples applies the default.
func (c GatewaySkipSlowConfig) EffectiveMinSamples() int {
	if c.MinSamples <= 0 {
		return GatewaySkipSlowDefaultMinSamples
	}
	return c.MinSamples
}

// EffectiveProbeInterval applies the default. A negative value means probing is
// disabled; an empty or zero value means "use the default".
func (c GatewaySkipSlowConfig) EffectiveProbeInterval() time.Duration {
	if strings.TrimSpace(c.ProbeInterval) == "" {
		return GatewaySkipSlowDefaultProbe
	}
	d, err := time.ParseDuration(c.ProbeInterval)
	if err != nil {
		return GatewaySkipSlowDefaultProbe
	}
	if d <= 0 {
		return 0
	}
	return d
}

// Validate rejects settings that cannot mean anything, so a typo is reported
// instead of quietly changing routing.
func (c GatewaySkipSlowConfig) Validate() error {
	if c.MaxFirstTokenMs < 0 {
		return fmt.Errorf("gateway.skipSlow.maxFirstTokenMs must be >= 0")
	}
	if c.RelativeFactor < 0 {
		return fmt.Errorf("gateway.skipSlow.relativeFactor must be >= 0")
	}
	if c.MinSamples < 0 {
		return fmt.Errorf("gateway.skipSlow.minSamples must be >= 0")
	}
	if strings.TrimSpace(c.ProbeInterval) != "" {
		if _, err := time.ParseDuration(c.ProbeInterval); err != nil {
			return fmt.Errorf("gateway.skipSlow.probeInterval: %w", err)
		}
	}
	if c.Enabled != nil && *c.Enabled && c.MaxFirstTokenMs == 0 && c.RelativeFactor == 0 {
		// 显式写了 enabled: true 却两个规则都为 0：等于配了但不生效，属于笔误。
		//
		// 判据必须是"显式给了 enabled"，不能用零值判断：整个 skipSlow 块缺失时
		// 得到的也是零值，把它当错误会让每一份既有配置都加载失败——默认开启的
		// 行为不需要用户写任何东西。
		return fmt.Errorf("gateway.skipSlow: set maxFirstTokenMs or relativeFactor, or enabled: false")
	}
	return nil
}

// GatewayFailoverConfig tunes the failover decision. Defaults: every 3xx/4xx/5xx
// response is provider-specific enough to be worth another credential, except
// 413 (payload too large, which every upstream repeats), and a request may
// continue on another provider that serves the same bare model name.
type GatewayFailoverConfig struct {
	// CrossProvider allows one request to continue on another provider that
	// serves the same bare model name. Default true. Names qualified with a
	// provider (provider/model) always stay pinned to that provider.
	CrossProvider *bool `yaml:"crossProvider" json:"crossProvider,omitempty"`
	// Statuses replaces the default eligible set, e.g. [429, 500, 502, 503].
	// Empty keeps the default.
	Statuses []int `yaml:"statuses" json:"statuses,omitempty"`
	// ExcludeStatuses removes statuses from the effective set, e.g. [400, 413]
	// to hand client-input errors straight back.
	ExcludeStatuses []int `yaml:"excludeStatuses" json:"excludeStatuses,omitempty"`
}

// Validate rejects impossible failover statuses early so a typo cannot
// silently disable protection.
func (f GatewayFailoverConfig) Validate() error {
	check := func(field string, values []int) error {
		for _, status := range values {
			if status < 100 || status > 599 {
				return fmt.Errorf("gateway.failover.%s: %d is not an HTTP status code", field, status)
			}
		}
		return nil
	}
	if err := check("statuses", f.Statuses); err != nil {
		return err
	}
	if err := check("excludeStatuses", f.ExcludeStatuses); err != nil {
		return err
	}
	return nil
}

// CrossProviderEnabled reports whether failover may switch providers.
func (f GatewayFailoverConfig) CrossProviderEnabled() bool {
	return f.CrossProvider == nil || *f.CrossProvider
}

// GatewayCooldownConfig holds cooldown durations (Go duration strings) for
// each error class. Empty values use built-in defaults.
type GatewayCooldownConfig struct {
	// Auth is applied on 401/402 (default 10m).
	Auth string `yaml:"auth" json:"auth"`
	// AuthProbe is how long the FIRST 401 benches a credential before the full
	// auth cooldown applies. Upstreams occasionally answer 401 transiently, so
	// the credential is retried soon; a repeated 401 within the escalation
	// window (2 minutes) applies Auth instead (default 30s).
	// Set to "0s" to disable probing: every 401 gets the full Auth cooldown.
	AuthProbe string `yaml:"authProbe" json:"authProbe,omitempty"`
	// RateLimit is applied on 429 without a usable Retry-After (default 60s).
	RateLimit string `yaml:"rateLimit" json:"rateLimit"`
	// Transient is applied on 5xx/network errors (default 30s).
	Transient string `yaml:"transient" json:"transient"`
}

// Validate rejects unparsable cooldown durations so a typo cannot silently
// fall back to the default (which would hide the mistake).
func (c GatewayCooldownConfig) Validate() error {
	for _, entry := range []struct {
		field string
		value string
	}{
		{"auth", c.Auth},
		{"authProbe", c.AuthProbe},
		{"rateLimit", c.RateLimit},
		{"transient", c.Transient},
	} {
		raw := strings.TrimSpace(entry.value)
		if raw == "" {
			continue
		}
		duration, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("gateway.cooldown.%s: %q is not a duration", entry.field, entry.value)
		}
		if duration < 0 {
			return fmt.Errorf("gateway.cooldown.%s must be >= 0", entry.field)
		}
	}
	return nil
}

// ProviderModelFQN returns the legacy fully qualified routing name for a
// provider model. It stays routable as an alias; the client-facing name is
// ProviderModelName(ModelPrefix, modelID).
func ProviderModelFQN(providerID, modelID string) string {
	return providerID + "/" + modelID
}

// ProviderModelName returns the client-facing name of a provider model: the
// provider's prefix plus the local model ID (no prefix by default).
func ProviderModelName(prefix, modelID string) string {
	return prefix + modelID
}

// ExposedName returns the client-facing name of one model of this provider.
func (p ProviderConfig) ExposedName(modelID string) string {
	return ProviderModelName(p.ModelPrefix, modelID)
}

// ResolveProviderModel resolves a fully qualified provider model name, or a
// bare name when exactly one enabled provider exposes it. When several
// providers expose the same bare name the result is a model pool: found is
// true and pool lists every provider exposing it (the gateway schedules
// between them by priority/weight).
func (c *Config) ResolveProviderModel(search string) (providerID, modelID string, pool []string, found bool) {
	// poolModels maps a pool member back to its local model ID: with a
	// modelPrefix the client-facing name differs from the configured ID.
	poolModels := make(map[string]string)
	for id, provider := range c.Providers {
		if provider.Disabled {
			continue
		}
		seen := make(map[string]struct{})
		for _, model := range provider.AllModels() {
			for _, name := range provider.ModelNames(model) {
				// Two models of one provider may collapse to the same
				// client-facing name when a prefix makes them collide; keep the
				// first.
				exposed := provider.ExposedName(name)
				if _, duplicate := seen[exposed]; duplicate {
					continue
				}
				seen[exposed] = struct{}{}
				if ProviderModelFQN(id, name) == search {
					return id, model.Local, nil, true
				}
				if exposed == search {
					if _, exists := poolModels[id]; !exists {
						pool = append(pool, id)
					}
					poolModels[id] = model.Local
				}
			}
		}
	}
	sort.Strings(pool)
	if len(pool) == 1 {
		return pool[0], poolModels[pool[0]], nil, true
	}
	if len(pool) > 1 {
		return "", search, pool, true
	}
	return "", "", nil, false
}

// ValidateProviderNamespace ensures provider fully qualified names do not
// collide with models, peers, aliases, selectors or profiles, and that bare
// names are reserved consistently. Bare-name overlap between providers is
// allowed (model pool) but a bare name must not collide with a peer bare name
// where ambiguity would silently change routing.
func ValidateProviderNamespace(c Config) error {
	fqnTargets := make(map[string]string) // fqn -> providerID
	bareOwners := make(map[string][]string)

	providerIDs := make([]string, 0, len(c.Providers))
	for providerID := range c.Providers {
		providerIDs = append(providerIDs, providerID)
	}
	sort.Strings(providerIDs)

	for _, providerID := range providerIDs {
		seen := make(map[string]struct{})
		provider := c.Providers[providerID]
		for _, model := range provider.AllModels() {
			for _, name := range provider.ModelNames(model) {
				exposed := provider.ExposedName(name)
				if _, duplicate := seen[exposed]; duplicate {
					continue
				}
				seen[exposed] = struct{}{}

				fqn := ProviderModelFQN(providerID, name)
				if existing, ok := fqnTargets[fqn]; ok && existing != providerID {
					return fmt.Errorf(
						"provider model name %q is ambiguous between providers.%s and providers.%s",
						fqn, existing, providerID,
					)
				}
				fqnTargets[fqn] = providerID
				// Bare-name ownership follows the client-facing name: several
				// providers may share one (model pool), but it must not clash
				// with peer model names.
				bareOwners[exposed] = append(bareOwners[exposed], providerID)
			}
		}
	}

	for fqn, providerID := range fqnTargets {
		if _, ok := c.Models[fqn]; ok {
			return fmt.Errorf("model ID %q conflicts with fully qualified provider model name (providers.%s)", fqn, providerID)
		}
		for modelID, model := range c.Models {
			for _, alias := range model.Aliases {
				if alias == fqn {
					return fmt.Errorf("model %s alias %q conflicts with fully qualified provider model name", modelID, fqn)
				}
			}
		}
		if _, ok := c.aliases[fqn]; ok {
			return fmt.Errorf("model alias %q conflicts with fully qualified provider model name", fqn)
		}
		if _, ok := c.Selectors[fqn]; ok {
			return fmt.Errorf("selector ID %q conflicts with fully qualified provider model name", fqn)
		}
		for profileID, profile := range c.Profiles {
			if _, ok := profile.Pins[fqn]; ok {
				return fmt.Errorf("profile %s pin %q conflicts with fully qualified provider model name", profileID, fqn)
			}
		}
		for peerID, peer := range c.Peers {
			for _, modelID := range peer.Models {
				if PeerModelFQN(peerID, modelID) == fqn {
					return fmt.Errorf("provider model name %q conflicts with peer model name (peers.%s)", fqn, peerID)
				}
			}
		}
	}

	// A bare name may be shared by several providers (pool), but it must not
	// also be a peer bare name: peer routes require uniqueness and would
	// silently lose against or win over the pool depending on order.
	for bare, owners := range bareOwners {
		for peerID, peer := range c.Peers {
			for _, modelID := range peer.Models {
				if modelID == bare {
					return fmt.Errorf(
						"model %q is exposed by providers (%s) and peers.%s; rename one of them",
						bare, strings.Join(owners, ","), peerID,
					)
				}
			}
		}
	}

	return nil
}

func (p *ProviderConfig) UnmarshalYAML(unmarshal func(interface{}) error) error {
	type rawProviderConfig ProviderConfig
	// Type is deliberately left empty here: an adapter-style provider must not
	// get the single-adapter default, which would look like both spellings
	// being configured. normalize() applies the openai default when no
	// adapters are present.
	defaults := rawProviderConfig{
		Sticky:         nil,
		BareNamePolicy: "local-first",
		MaxRetries:     intPointer(2),
		ApiKeys:        []ProviderKey{},
		Models:         []ProviderModel{},
		Timeouts: TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: 600,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		},
	}

	if err := unmarshal(&defaults); err != nil {
		return err
	}

	*p = ProviderConfig(defaults)
	return p.normalize()
}

// normalize validates and fills derived fields. It is separate from
// UnmarshalYAML so programmatically-built configs (tests, API-applied YAML)
// get the same checks.
func (p *ProviderConfig) normalize() error {
	if p.BareNamePolicy == "" {
		p.BareNamePolicy = "local-first"
	}
	if p.MaxRetries == nil {
		p.MaxRetries = intPointer(2)
	}
	if p.Timeouts == (TimeoutsConfig{}) {
		p.Timeouts = TimeoutsConfig{
			Connect:        30,
			KeepAlive:      30,
			ResponseHeader: 600,
			TLSHandshake:   10,
			ExpectContinue: 1,
			IdleConn:       90,
		}
	}

	if len(p.Adapters) > 0 {
		// The adapters own the endpoints and models; the single-adapter
		// shorthand must not also be set (that would silently shadow one of
		// the two).
		if p.Type != "" || p.Endpoint != "" || len(p.Models) > 0 {
			return fmt.Errorf("providers: adapters cannot be combined with top-level type/endpoint/models; move them into the adapter entries")
		}
	} else {
		if p.Type == "" {
			p.Type = ProviderTypeOpenAI
		}
		adapter := ProviderAdapter{Type: p.Type, Endpoint: p.Endpoint, Models: p.Models}
		if err := p.normalizeAdapter("the provider", &adapter); err != nil {
			return err
		}
		p.Type, p.Endpoint, p.Models = adapter.Type, adapter.Endpoint, adapter.Models
	}

	defaults := 0
	for i := range p.Adapters {
		label := fmt.Sprintf("adapters[%d]", i)
		if name := strings.TrimSpace(p.Adapters[i].Name); name != "" {
			label = fmt.Sprintf("adapter %q", name)
			p.Adapters[i].Name = name
		}
		if err := p.normalizeAdapter(label, &p.Adapters[i]); err != nil {
			return err
		}
		if p.Adapters[i].Default {
			defaults++
		}
	}
	if defaults > 1 {
		return fmt.Errorf("providers: at most one adapter may set default: true")
	}
	if err := validateModelUniqueness(p.EffectiveAdapters()); err != nil {
		return err
	}

	if p.BareNamePolicy != "local-first" && p.BareNamePolicy != "provider-first" {
		return fmt.Errorf("providers: bareNamePolicy must be \"local-first\" or \"provider-first\", got %q", p.BareNamePolicy)
	}
	if p.MaxRetries == nil || *p.MaxRetries < 0 {
		return fmt.Errorf("providers: maxRetries must be >= 0")
	}
	if p.Weight < 0 || p.Priority < 0 {
		return fmt.Errorf("providers: weight and priority must be >= 0")
	}

	if err := validateProxyValue(p.Proxy); err != nil {
		return fmt.Errorf("providers: %w", err)
	}

	if prefix := strings.TrimSpace(p.ModelPrefix); prefix != p.ModelPrefix || strings.ContainsAny(p.ModelPrefix, "/ 	") {
		return fmt.Errorf("providers: modelPrefix must not contain slashes or whitespace, got %q", p.ModelPrefix)
	}

	sessionHeaders := make([]string, 0, len(p.SessionHeaders))
	seenSessionHeaders := make(map[string]struct{}, len(p.SessionHeaders))
	for _, header := range p.SessionHeaders {
		header = strings.TrimSpace(header)
		if header == "" {
			continue
		}
		if !httpguts.ValidHeaderFieldName(header) {
			return fmt.Errorf("providers: sessionHeaders entry %q is not a valid HTTP header name", header)
		}
		canonical := textproto.CanonicalMIMEHeaderKey(header)
		key := strings.ToLower(canonical)
		if _, duplicate := seenSessionHeaders[key]; duplicate {
			continue
		}
		seenSessionHeaders[key] = struct{}{}
		sessionHeaders = append(sessionHeaders, canonical)
	}
	p.SessionHeaders = sessionHeaders

	normalizedHeaders := make(map[string]string, len(p.Headers))
	if err := normalizeStaticHeaders(normalizedHeaders, p.Headers); err != nil {
		return err
	}
	p.Headers = normalizedHeaders

	if err := normalizeProviderKeys(p.ApiKeys, "providers", p); err != nil {
		return err
	}
	if len(p.ApiKeys) == 0 && !p.hasAdapterKeys() {
		return fmt.Errorf("providers: at least one apiKey is required")
	}

	if len(p.AllModels()) == 0 {
		return fmt.Errorf("providers: at least one model is required")
	}

	if err := p.Cooldown.normalize(); err != nil {
		return err
	}

	if err := p.Quota.normalize(); err != nil {
		return err
	}

	return nil
}

// hasAdapterKeys reports whether any adapter carries its own credentials.
func (p ProviderConfig) hasAdapterKeys() bool {
	for _, adapter := range p.Adapters {
		if len(adapter.ApiKeys) > 0 {
			return true
		}
	}
	return false
}

// normalizeAdapter validates one adapter in place. The label appears in error
// messages ("the provider" for the implicit single adapter).
func (p *ProviderConfig) normalizeAdapter(label string, adapter *ProviderAdapter) error {
	if adapter.Type == "" {
		adapter.Type = ProviderTypeOpenAI
	}
	switch adapter.Type {
	case ProviderTypeOpenAI, ProviderTypeOpenAIResponses, ProviderTypeAnthropic, ProviderTypeGemini, ProviderTypeGeminiInteractions:
	default:
		return fmt.Errorf("providers: %s: type must be one of %q, %q, %q, %q or %q, got %q",
			label, ProviderTypeOpenAI, ProviderTypeOpenAIResponses, ProviderTypeAnthropic, ProviderTypeGemini, ProviderTypeGeminiInteractions, adapter.Type)
	}

	endpoint := strings.TrimRight(strings.TrimSpace(adapter.Endpoint), "/")
	if endpoint == "" {
		return fmt.Errorf("providers: %s: endpoint is required", label)
	}
	// Users following the OpenAI SDK convention put the version prefix in the
	// base URL, but forwarding appends /v1/... paths to this value. Strip one
	// trailing /v1 so both spellings behave the same.
	endpoint = strings.TrimSuffix(endpoint, "/v1")
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("providers: %s: invalid endpoint URL (%s): %w", label, adapter.Endpoint, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("providers: %s: endpoint must use http or https, got %q", label, parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("providers: %s: endpoint is missing a host", label)
	}
	adapter.Endpoint = endpoint

	if err := validateProxyValue(adapter.Proxy); err != nil {
		return fmt.Errorf("providers: %s: %w", label, err)
	}

	for from, to := range adapter.PathMap {
		if !strings.HasPrefix(strings.TrimSpace(from), "/") || !strings.HasPrefix(strings.TrimSpace(to), "/") {
			return fmt.Errorf("providers: %s: pathMap entries must be path prefixes starting with %q", label, "/")
		}
	}

	for i, pattern := range adapter.ModelMatch {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return fmt.Errorf("providers: %s: modelMatch[%d] is empty", label, i)
		}
		if _, err := path.Match(pattern, "probe"); err != nil {
			return fmt.Errorf("providers: %s: modelMatch[%d] is not a valid glob pattern: %w", label, i, err)
		}
		adapter.ModelMatch[i] = pattern
	}

	for i := range adapter.Models {
		model := &adapter.Models[i]
		model.Local = strings.TrimSpace(model.Local)
		model.Remote = strings.TrimSpace(model.Remote)
		if model.Local == "" {
			return fmt.Errorf("providers: %s: models[%d] has an empty local name", label, i)
		}
		if model.Remote == "" {
			model.Remote = model.Local
		}
		if err := validateProxyValue(model.Proxy); err != nil {
			return fmt.Errorf("providers: %s: models[%d] (%s): %w", label, i, model.Local, err)
		}
		aliases := make([]string, 0, len(model.Aliases))
		seenAliases := map[string]struct{}{strings.ToLower(model.Local): {}}
		for _, alias := range model.Aliases {
			alias = strings.TrimSpace(alias)
			if alias == "" {
				continue
			}
			key := strings.ToLower(alias)
			if _, duplicate := seenAliases[key]; duplicate {
				continue
			}
			seenAliases[key] = struct{}{}
			aliases = append(aliases, alias)
		}
		model.Aliases = aliases
	}

	if err := normalizeProviderKeys(adapter.ApiKeys, fmt.Sprintf("providers: %s", label), p); err != nil {
		return err
	}
	return nil
}

// normalizeProviderKeys validates credential entries and applies provider
// defaults for weight/priority.
func normalizeProviderKeys(keys []ProviderKey, prefix string, provider *ProviderConfig) error {
	for i := range keys {
		key := &keys[i]
		if key.Value == "" && key.ValueEnv == "" {
			return fmt.Errorf("%s: apiKeys[%d] needs a value or valueEnv", prefix, i)
		}
		if key.Weight < 0 || key.Priority < 0 {
			return fmt.Errorf("%s: apiKeys[%d]: weight and priority must be >= 0", prefix, i)
		}
		if key.MaxConcurrent < 0 {
			return fmt.Errorf("%s: apiKeys[%d]: maxConcurrent must be >= 0", prefix, i)
		}
		if err := validateProxyValue(key.Proxy); err != nil {
			return fmt.Errorf("%s: apiKeys[%d]: %w", prefix, i, err)
		}
		// Inherit provider defaults.
		if key.Weight == 0 {
			key.Weight = provider.Weight
		}
		if key.Priority == 0 {
			key.Priority = provider.Priority
		}
	}
	return nil
}

// normalizeStaticHeaders canonicalizes header names in place and rejects names
// owned by the credential layer, which would otherwise be silently
// overwritten when the request is forwarded.
func normalizeStaticHeaders(dst, headers map[string]string) error {
	for name, value := range headers {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !httpguts.ValidHeaderFieldName(name) {
			return fmt.Errorf("providers: headers entry %q is not a valid HTTP header name", name)
		}
		canonical := textproto.CanonicalMIMEHeaderKey(name)
		switch strings.ToLower(canonical) {
		case "authorization", "x-api-key", "x-goog-api-key", "proxy-authorization":
			return fmt.Errorf("providers: headers must not set %q; credential headers are managed by the gateway", canonical)
		}
		dst[canonical] = value
	}
	return nil
}

// validateModelUniqueness accepts a name declared more than once, in one
// adapter or across several: a name that appears on two adapters is the same
// model exposed by two API surfaces (the gateway picks the surface matching the
// client's dialect), and a name that appears twice inside one adapter resolves
// to the model that declares it as its own name before any alias claims it.
// Duplicates are resolved when routes are built, not rejected here.
func validateModelUniqueness(adapters []ProviderAdapter) error {
	for _, adapter := range adapters {
		for _, model := range adapter.Models {
			if err := model.Capabilities.Validate(); err != nil {
				return fmt.Errorf("providers: model %q: %w", model.Local, err)
			}
			for _, name := range model.Names() {
				if strings.TrimSpace(name) == "" {
					label := adapter.Name
					if label == "" {
						label = adapter.Type
					}
					return fmt.Errorf("providers: adapter %q declares an empty model name", label)
				}
			}
		}
	}
	return nil
}

// validateProxyValue accepts http/https/socks5/socks5h proxy URLs, the
// "direct" sentinel, or empty (inherit environment).
func validateProxyValue(proxy string) error {
	proxy = strings.TrimSpace(proxy)
	if proxy == "" || proxy == ProxyDirect {
		return nil
	}
	parsed, err := url.Parse(proxy)
	if err != nil {
		return fmt.Errorf("invalid proxy URL (%s): %w", proxy, err)
	}
	switch parsed.Scheme {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy must use http, https, socks5 or socks5h, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("proxy URL is missing a host")
	}
	return nil
}

// UnmarshalYAML for the models list accepts scalars (local==remote),
// single-pair shorthand mappings ({localName: upstreamName}), or objects
// ({local:, remote:}).
func (m *ProviderModel) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar string
	if err := unmarshal(&scalar); err == nil {
		m.Local = scalar
		m.Remote = scalar
		return nil
	}
	var mapping map[string]string
	if err := unmarshal(&mapping); err == nil && len(mapping) == 1 {
		for local, remote := range mapping {
			m.Local = local
			m.Remote = remote
		}
		return nil
	}
	type rawProviderModel ProviderModel
	var raw rawProviderModel
	if err := unmarshal(&raw); err != nil {
		return fmt.Errorf("provider model must be a string, a {local: remote} mapping, or an object: %w", err)
	}
	*m = ProviderModel(raw)
	return nil
}

// UnmarshalYAML for apiKeys entries accepts plain strings or structured
// objects.
func (k *ProviderKey) UnmarshalYAML(unmarshal func(interface{}) error) error {
	var scalar string
	if err := unmarshal(&scalar); err == nil {
		k.Value = scalar
		return nil
	}
	type rawProviderKey ProviderKey
	var raw rawProviderKey
	if err := unmarshal(&raw); err != nil {
		return fmt.Errorf("apiKey must be a string or an object: %w", err)
	}
	*k = ProviderKey(raw)
	return nil
}
