// Functions related to managing connections to hosts. Each connection must kept to one server
// connection to accurately measure the maximum allowed connections.
package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"slices"
	"sync"
	"time"
)

type workRequest func(cancel <-chan struct{}) bool

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

func canonicalHost(url *url.URL) string {
	return net.JoinHostPort(url.Hostname(), urlPort(url))
}

func indexKeepAliveConnection(conns []*connection) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return time.Since(c.lastReply) > 3500*time.Millisecond && time.Since(c.lastRequest) > 1000*time.Millisecond
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

func makeConnection(addr netip.AddrPort, h *url.URL) *connection {
	c := &connection{
		client:        makeClient(),
		url:           h,
		uncrawledUrls: []*url.URL{h},
		host: &host{
			hostPort: canonicalHost(h),
			ip:       addr,
		},
	}
	return c
}

func getNextConnection(pendingConns, activeConns []*connection) *connection {
	var c *connection
	keepAliveIdx := indexKeepAliveConnection(activeConns)
	keepAliveConnection := keepAliveIdx != -1
	if keepAliveConnection {
		// Prioritise existing connections to avoid http keep-alive
		// or NAT mapping expires
		c = activeConns[keepAliveIdx]
	} else if len(pendingConns) > 0 {
		// Create a new connection
		c = pendingConns[0]
	}
	return c
}

func getNextUrl(pendingResolutions [][]*url.URL) *url.URL {
	if len(pendingResolutions) == 0 {
		return nil
	}

	return pendingResolutions[0][0]
}

func worker(wg *sync.WaitGroup, requests <-chan workRequest, cancel <-chan struct{}) {
	defer wg.Done()

	for {
		var w workRequest
		var ok bool

		// Quit without possibility of selecting other cases
		select {
		case <-cancel:
			return
		default:
		}

		// Stop blocking if canceled
		select {
		case w, ok = <-requests:
		case <-cancel:
			return
		}
		if !ok {
			break
		}

		canceled := w(cancel)
		if canceled {
			return
		}
	}
}

func makeLookupAddrWorkRequest(h *url.URL, resolvedAddr chan<- *resolvedUrl) workRequest {
	return func(cancel <-chan struct{}) bool {
		select {
		case resolvedAddr <- lookupAddr(h):
		case <-cancel:
			return true
		}
		return false
	}
}

func makeScrapConnectionWorkRequest(r *roundtrip, scraped chan<- *roundtrip) workRequest {
	ctx := context.WithValue(context.Background(), ctxAddrKey{}, r.host.ip)
	return func(cancel <-chan struct{}) bool {
		select {
		case scraped <- scrapConnection(ctx, r):
		case <-cancel:
			return true
		}
		return false
	}
}

func MeasureMaxConnections(urls []*url.URL) int {
	workRequests := make(chan workRequest)
	lookupAddrReply := make(chan *resolvedUrl)
	scrapedReply := make(chan *roundtrip)
	stopC := make(chan struct{})
	workerCounter := sync.WaitGroup{}

	connectionIdCtr := uint(0)
	pendingConns := []*connection{}
	pendingResolutions := [][]*url.URL{}
	for _, u := range urls {
		pendingResolutions = append(pendingResolutions, []*url.URL{u})
	}

	nWorkers := min(len(urls), 2*runtime.NumCPU())
	nWorkers = max(nWorkers, int(len(urls)/100))
	for i := 0; i < 2*nWorkers; i++ {
		workerCounter.Add(1)
		go worker(&workerCounter, workRequests, stopC)
	}
	maxNWorkers := len(urls)

	keepAliveRequestsInWindow := 0
	activeConns := make([]*connection, 0)
	failedConns := make([]*connection, 0)
	for {
		emptyWorkRequest := func(cancel <-chan struct{}) bool { return false }
		var lookupAddrRequestC chan<- workRequest = nil
		var scrapRequestC chan<- workRequest = nil
		lookupRequest := emptyWorkRequest
		scrapRequest := emptyWorkRequest
		timedOut := false

		hUrl := getNextUrl(pendingResolutions)
		if hUrl != nil {
			lookupAddrRequestC = workRequests
			lookupRequest = makeLookupAddrWorkRequest(hUrl, lookupAddrReply)
		}

		var request *roundtrip = nil
		c := getNextConnection(pendingConns, activeConns)
		if c != nil {
			target := c.url
			if len(c.uncrawledUrls) > 0 {
				target = c.uncrawledUrls[0]
			}
			request = &roundtrip{connId: c.id, client: c.client, url: target, host: c.host}
			scrapRequestC = workRequests
			scrapRequest = makeScrapConnectionWorkRequest(request, scrapedReply)
		}

		select {
		case <-time.After(50 * time.Millisecond):
			timedOut = true
		case lookupAddrRequestC <- lookupRequest:
			// Rotate lookups from reach connection response to avoid spending ages on one
			// connection whilst others are already getting re-requested
			if len(pendingResolutions[0]) < 2 {
				pendingResolutions = pendingResolutions[1:]
			} else {
				pendingResolutions = append(pendingResolutions[1:], pendingResolutions[0])
			}
		case h, ok := <-lookupAddrReply:
			if !ok {
				return -1
			}

			unusedAddresses := []netip.AddrPort{}
			for _, address := range h.addresses {
				k := indexConnectionByAddr(activeConns, address)
				if k != -1 {
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
		case scrapRequestC <- scrapRequest:
			if len(pendingConns) > 0 && pendingConns[0] == c {
				pendingConns = pendingConns[1:]
			}

			// Track how many of the last 10 scraps were for an existing connection
			if len(c.crawledUrls) > 0 {
				keepAliveRequestsInWindow = min(keepAliveRequestsInWindow+1, 10)
			} else {
				keepAliveRequestsInWindow = max(keepAliveRequestsInWindow-1, 0)
			}

			c.lastRequest = time.Now()
		case reply, ok := <-scrapedReply:
			if !ok {
				return -1
			}

			newUrls := []*url.URL{}
			for i := range reply.scrapedUrls {
				u := reply.scrapedUrls[i]
				j := indexConnectionByHostPort(activeConns, u)
				if j == -1 {
					newUrls = append(newUrls, u)
					continue
				}

				c := activeConns[j]
				u.Host = fmt.Sprintf("%v:%v", u.Hostname(), c.host.ip.Port())
				c.uncrawledUrls = append(c.uncrawledUrls, u)
			}
			urlsToResolve := []*url.URL{}
			for i := range newUrls {
				u := newUrls[i]
				j := indexConnectionByHostPort(pendingConns, u)
				if j != -1 {
					continue
				}
				urlsToResolve = append(urlsToResolve, u)
			}
			if len(urlsToResolve) > 0 {
				pendingResolutions = append(pendingResolutions, urlsToResolve)
			}

			var c *connection
			i := slices.IndexFunc(activeConns, func(c *connection) bool {
				return reply.connId == c.id
			})
			if i != -1 {
				c = activeConns[i]

				// Adjust uncrawled and crawled urls
				j := slices.IndexFunc(c.uncrawledUrls, func(u *url.URL) bool {
					return *u == *reply.url
				})
				if j != -1 {
					c.uncrawledUrls = slices.Delete(c.uncrawledUrls, j, j+1)
				}

				if reply.failed {
					failedConns = append(failedConns, c)
					activeConns = slices.Delete(activeConns, i, i+1)
				}
			} else {
				c = &connection{
					id: reply.connId, client: reply.client, url: reply.url,
					uncrawledUrls: []*url.URL{},
					crawledUrls:   []*url.URL{reply.url},
					host:          reply.host,
				}

				if reply.failed {
					failedConns = append(failedConns, c)
				} else {
					activeConns = append(activeConns, c)
				}
			}
			c.lastRequest = reply.requestTs
			c.lastReply = reply.replyTs

			if !sliceContainsUrl(c.crawledUrls, reply.url) {
				c.crawledUrls = append(c.crawledUrls, reply.url)
			}
		}

		if (len(activeConns) + len(failedConns)) == len(urls) {
			break
		}

		// Grow number of workers based on demand, connection upkeep should
		// account a maximum of 10% of the requests.
		if !timedOut && keepAliveRequestsInWindow > 1 && nWorkers < maxNWorkers {
			nNewWorkers := min((keepAliveRequestsInWindow-1)*nWorkers, maxNWorkers-nWorkers)
			for s := 0; s < nNewWorkers; s++ {
				workerCounter.Add(1)
				go worker(&workerCounter, workRequests, stopC)

			}
			nWorkers += nNewWorkers
		}
	}

	close(stopC)
	close(workRequests)
	workerCounter.Wait()
	close(lookupAddrReply)
	close(scrapedReply)
	return len(activeConns)
}
