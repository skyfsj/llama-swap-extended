package netproxy

import (
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestNetproxy_ValidateAcceptsSupportedSchemes(t *testing.T) {
	for _, value := range []string{
		"",
		"direct",
		"NONE",
		"http://proxy.example.com:3128",
		"https://user:pass@proxy.example.com:3128",
		"socks5://127.0.0.1:1080",
		"socks5h://127.0.0.1:1080",
	} {
		if err := Validate(value); err != nil {
			t.Fatalf("Validate(%q) returned %v, want nil", value, err)
		}
	}
}

func TestNetproxy_ValidateRejectsBadValues(t *testing.T) {
	cases := map[string]string{
		"ftp://proxy.example.com:21": "scheme",
		"proxy.example.com:3128":     "scheme",
		"http://":                    "no host",
	}
	for value, want := range cases {
		err := Validate(value)
		if err == nil {
			t.Fatalf("Validate(%q) returned nil, want an error mentioning %q", value, want)
		}
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("Validate(%q) = %v, want it to mention %q", value, err, want)
		}
	}
}

func TestNetproxy_ProxyFuncFollowsEnvironmentWhenUnset(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.example.com:8080")
	t.Setenv("HTTPS_PROXY", "http://env-proxy.example.com:8080")
	t.Setenv("NO_PROXY", "")

	settings := Settings{}
	transport, err := settings.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	proxyURL, err := transport.Proxy(request)
	if err != nil || proxyURL == nil {
		t.Fatalf("Proxy() = (%v, %v), want the environment proxy", proxyURL, err)
	}
	if proxyURL.Host != "env-proxy.example.com:8080" {
		t.Fatalf("Proxy() host = %q, want env-proxy.example.com:8080", proxyURL.Host)
	}
}

func TestNetproxy_ProxyFuncDirectIgnoresEnvironment(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://env-proxy.example.com:8080")
	t.Setenv("HTTPS_PROXY", "http://env-proxy.example.com:8080")

	transport, err := Settings{Proxy: "direct"}.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	proxyURL, err := transport.Proxy(request)
	if err != nil || proxyURL != nil {
		t.Fatalf("Proxy() = (%v, %v), want direct (nil, nil)", proxyURL, err)
	}
}

func TestNetproxy_ProxyFuncUsesConfiguredProxy(t *testing.T) {
	transport, err := Settings{Proxy: "http://proxy.example.com:3128"}.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	proxyURL, err := transport.Proxy(request)
	if err != nil || proxyURL == nil {
		t.Fatalf("Proxy() = (%v, %v), want the configured proxy", proxyURL, err)
	}
	if proxyURL.Host != "proxy.example.com:3128" {
		t.Fatalf("Proxy() host = %q, want proxy.example.com:3128", proxyURL.Host)
	}
}

func TestNetproxy_NoProxyBypassesConfiguredProxy(t *testing.T) {
	transport, err := Settings{
		Proxy:   "http://proxy.example.com:3128",
		NoProxy: []string{"internal.example.com", ".corp.example.com", "localhost"},
	}.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}

	bypassed := []string{
		"http://internal.example.com/",
		"http://api.corp.example.com/",
		"http://localhost:8080/health",
	}
	for _, target := range bypassed {
		request, _ := http.NewRequest(http.MethodGet, target, nil)
		proxyURL, err := transport.Proxy(request)
		if err != nil || proxyURL != nil {
			t.Fatalf("Proxy(%s) = (%v, %v), want direct", target, proxyURL, err)
		}
	}

	proxied, _ := http.NewRequest(http.MethodGet, "https://public.example.com/", nil)
	proxyURL, err := transport.Proxy(proxied)
	if err != nil || proxyURL == nil {
		t.Fatalf("Proxy(public) = (%v, %v), want the configured proxy", proxyURL, err)
	}
}

func TestNetproxy_NoProxyWildcardDisablesProxying(t *testing.T) {
	transport, err := Settings{Proxy: "http://proxy.example.com:3128", NoProxy: []string{"*"}}.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}
	request, _ := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	proxyURL, err := transport.Proxy(request)
	if err != nil || proxyURL != nil {
		t.Fatalf("Proxy() = (%v, %v), want direct for a wildcard bypass", proxyURL, err)
	}
}

func TestNetproxy_Socks5ReplacesDialer(t *testing.T) {
	direct, err := Settings{Proxy: "http://proxy.example.com:3128"}.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}
	socks, err := Settings{Proxy: "socks5://127.0.0.1:1080"}.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}
	// A socks5 proxy cannot be expressed as an HTTP proxy, so the connection
	// must be dialed by the SOCKS dialer while the Proxy callback reports no
	// HTTP proxy.
	if socks.DialContext == nil {
		t.Fatalf("socks5 transport has no dialer")
	}
	if socks.Proxy == nil {
		t.Fatalf("socks5 transport has no Proxy callback")
	}
	public, _ := http.NewRequest(http.MethodGet, "https://api.github.com/zen", nil)
	if proxyURL, err := socks.Proxy(public); err != nil || proxyURL != nil {
		t.Fatalf("socks5 Proxy() = (%v, %v), want the dialer to carry the proxy", proxyURL, err)
	}
	if direct.DialContext == nil {
		t.Fatalf("http proxy transport has no dialer")
	}
}

func TestNetproxy_ApplyEnvPublishesConfiguredProxy(t *testing.T) {
	resetEnvSnapshotForTest()
	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY"} {
		t.Setenv(name, "")
		t.Setenv(strings.ToLower(name), "")
	}

	Settings{Proxy: "http://proxy.example.com:3128", NoProxy: []string{"localhost"}}.ApplyEnv()

	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		if got := os.Getenv(name); got != "http://proxy.example.com:3128" {
			t.Fatalf("%s = %q, want the configured proxy", name, got)
		}
	}
	if got := os.Getenv("NO_PROXY"); got != "localhost" {
		t.Fatalf("NO_PROXY = %q, want localhost", got)
	}
}

func TestNetproxy_ApplyEnvDirectClearsAndBypasses(t *testing.T) {
	resetEnvSnapshotForTest()
	t.Setenv("HTTP_PROXY", "http://stale.example.com:8080")
	t.Setenv("HTTPS_PROXY", "http://stale.example.com:8080")
	t.Setenv("ALL_PROXY", "http://stale.example.com:8080")
	t.Setenv("NO_PROXY", "")

	Settings{Proxy: "direct"}.ApplyEnv()

	for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY"} {
		if got := os.Getenv(name); got != "" {
			t.Fatalf("%s = %q, want it cleared", name, got)
		}
	}
	if got := os.Getenv("NO_PROXY"); got != "*" {
		t.Fatalf("NO_PROXY = %q, want * so nothing is proxied", got)
	}
}

func TestNetproxy_ApplyEnvLeavesEnvironmentWhenUnconfigured(t *testing.T) {
	resetEnvSnapshotForTest()
	t.Setenv("HTTP_PROXY", "http://existing.example.com:8080")
	t.Setenv("NO_PROXY", "keep-me")

	Settings{}.ApplyEnv()

	if got := os.Getenv("HTTP_PROXY"); got != "http://existing.example.com:8080" {
		t.Fatalf("HTTP_PROXY = %q, want the pre-existing value untouched", got)
	}
	if got := os.Getenv("NO_PROXY"); got != "keep-me" {
		t.Fatalf("NO_PROXY = %q, want the pre-existing value untouched", got)
	}
}

func TestNetproxy_ApplyEnvRestoresStartupEnvironmentWhenCleared(t *testing.T) {
	resetEnvSnapshotForTest()
	t.Setenv("HTTP_PROXY", "http://startup.example.com:8080")
	t.Setenv("NO_PROXY", "startup.example.com")

	Settings{Proxy: "http://proxy.example.com:3128", NoProxy: []string{"localhost"}}.ApplyEnv()
	if got := os.Getenv("HTTP_PROXY"); got != "http://proxy.example.com:3128" {
		t.Fatalf("HTTP_PROXY = %q, want the configured proxy", got)
	}

	// Clearing the policy must put the daemon's own environment back: child
	// processes spawned later (pip, npm, git) would otherwise keep dialing a
	// proxy the operator just removed.
	Settings{}.ApplyEnv()
	if got := os.Getenv("HTTP_PROXY"); got != "http://startup.example.com:8080" {
		t.Fatalf("HTTP_PROXY = %q after clearing, want the startup value", got)
	}
	if got := os.Getenv("NO_PROXY"); got != "startup.example.com" {
		t.Fatalf("NO_PROXY = %q after clearing, want the startup value", got)
	}
}

func TestNetproxy_EnvRoundTripsForChildProcesses(t *testing.T) {
	settings := Settings{Proxy: "socks5://127.0.0.1:1080", NoProxy: []string{"localhost", "127.0.0.1"}}
	env := settings.Env()

	want := map[string]string{
		EnvProxy:   "socks5://127.0.0.1:1080",
		EnvNoProxy: "localhost,127.0.0.1",
	}
	for key, value := range want {
		if !containsEnv(env, key, value) {
			t.Fatalf("Env() = %v, want %s=%s", env, key, value)
		}
	}

	t.Setenv(EnvProxy, "socks5://127.0.0.1:1080")
	t.Setenv(EnvNoProxy, "localhost,127.0.0.1")
	restored := FromEnv()
	if restored.Proxy != "socks5://127.0.0.1:1080" {
		t.Fatalf("FromEnv().Proxy = %q", restored.Proxy)
	}
	if strings.Join(restored.NoProxy, ",") != "localhost,127.0.0.1" {
		t.Fatalf("FromEnv().NoProxy = %v", restored.NoProxy)
	}
	// "direct" survives the round trip, which the classic variables cannot
	// express on their own.
	t.Setenv(EnvProxy, "direct")
	if !FromEnv().Direct() {
		t.Fatalf("FromEnv() did not preserve the direct policy")
	}
}

func TestNetproxy_RedactRemovesCredentials(t *testing.T) {
	got := Redact("http://user:secret@proxy.example.com:3128")
	if strings.Contains(got, "secret") || strings.Contains(got, "user") {
		t.Fatalf("Redact() = %q, want the credentials removed", got)
	}
	if !strings.Contains(got, "proxy.example.com:3128") {
		t.Fatalf("Redact() = %q, want the host preserved", got)
	}
}

func containsEnv(env []string, key, value string) bool {
	prefix := key + "="
	for _, entry := range env {
		if strings.HasPrefix(entry, prefix) && strings.TrimPrefix(entry, prefix) == value {
			return true
		}
	}
	return false
}

func TestNetproxy_LocalTrafficBypassesConfiguredProxy(t *testing.T) {
	transport, err := Settings{Proxy: "http://proxy.example.com:3128"}.Transport()
	if err != nil {
		t.Fatalf("Transport() returned %v", err)
	}
	// Model backends, peer nodes and container names never belong in an
	// egress proxy: routing them through one would break the connections
	// that must keep working.
	local := []string{
		"http://127.0.0.1:8080/v1/models",
		"http://[::1]:8080/health",
		"http://localhost:8080/health",
		"http://gpu-server:8000/v1/models",
		"http://10.0.0.5:8000/health",
		"http://172.16.4.9:8000/health",
		"http://192.168.100.10:9292/health",
		"http://169.254.1.1/health",
		"http://[fd00::1]:8080/health",
		"http://host.docker.internal:8080/health",
	}
	for _, target := range local {
		request, _ := http.NewRequest(http.MethodGet, target, nil)
		proxyURL, err := transport.Proxy(request)
		if err != nil || proxyURL != nil {
			t.Fatalf("Proxy(%s) = (%v, %v), want a direct connection", target, proxyURL, err)
		}
	}

	public, _ := http.NewRequest(http.MethodGet, "https://api.github.com/zen", nil)
	if proxyURL, err := transport.Proxy(public); err != nil || proxyURL == nil {
		t.Fatalf("Proxy(public) = (%v, %v), want the configured proxy", proxyURL, err)
	}
}

func TestNetproxy_TransportReportsBadProxy(t *testing.T) {
	if _, err := (Settings{Proxy: "ftp://proxy.example.com:21"}).Transport(); err == nil {
		t.Fatal("Transport() accepted an unsupported proxy scheme")
	}
}

func TestNetproxy_InstallDrivesDefaultTransportAndReloads(t *testing.T) {
	saved := http.DefaultTransport
	t.Cleanup(func() { http.DefaultTransport = saved })

	Install(Settings{Proxy: "http://proxy.example.com:3128"})
	if got := Current().Proxy; got != "http://proxy.example.com:3128" {
		t.Fatalf("Current().Proxy = %q", got)
	}

	public, _ := http.NewRequest(http.MethodGet, "https://api.github.com/zen", nil)
	proxyURL, err := http.DefaultTransport.(*http.Transport).Proxy(public)
	if err != nil || proxyURL == nil {
		t.Fatalf("default transport Proxy() = (%v, %v), want the configured proxy", proxyURL, err)
	}

	// A reload must take effect without a restart: Go caches the proxy
	// environment on first use, so only a live transport indirection can
	// change this.
	Install(Settings{Proxy: "direct"})
	proxyURL, err = http.DefaultTransport.(*http.Transport).Proxy(public)
	if err != nil || proxyURL != nil {
		t.Fatalf("after reload Proxy() = (%v, %v), want direct", proxyURL, err)
	}

	// And local traffic stays direct under any policy.
	local, _ := http.NewRequest(http.MethodGet, "http://127.0.0.1:8000/health", nil)
	Install(Settings{Proxy: "http://proxy.example.com:3128"})
	proxyURL, err = http.DefaultTransport.(*http.Transport).Proxy(local)
	if err != nil || proxyURL != nil {
		t.Fatalf("default transport proxied a loopback request: (%v, %v)", proxyURL, err)
	}

	// The HTTP version stays automatic — an upstream that only speaks HTTP/2
	// has to be reachable — and the policy is protected by the transport
	// itself instead: net/http serves pooled HTTPS requests over the HTTP/2
	// alternative protocol without consulting Transport.Proxy, and that pool
	// survives CloseIdleConnections (measured), so a policy change installs a
	// fresh transport rather than reusing the old one's connections.
	installed, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", http.DefaultTransport)
	}
	if !installed.ForceAttemptHTTP2 {
		t.Fatal("policy transport no longer negotiates HTTP/2: an HTTP/2-only upstream would be unreachable")
	}
	Install(Settings{Proxy: "direct"})
	replaced, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", http.DefaultTransport)
	}
	if replaced == installed {
		t.Fatal("a policy change kept the previous transport: its pooled connections would ignore the new policy")
	}

	Install(Settings{})
	if Current().Configured() {
		t.Fatalf("Current() still reports a configured proxy: %+v", Current())
	}
}

// TestNetproxy_PolicyChangeReplacesTheTransport guards the two halves of the
// outbound policy.
//
// The version is negotiated, not pinned: an upstream that only speaks HTTP/2 is
// reached over HTTP/2. And a policy change replaces the process transport,
// because net/http hands HTTPS requests to the HTTP/2 alternative protocol
// before it consults Transport.Proxy, pools that connection per host, and does
// not close it through CloseIdleConnections — measured here before the
// transport was rebuilt instead: a request after switching to a proxy rode the
// direct HTTP/2 connection the previous policy had opened.
//
// The end-to-end form (the second request failing against the proxy) cannot be
// exercised with a local upstream: isLocalHost bypasses every loopback and
// private address by design, so a configured proxy never applies to it.
func TestNetproxy_PolicyChangeReplacesTheTransport(t *testing.T) {
	protocols := make(chan string, 4)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case protocols <- r.Proto:
		default:
		}
		_, _ = w.Write([]byte("ok"))
	}))
	upstream.EnableHTTP2 = true
	upstream.StartTLS()
	defer upstream.Close()

	Install(Settings{Proxy: "direct"})
	t.Cleanup(func() { Install(Settings{Proxy: "direct"}) })

	direct, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", http.DefaultTransport)
	}
	// Only the trust anchor is added: replacing the config would drop the "h2"
	// entry the transport's HTTP/2 setup already put in it.
	if direct.TLSClientConfig == nil {
		direct.TLSClientConfig = &tls.Config{}
	}
	pool := x509.NewCertPool()
	pool.AddCert(upstream.Certificate())
	previousRoots := direct.TLSClientConfig.RootCAs
	t.Cleanup(func() { direct.TLSClientConfig.RootCAs = previousRoots })
	direct.TLSClientConfig.RootCAs = pool

	first, err := http.DefaultClient.Get(upstream.URL)
	if err != nil {
		t.Fatalf("direct request: %v", err)
	}
	first.Body.Close()
	if protocol := <-protocols; protocol != "HTTP/2.0" {
		t.Fatalf("first request protocol = %q, want HTTP/2.0", protocol)
	}

	Install(Settings{Proxy: "http://127.0.0.1:1"})
	after, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatalf("default transport is %T, want *http.Transport", http.DefaultTransport)
	}
	if after == direct {
		t.Fatal("the process transport survived a policy change: its pooled connections would serve requests under the old policy")
	}
	// The replacement consults the new policy for a host a proxy can serve.
	request, err := http.NewRequest(http.MethodGet, "https://example.com/", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := after.Proxy(request)
	if err != nil {
		t.Fatalf("proxy decision: %v", err)
	}
	if proxyURL == nil || proxyURL.Host != "127.0.0.1:1" {
		t.Fatalf("proxy decision = %v, want the newly configured proxy", proxyURL)
	}
}
