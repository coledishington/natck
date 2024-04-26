// Functions related to resolving URLs to IP addresses.
package main

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"strconv"
)

type resolvedUrl struct {
	url       *url.URL
	addresses []netip.AddrPort
}

func lookupAddr(network string, h *url.URL) *resolvedUrl {
	r := resolvedUrl{url: h}

	portString := urlPort(h)
	p64, err := strconv.ParseUint(portString, 10, 16)
	if err != nil {
		return &r
	}
	p := uint16(p64)

	resolver := net.DefaultResolver
	addrs, err := resolver.LookupNetIP(context.Background(), network, r.url.Hostname())
	if err != nil {
		return &r
	}

	for i := range addrs {
		addrport := netip.AddrPortFrom(addrs[i], p)
		r.addresses = append(r.addresses, addrport)
	}
	return &r
}
