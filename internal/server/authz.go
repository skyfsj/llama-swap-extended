package server

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/auth"
	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
	"github.com/mostlygeek/llama-swap/internal/store"
	"github.com/mostlygeek/llama-swap/internal/swaputil"
)

type authIdentityKey struct{}

func identityFromContext(ctx context.Context) auth.Identity {
	identity, _ := ctx.Value(authIdentityKey{}).(auth.Identity)
	return identity
}

// identityIDFromContext names the credential for stored-state scoping. An
// empty id means no credential was presented (a keyless deployment), which is
// a single trust domain.
func identityIDFromContext(ctx context.Context) string {
	return strings.TrimSpace(identityFromContext(ctx).ID)
}

func withIdentity(ctx context.Context, identity auth.Identity) context.Context {
	return context.WithValue(ctx, authIdentityKey{}, identity)
}

// CreateScopedAuthMiddleware preserves the legacy default-allow mode while
// adding hash-backed managed keys and per-route scopes. Legacy YAML keys are
// intentionally marked Legacy so they retain the user's existing full access.
//
// When a key cache is provided (the normal path: routes() builds one for
// every server that has a store), the configured/authenticated decision is
// made entirely from the YAML keys and the in-memory snapshot. The durable
// store is never probed per request, so a transient SQLite failure cannot
// flip a keyless deployment into requiring credentials. Without a cache the
// middleware falls back to the historical per-request store probe, which
// still fails closed, for embedders that construct middleware directly.
//
// A model-access key additionally carries request-time restrictions (caller IP
// allowlist and concurrency ceiling), which are enforced here so every route —
// inference, control plane and upstream — is covered by the same decision.
func CreateScopedAuthMiddleware(cfg config.Config, st *store.Store, keys *keyCache) chain.Middleware {
	legacy := append([]string(nil), cfg.RequiredAPIKeys...)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			provided := strings.TrimSpace(swaputil.ExtractAPIKey(r))
			var identity auth.Identity
			var valid bool
			if keys != nil {
				// A revoked key still means authentication has been configured;
				// the snapshot counts revoked rows, otherwise revoking the last
				// key would silently reopen every management endpoint to
				// anonymous callers.
				configured := len(legacy) > 0 || keys.configured()
				if !configured && provided != "" {
					// A credential was presented even though the snapshot holds
					// no key: refresh once (rate-limited) so direct or
					// out-of-band writers to the key table are visible without
					// paying a per-request store probe.
					keys.probeForKey()
					configured = keys.configured()
				}
				if !configured {
					// Product compatibility: with no configured key, all routes
					// remain open, including the management surface.
					identity = auth.Identity{ID: "anonymous", Legacy: true}
					valid = true
				} else {
					identity, valid = keys.validate(provided, cfg)
				}
			} else {
				configured := len(legacy) > 0
				if st != nil && !configured {
					probeCtx, probeCancel := context.WithTimeout(context.Background(), 2*time.Second)
					stored, err := st.ListAPIKeys(probeCtx, true)
					probeCancel()
					if err != nil {
						// A control-plane store failure must fail closed. Treating a
						// transient SQLite error as an empty key set would reopen the
						// entire management surface to anonymous callers.
						configured = true
					} else {
						configured = len(stored) > 0
					}
				}
				if !configured {
					identity = auth.Identity{ID: "anonymous", Legacy: true}
					valid = true
				} else {
					var err error
					identity, valid, err = lookupAPIIdentity(r.Context(), cfg, st, provided)
					if err != nil {
						swaputil.SendResponse(w, r, http.StatusInternalServerError, err.Error())
						return
					}
				}
			}
			if !valid {
				markAuthFailure(r)
				w.Header().Set("WWW-Authenticate", apiKeyAuthChallenge)
				swaputil.SendResponse(w, r, http.StatusUnauthorized, "unauthorized: invalid or missing API key")
				return
			}
			// The credential is valid; clear the caller's failure window even
			// if a later layer (scope, model restriction, upstream) rejects the
			// request. Those rejections are not credential guessing.
			markAuthSuccess(r)
			// Last-used timestamps are buffered in memory and persisted by a
			// background flusher: the authentication hot path must stay
			// store-free, and all traffic shares one SQLite connection.
			if identity.ID != "" {
				// The caller address travels with the timestamp: it is the other
				// half of "who used this key, from where". A key declared in the
				// configuration file is seeded as a durable row under the same
				// id, so it gets the same audit trail as a panel-created one.
				callerIP := clientIP(r)
				if keys != nil {
					keys.recordTouch(identity.ID, time.Now(), callerIP)
				} else if st != nil {
					_ = st.TouchAPIKey(r.Context(), identity.ID, time.Now(), callerIP)
				}
			}
			// RequireScope runs before CreateRequestContextMiddleware, so a
			// model-scoped key would otherwise have no model to authorize for
			// vLLM's optional-model batch route. Resolve that route now; the
			// request body is restored by FetchContext and the cached context is
			// reused by downstream middleware.
			if len(identity.Models) > 0 && swaputil.ModelOptionalForRequest(r) {
				if _, err := swaputil.FetchContext(r, cfg); err != nil {
					swaputil.SendError(w, r, err)
					return
				}
			}
			// Access keys carry caller IP, concurrency and expiry restrictions
			// that only exist at request time. The slot is held until the
			// handler returns, so a streaming response counts for its whole
			// lifetime rather than only until the first write.
			release, status, message := enforceAccessKeyLimits(r, identity)
			if status != 0 {
				writeKeyLimitRejection(w, r, status, message)
				return
			}
			if release != nil {
				defer release()
			}
			next.ServeHTTP(w, r.WithContext(withIdentity(r.Context(), identity)))
		})
	}
}

// expandModelAliases keeps model-scoped keys compatible with the request
// aliases exposed by the OpenAI model list. A restriction written for the
// canonical model id must authorize its configured aliases as well; otherwise
// RequireScope runs before request-context normalization and would reject a
// valid alias even though dispatch resolves it to the same backend.
func expandModelAliases(cfg config.Config, models []string) []string {
	if len(models) == 0 || (len(cfg.Models) == 0 && len(cfg.Selectors) == 0 && len(cfg.Providers) == 0) {
		return append([]string(nil), models...)
	}
	out := append([]string(nil), models...)
	seen := make(map[string]struct{}, len(out))
	for _, model := range out {
		seen[model] = struct{}{}
	}
	restrictions := auth.Identity{Models: models}
	for canonical, modelConfig := range cfg.Models {
		// Treat a canonical ID and each configured alias as one authorization
		// target. This handles both directions: a key restricted to the
		// canonical ID may call an alias, and a key created with an alias may
		// still call the canonical ID after request normalization.
		allowed := restrictions.HasModel(canonical)
		if !allowed {
			for _, alias := range modelConfig.Aliases {
				if restrictions.HasModel(alias) {
					allowed = true
					break
				}
			}
		}
		if !allowed {
			continue
		}
		if _, exists := seen[canonical]; !exists {
			seen[canonical] = struct{}{}
			out = append(out, canonical)
		}
		for _, alias := range modelConfig.Aliases {
			if alias = strings.TrimSpace(alias); alias != "" {
				if _, exists := seen[alias]; !exists {
					seen[alias] = struct{}{}
					out = append(out, alias)
				}
			}
		}
	}
	// Provider models work the same way: the client-facing name, every alias
	// and the provider-qualified form all name one model, so an allowlist entry
	// in any of those spellings authorizes the whole group. An operator picks
	// the model once; the alias spellings follow from the model definition.
	for _, providerID := range sortedProviderIDs(cfg) {
		provider := cfg.Providers[providerID]
		if provider.Disabled {
			continue
		}
		for _, model := range provider.AllModels() {
			names := providerActivityNames(providerID, provider, model)
			authorized := false
			for _, name := range names {
				if _, exists := seen[name]; exists {
					authorized = true
					break
				}
			}
			if !authorized {
				continue
			}
			for _, name := range names {
				if _, exists := seen[name]; !exists {
					seen[name] = struct{}{}
					out = append(out, name)
				}
			}
		}
	}

	// RequireScope runs before selector rewriting, so a model-scoped key must
	// also carry the selector name when every concrete target is already
	// allowed.  Appending only fully-authorized selectors prevents a selector
	// from becoming an indirect escape hatch to a denied model while allowing
	// the normal selector middleware to perform its own target check later in
	// the request chain. Selector chaining is rejected by config validation, so
	// this one pass is sufficient and cannot recurse indefinitely.
	for selectorID, selector := range cfg.Selectors {
		if _, exists := seen[selectorID]; exists || len(selector.Targets) == 0 {
			continue
		}
		allowed := true
		for _, target := range selector.Targets {
			if !modelAllowedForIdentity(cfg, restrictions, target) {
				allowed = false
				break
			}
		}
		if allowed {
			seen[selectorID] = struct{}{}
			out = append(out, selectorID)
		}
	}
	return out
}

// modelAllowedForIdentity resolves a request alias before checking a
// model-scoped identity. Inference routes are normally checked by
// RequireScope, but control-plane resources also carry model data in query
// parameters or response lists and must use the same canonical/alias rules.
func modelAllowedForIdentity(cfg config.Config, identity auth.Identity, requested string) bool {
	if identity.Legacy || len(identity.Models) == 0 {
		return true
	}
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return false
	}
	if identity.HasModel(requested) {
		return true
	}
	// A provider model answers to its client-facing name (modelPrefix + model
	// ID) and to the legacy provider/model form; an allowlist entry in either
	// spelling grants access, so introducing a modelPrefix cannot lock a key
	// out of models it was already allowed to use.
	if providerID, modelID, pool, found := cfg.ResolveProviderModel(requested); found {
		owners := []string{providerID}
		if len(pool) > 0 {
			owners = pool
		}
		for _, owner := range owners {
			if identity.HasModel(config.ProviderModelFQN(owner, modelID)) {
				return true
			}
			// The request resolved to this model: an allowlist entry naming any
			// of its spellings (client-facing name, alias, qualified form)
			// authorizes it. Requesting one name and being denied because the
			// key listed another spelling of the same model is the trap this
			// prevents.
			provider := cfg.Providers[owner]
			for _, model := range provider.AllModels() {
				if model.Local != modelID {
					continue
				}
				for _, name := range providerActivityNames(owner, provider, model) {
					if identity.HasModel(name) {
						return true
					}
				}
			}
		}
	}
	canonical, ok := cfg.RealModelName(requested)
	if ok {
		if identity.HasModel(canonical) {
			return true
		}
		if model, exists := cfg.Models[canonical]; exists {
			for _, alias := range model.Aliases {
				if identity.HasModel(alias) {
					return true
				}
			}
		}
	}
	// Peer targets are normally exposed as fully qualified names. Selector
	// configuration may still use an unqualified name when exactly one peer
	// owns it, so authorize both spellings against the same concrete target.
	if peerID, peerModel, found := cfg.ResolvePeerModel(requested); found {
		if identity.HasModel(config.PeerModelFQN(peerID, peerModel)) {
			return true
		}
		// An unqualified peer model is only an alias when it resolves to one
		// owner. Do not let an ambiguous name authorize every peer namespace.
		if resolvedPeer, resolvedModel, unique := cfg.ResolvePeerModel(peerModel); unique && resolvedPeer == peerID && resolvedModel == peerModel && identity.HasModel(peerModel) {
			return true
		}
	}
	return false
}

// selectorAllowedForIdentity prevents a model-scoped key from using a virtual
// selector as an indirect route to a target it could not call directly. A
// selector can choose any of its configured targets, so every target must be
// visible before the selector itself is considered usable.
func selectorAllowedForIdentity(cfg config.Config, identity auth.Identity, selector config.SelectorConfig) bool {
	if identity.Legacy || len(identity.Models) == 0 {
		return true
	}
	for _, target := range selector.Targets {
		if !modelAllowedForIdentity(cfg, identity, target) {
			return false
		}
	}
	return len(selector.Targets) > 0
}

// allowedCanonicalModels returns the concrete model IDs visible to a
// model-scoped identity. A nil slice means unrestricted access; an empty
// non-nil slice means the key is restricted but none of its patterns matches a
// configured model. Keeping that distinction lets callers return an empty
// aggregate instead of accidentally dropping the authorization filter.
func allowedCanonicalModels(cfg config.Config, identity auth.Identity) (models []string, restricted bool) {
	if identity.Legacy || len(identity.Models) == 0 {
		return nil, false
	}
	models = make([]string, 0, len(cfg.Models)+len(cfg.Peers))
	for canonical := range cfg.Models {
		if modelAllowedForIdentity(cfg, identity, canonical) {
			models = append(models, canonical)
		}
	}
	seen := make(map[string]struct{}, len(models))
	for _, model := range models {
		seen[model] = struct{}{}
	}
	for peerID, peer := range cfg.Peers {
		for _, peerModel := range peer.Models {
			fqn := config.PeerModelFQN(peerID, peerModel)
			if modelAllowedForIdentity(cfg, identity, fqn) {
				if _, duplicate := seen[fqn]; duplicate {
					continue
				}
				seen[fqn] = struct{}{}
				models = append(models, fqn)
			}
		}
	}
	sort.Strings(models)
	return models, true
}

// runtimeAllowedForIdentity treats a runtime as a shared control-plane
// resource. A model-scoped key may inspect or mutate it only when the runtime
// is referenced by at least one configured model and every such model is in
// the key's allowlist. Allowing a runtime shared with a denied model would
// leak its version/source and, for admin operations, could change the runtime
// used by another tenant. Unrestricted and legacy identities retain the
// existing global behavior.
func runtimeAllowedForIdentity(cfg config.Config, identity auth.Identity, runtimeName string) bool {
	if identity.Legacy || len(identity.Models) == 0 {
		return true
	}
	runtimeName = strings.TrimSpace(runtimeName)
	if runtimeName == "" {
		return false
	}
	found := false
	for modelID, model := range cfg.Models {
		if strings.TrimSpace(model.Backend.Runtime) != runtimeName {
			continue
		}
		found = true
		if !modelAllowedForIdentity(cfg, identity, modelID) {
			return false
		}
	}
	return found
}

// configuredActivityModels returns the concrete model IDs that are currently
// routable. Activity rows are intentionally retained after a model is deleted
// so audit history remains intact; callers can opt into this set when they
// need a current-model usage view.
func configuredActivityModels(cfg config.Config) []string {
	models := make([]string, 0, len(cfg.Models)+len(cfg.Peers))
	seen := make(map[string]struct{}, len(cfg.Models)+len(cfg.Peers))
	for model := range cfg.Models {
		if _, duplicate := seen[model]; duplicate {
			continue
		}
		seen[model] = struct{}{}
		models = append(models, model)
	}
	// Provider models are forwarded upstream, so usage and request records for
	// them are as real as a local model's. A request may name one through any
	// of its routing names: the client-facing name (prefix + model), an alias,
	// or the provider-qualified form.
	for _, providerID := range sortedProviderIDs(cfg) {
		provider := cfg.Providers[providerID]
		if provider.Disabled {
			continue
		}
		for _, model := range provider.AllModels() {
			for _, name := range providerActivityNames(providerID, provider, model) {
				if _, duplicate := seen[name]; duplicate {
					continue
				}
				seen[name] = struct{}{}
				models = append(models, name)
			}
		}
	}
	for peerID, peer := range cfg.Peers {
		for _, model := range peer.Models {
			fqn := config.PeerModelFQN(peerID, model)
			if _, duplicate := seen[fqn]; duplicate {
				continue
			}
			seen[fqn] = struct{}{}
			models = append(models, fqn)
		}
	}
	sort.Strings(models)
	return models
}

// providerActivityNames lists every name a provider model can be requested
// under, so a usage filter can resolve what was recorded.
func providerActivityNames(providerID string, provider config.ProviderConfig, model config.ProviderModel) []string {
	names := make([]string, 0, 4)
	if name := strings.TrimSpace(config.ProviderModelName(provider.ModelPrefix, model.Local)); name != "" {
		names = append(names, name)
	}
	if fqn := config.ProviderModelFQN(providerID, model.Local); fqn != "" {
		names = append(names, fqn)
	}
	for _, alias := range model.Aliases {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		names = append(names, config.ProviderModelName(provider.ModelPrefix, alias))
		names = append(names, config.ProviderModelFQN(providerID, alias))
	}
	// The upstream name is callable when the provider exposes it, so a key
	// restricted to this model has to accept it as well.
	if provider.ExposesUpstreamName() {
		if remote := strings.TrimSpace(model.Remote); remote != "" {
			names = append(names, config.ProviderModelName(provider.ModelPrefix, remote))
			names = append(names, config.ProviderModelFQN(providerID, remote))
		}
	}
	return names
}

func configuredActivityModel(cfg config.Config, requested string) (string, bool) {
	requested = strings.TrimSpace(requested)
	if requested == "" {
		return "", false
	}
	if canonical, ok := cfg.RealModelName(requested); ok {
		return canonical, true
	}
	if peerID, peerModel, ok := cfg.ResolvePeerModel(requested); ok {
		return config.PeerModelFQN(peerID, peerModel), true
	}
	// A provider model keeps the spelling the client used: the gateway accepts
	// the client-facing name, an alias and the provider-qualified form, and the
	// recorded row carries whatever the request said.
	for _, providerID := range sortedProviderIDs(cfg) {
		provider := cfg.Providers[providerID]
		if provider.Disabled {
			continue
		}
		for _, model := range provider.AllModels() {
			for _, name := range providerActivityNames(providerID, provider, model) {
				if name == requested {
					return requested, true
				}
			}
		}
	}
	return "", false
}

// scopedActivityModels canonicalizes model query parameters and applies the
// identity's model allowlist before an activity/stats query reaches SQLite.
// The empty result flag is distinct from an unrestricted nil slice: a scoped
// key whose patterns match no configured model must receive an empty result,
// never an accidentally unfiltered table scan.
func scopedActivityModels(cfg config.Config, identity auth.Identity, requested []string, configuredOnly bool) (models []string, empty bool, err error) {
	cleanRequested := make([]string, 0, len(requested))
	for _, raw := range requested {
		if value := strings.TrimSpace(raw); value != "" {
			cleanRequested = append(cleanRequested, value)
		}
	}
	requested = cleanRequested
	if len(requested) == 0 {
		if configuredOnly {
			requested = configuredActivityModels(cfg)
		} else {
			allowed, restricted := allowedCanonicalModels(cfg, identity)
			if restricted {
				if len(allowed) == 0 {
					return nil, true, nil
				}
				return allowed, false, nil
			}
			return nil, false, nil
		}
	}
	seen := make(map[string]struct{}, len(requested))
	for _, raw := range requested {
		requestedModel := strings.TrimSpace(raw)
		if requestedModel == "" {
			continue
		}
		canonical := requestedModel
		if configuredOnly {
			resolved, ok := configuredActivityModel(cfg, requestedModel)
			if !ok {
				// A stale UI filter should produce an empty current-model view,
				// not resurrect a historical row for a deleted model.
				continue
			}
			canonical = resolved
		}
		if !modelAllowedForIdentity(cfg, identity, requestedModel) {
			return nil, false, fmt.Errorf("forbidden: API key is not permitted for model %s", requestedModel)
		}
		if !configuredOnly {
			if resolved, ok := cfg.RealModelName(requestedModel); ok {
				canonical = resolved
			}
		}
		if _, exists := seen[canonical]; exists {
			continue
		}
		seen[canonical] = struct{}{}
		models = append(models, canonical)
	}
	if len(models) == 0 {
		return nil, true, nil
	}
	sort.Strings(models)
	return models, false, nil
}

func RequireScope(scope string) chain.Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			identity := identityFromContext(r.Context())
			// No configured key is the legacy default-allow state; the auth
			// middleware has already made that determination.
			if identity.ID == "" || identity.Has(scope) {
				if len(identity.Models) > 0 {
					model := strings.TrimSpace(r.PathValue("model"))
					if model == "" {
						if data, ok := swaputil.ReadContext(r.Context()); ok {
							model = strings.TrimSpace(data.Model)
						}
					}
					if model == "" && scope == auth.ScopeInference {
						model, _ = swaputil.ExtractModel(r)
					}
					if model != "" && !identity.HasModel(model) {
						swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: API key is not permitted for model "+model)
						return
					}
				}
				next.ServeHTTP(w, r)
				return
			}
			swaputil.SendResponse(w, r, http.StatusForbidden, "forbidden: missing scope "+scope)
		})
	}
}
