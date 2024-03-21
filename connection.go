// Functions related to managing connections to hosts. Each connection must kept to one server
// connection to accurately measure the maximum allowed connections.
package main

import (
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"runtime"
	"slices"
	"sync"
	"time"
)

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

func indexKeepAliveConnection(conns []*connection) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return time.Since(c.lastReply) > 3500*time.Millisecond && time.Since(c.lastRequest) > 1000*time.Millisecond
	})
}

func indexConnectionByAddrs(conns []*connection, addrs []netip.Addr) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return slices.ContainsFunc(addrs, func(addr netip.Addr) bool {
			return c.host.ip.Addr() == addr
		})
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
	client := http.Client{
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			// Do not follow re-directs
			return http.ErrUseLastResponse
		},
		Transport: transport,
	}
	return &client
}

func makeConnection(h *url.URL) *connection {
	c := &connection{
		client:        makeClient(),
		url:           h,
		uncrawledUrls: []*url.URL{h},
		host:          &host{hostnames: []string{h.Hostname()}},
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

func MeasureMaxConnections(urls []*url.URL) int {
	lookupAddrRequest := make(chan *url.URL)
	lookupAddrReply := make(chan *resolvedUrl)
	scrapRequest := make(chan *roundtrip)
	scrapedReply := make(chan *roundtrip)
	stopC := make(chan struct{})
	lookupCounter := sync.WaitGroup{}
	scapersCounter := sync.WaitGroup{}

	pendingResolutions := [][]*url.URL{}
	pendingConns := []*connection{}
	for i := range urls {
		c := makeConnection(urls[i])
		c.id = uint(i)
		pendingConns = append(pendingConns, c)
	}

	nScrapers := min(len(urls), 2*runtime.NumCPU())
	nScrapers = max(nScrapers, int(len(urls)/100))
	for i := 0; i < nScrapers; i++ {
		lookupCounter.Add(1)
		go lookupAddrs(&lookupCounter, lookupAddrRequest, lookupAddrReply, stopC)

		scapersCounter.Add(1)
		go scrapConnections(&scapersCounter, scrapRequest, scrapedReply, stopC)
	}
	maxNScrapers := len(urls)

	keepAliveRequestsInWindow := 0
	activeConns := make([]*connection, 0)
	failedConns := make([]*connection, 0)
	for {
		var lookupAddrRequestC chan<- *url.URL = nil
		var scrapRequestC chan<- *roundtrip = nil
		timedOut := false

		hUrl := getNextUrl(pendingResolutions)
		if hUrl != nil {
			lookupAddrRequestC = lookupAddrRequest
		}

		var request *roundtrip = nil
		c := getNextConnection(pendingConns, activeConns)
		if c != nil {
			scrapRequestC = scrapRequest
			target := c.url
			if len(c.uncrawledUrls) > 0 {
				target = c.uncrawledUrls[0]
			}
			request = &roundtrip{connId: c.id, client: c.client, url: target, host: c.host}
		}

		select {
		case <-time.After(50 * time.Millisecond):
			timedOut = true
		case lookupAddrRequestC <- hUrl:
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

			i := indexConnectionByAddrs(activeConns, h.addresses)
			if i == -1 {
				break
			}
			c := activeConns[i]
			h.url.Host = fmt.Sprintf("%v:%v", h.url.Hostname(), c.host.ip.Port())
			activeConns[i].uncrawledUrls = append(activeConns[i].uncrawledUrls, h.url)
		case scrapRequestC <- request:
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

			if len(reply.scrapedUrls) > 0 {
				pendingResolutions = append(pendingResolutions, reply.scrapedUrls)
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

		// Grow number of scrapers based on demand, connection upkeep should
		// account a maximum of 10% of the requests.
		if !timedOut && keepAliveRequestsInWindow > 1 && nScrapers < maxNScrapers {
			nNewScrapers := min((keepAliveRequestsInWindow-1)*nScrapers, maxNScrapers-nScrapers)
			for s := 0; s < nNewScrapers; s++ {
				scapersCounter.Add(1)
				go scrapConnections(&scapersCounter, scrapRequest, scrapedReply, stopC)

			}
			nScrapers += nNewScrapers
		}
	}

	close(stopC)
	close(lookupAddrRequest)
	close(scrapRequest)
	lookupCounter.Wait()
	scapersCounter.Wait()
	close(lookupAddrReply)
	close(scrapedReply)
	return len(activeConns)
}
