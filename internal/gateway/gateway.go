// Package gateway implements the upstream provider forwarding path: a
// scheduler that picks one of a provider's API keys per request, and an
// HTTP forwarder with streaming-bootstrap retry semantics.
//
// Scheduling follows the "filter at selection time" model (no enable/disable
// state machines): every credential carries cooldown timestamps and a
// concurrency semaphore, and each pick filters out credentials that are
// cooling down or saturated. Candidates are grouped into priority tiers; the
// highest tier with an available credential is chosen, weighted round-robin
// within the tier. Session affinity overrides the rotation when the session's
// bound credential still passes all filters.
package gateway

import (
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/proxy"

	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/logmon"
	"github.com/mostlygeek/llama-swap/internal/netproxy"
	protocolrelaykit "github.com/mostlygeek/llama-swap/internal/protocol/relaykit"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

// ErrNoProviderModelFound is returned when a request names a model no
// provider serves.
var ErrNoProviderModelFound = errors.New("no provider model found")

// Default cooldown durations, overridable via gateway.cooldown config.
const (
	DefaultAuthCooldown      = 10 * time.Minute
	DefaultRateLimitCooldown = 60 * time.Second
	DefaultTransientCooldown = 30 * time.Second
	DefaultStickyTTL         = time.Hour

	// DefaultAuthProbeCooldown is how long the first 401 benches a credential.
	// Upstreams occasionally answer 401 transiently (edge hiccups, rotating
	// gateways), so the credential is retried soon instead of being parked for
	// the full auth cooldown; any further 401 escalates to the full cooldown
	// until an operator clears it. Configurable via
	// gateway.cooldown.authProbe ("0s" disables probing).
	DefaultAuthProbeCooldown = 30 * time.Second
)

// Route is one schedulable provider model: a scheduler over the credentials
// allowed to serve it plus the upstream mapping.
type Route struct {
	ProviderID string
	// AdapterName labels the provider adapter (API surface) this route is
	// configured on. Empty for a provider with the single implicit adapter.
	AdapterName string
	// ModelID is the local model ID as configured under the provider's adapter.
	ModelID string
	// ExposedName is the client-facing model name: the provider's modelPrefix
	// plus ModelID (the bare ID when no prefix is configured). Requests may
	// also use the legacy provider/model form, and every alias of the model
	// entry is routable too (see ExposedNameFor).
	ExposedName string
	// ModelAliases are extra client-facing names configured on the model entry.
	ModelAliases []string
	// Capabilities is the model entry's capability declaration, surfaced in
	// /v1/models so clients file provider models like local ones.
	Capabilities config.ModelCapConfig
	// prefix is the provider's modelPrefix, kept to build alias names.
	prefix string
	// RemoteName is the upstream model name sent to the provider.
	RemoteName string
	// ProviderFirst marks bareNamePolicy=provider-first.
	ProviderFirst bool
	// Sticky controls session affinity for this route.
	Sticky bool
	// MaxRetries is how many times one credential may be retried before it is
	// benched and the request continues on the next credential of the chain.
	MaxRetries int
	// SessionHeaders are request headers set to the session id on every
	// upstream attempt; an empty session id leaves them untouched.
	SessionHeaders []string
	// StaticHeaders are sent upstream on every attempt, before the credential
	// and session headers (which win on conflicts).
	StaticHeaders map[string]string
	// proxy overrides the credential/provider egress for this route (model
	// level beats adapter level). Empty inherits the credential's proxy.
	proxy string
	// Members are the credentials serving this route. For bare-name pools
	// this spans several providers; the credential ID prefix identifies the
	// origin provider.
	Members []*Credential

	sched *scheduler
	// cooldowns is the provider's effective cooldown configuration (gateway
	// defaults merged with any provider override); cooldownDisabled turns
	// benching off entirely for this provider.
	cooldowns        cooldownConfig
	cooldownDisabled bool
	ptype            providerType
	typeName         string
	pathMap          map[string]string
	endpoint         *url.URL
	timeouts         transportTimeouts
}

// Key identifies the upstream surface a route belongs to: provider, adapter
// (API surface) and model. Session affinity binds to this key rather than to
// the provider alone, because a provider's surfaces keep separate upstream
// caches: the same conversation sent to the Responses adapter and to the Chat
// adapter is cached twice.
func (r *Route) Key() string {
	if r == nil {
		return ""
	}
	return r.ProviderID + "\x00" + r.AdapterName + "\x00" + r.ModelID
}

// Available reports whether any credential of this route can serve a request
// right now (not disabled, not cooling down, not at its concurrency cap).
func (r *Route) Available() bool {
	if r == nil {
		return false
	}
	return r.sched.hasCandidate(nil)
}

// poolBid returns the priority and weight this route's provider bids with in a
// bare-name pool. The provider's highest-priority key speaks for the whole
// provider: its adapters share one account and one quota, so a provider with
// several surfaces must not look more important than its keys say. Keys that
// are cooling down are only considered when nothing is available, which keeps
// the order stable while the pool reports the real upstream state.
func (r *Route) poolBid() (priority, weight int) {
	if r == nil {
		return 0, 0
	}
	now := time.Now()
	var availableBest, anyBest *Credential
	for _, cred := range r.Members {
		if anyBest == nil || cred.Priority > anyBest.Priority {
			anyBest = cred
		}
		if !cred.available(now) {
			continue
		}
		if availableBest == nil || cred.Priority > availableBest.Priority {
			availableBest = cred
		}
	}
	best := availableBest
	if best == nil {
		best = anyBest
	}
	if best == nil {
		return 0, 0
	}
	return best.Priority, best.Weight
}

// egressProxy resolves the proxy a request uses: the route's model/adapter
// override when set, otherwise the credential's (key or provider) proxy.
func (r *Route) egressProxy(cred *Credential) string {
	if r.proxy != "" {
		return r.proxy
	}
	if cred == nil {
		return ""
	}
	return cred.Proxy
}

// ExposedNameFor returns the client-facing name of one of the route's model
// names (the primary name plus any aliases).
func (r *Route) ExposedNameFor(name string) string {
	if r.prefix == "" || name == "" {
		return name
	}
	return r.prefix + name
}

// EgressProxy reports the configured override for this route ("" inherits the
// credential), for status and diagnostics.
func (r *Route) EgressProxy() string { return r.proxy }

// Type returns the route's upstream dialect ("openai", "anthropic" or
// "gemini"), i.e. the adapter's configured type.
func (r *Route) Type() string { return r.typeName }

// Adapter returns the adapter name the route belongs to ("" for the implicit
// single-adapter provider).
func (r *Route) Adapter() string { return r.AdapterName }

type providerType int

const (
	providerOpenAI providerType = iota
	providerAnthropic
	providerGemini
)

// Gateway forwards requests to configured upstream providers.
type Gateway struct {
	mu         sync.RWMutex
	routes     map[string]*Route // FQN provider/model -> route
	bareRoutes map[string][]*Route
	// providerCredentials holds every configured credential per provider,
	// including keys disabled in configuration. Disabled keys never reach a
	// route's member list (the scheduler must not pick them), but the quota
	// card and the status API still address them: a disabled key keeps its
	// subscription, and its balance is exactly what an operator parks a key to
	// watch.
	providerCredentials map[string][]*Credential
	sticky              *stickyTable
	cooldown            cooldownConfig
	failover            failoverPolicy
	skipSlow            skipSlowPolicy
	emptyResponse       emptyResponsePolicy
	latency             *latencyTable
	logger              *logmon.Monitor
	transports          *transportCache

	// pools carries one rotation cursor per bare model name: a pool's
	// selection must remember its previous pick across requests, otherwise a
	// zero-weight pool always starts from the same sorted candidate. The map is
	// a sync.Map because every request looks its own cursor up, and a global
	// mutex there serialized planning across all models.
	pools sync.Map // map[string]*poolRotator

	// shutdown cancels the background loops the Gateway starts. Shutdown
	// replaces the sticky table, so a sweep that is still running would write
	// into the discarded one.
	shutdown   context.Context
	shutdownFn context.CancelFunc
}

// failoverPolicy decides which upstream responses are worth another attempt
// and whether a request may continue on another provider.
type failoverPolicy struct {
	crossProvider bool
	// statuses nil means the built-in default set; otherwise only these.
	statuses map[int]struct{}
	exclude  map[int]struct{}
}

// maxFailoverAttempts bounds the total attempts of one request regardless of
// how many credentials the failover chain holds.
const maxFailoverAttempts = 12

// eligible reports whether an upstream status should trigger another attempt
// instead of being handed to the client.
func (p failoverPolicy) eligible(status int) bool {
	if status < 300 || status > 599 {
		return false
	}
	if _, excluded := p.exclude[status]; excluded {
		return false
	}
	if p.statuses != nil {
		_, ok := p.statuses[status]
		return ok
	}
	// Default: every error status is provider-specific enough to be worth one
	// more credential, except a payload the upstream will reject everywhere.
	return status != http.StatusRequestEntityTooLarge
}

func resolveFailover(fc config.GatewayFailoverConfig) failoverPolicy {
	policy := failoverPolicy{crossProvider: fc.CrossProviderEnabled()}
	if len(fc.Statuses) > 0 {
		policy.statuses = make(map[int]struct{}, len(fc.Statuses))
		for _, status := range fc.Statuses {
			policy.statuses[status] = struct{}{}
		}
	}
	if len(fc.ExcludeStatuses) > 0 {
		policy.exclude = make(map[int]struct{}, len(fc.ExcludeStatuses))
		for _, status := range fc.ExcludeStatuses {
			policy.exclude[status] = struct{}{}
		}
	}
	return policy
}

type cooldownConfig struct {
	auth, authProbe, rateLimit, transient time.Duration
	stickyTTL                             time.Duration
}

// New builds a Gateway from configuration. Credentials whose valueEnv
// variable is unset are kept but marked unschedulable, so a missing env var
// degrades one key instead of failing the whole config.
func New(cfg config.Config, logger *logmon.Monitor) (*Gateway, error) {
	g := &Gateway{
		routes:              make(map[string]*Route),
		bareRoutes:          make(map[string][]*Route),
		providerCredentials: make(map[string][]*Credential),
		logger:              logger,
		transports:          newTransportCache(),
	}
	g.cooldown = resolveCooldowns(cfg.Gateway)
	g.failover = resolveFailover(cfg.Gateway.Failover)
	g.skipSlow = resolveSkipSlow(cfg.Gateway.SkipSlow)
	g.emptyResponse = resolveEmptyResponse(cfg.Gateway.EmptyResponse)
	g.latency = newLatencyTable()
	g.sticky = newStickyTable(g.cooldown.stickyTTL)
	g.shutdown, g.shutdownFn = context.WithCancel(context.Background())
	if g.skipSlow.enabled && logger != nil {
		logger.Infof("gateway: skip-slow providers enabled (%s)", g.skipSlow.describe())
	}

	providerIDs := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)

	for _, id := range providerIDs {
		pcfg := cfg.Providers[id]

		// Credentials exist for every configured provider, including one
		// that is switched off: quota cards read each key's own subscription.
		// One Credential per key id, shared by every adapter of the provider:
		// cooldowns, in-flight counts, quota parks and the sticky binding must
		// describe the key, not the API surface it was reached through.
		credentialsByID := newProviderCredentials(id, pcfg)
		g.providerCredentials[id] = sortedCredentials(credentialsByID)

		if pcfg.Disabled {
			continue
		}

		providerCooldowns, cooldownDisabled, cerr := resolveProviderCooldowns(g.cooldown, pcfg.Cooldown)
		if cerr != nil {
			return nil, fmt.Errorf("providers.%s: %w", id, cerr)
		}

		providerKeys := pcfg.ApiKeys
		providerRoutes := make([]providerRoute, 0, len(pcfg.AllModels()))
		for _, adapter := range pcfg.EffectiveAdapters() {
			endpoint, err := url.Parse(adapter.Endpoint)
			if err != nil {
				return nil, fmt.Errorf("providers.%s: invalid endpoint: %w", id, err)
			}

			ptype, err := providerTypeFor(adapter.Type)
			if err != nil {
				return nil, fmt.Errorf("providers.%s: %w", id, err)
			}

			// An adapter may carry its own credentials; otherwise it shares
			// the provider's key set.
			keys := providerKeys
			if len(adapter.ApiKeys) > 0 {
				keys = adapter.ApiKeys
			}

			members := make([]*Credential, 0, len(keys))
			for _, keyCfg := range keys {
				cred := credentialsByID[keyID(id, valueOrEnvRef(resolveKeyValue(keyCfg), keyCfg.ValueEnv))]
				if cred == nil {
					continue
				}
				if !cred.Disabled {
					members = append(members, cred)
				}
			}

			for _, model := range adapter.Models {
				// Most specific wins: model proxy, then adapter proxy, then
				// (empty here) the credential/provider resolution at attempt
				// time.
				routeProxy := model.Proxy
				if routeProxy == "" {
					routeProxy = adapter.Proxy
				}
				route := &Route{
					ProviderID:       id,
					AdapterName:      adapter.Name,
					ModelID:          model.Local,
					ExposedName:      config.ProviderModelName(pcfg.ModelPrefix, model.Local),
					ModelAliases:     model.Aliases,
					prefix:           pcfg.ModelPrefix,
					RemoteName:       model.Remote,
					Capabilities:     model.Capabilities,
					ProviderFirst:    pcfg.BareNamePolicy == "provider-first",
					Sticky:           pcfg.StickyEnabled(),
					MaxRetries:       pcfg.MaxRetriesPerCredential(),
					SessionHeaders:   pcfg.SessionHeaders,
					StaticHeaders:    pcfg.Headers,
					proxy:            routeProxy,
					cooldowns:        providerCooldowns,
					cooldownDisabled: cooldownDisabled,
					Members:          members,
					ptype:            ptype,
					typeName:         adapter.Type,
					pathMap:          adapter.PathMap,
					endpoint:         endpoint,
					timeouts: transportTimeouts{
						connect:        pcfg.Timeouts.Connect,
						keepAlive:      pcfg.Timeouts.KeepAlive,
						responseHeader: pcfg.Timeouts.ResponseHeader,
						tlsHandshake:   pcfg.Timeouts.TLSHandshake,
						idleConn:       pcfg.Timeouts.IdleConn,
					},
				}
				route.sched = newScheduler(members)
				providerRoutes = append(providerRoutes, providerRoute{route: route, model: model})
			}
		}

		// Register the names in three passes: a model's own name claims the
		// exact-name route before any alias of the same provider competes for
		// it, whichever adapter declared it, and the upstream name (when the
		// provider exposes it, the default) claims last so it can never shadow a
		// name the provider declares. A name that loses simply stays out of
		// g.routes (the bare-name pool below still lists every surface that
		// serves the name).
		for _, names := range []func(config.ProviderModel) []string{
			func(model config.ProviderModel) []string { return []string{model.Local} },
			func(model config.ProviderModel) []string { return model.Aliases },
			func(model config.ProviderModel) []string { return pcfg.UpstreamNames(model) },
		} {
			for _, entry := range providerRoutes {
				route := entry.route
				for _, name := range names(entry.model) {
					name = strings.TrimSpace(name)
					if name == "" {
						continue
					}
					fqn := config.ProviderModelFQN(id, name)
					if existing, exists := g.routes[fqn]; exists {
						if existing.ProviderID != id {
							return nil, fmt.Errorf("providers.%s: model %q is already served by provider %s",
								id, fqn, existing.ProviderID)
						}
						// The name is already claimed inside this provider: by
						// another adapter exposing the same model, or by an
						// alias that met the model's own name in the pass
						// above. Every surface still serves it through the
						// pool.
						g.bareRoutes[route.ExposedNameFor(name)] = append(g.bareRoutes[route.ExposedNameFor(name)], route)
						continue
					}
					g.routes[fqn] = route
					// The client-facing name (prefix + name) resolves through
					// bareRoutes so that two providers exposing the same name
					// arbitrate as a pool, exactly like a shared bare model name
					// does. The legacy provider/model name above keeps working as
					// a pinned alias.
					g.bareRoutes[route.ExposedNameFor(name)] = append(g.bareRoutes[route.ExposedNameFor(name)], route)
				}
			}
		}
	}
	return g, nil
}

// providerRoute pairs a built route with the model entry it came from, so name
// registration can order model names before alias names.
type providerRoute struct {
	route *Route
	model config.ProviderModel
}

// providerTypeFor maps a configured provider type onto the credential
// injection strategy. Config validation already rejects unknown types; this
// guard keeps programmatically built configs honest.
func providerTypeFor(providerType string) (providerType, error) {
	switch providerType {
	case "", config.ProviderTypeOpenAI:
		return providerOpenAI, nil
	case config.ProviderTypeOpenAIResponses:
		// Responses and Completions share the OpenAI auth style.
		return providerOpenAI, nil
	case config.ProviderTypeAnthropic:
		return providerAnthropic, nil
	case config.ProviderTypeGemini:
		return providerGemini, nil
	case config.ProviderTypeGeminiInteractions:
		// The Interactions API authenticates exactly like generateContent
		// (x-goog-api-key) and speaks a different body dialect.
		return providerGemini, nil
	default:
		return providerOpenAI, fmt.Errorf("unknown provider type %q", providerType)
	}
}

// resolveProviderCooldowns merges a provider's cooldown override into the
// gateway-wide configuration. Disabled turns benching off for the provider
// (durations become irrelevant).
func resolveProviderCooldowns(base cooldownConfig, override config.ProviderCooldownConfig) (cooldownConfig, bool, error) {
	effective := base
	apply := func(field, raw string, target *time.Duration) error {
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		duration, err := time.ParseDuration(raw)
		if err != nil {
			return fmt.Errorf("cooldown.%s: %q is not a duration", field, raw)
		}
		if duration < 0 {
			return fmt.Errorf("cooldown.%s must be >= 0", field)
		}
		*target = duration
		return nil
	}
	if err := apply("auth", override.Auth, &effective.auth); err != nil {
		return base, false, err
	}
	if err := apply("authProbe", override.AuthProbe, &effective.authProbe); err != nil {
		return base, false, err
	}
	if err := apply("rateLimit", override.RateLimit, &effective.rateLimit); err != nil {
		return base, false, err
	}
	if err := apply("transient", override.Transient, &effective.transient); err != nil {
		return base, false, err
	}
	return effective, override.Disabled(), nil
}

func resolveCooldowns(gw config.GatewayConfig) cooldownConfig {
	cc := cooldownConfig{
		auth:      DefaultAuthCooldown,
		authProbe: DefaultAuthProbeCooldown,
		rateLimit: DefaultRateLimitCooldown,
		transient: DefaultTransientCooldown,
		stickyTTL: DefaultStickyTTL,
	}
	if d, err := time.ParseDuration(gw.StickyTTL); err == nil && d > 0 {
		cc.stickyTTL = d
	}
	if d, err := time.ParseDuration(gw.Cooldown.Auth); err == nil && d > 0 {
		cc.auth = d
	}
	if strings.TrimSpace(gw.Cooldown.AuthProbe) != "" {
		// AuthProbe is explicitly configurable: "0s" disables probing (every
		// 401 gets the full auth cooldown).
		if d, err := time.ParseDuration(gw.Cooldown.AuthProbe); err == nil && d >= 0 {
			cc.authProbe = d
		}
	}
	if d, err := time.ParseDuration(gw.Cooldown.RateLimit); err == nil && d > 0 {
		cc.rateLimit = d
	}
	if d, err := time.ParseDuration(gw.Cooldown.Transient); err == nil && d > 0 {
		cc.transient = d
	}
	return cc
}

func valueOrEnvRef(value, envName string) string {
	if value != "" {
		return value
	}
	return "env:" + envName
}

// resolveKeyValue reads a key's secret, falling back to its valueEnv variable.
func resolveKeyValue(key config.ProviderKey) string {
	if key.Value != "" {
		return key.Value
	}
	if key.ValueEnv == "" {
		return ""
	}
	return os.Getenv(key.ValueEnv)
}

// CredentialID derives the stable id a provider key gets, exactly like the
// gateway's own credential construction. Callers outside this package (the
// quota refresher) need it to address a credential that belongs to a disabled
// key, which never reaches a route.
func CredentialID(providerID string, key config.ProviderKey) string {
	return keyID(providerID, valueOrEnvRef(resolveKeyValue(key), key.ValueEnv))
}

// newProviderCredentials builds one Credential per distinct key of a provider,
// covering provider-level keys and adapter-owned key sets. Disabled keys are
// created too: they stay out of routing but keep reporting their subscription.
func newProviderCredentials(providerID string, pcfg config.ProviderConfig) map[string]*Credential {
	credentials := make(map[string]*Credential)
	add := func(keys []config.ProviderKey) {
		for _, keyCfg := range keys {
			credID := CredentialID(providerID, keyCfg)
			if _, exists := credentials[credID]; exists {
				continue
			}
			credentials[credID] = &Credential{
				ID:            credID,
				Value:         resolveKeyValue(keyCfg),
				Priority:      keyCfg.Priority,
				Weight:        keyCfg.Weight,
				MaxConcurrent: keyCfg.MaxConcurrent,
				Proxy:         pcfg.ResolveProviderKeyProxy(keyCfg),
				Disabled:      keyCfg.Disabled,
			}
		}
	}
	add(pcfg.ApiKeys)
	for _, adapter := range pcfg.EffectiveAdapters() {
		add(adapter.ApiKeys)
	}
	return credentials
}

// sortedCredentials returns the credentials ordered by id, so the status API
// and the quota targets are stable across rebuilds.
func sortedCredentials(credentials map[string]*Credential) []*Credential {
	out := make([]*Credential, 0, len(credentials))
	for _, cred := range credentials {
		out = append(out, cred)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ProviderCredentials returns every configured credential of a provider,
// including keys disabled in configuration. The returned slice is a copy; the
// Credentials themselves are shared with the scheduler.
func (g *Gateway) ProviderCredentials(providerID string) []*Credential {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	stored := g.providerCredentials[providerID]
	if len(stored) == 0 {
		return nil
	}
	return append([]*Credential(nil), stored...)
}

// Handles reports whether the gateway serves this model name (FQN or bare).
func (g *Gateway) Handles(model string) bool {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if _, ok := g.routes[model]; ok {
		return true
	}
	return len(g.bareRoutes[model]) > 0
}

// RouteFor resolves a request model name to a route. FQNs route exactly;
// bare names arbitrate across the pool: routes with bareNamePolicy
// provider-first outrank local models (localHasModel), then priority/weight
// picks among the remaining candidates.
// FirstRouteOfType returns any route served by a provider of the given type.
// It exists for control-plane endpoints that are not model-scoped (an
// Interactions retrieval, for example): the dialect decides which provider can
// answer, not a model name.
func (g *Gateway) FirstRouteOfType(providerType string) *Route {
	if g == nil {
		return nil
	}
	g.mu.RLock()
	defer g.mu.RUnlock()
	ids := make([]string, 0, len(g.routes))
	for id := range g.routes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		route := g.routes[id]
		if route == nil || len(route.Members) == 0 {
			continue
		}
		if route.typeName == providerType {
			return route
		}
	}
	return nil
}

// RouteFor answers "which route would serve this model" for dispatch
// decisions: it is deterministic and side-effect free (no rotation), so the
// forwarding path that follows keeps full control of the pool's cursor.
//
// A provider-qualified name resolves to its pinned route even when every
// credential of that provider is parked: the forwarder then reports the real
// upstream state. A bare-name pool yields nil only when no candidate at all is
// usable, which is what makes the dispatcher answer "all keys are cooling
// down" instead of picking a dead route.
func (g *Gateway) RouteFor(model string, localHasModel bool) *Route {
	g.mu.RLock()
	exact, pinned := g.routes[model]
	g.mu.RUnlock()
	if pinned {
		return exact
	}

	routes := g.PoolRoutes(model, localHasModel, "")
	if len(routes) == 0 {
		return nil
	}
	if len(routes) > 1 {
		routes = filterSlowRoutes(routes, g.skipSlow, g.latency, g.logger)
	}
	for _, route := range routes {
		if route.Available() {
			return route
		}
	}
	return nil
}

// Credentials returns the route's credential set (status API).
func (r *Route) Credentials() []*Credential {
	return r.Members
}

// BestCredential returns the route's highest-priority available credential
// (used for cross-provider arbitration and health reporting).
func (r *Route) BestCredential() *Credential {
	r.sched.mu.Lock()
	defer r.sched.mu.Unlock()
	now := time.Now()
	var best *Credential
	for _, c := range r.Members {
		if !c.available(now) {
			continue
		}
		if best == nil || c.Priority > best.Priority {
			best = c
		}
	}
	return best
}

// ModelIDs lists one route name per configured provider model: its primary
// client-facing name. Aliases are routable but are not separate models.
func (g *Gateway) ModelIDs() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ids := make([]string, 0, len(g.routes))
	seen := make(map[*Route]struct{}, len(g.routes))
	for _, route := range g.routes {
		if _, duplicate := seen[route]; duplicate {
			continue
		}
		seen[route] = struct{}{}
		ids = append(ids, config.ProviderModelFQN(route.ProviderID, route.ModelID))
	}
	sort.Strings(ids)
	return ids
}

// RouteByID returns a route by exact FQN (status API).
func (g *Gateway) RouteByID(fqn string) *Route {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.routes[fqn]
}

// AllRoutes snapshots the route table (status API).
func (g *Gateway) AllRoutes() []*Route {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]*Route, 0, len(g.routes))
	for _, fqn := range sortedKeys(g.routes) {
		out = append(out, g.routes[fqn])
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// stickySweepInterval caps how often expired session bindings are dropped. The
// sweep is tied to the sticky TTL, because an entry that outlived its TTL is
// unreachable anyway and waking more often than the TTL would be pointless.
const stickySweepInterval = time.Minute

// startStickySweep drops expired session bindings in the background. A binding
// is removed lazily when its session comes back, which never happens for a
// session that does not return, so without this sweep the table would grow for
// as long as the daemon runs. Only entries already past their TTL are removed,
// so live sessions keep their provider.
func (g *Gateway) startStickySweep() {
	if g.shutdown == nil {
		return
	}
	interval := g.cooldown.stickyTTL
	if interval <= 0 || interval > stickySweepInterval {
		interval = stickySweepInterval
	}
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-g.shutdown.Done():
				return
			case <-ticker.C:
				g.mu.RLock()
				sticky := g.sticky
				g.mu.RUnlock()
				if sticky == nil {
					return
				}
				sticky.sweepExpired(time.Now())
			}
		}
	}()
}

// stickyTable returns the live sticky table, or nil once the Gateway has been
// shut down.
func (g *Gateway) stickyTable() *stickyTable {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.sticky
}

// Shutdown drops sticky state and releases cached transports' idle
// connections.
func (g *Gateway) Shutdown() {
	if g.shutdownFn != nil {
		g.shutdownFn()
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sticky = newStickyTable(g.cooldown.stickyTTL)
	// The rotation cursors are per-model and carry no resource: dropping the
	// map is enough, and each cursor is recreated on the next request.
	g.pools = sync.Map{}
	g.transports.close()
	g.startStickySweep()
}

// outcome describes what one forwarding attempt concluded.
type outcome int

const (
	// outcomeCommit: the response was handed to the client (success or a
	// request-scoped upstream error). Terminal.
	outcomeCommit outcome = iota
	// outcomeRetry: the credential failed before any byte reached the
	// client; cool it down and try the next credential.
	outcomeRetry
	// outcomeStop: terminal without a response (client disconnected).
	outcomeStop
)

// Session identifies the caller of one forwarded request.
type Session struct {
	// Hash is the sticky affinity key. Empty disables affinity for the request.
	Hash string
	// Model is the client-facing model the request asked for. Affinity is
	// tracked per conversation *and* model so a session that talks to several
	// models keeps one binding per model instead of starving the others.
	Model string
	// ID is the session id echoed upstream under each route session header.
	// The server fills it with the caller's session id when present, otherwise
	// with a hash derived from the conversation prefix. Empty injects nothing.
	ID string
}

// affinityKey is the sticky-table key for this request. Empty when the caller
// sent nothing that identifies a session.
func (s Session) affinityKey() string {
	if s.Hash == "" {
		return ""
	}
	return s.Hash + "\x00" + s.Model
}

// ServeHTTP forwards a request to the resolved route's provider. The caller
// resolves the model (swaputil.FetchContext) and passes the caller's session
// identity; the gateway rewrites the model name for whichever route it
// actually forwards to (each attempt, so failover can change providers).
func (g *Gateway) ServeHTTP(w http.ResponseWriter, req *http.Request, route *Route, session Session) {
	g.ServeHTTPWithFailover(w, req, []*Route{route}, session)
}

// AttemptOptions tunes one failover pass.
type AttemptOptions struct {
	// StopOnRequestScoped returns an upstream error that is scoped to the
	// request (a 4xx that is not credential-related) to the caller instead of
	// writing it to the client, so a caller with another upstream surface to
	// try can retry there.
	StopOnRequestScoped bool
	// StopOnUnavailable returns "this route has no usable credential" to the
	// caller instead of writing a 502, so a caller with more providers in its
	// pool can continue there. Without it a single cooling provider would
	// answer for the whole pool.
	StopOnUnavailable bool
}

// AttemptOutcome reports how one pass ended.
type AttemptOutcome struct {
	// Committed is true when a response was written to the client.
	Committed bool
	// Status and Snippet describe the upstream error when nothing was written.
	Status  int
	Snippet string
}

// ServeHTTPAttempt runs one failover pass and reports the outcome.
func (g *Gateway) ServeHTTPAttempt(w http.ResponseWriter, req *http.Request, chain []*Route, session Session, opts AttemptOptions) AttemptOutcome {
	return g.serveHTTP(w, req, chain, session, opts)
}

// ServeHTTPWithFailover forwards a request along an ordered failover chain:
// the resolved route first, then the other providers serving the same bare
// model name (see FailoverRoutes). Every attempt stays invisible until the
// upstream produces data, so a failure can continue on the next credential or
// provider.
func (g *Gateway) ServeHTTPWithFailover(w http.ResponseWriter, req *http.Request, chain []*Route, session Session) {
	g.serveHTTP(w, req, chain, session, AttemptOptions{})
}

func (g *Gateway) serveHTTP(w http.ResponseWriter, req *http.Request, chain []*Route, session Session, opts AttemptOptions) AttemptOutcome {
	if len(chain) == 0 {
		swaputil.SendResponse(w, req, http.StatusBadGateway, "gateway: no route for request")
		return AttemptOutcome{Committed: true, Status: http.StatusBadGateway}
	}
	g.mu.RLock()
	cooldown := g.cooldown
	g.mu.RUnlock()
	primary := chain[0]

	// Session affinity: a bound credential short-circuits rotation on the
	// first attempt only; if it fails the retry loop re-schedules normally.
	// The binding is route-level (provider + adapter + model), so it applies
	// only when this pass targets that same surface: the caller orders the
	// pool from that binding (PlanRoutes), and a pass for another surface must
	// not hijack the session back to an upstream cache it is not using.
	//
	// boundRoute is remembered separately from bound: whether the bound
	// surface could serve this request at all decides if a successful answer on
	// another surface may replace the binding (see the commit case below). A
	// binding that expired is treated as absent rather than unusable, because
	// there is then nothing to preserve.
	// boundRouteKey is the session's binding at the start of this request, when
	// it applies to the surface this pass targets. It decides whether a
	// successful answer on another surface may take the session over (see the
	// commit case): a session whose bound provider failed has to follow the
	// provider that answered, or it would keep returning to the one that is
	// down. A credential that is cooling down, parked or at its concurrency cap
	// is still listed in its route's members, so membership alone must not count
	// as healthy.
	var bound *Credential
	boundRouteKey := ""
	boundRouteHealthy := true
	if primary.Sticky && session.Hash != "" {
		if entry, ok := g.sticky.get(session.affinityKey(), time.Now()); ok && entry.route == primary.Key() {
			for _, c := range primary.Members {
				if c.ID == entry.credentialID {
					bound, boundRouteKey = c, entry.route
					boundRouteHealthy = c.available(time.Now())
					break
				}
			}
		}
	}

	// maxRetries is a per-credential budget, not a per-request one: one
	// credential may be tried maxRetries+1 times before it is benched and the
	// request moves on to the next credential of the chain. Retrying the same
	// credential is what lets a single-key provider ride out a transient
	// failure instead of failing on the first 5xx, while a pool still walks
	// its keys in order.
	maxRetries := primary.MaxRetries
	if maxRetries < 0 {
		maxRetries = 0
	}
	attempts := primary.MaxRetries + 1
	if credits := chainCredentialCount(chain); credits*(maxRetries+1) > attempts {
		attempts = credits * (maxRetries + 1)
	}
	if attempts > maxFailoverAttempts {
		attempts = maxFailoverAttempts
	}
	if attempts < 1 {
		attempts = 1
	}

	// tried holds the credentials that are done for this request: they either
	// used their whole retry budget or failed in a way no retry can fix.
	tried := map[string]struct{}{}
	credentialAttempts := make(map[string]int, len(primary.Members))
	retrySleep := time.Duration(0)
	var (
		lastStatus  int
		lastSnippet string
		lastRoute   *Route
		// retryCred/retryRoute carry the credential the next iteration must
		// retry. A transient failure spends one unit of that credential's
		// budget instead of benching it, so a single-key provider can recover
		// from a hiccup within one request.
		retryCred  *Credential
		retryRoute *Route
	)

	// Retryable attempts must re-send the request body. httptest and plain
	// handlers do not always populate GetBody, so buffer once here and
	// rebuild the body per attempt.
	req, cancelBody, err := bufferRequestBody(req)
	if err != nil {
		swaputil.SendResponse(w, req, http.StatusBadRequest, err.Error())
		return AttemptOutcome{Committed: true, Status: http.StatusBadRequest}
	}
	defer cancelBody()

	for attempt := 0; attempt < attempts; attempt++ {
		cred := bound
		route := primary
		if retryCred != nil {
			cred, route = retryCred, retryRoute
			retryCred, retryRoute = nil, nil
			// Hammering a struggling upstream immediately is how a retry makes
			// things worse. Back off between attempts on the same credential,
			// bounded both per wait and across the whole request so a
			// misconfigured budget cannot stall the caller.
			if wait := g.retryBackoff(credentialAttempts[cred.ID], route); wait > 0 {
				if remaining := maxTotalRetryBackoff - retrySleep; remaining < wait {
					wait = remaining
				}
				if wait > 0 {
					retrySleep += wait
					timer := time.NewTimer(wait)
					select {
					case <-timer.C:
					case <-req.Context().Done():
						timer.Stop()
						return AttemptOutcome{Committed: true, Status: http.StatusGatewayTimeout}
					}
				}
			}
		}
		bound = nil
		if cred == nil {
			cred, route = g.pickAcrossChain(chain, tried, time.Now())
		}
		if cred == nil || route == nil {
			break
		}

		release, ok := cred.acquire()
		if !ok {
			tried[cred.ID] = struct{}{}
			continue
		}

		// The in-flight table attributes a request while it runs, which the
		// answer-time publish cannot do: a slow upstream would leave the row
		// unattributed for the whole wait, and a failover would keep pointing
		// at the attempt that is no longer being tried.
		swaputil.ReportInferenceAttempt(req.Context(), route.ProviderID, route.AdapterName)
		// A provider request has no local process to wait for, so the initial
		// "waiting for process readiness" phase is wrong for it: from here the
		// request is waiting on the upstream. The next phase is driven by the
		// actual first content, not by the first byte (lifecycle frames such as
		// response.created or message_start are not output).
		swaputil.ReportInferencePhase(req.Context(), "upstream", "waiting for the provider's response")

		res := g.attempt(w, req, route, cred, session, opts)
		release()

		switch res.outcome {
		case outcomeCommit:
			// Success (2xx/3xx or a committed request-scoped error): bind the
			// session to the surface and credential that served it. The
			// binding survives a key rotation within the same surface: only a
			// different provider/adapter costs the session its upstream cache.
			//
			// A session that already had a binding is only re-bound when that
			// binding is no longer usable: it moved off its provider because
			// that provider could not serve it, and it has since built its
			// cache on the one that answered. Pulling it back when the original
			// recovers would throw that cache away, so the new provider keeps
			// the session until it fails too.
			if res.status < 400 && route.Sticky && session.Hash != "" {
				// The session moves when its own provider could not serve this
				// request — either it was already cooling down when the request
				// started, or it failed during it (the first attempt is the
				// bound credential, so a retry that lands elsewhere proves it).
				boundUnavailable := boundRouteKey != "" && (!boundRouteHealthy || boundRouteKey != route.Key())
				g.sticky.bind(session.affinityKey(), route.ProviderID, route.Key(), cred.ID, time.Now(), boundUnavailable)
			}
			return AttemptOutcome{Committed: true, Status: res.status}
		case outcomeStop:
			return AttemptOutcome{Committed: true, Status: res.status}
		case outcomeRetry:
			credentialAttempts[cred.ID]++
			lastStatus = res.status
			lastSnippet = res.snippet
			lastRoute = route
			// Whether the credential is benched now depends on the failure:
			// a verdict on the credential (401/402/429) or a request-scoped
			// rejection ends its participation at once, because retrying it
			// cannot help. A transient failure (5xx, transport, truncated
			// stream) instead spends one unit of its retry budget; only when
			// that budget is gone is the credential benched and the request
			// handed to the next one.
			spent := credentialAttempts[cred.ID] > maxRetries
			// The request-wide cap can end the loop while a credential still
			// has budget left. Leaving it less than fully spent would let the
			// next request hammer a key that is in fact failing, so a cut-off
			// credential is benched exactly like an exhausted one.
			lastIteration := attempt == attempts-1
			switch {
			case res.skipCooldown:
				// A request-scoped rejection says nothing about the credential:
				// record it and drop it for this request without benching.
				g.applyFailure(cred, res, route, cooldown)
				tried[cred.ID] = struct{}{}
			case !res.retryableOnCredential || spent || lastIteration:
				g.applyFailure(cred, res, route, cooldown)
				tried[cred.ID] = struct{}{}
			default:
				// Not benched yet: keep the credential schedulable and retry
				// it. The upstream error is still recorded on it so the status
				// API shows what happened.
				g.noteFailureOnly(cred, res)
				retryCred, retryRoute = cred, route
			}
			if g.logger != nil {
				left := maxRetries - credentialAttempts[cred.ID] + 1
				if left < 0 {
					left = 0
				}
				// The upstream's own words are the difference between "it said
				// 400" and "it refused the history because reasoning_content was
				// missing": an operator debugging a failover needs the second.
				g.logger.Warnf("gateway: provider %s adapter %q model %s credential %s failed (%s), %d retry attempt(s) left for this credential: %s",
					route.ProviderID, route.Adapter(), route.ModelID, cred.ID, res.cause, left, oneLine(res.snippet, 400))
			}
		}
	}

	if lastStatus != 0 && lastRoute != nil {
		// Every attempt failed. The request still went to (and was refused by)
		// a real provider, and that is exactly the row an operator needs to
		// attribute an outage: report the last route tried, not nothing.
		//
		// Only the pass that answers the client may do so. The record holder is
		// first-publisher-wins, and a caller that will try another surface
		// (StopOnRequestScoped) may still land on a route that produces the
		// answer: publishing here would pin the row to a route that never
		// served anything. A fully exhausted pool is covered by the caller,
		// which attributes the last candidate it tried.
		if !opts.StopOnRequestScoped {
			swaputil.PublishRequestProvider(req.Context(), lastRoute.ProviderID, lastRoute.AdapterName)
		}
		if opts.StopOnRequestScoped {
			return AttemptOutcome{Committed: false, Status: lastStatus, Snippet: lastSnippet}
		}
		swaputil.SendResponse(w, req, lastStatus, truncate(lastSnippet, 2048))
		return AttemptOutcome{Committed: true, Status: lastStatus}
	}
	// Nothing was schedulable this pass: every credential of the chain is
	// cooling down, parked or disabled. The upstream has already answered this
	// account recently, so report that answer rather than a synthetic 502 — a
	// client whose keys all return 401 should keep seeing 401, not a shape it
	// cannot interpret. Fall back to the synthetic error only when the chain
	// never received an upstream answer at all (every key disabled or with an
	// unset env var, say).
	if status, snippet := chainLastUpstream(chain); status != 0 {
		// Same rule as the exhausted chain above: a pass that hands the request
		// onward leaves the record open, so the route that eventually answers
		// owns the row instead of a benched chain that never served it.
		if route := chain[0]; route != nil && !opts.StopOnUnavailable {
			swaputil.PublishRequestProvider(req.Context(), route.ProviderID, route.AdapterName)
		}
		if opts.StopOnUnavailable {
			// Another provider may still serve this request; the caller decides
			// whether to continue instead of answering from here.
			return AttemptOutcome{Committed: false, Status: status, Snippet: snippet}
		}
		swaputil.SendResponse(w, req, status, truncate(snippet, 2048))
		return AttemptOutcome{Committed: true, Status: status}
	}
	snippet := fmt.Sprintf("gateway: no available credential for provider %s model %s", primary.ProviderID, primary.ModelID)
	if opts.StopOnUnavailable {
		// Another provider may still serve this request; the caller decides
		// whether to continue instead of committing to a 502 here.
		return AttemptOutcome{Committed: false, Status: http.StatusBadGateway, Snippet: snippet}
	}
	swaputil.SendResponse(w, req, http.StatusBadGateway, snippet)
	return AttemptOutcome{Committed: true, Status: http.StatusBadGateway}
}

// chainLastUpstream returns the most recent upstream answer recorded by any
// credential of the chain, so a pass that could not schedule anything still
// reports the real reason (status 0 when the chain never heard from upstream).
func chainLastUpstream(chain []*Route) (int, string) {
	var (
		bestStatus int
		bestBody   string
		bestAt     time.Time
	)
	for _, route := range chain {
		if route == nil {
			continue
		}
		for _, cred := range route.Members {
			status, snippet := cred.LastUpstream()
			if status == 0 {
				continue
			}
			at := cred.LastErrorAt
			if at.IsZero() || !at.After(bestAt) {
				continue
			}
			bestAt, bestStatus, bestBody = at, status, snippet
		}
	}
	return bestStatus, bestBody
}

// FailoverRoutes returns the routes one request may continue on, with the
// given route first. A provider-qualified request (provider/model) stays
// pinned; a bare model name may continue on every other provider and surface
// that serves it (the same eligibility rule RouteFor applies).
func (g *Gateway) FailoverRoutes(model string, localHasModel bool, primary *Route) []*Route {
	if primary == nil {
		return nil
	}
	if !g.failover.crossProvider || !primary.servesName(model) {
		return []*Route{primary}
	}
	chain := g.PlanRoutes(model, localHasModel, "", Session{})
	for index, route := range chain {
		if route == primary {
			return promoteRoute(chain, index)
		}
	}
	if len(chain) == 0 {
		return []*Route{primary}
	}
	return chain
}

// rotator returns the rotation cursor of one bare model name. The cursor has
// to live on the Gateway: building a scheduler per request would reset it and
// always pick the same first candidate.
func (g *Gateway) rotator(model string) *poolRotator {
	if rotator, ok := g.pools.Load(model); ok {
		return rotator.(*poolRotator)
	}
	rotator, _ := g.pools.LoadOrStore(model, &poolRotator{})
	return rotator.(*poolRotator)
}

// PlanRoutes returns the ordered routes one request may use, across every
// provider and API surface that serves the name.
//
// The order is: the session's bound route first when it is usable (that is
// what keeps a conversation on the upstream surface whose prompt cache holds
// its prefix), otherwise one provider chosen by priority/weight rotation,
// followed by the remaining candidates as failover targets. The rotation is
// per bare model name so two models do not skew each other's distribution.
func (g *Gateway) PlanRoutes(model string, localHasModel bool, clientSurface string, session Session) []*Route {
	if pinned, ok := g.pinnedRoute(model); ok {
		return pinned
	}
	eligible := g.poolCandidates(model, localHasModel)
	if len(eligible) == 0 {
		return nil
	}
	if len(eligible) > 1 {
		eligible = filterSlowRoutes(eligible, g.skipSlow, g.latency, g.logger)
	}

	ordered := orderPoolRoutes(eligible, clientSurface)

	if key := session.affinityKey(); key != "" {
		if entry, ok := g.sticky.get(key, time.Now()); ok {
			bound := false
			for index, route := range ordered {
				if route.Key() != entry.route {
					continue
				}
				bound = true
				if route.Available() {
					return promoteRoute(ordered, index)
				}
				break
			}
			if !bound {
				// The bound surface left the configuration (the provider was
				// removed, or the model moved to another adapter): drop the
				// binding now instead of waiting for the TTL, so the session
				// starts a fresh one on whatever serves it today.
				g.sticky.unbind(key)
			}
		}
	}

	if len(ordered) <= 1 {
		return ordered
	}

	// Rotation is between providers, not routes: the adapters of one provider
	// are one upstream account with one quota.
	groups := make(map[string][]*Route)
	order := make([]string, 0, len(ordered))
	for _, route := range ordered {
		if _, ok := groups[route.ProviderID]; !ok {
			order = append(order, route.ProviderID)
		}
		groups[route.ProviderID] = append(groups[route.ProviderID], route)
	}
	sort.Strings(order)

	bids := make([]poolCandidate, 0, len(order))
	for _, provider := range order {
		routes := groups[provider]
		usable := false
		for _, route := range routes {
			if route.Available() {
				usable = true
				break
			}
		}
		if !usable {
			// Every key of this provider is cooling down or parked: it cannot
			// win this request, but it stays in the chain below so a failure on
			// the chosen provider can still fall back to it.
			continue
		}
		priority, weight := routes[0].poolBid()
		bids = append(bids, poolCandidate{
			provider: provider,
			route:    routes[0],
			priority: priority,
			weight:   weight,
		})
	}
	if len(bids) == 0 {
		// Nothing is schedulable: hand back the ordered chain so the caller
		// reports the real upstream state instead of a synthetic error.
		return ordered
	}

	chosen, ok := g.rotator(model).pick(bids, rand.Intn)
	if !ok {
		return ordered
	}
	head := groups[chosen.provider]
	rest := make([]*Route, 0, len(ordered)-len(head))
	for _, route := range ordered {
		if route.ProviderID != chosen.provider {
			rest = append(rest, route)
		}
	}
	return append(append([]*Route(nil), head...), rest...)
}

// promoteRoute moves one route to the front of the chain, keeping the order of
// the others.
func promoteRoute(routes []*Route, index int) []*Route {
	if index <= 0 || index >= len(routes) {
		return routes
	}
	out := make([]*Route, 0, len(routes))
	out = append(out, routes[index])
	out = append(out, routes[:index]...)
	out = append(out, routes[index+1:]...)
	return out
}

// PoolRoutes lists every route that serves a bare model name, in deterministic
// order (client dialect first, then priority, provider and adapter). It is
// side-effect free: unlike PlanRoutes it neither rotates nor filters by
// cooldowns, so callers that only need the shape of the pool (dialect
// resolution, status APIs) do not perturb scheduling.
func (g *Gateway) PoolRoutes(model string, localHasModel bool, clientSurface string) []*Route {
	if pinned, ok := g.pinnedRoute(model); ok {
		return pinned
	}
	return orderPoolRoutes(g.poolCandidates(model, localHasModel), clientSurface)
}

// pinnedRoute reports the route of a provider-qualified name, which is pinned
// to that provider and has no pool. The second result distinguishes "no such
// name" (a nil slice, no routes) from "not a qualified name" (ok is false, so
// the caller builds the bare-name pool).
func (g *Gateway) pinnedRoute(model string) ([]*Route, bool) {
	g.mu.RLock()
	defer g.mu.RUnlock()
	exact, pinned := g.routes[model]
	if !pinned {
		return nil, false
	}
	if exact == nil {
		return nil, true
	}
	return []*Route{exact}, true
}

// poolCandidates lists the deduplicated routes a bare model name may use. Both
// PoolRoutes and PlanRoutes build the pool this way, so a request that resolves
// its dialect and then plans its chain sees exactly the same candidate set.
func (g *Gateway) poolCandidates(model string, localHasModel bool) []*Route {
	g.mu.RLock()
	candidates := append([]*Route(nil), preferOwnName(g.bareRoutes[model], model)...)
	g.mu.RUnlock()

	eligible := make([]*Route, 0, len(candidates))
	seen := make(map[*Route]struct{}, len(candidates))
	for _, route := range candidates {
		if localHasModel && !route.ProviderFirst {
			continue
		}
		if _, duplicate := seen[route]; duplicate {
			continue
		}
		seen[route] = struct{}{}
		eligible = append(eligible, route)
	}
	return eligible
}

// orderPoolRoutes sorts one pool deterministically: the client's own dialect
// first (no conversion needed), then higher provider priority, then provider
// name. Ties keep the configured registration order (adapters in declaration
// order), which is what makes "the first adapter listed" a meaningful choice
// for clients whose dialect no adapter speaks.
func orderPoolRoutes(routes []*Route, clientSurface string) []*Route {
	ordered := append([]*Route(nil), routes...)
	// The bid of a route depends on which of its credentials are currently
	// available, so it is read once per route here instead of inside the
	// comparator: sorting would otherwise take a credential lock per
	// comparison, which is O(c log c) lock acquisitions per request. The
	// snapshot is taken before the sort, so a route's own bid cannot change
	// halfway through its comparison.
	bids := make(map[*Route]int, len(ordered))
	for _, route := range ordered {
		priority, _ := route.poolBid()
		bids[route] = priority
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		leftNative := clientSurface != "" && ordered[i].Type() == clientSurface
		rightNative := clientSurface != "" && ordered[j].Type() == clientSurface
		if leftNative != rightNative {
			return leftNative
		}
		leftPriority := bids[ordered[i]]
		rightPriority := bids[ordered[j]]
		if leftPriority != rightPriority {
			return leftPriority > rightPriority
		}
		return ordered[i].ProviderID < ordered[j].ProviderID
	})
	return ordered
}

// servesName reports whether the client-facing name belongs to this route
// (its primary name or one of its aliases).
// servesOwnName reports whether the name is the model's own client-facing name
// (prefix + model ID, or the provider-qualified form) rather than one of its
// aliases.
func (r *Route) servesOwnName(name string) bool {
	if name == "" {
		return false
	}
	if name == r.ExposedName || name == config.ProviderModelFQN(r.ProviderID, r.ModelID) {
		return true
	}
	return false
}

// preferOwnName narrows a pool to the routes that serve the name as their own
// model name when any of them do: a name that is a model's own name outranks
// the same name declared as another model's alias, while an alias-only pool
// keeps every candidate.
func preferOwnName(routes []*Route, name string) []*Route {
	if len(routes) <= 1 {
		return routes
	}
	own := make([]*Route, 0, len(routes))
	for _, route := range routes {
		if route.servesOwnName(name) {
			own = append(own, route)
		}
	}
	if len(own) == 0 {
		return routes
	}
	return own
}

func (r *Route) servesName(name string) bool {
	if name == "" {
		return false
	}
	if name == r.ExposedName {
		return true
	}
	for _, alias := range r.ModelAliases {
		if name == r.ExposedNameFor(alias) {
			return true
		}
	}
	return false
}

// pickAcrossChain selects the next untried available credential, walking the
// chain in order so the primary provider is exhausted before failover moves on.
func (g *Gateway) pickAcrossChain(chain []*Route, tried map[string]struct{}, now time.Time) (*Credential, *Route) {
	// Skip-slow is decided per pick, not once per request: a skipped provider
	// stays in the chain, so a later attempt (after a sibling failed) can fall
	// back to it instead of the request failing.
	//
	// Routes with nothing left to try are removed before the decision, so a
	// provider whose credentials are merely cooling down cannot be counted as
	// "the fastest alternative" and suppress a healthy sibling. If nothing is
	// left, the original chain is used so the caller reports the real state.
	candidates := make([]*Route, 0, len(chain))
	for _, route := range chain {
		if route.sched.hasCandidate(tried) {
			candidates = append(candidates, route)
		}
	}
	if len(candidates) > 0 {
		chain = g.fastRoutes(candidates)
	}
	for _, route := range chain {
		if cred := route.sched.pick(now, tried); cred != nil {
			return cred, route
		}
	}

	// Nothing is available: every credential this request has not tried yet is
	// cooling down. Rather than fail the request while the pool recovers, try
	// the key whose cooldown ends soonest. A cooldown is a prediction, not a
	// guarantee — the upstream may serve the request now, and if it does not,
	// the attempt returns that provider's real error instead of a synthetic
	// "all keys are cooling down". Only keys this request has not already tried
	// are eligible, so a failing provider is never retried in the same request.
	//
	// This runs last on purpose: normal scheduling always wins when anything is
	// available, so the fallback cannot perturb the distribution.
	var (
		fallback      *Credential
		fallbackRoute *Route
		fallbackWait  time.Duration
	)
	for _, route := range chain {
		cred, wait := route.sched.pickFallback(now, tried)
		if cred == nil {
			continue
		}
		if fallback == nil || wait < fallbackWait {
			fallback, fallbackRoute, fallbackWait = cred, route, wait
		}
	}
	if fallback != nil && g.logger != nil {
		g.logger.Warnf("gateway: every credential is cooling down for model %s; trying %s (cooldown ends in %s) instead of failing the request",
			fallbackRoute.ModelID, fallback.ID, fallbackWait.Round(time.Millisecond))
	}
	return fallback, fallbackRoute
}

// chainCredentialCount counts the distinct credentials of a failover chain.
func chainCredentialCount(chain []*Route) int {
	seen := make(map[string]struct{})
	for _, route := range chain {
		for _, cred := range route.Members {
			seen[cred.ID] = struct{}{}
		}
	}
	if len(seen) == 0 {
		// Routes without schedulable members still deserve one attempt so the
		// caller gets the real upstream error instead of a synthetic one.
		return len(chain)
	}
	return len(seen)
}

// bufferRequestBody makes the request body re-readable across attempts. It
// returns a cleanup that restores the original body (nil when nothing was
// buffered).
func bufferRequestBody(req *http.Request) (*http.Request, func(), error) {
	restore := func() {}
	if req.Body == nil || req.Body == http.NoBody {
		return req, restore, nil
	}
	if req.GetBody != nil {
		// net/http already provides a replayable body.
		return req, restore, nil
	}
	body, err := io.ReadAll(io.LimitReader(req.Body, swaputil.MaxRequestBodySize+1))
	if err != nil {
		req.Body.Close()
		return req, restore, errors.New("could not read request body")
	}
	if len(body) > swaputil.MaxRequestBodySize {
		req.Body.Close()
		return req, restore, errors.New("request body exceeds the gateway limit")
	}
	req.Body.Close()
	req.Body = io.NopCloser(bytes.NewReader(body))
	contentLen := int64(len(body))
	req.ContentLength = contentLen
	req.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}
	return req, restore, nil
}

// attemptResult summarizes one forwarding attempt.
type attemptResult struct {
	outcome outcome
	// failureClass is set together with cause so downstream logic never has
	// to re-parse a human-readable string to recover the classification.
	failureClass CooldownClass
	status       int    // upstream status (0 when no response arrived)
	snippet      string // bounded upstream body / transport error text
	cause        string // short classification for logs
	// retryAfter, when set on a 429, extends the cooldown.
	retryAfter time.Time
	// skipCooldown marks a retry that must not bench the credential: the
	// upstream rejected this request, which says nothing about the credential
	// itself (a 404 for one model, a malformed parameter, ...). The credential
	// is still excluded for the rest of this request.
	skipCooldown bool
	// retryableOnCredential marks a failure that retrying the same credential
	// may actually fix (transport errors, truncated streams, 5xx). Auth and
	// rate-limit failures are verdicts on the credential, so they bench it at
	// once instead of spending its retry budget.
	retryableOnCredential bool
}

// attempt performs one forwarding attempt. It buffers the upstream response
// head and the first body chunk (streaming bootstrap): until the first byte
// reaches the client a failure can be retried on another credential
// invisibly. Once the head is written the outcome is terminal.
func (g *Gateway) attempt(w http.ResponseWriter, req *http.Request, route *Route, cred *Credential, session Session, opts AttemptOptions) attemptResult {
	transport, err := g.transports.get(route.egressProxy(cred), route)
	if err != nil {
		// Bad proxy configuration is credential-scoped: cool the credential
		// down and let the next attempt proceed.
		return attemptResult{outcome: outcomeRetry, cause: "bad proxy config", snippet: err.Error()}
	}

	// Every attempt works on its own copy with a freshly rebuilt body: the
	// previous RoundTrip consumed the original Body, and a failover attempt
	// may target a provider whose upstream model name differs.
	outReq := req.Clone(req.Context())
	if outReq.GetBody != nil {
		body, berr := outReq.GetBody()
		if berr != nil {
			return attemptResult{outcome: outcomeStop, cause: "request body unavailable", snippet: berr.Error()}
		}
		outReq.Body = body
	}
	// The model name is rewritten per attempt because a failover may target a
	// provider whose upstream name differs, and a pool may even hold routes
	// whose local names differ (same bare name reached as another model's
	// alias). Read the name the request currently carries instead of assuming
	// the primary route's local name: assuming it silently sends the client's
	// name upstream whenever the two differ.
	targetModel := route.RemoteName
	if targetModel == "" {
		targetModel = route.ModelID
	}
	if targetModel != "" {
		if current, merr := swaputil.ExtractModel(outReq); merr == nil && current != "" && current != targetModel {
			rewritten, rerr := swaputil.ReplaceRequestModel(outReq, current, targetModel)
			if rerr != nil {
				return attemptResult{outcome: outcomeRetry, cause: "model rewrite failed", snippet: rerr.Error()}
			}
			outReq = rewritten
		}
	}

	outURL := *route.endpoint
	outPath := applyPathMap(outReq.URL.Path, route.pathMap)
	outURL.Path = joinPath(route.endpoint.Path, outPath)
	if outPath == outReq.URL.Path {
		outURL.RawPath = outReq.URL.RawPath
	} else {
		// The escaped form describes the original path; drop it so the
		// rewritten path is escaped from scratch.
		outURL.RawPath = ""
	}
	outURL.RawQuery = outReq.URL.RawQuery

	outReq.URL = &outURL
	outReq.Host = outURL.Host
	outReq.RequestURI = ""
	removeHopHeaders(outReq.Header)
	applyStaticHeaders(outReq.Header, route)
	setAuthHeaders(outReq, route.ptype, cred.Value)
	applySessionHeaders(outReq.Header, route, session)
	outReq = outReq.WithContext(swaputil.WithUpstreamHTTPClient(outReq.Context(), &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}))
	outReq.Header.Set("Accept-Encoding", "identity")
	hooks := swaputil.UpstreamHooksFrom(outReq.Context())
	scope := routeScope(route, targetModel)
	scope.Credential = cred.ID
	if hooks.Prepare != nil {
		if err := hooks.Prepare(outReq.Context(), scope, outReq); err != nil {
			return attemptResult{outcome: outcomeRetry, status: http.StatusBadRequest, snippet: err.Error(), cause: "request preparation failed", skipCooldown: true}
		}
	}

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		if req.Context().Err() != nil {
			// The client is gone: terminal, nothing to retry for.
			return attemptResult{outcome: outcomeStop, cause: "client disconnected", snippet: err.Error()}
		}
		return attemptResult{outcome: outcomeRetry, cause: "transport error", snippet: err.Error(), retryableOnCredential: true}
	}
	defer func() {
		if resp.Body != nil {
			resp.Body.Close()
		}
	}()
	if hooks.Complete != nil {
		capture := &upstreamCapture{}
		resp.Body = &capturedBody{Reader: io.TeeReader(resp.Body, capture), Closer: resp.Body}
		input := requestBytes(outReq)
		defer func() {
			hooks.Complete(outReq.Context(), scope, input, capture.body.Bytes(), resp.Header.Clone(), resp.StatusCode, capture.truncated)
		}()
	}

	// Buffer head + first chunk so failures before the first byte stay
	// retryable. classify with a single Read: a clean body end reports EOF
	// while a connection that died mid-message reports ErrUnexpectedEOF
	// (io.ReadFull cannot tell "short body" from "truncated body": it reports
	// ErrUnexpectedEOF for both).
	buf := make([]byte, 0, 8192)
	first := make([]byte, 8192)
	n, rerr := resp.Body.Read(first)
	truncated := rerr == io.ErrUnexpectedEOF
	if rerr != nil && rerr != io.EOF && !truncated {
		return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, cause: "read error", snippet: rerr.Error(), retryableOnCredential: true}
	}
	// Only a body the upstream declared a length for (or an error body, which
	// is bounded by definition) is read further here: that is where "the
	// upstream died mid-answer" is observable before the client sees it. A
	// chunked success response is a stream and is forwarded at once — waiting
	// for a full buffer would hold back the first token until the upstream
	// produced 8 KB or finished the answer.
	if (resp.StatusCode >= 400 || resp.ContentLength >= 0) && !truncated && rerr == nil && n < len(first) {
		filled, ferr := io.ReadFull(resp.Body, first[n:])
		n += filled
		if ferr != nil && ferr != io.EOF && ferr != io.ErrUnexpectedEOF {
			return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, cause: "read error", snippet: ferr.Error(), retryableOnCredential: true}
		}
		// A declared length that was not delivered means the upstream closed
		// mid-message. (For an unknown length the reader cannot tell a short
		// body from a truncated one, so only the first read classifies it.)
		if ferr == io.ErrUnexpectedEOF && resp.ContentLength >= 0 && int64(n) < resp.ContentLength {
			truncated = true
		}
	}
	buf = append(buf, first[:n]...)
	snippet := decodeSnippet(resp, buf)
	// A response whose whole body is already in hand can be judged as a unit;
	// a stream (or a body still arriving) is judged as it flows.
	bodyComplete := rerr == io.EOF || (resp.ContentLength >= 0 && int64(len(buf)) >= resp.ContentLength)

	// Truncated or empty success responses: the upstream aborted before it
	// produced a usable answer. Both stay retryable because nothing has
	// reached the client yet.
	if resp.StatusCode < 400 && req.Context().Err() == nil {
		switch {
		case truncated:
			return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, cause: "response truncated (EOF before the declared length)", snippet: snippet, retryableOnCredential: true}
		case n == 0 && isStreamingResponse(resp):
			return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, cause: "stream ended before first event", snippet: "", retryableOnCredential: true}
		case n == 0 && requestCarriesBody(req):
			return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, cause: "upstream closed before sending a response body", snippet: "", retryableOnCredential: true}
		}
	}

	if class, ok := credentialFailure(resp.StatusCode); ok {
		res := attemptResult{outcome: outcomeRetry, status: resp.StatusCode, snippet: snippet}
		switch class {
		case CooldownAuth:
			res.failureClass = CooldownAuth
			res.cause = fmt.Sprintf("status %d (auth)", resp.StatusCode)
		case CooldownRateLimit:
			res.failureClass = CooldownRateLimit
			res.cause = fmt.Sprintf("status %d (rate limit)", resp.StatusCode)
			if ra, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
				res.retryAfter = ra
			} else if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs >= 0 {
				res.retryAfter = time.Now().Add(time.Duration(secs) * time.Second)
			}
		case CooldownTransient:
			res.failureClass = CooldownTransient
			res.cause = fmt.Sprintf("status %d (transient)", resp.StatusCode)
			// A 5xx is the one upstream verdict that retrying the same
			// credential can fix: an overloaded node sheds its load, a gateway
			// timeout is a passing condition. Auth (401/402) and rate limits
			// (429) are statements about the credential itself.
			res.retryableOnCredential = true
		}
		return res
	}

	if g.failover.eligible(resp.StatusCode) {
		// An error status that another credential (or provider) may not
		// repeat: continue while the client still has not seen anything. The
		// statuses that implicate the credential itself were classified above
		// (auth/rate-limit/transient) and keep their cooldown.
		return attemptResult{
			outcome:      outcomeRetry,
			status:       resp.StatusCode,
			snippet:      snippet,
			cause:        fmt.Sprintf("status %d (failover)", resp.StatusCode),
			skipCooldown: true,
		}
	}

	if resp.StatusCode >= 400 {
		// Excluded statuses (e.g. 413 payload too large) are handed back
		// untouched: another credential would refuse them the same way. A
		// caller with another upstream surface to try asks for the error
		// instead of the response so it can retry there.
		if opts.StopOnRequestScoped {
			return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, snippet: snippet,
				cause:        fmt.Sprintf("status %d (request-scoped, another surface may serve it)", resp.StatusCode),
				skipCooldown: true}
		}
		cred.noteError(fmt.Sprintf("status %d (request-scoped)", resp.StatusCode))
		// The response is going to the client, so this provider is the answer
		// to "where did this go" — an operator debugging a failing upstream
		// needs it recorded on the failing rows, not only on the 2xx ones.
		swaputil.PublishRequestProvider(req.Context(), route.ProviderID, route.AdapterName)
		commitResponse(w, resp, buf)
		g.forwardBody(w, req, resp.Body, cred) // an error body is not model output
		return attemptResult{outcome: outcomeCommit, status: resp.StatusCode, snippet: snippet}
	}

	// Success: commit head, replay buffered bytes, stream the rest.
	//
	// This is the one point where the request definitively landed on this
	// route: every earlier failure either retried (so a later route answers)
	// or was handed back to a caller that may still try another surface.
	// Publishing here keeps the recorded provider free of attempts that never
	// produced an answer.
	//
	// A provider sometimes answers 200 and then produces no model output at
	// all (a lifecycle-only stream, an empty choices array, a truncated tool
	// call). Nothing has reached the client yet, so that answer is retried on
	// the next credential or provider instead of being committed: handing the
	// caller an empty success is worse than one more attempt.
	if resp.StatusCode < 400 && g.emptyResponse.enabled && probeableResponse(req, resp) {
		gated, result := g.awaitContent(w, req, resp, route, cred, buf, bodyComplete)
		if gated {
			return result
		}
		if strings.TrimSpace(result.snippet) == "" {
			result.snippet = "upstream returned no model output"
		}
		return attemptResult{
			outcome: outcomeRetry,
			status:  http.StatusBadGateway,
			snippet: result.snippet,
			cause:   "success response carried no model output",
		}
	}

	swaputil.PublishRequestProvider(req.Context(), route.ProviderID, route.AdapterName)
	// The stream starts here. Its "decode" phase is published later, when the
	// first chunk that actually carries model output arrives: the first bytes
	// of a stream are usually lifecycle frames, and reporting those as decode
	// makes the phase look earlier than the first token really was.
	commitResponse(w, resp, buf)
	g.forwardBodyFor(w, req, resp.Body, cred, route.Type(), buf)
	return attemptResult{outcome: outcomeCommit, status: resp.StatusCode}
}

// awaitContent gates a success response on actually carrying model output.
//
// It returns gated=true when the response was committed (or the client went
// away), and gated=false with the observed snippet when the upstream finished
// without producing any output. A stream is inspected only until its first
// content or the configured hold: after that it is committed and streamed
// unchanged, so a slow provider is never treated as a broken one.
func (g *Gateway) awaitContent(w http.ResponseWriter, req *http.Request, resp *http.Response, route *Route, cred *Credential, buffered []byte, bodyComplete bool) (bool, attemptResult) {
	probe := newContentProbe(route.Type(), isStreamingResponse(resp))
	probe.Write(buffered)

	commit := func(head []byte, rest io.Reader) (bool, attemptResult) {
		swaputil.PublishRequestProvider(req.Context(), route.ProviderID, route.AdapterName)
		// The buffered head may already hold the first content: the phase is
		// settled before the stream is handed over so a stream whose first
		// chunk carried output does not wait for a later flush.
		if probe.HasContent() {
			swaputil.ReportInferencePhase(req.Context(), "decode", "model output started")
		}
		commitResponse(w, resp, head)
		g.forwardBodyFor(w, req, rest, cred, route.Type(), nil)
		return true, attemptResult{outcome: outcomeCommit, status: resp.StatusCode}
	}

	if bodyComplete {
		probe.Finish()
		if !probe.Unknown() && probe.Verdict() == probeEmpty {
			return false, attemptResult{status: resp.StatusCode, snippet: decodeSnippet(resp, buffered)}
		}
		return commit(buffered, resp.Body)
	}

	hold := g.emptyResponse.hold
	if hold <= 0 {
		// Stream inspection disabled: keep the pre-existing behaviour.
		return commit(buffered, resp.Body)
	}

	pump := startBodyPump(req.Context(), resp.Body)
	defer pump.stop()
	reader := &pumpReader{events: pump.events}

	deadline := time.NewTimer(hold)
	defer deadline.Stop()

	collected := buffered
	for {
		switch probe.Verdict() {
		case probeContent:
			return commit(collected, reader)
		case probeEmpty:
			return false, attemptResult{status: resp.StatusCode, snippet: ""}
		}
		if probe.Unknown() || probe.Forward() {
			// Not a shape the probe understands, or a correct answer that
			// carries no model output (a provider-owned background submit
			// acknowledges with status=queued/in_progress): forward it now.
			// Holding either would delay a legitimate response by the whole
			// grace period for no benefit.
			return commit(collected, reader)
		}
		if len(collected) > emptyResponseCommitBufferBytes {
			// Nothing recognizable in a lot of bytes: the response is not a
			// shape we can judge, so fail open rather than retry forever.
			return commit(collected, reader)
		}

		select {
		case ev, ok := <-pump.events:
			if !ok {
				probe.Finish()
				if probe.Verdict() == probeEmpty {
					return false, attemptResult{status: resp.StatusCode, snippet: ""}
				}
				return commit(collected, reader)
			}
			if ev.err != nil {
				// The upstream died before producing content: still retryable,
				// because nothing has been committed.
				return false, attemptResult{status: resp.StatusCode, snippet: ev.err.Error()}
			}
			if len(ev.data) > 0 {
				collected = append(collected, ev.data...)
				probe.Write(ev.data)
			}
		case <-deadline.C:
			// The stream is alive but still has no content: commit and keep
			// streaming. Waiting longer would turn a slow first token into a
			// failure.
			return commit(collected, reader)
		case <-req.Context().Done():
			return true, attemptResult{outcome: outcomeStop, cause: "client disconnected"}
		}
	}
}

// probeableResponse reports whether a success response may be inspected for
// model output: a generation endpoint (not embeddings or a media job) whose
// body we can read without decoding a content coding.
func probeableResponse(req *http.Request, resp *http.Response) bool {
	if req == nil || resp == nil {
		return false
	}
	if encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding"))); encoding != "" && encoding != "identity" {
		// The probe parses JSON; compressed bytes would read as pending and
		// hold every request for the whole grace period.
		return false
	}
	_, ok := protocolrelaykit.RouteForRequest(req.Method, req.URL.Path)
	return ok
}

// decodeSnippet renders buffered upstream bytes for an error message. When the
// upstream compressed the body (we forward the client's Accept-Encoding), the
// raw bytes are unreadable and would corrupt the JSON error envelope, so the
// snippet is decompressed (or replaced with a note when the codec is unknown).
func decodeSnippet(resp *http.Response, raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	encoding := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	switch {
	case encoding == "":
		return string(raw)
	case strings.Contains(encoding, "gzip"):
		reader, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			return "(gzip response body)"
		}
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, 64<<10))
		if err != nil && len(decoded) == 0 {
			return "(gzip response body)"
		}
		return string(decoded)
	case strings.Contains(encoding, "deflate"):
		reader := flate.NewReader(bytes.NewReader(raw))
		defer reader.Close()
		decoded, err := io.ReadAll(io.LimitReader(reader, 64<<10))
		if err != nil && len(decoded) == 0 {
			return "(deflate response body)"
		}
		return string(decoded)
	default:
		return "(" + encoding + " response body)"
	}
}

// forwardBody streams the rest of the upstream response to the client. A
// failure here cannot be retried (the head is already committed), so it is
// recorded on the credential instead: a credential that truncates streams
// should not immediately serve the next request.
//
// The reader is wrapped in a probe so the "decode" phase flips on the first
// chunk that carries model output. Lifecycle frames (response.created,
// message_start, a role-only delta) arrive first and are not output; reporting
// them as decode made the live phase lie about when the first token landed.
func (g *Gateway) forwardBody(w http.ResponseWriter, req *http.Request, body io.Reader, cred *Credential) {
	g.forwardBodyFor(w, req, body, cred, "", nil)
}

func (g *Gateway) forwardBodyFor(w http.ResponseWriter, req *http.Request, body io.Reader, cred *Credential, dialect string, seed []byte) {
	probe := newContentProbe(dialect, true)
	if len(seed) > 0 {
		probe.Write(seed)
	}
	// Flush after every upstream read. net/http buffers a response writer and
	// only pushes the buffer out when it fills or the handler returns, so a
	// copy without flushes delivers a short answer — an SSE stream in
	// particular — in one lump at the end instead of as it arrives.
	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	for {
		n, rerr := body.Read(buf)
		if n > 0 {
			if dialect != "" && !probe.HasContent() {
				probe.Write(buf[:n])
				if probe.HasContent() {
					swaputil.ReportInferencePhase(req.Context(), "decode", "model output started")
				}
			}
			if _, werr := w.Write(buf[:n]); werr != nil {
				if req.Context().Err() != nil {
					// The client is gone; the upstream didn't fail.
					return
				}
				cred.noteError(fmt.Sprintf("stream interrupted: %v", werr))
				if g.logger != nil {
					g.logger.Warnf("gateway: credential %s stream interrupted after commit: %v", cred.ID, werr)
				}
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				return
			}
			if req.Context().Err() != nil {
				return
			}
			cred.noteError(fmt.Sprintf("stream interrupted: %v", rerr))
			if g.logger != nil {
				g.logger.Warnf("gateway: credential %s stream interrupted after commit: %v", cred.ID, rerr)
			}
			return
		}
	}
}

// requestCarriesBody reports whether the request has a payload. An inference
// request always does, so an empty 2xx response to one is a failure rather
// than a legitimate empty result.
func requestCarriesBody(req *http.Request) bool {
	if req == nil || req.Body == nil || req.Body == http.NoBody {
		return false
	}
	return req.ContentLength != 0 || len(req.TransferEncoding) > 0
}

// noteFailureOnly records a failure that is about to be retried on the same
// credential, without benching it. The cooldown is what ends a credential's
// participation in a request; until its retry budget is spent, the transient
// failure stays observable but must not take the key out of rotation.
func (g *Gateway) noteFailureOnly(cred *Credential, res attemptResult) {
	cred.noteUpstream(res.status, res.snippet)
	cred.noteError(res.cause)
}

// Retry backoff bounds. The first retry of a credential waits retryBaseDelay;
// each later one doubles. maxRetryBackoff caps one wait, and
// maxTotalRetryBackoff caps everything one request may spend sleeping: a
// misconfigured pool must fail quickly with the upstream's own error, not
// stall the caller while every key exhausts a long budget.
const (
	retryBaseDelay       = 250 * time.Millisecond
	maxRetryBackoff      = 4 * time.Second
	maxTotalRetryBackoff = 5 * time.Second
)

// retryBackoff returns how long to wait before retrying a credential that has
// already used attempts attempts. The first retry waits retryBaseDelay, each
// further one doubles up to maxRetryBackoff.
func (g *Gateway) retryBackoff(attempts int, route *Route) time.Duration {
	if attempts <= 0 {
		return 0
	}
	wait := retryBaseDelay << (attempts - 1)
	if wait > maxRetryBackoff {
		wait = maxRetryBackoff
	}
	return wait
}

// applyFailure records a credential failure: cooldown by class, with
// Retry-After extending rate-limit cooldowns. Transport errors (status 0)
// cool down transiently.
func (g *Gateway) applyFailure(cred *Credential, res attemptResult, route *Route, fallback cooldownConfig) {
	cc := fallback
	if route != nil {
		cc = route.cooldowns
	}
	// The upstream answered, even when this particular failure does not bench
	// the credential: remember it so requests arriving during the cooldown
	// report the real status and body.
	cred.noteUpstream(res.status, res.snippet)
	if route != nil && route.cooldownDisabled {
		// The provider opted out of benching: this failure is still visible in
		// the status API, but the credential keeps serving (and the request may
		// still fail over to another credential or provider).
		cred.noteError(res.cause)
		if g.logger != nil {
			g.logger.Warnf("gateway: credential %s failed (%s); cooldown disabled for provider %s",
				cred.ID, res.cause, route.ProviderID)
		}
		return
	}
	if res.skipCooldown {
		// Request-scoped rejection: record it for observability, but keep the
		// credential schedulable for other requests.
		cred.noteError(fmt.Sprintf("status %d (failover)", res.status))
		return
	}
	now := time.Now()
	switch res.failureClass {
	case CooldownAuth:
		until := now.Add(cc.auth)
		// 401 is often transient: probe again soon unless this credential has
		// already been rejected by auth. The check is a counter rather than a
		// wall-clock window on purpose: while a credential is benched for the
		// full auth cooldown (ten minutes by default) it is not retried, so a
		// recency window would have expired by the time the next 401 arrives
		// and would restart the short probe — alternating probe and full
		// cooldown forever instead of staying benched. 402 (payment required)
		// is not transient and keeps the full cooldown immediately.
		if res.status == http.StatusUnauthorized && cc.authProbe > 0 && !cred.RecentAuthFailure(now) {
			probe := cc.authProbe
			if probe > cc.auth {
				probe = cc.auth
			}
			until = now.Add(probe)
		}
		cred.cooldown(now, CooldownAuth, until, fmt.Sprintf("status %d: %s", res.status, truncate(res.snippet, 256)))
	case CooldownRateLimit:
		until := now.Add(cc.rateLimit)
		if res.retryAfter.After(until) {
			until = res.retryAfter
		}
		cred.cooldown(now, CooldownRateLimit, until, fmt.Sprintf("status %d: %s", res.status, truncate(res.snippet, 256)))
	case CooldownTransient:
		cred.cooldown(now, CooldownTransient, now.Add(cc.transient), fmt.Sprintf("status %d: %s", res.status, truncate(res.snippet, 256)))
	default:
		// Unclassified retry (transport read error, truncated response, an
		// upstream that closed before its first event): brief transient
		// cooldown to shed load, never an auth bench.
		cred.cooldown(now, CooldownTransient, now.Add(cc.transient), truncate(res.snippet, 256))
	}
}

// credentialFailure classifies status codes that implicate the credential
// (CLIProxyAPI error quadrants, minus free-text keyword matching).
//
// 403 is deliberately not an auth failure: upstreams use it for policy
// rejections that are scoped to one model or region ("this model is not
// available in your country"), so benching the whole credential for minutes
// would take every other model of that provider down with it. It still fails
// over (see failoverPolicy.eligible) and the credential is excluded for the
// rest of the request.
func credentialFailure(status int) (CooldownClass, bool) {
	switch status {
	case http.StatusUnauthorized, http.StatusPaymentRequired:
		return CooldownAuth, true
	case http.StatusTooManyRequests:
		return CooldownRateLimit, true
	case http.StatusRequestTimeout, http.StatusInternalServerError,
		http.StatusNotImplemented, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout,
		http.StatusHTTPVersionNotSupported:
		return CooldownTransient, true
	}
	// CDN/overload codes seen in the wild (Cloudflare, Anthropic 529).
	switch status {
	case 520, 521, 522, 523, 525, 526, 529:
		return CooldownTransient, true
	}
	return 0, false
}

func setAuthHeaders(req *http.Request, ptype providerType, value string) {
	switch ptype {
	case providerAnthropic:
		req.Header.Set("x-api-key", value)
		req.Header.Set("anthropic-version", "2023-06-01")
		req.Header.Del("Authorization")
		req.Header.Del("x-goog-api-key")
	case providerGemini:
		req.Header.Set("x-goog-api-key", value)
		req.Header.Del("Authorization")
		req.Header.Del("x-api-key")
	default:
		req.Header.Set("Authorization", "Bearer "+value)
		req.Header.Del("x-api-key")
		req.Header.Del("x-goog-api-key")
	}
}

// applyPathMap rewrites the request path through the adapter's literal prefix
// map; the longest matching prefix wins. Without a match the path is returned
// unchanged.
func applyPathMap(requestPath string, pathMap map[string]string) string {
	if len(pathMap) == 0 {
		return requestPath
	}
	best := ""
	for prefix := range pathMap {
		if strings.HasPrefix(requestPath, prefix) && len(prefix) > len(best) {
			best = prefix
		}
	}
	if best == "" {
		return requestPath
	}
	return pathMap[best] + strings.TrimPrefix(requestPath, best)
}

// applyStaticHeaders sets the provider's configured static headers. It runs
// before the credential and session headers so those always win.
func applyStaticHeaders(header http.Header, route *Route) {
	for name, value := range route.StaticHeaders {
		header.Set(name, value)
	}
}

// applySessionHeaders echoes the session id into the request headers the
// provider opted into (providers.<id>.sessionHeaders). An empty session id
// leaves the headers untouched so a client-supplied value or upstream default
// is not clobbered with an empty string.
func applySessionHeaders(header http.Header, route *Route, session Session) {
	if session.ID == "" {
		return
	}
	for _, name := range route.SessionHeaders {
		header.Set(name, session.ID)
	}
}

// removeHopHeaders strips hop-by-hop headers and inbound auth so the
// upstream sees a clean request.
func removeHopHeaders(h http.Header) {
	h.Del("Authorization")
	h.Del("x-api-key")
	h.Del("x-goog-api-key")
	for _, name := range []string{
		"Connection", "Proxy-Connection", "Keep-Alive", "Proxy-Authenticate",
		"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
	} {
		h.Del(name)
	}
}

// commitResponse copies the upstream response head to the client and flushes
// the buffered first chunk; SSE responses disable proxy buffering.
func commitResponse(w http.ResponseWriter, resp *http.Response, buffered []byte) {
	for k, vv := range resp.Header {
		for _, v := range vv {
			w.Header().Add(k, v)
		}
	}
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream") {
		w.Header().Set("X-Accel-Buffering", "no")
	}
	w.WriteHeader(resp.StatusCode)
	if len(buffered) > 0 {
		w.Write(buffered)
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// isStreamingResponse reports whether the upstream response is an SSE stream.
func isStreamingResponse(resp *http.Response) bool {
	return strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
}

func joinPath(base, path string) string {
	if base == "" {
		return path
	}
	return strings.TrimSuffix(base, "/") + path
}

// transportTimeouts mirrors config.TimeoutsConfig in seconds.
type transportTimeouts struct {
	connect, keepAlive, responseHeader, tlsHandshake, idleConn int
}

// transportCache builds and caches http.Transport per (proxy, timeouts).
// Transports hold connection pools; caching per proxy value avoids
// per-request setup (new-api/sub2api pattern).
type transportCache struct {
	mu   sync.Mutex
	keys map[string]*http.Transport
}

func newTransportCache() *transportCache {
	return &transportCache{keys: make(map[string]*http.Transport)}
}

func (t *transportCache) get(proxyValue string, route *Route) (http.RoundTripper, error) {
	key := transportKey(proxyValue, route.timeouts)
	t.mu.Lock()
	defer t.mu.Unlock()
	if tr, ok := t.keys[key]; ok {
		return tr, nil
	}
	tr, err := buildTransport(proxyValue, route.timeouts)
	if err != nil {
		return nil, err
	}
	t.keys[key] = tr
	return tr, nil
}

func transportKey(proxyValue string, cfg transportTimeouts) string {
	return fmt.Sprintf("%s|%d|%d|%d|%d|%d",
		proxyValue, cfg.connect, cfg.keepAlive, cfg.responseHeader, cfg.tlsHandshake, cfg.idleConn)
}

// buildTransport constructs an http.Transport for the proxy mode:
// empty -> environment proxies, "direct" -> no proxy, otherwise the proxy
// URL (http/https via ProxyURL, socks5/socks5h via x/net/proxy, which
// resolves DNS remotely — socks5h semantics).
// BuildTransport exposes the transport builder for probe/API use.
func BuildTransport(proxyValue string, connect, keepAlive, responseHeader, tlsHandshake, idleConn int) (*http.Transport, error) {
	return buildTransport(proxyValue, transportTimeouts{
		connect: connect, keepAlive: keepAlive, responseHeader: responseHeader,
		tlsHandshake: tlsHandshake, idleConn: idleConn,
	})
}

func buildTransport(proxyValue string, cfg transportTimeouts) (*http.Transport, error) {
	tr := &http.Transport{
		DialContext: (&net.Dialer{
			Timeout:   time.Duration(cfg.connect) * time.Second,
			KeepAlive: time.Duration(cfg.keepAlive) * time.Second,
		}).DialContext,
		TLSHandshakeTimeout:   time.Duration(cfg.tlsHandshake) * time.Second,
		ResponseHeaderTimeout: time.Duration(cfg.responseHeader) * time.Second,
		ExpectContinueTimeout: time.Second,
		IdleConnTimeout:       time.Duration(cfg.idleConn) * time.Second,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   10,
	}

	switch normalizeProxy(proxyValue) {
	case "":
		// No per-credential proxy: follow the process policy. Resolved per
		// request instead of reading the environment once, so a settings
		// reload that changes the proxy reaches this client too.
		tr.Proxy = netproxy.Current().ProxyFunc()
	case config.ProxyDirect:
		// Explicit bypass: no proxy at all.
	default:
		pu, err := url.Parse(proxyValue)
		if err != nil {
			return nil, fmt.Errorf("invalid proxy URL (%s): %w", proxyValue, err)
		}
		switch pu.Scheme {
		case "http", "https":
			tr.Proxy = http.ProxyURL(pu)
		case "socks5", "socks5h":
			dialer, err := proxy.FromURL(pu, proxy.Direct)
			if err != nil {
				return nil, fmt.Errorf("invalid socks proxy (%s): %w", proxyValue, err)
			}
			cd, ok := dialer.(proxy.ContextDialer)
			if !ok {
				return nil, fmt.Errorf("socks proxy (%s) does not support context dialing", proxyValue)
			}
			tr.DialContext = cd.DialContext
		default:
			return nil, fmt.Errorf("proxy must use http, https, socks5 or socks5h, got %q", pu.Scheme)
		}
	}
	return tr, nil
}

func normalizeProxy(p string) string {
	p = strings.TrimSpace(p)
	if p == "none" {
		return config.ProxyDirect
	}
	return p
}

func (t *transportCache) close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, tr := range t.keys {
		tr.CloseIdleConnections()
	}
	t.keys = make(map[string]*http.Transport)
}

// oneLine renders an upstream error snippet for a log line: control characters
// would otherwise let a body forge extra log entries, and a long body would
// drown the line that carries it.
func oneLine(text string, limit int) string {
	trimmed := strings.Join(strings.Fields(text), " ")
	if trimmed == "" {
		return "(no body)"
	}
	return truncate(trimmed, limit)
}
