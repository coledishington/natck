package main

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"os"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"
)

type resolvedUrl struct {
	url       *url.URL
	addresses []netip.Addr
}

type host struct {
	ip        netip.AddrPort
	hostnames []string
}

type roundtrip struct {
	connId      uint
	client      *http.Client
	host        *host
	url         *url.URL
	scrapedUrls []*url.URL
	failed      bool
	requestTs   time.Time
	replyTs     time.Time
}

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

func sliceContainsUrl(urls []*url.URL, needle *url.URL) bool {
	return slices.ContainsFunc(urls, func(u *url.URL) bool {
		return *u == *needle
	})
}

func indexConnectionByAddrs(conns []*connection, addrs []netip.Addr) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return slices.ContainsFunc(addrs, func(addr netip.Addr) bool {
			return c.host.ip.Addr() == addr
		})
	})
}

func indexKeepAliveConnection(conns []*connection) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return time.Since(c.lastReply) > 3500*time.Millisecond && time.Since(c.lastRequest) > 1000*time.Millisecond
	})
}

func readUrls(input io.Reader) ([]*url.URL, error) {
	urls := make([]*url.URL, 0)
	r := bufio.NewReaderSize(input, 160)
	for more := true; more; {
		line, rErr := r.ReadString('\n')
		if rErr != nil && rErr != io.EOF {
			err := fmt.Errorf("failed to read url line: %w", rErr)
			return nil, err
		}
		more = rErr != io.EOF

		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		u, err := url.Parse(line)
		if err != nil {
			continue
		}

		urls = append(urls, u)
	}
	return urls, nil
}

func getUrl(client *http.Client, target *url.URL) (netip.AddrPort, *http.Response, error) {
	remoteAddr := netip.AddrPortFrom(netip.IPv4Unspecified(), 0)
	targetUrl := target.String()
	req, err := http.NewRequest(http.MethodGet, targetUrl, nil)
	if err != nil {
		err = fmt.Errorf("failed to make request: %w", err)
		return remoteAddr, nil, err
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		ConnectDone: func(network, addr string, _ error) {
			rAddr, err := netip.ParseAddrPort(addr)
			if err != nil {
				return
			}
			remoteAddr = rAddr
		},
	}))
	resp, err := client.Do(req)
	if err != nil {
		err = fmt.Errorf("failed get uri %v: %w", targetUrl, err)
		return remoteAddr, nil, err
	}
	return remoteAddr, resp, err
}

func scrapResponse(target *url.URL, resp *http.Response) []*url.URL {
	// Parse urls from content
	urls := Scrap(target, resp.Body)

	// Add url from redirect if it belongs to the same server
	location, err := resp.Location()
	if err == nil && !sliceContainsUrl(urls, location) {
		urls = append(urls, location)
	}
	return urls
}

func scrapHostUrl(client *http.Client, host *host, target *url.URL) ([]*url.URL, error) {
	remoteIP, resp, err := getUrl(client, target)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	// Use the new IP if a new connection was made
	if !remoteIP.Addr().IsUnspecified() {
		host.ip = remoteIP
	}

	return scrapResponse(target, resp), nil
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

		/* Quit without possibility of selecting other cases */
		select {
		case <-cancel:
			return
		default:
		}

		/* Stop blocking if canceled */
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

func scrapConnection(r *roundtrip) *roundtrip {
	r.requestTs = time.Now()
	sUrls, err := scrapHostUrl(r.client, r.host, r.url)
	r.replyTs = time.Now()
	r.failed = err != nil
	r.scrapedUrls = sUrls
	return r
}

func scrapConnections(wg *sync.WaitGroup, pendingScraps <-chan *roundtrip, scraped chan<- *roundtrip, cancel <-chan struct{}) {
	defer wg.Done()

	for {
		var r *roundtrip
		var ok bool

		/* Quit without possibility of selecting other cases */
		select {
		case <-cancel:
			return
		default:
		}

		/* Stop blocking if canceled */
		select {
		case r, ok = <-pendingScraps:
		case <-cancel:
			return
		}
		if !ok {
			break
		}

		select {
		case scraped <- scrapConnection(r):
		case <-cancel:
			return
		}
	}
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

func main() {
	urls, err := readUrls(os.Stdin)
	if err != nil {
		fmt.Printf("Failed to read urls from stdin: %v", err)
		os.Exit(1)
	}

	nConns := MeasureMaxConnections(urls)
	fmt.Println("Max connections are", nConns)
}
