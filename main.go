package main

// TODO: Unit testing
// * net.Pipe is a pipe that fufills the net.Conn interface
// * https://pkg.go.dev/github.com/pion/transport/vnet#section-readme but in early days so far
// * https://pkg.go.dev/cunicu.li/gont/v2#section-readme is Linux specific

import (
	"container/heap"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/net/html"
)

// Features:
// * Count the number of port mappings supported by the CGNAT
// * Could check if TCP and UDP mapping numbers are the same, this would require a different protocol (QUIC)?

// TODO:
// * Try UpnP
// * Add pion stun package to gain information about NAT behaviour
//   * https://github.com/pion/stun
//   * https://www.voip-info.org/stun/
// * Try and run a traceroute through the IP to figure out more details about the NAT setup
// * Fetch a list (of top uris to use)
// * Resolve hostnames of uris and add IPs to sets to avoid communicating with the same IP twice
//   * Does this make any sense what type of NAT wouldn't make a new
// * Resolve IP, use IP in https request but mannually set the SNI do the certificate validation still passes

// * How does truthy work in Go? Tidy up statement after checking

// TODO: approximate RTT of sites to guess how long to timeout the dialer

// TODO: first measure how long a NAT will leave a port open for, need a reply from the port, does stun do that?

const (
	successfulRequest = iota
	retryRequest      = iota
	rejectedRequest   = iota
	fatalRequest      = iota
)

type host struct {
	ip  net.IP
	url url.URL
}

type request struct {
	host
	connId uint // connection the request belongs to
	client *http.Client
}

type reply struct {
	host
	connId uint
	urls   []url.URL
	status int
}

type connection struct {
	host
	id            uint
	client        *http.Client
	uncrawledUrls []url.URL
	crawledUrls   []url.URL
	// keepAlive     time.Duration // Remember that the time we are interested in is really how long a NAT will leave a port open for
	lastRequest     time.Time
	connectAttempts int
	connected       bool
	// extra for experts would add some latency to figure out when it should be re-newed (moving average for latency of connection)
}

func (h host) hostIPUrl() *url.URL {
	ipUrl := h.url
	ipUrl.Host = fmt.Sprintf("%s:%s", h.ip.String(), h.url.Port())
	return &ipUrl
}

// Define an interface that has a timestamp and time to next timestamp
func (c connection) nextRequest() time.Time {
	// TODO: set keepAlive on connections
	return c.lastRequest.Add(5 * time.Second)
}

type connectionHeap []connection

func (h connectionHeap) Len() int      { return len(h) }
func (h connectionHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }
func (h connectionHeap) Less(i, j int) bool {
	timeI := h[i].nextRequest()
	timeJ := h[j].nextRequest()
	return timeI.Before(timeJ)
}
func (h *connectionHeap) Push(x any) {
	*h = append(*h, x.(connection))
}

func (h *connectionHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[0 : n-1]
	return x
}

func makeUrl(rawUrl string) (*url.URL, error) {
	if strings.HasPrefix(rawUrl, "https:") {
		return url.Parse(rawUrl)
	}
	if strings.HasPrefix(rawUrl, "http:") {
		// rawUrl = strings.Replace(rawUrl, "http:", "https:", 1)
		return url.Parse(rawUrl)
	}
	return url.Parse("https://" + rawUrl)
}

func makeHttpClient(host url.URL) *http.Client {
	dialer := net.Dialer{
		Timeout:   1 * time.Second, // TODO: Probably adjust for average RTT based on random selection of servers
		KeepAlive: 10,
	}
	tls := tls.Config{
		ServerName: host.Hostname(),
	}
	transport := http.Transport{
		DisableKeepAlives: false,
		// TLSHandshakeTimeout: 1,
		DialContext:     dialer.DialContext,
		TLSClientConfig: &tls,
	}
	client := http.Client{
		Transport: &transport,
	}
	return &client
}

// TODO: channels and reporting errors
func resolveUrlIPs(wg *sync.WaitGroup, urls <-chan string, resolvedUrls chan<- host, cancel <-chan struct{}) {
	defer wg.Done()
	defer fmt.Println("sendDnsRequests: Done")

	for {
		var rawUrl string
		var ip net.IP
		var ok bool

		/* Quit without possibility of selecting other cases */
		select {
		case <-cancel:
			return
		default:
		}

		/* Stop blocking if canceled */
		select {
		case rawUrl, ok = <-urls:
		case <-cancel:
			return
		}
		if !ok {
			break
		}

		u, err := makeUrl(rawUrl)
		if err != nil {
			fmt.Printf("Error in sendDnsRequests: %v\n", err)
			continue
		}

		hostname := u.Hostname()

		/* Detect if the url already contains an IP address,
		 * no need to send DNS request */
		ip = net.ParseIP(hostname)
		if ip != nil {
			fmt.Printf("Pre-Resoluted %v->%v\n", u, ip)
			resolvedUrls <- host{ip: ip, url: *u}
			continue
		}

		// TODO: Check if I should change to SRV record
		addrs, err := net.LookupHost(hostname)
		if err != nil {
			fmt.Printf("Error in sendDnsRequests: %v\n", err)
			continue
		}
		if len(addrs) == 0 {
			fmt.Printf("Error in sendDnsRequests: %v\n", err)
			continue
		}
		ip = net.ParseIP(addrs[0])
		if ip == nil {
			fmt.Printf("Error in sendDnsRequests: %v\n", err)
			continue
		}

		fmt.Printf("Resolved %v->%v\n", u, ip)
		select {
		case resolvedUrls <- host{ip: ip, url: *u}:
		case <-cancel:
			return
		}
	}
}

func crawlHttpHost(c *http.Client, h host) (int, []url.URL, error) {
	client := makeHttpClient(h.url) // TODO: add an http connection map for re-use

	ipUrl := h.hostIPUrl()
	fmt.Printf("sendHttpRequest: Making url %s:%s\n", h.ip.String(), h.url.Port())
	fmt.Printf("sendHttpRequest: Calling get on %v\n", ipUrl.String())
	resp, err := client.Get(ipUrl.String())
	if err != nil {
		fmt.Printf("sendHttpRequest Error: %v\n", err)
		return -1, nil, err
	}
	fmt.Printf("sendHttpRequest: Done get on %v\n", ipUrl.String())

	// TODO: create err for empty body
	if resp != nil {
		defer resp.Body.Close()
		// 	_, err = io.ReadAll(resp.Body)
	}
	// if err != nil {
	// 	fmt.Printf("sendHttpRequest Error: %v\n", err)
	// 	return nil, err
	// }

	urls := []url.URL{}

	// Need to find <base> (only one and must be inside the head) <---
	// support <link> (head or body)
	// Support <area> (body)
	for z := html.NewTokenizer(resp.Body); ; {
		tt := z.Next()
		if tt == html.ErrorToken {
			// TODO: check z.Err == io.EOF (this is the normal success case)
			break
		}
		if tt != html.StartTagToken {
			continue
		}
		name, hasAttrs := z.TagName()
		if string(name) != "a" || !hasAttrs {
			continue
		}

		for {
			keyb, valb, more := z.TagAttr()
			key := string(keyb)
			if string(key) != "href" {
				continue
			}

			val := string(valb)
			u, err := url.Parse(val)
			if err != nil {
				continue
			}

			if u.IsAbs() {
				if u.Host == h.url.Host {
					// TODO: add all but then resolve the Hosts and figure out IPs
					urls = append(urls, *u)
				}
				continue
			}

			/* Handle relative hrefs */
			urlAbs := h.url
			urlAbs.Path = u.Path
			urls = append(urls, urlAbs)

			if !more {
				break
			}
		}
	}

	fmt.Printf("sendHttpRequest Status: [%v] %v: %v\n", h.url, resp.Status, urls)
	// TODO: parse out urls
	return resp.StatusCode, urls, nil
}

func crawlHttpHosts(wg *sync.WaitGroup, hosts <-chan request, results chan<- reply, cancel <-chan struct{}) {
	defer wg.Done()

	for {
		var r request
		var ok bool

		/* Quit without possibility of selecting other cases */
		select {
		case <-cancel:
			return
		default:
		}

		/* Stop blocking if canceled */
		select {
		case r, ok = <-hosts:
		case <-cancel:
			return
		}
		if !ok {
			break
		}

		cstatus := successfulRequest
		status, urls, err := crawlHttpHost(r.client, r.host)
		fmt.Printf("crawlHttpUrls: crawl on connection %v\n", r.connId)
		if err != nil {
			cstatus = fatalRequest
		} else if status == 504 {
			cstatus = retryRequest
		} else if status >= 400 {
			cstatus = rejectedRequest
		}

		select {
		case results <- reply{connId: r.connId, status: cstatus, host: r.host, urls: urls}:
		case <-cancel:
			return
		}
	}
	fmt.Println("sendHttpRequests: Finished Worker")
}

func MeasureMaxConnections(urls []string) int {
	ncpus := runtime.NumCPU()
	fmt.Printf("ncpus = %v\n", ncpus)
	ncpus = 1

	var dnsWorkersCounter sync.WaitGroup
	var httpWorkersCounter sync.WaitGroup

	resolveUrls := make(chan string)
	resolvedUrls := make(chan host)
	crawlUrls := make(chan request)
	crawledUrls := make(chan reply)
	stopC := make(chan struct{})

	// TODO: Need an atomic int variable to keep track of currently open connections.

	/* Start DNS resolvers */
	for i := 0; i < ncpus; i++ {
		dnsWorkersCounter.Add(1)
		go resolveUrlIPs(&dnsWorkersCounter, resolveUrls, resolvedUrls, stopC)
	}

	/* Start HTTP crawlers */
	for i := 0; i < ncpus; i++ {
		httpWorkersCounter.Add(1)
		go crawlHttpHosts(&httpWorkersCounter, crawlUrls, crawledUrls, stopC)
	}

	pendingConnections := make([]connection, 0)
	failedConnections := make([]connection, 0)
	activeConnections := make(connectionHeap, 0, len(urls))
	heap.Init(&activeConnections)
	// When all activeConnections have recieved a reply (crawledUrl)
	// When connections get dropped
	for rawUrlIdx, connId := 0, uint(0); ; {
		// TODO: Stop when I start dropping connections, or all connections are alive and nothing is dropped
		var workDone int
		var nReplied int

		// TODO: Some hosts will fail to connect
		// This forgets about unreplied connections
		// TODO: need to timeout connections
		if (len(activeConnections) + len(failedConnections)) == len(urls) {
			for _, c := range activeConnections {
				if len(c.crawledUrls) > 0 {
					nReplied++
				}
			}
			if nReplied == len(activeConnections) {
				break
			}
		}

		/* Send and recieve DNS resolutions */
		for more := true; more && rawUrlIdx < len(urls); {
			fmt.Printf("Try pipe resolve host\n")
			select {
			case resolveUrls <- urls[rawUrlIdx]:
				fmt.Printf("Piped raw url %v\n", urls[rawUrlIdx])
				workDone++
				rawUrlIdx++
			default:
				/* Need to avoid tight-looping all dns requests at once */
				more = false
			}
		}
		fmt.Printf("Past pipe resolved host\n")

		for more := true; more; {
			fmt.Printf("Try Read resolved host\n")
			select {
			case h, ok := <-resolvedUrls:
				if !ok {
					// TODO: retry the failed ones a couple of times maybe
					continue
				}
				fmt.Printf("Read resolved host ip=%v,url=%v\n", h.ip, h.url)
				c := connection{
					id:            connId,
					client:        makeHttpClient(h.url),
					host:          h,
					crawledUrls:   []url.URL{},
					uncrawledUrls: []url.URL{h.url},
					lastRequest:   time.Now(),
					connected:     false,
				}
				connId++
				pendingConnections = append(pendingConnections, c)
				workDone++
			default:
				/* Need to avoid tight-looping all dns requests at once */
				more = false
			}
		}
		fmt.Printf("Past read resolved host\n")

		/* Send off any new packets before keep alive runs out */
		for more := true; more; {
			if len(activeConnections) == 0 {
				break
			}

			c := &activeConnections[0]
			// TODO: get a better latency measure than 2
			fmt.Printf("Time until next response on connection %v is %v\n", c.id, time.Until(c.nextRequest()).Seconds())
			if time.Until(c.nextRequest()).Seconds() > 2 {
				break
			}

			fmt.Printf("Try re-request host\n")
			if len(c.uncrawledUrls) == 0 {
				fmt.Printf("Try host is out of uncrawled urls\n")
				// continue: TODO: this should be able to handle uncrawled URLs
				break
			}

			r := request{host: host{c.ip, c.uncrawledUrls[0]}, connId: c.id}
			select {
			case crawlUrls <- r:
				fmt.Printf("Piped re-request on connection %v\n", c.id)
				// TODO: What is the packet never arrives? This isn't going to do anything for 2 seconds.
				c.lastRequest = time.Now()
				heap.Fix(&activeConnections, 0)
				workDone++
			default:
				more = false
			}
		}
		fmt.Printf("Past rerequest host\n")

		/* Send off first packets of connection */
		for more := true; more; {
			if len(pendingConnections) == 0 {
				break
			}

			c := &pendingConnections[0]
			r := request{host: c.host, connId: c.id, client: c.client}
			fmt.Printf("Try request host\n")
			select {
			case crawlUrls <- r:
				fmt.Printf("Piped request on connection %v\n", connId)
				c.connectAttempts++
				activeConnections = append(activeConnections, pendingConnections[0])
				pendingConnections = pendingConnections[1:]
				workDone++
			default:
				more = false
			}
		}
		fmt.Printf("Past request host\n")

		/* Collect completed connections */
		for more := true; more; {
			fmt.Printf("Try receive host\n")
			select {
			case r, ok := <-crawledUrls:
				if !ok {
					more = false
					break
				}

				workDone++
				i := slices.IndexFunc(activeConnections, func(c connection) bool {
					return r.connId == c.id
				})
				if i == -1 {
					fmt.Println("Unknown connection id", r.connId)
					break
				}

				c := &activeConnections[i]
				c.connected = r.status != fatalRequest
				fmt.Printf("Active connections returned with status %v\n", r.status)
				if !c.connected {
					heap.Remove(&activeConnections, i)
					fmt.Printf("Active connections failed %v with %v attempts left\n", r.urls, 3-c.connectAttempts)
					if c.connectAttempts < 3 {
						pendingConnections = append(pendingConnections, *c)
					} else {
						c.client = nil
						failedConnections = append(failedConnections, *c)
					}
					break
				}
				c.connectAttempts = 0
				c.crawledUrls = append(c.crawledUrls, r.url)

				// Remove url from uncraledUrls
				j := slices.Index(c.uncrawledUrls, r.url)
				if j != -1 {
					c.uncrawledUrls = slices.Delete(c.uncrawledUrls, j, j+1)
				}

				// TODO: This will happen a lot Find and modify or add activeConnections
				fmt.Printf("Active connections %v\n", r.urls)
				for _, u := range r.urls {
					if !slices.Contains(c.uncrawledUrls, u) {
						c.uncrawledUrls = append(c.uncrawledUrls, u)
					}
				}
			default:
				more = false
			}
		}
		fmt.Printf("past receive host\n")

		/* Avoid tightly looping */
		if workDone == 0 {
			// TODO: base this sleep timer off min(1s, )
			fmt.Println("Start Sleep")
			time.Sleep(time.Second)
			fmt.Println("End Sleep")
		}
	}
	fmt.Printf("*** Finished loop\n")

	close(stopC)

	/* Wait until resolvers have shutdown */
	close(resolveUrls)
	dnsWorkersCounter.Wait()
	close(resolvedUrls)

	/* Wait until http crawlers have shutdown */
	close(crawlUrls)
	httpWorkersCounter.Wait()
	close(crawledUrls)

	nConns := 0
	for _, c := range activeConnections {
		if c.connected {
			nConns++
		}
	}
	return nConns
}

func main() {
	fmt.Println("Hello, World!")
	// /* Send hostnames off to DNS workers to be resolved */
	// // TODO: read these rawUrls from a file
	rawUrls := []string{
		// "https://localhost:4443/",
		"http://192.168.65.2:8080/",
		"http://192.168.65.2:8080/ghost.html",
		// "http://192.168.65.2:8080/",
		// "http://192.168.65.2:8080/",
		// "http://192.168.65.2:8080/index.html",
	}
	nConns := MeasureMaxConnections(rawUrls)
	fmt.Printf("nConns=%v\n", nConns)
	fmt.Println("Bye, World!")
}
