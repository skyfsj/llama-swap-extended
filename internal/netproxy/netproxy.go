// Package netproxy builds the outbound HTTP clients the daemon uses for
// everything that is not a model backend: extension fetches, model downloads,
// pricing refreshes and runtime metadata lookups.
//
// One place decides what "use the proxy" means so an operator configures it
// once instead of per subsystem. The semantics mirror the provider gateway's
// per-credential proxy handling, which is the existing precedent:
//
//   - an empty setting follows the process environment (HTTP_PROXY,
//     HTTPS_PROXY, ALL_PROXY, NO_PROXY) — the historical behavior;
//   - "direct" (or "none") forces a direct connection, ignoring the
//     environment;
//   - an http/https URL is used as the proxy;
//   - a socks5:// or socks5h:// URL is dialed through golang.org/x/net/proxy
//     (socks5h resolves names remotely).
//
// NoProxy entries are matched against the request host the way curl's
// --noproxy does: an exact host, a leading-dot suffix or a bare domain all
// match that domain and its subdomains, and "*" disables proxying entirely.
//
// Clients that make their own internet requests should build one from here
// (Client, Transport) rather than relying on the process default transport:
// net/http hands HTTPS requests to the HTTP/2 alternative protocol before it
// consults Transport.Proxy, so a pooled HTTP/2 connection would keep using the
// decision made when it was established. The process transport keeps HTTP/2
// enabled and closes its idle connections whenever the policy changes, so the
// protocol is negotiated while every connection still answers to the policy in
// force when it was opened.
package netproxy

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/net/proxy"
)

// Settings is the resolved network configuration for outbound clients.
type Settings struct {
	// Proxy is the operator's proxy setting: empty (follow the environment),
	// "direct"/"none", or an http(s)/socks5(h) URL.
	Proxy string
	// NoProxy lists hosts that bypass the proxy.
	NoProxy []string
}

// Direct reports whether proxying is disabled outright.
func (s Settings) Direct() bool {
	switch strings.ToLower(strings.TrimSpace(s.Proxy)) {
	case "direct", "none":
		return true
	}
	return false
}

// Configured reports whether the operator pinned a proxy (not the environment
// default).
func (s Settings) Configured() bool {
	return strings.TrimSpace(s.Proxy) != ""
}

// proxyFunc returns the transport's Proxy callback.
func (s Settings) proxyFunc() (func(*http.Request) (*url.URL, error), error) {
	if _, err := s.proxyURL(); err != nil {
		return nil, err
	}
	return s.proxyFor, nil
}

// proxyURL parses the configured proxy, returning nil for the cases that need
// no Proxy callback (unset, direct, socks5 — socks5 is handled by a dialer).
func (s Settings) proxyURL() (*url.URL, error) {
	trimmed := strings.TrimSpace(s.Proxy)
	if trimmed == "" || s.Direct() {
		return nil, nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("network proxy %q is not a valid URL: %w", Redact(trimmed), err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return parsed, nil
	case "socks5", "socks5h":
		return nil, nil
	default:
		return nil, fmt.Errorf("network proxy %q has an unsupported scheme %q", Redact(trimmed), parsed.Scheme)
	}
}

// proxyFor decides per request whether it goes through the proxy.
//
// Once a policy is configured, traffic that cannot meaningfully travel through
// a proxy is sent directly: loopback (a model backend on 127.0.0.1, a peer on
// the same host), private, link-local and unique-local addresses (the model
// containers and other machines on the LAN), and single-label hostnames (a
// container or host name that only resolves inside the local network). Nothing
// on an operator's local network needs an egress proxy, and routing it through
// one would break exactly the connections that must keep working.
//
// Leaving the policy empty keeps the historical behavior untouched: whatever
// the process environment says, resolved by net/http exactly as before.
func (s Settings) proxyFor(request *http.Request) (*url.URL, error) {
	host := request.URL.Hostname()
	if !s.Configured() {
		if matchesNoProxy(host, s.NoProxy) {
			return nil, nil
		}
		return s.envProxyFor(request)
	}
	if isLocalHost(host) || matchesNoProxy(host, s.NoProxy) {
		return nil, nil
	}
	if s.Direct() {
		return nil, nil
	}
	return s.proxyURL()
}

// envProxyFor resolves the request through the process environment (or the
// transport's own proxy function when the default transport was customized).
func (s Settings) envProxyFor(request *http.Request) (*url.URL, error) {
	if original := originalProxy(); original != nil {
		return original(request)
	}
	return http.ProxyFromEnvironment(request)
}

// isLocalHost reports whether a host is local enough that a proxy cannot help.
func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	switch host {
	case "localhost", "host.docker.internal", "gateway.docker.internal":
		return true
	}
	if !strings.Contains(host, ".") {
		// A single label resolves inside the local network, never on the
		// public internet: a container name, a hostname without a domain.
		return true
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast()
}

// Client builds an outbound client under the policy currently installed for
// the process. Clients that make internet requests on their own should use
// this instead of relying on the default transport: the policy is then a
// property of the request path rather than of a global that anything could
// replace.
func Client(timeout time.Duration) (*http.Client, error) {
	return Current().Client(timeout)
}

// ProxyFunc returns the per-request proxy decision for this policy. A client
// that builds its own transport can hand it to http.Transport.Proxy so it
// follows the policy (and later reloads) without duplicating the rules.
func (s Settings) ProxyFunc() func(*http.Request) (*url.URL, error) {
	return s.proxyFor
}

// Transport builds an outbound transport honoring the settings. The returned
// transport has the same connection hygiene the extension fetcher already
// used (bounded dial timeout) plus proxy support.
func (s Settings) Transport() (*http.Transport, error) {
	proxyFn, err := s.proxyFunc()
	if err != nil {
		return nil, err
	}
	transport := &http.Transport{
		Proxy:               proxyFn,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
		MaxIdleConns:        32,
		IdleConnTimeout:     30 * time.Second,
		TLSHandshakeTimeout: 10 * time.Second,
		// A proxy that accepts connections and then says nothing would
		// otherwise hold the caller for as long as the caller tolerates: these
		// clients only fetch metadata and few-hundred-kilobyte catalogs, so
		// waiting minutes for response headers is never progress.
		ResponseHeaderTimeout: 30 * time.Second,
	}
	// A socks5 proxy needs its own dialer: Go's Transport can only dial HTTP
	// proxies through the Proxy callback.
	if dialer, ok := s.socksDialer(); ok {
		transport.DialContext = dialer
	}
	return transport, nil
}

// The process-wide policy. Clients that build their own transport without a
// proxy (pricing refresh, model downloads, runtime metadata, backend calls)
// all fall back to http.DefaultTransport, so one policy-aware default
// transport covers them without touching each call site.
var (
	livePolicy atomic.Pointer[Settings]
	// envProxy keeps the transport's own proxy function so a customized
	// default transport is still honored when no policy is set.
	envProxy atomic.Pointer[func(*http.Request) (*url.URL, error)]
)

func originalProxy() func(*http.Request) (*url.URL, error) {
	if fn := envProxy.Load(); fn != nil {
		return *fn
	}
	return nil
}

// Install publishes the policy to the whole process: http.DefaultTransport for
// clients that do not build their own, and the environment for child
// processes.
//
// The transport indirection is not decoration: net/http caches the proxy
// environment on first use, so os.Setenv alone cannot change the proxy of a
// running process — a reload would silently keep using the old value.
// The process transport is rebuilt whenever the policy changes, and that is not
// cosmetic: net/http hands HTTPS requests to the HTTP/2 alternative protocol
// before it consults Transport.Proxy, an HTTP/2 connection is pooled per host,
// and Transport.CloseIdleConnections does not reach that pool. A transport that
// survived a policy change would keep serving requests over connections the
// previous policy opened — measured, not assumed. A fresh transport makes every
// connection answer to the policy in force when it was opened, which is what
// lets the protocol itself stay automatic.
var (
	liveTransportMu  sync.Mutex
	liveTransport    *http.Transport
	liveTransportKey string
)

func Install(settings Settings) {
	policy := settings
	livePolicy.Store(&policy)
	slog.Info("network policy applied", "proxy", Redact(strings.TrimSpace(policy.Proxy)), "noProxy", strings.Join(policy.NoProxy, ","))

	liveTransportMu.Lock()
	if key := policyKey(settings); liveTransport == nil || key != liveTransportKey {
		liveTransport = newPolicyTransport()
		liveTransportKey = key
		http.DefaultTransport = liveTransport
	}
	liveTransportMu.Unlock()

	settings.ApplyEnv()
}

// policyKey identifies a policy so Install only rebuilds on a real change.
func policyKey(settings Settings) string {
	return strings.TrimSpace(settings.Proxy) + "\x00" + strings.Join(settings.NoProxy, ",")
}

// newPolicyTransport builds the process transport: the HTTP version is
// negotiated (an upstream that only speaks HTTP/2 is reached over HTTP/2) and
// the proxy decision is made per request.
//
// Two inherited fields have to be reset for HTTP/2 to actually work. The clone
// carries a DialContext, which turns HTTP/2 off unless ForceAttemptHTTP2 is set,
// and it carries the TLSNextProto map of a transport that has already served
// requests — net/http reads a non-nil map as "HTTP/2 is already configured" and
// skips its own setup, leaving this transport on HTTP/1.1 while its TLS config
// still advertises h2 over ALPN. That mismatch made an HTTP/2-only upstream
// answer with HTTP/2 frames on an HTTP/1.1 request.
func newPolicyTransport() *http.Transport {
	base, _ := http.DefaultTransport.(*http.Transport)
	if base == nil {
		base = &http.Transport{}
	}
	transport := base.Clone()
	previous := transport.Proxy
	if previous != nil {
		envProxy.Store(&previous)
	}
	transport.ForceAttemptHTTP2 = true
	transport.TLSNextProto = nil
	transport.Proxy = func(request *http.Request) (*url.URL, error) {
		if current := livePolicy.Load(); current != nil {
			return current.proxyFor(request)
		}
		if previous != nil {
			return previous(request)
		}
		return http.ProxyFromEnvironment(request)
	}
	return transport
}

// Current returns the policy installed for the process.
func Current() Settings {
	if policy := livePolicy.Load(); policy != nil {
		return *policy
	}
	return Settings{}
}

func (s Settings) socksDialer() (func(ctx context.Context, network, addr string) (net.Conn, error), bool) {
	trimmed := strings.TrimSpace(s.Proxy)
	parsed, err := url.Parse(trimmed)
	if err != nil || (parsed.Scheme != "socks5" && parsed.Scheme != "socks5h") {
		return nil, false
	}
	auth := (*proxy.Auth)(nil)
	if parsed.User != nil {
		password, _ := parsed.User.Password()
		auth = &proxy.Auth{User: parsed.User.Username(), Password: password}
	}
	base := proxy.Direct
	if dialer, err := proxy.SOCKS5("tcp", parsed.Host, auth, base); err == nil {
		if contextDialer, ok := dialer.(proxy.ContextDialer); ok {
			return contextDialer.DialContext, true
		}
	}
	return nil, false
}

// Client builds an outbound client with the transport above and the supplied
// timeout (zero keeps no overall timeout).
func (s Settings) Client(timeout time.Duration) (*http.Client, error) {
	transport, err := s.Transport()
	if err != nil {
		return nil, err
	}
	return &http.Client{Transport: transport, Timeout: timeout}, nil
}

// Env renders the proxy as environment variables for a child process (runtime
// builds, extension installs). Explicitly configured variables win: an empty
// result means the child inherits the daemon environment unchanged.
func (s Settings) Env() []string {
	if !s.Configured() {
		if len(s.NoProxy) == 0 {
			return nil
		}
	}
	env := make([]string, 0, 6)
	env = append(env, EnvProxy+"="+strings.TrimSpace(s.Proxy))
	if len(s.NoProxy) > 0 {
		env = append(env, EnvNoProxy+"="+strings.Join(s.NoProxy, ","))
	}
	if !s.Direct() {
		if trimmed := strings.TrimSpace(s.Proxy); trimmed != "" {
			env = append(env, "HTTP_PROXY="+trimmed, "HTTPS_PROXY="+trimmed, "ALL_PROXY="+trimmed)
		}
	}
	if len(s.NoProxy) > 0 {
		env = append(env, "NO_PROXY="+strings.Join(s.NoProxy, ","))
	}
	return env
}

// ApplyEnv publishes the policy to the process environment so every child
// process (runtime builds, extension installs: pip, uv, npm, git) inherits it
// without each of them being wired individually.
//
// The environment is left untouched when nothing is configured, preserving the
// historical "whatever the daemon was started with" behavior — and a policy
// that was cleared restores that original environment instead of leaving the
// previous proxy behind for every process spawned afterwards. "direct" clears
// the variables and adds "*" to NO_PROXY, because the classic variables are
// additive: only an empty value plus a wildcard expresses "never proxy".
func (s Settings) ApplyEnv() {
	// Snapshot before touching anything: this is what a cleared policy
	// restores.
	rememberOriginalEnv()
	hosts := strings.Join(s.NoProxy, ",")
	if !s.Configured() && hosts == "" {
		restoreOriginalEnv()
		return
	}
	if !s.Configured() {
		os.Setenv("NO_PROXY", hosts)
		os.Setenv("no_proxy", hosts)
		return
	}
	value := strings.TrimSpace(s.Proxy)
	if s.Direct() {
		value = ""
		if hosts == "" {
			hosts = "*"
		}
	}
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		os.Setenv(name, value)
		os.Setenv(strings.ToLower(name), value)
	}
	if hosts != "" {
		os.Setenv("NO_PROXY", hosts)
		os.Setenv("no_proxy", hosts)
	}
}

// The variables ApplyEnv may touch, with the values the daemon was started
// with, so clearing the policy can put the environment back the way it was.
var (
	envNames      = []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"}
	envOnce       sync.Once
	envOriginal   map[string]string
	envOriginalOK map[string]bool
)

func rememberOriginalEnv() {
	envOnce.Do(func() {
		envOriginal = make(map[string]string, len(envNames))
		envOriginalOK = make(map[string]bool, len(envNames))
		for _, name := range envNames {
			value, ok := os.LookupEnv(name)
			envOriginal[name] = value
			envOriginalOK[name] = ok
		}
	})
}

// resetEnvSnapshotForTest lets a test define what "the daemon was started
// with" means; production captures the snapshot once, on the first apply.
func resetEnvSnapshotForTest() {
	envOnce = sync.Once{}
	envOriginal, envOriginalOK = nil, nil
}

func restoreOriginalEnv() {
	for _, name := range envNames {
		if envOriginalOK[name] {
			os.Setenv(name, envOriginal[name])
			continue
		}
		os.Unsetenv(name)
	}
}

// matchesNoProxy reports whether host bypasses the proxy.
func matchesNoProxy(host string, entries []string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	if host == "" {
		return false
	}
	for _, entry := range entries {
		entry = strings.ToLower(strings.TrimSpace(entry))
		if entry == "" {
			continue
		}
		if entry == "*" {
			return true
		}
		entry = strings.TrimPrefix(entry, ".")
		if host == entry || strings.HasSuffix(host, "."+entry) {
			return true
		}
	}
	return false
}

// The daemon passes its resolved policy to extension worker subprocesses
// through these variables: the workers do their own outbound fetches, and the
// classic HTTP_PROXY variables cannot express "direct" (they are additive).
const (
	EnvProxy   = "LLAMA_SWAP_NETWORK_PROXY"
	EnvNoProxy = "LLAMA_SWAP_NETWORK_NO_PROXY"
)

// FromEnv reads the policy a parent process passed down.
func FromEnv() Settings {
	return Settings{
		Proxy:   strings.TrimSpace(os.Getenv(EnvProxy)),
		NoProxy: splitHosts(os.Getenv(EnvNoProxy)),
	}
}

func splitHosts(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// Redact removes credentials from a proxy URL for logs and error messages.
func Redact(value string) string {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.User == nil {
		return value
	}
	parsed.User = url.UserPassword("***", "***")
	return parsed.String()
}

// Validate checks an operator-supplied proxy value.
func Validate(value string) error {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	switch strings.ToLower(trimmed) {
	case "direct", "none":
		return nil
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("proxy is not a valid URL: %w", err)
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return fmt.Errorf("proxy scheme %q is not supported (use http, https, socks5 or socks5h)", parsed.Scheme)
	}
	if parsed.Host == "" {
		return fmt.Errorf("proxy %q has no host", Redact(trimmed))
	}
	return nil
}
