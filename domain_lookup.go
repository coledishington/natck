// Functions related to resolving URLs to IP addresses.
package main

import (
	"net"
	"net/netip"
	"net/url"
	"sync"
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

func lookupAddrs(wg *sync.WaitGroup, hostnames <-chan *url.URL, resolvedAddr chan<- *resolvedUrl, cancel <-chan struct{}) {
	defer wg.Done()

	for {
		var h *url.URL
		var ok bool

		// Quit without possibility of selecting other cases
		select {
		case <-cancel:
			return
		default:
		}

		// Stop blocking if canceled
		select {
		case h, ok = <-hostnames:
		case <-cancel:
			return
		}
		if !ok {
			break
		}

		select {
		case resolvedAddr <- lookupAddr(h):
		case <-cancel:
			return
		}
	}
}
