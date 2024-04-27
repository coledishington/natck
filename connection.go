// Functions related to managing connections to hosts. Each connection must kept to one server
// connection to accurately measure the maximum allowed connections.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"time"
)

const reRequestInterval = 3500 * time.Millisecond

type ctxAddrKey struct{}

type relativeUrl struct {
	// Url fragment is stripped before sending to servers.
	path, query string
}

type connection struct {
	id            uint
	host          *host
	url           *url.URL
	client        *http.Client
	uncrawledUrls map[relativeUrl]bool
	crawlingUrls  map[relativeUrl]bool
	crawledUrls   map[relativeUrl]bool
	crawlDelay    time.Duration
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

func urlToRelativeUrl(u *url.URL) relativeUrl {
	p := u.RawPath
	if p == "" {
		p = u.Path
	}
	return relativeUrl{
		path:  p,
		query: u.RawQuery,
	}
}

func resolveRelativeUrl(b *url.URL, r relativeUrl) (*url.URL, error) {
	path := r.path
	if r.query != "" {
		path = fmt.Sprintf("%v?%v", path, r.query)
	}
	rUrl, err := url.Parse(path)
	if err != nil {
		return nil, err
	}
	return b.ResolveReference(rUrl), nil
}

func isDialError(err error) bool {
	var needle *net.OpError

	if !errors.As(err, &needle) {
		return false
	}
	return needle.Op == "dial"
}

func canonicalHost(u *url.URL) string {
	return net.JoinHostPort(u.Hostname(), urlPort(u))
}

func indexKeepAliveConnection(conns []*connection) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return time.Since(c.lastReply) > reRequestInterval && time.Since(c.lastRequest) > c.crawlDelay
	})
}

func maxLastReplyConnection(conns []*connection) *connection {
	return slices.MaxFunc(conns, func(c1, c2 *connection) int {
		return c1.lastReply.Compare(c2.lastReply)
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

func deleteDuplicateUrlsByHostPort(urls []*url.URL) []*url.URL {
	uniqueUrls := []*url.URL{}
	for _, u := range urls {
		if i := indexUrlByHostPort(uniqueUrls, u); i != -1 {
			continue
		}
		uniqueUrls = append(uniqueUrls, u)
	}
	return uniqueUrls
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
		client: makeClient(),
		url:    target,
		uncrawledUrls: map[relativeUrl]bool{
			{path: "/robots.txt"}:    true,
			urlToRelativeUrl(target): true,
		},
		crawlingUrls: map[relativeUrl]bool{},
		crawledUrls:  map[relativeUrl]bool{},
		host: &host{
			ip:       addr,
			hostPort: canonicalHost(target),
		},
		crawlDelay: reRequestInterval,
	}
	return c
}

func getNextConnection(pendingConns, activeConns []*connection, freeWorkers int) *connection {
	if i := indexKeepAliveConnection(activeConns); i != -1 {
		// Prioritise existing connections to avoid http keep-alive
		// or NAT mapping expires
		return activeConns[i]
	}
	if len(pendingConns) > 0 {
		return pendingConns[0]
	}
	if freeWorkers > 0 && len(activeConns) > 0 {
		// If there are free workers and uncrawled urls, increase crawl
		// frequency to try to find new hosts
		availableToCrawl := []*connection{}
		for _, c := range activeConns {
			if len(c.uncrawledUrls) == 0 || time.Since(c.lastRequest) < c.crawlDelay {
				continue
			}
			availableToCrawl = append(availableToCrawl, c)
		}
		if len(availableToCrawl) > 0 {
			if c := maxLastReplyConnection(availableToCrawl); c != nil {
				return c
			}
		}
	}
	return nil
}

func getNextUrlToCrawl(c *connection) *url.URL {
	target := c.url

	// Always visit robots.txt first
	robots := relativeUrl{path: "/robots.txt"}
	if _, found := c.uncrawledUrls[robots]; found {
		var err error
		target, err = resolveRelativeUrl(c.url, robots)
		if err == nil {
			return target
		}
	}

	for r := range c.uncrawledUrls {
		var err error
		target, err = resolveRelativeUrl(c.url, r)
		if err != nil {
			// Shouldn't be possible, try again next time
			continue
		}
		break
	}
	return target
}

func stealUrlsForConnections(connections []*connection, urls []*url.URL) []*url.URL {
	unusedUrls := []*url.URL{}
	for _, u := range urls {
		if i := indexConnectionByHostPort(connections, u); i != -1 {
			c := connections[i]
			r := urlToRelativeUrl(u)
			_, inCrawling := c.crawlingUrls[r]
			_, inCrawled := c.crawledUrls[r]
			if !inCrawling && !inCrawled {
				c.uncrawledUrls[r] = true
			}
			continue
		}
		unusedUrls = append(unusedUrls, u)
	}
	return unusedUrls
}

func makeCrawlRequest(c *connection) *roundtrip {
	target := getNextUrlToCrawl(c)
	return &roundtrip{
		connId:     c.id,
		client:     c.client,
		url:        target,
		host:       c.host,
		crawlDelay: c.crawlDelay,
	}
}

func lookupv4AddrRequest(h *url.URL, resolvedAddr chan<- *resolvedUrl, cancel <-chan struct{}) {
	select {
	case resolvedAddr <- lookupAddr("ip4", h):
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

	workerLimit := 10000
	semC := make(chan struct{}, workerLimit)

	urls = deleteDuplicateUrlsByHostPort(urls)
	pendingResolutions := lookupQueue{}
	for _, u := range urls {
		pendingResolutions.put(u)
	}

	connectionIdCtr := uint(0)
	repeatedDailFails := 0
	pendingConns := []*connection{}
	activeConns := []*connection{}
	failedConns := []*connection{}
	for {
		var lookupAddrSemC chan<- struct{} = nil
		var scrapRequestSemC chan<- struct{} = nil

		if pendingResolutions.peek() != nil {
			lookupAddrSemC = semC
		}

		crawlConnection := getNextConnection(pendingConns, activeConns, workerLimit-len(semC))
		if crawlConnection != nil {
			scrapRequestSemC = semC
		}

		select {
		case <-time.After(50 * time.Millisecond):
		case lookupAddrSemC <- struct{}{}:
			hUrl := pendingResolutions.pop()
			go func() {
				// Only lookup IPv4 addresses. IPv6 addresses are
				// not running out so no need for CGNAT.
				lookupv4AddrRequest(hUrl, lookupAddrReply, stopC)
				<-semC
			}()
		case h, ok := <-lookupAddrReply:
			if !ok {
				return -1
			}

			i := slices.IndexFunc(h.addresses, func(a netip.AddrPort) bool {
				return indexConnectionByAddr(pendingConns, a) == -1 &&
					indexConnectionByAddr(activeConns, a) == -1
			})
			if i == -1 {
				break
			}
			c := makeConnection(h.addresses[i], h.url)
			c.id = connectionIdCtr
			pendingConns = append(pendingConns, c)
			connectionIdCtr++
		case scrapRequestSemC <- struct{}{}:
			request := makeCrawlRequest(crawlConnection)
			crawlConnection.lastRequest = time.Now()
			go func() {
				scrapConnectionRequest(request, scrapedReply, stopC)
				<-semC
			}()

			rUrl := urlToRelativeUrl(request.url)
			delete(crawlConnection.uncrawledUrls, rUrl)
			crawlConnection.crawlingUrls[rUrl] = true

			if len(pendingConns) > 0 && pendingConns[0] == crawlConnection {
				pendingConns = pendingConns[1:]
				activeConns = append(activeConns, crawlConnection)
			}
		case reply, ok := <-scrapedReply:
			if !ok {
				return -1
			}

			i := indexConnectionById(activeConns, reply.connId)
			if i == -1 {
				break
			}

			c := activeConns[i]

			// Dial errors may signify the middleware NAT device has run out
			// of ports for this client
			if isDialError(reply.err) {
				repeatedDailFails++
			} else if len(c.crawledUrls) == 0 {
				repeatedDailFails = 0
			}

			// Add new connections
			rUrl := urlToRelativeUrl(reply.url)
			delete(c.crawlingUrls, rUrl)
			c.crawledUrls[rUrl] = true
			c.crawlDelay = reply.crawlDelay
			c.lastRequest = reply.requestTs
			c.lastReply = reply.replyTs

			if reply.err != nil {
				failedConns = append(failedConns, activeConns[i])
				activeConns = slices.Delete(activeConns, i, i+1)
			}

			// Determine where to put the newly scraped urls
			newUrls := stealUrlsForConnections(activeConns, reply.scrapedUrls)
			urlsToResolve := []*url.URL{}
			for _, u := range newUrls {
				if i := indexUrlByHostPort(urlsToResolve, u); i != -1 {
					continue
				}
				if i := indexConnectionByHostPort(pendingConns, u); i != -1 {
					continue
				}
				if i := indexConnectionByHostPort(failedConns, u); i != -1 {
					// Avoid consuming extra NAT translations on a bad server
					continue
				}
				urlsToResolve = append(urlsToResolve, u)
			}
			if len(urlsToResolve) > 0 {
				pendingResolutions.put(urlsToResolve...)
			}
		}

		if repeatedDailFails >= 5 {
			// Assume the NAT has exceeded the allowed connections
			// after repeated dail failures.
			break
		}
		if len(pendingConns) == 0 && len(pendingResolutions.urls) == 0 && len(semC) == 0 {
			for _, c := range activeConns {
				if len(c.uncrawledUrls)+len(c.crawlingUrls) > 0 {
					break
				}
			}
			haveMoreUrls := slices.ContainsFunc(activeConns, func(c *connection) bool {
				return len(c.uncrawledUrls)+len(c.crawlingUrls) > 0
			})
			if !haveMoreUrls {
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
