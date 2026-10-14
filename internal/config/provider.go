package config

import (
	"fmt"
	"net/url"
	"sort"
	"strings"
)

// ProviderDictionaryConfig maps provider IDs to their upstream definitions.
type ProviderDictionaryConfig map[string]ProviderConfig

// Provider types. The type controls auth header injection and which client
// endpoints the provider can serve.
const (
	ProviderTypeOpenAI    = "openai"
	ProviderTypeAnthropic = "anthropic"
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
}

// ProviderConfig defines one upstream provider entry.
type ProviderConfig struct {
	Type     string `yaml:"type" json:"type"`
	Endpoint string `yaml:"endpoint" json:"endpoint"`
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
	// BareNamePolicy controls arbitration for the bare (unqualified) model
	// name when a local model shares it: "local-first" (default) keeps local
	// precedence, "provider-first" lets this provider win.
	BareNamePolicy string `yaml:"bareNamePolicy" json:"bareNamePolicy,omitempty"`
	// MaxRetries is how many times a failed request may switch to another
	// key (or provider sharing the same bare name) before giving up.
	MaxRetries int `yaml:"maxRetries" json:"maxRetries,omitempty"`

	Disabled bool `yaml:"disabled" json:"disabled,omitempty"`

	ApiKeys []ProviderKey   `yaml:"apiKeys" json:"-"`
	Models  []ProviderModel `yaml:"models" json:"models"`

	Timeouts TimeoutsConfig `yaml:"timeouts" json:"timeouts"`
}

// StickyEnabled reports whether session affinity is on for this provider.
func (p ProviderConfig) StickyEnabled() bool {
	return p.Sticky == nil || *p.Sticky
}

// ResolveProviderKeyProxy returns the effective proxy for a key: key-level
// overrides provider-level; empty inherits environment proxies.
func (p ProviderConfig) ResolveProviderKeyProxy(key ProviderKey) string {
	if key.Proxy != "" {
		return key.Proxy
	}
	return p.Proxy
}

// GatewayConfig holds gateway-wide settings.
type GatewayConfig struct {
	// StickyTTL is the idle expiry for session affinity entries. Hits slide
	// the expiry forward.
	StickyTTL string `yaml:"stickyTTL" json:"stickyTTL"`
	// Cooldown durations per error class.
	Cooldown GatewayCooldownConfig `yaml:"cooldown" json:"cooldown"`
}

// GatewayCooldownConfig holds cooldown durations (Go duration strings) for
// each error class. Empty values use built-in defaults.
type GatewayCooldownConfig struct {
	// Auth is applied on 401/403 (default 10m).
	Auth string `yaml:"auth" json:"auth"`
	// RateLimit is applied on 429 without a usable Retry-After (default 60s).
	RateLimit string `yaml:"rateLimit" json:"rateLimit"`
	// Transient is applied on 5xx/network errors (default 30s).
	Transient string `yaml:"transient" json:"transient"`
}

// ProviderModelFQN returns the fully qualified routing name for a provider
// model. Provider and model IDs may contain slashes; treat the result as an
// exact configured name.
func ProviderModelFQN(providerID, modelID string) string {
	return providerID + "/" + modelID
}

// ResolveProviderModel resolves a fully qualified provider model name, or a
// bare name when exactly one enabled provider exposes it. When several
// providers expose the same bare name the result is a model pool: found is
// true and pool lists every provider exposing it (the gateway schedules
// between them by priority/weight).
func (c *Config) ResolveProviderModel(search string) (providerID, modelID string, pool []string, found bool) {
	for id, provider := range c.Providers {
		if provider.Disabled {
			continue
		}
		seen := make(map[string]struct{})
		for _, model := range provider.Models {
			if _, duplicate := seen[model.Local]; duplicate {
				continue
			}
			seen[model.Local] = struct{}{}
			if ProviderModelFQN(id, model.Local) == search {
				return id, model.Local, nil, true
			}
			if model.Local == search {
				pool = append(pool, id)
			}
		}
	}
	sort.Strings(pool)
	if len(pool) == 1 {
		return pool[0], search, nil, true
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
		for _, model := range c.Providers[providerID].Models {
			if _, duplicate := seen[model.Local]; duplicate {
				continue
			}
			seen[model.Local] = struct{}{}

			fqn := ProviderModelFQN(providerID, model.Local)
			if existing, ok := fqnTargets[fqn]; ok {
				return fmt.Errorf(
					"provider model name %q is ambiguous between providers.%s model %q and providers.%s model %q",
					fqn, existing, model.Local, providerID, model.Local,
				)
			}
			fqnTargets[fqn] = providerID
			bareOwners[model.Local] = append(bareOwners[model.Local], providerID)
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
	defaults := rawProviderConfig{
		Type:           ProviderTypeOpenAI,
		Sticky:         nil,
		BareNamePolicy: "local-first",
		MaxRetries:     2,
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
	if p.MaxRetries == 0 {
		p.MaxRetries = 2
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

	switch p.Type {
	case ProviderTypeOpenAI, ProviderTypeAnthropic:
	default:
		return fmt.Errorf("providers: type must be one of %q or %q, got %q", ProviderTypeOpenAI, ProviderTypeAnthropic, p.Type)
	}

	endpoint := strings.TrimRight(strings.TrimSpace(p.Endpoint), "/")
	if endpoint == "" {
		return fmt.Errorf("providers: endpoint is required")
	}
	// Users following the OpenAI SDK convention put the version prefix in the
	// base URL, but forwarding appends /v1/... paths to this value. Strip one
	// trailing /v1 so both spellings behave the same.
	endpoint = strings.TrimSuffix(endpoint, "/v1")
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return fmt.Errorf("providers: invalid endpoint URL (%s): %w", p.Endpoint, err)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("providers: endpoint must use http or https, got %q", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("providers: endpoint is missing a host")
	}
	p.Endpoint = endpoint

	if p.BareNamePolicy != "local-first" && p.BareNamePolicy != "provider-first" {
		return fmt.Errorf("providers: bareNamePolicy must be \"local-first\" or \"provider-first\", got %q", p.BareNamePolicy)
	}
	if p.MaxRetries < 0 {
		return fmt.Errorf("providers: maxRetries must be >= 0")
	}
	if p.Weight < 0 || p.Priority < 0 {
		return fmt.Errorf("providers: weight and priority must be >= 0")
	}

	if err := validateProxyValue(p.Proxy); err != nil {
		return fmt.Errorf("providers: %w", err)
	}

	if len(p.ApiKeys) == 0 {
		return fmt.Errorf("providers: at least one apiKey is required")
	}
	for i := range p.ApiKeys {
		key := &p.ApiKeys[i]
		if key.Value == "" && key.ValueEnv == "" {
			return fmt.Errorf("providers: apiKeys[%d] needs a value or valueEnv", i)
		}
		if key.Weight < 0 || key.Priority < 0 {
			return fmt.Errorf("providers: apiKeys[%d]: weight and priority must be >= 0", i)
		}
		if key.MaxConcurrent < 0 {
			return fmt.Errorf("providers: apiKeys[%d]: maxConcurrent must be >= 0", i)
		}
		if err := validateProxyValue(key.Proxy); err != nil {
			return fmt.Errorf("providers: apiKeys[%d]: %w", i, err)
		}
		// Inherit provider defaults.
		if key.Weight == 0 {
			key.Weight = p.Weight
		}
		if key.Priority == 0 {
			key.Priority = p.Priority
		}
	}

	if len(p.Models) == 0 {
		return fmt.Errorf("providers: at least one model is required")
	}
	seen := make(map[string]struct{})
	for i := range p.Models {
		model := &p.Models[i]
		model.Local = strings.TrimSpace(model.Local)
		model.Remote = strings.TrimSpace(model.Remote)
		if model.Local == "" {
			return fmt.Errorf("providers: models[%d] has an empty local name", i)
		}
		if model.Remote == "" {
			model.Remote = model.Local
		}
		if _, duplicate := seen[model.Local]; duplicate {
			return fmt.Errorf("providers: duplicate model %q", model.Local)
		}
		seen[model.Local] = struct{}{}
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
