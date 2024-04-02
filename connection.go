// Functions related to managing connections to hosts. Each connection must kept to one server
// connection to accurately measure the maximum allowed connections.
package main

import (
	"context"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"time"
)

type ctxAddrKey struct{}

type connection struct {
	id            uint
	host          *host
	client        *http.Client
	url           *url.URL
	uncrawledUrls []*url.URL
	crawledUrls   []*url.URL
	lastRequest   time.Time
	lastReply     time.Time
}

// Rotates lookups from each connection response to avoid
// spending ages resolving one connection whilst others
// are already.
type lookupQueue struct {
	urls [][]*url.URL
}

func (q lookupQueue) peek() *url.URL {
	if len(q.urls) == 0 {
		return nil
	}
	return q.urls[0][0]
}

func (q *lookupQueue) put(u ...*url.URL) {
	q.urls = append(q.urls, u)
}

func (q *lookupQueue) pop() *url.URL {
	u := q.urls[0][0]

	if len(q.urls[0]) == 1 {
		q.urls = q.urls[1:]
		return u
	}

	q.urls = append(q.urls[1:], q.urls[0][1:])
	return u
}

func urlPort(u *url.URL) string {
	schemeToPort := map[string]string{
		"http":  "80",
		"https": "443",
	}
	port := u.Port()
	if port != "" {
		return port
	}
	return schemeToPort[u.Scheme]
}

func canonicalHost(u *url.URL) string {
	return net.JoinHostPort(u.Hostname(), urlPort(u))
}

func indexUrls(urls []*url.URL, needle *url.URL) int {
	return slices.IndexFunc(urls, func(u *url.URL) bool {
		return *u == *needle
	})
}

func indexKeepAliveConnection(conns []*connection) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return time.Since(c.lastReply) > 3500*time.Millisecond && time.Since(c.lastRequest) > 1000*time.Millisecond
	})
}

func indexUrlByHostPort(urls []*url.URL, needle *url.URL) int {
	hostPort := canonicalHost(needle)
	return slices.IndexFunc(urls, func(u *url.URL) bool {
		return canonicalHost(u) == hostPort
	})
}

func indexConnectionById(conns []*connection, needle uint) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return c.id == needle
	})
}

func indexConnectionByAddr(conns []*connection, addr netip.AddrPort) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return c.host.ip == addr
	})
}

func indexConnectionByHostPort(conns []*connection, u *url.URL) int {
	hostPort := canonicalHost(u)
	return slices.IndexFunc(conns, func(c *connection) bool {
		return c.host.hostPort == hostPort
	})
}

func makeClient() *http.Client {
	// Need a unique transport per http.Client to avoid re-using the same
	// connections, otherwise the NAT count will be wrong.
	// The transport should only have one connection that never times out.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.IdleConnTimeout = 0
	transport.MaxIdleConns = 1
	transport.MaxConnsPerHost = 1
	transport.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
		// Http clients should not resolve the address. Overriding the dial avoids having to
		// override URL and TLS ServerName.
		addrShouldUse := ctx.Value(ctxAddrKey{}).(netip.AddrPort)
		return http.DefaultTransport.(*http.Transport).DialContext(ctx, network, addrShouldUse.String())
	}

	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Do not follow re-directs
			return http.ErrUseLastResponse
		},
		Transport: transport,
	}
	return &client
}

func makeConnection(addr netip.AddrPort, target *url.URL) *connection {
	c := &connection{
		client:        makeClient(),
		url:           target,
		uncrawledUrls: []*url.URL{target},
		host: &host{
			ip:       addr,
			hostPort: canonicalHost(target),
		},
	}
	return c
}

func getNextConnection(pendingConns, activeConns []*connection) *connection {
	if i := indexKeepAliveConnection(activeConns); i != -1 {
		// Prioritise existing connections to avoid http keep-alive
		// or NAT mapping expires
		return activeConns[i]
	}
	if len(pendingConns) > 0 {
		return pendingConns[0]
	}
	return nil
}

func lookupAddrRequest(h *url.URL, resolvedAddr chan<- *resolvedUrl, cancel <-chan struct{}) {
	select {
	case resolvedAddr <- lookupAddr(h):
	case <-cancel:
	}
}

func scrapConnectionRequest(r *roundtrip, scraped chan<- *roundtrip, cancel <-chan struct{}) {
	ctx := context.WithValue(context.Background(), ctxAddrKey{}, r.host.ip)
	select {
	case scraped <- scrapConnection(ctx, r):
	case <-cancel:
	}
}

func MeasureMaxConnections(urls []*url.URL) int {
	lookupAddrReply := make(chan *resolvedUrl)
	scrapedReply := make(chan *roundtrip)
	stopC := make(chan struct{})

	workerLimit := len(urls)
	semC := make(chan struct{}, workerLimit)

	connectionIdCtr := uint(0)
	pendingConns := []*connection{}
	pendingResolutions := lookupQueue{}

	uniqueUrls := []*url.URL{}
	for _, u := range urls {
		if i := indexUrlByHostPort(uniqueUrls, u); i != -1 {
			continue
		}
		uniqueUrls = append(uniqueUrls, u)
	}
	for _, u := range uniqueUrls {
		pendingResolutions.put(u)
	}

	activeConns := make([]*connection, 0)
	for {
		var lookupAddrSemC chan<- struct{} = nil
		var scrapRequestSemC chan<- struct{} = nil

		if pendingResolutions.peek() != nil {
			lookupAddrSemC = semC
		}

		var request *roundtrip = nil
		c := getNextConnection(pendingConns, activeConns)
		if c != nil {
			target := c.url
			if len(c.uncrawledUrls) > 0 {
				target = c.uncrawledUrls[0]
			}
			request = &roundtrip{connId: c.id, client: c.client, url: target, host: c.host}
			scrapRequestSemC = semC
		}

		select {
		case <-time.After(50 * time.Millisecond):
		case lookupAddrSemC <- struct{}{}:
			hUrl := pendingResolutions.pop()
			go func() {
				lookupAddrRequest(hUrl, lookupAddrReply, stopC)
				<-semC
			}()
		case h, ok := <-lookupAddrReply:
			if !ok {
				return -1
			}

			unusedAddresses := []netip.AddrPort{}
			for _, address := range h.addresses {
				if indexConnectionByAddr(activeConns, address) != -1 {
					break
				}
				unusedAddresses = append(unusedAddresses, address)
			}
			if len(unusedAddresses) == 0 {
				break
			}

			c := makeConnection(unusedAddresses[0], h.url)
			c.id = connectionIdCtr
			pendingConns = append(pendingConns, c)
			connectionIdCtr++
		case scrapRequestSemC <- struct{}{}:
			go func() {
				scrapConnectionRequest(request, scrapedReply, stopC)
				<-semC
			}()

			if len(pendingConns) > 0 && pendingConns[0] == c {
				pendingConns = pendingConns[1:]
			}

			c.lastRequest = time.Now()
		case reply, ok := <-scrapedReply:
			if !ok {
				return -1
			}

			newUrls := []*url.URL{}
			for _, u := range reply.scrapedUrls {
				if i := indexConnectionByHostPort(activeConns, u); i != -1 {
					c := activeConns[i]
					if !sliceContainsUrl(c.uncrawledUrls, u) || !sliceContainsUrl(c.crawledUrls, u) {
						c.uncrawledUrls = append(c.uncrawledUrls, u)
					}
					continue
				}

				newUrls = append(newUrls, u)
			}
			urlsToResolve := []*url.URL{}
			for _, u := range newUrls {
				if i := indexUrlByHostPort(urlsToResolve, u); i != -1 {
					continue
				}
				if i := indexConnectionByHostPort(pendingConns, u); i != -1 {
					continue
				}
				urlsToResolve = append(urlsToResolve, u)
			}
			if len(urlsToResolve) > 0 {
				pendingResolutions.put(urlsToResolve...)
			}

			var c *connection
			if i := indexConnectionById(activeConns, reply.connId); i != -1 {
				c = activeConns[i]

				// Adjust uncrawled urls
				if j := indexUrls(c.uncrawledUrls, reply.url); j != -1 {
					c.uncrawledUrls = slices.Delete(c.uncrawledUrls, j, j+1)
				}

				if reply.failed {
					activeConns = slices.Delete(activeConns, i, i+1)
				}
			} else {
				c = &connection{
					id: reply.connId, client: reply.client, url: reply.url,
					uncrawledUrls: []*url.URL{},
					crawledUrls:   []*url.URL{reply.url},
					host:          reply.host,
				}

				if !reply.failed {
					activeConns = append(activeConns, c)
				}
			}
			c.lastRequest = reply.requestTs
			c.lastReply = reply.replyTs

			if !sliceContainsUrl(c.crawledUrls, reply.url) {
				c.crawledUrls = append(c.crawledUrls, reply.url)
			}
		}

		if len(pendingConns) == 0 && len(pendingResolutions.urls) == 0 && len(semC) == 0 {
			haveUncrawledUrls := false
			for _, c := range activeConns {
				haveUncrawledUrls = len(c.uncrawledUrls) > 0
				if haveUncrawledUrls {
					break
				}
			}
			if !haveUncrawledUrls {
				break
			}
		}
	}

	close(stopC)
	for i := workerLimit; i > 0; i-- {
		semC <- struct{}{}
	}
	close(semC)
	close(lookupAddrReply)
	close(scrapedReply)
	return len(activeConns)
}
