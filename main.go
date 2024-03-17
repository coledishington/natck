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

type host struct {
	ip        netip.Addr
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

func indexKeepAliveConnection(conns []*connection) int {
	return slices.IndexFunc(conns, func(c *connection) bool {
		return time.Since(c.lastReply) > 3500*time.Millisecond && time.Since(c.lastRequest) > 1000*time.Millisecond
	})
}

func hostnameHasIPAddr(hostname string, needle netip.Addr) bool {
	found := false
	addrs, err := net.LookupIP(hostname)
	if err != nil {
		return found
	}
	for _, addrBytes := range addrs {
		addr, ok := netip.AddrFromSlice(addrBytes)
		if !ok {
			continue
		}
		found = addr == needle
		if found {
			break
		}
	}
	return found
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

func getUrl(client *http.Client, target *url.URL) (netip.Addr, *http.Response, error) {
	remoteAddr := netip.IPv4Unspecified()
	targetUrl := target.String()
	req, err := http.NewRequest(http.MethodGet, targetUrl, nil)
	if err != nil {
		err = fmt.Errorf("failed to make request: %w", err)
		return remoteAddr, nil, err
	}
	req = req.WithContext(httptrace.WithClientTrace(req.Context(), &httptrace.ClientTrace{
		ConnectDone: func(network, addr string, err error) {
			rAddr, err := netip.ParseAddr(addr)
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
	if !remoteIP.IsUnspecified() {
		host.ip = remoteIP
	}

	urls := scrapResponse(target, resp)

	// Find urls matching the scraped host
	hostUrls := []*url.URL{}
	for i := range urls {
		u := urls[i]
		if *u == *target {
			continue
		}
		uHost := u.Hostname()
		if !slices.Contains(host.hostnames, uHost) && hostnameHasIPAddr(uHost, host.ip) {
			host.hostnames = append(host.hostnames, uHost)
		}
		hostUrls = append(hostUrls, urls[i])
	}
	return hostUrls, nil
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

func MeasureMaxConnections(urls []*url.URL) int {
	scrapRequest := make(chan *roundtrip)
	scrapedReply := make(chan *roundtrip)
	stopScraping := make(chan struct{})
	scapersCounter := sync.WaitGroup{}

	pendingConns := make([]*connection, 0)
	for i := range urls {
		c := makeConnection(urls[i])
		c.id = uint(i)
		pendingConns = append(pendingConns, c)
	}

	nScrapers := min(len(urls), 2*runtime.NumCPU())
	nScrapers = max(nScrapers, int(len(urls)/100))
	for i := 0; i < nScrapers; i++ {
		scapersCounter.Add(1)
		go scrapConnections(&scapersCounter, scrapRequest, scrapedReply, stopScraping)
	}
	maxNScrapers := len(urls)

	keepAliveRequestsInWindow := 0
	activeConns := make([]*connection, 0)
	failedConns := make([]*connection, 0)
	for {
		var scrapRequestC chan<- *roundtrip = nil
		timedOut := false

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

		var request *roundtrip = nil
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
		case scrapRequestC <- request:
			if len(pendingConns) > 0 && pendingConns[0] == c {
				pendingConns = pendingConns[1:]
			}

			// Track how many of the last 10 scraps were for an existing connection
			if keepAliveConnection {
				keepAliveRequestsInWindow = min(keepAliveRequestsInWindow+1, 10)
			} else {
				keepAliveRequestsInWindow = max(keepAliveRequestsInWindow-1, 0)
			}

			c.lastRequest = time.Now()
		case reply, ok := <-scrapedReply:
			if !ok {
				return -1
			}

			i := slices.IndexFunc(activeConns, func(c *connection) bool {
				return reply.connId == c.id
			})
			if i == -1 {
				c := &connection{
					id: reply.connId, client: reply.client, url: reply.url,
					uncrawledUrls: reply.scrapedUrls,
					crawledUrls:   []*url.URL{reply.url},
					host:          reply.host,
					lastRequest:   reply.requestTs,
					lastReply:     reply.replyTs,
				}
				if reply.failed {
					failedConns = append(failedConns, c)
				} else {
					activeConns = append(activeConns, c)
				}
				break
			}
			c := activeConns[i]
			// Adjust uncrawled and crawled urls
			j := slices.IndexFunc(c.uncrawledUrls, func(u *url.URL) bool {
				return *u == *reply.url
			})
			if j != -1 {
				c.uncrawledUrls = slices.Delete(c.uncrawledUrls, j, j+1)
			}
			// Add any new urls to the connection's uncrawled urls
			for i := range reply.scrapedUrls {
				u := reply.scrapedUrls[i]
				if sliceContainsUrl(c.uncrawledUrls, u) || sliceContainsUrl(c.crawledUrls, u) {
					continue
				}
				c.uncrawledUrls = append(c.uncrawledUrls, u)
			}
			if !sliceContainsUrl(c.crawledUrls, reply.url) {
				c.crawledUrls = append(c.crawledUrls, reply.url)
			}
			c.lastRequest = reply.requestTs
			c.lastReply = reply.replyTs
			if reply.failed {
				failedConns = append(failedConns, activeConns[i])
				activeConns = slices.Delete(activeConns, i, i+1)
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
				go scrapConnections(&scapersCounter, scrapRequest, scrapedReply, stopScraping)

			}
			nScrapers += nNewScrapers
		}
	}

	close(stopScraping)
	close(scrapRequest)
	scapersCounter.Wait()
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
