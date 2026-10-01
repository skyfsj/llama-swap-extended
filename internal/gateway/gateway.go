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
	"errors"
	"fmt"
	"io"
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
)

// Route is one schedulable provider model: a scheduler over the credentials
// allowed to serve it plus the upstream mapping.
type Route struct {
	ProviderID string
	// ModelID is the local model ID as configured under providers.<id>.models.
	ModelID string
	// RemoteName is the upstream model name sent to the provider.
	RemoteName string
	// ProviderFirst marks bareNamePolicy=provider-first.
	ProviderFirst bool
	// Sticky controls session affinity for this route.
	Sticky bool
	// MaxRetries is how many credential switches a failing request may make.
	MaxRetries int
	// Members are the credentials serving this route. For bare-name pools
	// this spans several providers; the credential ID prefix identifies the
	// origin provider.
	Members []*Credential

	sched    *scheduler
	ptype    providerType
	endpoint *url.URL
	timeouts transportTimeouts
}

type providerType int

const (
	providerOpenAI providerType = iota
	providerAnthropic
)

// Gateway forwards requests to configured upstream providers.
type Gateway struct {
	mu         sync.RWMutex
	routes     map[string]*Route // FQN provider/model -> route
	bareRoutes map[string][]*Route
	sticky     *stickyTable
	cooldown   cooldownConfig
	logger     *logmon.Monitor
	transports *transportCache
}

type cooldownConfig struct {
	auth, rateLimit, transient time.Duration
	stickyTTL                  time.Duration
}

// New builds a Gateway from configuration. Credentials whose valueEnv
// variable is unset are kept but marked unschedulable, so a missing env var
// degrades one key instead of failing the whole config.
func New(cfg config.Config, logger *logmon.Monitor) (*Gateway, error) {
	g := &Gateway{
		routes:     make(map[string]*Route),
		bareRoutes: make(map[string][]*Route),
		logger:     logger,
		transports: newTransportCache(),
	}
	g.cooldown = resolveCooldowns(cfg.Gateway)
	g.sticky = newStickyTable(g.cooldown.stickyTTL)

	providerIDs := make([]string, 0, len(cfg.Providers))
	for id := range cfg.Providers {
		providerIDs = append(providerIDs, id)
	}
	sort.Strings(providerIDs)

	for _, id := range providerIDs {
		pcfg := cfg.Providers[id]
		if pcfg.Disabled {
			continue
		}

		endpoint, err := url.Parse(pcfg.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("providers.%s: invalid endpoint: %w", id, err)
		}

		ptype := providerOpenAI
		if pcfg.Type == config.ProviderTypeAnthropic {
			ptype = providerAnthropic
		}

		for _, model := range pcfg.Models {
			members := make([]*Credential, 0, len(pcfg.ApiKeys))
			for _, keyCfg := range pcfg.ApiKeys {
				value := keyCfg.Value
				if value == "" && keyCfg.ValueEnv != "" {
					value = os.Getenv(keyCfg.ValueEnv)
				}
				cred := &Credential{
					ID:            keyID(id, valueOrEnvRef(value, keyCfg.ValueEnv)),
					Value:         value,
					Priority:      keyCfg.Priority,
					Weight:        keyCfg.Weight,
					MaxConcurrent: keyCfg.MaxConcurrent,
					Proxy:         pcfg.ResolveProviderKeyProxy(keyCfg),
					Disabled:      keyCfg.Disabled,
				}
				if !keyCfg.Disabled {
					members = append(members, cred)
				}
			}

			route := &Route{
				ProviderID:    id,
				ModelID:       model.Local,
				RemoteName:    model.Remote,
				ProviderFirst: pcfg.BareNamePolicy == "provider-first",
				Sticky:        pcfg.StickyEnabled(),
				MaxRetries:    pcfg.MaxRetries,
				Members:       members,
				ptype:         ptype,
				endpoint:      endpoint,
				timeouts: transportTimeouts{
					connect:        pcfg.Timeouts.Connect,
					keepAlive:      pcfg.Timeouts.KeepAlive,
					responseHeader: pcfg.Timeouts.ResponseHeader,
					tlsHandshake:   pcfg.Timeouts.TLSHandshake,
					idleConn:       pcfg.Timeouts.IdleConn,
				},
			}
			route.sched = newScheduler(members)

			fqn := config.ProviderModelFQN(id, model.Local)
			if _, exists := g.routes[fqn]; exists {
				return nil, fmt.Errorf("providers.%s: duplicate model route %q", id, fqn)
			}
			g.routes[fqn] = route
			g.bareRoutes[model.Local] = append(g.bareRoutes[model.Local], route)
		}
	}
	return g, nil
}

func resolveCooldowns(gw config.GatewayConfig) cooldownConfig {
	cc := cooldownConfig{
		auth:      DefaultAuthCooldown,
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
func (g *Gateway) RouteFor(model string, localHasModel bool) *Route {
	g.mu.RLock()
	defer g.mu.RUnlock()
	if route, ok := g.routes[model]; ok {
		return route
	}
	candidates := g.bareRoutes[model]
	if len(candidates) == 0 {
		return nil
	}
	eligible := make([]*Route, 0, len(candidates))
	for _, route := range candidates {
		if localHasModel && !route.ProviderFirst {
			continue
		}
		eligible = append(eligible, route)
	}
	if len(eligible) == 0 {
		return nil
	}
	if len(eligible) == 1 {
		return eligible[0]
	}
	// Cross-provider pool: schedule by priority tier then weight over each
	// route's best credential.
	members := make([]*Credential, 0, len(eligible))
	byID := make(map[string]*Route, len(eligible))
	for _, route := range eligible {
		best := route.BestCredential()
		if best == nil {
			continue
		}
		byID[best.ID] = route
		members = append(members, best)
	}
	if len(members) == 0 {
		return nil
	}
	chosen := newScheduler(members).pick(time.Now(), nil)
	if chosen == nil {
		return nil
	}
	return byID[chosen.ID]
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

// ModelIDs lists every route's FQN (for /v1/models merging and status).
func (g *Gateway) ModelIDs() []string {
	g.mu.RLock()
	defer g.mu.RUnlock()
	ids := make([]string, 0, len(g.routes))
	for id := range g.routes {
		ids = append(ids, id)
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

// Shutdown drops sticky state and releases cached transports' idle
// connections.
func (g *Gateway) Shutdown() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.sticky = newStickyTable(g.cooldown.stickyTTL)
	g.transports.close()
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

// ServeHTTP forwards a request to the resolved route's provider. The caller
// resolves the model (swaputil.FetchContext), rewrites the model name to the
// upstream name (swaputil.ReplaceRequestModel) and passes the sticky session
// hash ("" to skip affinity), mirroring the peer dispatch path.
func (g *Gateway) ServeHTTP(w http.ResponseWriter, req *http.Request, route *Route, sessionHash string) {
	g.mu.RLock()
	cooldown := g.cooldown
	g.mu.RUnlock()

	// Session affinity: a bound credential short-circuits rotation on the
	// first attempt only; if it fails the retry loop re-schedules normally
	// and rebinds.
	var bound *Credential
	if route.Sticky && sessionHash != "" {
		if id, ok := g.sticky.get(sessionHash, time.Now()); ok {
			for _, c := range route.Members {
				if c.ID == id {
					bound = c
					break
				}
			}
			if bound == nil {
				// The bound credential left the configuration.
				g.sticky.unbind(sessionHash)
			}
		}
	}

	attempts := route.MaxRetries + 1
	if attempts < 1 {
		attempts = 1
	}
	tried := map[string]struct{}{}
	var lastStatus int
	var lastSnippet string

	// Retryable attempts must re-send the request body. httptest and plain
	// handlers do not always populate GetBody, so buffer once here and
	// rebuild the body per attempt.
	req, cancelBody, err := bufferRequestBody(req)
	if err != nil {
		swaputil.SendResponse(w, req, http.StatusBadRequest, err.Error())
		return
	}
	defer cancelBody()

	for attempt := 0; attempt < attempts; attempt++ {
		cred := bound
		bound = nil
		if cred == nil {
			cred = route.sched.pick(time.Now(), tried)
		}
		if cred == nil {
			break
		}

		release, ok := cred.acquire()
		if !ok {
			tried[cred.ID] = struct{}{}
			continue
		}

		res := g.attempt(w, req, route, cred)
		release()

		switch res.outcome {
		case outcomeCommit:
			// Success (2xx/3xx or a committed request-scoped error): bind
			// the session to the credential that served it.
			if res.status < 400 && route.Sticky && sessionHash != "" {
				g.sticky.bind(sessionHash, cred.ID, time.Now())
			}
			return
		case outcomeStop:
			return
		case outcomeRetry:
			g.applyFailure(cred, res, cooldown)
			tried[cred.ID] = struct{}{}
			lastStatus = res.status
			lastSnippet = res.snippet
			g.logger.Warnf("gateway: provider %s model %s credential %s failed (%s), %d attempt(s) left",
				route.ProviderID, route.ModelID, cred.ID, res.cause, attempts-attempt-1)
		}
	}

	if lastStatus != 0 {
		swaputil.SendResponse(w, req, lastStatus, truncate(lastSnippet, 2048))
		return
	}
	swaputil.SendResponse(w, req, http.StatusBadGateway,
		fmt.Sprintf("gateway: no available credential for provider %s model %s", route.ProviderID, route.ModelID))
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
	status  int    // upstream status (0 when no response arrived)
	snippet string // bounded upstream body / transport error text
	cause   string // short classification for logs
	// retryAfter, when set on a 429, extends the cooldown.
	retryAfter time.Time
}

// attempt performs one forwarding attempt. It buffers the upstream response
// head and the first body chunk (streaming bootstrap): until the first byte
// reaches the client a failure can be retried on another credential
// invisibly. Once the head is written the outcome is terminal.
func (g *Gateway) attempt(w http.ResponseWriter, req *http.Request, route *Route, cred *Credential) attemptResult {
	transport, err := g.transports.get(cred.Proxy, route)
	if err != nil {
		// Bad proxy configuration is credential-scoped: cool the credential
		// down and let the next attempt proceed.
		return attemptResult{outcome: outcomeRetry, cause: "bad proxy config", snippet: err.Error()}
	}

	outURL := *route.endpoint
	outURL.Path = joinPath(route.endpoint.Path, req.URL.Path)
	outURL.RawPath = req.URL.RawPath
	outURL.RawQuery = req.URL.RawQuery

	outReq := req.Clone(req.Context())
	outReq.URL = &outURL
	outReq.Host = outURL.Host
	outReq.RequestURI = ""
	removeHopHeaders(outReq.Header)
	setAuthHeaders(outReq, route.ptype, cred.Value)
	// Rebuild the body for every attempt: the previous RoundTrip consumed
	// the original Body.
	if outReq.GetBody != nil {
		body, berr := outReq.GetBody()
		if berr != nil {
			return attemptResult{outcome: outcomeStop, cause: "request body unavailable", snippet: berr.Error()}
		}
		outReq.Body = body
	}

	resp, err := transport.RoundTrip(outReq)
	if err != nil {
		if req.Context().Err() != nil {
			// The client is gone: terminal, nothing to retry for.
			return attemptResult{outcome: outcomeStop, cause: "client disconnected", snippet: err.Error()}
		}
		return attemptResult{outcome: outcomeRetry, cause: "transport error", snippet: err.Error()}
	}
	defer func() {
		if resp.Body != nil {
			resp.Body.Close()
		}
	}()

	// Buffer head + first chunk so failures before the first byte stay
	// retryable.
	buf := make([]byte, 0, 8192)
	first := make([]byte, 8192)
	n, rerr := io.ReadFull(resp.Body, first)
	if rerr != nil && rerr != io.EOF && rerr != io.ErrUnexpectedEOF {
		return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, cause: "read error", snippet: rerr.Error()}
	}
	buf = append(buf, first[:n]...)
	snippet := string(buf)

	// An SSE response with an empty bootstrap read means the upstream
	// aborted (or delivered nothing) before the first event: treat it as a
	// transient failure while we can still retry invisibly.
	if resp.StatusCode < 400 && n == 0 && isStreamingResponse(resp) && req.Context().Err() == nil {
		return attemptResult{outcome: outcomeRetry, status: resp.StatusCode, cause: "stream ended before first event", snippet: ""}
	}

	if class, ok := credentialFailure(resp.StatusCode); ok {
		res := attemptResult{outcome: outcomeRetry, status: resp.StatusCode, snippet: snippet}
		switch class {
		case CooldownAuth:
			res.cause = fmt.Sprintf("status %d (auth)", resp.StatusCode)
		case CooldownRateLimit:
			res.cause = fmt.Sprintf("status %d (rate limit)", resp.StatusCode)
			if ra, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
				res.retryAfter = ra
			} else if secs, err := strconv.Atoi(strings.TrimSpace(resp.Header.Get("Retry-After"))); err == nil && secs >= 0 {
				res.retryAfter = time.Now().Add(time.Duration(secs) * time.Second)
			}
		case CooldownTransient:
			res.cause = fmt.Sprintf("status %d (transient)", resp.StatusCode)
		}
		return res
	}

	if resp.StatusCode >= 400 {
		// Remaining upstream 4xx are request-scoped client errors: pass
		// through without cooldown or retry.
		cred.noteError(fmt.Sprintf("status %d (request-scoped)", resp.StatusCode))
		commitResponse(w, resp, buf)
		io.Copy(w, resp.Body)
		return attemptResult{outcome: outcomeCommit, status: resp.StatusCode, snippet: snippet}
	}

	// Success: commit head, replay buffered bytes, stream the rest.
	commitResponse(w, resp, buf)
	io.Copy(w, resp.Body)
	return attemptResult{outcome: outcomeCommit, status: resp.StatusCode}
}

// applyFailure records a credential failure: cooldown by class, with
// Retry-After extending rate-limit cooldowns. Transport errors (status 0)
// cool down transiently.
func (g *Gateway) applyFailure(cred *Credential, res attemptResult, cc cooldownConfig) {
	now := time.Now()
	switch {
	case res.status == 0:
		cred.cooldown(now, CooldownTransient, now.Add(cc.transient), truncate(res.snippet, 256))
	case res.cause != "" && strings.Contains(res.cause, "(auth)"):
		cred.cooldown(now, CooldownAuth, now.Add(cc.auth), fmt.Sprintf("status %d: %s", res.status, truncate(res.snippet, 256)))
	case res.cause != "" && strings.Contains(res.cause, "(rate limit)"):
		until := now.Add(cc.rateLimit)
		if res.retryAfter.After(until) {
			until = res.retryAfter
		}
		cred.cooldown(now, CooldownRateLimit, until, fmt.Sprintf("status %d: %s", res.status, truncate(res.snippet, 256)))
	case res.cause != "" && strings.Contains(res.cause, "(transient)"):
		cred.cooldown(now, CooldownTransient, now.Add(cc.transient), fmt.Sprintf("status %d: %s", res.status, truncate(res.snippet, 256)))
	default:
		// Unclassified retry: brief transient cooldown to shed load.
		cred.cooldown(now, CooldownTransient, now.Add(cc.transient), fmt.Sprintf("status %d: %s", res.status, truncate(res.snippet, 256)))
	}
}

// credentialFailure classifies status codes that implicate the credential
// (CLIProxyAPI error quadrants, minus free-text keyword matching).
func credentialFailure(status int) (CooldownClass, bool) {
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusPaymentRequired:
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
	default:
		req.Header.Set("Authorization", "Bearer "+value)
		req.Header.Del("x-api-key")
	}
}

// removeHopHeaders strips hop-by-hop headers and inbound auth so the
// upstream sees a clean request.
func removeHopHeaders(h http.Header) {
	h.Del("Authorization")
	h.Del("x-api-key")
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
		tr.Proxy = http.ProxyFromEnvironment
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
