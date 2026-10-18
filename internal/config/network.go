package config

import (
	"fmt"
	"net"
	"strings"

	"github.com/mostlygeek/llama-swap/internal/netproxy"
)

// NetworkConfig is the daemon's outbound network policy: the proxy every
// host-side client uses (extension fetches, model downloads, pricing and
// runtime metadata, provider fallbacks) and the hosts that bypass it.
//
// It is deliberately not a per-model or per-provider setting: an operator who
// needs a proxy needs it for everything that leaves the host, and the
// per-credential `proxy` on a provider stays available for the rare case where
// one upstream must be reached directly.
//
// The section also decides which address is treated as the caller. By default
// that is always the TCP peer, because forwarding headers are attacker
// controlled on a direct connection and they feed the per-key IP allowlist. A
// deployment behind a reverse proxy opts in explicitly.
type NetworkConfig struct {
	// Proxy is empty (follow HTTP_PROXY/HTTPS_PROXY/ALL_PROXY from the
	// environment), "direct"/"none", or an http(s)/socks5(h) URL.
	Proxy string `yaml:"proxy" json:"proxy"`
	// NoProxy lists hosts (or domains, or a leading-dot suffix) that skip the
	// proxy. "*" disables proxying entirely.
	NoProxy []string `yaml:"noProxy" json:"noProxy"`
	// TrustForwardedFor makes the caller address come from
	// X-Forwarded-For / X-Real-IP instead of the TCP peer. Enable it only
	// when every request reaches llama-swap through a reverse proxy you
	// control; a direct client could otherwise claim any address and defeat
	// the API-key IP allowlist. Default false.
	TrustForwardedFor bool `yaml:"trustForwardedFor" json:"trustForwardedFor"`
	// TrustedProxies restricts the forwarding headers to the listed peer
	// addresses or CIDR blocks (for example 127.0.0.1/32 or 10.0.0.0/8).
	// Empty means "any peer", which is only reasonable when the listener is
	// not reachable except through the proxy.
	TrustedProxies []string `yaml:"trustedProxies" json:"trustedProxies"`
}

// Settings converts the configuration into the transport-building form.
func (c NetworkConfig) Settings() netproxy.Settings {
	return netproxy.Settings{Proxy: c.Proxy, NoProxy: c.NoProxy}
}

// Configured reports whether the operator pinned a proxy.
func (c NetworkConfig) Configured() bool {
	return strings.TrimSpace(c.Proxy) != "" || len(c.NoProxy) > 0
}

// Effective trims the forwarding-header peer list so YAML with blank entries
// behaves like an absent list. An absent list stays nil so a config built in
// code compares equal to one loaded from YAML.
func (c NetworkConfig) Effective() NetworkConfig {
	if len(c.TrustedProxies) == 0 {
		c.TrustedProxies = nil
		return c
	}
	trimmed := make([]string, 0, len(c.TrustedProxies))
	seen := make(map[string]struct{}, len(c.TrustedProxies))
	for _, entry := range c.TrustedProxies {
		value := strings.TrimSpace(entry)
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		trimmed = append(trimmed, value)
	}
	if len(trimmed) == 0 {
		trimmed = nil
	}
	c.TrustedProxies = trimmed
	return c
}

// Redacted returns a log-safe description of the policy.
func (c NetworkConfig) Redacted() string {
	switch {
	case !c.Configured():
		return "environment"
	case netproxy.Settings{Proxy: c.Proxy}.Direct():
		return "direct"
	default:
		return netproxy.Redact(strings.TrimSpace(c.Proxy))
	}
}

// Validate checks the proxy URL, the bypass list and the trusted-proxy
// entries. Trusted proxies are only meaningful together with
// trustForwardedFor: accepting the list without the switch would silently do
// nothing and leave an operator believing the allowlist is proxy-aware.
func (c NetworkConfig) Validate() error {
	if err := netproxy.Validate(c.Proxy); err != nil {
		return fmt.Errorf("network.proxy: %w", err)
	}
	for index, entry := range c.NoProxy {
		trimmed := strings.TrimSpace(entry)
		if trimmed == "" {
			return fmt.Errorf("network.noProxy entry %d is empty", index)
		}
		if strings.ContainsAny(trimmed, " \t/") {
			return fmt.Errorf("network.noProxy entry %q must be a host or domain, not a URL", trimmed)
		}
	}
	if len(c.TrustedProxies) > 0 && !c.TrustForwardedFor {
		return fmt.Errorf("network.trustedProxies requires network.trustForwardedFor: true")
	}
	for index, entry := range c.TrustedProxies {
		value := strings.TrimSpace(entry)
		if value == "" {
			return fmt.Errorf("network.trustedProxies entry %d is empty", index)
		}
		if strings.Contains(value, "/") {
			if _, _, err := net.ParseCIDR(value); err != nil {
				return fmt.Errorf("network.trustedProxies entry %q is not a valid address or CIDR block", value)
			}
			continue
		}
		if net.ParseIP(value) == nil {
			return fmt.Errorf("network.trustedProxies entry %q is not a valid address or CIDR block", value)
		}
	}
	return nil
}
