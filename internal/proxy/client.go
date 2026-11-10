package proxy

import (
	"errors"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strings"
)

func ParseTrustedProxies(value string) ([]netip.Prefix, error) {
	if value == "" {
		return nil, nil
	}
	var prefixes []netip.Prefix
	for _, item := range strings.Split(value, ",") {
		prefix, err := netip.ParsePrefix(strings.TrimSpace(item))
		if err != nil || prefix.Addr().Is4In6() || prefix.Bits() == 0 {
			return nil, fmt.Errorf("invalid trusted proxy CIDR %q", item)
		}
		prefixes = append(prefixes, prefix.Masked())
	}
	return prefixes, nil
}

func clientAddress(r *http.Request, trusted []netip.Prefix) (netip.Addr, error) {
	peer, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return netip.Addr{}, err
	}
	address := peer.Addr().WithZone("").Unmap()
	isTrusted := func() bool {
		return slices.ContainsFunc(trusted, func(prefix netip.Prefix) bool { return prefix.Contains(address) })
	}
	if !isTrusted() {
		return address, nil
	}
	forwarded := r.Header.Values("X-Forwarded-For")
	for i := len(forwarded) - 1; i >= 0; i-- {
		hops := strings.Split(forwarded[i], ",")
		for j := len(hops) - 1; j >= 0; j-- {
			hop, err := netip.ParseAddr(strings.TrimSpace(hops[j]))
			if err != nil || hop.Zone() != "" {
				return netip.Addr{}, errors.New("malformed forwarded address")
			}
			address = hop.Unmap()
			if !isTrusted() {
				return address, nil
			}
		}
	}
	return netip.Addr{}, errors.New("trusted proxy did not supply a client address")
}
