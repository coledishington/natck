// Functions related to resolving URLs to IP addresses.
package main

import (
	"net"
	"net/netip"
	"net/url"
)

type resolvedUrl struct {
	url       *url.URL
	addresses []netip.Addr
}

func lookupAddr(h *url.URL) *resolvedUrl {
	r := resolvedUrl{url: h}
	addrs, err := net.LookupIP(r.url.Hostname())
	if err != nil {
		return &r
	}

	for i := range addrs {
		addr, ok := netip.AddrFromSlice(addrs[i])
		if !ok {
			continue
		}
		r.addresses = append(r.addresses, addr)
	}
	return &r
}
