package server

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mostlygeek/llama-swap/internal/config"
)

func resolveWith(t *testing.T, network config.NetworkConfig, remoteAddr string, headers map[string]string) string {
	t.Helper()
	resolver := newClientIPResolver(network)
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = remoteAddr
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	return resolver.resolve(request)
}

func TestClientIPResolver_DefaultIgnoresForwardingHeaders(t *testing.T) {
	forwarding := map[string]string{
		"X-Forwarded-For": "203.0.113.9, 10.0.0.1",
		"X-Real-IP":       "203.0.113.10",
	}
	if got := resolveWith(t, config.NetworkConfig{}, "198.51.100.4:5000", forwarding); got != "198.51.100.4" {
		t.Fatalf("default resolve = %q, want the connection peer", got)
	}
}

func TestClientIPResolver_TrustForwardedForWithoutProxyList(t *testing.T) {
	network := config.NetworkConfig{TrustForwardedFor: true}

	// Reverse proxies append by default (nginx $proxy_add_x_forwarded_for,
	// Caddy), so the right-most hop is the address the proxy saw. The left-most
	// entry is whatever the client chose to send, and returning it would let a
	// client defeat the allowlist with the switch on.
	got := resolveWith(t, network, "10.0.0.1:5000", map[string]string{"X-Forwarded-For": "198.51.100.7, 203.0.113.9"})
	if got != "203.0.113.9" {
		t.Fatalf("resolve = %q, want the right-most (proxy-appended) hop", got)
	}

	// A single hop is unambiguous.
	single := resolveWith(t, network, "10.0.0.1:5000", map[string]string{"X-Forwarded-For": "203.0.113.9"})
	if single != "203.0.113.9" {
		t.Fatalf("single hop resolve = %q", single)
	}

	realIP := resolveWith(t, network, "10.0.0.1:5000", map[string]string{"X-Real-IP": "203.0.113.11"})
	if realIP != "203.0.113.11" {
		t.Fatalf("X-Real-IP resolve = %q", realIP)
	}

	// An unparseable token must not become the caller.
	unknown := resolveWith(t, network, "10.0.0.1:5000", map[string]string{"X-Forwarded-For": "unknown"})
	if unknown != "10.0.0.1" {
		t.Fatalf("unknown token resolve = %q, want the peer", unknown)
	}
}

// TestClientIPResolver_ClientSuppliedHopIsNotTrusted is the regression guard
// for the original allowlist bypass: with only the switch enabled, a client
// that pre-seeds X-Forwarded-For must not be able to choose its own address.
// The proxy appends what it observed to the right of the forged entry.
func TestClientIPResolver_ClientSuppliedHopIsNotTrusted(t *testing.T) {
	network := config.NetworkConfig{TrustForwardedFor: true}
	got := resolveWith(t, network, "10.0.0.1:5000", map[string]string{
		// The client sent 198.51.100.7; the proxy appended the real peer.
		"X-Forwarded-For": "198.51.100.7, 203.0.113.9",
	})
	if got == "198.51.100.7" {
		t.Fatal("client-supplied hop was trusted; the allowlist is spoofable with only the switch on")
	}
	if got != "203.0.113.9" {
		t.Fatalf("resolve = %q, want the proxy-appended address", got)
	}
}

func TestClientIPResolver_TrustedProxiesWalkRightToLeft(t *testing.T) {
	network := config.NetworkConfig{
		TrustForwardedFor: true,
		TrustedProxies:    []string{"10.0.0.0/8", "192.168.5.5"},
	}

	// Right-most trusted hops are stripped; the first untrusted hop wins.
	got := resolveWith(t, network, "10.0.0.1:5000", map[string]string{
		"X-Forwarded-For": "203.0.113.9, 192.168.5.5, 10.1.1.1",
	})
	if got != "203.0.113.9" {
		t.Fatalf("resolve = %q, want 203.0.113.9", got)
	}

	// A connection that is not from a trusted proxy cannot influence the
	// result, even with a fully trusted header chain.
	spoofed := resolveWith(t, network, "198.51.100.4:5000", map[string]string{
		"X-Forwarded-For": "203.0.113.9",
	})
	if spoofed != "198.51.100.4" {
		t.Fatalf("untrusted peer resolve = %q, want the peer", spoofed)
	}

	// Every hop trusted: fall back to the right-most entry, the closest
	// observable address, rather than the left-most one the client may have
	// supplied.
	allTrusted := resolveWith(t, network, "10.0.0.1:5000", map[string]string{
		"X-Forwarded-For": "10.2.2.2, 10.3.3.3",
	})
	if allTrusted != "10.3.3.3" {
		t.Fatalf("all-trusted resolve = %q, want the right-most hop", allTrusted)
	}
}

func TestClientIPResolver_IPv4MappedAndPortForms(t *testing.T) {
	network := config.NetworkConfig{
		TrustForwardedFor: true,
		TrustedProxies:    []string{"192.168.1.10"},
	}
	got := resolveWith(t, network, "[::ffff:192.168.1.10]:5000", map[string]string{
		"X-Forwarded-For": "192.168.1.1:1234",
	})
	if got != "192.168.1.1" {
		t.Fatalf("resolve = %q, want the port-less forwarded address", got)
	}
}

func TestClientIPMiddleware_StoresResolvedAddressForDownstream(t *testing.T) {
	network := config.NetworkConfig{TrustForwardedFor: true, TrustedProxies: []string{"10.0.0.0/8"}}
	var observed string
	handler := CreateClientIPMiddleware(network)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		observed = clientIP(r)
	}))
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "10.0.0.1:5000"
	request.Header.Set("X-Forwarded-For", "203.0.113.9")
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if observed != "203.0.113.9" {
		t.Fatalf("downstream clientIP = %q, want the resolved caller", observed)
	}
}

// TestClientIPResolver_UnparseableAppendedHopFallsBackToPeer guards the narrow
// bypass the right-most rule alone does not close: a proxy that writes a
// non-address token for the peer must not let a client-supplied entry on the
// left become the answer.
func TestClientIPResolver_UnparseableAppendedHopFallsBackToPeer(t *testing.T) {
	network := config.NetworkConfig{TrustForwardedFor: true}
	got := resolveWith(t, network, "10.0.0.1:5000", map[string]string{
		"X-Forwarded-For": "198.51.100.7, unknown",
	})
	if got != "10.0.0.1" {
		t.Fatalf("resolve = %q, want the connection peer when the appended hop is unparseable", got)
	}
}

// TestClientIPResolver_TrustedProxyAppendsUnparseable covers the trusted-proxy
// mode of the same bypass: when the directly connected proxy writes a
// non-address token, no entry on the left may become the answer.
func TestClientIPResolver_TrustedProxyAppendsUnparseable(t *testing.T) {
	network := config.NetworkConfig{
		TrustForwardedFor: true,
		TrustedProxies:    []string{"10.0.0.0/8"},
	}
	cases := []string{
		"198.51.100.7, unknown",
		"198.51.100.7, 10.1.1.1, unknown",
		"198.51.100.7, ",
	}
	for _, header := range cases {
		got := resolveWith(t, network, "10.0.0.1:5000", map[string]string{"X-Forwarded-For": header})
		if got != "10.0.0.1" {
			t.Errorf("XFF %q resolved to %q, want the connection peer", header, got)
		}
	}
}

// TestClientIPResolver_AllTrustedWithUnparseableTail covers the fallback used
// when every parsed hop is a trusted proxy: an unparseable appended segment
// still means the header cannot be trusted.
func TestClientIPResolver_AllTrustedWithUnparseableTail(t *testing.T) {
	network := config.NetworkConfig{
		TrustForwardedFor: true,
		TrustedProxies:    []string{"10.0.0.0/8"},
	}
	got := resolveWith(t, network, "10.0.0.1:5000", map[string]string{
		"X-Forwarded-For": "10.2.2.2, 10.3.3.3, unknown",
	})
	if got != "10.0.0.1" {
		t.Fatalf("resolve = %q, want the connection peer", got)
	}
}
