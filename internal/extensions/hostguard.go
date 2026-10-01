package extensions

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
)

// checkPublicHost refuses URLs that point at the daemon's own machine, at the
// loopback interface, or into a private or reserved network. An extension's
// networkHosts allowlist names which external service it may call; it is not a
// licence to reach anything that name happens to resolve to.
//
// The host is resolved and every address it resolves to is validated, then the
// same check runs on every redirect hop. A name that resolves to several
// addresses, one of which is private, is refused rather than resolved again
// later, because the address a client would dial is not necessarily the one
// that was validated.
func checkPublicHost(host string) error {
	if literal, err := netip.ParseAddr(host); err == nil {
		return publicAddressError(literal)
	}
	addresses, lookupErr := net.DefaultResolver.LookupIPAddr(context.Background(), host)
	if lookupErr != nil || len(addresses) == 0 {
		return errors.New("could not resolve network host")
	}
	for _, address := range addresses {
		mapped, ok := netip.AddrFromSlice(address.IP)
		if !ok {
			return errors.New("could not resolve network host")
		}
		if err := publicAddressError(mapped); err != nil {
			return err
		}
	}
	return nil
}

func publicAddressError(addr netip.Addr) error {
	addr = addr.Unmap()
	if addr.Zone() != "" {
		return fmt.Errorf("host %s is not a public address", addr)
	}
	if !addr.IsGlobalUnicast() || addr.IsPrivate() || addr.IsLoopback() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsInterfaceLocalMulticast() || addr.IsMulticast() ||
		addr.IsUnspecified() || reservedRange(addr) {
		return fmt.Errorf("host %s is not a public address", addr)
	}
	return nil
}

// reservedRanges covers the blocks that are not globally routable but that
// netip does not call out on its own: carrier-grade NAT, the documentation and
// benchmarking ranges, and the IPv4 broadcast address.
var reservedRanges = mustPrefixes(
	"0.0.0.0/8", "100.64.0.0/10", "192.0.0.0/24", "192.0.2.0/24",
	"192.88.99.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24",
	"240.0.0.0/4", "255.255.255.255/32",
	"64:ff9b:1::/48", "2001:db8::/32", "ff00::/8",
)

func mustPrefixes(ranges ...string) []netip.Prefix {
	prefixes := make([]netip.Prefix, 0, len(ranges))
	for _, value := range ranges {
		prefixes = append(prefixes, netip.MustParsePrefix(value).Masked())
	}
	return prefixes
}

func reservedRange(addr netip.Addr) bool {
	probe := addr
	if !probe.Is4() {
		probe = probe.Unmap()
	}
	for _, prefix := range reservedRanges {
		if prefix.Contains(probe) {
			return true
		}
	}
	return false
}
