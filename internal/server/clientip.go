package server

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/chain"
	"github.com/mostlygeek/llama-swap/internal/config"
)

// clientIPContextKey carries the caller address resolved once per request.
// Every downstream decision (API-key IP allowlist, audit rows, access log,
// gateway session affinity) reads the same value, so a request can never be
// judged against one address and logged under another.
type clientIPContextKey struct{}

// clientIPResolver decides which address is the caller's.
//
// The default is the TCP peer. Forwarding headers are only consulted when the
// operator enabled them, because a direct client can send any value and the
// resolved address feeds the per-key IP allowlist. When a trusted-proxy list
// is configured the resolver walks X-Forwarded-For right to left, skipping
// hops that belong to trusted proxies; the first untrusted hop is the client.
type clientIPResolver struct {
	trustForwardedFor bool
	trustAnyPeer      bool
	trusted           []netip.Prefix
}

func newClientIPResolver(network config.NetworkConfig) *clientIPResolver {
	resolver := &clientIPResolver{trustForwardedFor: network.TrustForwardedFor}
	for _, entry := range network.Effective().TrustedProxies {
		if prefix, err := netip.ParsePrefix(entry); err == nil {
			resolver.trusted = append(resolver.trusted, prefix.Masked())
			continue
		}
		if addr, err := netip.ParseAddr(entry); err == nil {
			addr = addr.Unmap()
			resolver.trusted = append(resolver.trusted, netip.PrefixFrom(addr, addr.BitLen()))
		}
	}
	resolver.trustAnyPeer = len(resolver.trusted) == 0
	return resolver
}

// resolve returns the caller address for one request as a bare IP string. An
// unparseable peer address is returned verbatim so logs keep whatever the
// transport reported.
func (r *clientIPResolver) resolve(req *http.Request) string {
	remoteAddr, remoteOK := parseClientAddr(req.RemoteAddr)
	remote := strings.TrimSpace(req.RemoteAddr)
	if remoteOK {
		remote = remoteAddr.String()
	}
	if r == nil || !r.trustForwardedFor {
		return remote
	}
	if !r.trustAnyPeer && (!remoteOK || !r.isTrusted(remoteAddr)) {
		// The connection did not come from a configured proxy: its headers
		// carry no authority.
		return remote
	}

	// Parse the header into hops, but keep track of whether the final
	// (proxy-appended) segment was a real address. A proxy that writes a
	// non-address token such as "unknown" for the peer must not cause the
	// resolver to fall back to an entry further left — every entry to the left
	// of the appended one is client-supplied.
	hops, lastAppendedValid := forwardedHops(req.Header.Values("X-Forwarded-For"))
	if len(hops) == 0 {
		if addr, ok := parseClientAddr(req.Header.Get("X-Real-IP")); ok {
			return addr.String()
		}
		return remote
	}
	if !lastAppendedValid {
		// Applies to both modes: with a proxy list and without it, the only
		// trustworthy entry is the one the directly connected proxy appended.
		return remote
	}
	if r.trustAnyPeer {
		// No proxy list was configured, so only the hop the proxy itself
		// appended can be trusted. Reverse proxies append by default (nginx's
		// $proxy_add_x_forwarded_for, Caddy), so the right-most entry is the
		// address of the connection the proxy saw. The left-most entry is
		// whatever the client chose to send, and returning it would let a
		// direct client defeat the per-key allowlist even with the switch on.
		return hops[len(hops)-1].String()
	}
	for index := len(hops) - 1; index >= 0; index-- {
		if !r.isTrusted(hops[index]) {
			return hops[index].String()
		}
	}
	// Every hop is a trusted proxy; the right-most entry is the closest
	// observable address and, unlike the left-most one, cannot have been
	// supplied by the client.
	return hops[len(hops)-1].String()
}

func (r *clientIPResolver) isTrusted(addr netip.Addr) bool {
	if !addr.IsValid() {
		return false
	}
	addr = addr.Unmap()
	for _, prefix := range r.trusted {
		if prefix.Contains(addr) {
			return true
		}
	}
	return false
}

// forwardedHops parses the comma-separated X-Forwarded-For values into
// addresses. Non-address entries (the "unknown" token some proxies emit) are
// dropped rather than trusted.
//
// The second result reports whether the final segment — the one the directly
// connected proxy appended — was a parseable address. Callers that trust only
// that segment must fall back to the connection address when it is not,
// instead of using an entry further left that the client may have supplied.
func forwardedHops(values []string) ([]netip.Addr, bool) {
	hops := make([]netip.Addr, 0, len(values))
	if len(values) == 0 {
		return hops, false
	}
	// Flatten every comma-separated segment across all header values so the
	// trailing one can be validated on its own. An explicit trailing empty
	// segment counts as "the proxy appended nothing usable", not as "keep
	// looking to the left".
	segments := make([]string, 0, len(values))
	for _, value := range values {
		segments = append(segments, strings.Split(value, ",")...)
	}
	if len(segments) == 0 {
		return hops, false
	}
	for _, segment := range segments {
		if addr, ok := parseClientAddr(segment); ok {
			hops = append(hops, addr)
		}
	}
	_, lastAppendedValid := parseClientAddr(segments[len(segments)-1])
	return hops, lastAppendedValid
}

// parseClientAddr accepts "1.2.3.4", "1.2.3.4:5678", "[::1]:5678" and zoned
// IPv6 literals. IPv4-mapped IPv6 addresses are unmapped so an entry written
// as 192.168.1.1 matches a peer reported as ::ffff:192.168.1.1.
func parseClientAddr(value string) (netip.Addr, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return netip.Addr{}, false
	}
	if host, _, err := net.SplitHostPort(value); err == nil {
		value = host
	}
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.Unmap(), true
	}
	return netip.Addr{}, false
}

// CreateClientIPMiddleware resolves the caller address once and stores it in
// the request context. It must run before authentication, logging and the
// inflight tracker so all of them observe the same value.
func CreateClientIPMiddleware(network config.NetworkConfig) chain.Middleware {
	resolver := newClientIPResolver(network)
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := resolver.resolve(r)
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), clientIPContextKey{}, ip)))
		})
	}
}
