// Functions related to resolving URLs to IP addresses.
package main

import (
	"net"
	"net/netip"
	"net/url"
	"strconv"
)

type resolvedUrl struct {
	url       *url.URL
	addresses []netip.AddrPort
}

func lookupAddr(h *url.URL) *resolvedUrl {
	r := resolvedUrl{url: h}

	portString := urlPort(h)
	p64, err := strconv.ParseUint(portString, 10, 16)
	if err != nil {
		return &r
	}
	p := uint16(p64)

	addrs, err := net.LookupIP(r.url.Hostname())
	if err != nil {
		return &r
	}

	for i := range addrs {
		addr, ok := netip.AddrFromSlice(addrs[i])
		if !ok {
			continue
		}
		addrport := netip.AddrPortFrom(addr, p)
		r.addresses = append(r.addresses, addrport)
	}
	return &r
}
